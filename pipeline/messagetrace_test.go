package pipeline

import (
	"reflect"
	"testing"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/gliderlabs/logspout/router"
)

func TestTraceMessageAgreesWithGlobalAndTarget(t *testing.T) {
	rs := func(y string) RuleSet {
		r, err := ParseRuleSet([]byte(y))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	p, _, err := Build(Options{
		DefaultRules:      "latest",
		ExcludeContainers: []string{"excl*"},
		Rules:             rs("- name: drop\n  when: { match: noisy }\n  drop: true\n- name: msg\n  when: { match: 'x (?P<v>\\w+)' }\n  set: { message: 'got ${v}', fields.k: '${v}' }\n"),
		Targets: map[string]RuleSet{
			"a": rs("- name: a-lvl\n  when: { match: got }\n  set: { level: warning }\n"),
			"b": rs("- name: b-drop\n  when: { level: debug }\n  drop: true\n"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	routes := []string{"a", "b", "c"}
	mk := func(c, src, data, level string) *router.Message {
		return &router.Message{Container: &docker.Container{Name: "/" + c, Config: &docker.Config{}}, Source: src, Data: data, Level: level}
	}
	for _, m := range []*router.Message{
		mk("c1", "stdout", "x abc", ""),
		mk("c1", "stderr", "noisy", ""),
		mk("excluded", "stdout", "hi", ""),
		mk("c1", "stdout", "plain", "debug"),
	} {
		orig := CloneMessage(m)
		tr := p.TraceMessage(m, routes)
		if !reflect.DeepEqual(m, orig) {
			t.Fatalf("TraceMessage changed its input: %+v", m)
		}

		g := CloneMessage(m)
		dropped := p.Global(g)
		if tr.Global.Dropped != dropped {
			t.Errorf("%q: global dropped %t, real %t", m.Data, tr.Global.Dropped, dropped)
		}
		if dropped {
			if tr.Global.DroppedBy == nil || !tr.Dropped {
				t.Errorf("%q: %+v", m.Data, tr)
			}
			continue
		}
		if tr.Global.Message != g.Data || tr.Global.Level != g.Level || !reflect.DeepEqual(tr.Global.Fields, g.Fields) {
			t.Errorf("%q: global result %+v, real %+v", m.Data, tr.Global, g)
		}
		for _, tt := range tr.Targets {
			out, d := p.Target(tt.Name, g)
			if tt.Sent == d {
				t.Errorf("%q target %s: sent %t, real dropped %t", m.Data, tt.Name, tt.Sent, d)
			}
			if out != nil && (tt.Message != out.Data || tt.Level != out.Level || !reflect.DeepEqual(tt.Fields, out.Fields)) {
				t.Errorf("%q target %s: %+v, real %+v", m.Data, tt.Name, tt, out)
			}
		}
	}

	var nilP *Pipeline
	tr := nilP.TraceMessage(mk("c", "stdout", "x", ""), routes)
	if tr.Dropped || len(tr.Targets) != 3 || !tr.Targets[0].Sent || tr.Global.Message != "x" || tr.Global.Rules == nil {
		t.Errorf("nil pipeline: %+v", tr)
	}
	if tr := nilP.TraceMessage(mk("c", "stdout", "x", ""), nil); tr.Dropped || tr.Targets == nil {
		t.Errorf("no routes: %+v", tr)
	}
}
