package launcher

import (
	"log"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gliderlabs/logspout/router"
	"github.com/gliderlabs/logspout/runner"
)

func TestLoadConfigAppliesDefaults(t *testing.T) {
	optionsPath := filepath.Join(t.TempDir(), "options.json")
	err := os.WriteFile(optionsPath, []byte(`{"routes":["raw+tcp://sink:1514"]}`), 0600)
	if err != nil {
		t.Fatalf("write options: %v", err)
	}

	config, err := LoadConfig(optionsPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.Hostname != defaultHostname {
		t.Fatalf("expected default hostname %q, got %q", defaultHostname, config.Hostname)
	}
}

func TestBuildEnvironmentMapsAddonConfig(t *testing.T) {
	config := Config{
		Env: []EnvironmentEntry{
			{Name: "RAW_FORMAT", Value: "{{.Data}},{{.Container.Name}}"},
		},
		Hostname:  "ha-addon",
		Routes:    []string{"raw+tcp://sink:1514"},
		StripANSI: true,
	}

	env, err := BuildEnvironment(config, "/tmp/docker.sock", "/tmp/routes")
	if err != nil {
		t.Fatalf("BuildEnvironment() error = %v", err)
	}

	if got := env["DOCKER_HOST"]; got != "unix:///tmp/docker.sock" {
		t.Fatalf("expected DOCKER_HOST to use mounted socket, got %q", got)
	}
	if got := env["ROUTESPATH"]; got != "/tmp/routes" {
		t.Fatalf("expected ROUTESPATH override, got %q", got)
	}
	if got := env["SYSLOG_HOSTNAME"]; got != "ha-addon" {
		t.Fatalf("expected mapped hostname, got %q", got)
	}
	if got := env["STRIP_ANSI"]; got != "true" {
		t.Fatalf("expected STRIP_ANSI=true, got %q", got)
	}
	if got := env["RAW_FORMAT"]; got != "{{.Data}},{{.Container.Name}}" {
		t.Fatalf("expected RAW_FORMAT to preserve commas, got %q", got)
	}
}

func TestBuildEnvironmentRejectsReservedEnv(t *testing.T) {
	_, err := BuildEnvironment(Config{
		Env: []EnvironmentEntry{{Name: "DOCKER_HOST", Value: "tcp://example"}},
	}, "/tmp/docker.sock", "/tmp/routes")
	if err == nil {
		t.Fatal("expected reserved env validation error")
	}
}

func TestBuildEnvironmentRejectsDuplicateEnvNames(t *testing.T) {
	_, err := BuildEnvironment(Config{
		Env: []EnvironmentEntry{
			{Name: "RAW_FORMAT", Value: "{{.Data}}"},
			{Name: "RAW_FORMAT", Value: "{{.Container.Name}}"},
		},
	}, "/tmp/docker.sock", "/tmp/routes")
	if err == nil {
		t.Fatal("expected duplicate env validation error")
	}
}

func TestRunWithRunnerPassesRoutesAndManagedEnvironment(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "logspout-launcher-")
	if err != nil {
		t.Fatalf("make temp dir: %v", err)
	}
	defer os.RemoveAll(socketDir)
	socketPath := filepath.Join(socketDir, "docker.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer listener.Close()

	optionsPath := filepath.Join(t.TempDir(), "options.json")
	err = os.WriteFile(optionsPath, []byte(`{
		"routes":["raw+tcp://sink:1514?filter.name=comma,value"],
		"hostname":"ha-addon",
		"strip_ansi":true,
		"env":[{"name":"RAW_FORMAT","value":"{{.Data}},{{.Container.Name}}"}]
	}`), 0600)
	if err != nil {
		t.Fatalf("write options: %v", err)
	}

	t.Setenv("STRIP_ANSI", "stale")

	var captured runner.Options
	err = RunWithRunner(Options{
		DockerSocketPath: socketPath,
		OptionsPath:      optionsPath,
		RouteStorePath:   "/tmp/routes",
		Version:          "test-version",
	}, func(opts runner.Options) error {
		captured = opts
		return nil
	})
	if err != nil {
		t.Fatalf("RunWithRunner() error = %v", err)
	}

	if len(captured.RouteURIs) != 1 || captured.RouteURIs[0] != "raw+tcp://sink:1514?filter.name=comma,value" {
		t.Fatalf("expected explicit route URI to be preserved, got %#v", captured.RouteURIs)
	}
	if got := os.Getenv("DOCKER_HOST"); got != "unix://"+socketPath {
		t.Fatalf("expected DOCKER_HOST to be set, got %q", got)
	}
	if got := os.Getenv("SYSLOG_HOSTNAME"); got != "ha-addon" {
		t.Fatalf("expected hostname to be exported, got %q", got)
	}
	if got := os.Getenv("RAW_FORMAT"); got != "{{.Data}},{{.Container.Name}}" {
		t.Fatalf("expected custom env value to survive unchanged, got %q", got)
	}
}

func TestRunWithRunnerUsesJournalWithoutSocket(t *testing.T) {
	t.Setenv("DOCKER_HOST", "stale")
	optionsPath := filepath.Join(t.TempDir(), "options.json")
	err := os.WriteFile(optionsPath, []byte(`{"routes":["raw+tcp://sink:1514"]}`), 0600)
	if err != nil {
		t.Fatalf("write options: %v", err)
	}

	journalUsed := false
	err = RunWithRunner(Options{
		DockerSocketPath: filepath.Join(t.TempDir(), "missing.sock"),
		OptionsPath:      optionsPath,
		UseJournal:       func() { journalUsed = true },
	}, func(opts runner.Options) error {
		return nil
	})
	if err != nil {
		t.Fatalf("RunWithRunner() error = %v", err)
	}
	if !journalUsed {
		t.Fatal("expected journal log source without docker socket")
	}
	if got := os.Getenv("DOCKER_HOST"); got != "" {
		t.Fatalf("expected DOCKER_HOST to be cleared, got %q", got)
	}
}

func TestRunWithRunnerFailsWhenDockerRequestedWithoutSocket(t *testing.T) {
	optionsPath := filepath.Join(t.TempDir(), "options.json")
	err := os.WriteFile(optionsPath, []byte(`{"env":[{"name":"LOG_SOURCE","value":"docker"}]}`), 0600)
	if err != nil {
		t.Fatalf("write options: %v", err)
	}

	err = RunWithRunner(Options{
		DockerSocketPath: filepath.Join(t.TempDir(), "missing.sock"),
		OptionsPath:      optionsPath,
	}, func(opts runner.Options) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected missing socket error")
	}
}

func TestSelectLogSourceRejectsUnknownValue(t *testing.T) {
	config := Config{Env: []EnvironmentEntry{{Name: "LOG_SOURCE", Value: "files"}}}
	if _, err := selectLogSource(config, "/nonexistent"); err == nil {
		t.Fatal("expected error for unknown LOG_SOURCE")
	}
}

func TestConfigValidateRouteNames(t *testing.T) {
	tests := []struct {
		name    string
		routes  []string
		wantErr bool
	}{
		{"no fragment", []string{"syslog://a:1", "gelf://b:2"}, false},
		{"duplicate default allowed", []string{"syslog://a:1", "syslog://b:2"}, false},
		{"duplicate explicit", []string{"syslog://a:1#x", "gelf://b:2#x"}, false},
		{"explicit equals other default", []string{"syslog://a:1", "gelf://b:2#syslog"}, false},
		{"invalid name", []string{"syslog://a:1#a b"}, false},
		{"unparsable uri", []string{"syslog://a:1/%zz"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Config{Routes: tt.routes}.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func runWithOptionsJSON(t *testing.T, body string) (runner.Options, bool, error) {
	t.Helper()
	return runWithOptionsJSONIngress(t, body, "")
}

func runWithOptionsJSONIngress(t *testing.T, body, ingressAddr string) (runner.Options, bool, error) {
	t.Helper()
	optionsPath := filepath.Join(t.TempDir(), "options.json")
	if err := os.WriteFile(optionsPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	var got runner.Options
	started := false
	err := RunWithRunner(Options{
		DockerSocketPath: filepath.Join(t.TempDir(), "missing.sock"),
		OptionsPath:      optionsPath,
		UseJournal:       func() {},
		IngressAddr:      ingressAddr,
	}, func(o runner.Options) error {
		got, started = o, true
		return nil
	})
	return got, started, err
}

// A route name never stops start-up: bad or clashing names only log a warning.
func TestRunWithRunnerRouteNames(t *testing.T) {
	tests := []struct {
		name   string
		routes string
	}{
		{"old options without fragments", `["syslog+tcp://a:514","syslog+tcp://b:514","gelf://g:12201"]`},
		{"named routes", `["syslog+tcp://a:514#primary","syslog+tcp://b:514#backup"]`},
		{"duplicate explicit", `["syslog://a:514#x","gelf://g:1#x"]`},
		{"explicit equals other default", `["syslog://a:514","gelf://g:1#syslog"]`},
		{"invalid", `["syslog://a:514#a b"]`},
		{"empty fragment", `["syslog://a:514#"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, started, err := runWithOptionsJSON(t, `{"routes":`+tt.routes+`}`)
			if err != nil || !started {
				t.Fatalf("started = %v, err = %v", started, err)
			}
			if len(got.RouteURIs) == 0 {
				t.Error("routes not passed on")
			}
		})
	}
}

type nopAdapter struct{}

func (nopAdapter) Stream(logstream chan *router.Message) {}

type routeShape struct {
	Name, Adapter, Address, Path, FilterName string
	Options                                  map[string]string
}

// Routes with bad, duplicate or clashing #fragments are created exactly like
// routes without fragments; only the name differs.
func TestRouteNameProblemsDoNotChangeRoutes(t *testing.T) {
	var captured []*router.Route
	router.AdapterFactories.Unregister("lcap")
	router.AdapterFactories.Register(func(r *router.Route) (router.LogAdapter, error) {
		captured = append(captured, r)
		return nopAdapter{}, nil
	}, "lcap")
	t.Cleanup(func() { router.AdapterFactories.Unregister("lcap") })
	t.Setenv("ROUTESPATH", filepath.Join(t.TempDir(), "absent"))
	router.SetProcessor(nil)

	var logBuf strings.Builder
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	run := func(routes string) []routeShape {
		captured = nil
		logBuf.Reset()
		got, started, err := runWithOptionsJSON(t, `{"routes":`+routes+`}`)
		if err != nil || !started {
			t.Fatalf("started = %v, err = %v", started, err)
		}
		if err := router.Routes.SetupWithArgs(nil, got.RouteURIs); err != nil {
			t.Fatalf("adding routes: %v", err)
		}
		if router.CurrentProcessor() != nil {
			t.Error("processor installed")
		}
		var out []routeShape
		for _, r := range captured {
			out = append(out, routeShape{r.Name, r.Adapter, r.Address, r.Path, r.FilterName, r.Options})
			go func(r *router.Route) { <-r.Closer() }(r)
			router.Routes.Remove(r.ID)
		}
		return out
	}

	plain := run(`["lcap+udp://h:514?filter.name=web","lcap+udp://h:514?filter.name=web","lcap+tcp://i:514","lcap://j:1/p?a=b","lcap://k:1"]`)
	odd := run(`["lcap+udp://h:514?filter.name=web#my logs","lcap+udp://h:514?filter.name=web#a/b","lcap+tcp://i:514#x","lcap://j:1/p?a=b#x","lcap://k:1#lcap"]`)
	if len(plain) != 5 || len(odd) != 5 {
		t.Fatalf("routes: %d without, %d with fragments", len(plain), len(odd))
	}
	for i := range plain {
		// Names differ for the routes that got a valid fragment.
		a, b := plain[i], odd[i]
		a.Name, b.Name = "", ""
		if !reflect.DeepEqual(a, b) {
			t.Errorf("route %d: %+v, want %+v", i, odd[i], plain[i])
		}
	}
	for _, want := range []string{`"a/b"`, `"my logs"`, `more than one route has the name "x"`, `more than one route has the name "lcap"`} {
		if !strings.Contains(logBuf.String(), want) {
			t.Errorf("log lacks %s:\n%s", want, logBuf.String())
		}
	}
	// Invalid fragments are ignored: the route keeps its default name.
	if odd[0].Name != "lcap" || odd[1].Name != "lcap" || odd[2].Name != "x" || odd[3].Name != "x" {
		t.Errorf("names = %+v", odd)
	}
}

// The env option is applied after validation, so a fragment from env must give the same verdict in both places.
func TestRunWithRunnerRouteNameFromEnvOption(t *testing.T) {
	_, started, err := runWithOptionsJSON(t, `{
		"routes":["syslog://a:514#${MY_ROUTE}","gelf://g:1#dup"],
		"env":[{"name":"MY_ROUTE","value":"dup"}]}`)
	if err != nil || !started {
		t.Errorf("started = %v, err = %v", started, err)
	}
}

func TestBuildEnvironmentPipelineOptions(t *testing.T) {
	tests := []struct {
		name        string
		config      Config
		wantDefault string
		wantExclude string
	}{
		{"absent", Config{}, "", ""},
		{"off", Config{DefaultRules: "off"}, "", ""},
		{"empty list", Config{ExcludeContainers: []string{}}, "", ""},
		{"latest", Config{DefaultRules: "latest"}, "latest", ""},
		{"v1", Config{DefaultRules: "v1"}, "v1", ""},
		{"exclude", Config{ExcludeContainers: []string{"homeassistant", "addon_*_mosquitto"}}, "", "homeassistant,addon_*_mosquitto"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := BuildEnvironment(tt.config, "", "/tmp/routes")
			if err != nil {
				t.Fatal(err)
			}
			got, hasDefault := env["DEFAULT_RULES"]
			if got != tt.wantDefault || hasDefault != (tt.wantDefault != "") {
				t.Errorf("DEFAULT_RULES = %q (set %v), want %q", got, hasDefault, tt.wantDefault)
			}
			got, hasExclude := env["EXCLUDE_CONTAINERS"]
			if got != tt.wantExclude || hasExclude != (tt.wantExclude != "") {
				t.Errorf("EXCLUDE_CONTAINERS = %q (set %v), want %q", got, hasExclude, tt.wantExclude)
			}
		})
	}
}

// Without the new options the environment is exactly what it was before.
func TestBuildEnvironmentWithoutPipelineOptionsAddsNothing(t *testing.T) {
	env, err := BuildEnvironment(Config{Hostname: "h"}, "/run/docker.sock", "/data/routes")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"INACTIVITY_TIMEOUT": DefaultInactivityTimeout,
		"ROUTESPATH":         "/data/routes",
		"SYSLOG_HOSTNAME":    "h",
		"DOCKER_HOST":        "unix:///run/docker.sock",
	}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("env = %v, want %v", env, want)
	}
}

// Typos in the level 1 options must not stop logging.
func TestPipelineOptionsFromEnvIsLenient(t *testing.T) {
	tests := []struct {
		name        string
		defaults    string
		exclude     string
		wantDefault string
		wantExclude []string
	}{
		{"trim and skip empty", " v1 ", " a* , ,b,", "v1", []string{"a*", "b"}},
		{"only blanks", "", " , ", "", nil},
		{"unknown default", "v99", "a", "", []string{"a"}},
		{"uppercase default", "Latest", "", "", nil},
		{"bad glob drops the option", "latest", "a,b[", "latest", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"DEFAULT_RULES": tt.defaults, "EXCLUDE_CONTAINERS": tt.exclude}
			got := pipelineOptionsFromEnv(func(k string) string { return env[k] })
			if got.DefaultRules != tt.wantDefault || !reflect.DeepEqual(got.ExcludeContainers, tt.wantExclude) {
				t.Errorf("got %q %q, want %q %q", got.DefaultRules, got.ExcludeContainers, tt.wantDefault, tt.wantExclude)
			}
		})
	}
}

func TestBuildEnvironmentTrimsExcludeContainers(t *testing.T) {
	env, err := BuildEnvironment(Config{ExcludeContainers: []string{" a ", "", "  "}, DefaultRules: " v1 "}, "", "/r")
	if err != nil {
		t.Fatal(err)
	}
	if env["EXCLUDE_CONTAINERS"] != "a" || env["DEFAULT_RULES"] != "v1" {
		t.Errorf("env %v", env)
	}
	env, _ = BuildEnvironment(Config{ExcludeContainers: []string{" ", ""}}, "", "/r")
	if _, ok := env["EXCLUDE_CONTAINERS"]; ok {
		t.Error("blank entries set EXCLUDE_CONTAINERS")
	}
}

func TestRunWithRunnerPipeline(t *testing.T) {
	tests := []struct {
		name       string
		options    string
		wantActive bool
	}{
		{"no options", `{"routes":["gelf://g:1"]}`, false},
		{"off", `{"default_rules":"off"}`, false},
		{"latest", `{"default_rules":"latest"}`, true},
		{"exclude", `{"exclude_containers":["homeassistant"]}`, true},
		{"env option", `{"env":[{"name":"EXCLUDE_CONTAINERS","value":"a,b"}]}`, true},
		{"env option wins", `{"default_rules":"v1","env":[{"name":"DEFAULT_RULES","value":"off"}]}`, false},
		{"invalid option starts without it", `{"default_rules":"v99"}`, false},
		{"invalid env option", `{"env":[{"name":"DEFAULT_RULES","value":"bogus"}]}`, false},
		{"invalid env glob", `{"env":[{"name":"EXCLUDE_CONTAINERS","value":"a["}]}`, false},
		{"invalid glob in option", `{"exclude_containers":["a["]}`, false},
		{"blank entries", `{"exclude_containers":[" ",""]}`, false},
		{"one bad option, one good", `{"default_rules":"v99","exclude_containers":["a"]}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DEFAULT_RULES", "stale")
			t.Setenv("EXCLUDE_CONTAINERS", "stale")
			router.SetProcessor(nil)
			t.Cleanup(func() { router.SetProcessor(nil) })
			_, started, err := runWithOptionsJSON(t, tt.options)
			if err != nil || !started {
				t.Fatalf("start-up failed: started=%v err=%v", started, err)
			}
			if active := router.CurrentProcessor() != nil; active != tt.wantActive {
				t.Errorf("processor active = %v, want %v", active, tt.wantActive)
			}
		})
	}
}
