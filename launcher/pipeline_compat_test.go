package launcher

import (
	"os"
	"reflect"
	"testing"

	"github.com/gliderlabs/logspout/router"
)

func envSnapshot(keys ...string) map[string]string {
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			out[k] = v
		}
	}
	return out
}

// An options.json of an existing install must not install a processor or set new variables.
func TestRunWithRunnerExistingInstallUnchanged(t *testing.T) {
	t.Setenv("DEFAULT_RULES", "stale")
	t.Setenv("EXCLUDE_CONTAINERS", "stale")
	router.SetProcessor(nil)
	t.Cleanup(func() { router.SetProcessor(nil) })

	body := `{
	  "routes": ["syslog+tcp://a:514#primary", "gelf://g:12201?filter.sources=stdout", "multiline+syslog://m:514"],
	  "hostname": "ha",
	  "strip_ansi": true,
	  "env": [{"name": "SYSLOG_FORMAT", "value": "rfc3164"}, {"name": "MULTILINE_ENABLE_DEFAULT", "value": "true"}]
	}`
	got, started, err := runWithOptionsJSON(t, body)
	if err != nil || !started {
		t.Fatalf("started=%v err=%v", started, err)
	}
	if router.CurrentProcessor() != nil {
		t.Error("processor installed")
	}
	if e := envSnapshot("DEFAULT_RULES", "EXCLUDE_CONTAINERS"); len(e) != 0 {
		t.Errorf("new env vars set: %v", e)
	}
	wantEnv := map[string]string{
		"INACTIVITY_TIMEOUT":       DefaultInactivityTimeout,
		"ROUTESPATH":               DefaultRouteStorePath,
		"SYSLOG_HOSTNAME":          "ha",
		"STRIP_ANSI":               "true",
		"SYSLOG_FORMAT":            "rfc3164",
		"MULTILINE_ENABLE_DEFAULT": "true",
	}
	for k, want := range wantEnv {
		if v := os.Getenv(k); v != want {
			t.Errorf("%s = %q, want %q", k, v, want)
		}
	}
	if len(got.RouteURIs) != 3 || got.RouteURIs[0] != "syslog+tcp://a:514#primary" {
		t.Errorf("routes = %v", got.RouteURIs)
	}
}

func TestPipelineOptionsFromEnvEdgeCases(t *testing.T) {
	tests := []struct {
		name        string
		rules, excl string
		wantRules   string
		wantExcl    []string
	}{
		{"all empty", "", "", "", nil},
		{"off", "off", "", "off", nil},
		{"latest", "latest", "", "latest", nil},
		{"upper case ignored", "LATEST", "", "", nil},
		{"mixed case v1 ignored", "V1", "", "", nil},
		{"padded is trimmed", " v1", "", "v1", nil},
		{"single entry", "", "a", "", []string{"a"}},
		{"empty entry skipped", "", "a,,b", "", []string{"a", "b"}},
		{"only comma", "", ",", "", nil},
		{"blank entry skipped", "", "a, ", "", []string{"a"}},
		{"bad glob ignores the option", "", "a,[", "", nil},
		{"valid classes", "", "a[0-9]*,b?", "", []string{"a[0-9]*", "b?"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"DEFAULT_RULES": tt.rules, "EXCLUDE_CONTAINERS": tt.excl}
			o := pipelineOptionsFromEnv(func(k string) string { return env[k] })
			if o.DefaultRules != tt.wantRules || !reflect.DeepEqual(o.ExcludeContainers, tt.wantExcl) {
				t.Errorf("got %q %#v, want %q %#v", o.DefaultRules, o.ExcludeContainers, tt.wantRules, tt.wantExcl)
			}
		})
	}
}

// An empty value in the env option behaves as "not set".
func TestRunWithRunnerEmptyEnvValuesInactive(t *testing.T) {
	router.SetProcessor(nil)
	t.Cleanup(func() { router.SetProcessor(nil) })
	_, _, err := runWithOptionsJSON(t, `{"default_rules":"v1","env":[{"name":"DEFAULT_RULES","value":""},{"name":"EXCLUDE_CONTAINERS","value":""}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if router.CurrentProcessor() != nil {
		t.Error("processor installed")
	}
}

// A reload that ends with no rules must remove the processor of the previous run.
func TestRunWithRunnerRemovesPreviousProcessor(t *testing.T) {
	router.SetProcessor(nil)
	t.Cleanup(func() { router.SetProcessor(nil) })
	if _, _, err := runWithOptionsJSON(t, `{"default_rules":"latest"}`); err != nil {
		t.Fatal(err)
	}
	if router.CurrentProcessor() == nil {
		t.Fatal("expected processor")
	}
	if _, _, err := runWithOptionsJSON(t, `{}`); err != nil {
		t.Fatal(err)
	}
	if router.CurrentProcessor() != nil {
		t.Error("processor still installed")
	}
}

func TestBuildEnvironmentDefaultRulesOffAddsNothing(t *testing.T) {
	env, err := BuildEnvironment(Config{DefaultRules: "off", ExcludeContainers: []string{}}, "", "/data/routes")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"DEFAULT_RULES", "EXCLUDE_CONTAINERS"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s set", k)
		}
	}
}
