package launcher

import (
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
		{"duplicate explicit", []string{"syslog://a:1#x", "gelf://b:2#x"}, true},
		{"invalid name", []string{"syslog://a:1#a b"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Config{Routes: tt.routes}.Validate()
			if (err != nil) != tt.wantErr || (err != nil && !strings.Contains(err.Error(), "route name")) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func runWithOptionsJSON(t *testing.T, body string) (runner.Options, bool, error) {
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
	}, func(o runner.Options) error {
		got, started = o, true
		return nil
	})
	return got, started, err
}

func TestRunWithRunnerRouteNames(t *testing.T) {
	tests := []struct {
		name    string
		routes  string
		wantErr bool
	}{
		{"old options without fragments", `["syslog+tcp://a:514","syslog+tcp://b:514","gelf://g:12201"]`, false},
		{"named routes", `["syslog+tcp://a:514#primary","syslog+tcp://b:514#backup"]`, false},
		{"duplicate explicit", `["syslog://a:514#x","gelf://g:1#x"]`, true},
		{"explicit equals other default", `["syslog://a:514","gelf://g:1#syslog"]`, true},
		{"invalid", `["syslog://a:514#a b"]`, true},
		{"empty fragment", `["syslog://a:514#"]`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, started, err := runWithOptionsJSON(t, `{"routes":`+tt.routes+`}`)
			if (err != nil) != tt.wantErr || (err != nil && !strings.Contains(err.Error(), "route name")) {
				t.Fatalf("err = %v", err)
			}
			if tt.wantErr && started {
				t.Error("runner started despite invalid routes")
			}
			if !tt.wantErr && len(got.RouteURIs) == 0 {
				t.Error("routes not passed on")
			}
		})
	}
}

// The env option is applied after validation, so a fragment from env must give the same verdict in both places.
func TestRunWithRunnerRouteNameFromEnvOption(t *testing.T) {
	_, started, err := runWithOptionsJSON(t, `{
		"routes":["syslog://a:514#${MY_ROUTE}","gelf://g:1#dup"],
		"env":[{"name":"MY_ROUTE","value":"dup"}]}`)
	if err == nil && started {
		t.Error("duplicate explicit name via env option was not detected by the launcher")
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
