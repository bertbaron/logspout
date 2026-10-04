package router_test

import (
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	docker "github.com/fsouza/go-dockerclient"

	"github.com/gliderlabs/logspout/pipeline"
	"github.com/gliderlabs/logspout/router"
)

// Routes without rules get the very same message pointer, so nothing can differ from before.
func TestPipelineRouteWithoutRulesGetsSameMessage(t *testing.T) {
	p := router.NewJournalPump()
	a, b := make(chan *router.Message, 4), make(chan *router.Message, 4)
	p.AddStream(a, &router.Route{Name: "a"})
	p.AddStream(b, &router.Route{Name: "b"})
	install(t, pipeline.Options{Targets: map[string]pipeline.RuleSet{
		"b": parseRules(t, "- name: x\n  set: {level: error}\n"),
	}})
	m := &router.Message{Data: "x", Source: "stdout", Container: jc("/c")}
	p.Dispatch(m)
	if got := <-a; got != m {
		t.Error("route without rules got a copy")
	}
	if got := <-b; got == m || got.Level != "error" || m.Level != "" {
		t.Errorf("route with rules: got %p level %q, original level %q", got, got.Level, m.Level)
	}
}

func TestPipelineEmptyMessage(t *testing.T) {
	for name, mk := range pumpHarnesses {
		t.Run(name, func(t *testing.T) {
			install(t, pipeline.Options{
				DefaultRules:      "latest",
				ExcludeContainers: []string{"other"},
				Rules:             parseRules(t, "- name: r\n  when: {match: 'x'}\n  set: {level: error}\n"),
			})
			got := run(t, mk(t), []string{"a"}, []input{{"homeassistant", "stdout", ""}, {"homeassistant", "stderr", ""}})
			// The journal pump has always skipped empty lines; the Docker pump delivers them.
			want := 2
			if name == "journal" {
				want = 0
			}
			if len(got["a"]) != want {
				t.Fatalf("got %d messages, want %d: %+v", len(got["a"]), want, flatten(got["a"]))
			}
			for _, m := range got["a"] {
				if m.Data != "" {
					t.Errorf("data = %q", m.Data)
				}
			}
		})
	}
}

// SetProcessor must be safe while both pumps are dispatching. Run with -race.
func TestPipelineSwapProcessorWhileFlowing(t *testing.T) {
	t.Cleanup(func() { router.SetProcessor(nil) })
	jp := router.NewJournalPump()
	jch := make(chan *router.Message, 10)
	jp.AddStream(jch, &router.Route{Name: "a"})

	outr, outw := io.Pipe()
	errr, errw := io.Pipe()
	cp := router.NewContainerPump(&docker.Container{ID: "id", Name: "/homeassistant",
		Config: &docker.Config{}, HostConfig: &docker.HostConfig{}}, outr, errr)
	cch := make(chan *router.Message, 10)
	cp.Add(cch, &router.Route{Name: "a"})
	t.Cleanup(func() { outw.Close(); errw.Close() })

	var received atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, ch := range []chan *router.Message{jch, cch} {
		go func() {
			for {
				select {
				case <-ch:
					received.Add(1)
				case <-stop:
					return
				}
			}
		}()
	}
	pls := []*pipeline.Pipeline{}
	for _, o := range []pipeline.Options{
		{}, {DefaultRules: "latest"}, {DefaultRules: "v1"},
		{ExcludeContainers: []string{"nomatch"}},
		{Targets: map[string]pipeline.RuleSet{"a": parseRules(t, "- name: x\n  set: {level: error}\n")}},
	} {
		p, _, err := pipeline.Build(o)
		if err != nil {
			t.Fatal(err)
		}
		pls = append(pls, p)
	}
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			if i%7 == 0 {
				router.SetProcessor(nil)
			} else {
				router.SetProcessor(pls[i%len(pls)])
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			jp.Dispatch(&router.Message{Data: haWarning, Source: "stdout", Container: jc("/homeassistant")})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			io.WriteString(outw, haWarning+"\n")
		}
	}()
	wg.Wait()
	deadline := time.After(5 * time.Second)
	for received.Load() < 1000 {
		select {
		case <-deadline:
			t.Fatalf("received %d of 1000", received.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(stop)
}

// Routes may come and go while messages are dispatched with a processor installed.
func TestPipelineRoutesChangeDuringDispatch(t *testing.T) {
	install(t, pipeline.Options{
		DefaultRules: "latest",
		Targets: map[string]pipeline.RuleSet{
			"dyn": parseRules(t, "- name: x\n  set: {level: error}\n"),
		},
	})
	jp := router.NewJournalPump()
	cp := router.NewContainerPump(&docker.Container{ID: "id", Name: "/homeassistant",
		Config: &docker.Config{}, HostConfig: &docker.HostConfig{}}, io.NopCloser(&blockingReader{}), io.NopCloser(&blockingReader{}))
	stable := make(chan *router.Message, 100000)
	jp.AddStream(stable, &router.Route{Name: "stable"})
	cp.Add(stable, &router.Route{Name: "stable"})

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			ch := make(chan *router.Message, 1000)
			jp.AddStream(ch, &router.Route{Name: "dyn"})
			cp.Add(ch, &router.Route{Name: "dyn"})
			jp.RemoveStream(ch)
			cp.Remove(ch)
		}
		close(done)
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			m := func() *router.Message {
				return &router.Message{Data: haWarning, Source: "stdout", Container: jc("/homeassistant")}
			}
			jp.Dispatch(m())
			cp.Send(m())
		}
	}()
	wg.Wait()
	if len(stable) == 0 {
		t.Error("stable route received nothing")
	}
	for len(stable) > 0 {
		if m := <-stable; m.Level != "warning" {
			t.Fatalf("level = %q", m.Level)
		}
	}
}

type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) { select {} }

func jc(name string) *docker.Container {
	return &docker.Container{ID: "id" + name, Name: name, Config: &docker.Config{}}
}

// A line that Docker split in parts reaches the processor once, as a whole.
func TestPipelineJournalPartialMessages(t *testing.T) {
	install(t, pipeline.Options{DefaultRules: "latest"})
	p := router.NewJournalPump()
	ch := make(chan *router.Message, 10)
	p.AddStream(ch, &router.Route{Name: "a"})
	entry := func(msg, partial string) *router.JournalEntry {
		f := map[string]string{"CONTAINER_NAME": "homeassistant", "CONTAINER_ID_FULL": "id", "MESSAGE": msg, "PRIORITY": "6"}
		if partial != "" {
			f["CONTAINER_PARTIAL_MESSAGE"] = partial
		}
		return &router.JournalEntry{Fields: f}
	}
	p.Feed(entry("2026-10-04 12:00:00.123 WARNING (MainThread) [x] ", "true"))
	p.Feed(entry("part two", "true"))
	if len(ch) != 0 {
		t.Fatal("partial message was dispatched")
	}
	p.Feed(entry("end", ""))
	m := <-ch
	if m.Data != "2026-10-04 12:00:00.123 WARNING (MainThread) [x] part twoend" || m.Level != "warning" || len(ch) != 0 {
		t.Errorf("got %+v (%d more)", m, len(ch))
	}
}
