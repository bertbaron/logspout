package router_test

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gliderlabs/logspout/ingress"
	"github.com/gliderlabs/logspout/pipeline"
	"github.com/gliderlabs/logspout/router"
	"golang.org/x/net/websocket"
)

const liveRules = `
rules:
  - name: drop-noisy
    when: { match: noisy }
    drop: true
  - name: rewrite
    when: { match: 'ERR (?P<code>\d+)' }
    set: { level: error, message: 'failed ${code}', fields.code: '${code}' }
targets:
  syslog:
    rules:
      - name: syslog-drops-core
        when: { match: 'only-not-syslog' }
        drop: true
  loki:
    rules:
      - name: loki-no-debug
        when: { level: '<info' }
        drop: true
      - name: loki-tag
        when: { match: 'failed' }
        set: { fields.tag: loki }
`

type liveTarget struct {
	Name    string            `json:"name"`
	Sent    bool              `json:"sent"`
	Message string            `json:"message"`
	Level   string            `json:"level"`
	Fields  map[string]string `json:"fields"`
}

type liveResult struct {
	Input struct {
		Container string `json:"container"`
		Data      string `json:"data"`
	} `json:"input"`
	Global struct {
		Dropped  bool `json:"dropped"`
		Excluded bool `json:"excluded"`
	} `json:"global"`
	Targets []liveTarget `json:"targets"`
	Dropped bool         `json:"dropped"`
}

type outcome struct {
	Data, Level, Container string
	Fields                 map[string]string
}

func sorted(o []outcome) []outcome {
	sort.Slice(o, func(i, j int) bool {
		if o[i].Container != o[j].Container {
			return o[i].Container < o[j].Container
		}
		return o[i].Data < o[j].Data
	})
	return o
}

// The live trace must predict exactly what the routes receive, for both pumps:
// global drop, excluded container, per target drop, rewrites and fields.
func TestLiveTraceEqualsWhatRoutesReceive(t *testing.T) {
	inputs := []input{
		{"homeassistant", "stdout", haWarning},
		{"homeassistant", "stdout", "this is noisy"},
		{"homeassistant", "stderr", "ERR 42 boom"},
		{"homeassistant", "stdout", "only-not-syslog line"},
		{"addon_core_ssh", "stdout", "[12:00:00] INFO: Starting"},
		{"addon_core_ssh", "stderr", "[12:00:00] ERROR: Failed"},
		{"excluded_thing", "stdout", "from an excluded container"},
		{"excluded_thing", "stdout", "ERR 7 excluded"},
	}
	routes := []string{"syslog", "loki"}
	for name, mk := range pumpHarnesses {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "logspout.yaml")
			if err := os.WriteFile(path, []byte(liveRules), 0o644); err != nil {
				t.Fatal(err)
			}
			w := &pipeline.Watcher{
				Path: path,
				Env:  pipeline.FileEnv{Routes: routes, DefaultRules: "latest"},
				Base: pipeline.Options{DefaultRules: "latest", ExcludeContainers: []string{"excluded*"}},
			}
			t.Cleanup(func() { w.Stop(); router.SetProcessor(nil) })
			if res := w.Reload(); !res.Applied {
				t.Fatalf("rule file invalid: %+v", res)
			}
			srv := &ingress.Server{Watcher: w, AllowedIP: "127.0.0.1"}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { srv.Serve(ln); close(done) }()
			t.Cleanup(func() { srv.Close(); <-done })
			deadline := time.Now().Add(5 * time.Second)
			for router.CurrentTap() == nil && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			ws, err := websocket.Dial("ws://"+ln.Addr().String()+"/api/live?show_dropped=true", "", "http://ha.example/")
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close()
			// The dial returns before the handler registers the client.
			time.Sleep(200 * time.Millisecond)

			h := mk(t)
			chans := map[string]chan *router.Message{}
			for _, r := range routes {
				chans[r] = h.route(r)
			}
			for _, in := range inputs {
				h.feed(in.container, in.source, in.line)
			}
			type key struct{ c, s string }
			seen := map[key]bool{}
			nonExcluded := 0
			total := len(inputs)
			for _, in := range inputs {
				k := key{in.container, in.source}
				if seen[k] {
					continue
				}
				seen[k] = true
				h.feed(in.container, in.source, sentinel)
				total++
				if in.container != "excluded_thing" {
					nonExcluded++
				}
			}

			expect := map[string][]outcome{}
			for i := 0; i < total; i++ {
				var r liveResult
				ws.SetReadDeadline(time.Now().Add(5 * time.Second))
				if err := websocket.JSON.Receive(ws, &r); err != nil {
					t.Fatalf("live receive %d of %d: %v", i, total, err)
				}
				if r.Input.Data == sentinel {
					continue
				}
				if r.Input.Container == "excluded_thing" && !r.Global.Excluded {
					t.Errorf("%q: live does not show the exclusion", r.Input.Data)
				}
				for _, tt := range r.Targets {
					if tt.Sent {
						expect[tt.Name] = append(expect[tt.Name], outcome{tt.Message, tt.Level, r.Input.Container, tt.Fields})
					}
				}
			}
			for _, rn := range routes {
				var got []outcome
				for _, m := range collect(t, chans[rn], nonExcluded) {
					got = append(got, outcome{m.Data, m.Level, m.Container.Name[1:], m.Fields})
				}
				g, e := sorted(got), sorted(expect[rn])
				if len(g) != len(e) {
					t.Errorf("route %s: %d messages received, live trace says %d\n got %+v\nlive %+v", rn, len(g), len(e), g, e)
					continue
				}
				for i := range g {
					if g[i].Data != e[i].Data || g[i].Level != e[i].Level || g[i].Container != e[i].Container ||
						!(len(g[i].Fields) == 0 && len(e[i].Fields) == 0 || reflect.DeepEqual(g[i].Fields, e[i].Fields)) {
						t.Errorf("route %s differs:\n got %+v\nlive %+v", rn, g[i], e[i])
					}
				}
			}
		})
	}
}

// feedWriter ships log output like the journal does: asynchronously. A
// synchronous feed would re-enter the pump while it holds its lock.
type feedWriter chan string

func newFeedWriter(feed func(container, source, line string)) feedWriter {
	w := make(feedWriter, 10000)
	go func() {
		for line := range w {
			feed("logspout", "stdout", line)
		}
	}()
	return w
}

func (f feedWriter) Write(p []byte) (int, error) {
	select {
	case f <- strings.TrimSuffix(string(p), "\n"):
	default:
	}
	return len(p), nil
}

// With DEBUG_PIPELINE logspout ships its own trace lines. They must neither
// fill the samples nor reach a live client, in both pumps.
func TestSamplesAndLiveIgnoreTraceLinesEndToEnd(t *testing.T) {
	for name, mk := range pumpHarnesses {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "logspout.yaml")
			if err := os.WriteFile(path, []byte(liveRules), 0o644); err != nil {
				t.Fatal(err)
			}
			w := &pipeline.Watcher{
				Path: path,
				Env:  pipeline.FileEnv{Routes: []string{"syslog", "loki"}},
				Base: pipeline.Options{Debug: true},
			}
			h := mk(t)
			ch := h.route("loki")
			// Create the pump of the "logspout" container now: creating it logs, which would deadlock on the log mutex.
			h.feed("logspout", "stdout", "warm-up")
			t.Cleanup(func() { log.SetOutput(os.Stderr); w.Stop(); router.SetProcessor(nil) })
			if res := w.Reload(); !res.Applied {
				t.Fatalf("rule file invalid: %+v", res)
			}

			srv := &ingress.Server{Watcher: w, AllowedIP: "127.0.0.1"}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { srv.Serve(ln); close(done) }()
			t.Cleanup(func() { srv.Close(); <-done })
			for deadline := time.Now().Add(5 * time.Second); router.CurrentTap() == nil && time.Now().Before(deadline); {
				time.Sleep(5 * time.Millisecond)
			}
			ws, err := websocket.Dial("ws://"+ln.Addr().String()+"/api/live?show_dropped=true", "", "http://ha.example/")
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close()
			time.Sleep(200 * time.Millisecond)

			// Not earlier: other log lines (such as the reload summary) would trace and log under the log mutex.
			log.SetOutput(newFeedWriter(h.feed))
			for i := 0; i < 5; i++ {
				h.feed("c1", "stdout", fmt.Sprintf("ERR %d real line", i))
			}
			h.feed("c1", "stdout", sentinel)
			h.feed("logspout", "stdout", sentinel)
			var real []string
			traces := 0
			for _, m := range collect(t, ch, 2) {
				if pipeline.IsTraceLine(m.Data) {
					traces++
				} else if m.Data != "warm-up" {
					real = append(real, m.Data)
				}
			}
			if traces == 0 {
				t.Error("no trace lines were shipped: the test checks nothing")
			}
			time.Sleep(300 * time.Millisecond) // let any feedback loop show itself

			resp, err := http.Get("http://" + ln.Addr().String() + "/api/samples")
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Samples []struct{ Data string } `json:"samples"`
			}
			json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			nreal := 0
			for _, s := range body.Samples {
				if pipeline.IsTraceLine(s.Data) {
					t.Errorf("trace line in samples: %q", s.Data)
				}
				if strings.Contains(s.Data, "real line") {
					nreal++
				}
			}
			if nreal != 5 {
				t.Errorf("samples hold %d real lines, want 5 (%d samples)", nreal, len(body.Samples))
			}
			ws.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			for n := 0; ; n++ {
				var r liveResult
				if err := websocket.JSON.Receive(ws, &r); err != nil {
					if n < 6 {
						t.Errorf("live got only %d results: %v", n, err)
					}
					break
				}
				if pipeline.IsTraceLine(r.Input.Data) {
					t.Errorf("trace line in live: %q", r.Input.Data)
				}
				if n > 100 {
					t.Fatal("live does not stop: feedback loop")
				}
			}
			if len(real) != 5 {
				t.Errorf("route got %d real lines: %v", len(real), real)
			}
		})
	}
}
