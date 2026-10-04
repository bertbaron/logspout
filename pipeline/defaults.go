package pipeline

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Shipped default rule sets. v1, v2, ... are frozen once released.
//
//go:embed defaults/v*.yaml
var defaultsFS embed.FS

var defaultVersionRe = regexp.MustCompile(`^v([0-9]+)$`)

// DefaultVersions returns the embedded rule set versions, oldest first.
func DefaultVersions() []string {
	entries, err := defaultsFS.ReadDir("defaults")
	if err != nil {
		return nil
	}
	var versions []string
	for _, e := range entries {
		if v, ok := strings.CutSuffix(e.Name(), ".yaml"); ok && defaultVersionRe.MatchString(v) {
			versions = append(versions, v)
		}
	}
	sort.Slice(versions, func(i, j int) bool { return versionNumber(versions[i]) < versionNumber(versions[j]) })
	return versions
}

func versionNumber(v string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(v, "v"))
	return n
}

// LatestDefaults returns the newest embedded version, or "" if there is none.
func LatestDefaults() string {
	v := DefaultVersions()
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}

// ResolveDefaults maps the `default_rules` selector to a version. "off" and
// "" give "" (no defaults), "latest" the newest version, "vN" that version.
func ResolveDefaults(selector string) (string, error) {
	switch selector {
	case "", "off":
		return "", nil
	case "latest":
		if v := LatestDefaults(); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("no default rule sets available")
	}
	for _, v := range DefaultVersions() {
		if v == selector {
			return v, nil
		}
	}
	return "", fmt.Errorf("unknown default rule set %q (use off, latest or one of: %s)", selector, strings.Join(DefaultVersions(), ", "))
}

// defaultsFile is the layout of a rule file: a mapping with a rules list.
type defaultsFile struct {
	Rules RuleSet `yaml:"rules"`
}

func parseDefaults(data []byte) (RuleSet, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f defaultsFile
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("line %d: more than one YAML document (`---`)", extra.Line)
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	return f.Rules, nil
}

// selectDefaults loads a set and removes the disabled rules. Names that are
// not in the set are returned, not applied.
func selectDefaults(version string, disabled []string) (rules RuleSet, unknown []string, err error) {
	if !defaultVersionRe.MatchString(version) {
		return nil, nil, fmt.Errorf("unknown default rule set %q", version)
	}
	data, err := defaultsFS.ReadFile("defaults/" + version + ".yaml")
	if err != nil {
		return nil, nil, fmt.Errorf("unknown default rule set %q", version)
	}
	all, err := parseDefaults(data)
	if err != nil {
		return nil, nil, fmt.Errorf("default rule set %s: %w", version, err)
	}
	if len(disabled) == 0 {
		return all, nil, nil
	}
	off := make(map[string]bool, len(disabled))
	for _, n := range disabled {
		off[n] = true
	}
	rules = make(RuleSet, 0, len(all))
	for _, r := range all {
		if off[r.Name] {
			delete(off, r.Name)
			continue
		}
		rules = append(rules, r)
	}
	for n := range off {
		unknown = append(unknown, n)
	}
	sort.Strings(unknown)
	return rules, unknown, nil
}

// DefaultRules returns the rules of a default set without the rules named in
// disabled. A name that is not in the set is an error.
//
// Default rules use `stop`. Apply them as their own Compiled list (see
// CompileDefaults) before the user's rules, never in one list with them, or a
// default `stop` would skip the user's rules.
func DefaultRules(version string, disabled []string) (RuleSet, error) {
	rules, unknown, err := selectDefaults(version, disabled)
	if err != nil {
		return nil, err
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("disable_defaults: no rule named %s in default set %s", strings.Join(unknown, ", "), version)
	}
	return rules, nil
}

// CompileDefaults compiles a default set as its own list, named
// "defaults/<version>" in traces. Apply it before the user's rules. Unknown
// names in disabled do not fail: they are returned in unknownDisabled (for a
// warning) and the other rules still compile.
func CompileDefaults(version string, disabled []string) (c *Compiled, unknownDisabled []string, err error) {
	rules, unknown, err := selectDefaults(version, disabled)
	if err != nil {
		return nil, nil, err
	}
	c, err = Compile(rules)
	if err != nil {
		return nil, unknown, fmt.Errorf("default rule set %s: %w", version, err)
	}
	return c.WithName("defaults/" + version), unknown, nil
}
