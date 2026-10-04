package pipeline

import (
	"bufio"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/gliderlabs/logspout/router"
)

func TestDefaultVersions(t *testing.T) {
	v := DefaultVersions()
	if len(v) == 0 || v[0] != "v1" {
		t.Fatalf("versions %v", v)
	}
	if LatestDefaults() != v[len(v)-1] {
		t.Errorf("latest %q", LatestDefaults())
	}
}

func TestResolveDefaults(t *testing.T) {
	tests := []struct {
		in, want string
		err      bool
	}{
		{"", "", false},
		{"off", "", false},
		{"latest", LatestDefaults(), false},
		{DefaultVersions()[0], DefaultVersions()[0], false},
	}
	for _, in := range []string{"v0", "v999", "V1", "v01", "v1 ", " v1", "LATEST", "Off", "none", "v", "1", "v1.yaml", "v1/", "../v1", "defaults/v1", "v-1", "v1\n", "v\u0661", "stable", "default", "true", "false"} {
		tests = append(tests, struct {
			in, want string
			err      bool
		}{in, "", true})
	}
	for _, tt := range tests {
		got, err := ResolveDefaults(tt.in)
		if got != tt.want || (err != nil) != tt.err {
			t.Errorf("ResolveDefaults(%q) = %q, %v", tt.in, got, err)
		}
	}
	for _, v := range DefaultVersions() {
		if got, err := ResolveDefaults(v); err != nil || got != v {
			t.Errorf("round trip %q = %q, %v", v, got, err)
		}
	}
}

func TestDefaultRulesDisable(t *testing.T) {
	v1 := DefaultVersions()[0]
	all, err := defaultRules(v1, nil)
	if err != nil {
		t.Fatal(err)
	}
	some, err := defaultRules(v1, []string{all[0].Name, all[2].Name})
	if err != nil {
		t.Fatal(err)
	}
	if len(some) != len(all)-2 {
		t.Fatalf("got %d rules, want %d", len(some), len(all)-2)
	}
	for _, r := range some {
		if r.Name == all[0].Name || r.Name == all[2].Name {
			t.Errorf("rule %q not disabled", r.Name)
		}
	}
	if _, err := defaultRules(v1, []string{"no-such-rule"}); err == nil || !strings.Contains(err.Error(), "no-such-rule") {
		t.Errorf("unknown disabled name: %v", err)
	}
	for _, v := range []string{"v0", "V1", "v01", "v1.yaml", "../defaults/v1", "v1/", "", "latest", "off", "v999"} {
		if _, err := defaultRules(v, nil); err == nil {
			t.Errorf("defaultRules(%q) accepted", v)
		}
	}
	last := all[len(all)-1].Name
	if last != "addon-bashio" {
		t.Fatalf("catch-all is %q", last)
	}
	c := compileV1(t, "addon-bashio")
	runDefCases(t, c, []defCase{
		{"addon_core_ssh", "stdout", "[12:00:00] INFO: Starting", "", nil},
		{"hassio_cli", "stdout", "[12:00:00] INFO: Starting", "info", nil},
		{"addon_x_grafana", "stdout", "logger=a level=warn msg=x", "warning", nil},
	})

	everything := make([]string, len(all))
	for i, r := range all {
		everything[i] = r.Name
	}
	rs, err := defaultRules("v1", everything)
	if err != nil || len(rs) != 0 {
		t.Fatalf("disable all: %d rules, %v", len(rs), err)
	}
	if _, err := Compile(rs); err != nil {
		t.Errorf("compile empty: %v", err)
	}

	for _, bad := range [][]string{{"nope"}, {"addon-bashio", "nope"}, {"ADDON-BASHIO"}, {""}, {" addon-bashio"}} {
		if _, err := defaultRules(v1, bad); err == nil {
			t.Errorf("disabled %q: no error", bad)
		}
	}
	// Duplicate names in the list are harmless.
	if rs, err := defaultRules(v1, []string{"addon-bashio", "addon-bashio"}); err != nil || len(rs) != len(all)-1 {
		t.Errorf("duplicate disable: %d, %v", len(rs), err)
	}
	// Calling twice must not share state.
	again, _ := defaultRules(v1, nil)
	if len(again) != len(all) {
		t.Errorf("rules changed between calls")
	}
}

func TestParseDefaultsStrict(t *testing.T) {
	for name, doc := range map[string]string{
		"second document": "rules:\n  - {name: a, parse: bashio}\n---\nrules: []\n",
		"unknown key":     "rules: []\nextra: 1\n",
	} {
		if _, err := parseDefaults([]byte(doc)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if rs, err := parseDefaults([]byte("rules:\n  - {name: a, parse: bashio}\n")); err != nil || len(rs) != 1 {
		t.Errorf("valid doc: %v %v", rs, err)
	}
}

// Every embedded set must compile and may only classify.
func TestEmbeddedDefaultsValid(t *testing.T) {
	for _, v := range DefaultVersions() {
		t.Run(v, func(t *testing.T) {
			rules, err := defaultRules(v, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(rules) == 0 {
				t.Fatal("empty set")
			}
			seen := map[string]bool{}
			for _, r := range rules {
				if r.Name == "" {
					t.Errorf("rule without name at line %d", r.line)
				}
				if seen[r.Name] {
					t.Errorf("duplicate rule name %q", r.Name)
				}
				seen[r.Name] = true
				if r.Drop {
					t.Errorf("%s: defaults must not drop", r.Name)
				}
				if _, ok := r.Set["message"]; ok {
					t.Errorf("%s: defaults must not set message", r.Name)
				}
				if r.When == nil {
					t.Errorf("%s: default rule without when", r.Name)
				}
			}
			if _, err := Compile(rules); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type fixtureLine struct {
	file, container, source, line, level string
	fields                               map[string]string
}

// readDefaultFixtures reads defaults/testdata/<version>/*.txt. Columns, tab
// separated: container, source, line, expected level ("-" for unclassified),
// expected fields (k=v;k=v). \e is ESC, \t a tab.
func readDefaultFixtures(t *testing.T, version string) []fixtureLine {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join("defaults", "testdata", version, "*.txt"))
	if len(files) == 0 {
		t.Fatalf("no fixtures for %s", version)
	}
	var out []fixtureLine
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			cols := strings.Split(line, "\t")
			if len(cols) != 5 {
				t.Fatalf("%s: bad fixture line %q", file, line)
			}
			fl := fixtureLine{
				file: filepath.Base(file), container: cols[0], source: cols[1], level: cols[3],
				line: strings.NewReplacer(`\e`, "\x1b", `\t`, "\t").Replace(cols[2]),
			}
			if fl.level == "-" {
				fl.level = ""
			}
			if cols[4] != "" {
				fl.fields = map[string]string{}
				for _, kv := range strings.Split(cols[4], ";") {
					k, val, _ := strings.Cut(kv, "=")
					fl.fields[k] = val
				}
			}
			out = append(out, fl)
		}
		f.Close()
	}
	return out
}

func TestDefaultFixtures(t *testing.T) {
	for _, v := range DefaultVersions() {
		rules, err := defaultRules(v, nil)
		if err != nil {
			t.Fatal(err)
		}
		c, err := Compile(rules)
		if err != nil {
			t.Fatal(err)
		}
		matched := map[string]bool{}
		for _, fl := range readDefaultFixtures(t, v) {
			m := &router.Message{
				Data:      fl.line,
				Source:    fl.source,
				Container: &docker.Container{Name: "/" + fl.container},
			}
			var tr Trace
			if c.Apply(m, &tr) {
				t.Errorf("%s %q: dropped", fl.container, fl.line)
			}
			for _, rt := range tr.Rules {
				if rt.Matched && rt.Changed {
					matched[rt.Name] = true
				}
			}
			if m.Level != fl.level {
				t.Errorf("%s %q: level %q, want %q", fl.container, fl.line, m.Level, fl.level)
			}
			if m.Data != fl.line {
				t.Errorf("%s %q: message changed to %q", fl.container, fl.line, m.Data)
			}
			if !reflect.DeepEqual(m.Fields, fl.fields) {
				t.Errorf("%s %q: fields %v, want %v", fl.container, fl.line, m.Fields, fl.fields)
			}
		}
		// Quality guardrail: a rule without a fixture line is not accepted.
		for _, r := range rules {
			if !matched[r.Name] {
				t.Errorf("%s: rule %q is matched by no fixture line", v, r.Name)
			}
		}
	}
}

func TestCompileDefaults(t *testing.T) {
	v := LatestDefaults()
	c, unknown, err := CompileDefaults(v, []string{"addon-bashio", "no-such-rule", "also-missing"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(unknown, []string{"also-missing", "no-such-rule"}) {
		t.Errorf("unknown = %v", unknown)
	}
	all, _ := defaultRules(v, nil)
	if c.Len() != len(all)-1 {
		t.Errorf("compiled %d rules, want %d (unknown names must not drop the set)", c.Len(), len(all)-1)
	}
	if _, _, err := CompileDefaults("../v1", nil); err == nil {
		t.Error("bad version accepted")
	}
	if _, _, err := CompileDefaults("v999", nil); err == nil {
		t.Error("unknown version accepted")
	}
	c, unknown, err = CompileDefaults(v, nil)
	if err != nil || unknown != nil || c.Len() != len(all) {
		t.Errorf("plain: %d %v %v", c.Len(), unknown, err)
	}
}

// Default rules use stop. As a separate list they must not hide the user's rules.
func TestUserRulesRunAfterDefaults(t *testing.T) {
	defs, _, err := CompileDefaults("v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	line := "2026-10-04 12:00:00.123 INFO (MainThread) [homeassistant.setup] Setup of domain mqtt took 0.3 seconds"
	user := func(r Rule) *Compiled {
		c, err := Compile(RuleSet{r})
		if err != nil {
			t.Fatal(err)
		}
		return c.WithName("user")
	}
	cond := &Condition{Container: StringList{"homeassistant"}, Match: "Setup of domain"}

	m := defMsg("homeassistant", "stdout", line)
	var tr Trace
	dropped := defs.Apply(m, &tr)
	if dropped || m.Level != "info" {
		t.Fatalf("defaults: dropped=%v level=%q", dropped, m.Level)
	}
	if !user(Rule{When: cond, Drop: true}).Apply(m, &tr) {
		t.Error("user drop rule did not run after the defaults")
	}

	m = defMsg("homeassistant", "stdout", line)
	tr = Trace{}
	defs.Apply(m, &tr)
	user(Rule{When: cond, Set: map[string]string{"fields.x": "y"}}).Apply(m, &tr)
	if m.Fields["x"] != "y" || m.Level != "info" {
		t.Errorf("fields %v level %q", m.Fields, m.Level)
	}
	lists := map[string]bool{}
	for _, rt := range tr.Rules {
		lists[rt.List] = true
	}
	if !lists["defaults/v1"] || !lists["user"] || len(lists) != 2 {
		t.Errorf("trace lists %v", lists)
	}
}

func TestWithName(t *testing.T) {
	var nilC *Compiled
	if nilC.WithName("x") != nil {
		t.Error("nil WithName")
	}
	c, _ := Compile(RuleSet{{Set: map[string]string{"fields.a": "b"}}})
	named := c.WithName("n")
	var tr Trace
	named.Apply(&router.Message{Data: "x"}, &tr)
	c.Apply(&router.Message{Data: "x"}, &tr)
	if len(tr.Rules) != 2 || tr.Rules[0].List != "n" || tr.Rules[1].List != "" {
		t.Errorf("trace %+v", tr.Rules)
	}
}

// A rule whose gate matches but whose parser fails, or whose level is
// ignored, must not count as covered.
func TestTraceChangedParserFails(t *testing.T) {
	c, err := Compile(RuleSet{
		{Name: "gate-too-wide", When: &Condition{Container: StringList{"x"}}, Parse: "logfmt"},
		{Name: "ignored-level", Set: map[string]string{"level": "${message}"}},
		{Name: "ok", Set: map[string]string{"fields.a": "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var tr Trace
	c.Apply(defMsg("x", "stdout", "not logfmt"), &tr)
	if len(tr.Rules) != 3 {
		t.Fatalf("trace %+v", tr.Rules)
	}
	for i, want := range []bool{false, false, true} {
		if !tr.Rules[i].Matched || tr.Rules[i].Changed != want {
			t.Errorf("rule %d: %+v, want changed=%v", i, tr.Rules[i], want)
		}
	}
}
