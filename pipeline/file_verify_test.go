package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseFileInvalidKinds(t *testing.T) {
	env := FileEnv{Routes: []string{"syslog", "loki"}}
	tests := []struct {
		name, in string
		wantLine int // 0 means any positive line
		wantMsg  string
	}{
		{"tab indent", "rules:\n\t- name: x\n\t  drop: true\n", 2, ""},
		{"bad indentation", "rules:\n  - name: x\n   drop: true\n", 0, ""},
		{"unclosed double quote", "rules:\n  - name: \"x\n    drop: true\n", 0, ""},
		{"unclosed single quote", "rules:\n  - name: 'x\n    drop: true\n", 0, ""},
		{"unclosed flow map", "rules:\n  - name: x\n    when: { container: a\n    drop: true\n", 0, ""},
		{"unclosed flow seq", "disable_defaults: [a, b\nrules: []\n", 0, ""},
		{"duplicate top key", "rules: []\nrules: []\n", 2, "duplicate key"},
		{"duplicate rule key", "rules:\n  - name: a\n    name: b\n    drop: true\n", 3, ""},
		{"rules is map", "rules: {}\n", 1, "must be a list"},
		{"rules is string", "rules: abc\n", 1, "must be a list"},
		{"targets is list", "targets: []\n", 1, "must be a mapping"},
		{"targets is string", "targets: syslog\n", 1, "must be a mapping"},
		{"target is list", "targets:\n  syslog: []\n", 2, "must be a mapping"},
		{"target is null", "targets:\n  syslog:\n", 2, "must be a mapping"},
		{"when is string", "rules:\n  - name: a\n    when: container\n    drop: true\n", 3, ""},
		{"when is list", "rules:\n  - name: a\n    when: [x]\n    drop: true\n", 3, ""},
		{"rule is string", "rules:\n  - just text\n", 2, ""},
		{"drop is string", "rules:\n  - name: a\n    drop: maybe\n", 3, ""},
		{"set is list", "rules:\n  - name: a\n    set: [level]\n", 3, ""},
		{"defaults is list", "defaults: [v1]\n", 1, "defaults:"},
		{"defaults is null", "defaults:\n", 1, "defaults:"},
		{"defaults unknown", "defaults: v0\n", 1, "defaults:"},
		{"disable_defaults is string", "disable_defaults: a\n", 1, "list"},
		{"disable_defaults entry is map", "disable_defaults: [{a: b}]\n", 1, "rule names"},
		{"root is list", "- a\n", 1, "must be a mapping"},
		{"root is scalar", "just text\n", 1, "must be a mapping"},
		{"invalid regex", "rules:\n  - name: a\n    when: { match: '(' }\n    drop: true\n", 3, "match"},
		{"invalid expr", "rules:\n  - name: a\n    when: { expr: 'level ==' }\n    drop: true\n", 3, "expr"},
		{"unknown target", "targets:\n  nope:\n    rules: []\n", 2, `unknown target "nope"`},
		{"unknown target lists routes", "targets:\n  nope:\n    rules: []\n", 2, "routes: syslog, loki"},
		{"duplicate target", "targets:\n  syslog:\n    rules: []\n  syslog:\n    rules: []\n", 0, ""},
		{"alias loop", "a: &a [*a]\n", 1, ""},
		{"binary garbage", "\x00\x01\x02: [", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := ParseFile([]byte(tt.in), env)
			if res.Err() == nil || res.Config != nil {
				t.Fatalf("want errors, got Config=%+v", res.Config)
			}
			for _, e := range res.Errors {
				if e.Line < 1 {
					t.Errorf("error without line: %q", e)
				}
			}
			e := res.Errors[0]
			if tt.wantLine > 0 && e.Line != tt.wantLine {
				t.Errorf("first error at line %d, want %d: %v", e.Line, tt.wantLine, res.Errors)
			}
			if tt.wantMsg != "" && !strings.Contains(res.Err().Error(), tt.wantMsg) {
				t.Errorf("errors %v lack %q", res.Errors, tt.wantMsg)
			}
		})
	}
}

func TestParseFileTabsAndBOMAndCRLF(t *testing.T) {
	env := FileEnv{Routes: []string{"syslog"}}
	valid := "defaults: v1\nrules:\n  - name: a\n    when: { container: x }\n    drop: true\ntargets:\n  syslog:\n    rules:\n      - name: b\n        drop: true\n"
	for name, in := range map[string]string{
		"crlf":      strings.ReplaceAll(valid, "\n", "\r\n"),
		"bom":       "\xef\xbb\xbf" + valid,
		"bom+crlf":  "\xef\xbb\xbf" + strings.ReplaceAll(valid, "\n", "\r\n"),
		"no eol":    strings.TrimSuffix(valid, "\n"),
		"doc start": "---\n" + valid,
		"cr only":   strings.ReplaceAll(valid, "\n", "\r"),
	} {
		t.Run(name, func(t *testing.T) {
			res := ParseFile([]byte(in), env)
			if res.Err() != nil {
				t.Fatal(res.Err())
			}
			if len(res.Config.Rules) != 1 || len(res.Config.Targets["syslog"]) != 1 {
				t.Errorf("config %+v", res.Config)
			}
		})
	}
	// Error positions stay correct with CRLF.
	res := ParseFile([]byte("rules: []\r\nbogus: 1\r\n"), env)
	if len(res.Errors) != 1 || res.Errors[0].Line != 2 || res.Errors[0].Column != 1 {
		t.Errorf("errors %v", res.Errors)
	}
}

func TestParseFileEmptyish(t *testing.T) {
	for name, in := range map[string]string{
		"empty": "", "newlines": "\n\n\n", "comments": "# a\n# b\n", "doc marker": "---\n", "null": "~\n",
		"bom only": "\xef\xbb\xbf", "empty maps": "rules:\ntargets:\ndisable_defaults:\n",
		"explicit empty": "rules: []\ntargets: {}\ndisable_defaults: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			res := ParseFile([]byte(in), FileEnv{})
			if res.Err() != nil || res.Config == nil || len(res.Warnings) != 0 {
				t.Fatalf("%+v", res)
			}
			if opts := res.Config.Apply(Options{DefaultRules: "v1"}); opts.DefaultRules != "v1" || len(opts.Rules) != 0 {
				t.Errorf("options %+v", opts)
			}
			p, _, err := Build(res.Config.Apply(Options{}))
			if err != nil || !p.Empty() {
				t.Errorf("build: %v empty=%v", err, p.Empty())
			}
		})
	}
}

func TestParseFileTargetNames(t *testing.T) {
	// Routes named via #name are known by that name, not by the scheme.
	env := FileEnv{Routes: []string{"primary", "gelf"}}
	if res := ParseFile([]byte("targets:\n  primary:\n    rules: []\n"), env); res.Err() != nil {
		t.Errorf("named target: %v", res.Err())
	}
	res := ParseFile([]byte("targets:\n  syslog:\n    rules: []\n"), env)
	if res.Err() == nil || !strings.Contains(res.Err().Error(), "2:3") {
		t.Errorf("scheme name of a renamed route must be unknown: %v", res.Err())
	}
	// Two default-named routes: the name is ambiguous, the other one is fine.
	env = FileEnv{Routes: []string{"syslog", "gelf"}, Ambiguous: []string{"syslog"}}
	res = ParseFile([]byte("targets:\n  syslog:\n    rules: []\n  gelf:\n    rules: []\n"), env)
	if len(res.Errors) != 1 || res.Errors[0].Line != 2 || !strings.Contains(res.Errors[0].Message, "ambiguous") {
		t.Errorf("errors %v", res.Errors)
	}
	// A target name is case sensitive.
	res = ParseFile([]byte("targets:\n  Syslog:\n    rules: []\n"), env)
	if res.Err() == nil {
		t.Error("Syslog must not match syslog")
	}
}

func TestParseFileDefaultsSelection(t *testing.T) {
	env := FileEnv{Routes: []string{"syslog"}, DefaultRules: "v1"}
	// `latest` in the file wins over the option and resolves to the newest set.
	res := ParseFile([]byte("defaults: latest\n"), env)
	if res.Err() != nil {
		t.Fatal(res.Err())
	}
	latest, _ := ResolveDefaults("latest")
	p, _, err := Build(res.Config.Apply(Options{DefaultRules: "off"}))
	if err != nil || p.version != latest || latest == "" {
		t.Errorf("version %q err %v, want %q", p.version, err, latest)
	}
	// Option latest, file pins v1.
	res = ParseFile([]byte("defaults: v1\n"), FileEnv{DefaultRules: "latest"})
	p, _, _ = Build(res.Config.Apply(Options{DefaultRules: "latest"}))
	if p.version != "v1" {
		t.Errorf("version %q", p.version)
	}
	// Without defaults in the file the option stays.
	res = ParseFile([]byte("rules: []\n"), FileEnv{DefaultRules: "v1"})
	if got := res.Config.Apply(Options{DefaultRules: "v1"}).DefaultRules; got != "v1" {
		t.Errorf("got %q", got)
	}
	// Case matters, like for the add-on option.
	if res := ParseFile([]byte("defaults: LATEST\n"), env); res.Err() == nil {
		t.Error("LATEST accepted")
	}
}

func TestDisableDefaultsWarnings(t *testing.T) {
	tests := []struct {
		name, in, option string
		wantWarn         []string
	}{
		{"known rule, option v1", "disable_defaults: [ha-core-log]\n", "v1", nil},
		{"unknown rule", "disable_defaults: [nope]\n", "v1", []string{`1:20 disable_defaults: no rule named "nope"`}},
		{"two unknown, line numbers", "disable_defaults:\n  - a\n  - ha-core-log\n  - b\n", "v1", []string{`2:5`, `4:5`}},
		{"no defaults at all", "disable_defaults: [ha-core-log]\n", "", []string{"1:1 disable_defaults has no effect"}},
		{"file off beats option", "defaults: off\ndisable_defaults: [ha-core-log]\n", "v1", []string{"2:1 disable_defaults has no effect"}},
		{"file v1 beats option off", "defaults: v1\ndisable_defaults: [ha-core-log]\n", "off", nil},
		{"latest", "defaults: latest\ndisable_defaults: [nope]\n", "", []string{`2:20`}},
		{"empty list", "disable_defaults: []\n", "", nil},
		{"duplicate names", "disable_defaults: [ha-core-log, ha-core-log]\n", "v1", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := ParseFile([]byte(tt.in), FileEnv{DefaultRules: tt.option})
			if res.Err() != nil {
				t.Fatal(res.Err())
			}
			var got []string
			for _, w := range res.Warnings {
				got = append(got, w.String())
			}
			if len(got) != len(tt.wantWarn) {
				t.Fatalf("warnings %q, want %q", got, tt.wantWarn)
			}
			for i := range got {
				if !strings.HasPrefix(got[i], tt.wantWarn[i]) {
					t.Errorf("warning %d = %q, want prefix %q", i, got[i], tt.wantWarn[i])
				}
			}
		})
	}
}

func TestParseFileHuge(t *testing.T) {
	var b strings.Builder
	b.WriteString("rules:\n")
	const n = 5000
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "  - name: r%d\n    when: { container: 'addon_*_x%d', match: 'line %d' }\n    set: { level: warning }\n", i, i, i)
	}
	b.WriteString("targets:\n  syslog:\n    rules:\n      - name: bad\n        when: { match: '(' }\n        drop: true\n")
	res := ParseFile([]byte(b.String()), FileEnv{Routes: []string{"syslog"}})
	if len(res.Errors) != 1 || res.Errors[0].Line != 3*n+6 {
		t.Fatalf("errors %v, want one at line %d", res.Errors, 3*n+6)
	}
	ok := strings.Replace(b.String(), "'('", "'x'", 1)
	res = ParseFile([]byte(ok), FileEnv{Routes: []string{"syslog"}})
	if res.Err() != nil || len(res.Config.Rules) != n {
		t.Fatalf("err %v", res.Err())
	}
	// One long line and a deeply nested document must not hang or crash.
	long := "rules:\n  - name: a\n    when: { match: '" + strings.Repeat("a", 1<<20) + "' }\n    drop: true\n"
	if res := ParseFile([]byte(long), FileEnv{}); res.Err() != nil {
		t.Errorf("long regex: %v", res.Err())
	}
	deep := strings.Repeat("[", 100000)
	if res := ParseFile([]byte("rules: "+deep), FileEnv{}); res.Err() == nil {
		t.Error("deep nesting must be an error")
	}
}

func TestLoadFileUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permissions are not enforced")
	}
	path := filepath.Join(t.TempDir(), "logspout.yaml")
	if err := os.WriteFile(path, []byte("rules: []\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	res := LoadFile(path, FileEnv{})
	if res.Err() == nil || res.Config != nil || !strings.Contains(res.Err().Error(), "permission denied") {
		t.Errorf("%+v", res)
	}
	// A broken symlink counts as missing, like a missing file.
	link := filepath.Join(t.TempDir(), "l.yaml")
	if err := os.Symlink("/nonexistent/x.yaml", link); err != nil {
		t.Fatal(err)
	}
	if res := LoadFile(link, FileEnv{}); res.Err() != nil {
		t.Errorf("dangling symlink: %v", res.Err())
	}
}
