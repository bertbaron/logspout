package ingress

import (
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gliderlabs/logspout/router"
	"golang.org/x/net/websocket"
)

const (
	// MaxTestMessages is the most messages one POST /api/test traces.
	MaxTestMessages = 1000
	// MaxTestData is the most message data (bytes, before the per message cap) one test may hold.
	MaxTestData = 4 << 20
	// maxTestBody is at most 16 MiB: the draft, the messages and JSON escapes. Not
	// every combination of the maximums fits; MaxTestData is the real input limit.
	maxTestBody = 16 << 20
	// maxLiveClients bounds the traces that every message can cost.
	maxLiveClients = 8
)

// liveWriteTimeout is a variable for the tests.
var liveWriteTimeout = 10 * time.Second

func (s *Server) getSamples(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"capacity": SampleCount,
		"samples":  s.samples().ring.snapshot(),
	})
}

// testMessage is a pasted line. Only data is required.
type testMessage struct {
	Container string    `json:"container"`
	Image     string    `json:"image"`
	Source    string    `json:"source"`
	Time      time.Time `json:"time"`
	Data      *string   `json:"data"`
	Level     string    `json:"level"`
}

type testRequest struct {
	Content  *string        `json:"content"`
	Messages *[]testMessage `json:"messages"`
}

// test runs a draft config over messages. The draft is built, not installed:
// the active pipeline and the rule file stay as they are.
func (s *Server) test(w http.ResponseWriter, r *http.Request) {
	// One test at a time: it can take a lot of CPU and memory.
	if !s.testBusy.CompareAndSwap(false, true) {
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusTooManyRequests, "another test is running")
		return
	}
	defer s.testBusy.Store(false)

	var body testRequest
	if !decodeBody(w, r, maxTestBody, `{"content": "...", "messages": [{"container": "...", "data": "..."}]}`, &body) || !checkContent(w, body.Content) {
		return
	}
	var in []Sample
	if body.Messages == nil {
		in = s.samples().ring.snapshot()
	} else {
		if len(*body.Messages) > MaxTestMessages {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("at most %d messages", MaxTestMessages))
			return
		}
		total := 0
		for i, m := range *body.Messages {
			if m.Data == nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("messages[%d]: data is missing", i))
				return
			}
			if total += len(*m.Data); total > MaxTestData {
				writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the messages may hold at most %d bytes of data", MaxTestData))
				return
			}
			if m.Level != "" {
				l, ok := router.NormalizeLevel(m.Level)
				if !ok {
					writeError(w, http.StatusBadRequest, fmt.Sprintf("messages[%d]: unknown level %q", i, m.Level))
					return
				}
				m.Level = l
			}
			sm := Sample{Container: strings.TrimPrefix(m.Container, "/"), Image: m.Image, Source: m.Source, Time: m.Time, Data: *m.Data, Level: m.Level}
			if sm.Source == "" {
				sm.Source = "stdout"
			}
			sm.cap()
			in = append(in, sm)
		}
	}

	p, errs, warnings := s.build(*body.Content)
	if len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"errors": issues(errs), "warnings": issues(warnings)})
		return
	}
	routes := s.Watcher.Env.Routes
	results := make([]result, len(in))
	for i, sm := range in {
		if r.Context().Err() != nil {
			return // the client is gone
		}
		results[i] = result{Input: sm, MessageTrace: p.TraceMessage(sm.message(), routes)}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"routes":   routes,
		"results":  results,
		"warnings": issues(warnings),
	})
}

// overflow tells a live client how many messages it missed because it was too slow.
type overflow struct {
	Type    string `json:"type"`
	Dropped int64  `json:"dropped"`
}

// live streams traced messages as they flow through the active pipeline.
//
// There is no Origin check. Requests come through the Supervisor ingress
// proxy, which has authenticated the (admin) user; its Origin is the Home
// Assistant URL, which is not known here. Access is decided on the TCP peer in
// serve, before this runs. The default handler of x/net/websocket would also
// refuse a request without Origin header.
func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	// x/net/websocket hijacks before it looks at the headers.
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		writeError(w, http.StatusBadRequest, "websocket upgrade required")
		return
	}
	q := r.URL.Query()
	glob := strings.TrimPrefix(q.Get("container"), "/")
	if _, err := path.Match(glob, ""); err != nil {
		writeError(w, http.StatusBadRequest, "invalid container glob")
		return
	}
	h := s.samples()
	if !h.reserve() {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("at most %d live clients", maxLiveClients))
		return
	}
	defer h.release()
	c := &client{
		container: glob,
		showAll:   q.Get("show_dropped") == "true",
		ch:        make(chan *result, clientQueue),
		done:      make(chan struct{}),
	}
	websocket.Server{
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(conn *websocket.Conn) {
			// Close writes a close frame under the write lock, which a send that is
			// stuck on a full socket holds. An expired deadline frees it at once.
			c.close = func() {
				conn.SetDeadline(time.Now())
				conn.Close()
			}
			if !h.add(c) {
				return
			}
			defer h.remove(c)
			// The client sends nothing; reading detects that it left.
			go func() {
				io.Copy(io.Discard, conn)
				c.stop()
			}()
			for {
				select {
				case <-c.done:
					return
				case res := <-c.ch:
					if n := c.dropped.Swap(0); n > 0 && send(conn, overflow{"overflow", n}) != nil {
						return
					}
					if send(conn, res) != nil {
						return
					}
				}
			}
		},
	}.ServeHTTP(w, r)
}

func send(conn *websocket.Conn, v any) error {
	conn.SetWriteDeadline(time.Now().Add(liveWriteTimeout))
	err := websocket.JSON.Send(conn, v)
	// The reader answers a ping with a pong on this connection; an old deadline would fail it.
	conn.SetWriteDeadline(time.Time{})
	return err
}
