package launcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/gliderlabs/logspout/runner"
)

const (
	// DefaultDockerSocketPath is the Home Assistant Docker socket mount.
	DefaultDockerSocketPath = "/run/docker.sock"
	// DefaultInactivityTimeout keeps the existing addon behavior for Docker log streaming.
	DefaultInactivityTimeout = "5m"
	// DefaultOptionsPath is where Home Assistant writes addon options.
	DefaultOptionsPath = "/data/options.json"
	// DefaultRouteStorePath is where persisted route files live in the addon container.
	DefaultRouteStorePath = "/data/routes"
	defaultHostname       = "homeassistant"
)

var envNameRegexp = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var managedEnvironmentKeys = []string{
	"DOCKER_HOST",
	"INACTIVITY_TIMEOUT",
	"ROUTESPATH",
	"ROUTE_URIS",
	"STRIP_ANSI",
	"SYSLOG_HOSTNAME",
}

var reservedEnvironmentKeys = map[string]struct{}{
	"DOCKER_HOST":        {},
	"INACTIVITY_TIMEOUT": {},
	"ROUTESPATH":         {},
	"ROUTE_URIS":         {},
	"STRIP_ANSI":         {},
	"SYSLOG_HOSTNAME":    {},
}

// Config is the Home Assistant addon configuration persisted in /data/options.json.
type Config struct {
	Env       []EnvironmentEntry `json:"env"`
	Hostname  string             `json:"hostname"`
	Routes    []string           `json:"routes"`
	StripANSI bool               `json:"strip_ansi"`
}

// EnvironmentEntry models a name/value item from the addon config.
type EnvironmentEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Options configure launcher execution.
type Options struct {
	DockerSocketPath string
	OptionsPath      string
	RouteStorePath   string
	Version          string
}

// Run loads the Home Assistant config, configures the environment, and starts Logspout.
func Run(opts Options) error {
	return RunWithRunner(opts, runner.Run)
}

// RunWithRunner is like Run but allows tests to inject a fake runtime bootstrap.
func RunWithRunner(opts Options, start func(runner.Options) error) error {
	socketPath := valueOrDefault(opts.DockerSocketPath, DefaultDockerSocketPath)
	if err := validateDockerSocket(socketPath); err != nil {
		return err
	}

	config, err := LoadConfig(valueOrDefault(opts.OptionsPath, DefaultOptionsPath))
	if err != nil {
		return err
	}

	env, err := BuildEnvironment(config, socketPath, valueOrDefault(opts.RouteStorePath, DefaultRouteStorePath))
	if err != nil {
		return err
	}

	for _, key := range managedEnvironmentKeys {
		if err := os.Unsetenv(key); err != nil {
			return fmt.Errorf("clear %s: %w", key, err)
		}
	}
	for key, value := range env {
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set %s: %w", key, err)
		}
	}

	return start(runner.Options{
		RouteURIs: config.Routes,
		Version:   opts.Version,
	})
}

// LoadConfig reads and validates the Home Assistant addon options file.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if config.Hostname == "" {
		config.Hostname = defaultHostname
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// Validate checks the addon config for values the launcher cannot safely apply.
func (c Config) Validate() error {
	seen := make(map[string]struct{}, len(c.Env))
	for i, route := range c.Routes {
		if route == "" {
			return fmt.Errorf("routes[%d] must not be empty", i)
		}
	}
	for i, entry := range c.Env {
		if !envNameRegexp.MatchString(entry.Name) {
			return fmt.Errorf("env[%d].name %q is not a valid environment variable name", i, entry.Name)
		}
		if _, reserved := reservedEnvironmentKeys[entry.Name]; reserved {
			return fmt.Errorf("env[%d].name %q is reserved by the Home Assistant launcher", i, entry.Name)
		}
		if _, duplicate := seen[entry.Name]; duplicate {
			return fmt.Errorf("env[%d].name %q is duplicated", i, entry.Name)
		}
		seen[entry.Name] = struct{}{}
	}
	return nil
}

// BuildEnvironment returns the managed environment for launching Logspout.
func BuildEnvironment(config Config, dockerSocketPath, routeStorePath string) (map[string]string, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	env := map[string]string{
		"DOCKER_HOST":        "unix://" + dockerSocketPath,
		"INACTIVITY_TIMEOUT": DefaultInactivityTimeout,
		"ROUTESPATH":         routeStorePath,
		"SYSLOG_HOSTNAME":    config.Hostname,
	}
	if config.StripANSI {
		env["STRIP_ANSI"] = "true"
	}
	for _, entry := range config.Env {
		env[entry.Name] = entry.Value
	}
	return env, nil
}

func validateDockerSocket(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("docker socket not found at %s; disable protection mode and mount the supervisor Docker socket", path)
		}
		return fmt.Errorf("stat docker socket %s: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists but is not a unix socket", path)
	}
	return nil
}

func valueOrDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
