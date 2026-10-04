package router_test

import (
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/gliderlabs/logspout/pipeline"
	"github.com/gliderlabs/logspout/router"
)

type recordingTap struct {
	mu      sync.Mutex
	seen    []string
	procs   int
	changed int
}

func (r *recordingTap) Observe(m *router.Message, p router.Processor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, m.Data)
	if p != nil {
		r.procs++
	}
	if m.Level != "" || m.Fields != nil {
		r.changed++
	}
}

func TestTapSeesRawMessagesBeforePipeline(t *testing.T) {
	rules := parseRules(t, "- name: rewrite\n  when: { match: one }\n  set: { level: error, message: changed }\n- name: dropper\n  when: { match: two }\n  drop: true\n")
	for name, mk := range pumpHarnesses {
		t.Run(name, func(t *testing.T) {
			install(t, pipeline.Options{Rules: rules})
			tap := &recordingTap{}
			router.SetTap(tap)
			t.Cleanup(func() { router.SetTap(nil) })

			in := []input{{"c1", "stdout", "one"}, {"c1", "stdout", "two"}, {"c1", "stdout", "three"}}
			out := run(t, mk(t), []string{"a"}, in)

			if got := datas(out["a"]); !reflect.DeepEqual(got, []string{"changed", "three"}) {
				t.Errorf("route got %v", got)
			}
			tap.mu.Lock()
			defer tap.mu.Unlock()
			sort.Strings(tap.seen)
			if want := []string{sentinel, "one", "three", "two"}; !reflect.DeepEqual(tap.seen, want) {
				t.Errorf("tap saw %v, want %v (also the dropped line, unchanged)", tap.seen, want)
			}
			if tap.procs != len(tap.seen) || tap.changed != 0 {
				t.Errorf("processor passed %d times, %d messages already changed", tap.procs, tap.changed)
			}
		})
	}
}

func TestNoTapChangesNothing(t *testing.T) {
	routes := []string{"a", "b", stderrOnly}
	for name, mk := range pumpHarnesses {
		t.Run(name, func(t *testing.T) {
			router.SetTap(nil)
			if router.CurrentTap() != nil {
				t.Fatal("tap set")
			}
			without := run(t, mk(t), routes, compatInputs)

			router.SetTap(&recordingTap{})
			t.Cleanup(func() { router.SetTap(nil) })
			with := run(t, mk(t), routes, compatInputs)
			for _, r := range routes {
				if !reflect.DeepEqual(flatten(without[r]), flatten(with[r])) {
					t.Errorf("route %s differs with a tap", r)
				}
			}
		})
	}
}
