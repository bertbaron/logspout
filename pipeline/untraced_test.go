package pipeline

import "testing"

// A trace line stays untraced through global and target stages, with and without rewrites;
// an ordinary line is still traced.
func TestUntracedFlagAcrossStages(t *testing.T) {
	logs := captureLog(t)
	p, _, err := Build(Options{
		Debug:   true,
		Rules:   mustRules(t, "- name: a\n  set: { message: rewritten }\n"),
		Targets: map[string]RuleSet{"syslog": mustRules(t, "- name: b\n  set: { message: again }\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := wmsg(traceMarker + "x")
	p.Global(m)
	if !m.Untraced {
		t.Fatal("flag not set by the global stage")
	}
	out, _ := p.Target("syslog", m)
	if out == nil || !out.Untraced {
		t.Error("flag lost in the target copy")
	}
	p.Target("other", m)
	if logs() != "" {
		t.Errorf("trace line traced:\n%s", logs())
	}

	n := wmsg("ordinary line")
	p.Global(n)
	p.Target("syslog", n)
	if n.Untraced || logs() == "" {
		t.Errorf("ordinary line not traced (untraced=%v)", n.Untraced)
	}
}
