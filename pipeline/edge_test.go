package pipeline

import (
	"strings"
	"sync"
	"testing"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/gliderlabs/logspout/router"
	"gopkg.in/yaml.v3"
)

func apply(t *testing.T, rules RuleSet, m *router.Message) (*router.Message, bool) {
	t.Helper()
	d := mustCompile(t, rules).Apply(m, nil)
	return m, d
}

func TestEdgeMessages(t *testing.T) {
	long := strings.Repeat("x", 2<<20) + " error"
	generic := RuleSet{{Parse: "generic"}}
	for name, data := range map[string]string{
		"empty":        "",
		"only ansi":    "\x1b[31m\x1b[0m",
		"lone escape":  "\x1b",
		"broken csi":   "\x1b[31",
		"very long":    long,
		"multibyte":    "日本語のログ ERROR 失敗 éè",
		"invalid utf8": "\xff\xfe ERROR \xc3",
		"journal":      "Oct 04 12:00:00 host sshd[123]: error: bad key",
		"newline":      "line1\nERROR line2",
	} {
		t.Run(name, func(t *testing.T) {
			all := RuleSet{
				{Name: "p", Parse: "homeassistant"}, {Parse: "bashio"}, {Parse: "logfmt"},
				{Parse: "json"}, {Parse: "bracket"}, {Parse: "generic"},
				{When: &Condition{Match: `(?P<a>.*)`, Expr: `len(message) >= 0 && fields["x"] == ""`, Level: ">=debug"},
					Set: map[string]string{"message": "${a}", "fields.k": "${message}"}},
			}
			apply(t, all, &router.Message{Data: data, Source: "stdout"})
		})
	}
	m, _ := apply(t, generic, &router.Message{Data: ""})
	if m.Level != "" {
		t.Errorf("empty message got level %q", m.Level)
	}
	m, _ = apply(t, generic, &router.Message{Data: "\x1b[31m\x1b[0m"})
	if m.Level != "" {
		t.Errorf("ansi-only message got level %q", m.Level)
	}
	m, _ = apply(t, generic, &router.Message{Data: "日本語 ERROR 失敗"})
	if m.Level != "error" {
		t.Errorf("multibyte: %q", m.Level)
	}
	m, _ = apply(t, generic, &router.Message{Data: long})
	if m.Level != "" {
		t.Errorf("long line: level %q, keyword is past column 40", m.Level)
	}
	m, _ = apply(t, generic, &router.Message{Data: "Oct 04 12:00:00 host sshd[123]: error: bad key"})
	if m.Level != "error" {
		t.Errorf("journal: %q", m.Level)
	}
}

func TestEdgeNilContainer(t *testing.T) {
	rules := RuleSet{
		{Name: "a", When: &Condition{Container: StringList{"*"}, Image: StringList{"*"}, Expr: `container == "" && image == ""`},
			Set: map[string]string{"fields.c": "[${container}][${image}]"}},
	}
	for _, m := range []*router.Message{
		{Data: "x"},
		{Data: "x", Container: &docker.Container{}},
		{Data: "x", Container: &docker.Container{Name: "/n"}},
	} {
		apply(t, rules, m)
	}
	m := &router.Message{Data: "x", Container: &docker.Container{Name: "/n"}}
	apply(t, RuleSet{{Set: map[string]string{"fields.c": "${container}|${image}"}}}, m)
	if m.Fields["c"] != "n|" {
		t.Errorf("got %q", m.Fields["c"])
	}
	// A nil container has name "", which a `*` glob matches.
	m = &router.Message{Data: "x"}
	if _, d := apply(t, dropRule(&Condition{Container: StringList{"*"}}, nil), m); !d {
		t.Error("glob * should match a nil container (empty name)")
	}
}

func TestEdgeNilRuleSetAndMessage(t *testing.T) {
	var c *Compiled
	if c.Apply(&router.Message{}, &Trace{}) {
		t.Error("nil Compiled dropped")
	}
	if c.Len() != 0 {
		t.Error("nil Len")
	}
	c2, err := Compile(nil)
	if err != nil || c2.Len() != 0 {
		t.Fatalf("empty compile: %v", err)
	}
	m := &router.Message{Data: "x"}
	if c2.Apply(m, nil) || m.Data != "x" {
		t.Error("empty rules changed message")
	}
	if CloneMessage(nil) != nil {
		t.Error("CloneMessage(nil)")
	}
}

func TestEdgeInvalidInput(t *testing.T) {
	tests := []struct {
		name, want string
		rule       Rule
	}{
		{"regex unclosed", "match", Rule{Drop: true, When: &Condition{Match: "(abc"}}},
		{"regex bad repeat", "match", Rule{Drop: true, When: &Condition{Match: "*a"}}},
		{"regex lookahead unsupported (RE2)", "match", Rule{Drop: true, When: &Condition{Match: "(?=a)b"}}},
		{"regex backreference unsupported", "match", Rule{Drop: true, When: &Condition{Match: `(a)\1`}}},
		{"regex in unless", "unless", Rule{Drop: true, Unless: &Condition{Match: "["}}},
		{"expr returns string", "expr", Rule{Drop: true, When: &Condition{Expr: `message`}}},
		{"expr returns int", "expr", Rule{Drop: true, When: &Condition{Expr: `1 + 1`}}},
		{"expr returns nil", "expr", Rule{Drop: true, When: &Condition{Expr: `nil`}}},
		{"expr type error", "expr", Rule{Drop: true, When: &Condition{Expr: `message + 1 == 2`}}},
		{"expr unknown func", "expr", Rule{Drop: true, When: &Condition{Expr: `nofunc(message)`}}},
		{"expr in unless", "unless", Rule{Drop: true, Unless: &Condition{Expr: `level ==`}}},
		{"unknown parser", "unknown parser", Rule{Parse: "nope"}},
		{"parser wrong case", "unknown parser", Rule{Parse: "JSON"}},
		{"bad source", "source", Rule{Drop: true, When: &Condition{Source: "stdin"}}},
		{"bad level op", "level", Rule{Drop: true, When: &Condition{Level: "=>info"}}},
		{"bad level", "level", Rule{Drop: true, When: &Condition{Level: "<loud"}}},
		{"op only", "level", Rule{Drop: true, When: &Condition{Level: ">="}}},
		{"set bad key", "unknown key", Rule{Set: map[string]string{"foo": "x"}}},
		{"set fields. empty", "invalid field name", Rule{Set: map[string]string{"fields.": "x"}}},
		{"set field space", "invalid field name", Rule{Set: map[string]string{"fields.a b": "x"}}},
		{"set field unicode", "invalid field name", Rule{Set: map[string]string{"fields.é": "x"}}},
		{"set field id", "reserved", Rule{Set: map[string]string{"fields.id": "x"}}},
		{"set literal bad level", "invalid level", Rule{Set: map[string]string{"level": "loud"}}},
		{"unterminated var", "unterminated", Rule{Set: map[string]string{"message": "${x"}}},
		{"bad var name", "invalid variable", Rule{Set: map[string]string{"message": "${1x}"}}},
		{"empty var name", "invalid variable", Rule{Set: map[string]string{"message": "${}"}}},
		{"empty unless", "unless", Rule{Drop: true, Unless: &Condition{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Compile(RuleSet{tt.rule})
			if err == nil {
				t.Fatalf("want error containing %q, got nil (compiled=%v)", tt.want, c != nil)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
			if c != nil {
				t.Error("Compile must return nil on error")
			}
			if !strings.Contains(err.Error(), "rule 1") {
				t.Errorf("error lacks rule index: %q", err)
			}
		})
	}
}

func TestEdgeAllErrorsReported(t *testing.T) {
	_, err := Compile(RuleSet{
		{Name: "one", Drop: true, When: &Condition{Match: "("}},
		{Name: "ok", Drop: true},
		{Name: "three", Parse: "nope"},
	})
	if err == nil || !strings.Contains(err.Error(), `rule 1 "one"`) || !strings.Contains(err.Error(), `rule 3 "three"`) {
		t.Errorf("got %v", err)
	}
	if strings.Contains(err.Error(), `rule 2`) {
		t.Errorf("valid rule reported: %v", err)
	}
}

func TestEdgeInvalidYAML(t *testing.T) {
	for name, doc := range map[string]string{
		"tab indent":       "- name: x\n\tdrop: true\n",
		"scalar":           "just a string",
		"map not list":     "name: x\n",
		"when is list":     "- when: [a, b]\n  drop: true\n",
		"container is map": "- when: {container: {a: b}}\n  drop: true\n",
		"container nested": "- when: {container: [[a]]}\n  drop: true\n",
		"drop not bool":    "- drop: maybe\n",
		"set is list":      "- set: [a]\n",
		"unclosed":         "- name: \"x\n",
	} {
		t.Run(name, func(t *testing.T) {
			var rs RuleSet
			if err := yaml.Unmarshal([]byte(doc), &rs); err == nil {
				t.Errorf("want error, got %+v", rs)
			}
		})
	}
	var rs RuleSet
	if err := yaml.Unmarshal([]byte(""), &rs); err != nil || len(rs) != 0 {
		t.Errorf("empty doc: %v %v", rs, err)
	}
	// non-string scalars in `set` values decode as strings
	if err := yaml.Unmarshal([]byte("- set: {fields.n: 5, fields.b: true}\n"), &rs); err != nil {
		t.Errorf("numeric set value: %v", err)
	}
}

func TestEdgeExprRuntime(t *testing.T) {
	tests := []struct {
		name, expr string
		compiles   bool
		want       bool // dropped, for a message with Data "abcd"
		wantErr    bool // expr fails at run time: rule is skipped
	}{
		{"missing field key is empty string", `fields["missing"] == ""`, true, true, false},
		{"missing field not equal", `fields["missing"] == "x"`, true, false, false},
		{"missing field with in", `"missing" in fields`, true, false, false},
		{"field dot access", `fields.missing == ""`, true, true, false},
		{"int cast error skips rule", `int(message) > 0`, true, false, true},
		{"modulo by zero skips rule", `(len(message) % 0) > 0`, true, false, true},
		{"slice out of range is clamped", `message[5:10] == "x"`, true, false, false},
		{"constant index out of range is a compile error", `message[100] == "x"`, false, false, false},
		{"matches", `message matches "^ab"`, true, true, false},
		{"startsWith", `message startsWith "ab"`, true, true, false},
		{"len", `len(message) == 4`, true, true, false},
		{"container empty", `container == ""`, true, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Compile(dropRule(&Condition{Expr: tt.expr}, nil))
			if (err == nil) != tt.compiles {
				t.Fatalf("compile %q: err = %v, want compiles=%v", tt.expr, err, tt.compiles)
			}
			if err != nil {
				return
			}
			var tr Trace
			if got := c.Apply(&router.Message{Data: "abcd"}, &tr); got != tt.want {
				t.Errorf("%s: dropped=%v want %v", tt.expr, got, tt.want)
			}
			if gotErr := tr.Rules[0].Error != ""; gotErr != tt.wantErr {
				t.Errorf("%s: trace error %q, want error=%v", tt.expr, tr.Rules[0].Error, tt.wantErr)
			}
		})
	}
	// A runtime error skips the rule in `when` and in `unless`.
	c := mustCompile(t, dropRule(nil, &Condition{Expr: `int(message) > 0`}))
	var tr Trace
	if c.Apply(&router.Message{Data: "abc"}, &tr) || tr.Rules[0].Error == "" || tr.Rules[0].Matched {
		t.Errorf("runtime error in unless must skip the rule: %+v", tr)
	}
}

func TestEdgeLevelOperators(t *testing.T) {
	levels := []string{"debug", "info", "notice", "warning", "error", "critical"}
	for _, op := range []string{"=", "<", "<=", ">", ">="} {
		for ti, target := range levels {
			c := mustCompile(t, dropRule(&Condition{Level: op + target}, nil))
			c2 := mustCompile(t, dropRule(&Condition{Level: op + " " + strings.ToUpper(target)}, nil))
			for mi, lvl := range levels {
				var want bool
				switch op {
				case "=":
					want = mi == ti
				case "<":
					want = mi < ti
				case "<=":
					want = mi <= ti
				case ">":
					want = mi > ti
				case ">=":
					want = mi >= ti
				}
				if got := c.Apply(&router.Message{Level: lvl}, nil); got != want {
					t.Errorf("%s%s on %s: got %v want %v", op, target, lvl, got, want)
				}
				if got := c2.Apply(&router.Message{Level: lvl}, nil); got != want {
					t.Errorf("%q on %s: got %v want %v", op+" "+strings.ToUpper(target), lvl, got, want)
				}
			}
		}
	}
	// aliases in the condition
	for cond, lvl := range map[string]string{"warn": "warning", ">=fatal": "critical", "<=trace": "debug", "=err": "error", "emerg": "critical"} {
		if !mustCompile(t, dropRule(&Condition{Level: cond}, nil)).Apply(&router.Message{Level: lvl}, nil) {
			t.Errorf("%q should match %q", cond, lvl)
		}
	}
	// derived level
	for _, tt := range []struct{ src, cond string }{{"stderr", "=error"}, {"stdout", "=info"}, {"", "=info"}, {"stderr", ">=warning"}, {"stdout", "<notice"}} {
		if !mustCompile(t, dropRule(&Condition{Level: tt.cond}, nil)).Apply(&router.Message{Source: tt.src}, nil) {
			t.Errorf("source %q level %s", tt.src, tt.cond)
		}
	}
	// a non-normalized upstream level never matches, not even <, <= or >=
	for _, cond := range []string{"=info", "<critical", "<=critical", ">=debug", ">debug"} {
		if mustCompile(t, dropRule(&Condition{Level: cond}, nil)).Apply(&router.Message{Level: "WARN"}, nil) {
			t.Errorf("unnormalized level WARN must not match %q", cond)
		}
	}
	// explicit Level wins over Source
	if mustCompile(t, dropRule(&Condition{Level: "=error"}, nil)).Apply(&router.Message{Source: "stderr", Level: "info"}, nil) {
		t.Error("explicit level must beat source")
	}
}

func TestEdgeUnlessLists(t *testing.T) {
	c := mustCompile(t, dropRule(nil, &Condition{Container: StringList{"a", "b*"}}))
	for name, want := range map[string]bool{"a": false, "b": false, "bee": false, "c": true, "ab": true, "": true} {
		if got := c.Apply(msg(name, "", "stdout", "x"), nil); got != want {
			t.Errorf("container %q: dropped=%v want %v", name, got, want)
		}
	}
	// unless is AND inside: only exempt when all keys match
	c = mustCompile(t, dropRule(nil, &Condition{Container: StringList{"a", "b"}, Source: "stderr"}))
	if !c.Apply(msg("a", "", "stdout", "x"), nil) {
		t.Error("unless with two keys must need both")
	}
	if c.Apply(msg("a", "", "stderr", "x"), nil) {
		t.Error("both keys match: exempt")
	}
	// when and unless together
	c = mustCompile(t, dropRule(&Condition{Level: ">=error"}, &Condition{Match: "expected"}))
	if c.Apply(msg("a", "", "stderr", "this is expected"), nil) || !c.Apply(msg("a", "", "stderr", "boom"), nil) || c.Apply(msg("a", "", "stdout", "boom"), nil) {
		t.Error("when+unless combination wrong")
	}
	// unless sees the message before this rule's own set actions
	rs := RuleSet{{Unless: &Condition{Match: "^X"}, Set: map[string]string{"message": "X${message}"}}}
	m, _ := apply(t, rs, &router.Message{Data: "a"})
	if m.Data != "Xa" {
		t.Errorf("got %q", m.Data)
	}
}

func TestEdgeDropVsStop(t *testing.T) {
	var tr Trace
	rs := RuleSet{
		{Name: "tag", Set: map[string]string{"fields.a": "1"}},
		{Name: "stopper", When: &Condition{Match: "stop"}, Stop: true, Set: map[string]string{"fields.b": "1"}},
		{Name: "later", Set: map[string]string{"fields.c": "1"}},
		{Name: "dropper", When: &Condition{Match: "drop"}, Drop: true, Set: map[string]string{"fields.d": "1"}},
		{Name: "never", Set: map[string]string{"fields.e": "1"}},
	}
	c := mustCompile(t, rs)
	m := &router.Message{Data: "stop"}
	if c.Apply(m, &tr) || m.Fields["b"] != "1" || m.Fields["c"] != "" || len(tr.Rules) != 2 {
		t.Errorf("stop: fields=%v rules=%d", m.Fields, len(tr.Rules))
	}
	tr = Trace{}
	m = &router.Message{Data: "drop"}
	if !c.Apply(m, &tr) || m.Fields["d"] != "1" || m.Fields["e"] != "" || !tr.Dropped {
		t.Errorf("drop: fields=%v dropped=%v", m.Fields, tr.Dropped)
	}
	// set before drop is applied, and visible in trace
	if tr.Fields["d"] != "1" {
		t.Errorf("trace fields %v", tr.Fields)
	}
	// stop + drop in same rule: drop wins
	if !mustCompile(t, RuleSet{{Stop: true, Drop: true}}).Apply(&router.Message{}, nil) {
		t.Error("stop+drop should drop")
	}
	// a drop rule that does not match does not stop
	m = &router.Message{Data: "x"}
	if c.Apply(m, nil) || m.Fields["e"] != "1" {
		t.Errorf("non-matching drop/stop must continue: %v", m.Fields)
	}
}

func TestEdgeParserActionSemantics(t *testing.T) {
	// parse then set in same rule: set wins, parser failure keeps level
	m, _ := apply(t, RuleSet{{Parse: "bashio", Set: map[string]string{"level": "${level}"}}}, &router.Message{Data: "[12:00:00] ERROR: x"})
	if m.Level != "error" {
		t.Errorf("got %q", m.Level)
	}
	m, _ = apply(t, RuleSet{{Parse: "bashio"}}, &router.Message{Data: "plain", Level: "warning"})
	if m.Level != "warning" {
		t.Errorf("failed parse changed level to %q", m.Level)
	}
	// parser does not change the message text and does not strip ANSI from Data
	in := "\x1b[31m[12:00:00] ERROR: x\x1b[0m"
	m, _ = apply(t, RuleSet{{Parse: "bashio"}}, &router.Message{Data: in})
	if m.Data != in || m.Level != "error" {
		t.Errorf("data=%q level=%q", m.Data, m.Level)
	}
	// parser fields merge with existing fields, parser overwrites same key
	m, _ = apply(t, RuleSet{{Parse: "homeassistant"}}, &router.Message{
		Data:   "2026-10-04 12:00:00.123 INFO (T) [l] x",
		Fields: map[string]string{"logger": "old", "keep": "1"},
	})
	if m.Fields["logger"] != "l" || m.Fields["keep"] != "1" || m.Fields["thread"] != "T" {
		t.Errorf("fields %v", m.Fields)
	}
	// an explicit level after a parse in a later rule wins
	m, _ = apply(t, RuleSet{{Parse: "bashio"}, {Set: map[string]string{"level": "debug"}}}, &router.Message{Data: "[12:00:00] ERROR: x"})
	if m.Level != "debug" {
		t.Errorf("got %q", m.Level)
	}
	// set level with unknown runtime value leaves level unchanged
	m, _ = apply(t, RuleSet{{Set: map[string]string{"level": "${x}"}, When: &Condition{Match: `(?P<x>\w+)`}}}, &router.Message{Data: "loud", Level: "info"})
	if m.Level != "info" {
		t.Errorf("got %q", m.Level)
	}
	m, _ = apply(t, RuleSet{{When: &Condition{Match: `(?P<missing>zzz)?`}, Set: map[string]string{"level": "${missing}"}}}, &router.Message{Data: "x", Level: "info"})
	if m.Level != "info" {
		t.Errorf("empty variable changed level to %q", m.Level)
	}
}

func TestEdgeVariables(t *testing.T) {
	m := &router.Message{Data: "\x1b[31mhello\x1b[0m", Source: "stderr", Container: &docker.Container{Name: "/c", Config: &docker.Config{Image: "img:1"}}}
	apply(t, RuleSet{{
		When: &Condition{Match: `(?P<word>h\w+)(?P<nope>zzz)?`},
		Set: map[string]string{
			"fields.a": "${word}/${container}/${image}/${source}/${level}/${message}",
			"fields.b": "$$5 $x ${nope} $ ${word}$",
			"fields.c": "",
		},
	}}, m)
	if got := m.Fields["a"]; got != "hello/c/img:1/stderr/error/hello" {
		t.Errorf("a = %q", got)
	}
	if got := m.Fields["b"]; got != "$5 $x  $ hello$" {
		// "$x" stays literal; "${nope}" is empty.
		t.Errorf("b = %q", got)
	}
	if v, ok := m.Fields["c"]; !ok || v != "" {
		t.Errorf("empty value should still be set: %v", m.Fields)
	}
	// set values are resolved together
	m = &router.Message{Data: "m"}
	apply(t, RuleSet{{Set: map[string]string{"message": "new", "fields.old": "${message}"}}}, m)
	if m.Data != "new" || m.Fields["old"] != "m" {
		t.Errorf("%q %v", m.Data, m.Fields)
	}
	// named group wins over built-in; empty optional group is empty
	m = &router.Message{Data: "ab", Level: "info"}
	apply(t, RuleSet{{When: &Condition{Match: `(?P<level>a)(?P<opt>x)?`}, Set: map[string]string{"fields.l": "${level}", "fields.o": "[${opt}]"}}}, m)
	if m.Fields["l"] != "a" || m.Fields["o"] != "[]" {
		t.Errorf("%v", m.Fields)
	}
	// groups of unless are not variables
	if _, err := Compile(RuleSet{{Unless: &Condition{Match: `(?P<g>zzz)`}, Set: map[string]string{"fields.g": "${g}"}}}); err == nil {
		t.Error("variable from unless group must be rejected")
	}
	// unnamed groups are not variables
	m = &router.Message{Data: "ab"}
	if _, err := Compile(RuleSet{{Set: map[string]string{"fields.n": "${1}"}}}); err == nil {
		t.Error("numeric variable names must be rejected")
	}
	apply(t, RuleSet{{When: &Condition{Match: `(a)(?P<n>b)`}, Set: map[string]string{"fields.n": "${n}"}}}, m)
	if m.Fields["n"] != "b" {
		t.Errorf("%v", m.Fields)
	}
}

func TestEdgeDoesNotMutateSharedFieldsAfterClone(t *testing.T) {
	orig := &router.Message{Data: "x", Fields: map[string]string{"k": "v"}}
	cl := CloneMessage(orig)
	apply(t, RuleSet{{Set: map[string]string{"fields.k": "changed", "fields.n": "1"}}}, cl)
	if orig.Fields["k"] != "v" || len(orig.Fields) != 1 {
		t.Errorf("original mutated: %v", orig.Fields)
	}
	// Apply on message without Fields must not touch the shared emptyFields map
	m := &router.Message{Data: "x"}
	apply(t, RuleSet{{Set: map[string]string{"fields.a": "1"}, When: &Condition{Expr: `fields["a"] == ""`}}}, m)
	if len(emptyFields) != 0 {
		t.Errorf("shared emptyFields polluted: %v", emptyFields)
	}
	if m.Fields["a"] != "1" {
		t.Errorf("%v", m.Fields)
	}
}

func TestEdgeANSI(t *testing.T) {
	tests := map[string]string{
		"\x1b[0m":               "",
		"\x1b[1;31mred\x1b[0m":  "red",
		"\x1b[38;5;196mx\x1b[m": "x",
		"\x1b[?25lx\x1b[?25h":   "x",
		"a\x1b[2Kb":             "ab",
		"日本\x1b[31m語\x1b[0m":    "日本語",
		"\x1b[31":               "\x1b[31",
		"\x1b]0;title\x07x":     "x",
		"a\x1b]8;;http://u\x1b\\link\x1b]8;;\x1b\\": "alink",
		"\x1b(Bx\x1b(0":       "x",
		"\x1b]0;unterminated": "\x1b]0;unterminated",
		"plain":               "plain",
		"":                    "",
	}
	for in, want := range tests {
		if got := stripANSI(in); got != want {
			t.Errorf("stripANSI(%q) = %q, want %q", in, got, want)
		}
	}
	// match is anchored on the stripped text
	c := mustCompile(t, dropRule(&Condition{Match: `^ERROR$`}, nil))
	if !c.Apply(&router.Message{Data: "\x1b[31mERROR\x1b[0m"}, nil) {
		t.Error("match should see stripped text")
	}
	// raw Data is unchanged when no set message
	m, _ := apply(t, RuleSet{{When: &Condition{Match: "x"}, Set: map[string]string{"fields.a": "1"}}}, &router.Message{Data: "\x1b[31mx\x1b[0m"})
	if m.Data != "\x1b[31mx\x1b[0m" {
		t.Errorf("Data modified: %q", m.Data)
	}
	// set message re-evaluates later rules on the new text
	m = &router.Message{Data: "\x1b[31mfoo\x1b[0m"}
	c = mustCompile(t, RuleSet{
		{Set: map[string]string{"message": "bar ${message}"}},
		{When: &Condition{Match: `^bar foo$`}, Set: map[string]string{"fields.ok": "1"}},
	})
	c.Apply(m, nil)
	if m.Data != "bar foo" || m.Fields["ok"] != "1" {
		t.Errorf("%q %v", m.Data, m.Fields)
	}
}

func TestConcurrentCompileAndApply(t *testing.T) {
	// Compiled values are immutable; applying one while another is created must be race free.
	var cur sync.Map
	cur.Store("c", mustCompile(t, RuleSet{{When: &Condition{Match: `(?P<x>a)`}, Set: map[string]string{"fields.x": "${x}"}}}))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				c, _ := cur.Load("c")
				m := &router.Message{Data: "a", Fields: map[string]string{"k": "v"}}
				var tr Trace
				c.(*Compiled).Apply(CloneMessage(m), &tr)
				if j%50 == 0 {
					nc, err := Compile(RuleSet{{Parse: "generic"}, {When: &Condition{Expr: `level == "info"`}, Set: map[string]string{"fields.y": "1"}}})
					if err != nil {
						t.Error(err)
						return
					}
					cur.Store("c", nc)
				}
			}
		}()
	}
	wg.Wait()
}

func TestEdgeTrace(t *testing.T) {
	var tr Trace
	c := mustCompile(t, RuleSet{
		{Name: "no", When: &Condition{Match: "zzz"}, Drop: true},
		{Name: "yes", When: &Condition{Match: `(?P<g>a)`}, Set: map[string]string{"level": "${g}"}},
	})
	c.Apply(&router.Message{Data: "a"}, &tr)
	if len(tr.Rules) != 2 || tr.Rules[0].Matched || !tr.Rules[1].Matched || tr.Rules[1].Groups["g"] != "a" {
		t.Errorf("%+v", tr)
	}
}
