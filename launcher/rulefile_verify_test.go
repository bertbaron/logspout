package launcher

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gliderlabs/logspout/pipeline"
	"github.com/gliderlabs/logspout/router"
)

func startWith(t *testing.T, options string) string {
	t.Helper()
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

// withPipelineFile adds PIPELINE_FILE to the env option of a JSON options body.
func withPipelineFile(t *testing.T, path, body string) string {
	t.Helper()
	var opts map[string]any
	if err := json.Unmarshal([]byte(body), &opts); err != nil {
		t.Fatal(err)
	}
	env, _ := opts["env"].([]any)
	opts["env"] = append(env, map[string]any{"name": "PIPELINE_FILE", "value": path})
	out, err := json.Marshal(opts)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func startWithFile(t *testing.T, path, body string) string {
	t.Helper()
	return startWith(t, withPipelineFile(t, path, body))
}

func writeRuleFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "logspout.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// PIPELINE_FILE set through the add-on `env` option points at the rule file.
func TestPipelineFileFromEnvOption(t *testing.T) {
	path := writeRuleFile(t, "rules:\n  - name: g\n    drop: true\n")
	opts, _ := json.Marshal(map[string]any{
		"routes": []string{"gelf://g:12201"},
		"env":    []map[string]string{{"name": "PIPELINE_FILE", "value": path}},
	})
	startWith(t, string(opts))
	if router.CurrentProcessor() == nil {
		t.Fatal("rule file from the env option not used")
	}
	if s := currentSummary(t); !strings.Contains(s, "1 user rules (1 global)") {
		t.Errorf("summary %q", s)
	}
}

// Without PIPELINE_FILE the default path is used. It does not exist on a test host.
func TestPipelineFileDefaultPath(t *testing.T) {
	if pipeline.DefaultFilePath != "/config/logspout.yaml" {
		t.Fatalf("default path %q", pipeline.DefaultFilePath)
	}
	if _, err := os.Stat(pipeline.DefaultFilePath); err == nil {
		t.Skip("host has a rule file at the default path")
	}
	out := startWith(t, `{"routes": ["gelf://g:12201"]}`)
	if router.CurrentProcessor() != nil || strings.Contains(out, "ERROR") {
		t.Errorf("processor %v log %q", router.CurrentProcessor(), out)
	}
}

func TestRuleFileTargets(t *testing.T) {
	tests := []struct {
		name, routes, file string
		wantErr            string // empty: valid
	}{
		{"named via #name", `["syslog+tcp://a:514#primary","syslog+tcp://b:514#backup"]`, "targets:\n  primary:\n    rules:\n      - {name: a, drop: true}\n", ""},
		{"scheme name of named route", `["syslog+tcp://a:514#primary"]`, "targets:\n  syslog:\n    rules: []\n", `2:3 unknown target "syslog"`},
		{"two default-named routes", `["syslog+tcp://a:514","syslog+tcp://b:514"]`, "targets:\n  syslog:\n    rules: []\n", `2:3 ambiguous target name "syslog"`},
		{"one of two default-named is named", `["syslog+tcp://a:514","syslog+tcp://b:514#backup"]`, "targets:\n  syslog:\n    rules: [{name: a, drop: true}]\n  backup:\n    rules: []\n", ""},
		{"unknown target", `["gelf://g:12201"]`, "targets:\n  loki:\n    rules: []\n", `2:3 unknown target "loki" (routes: gelf)`},
		{"duplicate target", `["gelf://g:12201"]`, "targets:\n  gelf:\n    rules: []\n  gelf:\n    rules: []\n", `4:3 duplicate target "gelf"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := startWithFile(t, writeRuleFile(t, tt.file), `{"routes": `+tt.routes+`}`)
			if tt.wantErr == "" {
				if strings.Contains(out, "ERROR") || router.CurrentProcessor() == nil {
					t.Errorf("want valid, log:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, tt.wantErr) || !strings.Contains(out, "NOT used") {
				t.Errorf("log lacks %q:\n%s", tt.wantErr, out)
			}
			if router.CurrentProcessor() != nil {
				t.Errorf("processor installed for an invalid file")
			}
		})
	}
}

func TestRuleFileDefaultsVersusOption(t *testing.T) {
	routes := `"routes": ["gelf://g:12201"]`
	t.Run("file latest, option off", func(t *testing.T) {
		startWithFile(t, writeRuleFile(t, "defaults: latest\n"), `{`+routes+`, "default_rules": "off"}`)
		if s := currentSummary(t); !strings.Contains(s, "default rules v1") {
			t.Errorf("summary %q", s)
		}
	})
	t.Run("file off, option latest", func(t *testing.T) {
		startWithFile(t, writeRuleFile(t, "defaults: off\n"), `{`+routes+`, "default_rules": "latest"}`)
		if router.CurrentProcessor() != nil {
			t.Errorf("processor installed: %s", currentSummary(t))
		}
	})
	t.Run("file without defaults keeps option", func(t *testing.T) {
		startWithFile(t, writeRuleFile(t, "rules:\n  - {name: a, drop: true}\n"), `{`+routes+`, "default_rules": "v1"}`)
		if s := currentSummary(t); !strings.Contains(s, "default rules v1") || !strings.Contains(s, "1 user rules") {
			t.Errorf("summary %q", s)
		}
	})
	t.Run("disable_defaults warning with option off", func(t *testing.T) {
		out := startWithFile(t, writeRuleFile(t, "disable_defaults: [ha-core-log]\n"), `{`+routes+`}`)
		if !strings.Contains(out, "warning:") || !strings.Contains(out, "1:1 disable_defaults has no effect") || strings.Contains(out, "ERROR") {
			t.Errorf("log:\n%s", out)
		}
		if router.CurrentProcessor() != nil {
			t.Error("processor installed")
		}
	})
	t.Run("disabling a v1 rule removes it", func(t *testing.T) {
		startWithFile(t, writeRuleFile(t, "disable_defaults: [ha-core-log]\n"), `{`+routes+`, "default_rules": "v1"}`)
		with := currentSummary(t)
		startWithFile(t, writeRuleFile(t, "rules: []\n"), `{`+routes+`, "default_rules": "v1"}`)
		if with == currentSummary(t) {
			t.Errorf("disable_defaults changed nothing: %s", with)
		}
	})
}

func TestRuleFileOddContent(t *testing.T) {
	routes := `{"routes": ["gelf://g:12201"]}`
	t.Run("BOM and CRLF", func(t *testing.T) {
		content := "\xef\xbb\xbf" + strings.ReplaceAll("rules:\n  - name: a\n    drop: true\n", "\n", "\r\n")
		out := startWithFile(t, writeRuleFile(t, content), routes)
		if strings.Contains(out, "ERROR") || router.CurrentProcessor() == nil {
			t.Errorf("log:\n%s", out)
		}
	})
	t.Run("invalid yaml starts without processor", func(t *testing.T) {
		out := startWithFile(t, writeRuleFile(t, "rules:\n\t- a\n"), routes)
		if router.CurrentProcessor() != nil || !strings.Contains(out, "ERROR") || !strings.Contains(out, "line 2") {
			t.Errorf("log:\n%s", out)
		}
	})
	t.Run("every problem is logged", func(t *testing.T) {
		out := startWithFile(t, writeRuleFile(t, "rulez: []\ntargets:\n  x: {}\n"), routes)
		for _, want := range []string{`1:1 unknown key "rulez"`, `3:3 unknown target "x"`} {
			if !strings.Contains(out, want) {
				t.Errorf("log lacks %q:\n%s", want, out)
			}
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("permissions are not enforced")
		}
		path := writeRuleFile(t, "rules: []\n")
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		out := startWithFile(t, path, routes)
		if router.CurrentProcessor() != nil || !strings.Contains(out, "permission denied") {
			t.Errorf("log:\n%s", out)
		}
	})
	t.Run("PIPELINE_FILE is a directory", func(t *testing.T) {
		out := startWithFile(t, t.TempDir(), routes)
		if router.CurrentProcessor() != nil || !strings.Contains(out, "ERROR") {
			t.Errorf("log:\n%s", out)
		}
	})
}

// No file and no options: no processor, no new env vars, no log noise.
func TestNoFileNoOptionsIsOldBehavior(t *testing.T) {
	out := startWithFile(t, filepath.Join(t.TempDir(), "absent.yaml"), `{"routes": ["syslog+tcp://a:514","gelf://g:12201"], "hostname": "ha"}`)
	if router.CurrentProcessor() != nil {
		t.Error("processor installed")
	}
	if strings.Contains(out, "ERROR") || strings.Contains(out, "warning") {
		t.Errorf("unexpected log:\n%s", out)
	}
}
