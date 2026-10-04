package launcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"

	"github.com/gliderlabs/logspout/journal"
	"github.com/gliderlabs/logspout/router"
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

	logSourceKey     = "LOG_SOURCE"
	logSourceDocker  = "docker"
	logSourceJournal = "journal"
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
	// UseJournal switches logspout to the journal log source. Defaults to router.UseJournalPump with journal.Open.
	UseJournal func()
}

// Run loads the Home Assistant config, configures the environment, and starts Logspout.
func Run(opts Options) error {
	return RunWithRunner(opts, runner.Run)
}

// RunWithRunner is like Run but allows tests to inject a fake runtime bootstrap.
func RunWithRunner(opts Options, start func(runner.Options) error) error {
	socketPath := valueOrDefault(opts.DockerSocketPath, DefaultDockerSocketPath)

	config, err := LoadConfig(valueOrDefault(opts.OptionsPath, DefaultOptionsPath))
	if err != nil {
		return err
	}

	logSource, err := selectLogSource(config, socketPath)
	if err != nil {
		return err
	}
	dockerSocket := socketPath
	if logSource == logSourceJournal {
		dockerSocket = ""
	}

	env, err := BuildEnvironment(config, dockerSocket, valueOrDefault(opts.RouteStorePath, DefaultRouteStorePath))
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

	if logSource == logSourceJournal {
		log.Println("# log source: journald")
		useJournal := opts.UseJournal
		if useJournal == nil {
			useJournal = func() { router.UseJournalPump(journal.Open) }
		}
		useJournal()
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
	// Validate already passed, so this cannot fail.
	ambiguous, _ := router.ValidateRouteNamesWithEnv(config.Routes, config.lookupEnv)
	for _, name := range ambiguous {
		log.Printf("warning: more than one route has the default name %q; add #name to the route URI so rules can target it", name)
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
	if _, err := router.ValidateRouteNamesWithEnv(c.Routes, c.lookupEnv); err != nil {
		return err
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

// lookupEnv resolves a variable like AddFromURI will see it at start-up: the
// env option wins over the process environment.
func (c Config) lookupEnv(name string) string {
	for i := len(c.Env) - 1; i >= 0; i-- {
		if c.Env[i].Name == name {
			return c.Env[i].Value
		}
	}
	return os.Getenv(name)
}

// BuildEnvironment returns the managed environment for launching Logspout.
func BuildEnvironment(config Config, dockerSocketPath, routeStorePath string) (map[string]string, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	env := map[string]string{
		"INACTIVITY_TIMEOUT": DefaultInactivityTimeout,
		"ROUTESPATH":         routeStorePath,
		"SYSLOG_HOSTNAME":    config.Hostname,
	}
	if dockerSocketPath != "" {
		env["DOCKER_HOST"] = "unix://" + dockerSocketPath
	}
	if config.StripANSI {
		env["STRIP_ANSI"] = "true"
	}
	for _, entry := range config.Env {
		env[entry.Name] = entry.Value
	}
	return env, nil
}

// selectLogSource returns the log source to use. Home Assistant mounts the Docker socket only when
// protection mode is disabled, so without the socket the journal is used. LOG_SOURCE in the add-on env
// option or the process environment overrides this.
func selectLogSource(config Config, socketPath string) (string, error) {
	requested := os.Getenv(logSourceKey)
	for _, entry := range config.Env {
		if entry.Name == logSourceKey {
			requested = entry.Value
		}
	}

	switch requested {
	case "":
		if hasDockerSocket(socketPath) {
			return logSourceDocker, nil
		}
		return logSourceJournal, nil
	case logSourceJournal:
		return logSourceJournal, nil
	case logSourceDocker:
		if err := validateDockerSocket(socketPath); err != nil {
			return "", err
		}
		return logSourceDocker, nil
	default:
		return "", fmt.Errorf("%s %q is invalid, expected %q or %q", logSourceKey, requested, logSourceDocker, logSourceJournal)
	}
}

func hasDockerSocket(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode()&os.ModeSocket != 0
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
