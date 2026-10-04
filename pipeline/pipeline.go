package pipeline

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gliderlabs/logspout/router"
)

// Options select what Build compiles.
type Options struct {
	// DefaultRules is the `default_rules` selector: "", "off", "latest" or "vN".
	DefaultRules string
	// DisabledDefaults names shipped rules to switch off.
	DisabledDefaults []string
	// ExcludeContainers are container name globs that are dropped for all targets.
	ExcludeContainers []string
	// Rules are the user's global rules.
	Rules RuleSet
	// Targets are the rules per route name.
	Targets map[string]RuleSet
	// Debug logs a trace line per message and stage.
	Debug bool
}

// Pipeline is an immutable compiled pipeline. It implements router.Processor
// and is safe for concurrent use.
type Pipeline struct {
	version  string // resolved default set, "" when off
	defaults *Compiled
	exclude  *Compiled
	global   *Compiled
	targets  map[string]*Compiled

	excluded []string
	debug    bool
	limiter  *traceLimiter
}

var _ router.Processor = (*Pipeline)(nil)

// Build compiles the options. unknownDisabled lists names in
// DisabledDefaults that are not in the selected set; they are not applied.
func Build(o Options) (p *Pipeline, unknownDisabled []string, err error) {
	p = &Pipeline{excluded: cleanExcludes(o.ExcludeContainers), debug: o.Debug}
	if p.debug {
		p.limiter = &traceLimiter{now: time.Now}
	}
	if p.version, err = ResolveDefaults(o.DefaultRules); err != nil {
		return nil, nil, err
	}
	if p.version != "" {
		if p.defaults, unknownDisabled, err = CompileDefaults(p.version, o.DisabledDefaults); err != nil {
			return nil, nil, err
		}
	}
	if len(p.excluded) > 0 {
		rule := Rule{
			Name: "exclude_containers",
			When: &Condition{Container: StringList(p.excluded)},
			Drop: true,
		}
		c, err := Compile(RuleSet{rule})
		if err != nil {
			return nil, nil, fmt.Errorf("exclude_containers: %w", err)
		}
		p.exclude = c.WithName("exclude_containers")
	}
	if len(o.Rules) > 0 {
		c, err := Compile(o.Rules)
		if err != nil {
			return nil, nil, fmt.Errorf("rules: %w", err)
		}
		p.global = c.WithName("rules")
	}
	for name, rs := range o.Targets {
		if len(rs) == 0 {
			continue
		}
		c, err := Compile(rs)
		if err != nil {
			return nil, nil, fmt.Errorf("targets.%s: %w", name, err)
		}
		if p.targets == nil {
			p.targets = make(map[string]*Compiled)
		}
		p.targets[name] = c.WithName("targets/" + name)
	}
	return p, unknownDisabled, nil
}

// cleanExcludes strips the leading '/' that `docker ps` style names can have
// and skips entries that are empty after that.
func cleanExcludes(in []string) []string {
	var out []string
	for _, g := range in {
		if g = strings.TrimPrefix(g, "/"); g != "" {
			out = append(out, g)
		}
	}
	return out
}

// Empty reports whether the pipeline has no rules at all.
func (p *Pipeline) Empty() bool {
	return p == nil || (p.defaults == nil && p.exclude == nil && p.global == nil && len(p.targets) == 0)
}

// Global runs the excluded containers, the default rules and the global rules.
// Excluded containers go first: the result is the same and it saves the
// classification. The default list is separate from the user rules: a default
// `stop` must not skip them.
func (p *Pipeline) Global(m *router.Message) (dropped bool) {
	if p.debug {
		return p.globalDebug(m)
	}
	return p.exclude.Apply(m, nil) || p.defaults.Apply(m, nil) || p.global.Apply(m, nil)
}

// Target runs the rules of one route on a copy of m. A route without rules
// gets m itself, without a copy.
func (p *Pipeline) Target(routeName string, m *router.Message) (*router.Message, bool) {
	if p.debug {
		return p.targetDebug(routeName, m)
	}
	c := p.targets[routeName]
	if c == nil {
		return m, false
	}
	out := CloneMessage(m)
	if c.Apply(out, nil) {
		return nil, true
	}
	return out, false
}

// Summary is the one-line description that is logged on load. The rule
// counts do not include the exclude_containers rule, which is listed apart.
func (p *Pipeline) Summary() string {
	defaults := "off"
	if p.version != "" {
		defaults = fmt.Sprintf("%s (%d rules)", p.version, p.defaults.Len())
	}
	fileRules := p.global.Len()
	var targets []string
	for name, c := range p.targets {
		fileRules += c.Len()
		targets = append(targets, fmt.Sprintf("%s(%d)", name, c.Len()))
	}
	sort.Strings(targets)
	list := func(s []string) string {
		if len(s) == 0 {
			return "none"
		}
		return strings.Join(s, ",")
	}
	return fmt.Sprintf("pipeline: default rules %s, %d user rules (%d global), excluded containers: %s, targets with rules: %s",
		defaults, fileRules, p.global.Len(), list(p.excluded), list(targets))
}
