package launcher

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gliderlabs/logspout/pipeline"
	"github.com/gliderlabs/logspout/router"
)

// runWithRuleFile starts the launcher with a rule file and returns the log output.
// A nil content means the file does not exist.
func runWithRuleFile(t *testing.T, options string, content *string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "logspout.yaml")
	if content != nil {
		if err := os.WriteFile(path, []byte(*content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	options = withPipelineFile(t, path, options)
	router.SetProcessor(nil)
	t.Cleanup(func() { router.SetProcessor(nil) })

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	if _, started, err := runWithOptionsJSON(t, options); err != nil || !started {
		t.Fatalf("started=%v err=%v", started, err)
	}
	return buf.String()
}

func currentSummary(t *testing.T) string {
	t.Helper()
	p, ok := router.CurrentProcessor().(*pipeline.Pipeline)
	if !ok {
		t.Fatalf("processor = %T", router.CurrentProcessor())
	}
	return p.Summary()
}

func TestRuleFileAtStartup(t *testing.T) {
	const routes = `"routes": ["syslog+tcp://a:514#primary", "gelf://g:12201"]`
	valid := "rules:\n  - name: g\n    drop: true\ntargets:\n  primary:\n    rules:\n      - name: t\n        drop: true\n"
	invalid := "rules:\n  - name: bad\n    when: { match: '(' }\n    drop: true\ntargets:\n  nope:\n    rules: []\n"
	override := "defaults: off\n"
	empty := ""

	tests := []struct {
		name       string
		options    string
		file       *string
		wantNil    bool
		wantInSum  []string
		wantInLog  []string
		wantNotLog []string
	}{
		{name: "no file, no options", options: `{` + routes + `}`, wantNil: true},
		{name: "empty file", options: `{` + routes + `}`, file: &empty, wantNil: true},
		{
			name: "valid file", options: `{` + routes + `}`, file: &valid,
			wantInSum: []string{"default rules off", "2 user rules (1 global)", "primary(1)"},
		},
		{
			name:      "file defaults win over option",
			options:   `{` + routes + `, "default_rules": "v1"}`,
			file:      &override,
			wantNil:   true,
			wantInLog: nil,
		},
		{
			name:    "invalid file keeps level 1 options",
			options: `{` + routes + `, "default_rules": "v1", "exclude_containers": ["/noisy"]}`,
			file:    &invalid,
			wantInSum: []string{
				"default rules v1", "excluded containers: noisy", "0 user rules",
			},
			wantInLog: []string{
				"ERROR:", "is invalid", "NOT used", "3:5", "unknown target \"nope\"",
			},
		},
		{
			name:    "invalid file and no options still starts",
			options: `{` + routes + `}`,
			file:    &invalid,
			wantNil: true,
			wantInLog: []string{
				"ERROR:", "3:5",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := runWithRuleFile(t, tt.options, tt.file)
			if tt.wantNil {
				if router.CurrentProcessor() != nil {
					t.Errorf("processor installed: %s", currentSummary(t))
				}
			} else {
				s := currentSummary(t)
				for _, want := range tt.wantInSum {
					if !strings.Contains(s, want) {
						t.Errorf("summary %q lacks %q", s, want)
					}
				}
			}
			for _, want := range tt.wantInLog {
				if !strings.Contains(out, want) {
					t.Errorf("log lacks %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestRuleFileWarningDoesNotInvalidate(t *testing.T) {
	file := "defaults: v1\ndisable_defaults: [no-such-rule]\n"
	out := runWithRuleFile(t, `{"routes": ["gelf://g:12201"]}`, &file)
	if !strings.Contains(out, "warning:") || strings.Contains(out, "ERROR") {
		t.Errorf("log:\n%s", out)
	}
	if s := currentSummary(t); !strings.Contains(s, "default rules v1") {
		t.Errorf("summary %q", s)
	}
}

// An install without file and options must not get a new log line, and an
// unknown disable_defaults name is logged once.
func TestRuleFileLogLines(t *testing.T) {
	out := runWithRuleFile(t, `{"routes": ["gelf://g:12201"]}`, nil)
	if strings.Contains(out, "pipeline:") {
		t.Errorf("summary logged without rules:\n%s", out)
	}
	file := "defaults: v1\ndisable_defaults: [no-such-rule]\n"
	out = runWithRuleFile(t, `{"routes": ["gelf://g:12201"]}`, &file)
	if n := strings.Count(out, "no-such-rule"); n != 1 {
		t.Errorf("name logged %d times:\n%s", n, out)
	}
}

func TestDebugPipelineOption(t *testing.T) {
	for _, tt := range []struct {
		env  string
		want bool
	}{{"", false}, {"true", true}, {"1", true}, {"false", false}, {"nonsense", false}} {
		if got := pipelineOptionsFromEnv(func(k string) string {
			if k == debugPipelineKey {
				return tt.env
			}
			return ""
		}).Debug; got != tt.want {
			t.Errorf("DEBUG_PIPELINE=%q: %v", tt.env, got)
		}
	}
}

// DEBUG_PIPELINE is no rule: alone it installs nothing. With rules it traces.
func TestDebugPipelineInvalidValueLogged(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	opts := pipelineOptionsFromEnv(func(k string) string {
		if k == debugPipelineKey {
			return "maybe"
		}
		return ""
	})
	if opts.Debug || !strings.Contains(buf.String(), "ERROR: DEBUG_PIPELINE") {
		t.Errorf("debug=%v log %q", opts.Debug, buf.String())
	}
}

func TestDebugPipelineFromEnvOption(t *testing.T) {
	debug := `"env": [{"name": "DEBUG_PIPELINE", "value": "true"}]`
	out := runWithRuleFile(t, `{"routes": ["gelf://g:12201"], `+debug+`}`, nil)
	if router.CurrentProcessor() != nil || strings.Contains(out, "trace") {
		t.Errorf("processor installed for DEBUG_PIPELINE alone: %s", out)
	}

	rules := "rules:\n  - name: g\n    when: { match: x }\n    drop: true\n"
	var buf bytes.Buffer
	out = runWithRuleFile(t, `{"routes": ["gelf://g:12201"], `+debug+`}`, &rules)
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	router.CurrentProcessor().Global(&router.Message{Data: "x", Source: "stdout"})
	if !strings.Contains(buf.String(), `pipeline trace: global`) || !strings.Contains(buf.String(), "dropped=true") {
		t.Errorf("no trace:\n%s", buf.String())
	}
}

func TestManagedKeysCleared(t *testing.T) {
	for _, k := range []string{"PIPELINE_FILE", "DEBUG_PIPELINE"} {
		found := false
		for _, m := range managedEnvironmentKeys {
			found = found || m == k
		}
		if !found {
			t.Errorf("%s is not managed", k)
		}
	}
}
