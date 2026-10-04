package pipeline

import (
	"strings"
	"testing"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/gliderlabs/logspout/router"
)

func TestBuildEmptyAndSummary(t *testing.T) {
	p, unknown, err := Build(Options{})
	if err != nil || unknown != nil || !p.Empty() {
		t.Fatalf("empty: %v %v %v", p, unknown, err)
	}
	m := &router.Message{Data: "x"}
	if p.Global(m) || m.Level != "" || m.Fields != nil {
		t.Errorf("empty pipeline changed the message: %+v", m)
	}
	if out, dropped := p.Target("a", m); out != m || dropped {
		t.Error("route without rules must get the same message, not a copy")
	}
	if s := p.Summary(); !strings.Contains(s, "default rules off, 0 user rules (0 global)") || !strings.Contains(s, "excluded containers: none") {
		t.Errorf("summary %q", s)
	}

	p, _, err = Build(Options{
		DefaultRules:      "v1",
		DisabledDefaults:  []string{"addon-bashio", "nope"},
		ExcludeContainers: []string{"a", "b*"},
		Targets:           map[string]RuleSet{"loki": {{Name: "t", Drop: true}}},
	})
	if err != nil || p.Empty() {
		t.Fatal(err)
	}
	s := p.Summary()
	for _, want := range []string{"default rules v1", "excluded containers: a,b*", "targets with rules: loki(1)"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary %q lacks %q", s, want)
		}
	}
}

func TestBuildErrors(t *testing.T) {
	for name, o := range map[string]Options{
		"default": {DefaultRules: "v99"},
		"glob":    {ExcludeContainers: []string{"a["}},
		"rules":   {Rules: RuleSet{{Name: "bad", Parse: "nope"}}},
		"target":  {Targets: map[string]RuleSet{"x": {{Name: "bad", Parse: "nope"}}}},
	} {
		if _, _, err := Build(o); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	_, unknown, err := Build(Options{DefaultRules: "v1", DisabledDefaults: []string{"nope"}})
	if err != nil || len(unknown) != 1 {
		t.Errorf("unknown disabled: %v %v", unknown, err)
	}
}

// Excluded containers are dropped before the defaults classify them.
func TestPipelineOrder(t *testing.T) {
	p, _, err := Build(Options{
		DefaultRules:      "v1",
		ExcludeContainers: []string{"homeassistant"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := &router.Message{
		Container: &docker.Container{Name: "/homeassistant"},
		Data:      "2026-10-04 12:00:00.123 INFO (MainThread) [homeassistant.core] started",
	}
	if !p.Global(m) {
		t.Error("excluded container not dropped")
	}
	if m.Level != "" || m.Fields != nil {
		t.Errorf("excluded message was classified: %+v", m)
	}

	// A default `stop` does not skip the user's rules.
	p, _, err = Build(Options{
		DefaultRules: "v1",
		Rules:        RuleSet{{Name: "u", When: &Condition{Container: StringList{"homeassistant"}}, Drop: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Level = ""
	if !p.Global(m) {
		t.Error("user rule skipped after a default stop")
	}
}
