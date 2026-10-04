package router_test

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/gliderlabs/logspout/pipeline"
	"github.com/gliderlabs/logspout/router"
)

type nopAdapter struct{}

func (nopAdapter) Stream(chan *router.Message) {}

// removeAllRoutes empties the global route manager after a test.
func removeAllRoutes(t *testing.T) {
	t.Cleanup(func() {
		rs, _ := router.Routes.GetAll()
		for _, r := range rs {
			go func(r *router.Route) { <-r.Closer() }(r)
			router.Routes.Remove(r.ID)
		}
	})
}

// A rule file is validated against the configured routes only. When a stored
// or API route has the same name, the target rules must apply to neither.
func TestTargetRulesSkippedForNameSharedAtRunTime(t *testing.T) {
	router.AdapterFactories.Unregister("wtest")
	router.AdapterFactories.Register(func(*router.Route) (router.LogAdapter, error) { return nopAdapter{}, nil }, "wtest")
	t.Cleanup(func() { router.AdapterFactories.Unregister("wtest") })
	removeAllRoutes(t)
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	install(t, pipeline.Options{Targets: map[string]pipeline.RuleSet{
		"web": parseRules(t, "- name: drop all\n  drop: true\n"),
	}})
	if err := router.Routes.AddFromURI("wtest://h:1#web"); err != nil {
		t.Fatal(err)
	}
	all, _ := router.Routes.GetAll()
	configured := all[0]

	p := router.NewJournalPump()
	a := make(chan *router.Message, 8)
	p.AddStream(a, configured)
	p.Dispatch(&router.Message{Data: "x", Source: "stdout", Container: jc("/c")})
	if len(a) != 0 {
		t.Fatal("target rule did not apply to a unique name")
	}

	if err := router.Routes.Add(&router.Route{Adapter: "wtest", Address: "api:1", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	all, _ = router.Routes.GetAll()
	b := make(chan *router.Message, 8)
	for _, r := range all {
		if r != configured {
			p.AddStream(b, r)
		}
	}
	for i := 0; i < 3; i++ {
		p.Dispatch(&router.Message{Data: "x", Source: "stdout", Container: jc("/c")})
	}
	if len(a) != 3 || len(b) != 3 {
		t.Errorf("routes got %d and %d messages, want 3 and 3", len(a), len(b))
	}
	if n := strings.Count(logBuf.String(), `target rules for this name are not applied`); n != 1 {
		t.Errorf("warning logged %d times, want 1:\n%s", n, logBuf.String())
	}

	// Removing the clash makes the rule apply again.
	for _, r := range all {
		if r != configured {
			go func(r *router.Route) { <-r.Closer() }(r)
			router.Routes.Remove(r.ID)
		}
	}
	p.Dispatch(&router.Message{Data: "x", Source: "stdout", Container: jc("/c")})
	if len(a) != 3 {
		t.Errorf("rule did not apply again after the clash was removed (%d messages)", len(a))
	}
}

// No warning when the shared name has no target rules.
func TestNoWarningForSharedNameWithoutTargetRules(t *testing.T) {
	router.AdapterFactories.Unregister("wtest")
	router.AdapterFactories.Register(func(*router.Route) (router.LogAdapter, error) { return nopAdapter{}, nil }, "wtest")
	t.Cleanup(func() { router.AdapterFactories.Unregister("wtest") })
	removeAllRoutes(t)
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	install(t, pipeline.Options{Targets: map[string]pipeline.RuleSet{
		"other": parseRules(t, "- name: drop all\n  drop: true\n"),
	}})
	for _, u := range []string{"wtest://a:1#web", "wtest://b:1#web"} {
		if err := router.Routes.AddFromURI(u); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := router.Routes.GetAll()
	p := router.NewJournalPump()
	ch := make(chan *router.Message, 8)
	p.AddStream(ch, all[0])
	p.Dispatch(&router.Message{Data: "x", Source: "stdout", Container: jc("/c")})
	if len(ch) != 1 || strings.Contains(logBuf.String(), "target rules for this name") {
		t.Errorf("messages = %d, log:\n%s", len(ch), logBuf.String())
	}
}
