package launcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/gliderlabs/logspout/ingress"
	"github.com/gliderlabs/logspout/journal"
	"github.com/gliderlabs/logspout/pipeline"
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

	defaultRulesKey      = "DEFAULT_RULES"
	excludeContainersKey = "EXCLUDE_CONTAINERS"
	pipelineFileKey      = "PIPELINE_FILE"
	debugPipelineKey     = "DEBUG_PIPELINE"

	logSourceKey     = "LOG_SOURCE"
	logSourceDocker  = "docker"
	logSourceJournal = "journal"
)

var envNameRegexp = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// DEFAULT_RULES, EXCLUDE_CONTAINERS, PIPELINE_FILE and DEBUG_PIPELINE are managed but not reserved: the env option may set them.
var managedEnvironmentKeys = []string{
	defaultRulesKey,
	excludeContainersKey,
	pipelineFileKey,
	debugPipelineKey,
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
	Env               []EnvironmentEntry `json:"env"`
	Hostname          string             `json:"hostname"`
	Routes            []string           `json:"routes"`
	StripANSI         bool               `json:"strip_ansi"`
	DefaultRules      string             `json:"default_rules"`
	ExcludeContainers []string           `json:"exclude_containers"`
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
	// IngressAddr is where the Home Assistant ingress listener binds. Empty means no listener.
	IngressAddr string
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

	// The env option may also set DEFAULT_RULES, EXCLUDE_CONTAINERS, PIPELINE_FILE and DEBUG_PIPELINE, so build from the final environment.
	watcher := installPipeline(config)
	defer watcher.Stop()
	if opts.IngressAddr != "" {
		defer startIngress(opts.IngressAddr, watcher)()
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

// installPipeline builds the pipeline from the environment and the rule file,
// activates it and starts watching the file. Without rules nothing is
// installed, so the pumps keep their plain code path. A typo in the options or
// the file must never stop logging: an invalid option or an invalid file is
// logged as an error and ignored, the other settings stay active.
// The caller stops the returned watcher.
func installPipeline(config Config) *pipeline.Watcher {
	base := pipelineOptionsFromEnv(os.Getenv)
	w := &pipeline.Watcher{
		Path: valueOrDefault(os.Getenv(pipelineFileKey), pipeline.DefaultFilePath),
		Env:  fileEnv(config, base.DefaultRules),
		Base: base,
	}
	w.Reload()
	w.Start()
	return w
}

// startIngress serves the web UI in the background and returns the stop function.
// A port that cannot be bound is logged and never stops logging.
func startIngress(addr string, w *pipeline.Watcher) (stop func()) {
	srv := &ingress.Server{Watcher: w}
	go func() {
		if err := srv.ListenAndServe(addr); err != nil {
			log.Printf("ERROR: web interface (ingress) not available on %s: %v", addr, err)
		}
	}()
	return srv.Close
}

// fileEnv gives the rule file validation the names of the configured routes.
func fileEnv(config Config, defaultRules string) pipeline.FileEnv {
	env := pipeline.FileEnv{DefaultRules: defaultRules}
	seen := map[string]bool{}
	for _, uri := range config.Routes {
		name, _, err := router.RouteNameWithEnv(uri, config.lookupEnv)
		if err != nil || seen[name] {
			continue
		}
		seen[name] = true
		env.Routes = append(env.Routes, name)
	}
	// Validate already passed in LoadConfig, so this cannot fail.
	env.Ambiguous, _ = router.ValidateRouteNamesWithEnv(config.Routes, config.lookupEnv)
	return env
}

func pipelineOptionsFromEnv(getenv func(string) string) pipeline.Options {
	var opts pipeline.Options
	if v := strings.TrimSpace(getenv(defaultRulesKey)); v != "" {
		if err := validateDefaultRules(v); err != nil {
			log.Printf("ERROR: %s ignored, starting without default rules: %v", defaultRulesKey, err)
		} else {
			opts.DefaultRules = v
		}
	}
	if globs := splitGlobs(getenv(excludeContainersKey)); len(globs) > 0 {
		if err := validateExcludeContainers(globs); err != nil {
			log.Printf("ERROR: %s ignored, no containers are excluded: %v", excludeContainersKey, err)
		} else {
			opts.ExcludeContainers = globs
		}
	}
	if v := strings.TrimSpace(getenv(debugPipelineKey)); v != "" {
		if debug, err := strconv.ParseBool(v); err != nil {
			log.Printf("ERROR: %s=%q ignored, expected true or false", debugPipelineKey, v)
		} else {
			opts.Debug = debug
		}
	}
	return opts
}

// splitGlobs splits a comma separated list, trims spaces and skips empty entries.
func splitGlobs(v string) []string {
	return cleanGlobs(strings.Split(v, ","))
}

func cleanGlobs(in []string) []string {
	var out []string
	for _, g := range in {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

func validateDefaultRules(v string) error {
	_, err := pipeline.ResolveDefaults(v)
	return err
}

func validateExcludeContainers(globs []string) error {
	for _, g := range globs {
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("invalid glob %q: %w", g, err)
		}
	}
	return nil
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
	if v := strings.TrimSpace(config.DefaultRules); v != "" && v != "off" {
		env[defaultRulesKey] = v
	}
	if globs := cleanGlobs(config.ExcludeContainers); len(globs) > 0 {
		env[excludeContainersKey] = strings.Join(globs, ",")
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
