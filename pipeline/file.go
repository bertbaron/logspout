package pipeline

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

// DefaultFilePath is where the rule file lives in the add-on container.
const DefaultFilePath = "/config/logspout.yaml"

// Issue is one problem in the rule file. Line and Column are 1-based, 0 when unknown.
type Issue struct {
	Line, Column int
	Message      string
}

// String formats an issue as "line:col message".
func (i Issue) String() string {
	switch {
	case i.Line == 0:
		return i.Message
	case i.Column == 0:
		return fmt.Sprintf("line %d: %s", i.Line, i.Message)
	}
	return fmt.Sprintf("%d:%d %s", i.Line, i.Column, i.Message)
}

// FileConfig is the parsed and validated rule file.
type FileConfig struct {
	// Defaults overrides the add-on option `default_rules` when DefaultsSet.
	Defaults        string
	DefaultsSet     bool
	DisableDefaults []string
	Rules           RuleSet
	Targets         map[string]RuleSet
}

// Apply returns base with the file settings on top: `defaults` in the file
// wins over the add-on option, the rules are added.
func (f *FileConfig) Apply(base Options) Options {
	if f == nil {
		return base
	}
	if f.DefaultsSet {
		base.DefaultRules = f.Defaults
	}
	base.DisabledDefaults = f.DisableDefaults
	base.Rules = f.Rules
	base.Targets = f.Targets
	return base
}

// FileEnv is what a rule file is validated against.
type FileEnv struct {
	// Routes are the route names. Ambiguous lists default names that more
	// than one route has; rules cannot target them.
	Routes    []string
	Ambiguous []string
	// DefaultRules is the add-on option, used when the file has no `defaults`.
	DefaultRules string
}

// FileResult is the outcome of loading a rule file. Config is nil when there
// are errors. Warnings do not make the file invalid.
type FileResult struct {
	Config   *FileConfig
	Errors   []Issue
	Warnings []Issue
}

// Err returns all errors as one error, nil when the file is valid.
func (r *FileResult) Err() error {
	if len(r.Errors) == 0 {
		return nil
	}
	lines := make([]string, len(r.Errors))
	for i, e := range r.Errors {
		lines[i] = e.String()
	}
	return fmt.Errorf("%d problem(s) in the rule file:\n  %s", len(lines), strings.Join(lines, "\n  "))
}

// LoadFile reads and validates the rule file. A missing file is valid and
// has no rules.
func LoadFile(path string, env FileEnv) *FileResult {
	data, err := readLimited(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &FileResult{Config: &FileConfig{}}
	}
	if err != nil {
		return &FileResult{Errors: []Issue{{Message: err.Error()}}}
	}
	return ParseFile(data, env)
}

// MaxFileSize is the largest rule file that is loaded.
const MaxFileSize = 1 << 20

func readLimited(path string) ([]byte, error) {
	// O_NONBLOCK: opening a FIFO for reading would wait for a writer.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// A FIFO or device would block the read, and the start-up with it.
	if info, err := f.Stat(); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("file too large (more than %d bytes)", MaxFileSize)
	}
	return data, nil
}

var yamlLineRe = regexp.MustCompile(`^(?:yaml: )?line (\d+)(?:, column (\d+))?: (.*)$`)

// issuesFromError converts a yaml.v3 or rule decoding error. at is used when
// the error has no position.
func issuesFromError(err error, at *yaml.Node) []Issue {
	var msgs []string
	if te, ok := err.(*yaml.TypeError); ok {
		msgs = te.Errors
	} else {
		msgs = []string{err.Error()}
	}
	var out []Issue
	for _, m := range msgs {
		if sub := yamlLineRe.FindStringSubmatch(m); sub != nil {
			is := Issue{Message: sub[3]}
			is.Line, _ = strconv.Atoi(sub[1])
			is.Column, _ = strconv.Atoi(sub[2])
			if is.Column == 0 {
				is.Column = columnAtLine(at, is.Line)
			}
			out = append(out, is)
			continue
		}
		out = append(out, Issue{Line: at.Line, Column: at.Column, Message: strings.TrimPrefix(m, "yaml: ")})
	}
	return out
}

// columnAtLine finds the column of the first value node on a line, for errors
// that yaml.v3 reports with a line only. It returns 0 when there is none.
func columnAtLine(n *yaml.Node, line int) int {
	if n.Line == line && n.Kind != yaml.MappingNode && n.Kind != yaml.DocumentNode {
		return n.Column
	}
	for i, c := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 {
			continue // keys
		}
		if col := columnAtLine(c, line); col != 0 {
			return col
		}
	}
	return 0
}

type fileParser struct {
	env      FileEnv
	cfg      FileConfig
	errs     []Issue
	warnings []Issue
}

func (p *fileParser) errorAt(n *yaml.Node, format string, args ...any) {
	p.errs = append(p.errs, Issue{Line: n.Line, Column: n.Column, Message: fmt.Sprintf(format, args...)})
}

// ParseFile validates rule file content against the route names. It reports
// all problems at once and does not touch the disk.
func ParseFile(data []byte, env FileEnv) *FileResult {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return &FileResult{Config: &FileConfig{}}
		}
		return &FileResult{Errors: issuesFromError(err, &yaml.Node{Line: 1, Column: 1})}
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return &FileResult{Errors: []Issue{{Line: extra.Line, Column: extra.Column, Message: "more than one YAML document (`---`)"}}}
	} else if !errors.Is(err, io.EOF) {
		return &FileResult{Errors: issuesFromError(err, &yaml.Node{Line: 1, Column: 1})}
	}

	root := resolveAlias(doc.Content[0])
	p := &fileParser{env: env}
	switch {
	case root.Tag == "!!null":
		return &FileResult{Config: &FileConfig{}}
	case root.Kind != yaml.MappingNode:
		p.errorAt(root, "the file must be a mapping with the keys defaults, disable_defaults, rules and targets")
	default:
		p.parseRoot(root)
	}
	res := &FileResult{Errors: p.errs, Warnings: p.warnings}
	sortIssues(res.Errors)
	sortIssues(res.Warnings)
	if len(res.Errors) == 0 {
		res.Config = &p.cfg
	}
	return res
}

func sortIssues(l []Issue) {
	sort.SliceStable(l, func(i, j int) bool {
		if l[i].Line != l[j].Line {
			return l[i].Line < l[j].Line
		}
		return l[i].Column < l[j].Column
	})
}

func (p *fileParser) parseRoot(root *yaml.Node) {
	seen := map[string]bool{}
	var disableNode *yaml.Node
	var disableNames []*yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], resolveAlias(root.Content[i+1])
		if seen[k.Value] {
			p.errorAt(k, "duplicate key %q", k.Value)
			continue
		}
		seen[k.Value] = true
		switch k.Value {
		case "defaults":
			p.parseDefaults(v)
		case "disable_defaults":
			disableNode = k
			disableNames = p.parseNames(k, v)
		case "rules":
			p.cfg.Rules = p.parseRules(v)
		case "targets":
			p.parseTargets(k, v)
		default:
			p.errorAt(k, "unknown key %q (use defaults, disable_defaults, rules or targets)", k.Value)
		}
	}
	p.checkDisabled(disableNode, disableNames)
}

func (p *fileParser) parseDefaults(v *yaml.Node) {
	if v.Kind != yaml.ScalarNode || v.Tag == "!!null" {
		p.errorAt(v, "defaults: must be off, latest or a version such as v1")
		return
	}
	if v.Value == "" {
		p.errorAt(v, "defaults: empty value (use off to disable the default rules)")
		return
	}
	if _, err := ResolveDefaults(v.Value); err != nil {
		p.errorAt(v, "defaults: %v", err)
		return
	}
	p.cfg.Defaults, p.cfg.DefaultsSet = v.Value, true
}

func (p *fileParser) parseNames(k, v *yaml.Node) []*yaml.Node {
	if v.Tag == "!!null" {
		return nil
	}
	if v.Kind != yaml.SequenceNode {
		p.errorAt(v, "disable_defaults: must be a list of rule names")
		return nil
	}
	var names []*yaml.Node
	for _, item := range v.Content {
		item = resolveAlias(item)
		if item.Kind != yaml.ScalarNode || item.Value == "" {
			p.errorAt(item, "disable_defaults: entries must be rule names")
			continue
		}
		names = append(names, item)
		p.cfg.DisableDefaults = append(p.cfg.DisableDefaults, item.Value)
	}
	return names
}

// checkDisabled warns about names that are not in the active default set, so
// that a `latest` upgrade that removes a rule does not break the file.
func (p *fileParser) checkDisabled(key *yaml.Node, names []*yaml.Node) {
	if len(names) == 0 {
		return
	}
	selector := p.env.DefaultRules
	if p.cfg.DefaultsSet {
		selector = p.cfg.Defaults
	}
	version, err := ResolveDefaults(selector)
	if err != nil {
		return
	}
	if version == "" {
		p.warnings = append(p.warnings, Issue{Line: key.Line, Column: key.Column, Message: "disable_defaults has no effect: default rules are off"})
		return
	}
	_, unknown, err := selectDefaults(version, p.cfg.DisableDefaults)
	if err != nil {
		return
	}
	isUnknown := map[string]bool{}
	for _, n := range unknown {
		isUnknown[n] = true
	}
	for _, n := range names {
		if isUnknown[n.Value] {
			p.warnings = append(p.warnings, Issue{Line: n.Line, Column: n.Column,
				Message: fmt.Sprintf("disable_defaults: no rule named %q in default set %s, ignored", n.Value, version)})
		}
	}
}

func (p *fileParser) parseTargets(k, v *yaml.Node) {
	if v.Tag == "!!null" {
		return
	}
	if v.Kind != yaml.MappingNode {
		p.errorAt(v, "targets: must be a mapping of route name to {rules: [...]}")
		return
	}
	known := map[string]bool{}
	for _, n := range p.env.Routes {
		known[n] = true
	}
	ambiguous := map[string]bool{}
	for _, n := range p.env.Ambiguous {
		ambiguous[n] = true
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(v.Content); i += 2 {
		name, tv := v.Content[i], resolveAlias(v.Content[i+1])
		if seen[name.Value] {
			p.errorAt(name, "duplicate target %q", name.Value)
			continue
		}
		seen[name.Value] = true
		switch {
		case ambiguous[name.Value]:
			p.errorAt(name, "ambiguous target name %q: more than one route has this name, add #name to the route URIs", name.Value)
		case !known[name.Value]:
			p.errorAt(name, "unknown target %q (routes: %s)", name.Value, strings.Join(p.env.Routes, ", "))
		}
		if tv.Tag == "!!null" {
			continue // `gelf:` without value is the same as no rules
		}
		if tv.Kind != yaml.MappingNode {
			p.errorAt(tv, "target %s: must be a mapping with `rules`", name.Value)
			continue
		}
		var rules RuleSet
		hasRules := false
		for j := 0; j+1 < len(tv.Content); j += 2 {
			tk := tv.Content[j]
			if tk.Value != "rules" {
				p.errorAt(tk, "unknown target key %q (use rules)", tk.Value)
				continue
			}
			if hasRules {
				p.errorAt(tk, "duplicate key %q", tk.Value)
				continue
			}
			hasRules = true
			rules = p.parseRules(resolveAlias(tv.Content[j+1]))
		}
		if p.cfg.Targets == nil {
			p.cfg.Targets = map[string]RuleSet{}
		}
		p.cfg.Targets[name.Value] = rules
	}
}

// parseRules decodes and compiles every rule on its own, so that all
// problems are reported.
func (p *fileParser) parseRules(v *yaml.Node) RuleSet {
	if v.Tag == "!!null" {
		return nil
	}
	if v.Kind != yaml.SequenceNode {
		p.errorAt(v, "rules: must be a list")
		return nil
	}
	var rules RuleSet
	for _, item := range v.Content {
		item = resolveAlias(item)
		if item.Tag == "!!null" {
			p.errorAt(item, "empty rule")
			continue
		}
		var r Rule
		if err := item.Decode(&r); err != nil {
			p.errs = append(p.errs, issuesFromError(err, item)...)
			continue
		}
		if _, err := compileRule(len(rules), &r); err != nil {
			p.errs = append(p.errs, ruleIssue(&r, item, err))
			continue
		}
		rules = append(rules, r)
	}
	return rules
}

// ruleIssue points at the key that caused a compile error when it can tell,
// otherwise at the rule.
func ruleIssue(r *Rule, node *yaml.Node, err error) Issue {
	msg := err.Error()
	key := ""
	switch {
	case strings.HasPrefix(msg, "when:"):
		key = "when"
	case strings.HasPrefix(msg, "unless:"):
		key = "unless"
	case strings.HasPrefix(msg, "set "):
		key = "set"
	case strings.HasPrefix(msg, "unknown parser"):
		key = "parse"
	}
	is := Issue{Line: r.line, Column: r.col}
	for i := 0; key != "" && i+1 < len(node.Content); i += 2 {
		if k := node.Content[i]; k.Value == key {
			is.Line, is.Column = k.Line, k.Column
		}
	}
	if r.Name != "" {
		msg = fmt.Sprintf("rule %q: %s", r.Name, msg)
	} else {
		msg = "rule: " + msg
	}
	is.Message = msg
	return is
}

// ReadFile reads a rule file with the same checks as LoadFile: regular file, at most MaxFileSize.
func ReadFile(path string) ([]byte, error) { return readLimited(path) }
