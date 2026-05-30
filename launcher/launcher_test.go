package launcher

import (
	"net"
	"os"
	"path/filepath"
	"testing"

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

func TestRunWithRunnerFailsWithoutSocket(t *testing.T) {
	optionsPath := filepath.Join(t.TempDir(), "options.json")
	err := os.WriteFile(optionsPath, []byte(`{"routes":["raw+tcp://sink:1514"]}`), 0600)
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
