package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/gliderlabs/logspout/router"
)

func testFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "files", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

var testFileEnv = FileEnv{Routes: []string{"syslog", "graylog", "gelf"}}

func TestParseFile(t *testing.T) {
	ambiguous := FileEnv{Routes: []string{"syslog", "gelf"}, Ambiguous: []string{"syslog"}}
	tests := []struct {
		file string
		env  FileEnv
		// wantErrs are "line:col message-substring"
		wantErrs []string
	}{
		{"syntax_error.yaml", testFileEnv, []string{"line 3: did not find expected"}},
		{"unknown_top_key.yaml", testFileEnv, []string{`2:1 unknown key "rulez"`}},
		{"unknown_rule_key.yaml", testFileEnv, []string{`4:5 unknown rule key "dorp"`}},
		{"bad_regex.yaml", testFileEnv, []string{`5:5 rule "bad regex": when: `}},
		{"bad_expr.yaml", testFileEnv, []string{`3:5 rule "bad expr": when: `}},
		{"bad_parser.yaml", testFileEnv, []string{`3:5 rule "bad parser": unknown parser "nonesuch"`}},
		{"unknown_target.yaml", testFileEnv, []string{`2:3 unknown target "nope" (routes: syslog, graylog, gelf)`}},
		{"ambiguous_target.yaml", ambiguous, []string{`2:3 ambiguous target name "syslog"`}},
		{"bad_defaults.yaml", testFileEnv, []string{`1:11 defaults: unknown default rule set "v99"`}},
		{"multi_doc.yaml", testFileEnv, []string{"2:1 more than one YAML document"}},
		{"not_mapping.yaml", testFileEnv, []string{"1:1 the file must be a mapping"}},
		{"many_errors.yaml", testFileEnv, []string{
			"1:11 defaults:",
			`4:5 rule "bad regex"`,
			`6:5 rule "no action": rule has no action`,
			`11:9 rule "bad parser": unknown parser "zzz"`,
			`12:5 unknown target key "other"`,
			`13:3 unknown target "missing"`,
		}},
		{"valid.yaml", testFileEnv, nil},
		{"empty.yaml", testFileEnv, nil},
		{"comments_only.yaml", testFileEnv, nil},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			res := ParseFile(testFile(t, tt.file), tt.env)
			var got []string
			for _, e := range res.Errors {
				got = append(got, e.String())
			}
			if len(got) != len(tt.wantErrs) {
				t.Fatalf("errors:\n%s\nwant %d", strings.Join(got, "\n"), len(tt.wantErrs))
			}
			for i, want := range tt.wantErrs {
				if !strings.HasPrefix(got[i], strings.SplitN(want, " ", 2)[0]) || !strings.Contains(got[i], strings.SplitN(want, " ", 2)[1]) {
					t.Errorf("error %d = %q, want %q", i, got[i], want)
				}
			}
			if (res.Err() == nil) != (len(tt.wantErrs) == 0) || (res.Config == nil) != (len(tt.wantErrs) > 0) {
				t.Errorf("Err()=%v Config=%v", res.Err(), res.Config)
			}
		})
	}
}

func TestParseFileValid(t *testing.T) {
	res := ParseFile(testFile(t, "valid.yaml"), testFileEnv)
	if res.Err() != nil {
		t.Fatal(res.Err())
	}
	c := res.Config
	if !c.DefaultsSet || c.Defaults != "v1" || len(c.Rules) != 2 || len(c.Targets["syslog"]) != 1 || len(c.Targets["graylog"]) != 1 {
		t.Errorf("config %+v", c)
	}
	// The unknown disabled name is a warning at its line.
	if len(res.Warnings) != 1 || res.Warnings[0].Line != 2 || !strings.Contains(res.Warnings[0].Message, `"no-such-rule"`) {
		t.Errorf("warnings %v", res.Warnings)
	}
	p, unknown, err := Build(c.Apply(Options{}))
	if err != nil || len(unknown) != 1 {
		t.Fatalf("build: %v %v", err, unknown)
	}
	if s := p.Summary(); !strings.Contains(s, "4 user rules (2 global)") || !strings.Contains(s, "graylog(1),syslog(1)") {
		t.Errorf("summary %q", s)
	}
}

func TestFileDefaultsOverrideOption(t *testing.T) {
	// defaults: off in the file wins over the option and the disable_defaults warns.
	env := testFileEnv
	env.DefaultRules = "latest"
	res := ParseFile(testFile(t, "defaults_off.yaml"), env)
	if res.Err() != nil {
		t.Fatal(res.Err())
	}
	opts := res.Config.Apply(Options{DefaultRules: "latest"})
	if opts.DefaultRules != "off" {
		t.Errorf("DefaultRules = %q", opts.DefaultRules)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0].Message, "default rules are off") {
		t.Errorf("warnings %v", res.Warnings)
	}

	// Without `defaults` in the file the option stays.
	res = ParseFile([]byte("rules: []\n"), env)
	if got := res.Config.Apply(Options{DefaultRules: "latest"}).DefaultRules; got != "latest" {
		t.Errorf("DefaultRules = %q", got)
	}
}

func TestDisableDefaultsUsesOptionSet(t *testing.T) {
	env := testFileEnv
	env.DefaultRules = "v1"
	res := ParseFile([]byte("disable_defaults: [ha-core-log, nope]\n"), env)
	if res.Err() != nil || len(res.Warnings) != 1 || res.Warnings[0].Column != 33 {
		t.Errorf("err=%v warnings=%v", res.Err(), res.Warnings)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	res := LoadFile(filepath.Join(dir, "missing.yaml"), testFileEnv)
	if res.Err() != nil || res.Config == nil || len(res.Config.Rules) != 0 {
		t.Errorf("missing file: %+v", res)
	}
	path := filepath.Join(dir, "logspout.yaml")
	if err := os.WriteFile(path, testFile(t, "unknown_top_key.yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	res = LoadFile(path, testFileEnv)
	if err := res.Err(); err == nil || !strings.Contains(err.Error(), "2:1") {
		t.Errorf("err = %v", err)
	}
	// A directory is a read error, not "missing".
	if res = LoadFile(dir, testFileEnv); res.Err() == nil {
		t.Error("expected read error")
	}
}

func TestBuildStripsLeadingSlashOfExcludes(t *testing.T) {
	p, _, err := Build(Options{ExcludeContainers: []string{"/homeassistant", "/", "addon_*"}})
	if err != nil {
		t.Fatal(err)
	}
	m := &router.Message{Container: &docker.Container{Name: "/homeassistant"}, Data: "x"}
	if !p.Global(m) {
		t.Error("/homeassistant must exclude homeassistant")
	}
	if s := p.Summary(); !strings.Contains(s, "excluded containers: homeassistant,addon_*") || !strings.Contains(s, "0 user rules") {
		t.Errorf("summary %q", s)
	}
}

func TestParseFileStructuralErrors(t *testing.T) {
	tests := []struct {
		name, file string
		want       []string
	}{
		{"duplicate rules key", "targets:\n  gelf:\n    rules: []\n    rules: []\n", []string{"4:5 duplicate key \"rules\""}},
		{"null rule item", "rules:\n  -\n  - name: a\n    drop: true\n", []string{"2:4 empty rule"}},
		{"tilde rule item", "rules:\n  - ~\n", []string{"2:5 empty rule"}},
		{"duplicate target after non-mapping", "targets:\n  gelf: 5\n  gelf:\n    rules: []\n", []string{"2:9 target gelf: must be a mapping", "3:3 duplicate target \"gelf\""}},
		{"empty defaults", "defaults: ''\n", []string{"1:11 defaults: empty value"}},
		{"null defaults", "defaults:\n", []string{"1:10 defaults: must be"}},
		{"type error gets a column", "rules:\n  - name: a\n    drop: [1]\n", []string{"3:11 cannot unmarshal"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := ParseFile([]byte(tt.file), testFileEnv)
			if len(res.Errors) != len(tt.want) {
				t.Fatalf("errors %v, want %v", res.Errors, tt.want)
			}
			for i, want := range tt.want {
				if got := res.Errors[i].String(); !strings.HasPrefix(got, strings.SplitN(want, " ", 2)[0]) || !strings.Contains(got, strings.SplitN(want, " ", 2)[1]) {
					t.Errorf("error %d = %q, want %q", i, got, want)
				}
			}
		})
	}
}

func TestParseFileNullTarget(t *testing.T) {
	res := ParseFile([]byte("targets:\n  syslog:\n"), testFileEnv)
	if res.Err() != nil || res.Config == nil {
		t.Fatalf("null target is an error: %v", res.Err())
	}
	if p, _, err := Build(res.Config.Apply(Options{})); err != nil || !p.Empty() {
		t.Errorf("empty=%v err=%v", p.Empty(), err)
	}
	// The name is still checked.
	if res := ParseFile([]byte("targets:\n  nope:\n"), testFileEnv); res.Err() == nil {
		t.Error("unknown null target accepted")
	}
}

func TestLoadFileSizeCap(t *testing.T) {
	dir := t.TempDir()
	write := func(n int) string {
		p := filepath.Join(dir, "f.yaml")
		data := []byte("# " + strings.Repeat("x", n-3) + "\n")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if res := LoadFile(write(MaxFileSize), testFileEnv); res.Err() != nil {
		t.Errorf("file of exactly the maximum size: %v", res.Err())
	}
	res := LoadFile(write(MaxFileSize+1), testFileEnv)
	if res.Err() == nil || !strings.Contains(res.Err().Error(), "file too large") || res.Config != nil {
		t.Errorf("result %+v", res)
	}
}
