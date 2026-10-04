package pipeline

import (
	"reflect"
	"strings"
	"sync"
	"testing"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/gliderlabs/logspout/router"
	"gopkg.in/yaml.v3"
)

func msg(container, image, source, data string) *router.Message {
	m := &router.Message{Source: source, Data: data}
	if container != "" || image != "" {
		m.Container = &docker.Container{Name: container, Config: &docker.Config{Image: image}}
	}
	return m
}

func mustCompile(t *testing.T, rules RuleSet) *Compiled {
	t.Helper()
	c, err := Compile(rules)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return c
}

func dropRule(when *Condition, unless *Condition) RuleSet {
	return RuleSet{{Name: "r", When: when, Unless: unless, Drop: true}}
}

func TestConditions(t *testing.T) {
	const ansi = "\x1b[31mfailed\x1b[0m to connect"
	tests := []struct {
		name string
		when *Condition
		m    *router.Message
		want bool
	}{
		{"nil when matches all", nil, msg("a", "", "stdout", "x"), true},
		{"container exact", &Condition{Container: StringList{"homeassistant"}}, msg("/homeassistant", "", "stdout", "x"), true},
		{"container glob", &Condition{Container: StringList{"addon_*_zigbee2mqtt"}}, msg("/addon_45df7312_zigbee2mqtt", "", "stdout", "x"), true},
		{"container glob no match", &Condition{Container: StringList{"addon_*_zigbee2mqtt"}}, msg("/addon_45df7312_mosquitto", "", "stdout", "x"), false},
		{"container list", &Condition{Container: StringList{"a", "b"}}, msg("b", "", "stdout", "x"), true},
		{"container nil container", &Condition{Container: StringList{"*"}}, msg("", "", "stdout", "x"), true},
		{"container nil container exact", &Condition{Container: StringList{"a"}}, msg("", "", "stdout", "x"), false},
		{"image glob across slash", &Condition{Image: StringList{"ghcr.io/*/amd64-addon-x:*"}}, msg("a", "ghcr.io/home-assistant/amd64-addon-x:1.2", "stdout", "x"), true},
		{"image star across slashes", &Condition{Image: StringList{"*zigbee*"}}, msg("a", "ghcr.io/x/y/zigbee:1", "stdout", "x"), true},
		{"image no match", &Condition{Image: StringList{"foo"}}, msg("a", "bar", "stdout", "x"), false},
		{"image missing", &Condition{Image: StringList{"foo"}}, &router.Message{Source: "stdout", Container: &docker.Container{Name: "a"}}, false},
		{"image nil container", &Condition{Image: StringList{"foo"}}, &router.Message{}, false},
		{"source match", &Condition{Source: "stderr"}, msg("a", "", "stderr", "x"), true},
		{"source no match", &Condition{Source: "stderr"}, msg("a", "", "stdout", "x"), false},
		{"level exact derived stderr", &Condition{Level: "error"}, msg("a", "", "stderr", "x"), true},
		{"level exact derived stdout", &Condition{Level: "info"}, msg("a", "", "stdout", "x"), true},
		{"level exact alias", &Condition{Level: "warn"}, &router.Message{Level: "warning"}, true},
		{"level <", &Condition{Level: "<info"}, &router.Message{Level: "debug"}, true},
		{"level < equal", &Condition{Level: "<info"}, &router.Message{Level: "info"}, false},
		{"level <=", &Condition{Level: "<=info"}, &router.Message{Level: "info"}, true},
		{"level >", &Condition{Level: ">warning"}, &router.Message{Level: "error"}, true},
		{"level > equal", &Condition{Level: ">warning"}, &router.Message{Level: "warning"}, false},
		{"level >=", &Condition{Level: ">= warn"}, &router.Message{Level: "warning"}, true},
		{"level =", &Condition{Level: "=info"}, &router.Message{Level: "info"}, true},
		{"level = no", &Condition{Level: "=info"}, &router.Message{Level: "debug"}, false},
		{"level unknown msg level", &Condition{Level: ">=debug"}, &router.Message{Level: "bogus"}, false},
		{"match", &Condition{Match: `to connect$`}, msg("a", "", "stdout", "failed to connect"), true},
		{"match no", &Condition{Match: `^connect`}, msg("a", "", "stdout", "failed to connect"), false},
		{"match strips ansi", &Condition{Match: `^failed to`}, msg("a", "", "stdout", ansi), true},
		{"expr", &Condition{Expr: `source == "stderr" && level == "error"`}, msg("a", "", "stderr", "x"), true},
		{"expr container image", &Condition{Expr: `container == "a" && image startsWith "img"`}, msg("/a", "img:1", "stdout", "x"), true},
		{"expr message ansi", &Condition{Expr: `message startsWith "failed"`}, msg("a", "", "stdout", ansi), true},
		{"expr fields", &Condition{Expr: `fields["logger"] == "x"`}, &router.Message{Fields: map[string]string{"logger": "x"}}, true},
		{"expr nil fields", &Condition{Expr: `fields["logger"] == "x"`}, &router.Message{}, false},
		{"expr false", &Condition{Expr: `level == "debug"`}, msg("a", "", "stdout", "x"), false},
		{"AND all", &Condition{Container: StringList{"a"}, Source: "stderr", Match: "x"}, msg("a", "", "stderr", "x"), true},
		{"AND one fails", &Condition{Container: StringList{"a"}, Source: "stderr", Match: "x"}, msg("a", "", "stdout", "x"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := mustCompile(t, dropRule(tt.when, nil))
			if got := c.Apply(tt.m, nil); got != tt.want {
				t.Errorf("dropped = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUnless(t *testing.T) {
	tests := []struct {
		name string
		m    *router.Message
		want bool
	}{
		{"allow listed kept", msg("a", "", "stdout", "x"), false},
		{"other dropped", msg("c", "", "stdout", "x"), true},
		{"nil container dropped", &router.Message{}, true},
	}
	c := mustCompile(t, dropRule(nil, &Condition{Container: StringList{"a", "b"}}))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.Apply(tt.m, nil); got != tt.want {
				t.Errorf("dropped = %v, want %v", got, tt.want)
			}
		})
	}

	c = mustCompile(t, dropRule(&Condition{Source: "stderr"}, &Condition{Match: "benign"}))
	if c.Apply(msg("a", "", "stderr", "benign warning"), nil) {
		t.Error("unless match must skip the rule")
	}
	if !c.Apply(msg("a", "", "stderr", "bad"), nil) {
		t.Error("when matches and unless does not: want drop")
	}
}

func TestActionOrderAndFlow(t *testing.T) {
	t.Run("parse then set then drop", func(t *testing.T) {
		c := mustCompile(t, RuleSet{{
			Parse: "bashio",
			Set:   map[string]string{"message": "[${level}] ${message}", "fields.kind": "${level}"},
			Drop:  true,
		}})
		m := msg("a", "", "stdout", "[12:00:00] WARNING: hi")
		if !c.Apply(m, nil) {
			t.Fatal("want dropped")
		}
		// ${level} sees the parsed level, so parse ran before set.
		if m.Data != "[warning] [12:00:00] WARNING: hi" || m.Fields["kind"] != "warning" || m.Level != "warning" {
			t.Errorf("got %+v", m)
		}
	})
	t.Run("set values do not see each other", func(t *testing.T) {
		c := mustCompile(t, RuleSet{{Set: map[string]string{"message": "new", "fields.old": "${message}"}}})
		m := msg("a", "", "stdout", "orig")
		c.Apply(m, nil)
		if m.Data != "new" || m.Fields["old"] != "orig" {
			t.Errorf("got %+v", m)
		}
	})
	t.Run("drop ends the list", func(t *testing.T) {
		c := mustCompile(t, RuleSet{
			{Name: "d", Drop: true},
			{Name: "after", Set: map[string]string{"fields.x": "1"}},
		})
		m := msg("a", "", "stdout", "x")
		var tr Trace
		if !c.Apply(m, &tr) || m.Fields != nil || len(tr.Rules) != 1 || !tr.Dropped {
			t.Errorf("got %+v trace %+v", m, tr)
		}
	})
	t.Run("stop skips the rest", func(t *testing.T) {
		c := mustCompile(t, RuleSet{
			{Set: map[string]string{"fields.a": "1"}, Stop: true},
			{Set: map[string]string{"fields.b": "1"}},
		})
		m := msg("a", "", "stdout", "x")
		if c.Apply(m, nil) || !reflect.DeepEqual(m.Fields, map[string]string{"a": "1"}) {
			t.Errorf("got %+v", m)
		}
	})
	t.Run("stop only when rule matches", func(t *testing.T) {
		c := mustCompile(t, RuleSet{
			{When: &Condition{Source: "stderr"}, Stop: true},
			{Set: map[string]string{"fields.b": "1"}},
		})
		m := msg("a", "", "stdout", "x")
		c.Apply(m, nil)
		if m.Fields["b"] != "1" {
			t.Errorf("got %+v", m)
		}
	})
	t.Run("later rule sees earlier change", func(t *testing.T) {
		c := mustCompile(t, RuleSet{
			{Set: map[string]string{"level": "warn"}},
			{When: &Condition{Level: "warning"}, Set: map[string]string{"fields.seen": "yes"}},
		})
		m := msg("a", "", "stdout", "x")
		c.Apply(m, nil)
		if m.Level != "warning" || m.Fields["seen"] != "yes" {
			t.Errorf("got %+v", m)
		}
	})
	t.Run("failing parser changes nothing", func(t *testing.T) {
		c := mustCompile(t, RuleSet{{Parse: "bashio"}})
		m := msg("a", "", "stderr", "plain text")
		c.Apply(m, nil)
		if m.Level != "" || m.Fields != nil || m.Data != "plain text" {
			t.Errorf("got %+v", m)
		}
	})
	t.Run("parser keeps message text", func(t *testing.T) {
		c := mustCompile(t, RuleSet{{Parse: "homeassistant"}})
		in := "\x1b[32m2026-10-04 12:00:00.123 INFO (MainThread) [homeassistant.core] started\x1b[0m"
		m := msg("a", "", "stdout", in)
		c.Apply(m, nil)
		if m.Data != in || m.Level != "info" || m.Fields["logger"] != "homeassistant.core" || m.Fields["thread"] != "MainThread" {
			t.Errorf("got %+v", m)
		}
	})
	t.Run("nil Compiled", func(t *testing.T) {
		var c *Compiled
		if c.Apply(&router.Message{}, nil) || c.Len() != 0 {
			t.Error("nil Compiled must be a no-op")
		}
	})
}

func TestSetAndVariables(t *testing.T) {
	tests := []struct {
		name      string
		rule      Rule
		m         *router.Message
		wantLevel string
		wantData  string
		wantField map[string]string
	}{
		{
			name: "groups to level and message",
			rule: Rule{
				When: &Condition{Match: `^\[(?P<time>[^\]]+)\] (?P<level>\w+): (?P<msg>.*)$`},
				Set:  map[string]string{"level": "${level}", "message": "${msg}"},
			},
			m:         msg("a", "", "stdout", "[t] WARN: disk full"),
			wantLevel: "warning", wantData: "disk full",
		},
		{
			name:      "builtin variables",
			rule:      Rule{Set: map[string]string{"fields.c": "${container}", "fields.i": "${image}", "fields.s": "${source}", "fields.l": "${level}", "fields.m": "${message}"}},
			m:         msg("/web", "img:1", "stderr", "hello"),
			wantLevel: "",
			wantData:  "hello",
			wantField: map[string]string{"c": "web", "i": "img:1", "s": "stderr", "l": "error", "m": "hello"},
		},
		{
			name:      "unknown variable is empty",
			rule:      Rule{When: &Condition{Match: `(?P<nope>zzz)?`}, Set: map[string]string{"fields.x": "a${nope}b"}},
			m:         msg("a", "", "stdout", "x"),
			wantData:  "x",
			wantField: map[string]string{"x": "ab"},
		},
		{
			name:      "dollar dollar is literal",
			rule:      Rule{Set: map[string]string{"fields.x": "cost $$5 $${level}", "fields.y": "$ and $x"}},
			m:         msg("a", "", "stdout", "x"),
			wantData:  "x",
			wantField: map[string]string{"x": "cost $5 ${level}", "y": "$ and $x"},
		},
		{
			name: "group wins over builtin",
			rule: Rule{
				When: &Condition{Match: `(?P<container>\w+)`},
				Set:  map[string]string{"fields.c": "${container}"},
			},
			m:         msg("a", "", "stdout", "zzz"),
			wantData:  "zzz",
			wantField: map[string]string{"c": "zzz"},
		},
		{
			name:      "unknown level after substitution is unchanged",
			rule:      Rule{Set: map[string]string{"level": "${source}"}},
			m:         &router.Message{Level: "notice", Source: "stdout", Data: "x"},
			wantLevel: "notice", wantData: "x",
		},
		{
			name:      "empty level is unchanged",
			rule:      Rule{When: &Condition{Match: `(?P<nope>zzz)?`}, Set: map[string]string{"level": "${nope}"}},
			m:         &router.Message{Level: "debug", Data: "x"},
			wantLevel: "debug", wantData: "x",
		},
		{
			name:      "ansi stripped in variables",
			rule:      Rule{When: &Condition{Match: `(?P<w>\w+)$`}, Set: map[string]string{"message": "${message}!"}},
			m:         msg("a", "", "stdout", "\x1b[31mred\x1b[0m"),
			wantLevel: "", wantData: "red!",
		},
		{
			name:      "existing fields kept",
			rule:      Rule{Set: map[string]string{"fields.b": "2"}},
			m:         &router.Message{Data: "x", Fields: map[string]string{"a": "1"}},
			wantData:  "x",
			wantField: map[string]string{"a": "1", "b": "2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := mustCompile(t, RuleSet{tt.rule})
			c.Apply(tt.m, nil)
			if tt.m.Level != tt.wantLevel || tt.m.Data != tt.wantData {
				t.Errorf("level %q data %q, want %q %q", tt.m.Level, tt.m.Data, tt.wantLevel, tt.wantData)
			}
			if !reflect.DeepEqual(tt.m.Fields, tt.wantField) {
				t.Errorf("fields %v, want %v", tt.m.Fields, tt.wantField)
			}
		})
	}
}

func TestLevelNormalization(t *testing.T) {
	for in, want := range map[string]string{
		"warn": "warning", "WARNING": "warning", "err": "error", "fatal": "critical",
		"crit": "critical", "emerg": "critical", "emergency": "critical", "alert": "critical",
		"information": "info", "trace": "debug", "verbose": "debug", "Notice": "notice",
	} {
		c := mustCompile(t, RuleSet{{Set: map[string]string{"level": in}}})
		m := &router.Message{}
		c.Apply(m, nil)
		if m.Level != want {
			t.Errorf("set level %q: got %q, want %q", in, m.Level, want)
		}
	}
}

func TestCompileErrors(t *testing.T) {
	tests := []struct {
		name string
		rule Rule
		want string
	}{
		{"empty rule", Rule{Name: "empty"}, "no action"},
		{"empty rule with when", Rule{When: &Condition{Source: "stdout"}}, "no action"},
		{"invalid glob", Rule{Drop: true, When: &Condition{Container: StringList{"a[b"}}}, "invalid glob"},
		{"invalid image glob", Rule{Drop: true, When: &Condition{Image: StringList{"[x"}}}, "invalid glob"},
		{"value is empty", Rule{Drop: true, When: &Condition{Container: StringList{""}}}, "value is empty"},
		{"invalid regex", Rule{Drop: true, When: &Condition{Match: "("}}, "match"},
		{"expr repeat disabled", Rule{Drop: true, When: &Condition{Expr: `len(repeat("x", 1000000000)) > 0`}}, "repeat"},
		{"invalid expr syntax", Rule{Drop: true, When: &Condition{Expr: "level =="}}, "expr"},
		{"expr not bool", Rule{Drop: true, When: &Condition{Expr: "level"}}, "expr"},
		{"expr unknown variable", Rule{Drop: true, When: &Condition{Expr: `nope == "x"`}}, "expr"},
		{"unknown parser", Rule{Parse: "nope"}, "unknown parser"},
		{"unknown set key", Rule{Set: map[string]string{"colour": "x"}}, "unknown key"},
		{"bad field prefix", Rule{Set: map[string]string{"field.x": "x"}}, "unknown key"},
		{"invalid level literal", Rule{Set: map[string]string{"level": "loud"}}, "invalid level"},
		{"invalid level condition", Rule{Drop: true, When: &Condition{Level: ">loud"}}, "unknown level"},
		{"invalid level condition empty op", Rule{Drop: true, When: &Condition{Level: "<"}}, "unknown level"},
		{"invalid source", Rule{Drop: true, When: &Condition{Source: "stdin"}}, "source"},
		{"bad field name", Rule{Set: map[string]string{"fields.a b": "x"}}, "invalid field name"},
		{"empty field name", Rule{Set: map[string]string{"fields.": "x"}}, "invalid field name"},
		{"reserved field id", Rule{Set: map[string]string{"fields.id": "x"}}, "reserved"},
		{"unterminated var", Rule{Set: map[string]string{"message": "${x"}}, "unterminated"},
		{"bad var name", Rule{Set: map[string]string{"message": "${a b}"}}, "variable name"},
		{"empty unless", Rule{Drop: true, Unless: &Condition{}}, "unless"},
		{"empty when", Rule{Drop: true, When: &Condition{}}, "when: empty"},
		{"unknown variable", Rule{Set: map[string]string{"message": "${nope}"}}, "unknown variable ${nope}"},
		{"variable from unless group", Rule{Unless: &Condition{Match: `(?P<g>x)`}, Set: map[string]string{"message": "${g}"}}, "unknown variable ${g}"},
		{"duplicate group", Rule{Drop: true, When: &Condition{Match: `(?P<a>x)(?P<a>y)`}}, "duplicate group"},
		{"unless invalid regex", Rule{Drop: true, Unless: &Condition{Match: "("}}, "unless"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Compile(RuleSet{{Drop: true}, tt.rule})
			if err == nil || c != nil {
				t.Fatalf("want error, got %v", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "rule 2") {
				t.Errorf("error %q must name rule index", err)
			}
			if tt.rule.Name != "" && !strings.Contains(err.Error(), tt.rule.Name) {
				t.Errorf("error %q must name the rule", err)
			}
		})
	}

	t.Run("valid field names", func(t *testing.T) {
		mustCompile(t, RuleSet{{Set: map[string]string{"fields.a-b_c.1": "x", "fields.identifier": "x"}}})
	})
	t.Run("all errors reported", func(t *testing.T) {
		_, err := Compile(RuleSet{{Name: "one"}, {Name: "two", Parse: "x"}})
		if err == nil || !strings.Contains(err.Error(), `"one"`) || !strings.Contains(err.Error(), `"two"`) {
			t.Errorf("got %v", err)
		}
	})
}

func TestYAMLDecode(t *testing.T) {
	const doc = `
- name: a
  when:
    container: addon_*_zigbee2mqtt
    image: [x/*, y/*]
    level: '>=warning'
    match: '^\[(?P<l>\w+)\]'
  unless: { source: stdout }
  parse: bracket
  set: { level: '${l}', fields.n: 5 }
  stop: true
- name: b
  drop: true
`
	var rules RuleSet
	if err := yaml.Unmarshal([]byte(doc), &rules); err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules", len(rules))
	}
	r := rules[0]
	if !reflect.DeepEqual(r.When.Container, StringList{"addon_*_zigbee2mqtt"}) ||
		!reflect.DeepEqual(r.When.Image, StringList{"x/*", "y/*"}) ||
		r.Unless.Source != "stdout" || r.Parse != "bracket" || !r.Stop ||
		r.Set["fields.n"] != "5" || r.Set["level"] != "${l}" {
		t.Errorf("got %+v", r)
	}
	mustCompile(t, rules)
	if !rules[1].Drop {
		t.Error("rule b")
	}
}

func TestTrace(t *testing.T) {
	c := mustCompile(t, RuleSet{
		{Name: "skip", When: &Condition{Container: StringList{"zzz"}}, Drop: true},
		{
			Name: "grp",
			When: &Condition{Match: `(?P<lvl>\w+): (?P<rest>.*)`},
			Set:  map[string]string{"level": "${lvl}", "fields.r": "${rest}", "message": "m"},
		},
		{Name: "bad level", When: &Condition{Match: `(?P<rest>\w+)`}, Set: map[string]string{"level": "${rest}"}},
		{Name: "end", Stop: true},
		{Name: "never", Drop: true},
	})
	m := msg("a", "", "stdout", "WARN: x")
	var tr Trace
	if c.Apply(m, &tr) {
		t.Fatal("dropped")
	}
	if len(tr.Rules) != 4 {
		t.Fatalf("rules traced: %d", len(tr.Rules))
	}
	if tr.Rules[0].Matched || tr.Rules[0].Name != "skip" {
		t.Errorf("rule 0: %+v", tr.Rules[0])
	}
	g := tr.Rules[1]
	if !g.Matched || g.Groups["lvl"] != "WARN" || g.Groups["rest"] != "x" ||
		!reflect.DeepEqual(g.Actions, []string{"set fields.r", "set level=warning", "set message"}) {
		t.Errorf("rule 1: %+v", g)
	}
	if !strings.Contains(tr.Rules[2].Actions[0], "ignored") {
		t.Errorf("rule 2: %+v", tr.Rules[2])
	}
	if tr.Level != "warning" || tr.Message != "m" || tr.Dropped || tr.Fields["r"] != "x" {
		t.Errorf("final: %+v", tr)
	}
	// Trace fields are a copy.
	m.Fields["r"] = "changed"
	if tr.Fields["r"] != "x" {
		t.Error("trace shares Fields with message")
	}
}

func TestCloneMessage(t *testing.T) {
	if CloneMessage(nil) != nil {
		t.Error("nil clone")
	}
	orig := msg("a", "i", "stdout", "x")
	orig.Level = "info"
	cl := CloneMessage(orig)
	if cl.Fields != nil {
		t.Error("nil Fields must stay nil")
	}
	orig.Fields = map[string]string{"a": "1"}
	cl = CloneMessage(orig)
	cl.Fields["a"] = "2"
	cl.Fields["b"] = "3"
	cl.Data = "y"
	if orig.Fields["a"] != "1" || len(orig.Fields) != 1 || orig.Data != "x" {
		t.Errorf("original changed: %+v", orig)
	}
	if cl.Container != orig.Container || cl.Level != "info" {
		t.Errorf("clone: %+v", cl)
	}
}

func TestConcurrentApply(t *testing.T) {
	c := mustCompile(t, RuleSet{
		{When: &Condition{Match: `(?P<l>\w+): (?P<m>.*)`}, Set: map[string]string{"level": "${l}", "message": "${m}", "fields.x": "${container}"}},
		{When: &Condition{Expr: `level == "warning"`}, Set: map[string]string{"fields.w": "1"}},
		{Parse: "generic"},
		{When: &Condition{Level: ">=error"}, Drop: true, Unless: &Condition{Container: StringList{"keep"}}},
	})
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				var tr Trace
				m := msg("/c", "img", "stdout", "WARN: hello")
				if c.Apply(m, &tr) || m.Level != "warning" || m.Data != "hello" || m.Fields["w"] != "1" {
					t.Errorf("unexpected %+v", m)
					return
				}
				m = msg("/c", "img", "stderr", "boom")
				if !c.Apply(m, nil) {
					t.Error("want drop")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestNoAllocWhenCheapConditionFails(t *testing.T) {
	c := mustCompile(t, RuleSet{
		{When: &Condition{Container: StringList{"other"}, Match: `(?P<a>x)`, Expr: `message == "x"`}, Drop: true},
		{When: &Condition{Source: "stderr", Match: `(?P<a>x)`}, Drop: true},
		{When: &Condition{Level: ">=error", Match: `(?P<a>x)`}, Drop: true},
	})
	m := msg("/web", "img", "stdout", "\x1b[31mx\x1b[0m")
	if n := testing.AllocsPerRun(100, func() { c.Apply(m, nil) }); n != 0 {
		t.Errorf("allocs = %v, want 0", n)
	}
}

func BenchmarkApply(b *testing.B) {
	rules := RuleSet{
		{When: &Condition{Container: StringList{"homeassistant"}}, Parse: "homeassistant"},
		{When: &Condition{Container: StringList{"addon_*_x"}, Match: `^\[(?P<l>\w+)\]`}, Set: map[string]string{"level": "${l}"}},
	}
	c, _ := Compile(rules)
	m := msg("/homeassistant", "", "stdout", "2026-10-04 12:00:00.123 INFO (MainThread) [homeassistant.core] started")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		cm := *m
		c.Apply(&cm, nil)
	}
}

func TestParseRuleSet(t *testing.T) {
	tests := []struct {
		name, doc, wantErr string
		wantRules          int
	}{
		{"valid", "- name: a\n  when: {container: x}\n  drop: true\n", "", 1},
		{"empty document", "", "", 0},
		{"unknown rule key", "- name: a\n  dorp: true\n", "dorp", 0},
		{"unknown condition key", "- when: {containr: x}\n  drop: true\n", "containr", 0},
		{"unknown unless key", "- unless: {sorce: stderr}\n  drop: true\n", "sorce", 0},
		{"null container", "- when: {container: }\n  drop: true\n", "container: value is empty", 0},
		{"empty container list", "- when: {container: []}\n  drop: true\n", "container: value is empty", 0},
		{"null image", "- when: {image: }\n  drop: true\n", "image: value is empty", 0},
		{"null when", "- name: x\n  when:\n  drop: true\n", "when: empty condition", 0},
		{"tilde when", "- name: x\n  when: ~\n  drop: true\n", "when: empty condition", 0},
		{"null unless", "- name: x\n  unless:\n  drop: true\n", "unless: empty condition", 0},
		{"empty match", "- when: {container: homeassistant, match: ''}\n  drop: true\n", "match: value is empty", 0},
		{"null match", "- when: {match: }\n  drop: true\n", "match: value is empty", 0},
		{"empty expr", "- when: {expr: ''}\n  drop: true\n", "expr: value is empty", 0},
		{"null source", "- unless: {source: ~}\n  drop: true\n", "source: value is empty", 0},
		{"empty level", "- when: {level: ''}\n  drop: true\n", "level: value is empty", 0},
		{"null set value", "- set: {message: ~}\n", "set message: value is null", 0},
		{"missing set value", "- set:\n    message:\n", "set message: value is null", 0},
		{"empty set string allowed", "- set: {message: ''}\n", "", 1},
		{"two documents", "- drop: true\n---\n- stop: true\n", "more than one YAML document", 0},
		{"not a list", "name: a\n", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules, err := ParseRuleSet([]byte(tt.doc))
			if tt.name == "not a list" {
				if err == nil {
					t.Fatal("want error for a mapping")
				}
				return
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || len(rules) != tt.wantRules {
				t.Fatalf("rules %d err %v", len(rules), err)
			}
		})
	}
}

func TestTraceEffectiveLevel(t *testing.T) {
	c := mustCompile(t, RuleSet{{Name: "n", Set: map[string]string{"fields.x": "1"}}})
	for _, tt := range []struct {
		m    *router.Message
		want string
	}{
		{&router.Message{Source: "stderr"}, "error"},
		{&router.Message{Source: "stdout"}, "info"},
		{&router.Message{Source: "stderr", Level: "debug"}, "debug"},
	} {
		var tr Trace
		c.Apply(tt.m, &tr)
		if tr.EffectiveLevel != tt.want || tr.Level != tt.m.Level {
			t.Errorf("got level %q effective %q, want %q", tr.Level, tr.EffectiveLevel, tt.want)
		}
	}
}

func TestRuleErrorPosition(t *testing.T) {
	rules, err := ParseRuleSet([]byte("- name: ok\n  drop: true\n\n- name: bad\n  parse: nope\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Compile(rules)
	if err == nil || !strings.Contains(err.Error(), `rule 2 "bad" (line 4): `) {
		t.Errorf("got %v", err)
	}
	_, err = Compile(RuleSet{{Parse: "nope"}})
	if err == nil || !strings.HasPrefix(err.Error(), "rule 1: ") {
		t.Errorf("got %v", err)
	}
}

func TestNoAllocPerRuleForSlashedImage(t *testing.T) {
	var rules RuleSet
	for i := 0; i < 10; i++ {
		rules = append(rules, Rule{When: &Condition{Image: StringList{"docker.io/*"}}, Drop: true})
	}
	c := mustCompile(t, rules)
	m := msg("/web", "ghcr.io/home-assistant/amd64-hassio-supervisor", "stdout", "x")
	// the swapped image name is built once per message, not once per rule
	if n := testing.AllocsPerRun(100, func() { c.Apply(m, nil) }); n > 1 {
		t.Errorf("allocs = %v, want <= 1", n)
	}
}
