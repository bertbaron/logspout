package pipeline

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/gliderlabs/logspout/router"
)

// Compiled is an immutable, validated rule list. It is safe for concurrent use.
type Compiled struct {
	rules []compiledRule
}

type compiledRule struct {
	index  int
	name   string
	when   *compiledCond
	unless *compiledCond
	parser string
	parse  parserFunc
	set    []setAction
	drop   bool
	stop   bool
}

type setKind int

const (
	setLevel setKind = iota
	setMessage
	setField
)

type setAction struct {
	kind  setKind
	field string
	key   string // original key, for traces
	tmpl  template
}

type compiledCond struct {
	containers []string
	images     []string
	source     string
	level      *levelCond
	match      *regexp.Regexp
	groupNames []string // named groups of match, index = submatch index; nil if none
	expr       *vm.Program
}

type levelCond struct {
	op    string
	level string
}

var fieldNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// slashSwap lets `*` match across '/' in image names: both pattern and
// subject have '/' replaced before path.Match.
const slashSwap = "\x01"

// Len returns the number of rules.
func (c *Compiled) Len() int {
	if c == nil {
		return 0
	}
	return len(c.rules)
}

// Compile validates the rules and returns the executable form. All rule
// errors are reported, each prefixed with the rule index and name.
func Compile(rules RuleSet) (*Compiled, error) {
	c := &Compiled{rules: make([]compiledRule, 0, len(rules))}
	var errs []error
	for i := range rules {
		cr, err := compileRule(i, &rules[i])
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ruleLabel(i, &rules[i]), err))
			continue
		}
		c.rules = append(c.rules, cr)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// ruleLabel names a rule in errors: 1-based position, name and YAML line.
func ruleLabel(i int, r *Rule) string {
	l := fmt.Sprintf("rule %d", i+1)
	if r.Name != "" {
		l += fmt.Sprintf(" %q", r.Name)
	}
	if r.line > 0 {
		l += fmt.Sprintf(" (line %d)", r.line)
	}
	return l
}

func compileRule(index int, r *Rule) (compiledRule, error) {
	cr := compiledRule{index: index, name: r.Name, drop: r.Drop, stop: r.Stop}
	if r.Parse == "" && len(r.Set) == 0 && !r.Drop && !r.Stop {
		return cr, errors.New("rule has no action (parse, set, drop or stop)")
	}
	var err error
	if r.When != nil {
		if r.When.empty() {
			return cr, errors.New("when: empty condition (leave out `when` to match all messages)")
		}
		if cr.when, err = compileCond(r.When); err != nil {
			return cr, fmt.Errorf("when: %w", err)
		}
	}
	if r.Unless != nil {
		if r.Unless.empty() {
			return cr, errors.New("unless: empty condition would always match")
		}
		if cr.unless, err = compileCond(r.Unless); err != nil {
			return cr, fmt.Errorf("unless: %w", err)
		}
	}
	if r.Parse != "" {
		p, ok := parsers[r.Parse]
		if !ok {
			return cr, fmt.Errorf("unknown parser %q (known: %s)", r.Parse, strings.Join(ParserNames(), ", "))
		}
		cr.parser, cr.parse = r.Parse, p
	}
	var groupNames []string
	if cr.when != nil {
		groupNames = cr.when.groupNames
	}
	keys := make([]string, 0, len(r.Set))
	for k := range r.Set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a, err := compileSet(k, r.Set[k], groupNames)
		if err != nil {
			return cr, fmt.Errorf("set %s: %w", k, err)
		}
		cr.set = append(cr.set, a)
	}
	return cr, nil
}

func compileSet(key, value string, groupNames []string) (setAction, error) {
	a := setAction{key: key}
	switch {
	case key == "level":
		a.kind = setLevel
	case key == "message":
		a.kind = setMessage
	case strings.HasPrefix(key, "fields."):
		a.kind = setField
		a.field = strings.TrimPrefix(key, "fields.")
		if !fieldNameRe.MatchString(a.field) {
			return a, fmt.Errorf("invalid field name %q (allowed: A-Z a-z 0-9 _ . -)", a.field)
		}
		if a.field == "id" {
			return a, errors.New(`field name "id" is reserved`)
		}
	default:
		return a, errors.New("unknown key (allowed: level, message, fields.<name>)")
	}
	t, err := parseTemplate(value)
	if err != nil {
		return a, err
	}
	for _, p := range t.parts {
		if p.isName && !knownVar(p.name, groupNames) {
			return a, fmt.Errorf("unknown variable ${%s} (use a named group of when.match, or container, image, source, level, message)", p.name)
		}
	}
	a.tmpl = t
	if a.kind == setLevel && t.isLiteral() {
		if _, ok := router.NormalizeLevel(t.literal()); !ok {
			return a, fmt.Errorf("invalid level %q", value)
		}
	}
	return a, nil
}

func knownVar(name string, groups []string) bool {
	switch name {
	case "container", "image", "source", "level", "message":
		return true
	}
	for _, g := range groups {
		if g == name {
			return true
		}
	}
	return false
}

func compileCond(c *Condition) (*compiledCond, error) {
	cc := &compiledCond{source: c.Source}
	var err error
	if cc.containers, err = compileGlobs(c.Container, false); err != nil {
		return nil, fmt.Errorf("container: %w", err)
	}
	if cc.images, err = compileGlobs(c.Image, true); err != nil {
		return nil, fmt.Errorf("image: %w", err)
	}
	if c.Source != "" && c.Source != "stdout" && c.Source != "stderr" {
		return nil, fmt.Errorf("source: invalid value %q (stdout or stderr)", c.Source)
	}
	if c.Level != "" {
		if cc.level, err = parseLevelCond(c.Level); err != nil {
			return nil, fmt.Errorf("level: %w", err)
		}
	}
	if c.Match != "" {
		re, err := regexp.Compile(c.Match)
		if err != nil {
			return nil, fmt.Errorf("match: %w", err)
		}
		cc.match = re
		seen := map[string]bool{}
		for _, n := range re.SubexpNames() {
			if n == "" {
				continue
			}
			if seen[n] {
				return nil, fmt.Errorf("match: duplicate group name %q", n)
			}
			seen[n] = true
			cc.groupNames = re.SubexpNames()
		}
	}
	if c.Expr != "" {
		prog, err := expr.Compile(c.Expr, expr.Env(exprEnv{}), expr.AsBool())
		if err != nil {
			return nil, fmt.Errorf("expr: %w", err)
		}
		cc.expr = prog
	}
	return cc, nil
}

func compileGlobs(list StringList, swap bool) ([]string, error) {
	var out []string
	for _, g := range list {
		if g == "" {
			return nil, errors.New("value is empty")
		}
		if swap {
			g = strings.ReplaceAll(g, "/", slashSwap)
		}
		if _, err := path.Match(g, ""); err != nil {
			return nil, fmt.Errorf("invalid glob %q: %w", g, err)
		}
		out = append(out, g)
	}
	return out, nil
}

func parseLevelCond(s string) (*levelCond, error) {
	s = strings.TrimSpace(s)
	op := "="
	for _, o := range []string{"<=", ">=", "<", ">", "="} {
		if strings.HasPrefix(s, o) {
			op, s = o, strings.TrimSpace(s[len(o):])
			break
		}
	}
	l, ok := router.NormalizeLevel(s)
	if !ok {
		return nil, fmt.Errorf("unknown level %q", s)
	}
	return &levelCond{op: op, level: l}, nil
}

func (l *levelCond) matches(level string) bool {
	cmp, ok := router.CompareLevels(level, l.level)
	if !ok {
		return false
	}
	switch l.op {
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	}
	return cmp == 0
}

// template is a parsed value with ${var} references.
type template struct {
	parts []tmplPart
}

type tmplPart struct {
	lit    string
	name   string
	isName bool
}

func (t template) isLiteral() bool {
	for _, p := range t.parts {
		if p.isName {
			return false
		}
	}
	return true
}

func (t template) literal() string {
	var b strings.Builder
	for _, p := range t.parts {
		b.WriteString(p.lit)
	}
	return b.String()
}

func parseTemplate(s string) (template, error) {
	var t template
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			t.parts = append(t.parts, tmplPart{lit: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			lit.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$':
			lit.WriteByte('$')
			i++
		case '{':
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return t, fmt.Errorf("unterminated ${ in %q", s)
			}
			name := s[i+2 : i+end]
			if !varNameRe.MatchString(name) {
				return t, fmt.Errorf("invalid variable name %q in %q", name, s)
			}
			flush()
			t.parts = append(t.parts, tmplPart{name: name, isName: true})
			i += end
		default:
			lit.WriteByte('$')
		}
	}
	flush()
	return t, nil
}

var varNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
