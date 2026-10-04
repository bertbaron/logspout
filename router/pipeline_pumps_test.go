package router_test

import (
	"fmt"
	"io"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	docker "github.com/fsouza/go-dockerclient"

	"github.com/gliderlabs/logspout/pipeline"
	"github.com/gliderlabs/logspout/router"
)

const sentinel = "SENTINEL"

// harness feeds lines through one of the two pumps and collects what the routes receive.
type harness struct {
	feed  func(container, source, line string)
	route func(name string) chan *router.Message
}

func journalHarness(t *testing.T) *harness {
	p := router.NewJournalPump()
	return &harness{
		feed: func(container, source, line string) {
			prio := "6"
			if source == "stderr" {
				prio = "3"
			}
			p.Feed(&router.JournalEntry{
				Fields: map[string]string{
					"CONTAINER_NAME": container, "CONTAINER_ID_FULL": "id-" + container,
					"MESSAGE": line, "PRIORITY": prio,
				},
				Realtime: time.Unix(100, 0),
			})
		},
		route: func(name string) chan *router.Message {
			ch := make(chan *router.Message, 1000)
			p.AddStream(ch, newRoute(name))
			return ch
		},
	}
}

func dockerHarness(t *testing.T) *harness {
	type stream struct {
		ch chan *router.Message
		r  *router.Route
	}
	var (
		mu      sync.Mutex
		streams []stream
		pumps   = map[string]map[string]*io.PipeWriter{}
	)
	t.Cleanup(func() {
		for _, w := range pumps {
			for _, c := range w {
				c.Close()
			}
		}
	})
	return &harness{
		feed: func(container, source, line string) {
			mu.Lock()
			w, ok := pumps[container]
			if !ok {
				outr, outw := io.Pipe()
				errr, errw := io.Pipe()
				c := &docker.Container{
					ID: "id-" + container, Name: "/" + container,
					Config: &docker.Config{}, HostConfig: &docker.HostConfig{},
				}
				cp := router.NewContainerPump(c, outr, errr)
				for _, s := range streams {
					cp.Add(s.ch, s.r)
				}
				w = map[string]*io.PipeWriter{"stdout": outw, "stderr": errw}
				pumps[container] = w
			}
			mu.Unlock()
			if _, err := io.WriteString(w[source], line+"\n"); err != nil {
				t.Error(err)
			}
		},
		route: func(name string) chan *router.Message {
			ch := make(chan *router.Message, 1000)
			mu.Lock()
			streams = append(streams, stream{ch, newRoute(name)})
			mu.Unlock()
			return ch
		},
	}
}

var pumpHarnesses = map[string]func(*testing.T) *harness{
	"journal": journalHarness,
	"docker":  dockerHarness,
}

// stderrOnly is the name of a route that has filter.sources=stderr.
const stderrOnly = "stderr-only"

func newRoute(name string) *router.Route {
	r := &router.Route{Name: name}
	if name == stderrOnly {
		r.FilterSources = []string{"stderr"}
	}
	return r
}

// collect returns what ch received until it has seen n sentinel lines. The
// test feeds one sentinel per container and source, because the Docker pump
// reads each of them in its own goroutine.
func collect(t *testing.T, ch chan *router.Message, n int) []*router.Message {
	t.Helper()
	var out []*router.Message
	for {
		select {
		case m := <-ch:
			if m.Data == sentinel {
				if n--; n == 0 {
					return out
				}
				continue
			}
			out = append(out, m)
		case <-time.After(5 * time.Second):
			t.Error("timeout waiting for sentinel")
			return out
		}
	}
}

func install(t *testing.T, o pipeline.Options) {
	t.Helper()
	p, _, err := pipeline.Build(o)
	if err != nil {
		t.Fatal(err)
	}
	router.SetProcessor(p)
	t.Cleanup(func() { router.SetProcessor(nil) })
}

func datas(ms []*router.Message) []string {
	out := []string{}
	for _, m := range ms {
		out = append(out, m.Data)
	}
	return out
}

func parseRules(t *testing.T, yaml string) pipeline.RuleSet {
	t.Helper()
	rs, err := pipeline.ParseRuleSet([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

const (
	haWarning = "2026-10-04 12:00:00.123 WARNING (MainThread) [homeassistant.components.x] Something odd"
	haInfo    = "2026-10-04 12:00:00.123 INFO (MainThread) [homeassistant.core] started"
)

type flat struct {
	Container, Source, Data, Level string
	Fields                         map[string]string
}

func flatten(ms []*router.Message) []flat {
	out := []flat{}
	for _, m := range ms {
		out = append(out, flat{m.Container.Name, m.Source, m.Data, m.Level, m.Fields})
	}
	// Containers are read by separate goroutines in the Docker pump.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Container != out[j].Container {
			return out[i].Container < out[j].Container
		}
		return out[i].Source < out[j].Source
	})
	return out
}

type input struct{ container, source, line string }

var compatInputs = []input{
	{"homeassistant", "stdout", haWarning},
	{"homeassistant", "stdout", "\x1b[32mcolored\x1b[0m"},
	{"addon_core_ssh", "stdout", "[12:00:00] INFO: Starting"},
	{"homeassistant", "stdout", haInfo},
	{"homeassistant", "stderr", "plain stderr line"},
	{"addon_core_ssh", "stderr", "[12:00:00] ERROR: Failed"},
}

// run feeds the inputs, then a sentinel for every container and source, and
// returns what each route received.
func run(t *testing.T, h *harness, routes []string, inputs []input) map[string][]*router.Message {
	t.Helper()
	chans := map[string]chan *router.Message{}
	for _, r := range routes {
		chans[r] = h.route(r)
	}
	for _, in := range inputs {
		h.feed(in.container, in.source, in.line)
	}
	type key struct{ container, source string }
	seen := map[key]bool{}
	stderrs := 0
	for _, in := range inputs {
		k := key{in.container, in.source}
		if seen[k] {
			continue
		}
		seen[k] = true
		if in.source == "stderr" {
			stderrs++
		}
		h.feed(in.container, in.source, sentinel)
	}
	out := map[string][]*router.Message{}
	for r, ch := range chans {
		n := len(seen)
		if r == stderrOnly {
			n = stderrs
		}
		out[r] = collect(t, ch, n)
	}
	return out
}

// Without options the output is exactly what it is without a processor.
func TestPipelineBackwardCompatible(t *testing.T) {
	routes := []string{"a", "b", stderrOnly}
	for name, mk := range pumpHarnesses {
		t.Run(name, func(t *testing.T) {
			router.SetProcessor(nil)
			unset := run(t, mk(t), routes, compatInputs)

			install(t, pipeline.Options{})
			built := run(t, mk(t), routes, compatInputs)

			for _, r := range routes {
				want := len(compatInputs)
				if r == stderrOnly {
					want = 2
				}
				if len(unset[r]) != want {
					t.Fatalf("route %s got %d messages, want %d", r, len(unset[r]), want)
				}
				if !reflect.DeepEqual(flatten(unset[r]), flatten(built[r])) {
					t.Errorf("route %s differs:\nunset: %+v\nbuilt: %+v", r, flatten(unset[r]), flatten(built[r]))
				}
				for _, m := range built[r] {
					if m.Level != "" || m.Fields != nil {
						t.Errorf("route %s: level/fields set: %+v", r, m)
					}
					if r == stderrOnly && m.Source != "stderr" {
						t.Errorf("route %s got source %s", r, m.Source)
					}
				}
			}
		})
	}
}

func TestPipelineDefaultRules(t *testing.T) {
	tests := []struct {
		selector  string
		wantLevel string
		wantField bool
	}{
		{"", "", false},
		{"off", "", false},
		{"latest", "warning", true},
		{"v1", "warning", true},
	}
	for name, mk := range pumpHarnesses {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("%s/%q", name, tt.selector), func(t *testing.T) {
				install(t, pipeline.Options{DefaultRules: tt.selector})
				got := run(t, mk(t), []string{"a", "b"}, []input{{"homeassistant", "stdout", haWarning}})
				for _, r := range []string{"a", "b"} {
					if len(got[r]) != 1 {
						t.Fatalf("route %s: %d messages", r, len(got[r]))
					}
					m := got[r][0]
					if m.Level != tt.wantLevel || (m.Fields != nil) != tt.wantField || m.Data != haWarning {
						t.Errorf("route %s: %+v", r, m)
					}
				}
			})
		}
	}
}

func TestPipelineExcludeContainers(t *testing.T) {
	inputs := []input{
		{"homeassistant", "stdout", "excluded"},
		{"addon_abc_mosquitto", "stdout", "excluded by glob"},
		{"addon_abc_zigbee", "stdout", "kept"},
		{"homeassistant2", "stdout", "kept, no prefix match"},
		{"addon_abc_zigbee", "stdout", "kept too"},
	}
	for name, mk := range pumpHarnesses {
		t.Run(name, func(t *testing.T) {
			install(t, pipeline.Options{ExcludeContainers: []string{"homeassistant", "addon_*_mosquitto"}})
			h := mk(t)
			chans := map[string]chan *router.Message{"a": h.route("a"), "b": h.route("b")}
			for _, in := range inputs {
				h.feed(in.container, in.source, in.line)
			}
			// Containers have their own goroutine in the Docker pump, so wait per container.
			want := []string{"kept", "kept, no prefix match", "kept too"}
			for r, ch := range chans {
				var got []string
				deadline := time.After(5 * time.Second)
				for len(got) < len(want) {
					select {
					case m := <-ch:
						got = append(got, m.Data)
					case <-deadline:
						t.Fatalf("route %s: got %v", r, got)
					}
				}
				if len(got) != len(want) {
					t.Errorf("route %s: %v, want %v", r, got, want)
				}
				select {
				case m := <-ch:
					t.Errorf("route %s: extra message %q", r, m.Data)
				case <-time.After(100 * time.Millisecond):
				}
			}
		})
	}
}

func TestPipelineGlobalAndTargetDrop(t *testing.T) {
	inputs := []input{
		{"c1", "stdout", "noise"},
		{"c1", "stdout", "only-not-in-b"},
		{"c1", "stdout", "normal"},
	}
	for name, mk := range pumpHarnesses {
		t.Run(name, func(t *testing.T) {
			install(t, pipeline.Options{
				Rules: parseRules(t, "- {name: g, when: {match: '^noise$'}, drop: true}"),
				Targets: map[string]pipeline.RuleSet{
					"b": parseRules(t, "- {name: t, when: {match: 'only-not-in-b'}, drop: true}"),
				},
			})
			got := run(t, mk(t), []string{"a", "b", "c"}, inputs)
			want := map[string][]string{
				"a": {"only-not-in-b", "normal"},
				"b": {"normal"},
				"c": {"only-not-in-b", "normal"},
			}
			for r, w := range want {
				if !reflect.DeepEqual(datas(got[r]), w) {
					t.Errorf("route %s: %v, want %v", r, datas(got[r]), w)
				}
			}
		})
	}
}

// A target rule changes a copy: other routes see the original, also when the
// routes are consumed concurrently.
func TestPipelineTargetCopyOnWrite(t *testing.T) {
	rulesA := parseRules(t, `
- name: rewrite
  when: {match: Something}
  set: {level: error, message: 'rewritten ${message}', fields.team: a, fields.logger: changed}
`)
	rulesC := parseRules(t, "- {name: c, when: {match: Something}, set: {level: critical}}")
	const writers, perWriter = 3, 50
	for name, mk := range pumpHarnesses {
		t.Run(name, func(t *testing.T) {
			// v1 gives homeassistant messages a level and a logger field, so Fields is non-nil before the target stage.
			install(t, pipeline.Options{
				DefaultRules: "v1",
				Targets:      map[string]pipeline.RuleSet{"a": rulesA, "c": rulesC},
			})
			h := mk(t)
			chans := map[string]chan *router.Message{}
			for _, r := range []string{"a", "b", "c"} {
				chans[r] = h.route(r)
			}
			var wg sync.WaitGroup
			for i := 0; i < writers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for j := 0; j < perWriter; j++ {
						h.feed("homeassistant", "stdout", haWarning)
					}
				}()
			}
			wg.Wait()
			h.feed("homeassistant", "stdout", sentinel)

			got := map[string][]*router.Message{}
			var cwg sync.WaitGroup
			var mu sync.Mutex
			for r, ch := range chans {
				cwg.Add(1)
				go func() {
					defer cwg.Done()
					ms := collect(t, ch, 1)
					for _, m := range ms { // reads race with the other routes if messages are shared
						_ = m.Level
						_ = m.Fields["logger"]
					}
					mu.Lock()
					got[r] = ms
					mu.Unlock()
				}()
			}
			cwg.Wait()

			for r, ms := range got {
				if len(ms) != writers*perWriter {
					t.Fatalf("route %s: %d messages", r, len(ms))
				}
			}
			for _, m := range got["b"] {
				if m.Level != "warning" || m.Data != haWarning || m.Fields["logger"] != "homeassistant.components.x" || m.Fields["team"] != "" {
					t.Fatalf("route b saw a change: %+v", m)
				}
			}
			for _, m := range got["a"] {
				if m.Level != "error" || m.Data != "rewritten "+haWarning || m.Fields["team"] != "a" || m.Fields["logger"] != "changed" {
					t.Fatalf("route a: %+v", m)
				}
			}
			for _, m := range got["c"] {
				if m.Level != "critical" || m.Data != haWarning || m.Fields["team"] != "" || m.Fields["logger"] != "homeassistant.components.x" {
					t.Fatalf("route c: %+v", m)
				}
			}
		})
	}
}
