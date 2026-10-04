package ingress

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gliderlabs/logspout/pipeline"
	"github.com/gliderlabs/logspout/router"
)

func TestTestEndpointDataBudget(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	line := strings.Repeat("x", MaxSampleData)
	mk := func(n int) []map[string]any {
		out := make([]map[string]any, n)
		for i := range out {
			out[i] = map[string]any{"data": line}
		}
		return out
	}
	if rec := f.do("POST", "/api/test", testBody(t, "", mk(MaxTestData/MaxSampleData))); rec.Code != 200 {
		t.Errorf("at the budget: %d %.100s", rec.Code, rec.Body)
	}
	if rec := f.do("POST", "/api/test", testBody(t, "", mk(MaxTestData/MaxSampleData+1))); rec.Code != 413 {
		t.Errorf("over the budget: %d", rec.Code)
	}
}

func TestTestEndpointOneAtATime(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	f.srv.testBusy.Store(true)
	rec := f.do("POST", "/api/test", testBody(t, "", nil))
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Errorf("busy: %d", rec.Code)
	}
	f.srv.testBusy.Store(false)
	if rec := f.do("POST", "/api/test", testBody(t, "", nil)); rec.Code != 200 {
		t.Errorf("free: %d", rec.Code)
	}
	if f.srv.testBusy.Load() {
		t.Error("slot not released")
	}
	// The slot is also released after an error.
	f.do("POST", "/api/test", "nope")
	if f.srv.testBusy.Load() {
		t.Error("slot kept after an error")
	}
}

func TestTestEndpointStopsWhenClientGone(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequestWithContext(ctx, "POST", "/api/test", strings.NewReader(testBody(t, "", []map[string]any{{"data": "x"}})))
	r.RemoteAddr = testIP + ":1"
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, r)
	if rec.Body.Len() != 0 {
		t.Errorf("worked on for a gone client: %s", rec.Body)
	}
}

func TestTestEndpointLevelAndBadRequestText(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	var resp testResponse
	rec := f.do("POST", "/api/test", testBody(t, "", []map[string]any{{"data": "a", "level": "WARN"}, {"data": "b", "level": "Information"}}))
	must(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	if rec.Code != 200 || resp.Results[0].Input.Level != "warning" || resp.Results[1].Input.Level != "info" {
		t.Errorf("levels: %d %s", rec.Code, rec.Body)
	}
	rec = f.do("POST", "/api/test", testBody(t, "", []map[string]any{{"data": "a"}, {"data": "b", "level": "loud"}}))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "messages[1]") {
		t.Errorf("unknown level: %d %s", rec.Code, rec.Body)
	}
	msg := decode(t, f.do("POST", "/api/test", "nope"))["error"].(string)
	if !strings.Contains(msg, `"messages"`) || !strings.Contains(msg, "invalid character") {
		t.Errorf("test body error: %s", msg)
	}
	msg = decode(t, f.do("POST", "/api/validate", "nope"))["error"].(string)
	if strings.Contains(msg, "messages") || !strings.Contains(msg, "invalid character") {
		t.Errorf("validate body error: %s", msg)
	}
}

func TestObserveTracesOnlyForMatchingClients(t *testing.T) {
	var traced atomic.Int32
	h := &hub{routes: func() []string { traced.Add(1); return nil }}
	c := &client{container: "c2", ch: make(chan *result, 4), done: make(chan struct{})}
	h.add(c)
	h.Observe(cmsg("c1", "stdout", "x"), nil)
	if traced.Load() != 0 || len(c.ch) != 0 {
		t.Fatalf("traced %d for a container nobody wants", traced.Load())
	}
	h.Observe(cmsg("c2", "stdout", "x"), nil)
	if traced.Load() != 1 || len(c.ch) != 1 {
		t.Fatalf("traced %d, queued %d", traced.Load(), len(c.ch))
	}
}

func TestLiveClientLimitAndBadGlob(t *testing.T) {
	l := startLive(t, "127.0.0.1")
	hdr := map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"}
	if rec := l.doFrom("127.0.0.1:1", "GET", "/api/live?container=%5Bx", "", hdr); rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid container glob") {
		t.Errorf("bad glob: %d %s", rec.Code, rec.Body)
	}
	if _, err := l.dial("?container=%5Bx"); err == nil {
		t.Error("websocket accepted with a bad glob")
	}
	for i := 0; i < maxLiveClients; i++ {
		l.connect(t, "")
	}
	if rec := l.doFrom("127.0.0.1:1", "GET", "/api/live", "", hdr); rec.Code != 503 {
		t.Errorf("over the limit: %d", rec.Code)
	}
	if ws, err := l.dial(""); err == nil {
		ws.Close()
		t.Error("websocket accepted over the limit")
	}
}

func TestLivePingAnsweredOnIdleStream(t *testing.T) {
	old := liveWriteTimeout
	liveWriteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { liveWriteTimeout = old })

	l := startLive(t, "127.0.0.1")
	conn, err := net.Dial("tcp", l.addr)
	must(t, err)
	t.Cleanup(func() { conn.Close() })
	io.WriteString(conn, "GET /api/live HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nOrigin: http://x\r\n\r\n")
	br := bufio.NewReader(conn)
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	for {
		line, err := br.ReadString('\n')
		must(t, err)
		if line == "\r\n" {
			break
		}
	}
	waitFor(t, func() bool { return l.srv.samples().nlive.Load() == 1 })
	pump(cmsg("c1", "stdout", "hello"))
	if op, _ := readFrame(t, br); op != 0x1 {
		t.Fatalf("first frame opcode %x", op)
	}
	time.Sleep(2 * liveWriteTimeout)           // the deadline of the last send has passed
	conn.Write([]byte{0x89, 0x80, 0, 0, 0, 0}) // masked ping, no payload
	if op, _ := readFrame(t, br); op != 0xA {
		t.Fatalf("expected a pong, got opcode %x", op)
	}
}

// readFrame reads one unmasked server frame.
func readFrame(t *testing.T, br *bufio.Reader) (op byte, payload []byte) {
	t.Helper()
	h := make([]byte, 2)
	_, err := io.ReadFull(br, h)
	must(t, err)
	n := int(h[1] & 0x7f)
	if n == 126 {
		ext := make([]byte, 2)
		_, err = io.ReadFull(br, ext)
		must(t, err)
		n = int(ext[0])<<8 | int(ext[1])
	}
	payload = make([]byte, n)
	_, err = io.ReadFull(br, payload)
	must(t, err)
	return h[0] & 0x0f, payload
}

func TestServeErrorRemovesTap(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	t.Cleanup(func() { router.SetTap(nil) })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	ln.Close() // Serve fails on the first Accept
	if err := f.srv.Serve(ln); err == nil {
		t.Fatal("no error")
	}
	if router.CurrentTap() != nil {
		t.Error("tap still installed after Serve failed")
	}
}

// A handshake that fails after the slot was reserved must free the slot.
func TestLiveFailedHandshakeFreesSlot(t *testing.T) {
	l := startLive(t, "127.0.0.1")
	h := l.srv.samples()
	for i := 0; i < 2*maxLiveClients; i++ {
		conn, err := net.Dial("tcp", l.addr)
		must(t, err)
		io.WriteString(conn, "GET /api/live HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 99\r\nOrigin: http://x\r\n\r\n")
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		io.Copy(io.Discard, conn) // the server answers and closes
		conn.Close()
	}
	waitFor(t, func() bool { return h.slots.Load() == 0 })
	if h.nlive.Load() != 0 {
		t.Errorf("nlive %d", h.nlive.Load())
	}
	l.connect(t, "") // still room
}
