package pipeline

import (
	"path"
	"regexp"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/gliderlabs/logspout/router"
)

// exprEnv is the environment of `expr` conditions.
type exprEnv struct {
	Container string            `expr:"container"`
	Image     string            `expr:"image"`
	Source    string            `expr:"source"`
	Level     string            `expr:"level"`
	Message   string            `expr:"message"`
	Fields    map[string]string `expr:"fields"`
}

// emptyFields is shared and must never be written to.
var emptyFields = map[string]string{}

// CSI sequences, OSC sequences (BEL or ESC \ terminated) and charset
// selection such as ESC ( B.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][0-9A-Za-z]`)

func stripANSI(s string) string {
	if strings.IndexByte(s, 0x1b) < 0 {
		return s
	}
	return ansiRe.ReplaceAllString(s, "")
}

// CloneMessage returns a copy of m with its own Fields map. The Container
// pointer is shared; it is treated as read-only.
func CloneMessage(m *router.Message) *router.Message {
	if m == nil {
		return nil
	}
	c := *m
	if m.Fields != nil {
		c.Fields = make(map[string]string, len(m.Fields))
		for k, v := range m.Fields {
			c.Fields[k] = v
		}
	}
	return &c
}

// state holds per-message values that are computed at most once.
type state struct {
	m         *router.Message
	src, text string // cache of the ANSI-stripped message for src
	cached    bool

	swapped     string // image with '/' replaced, for glob matching
	swappedDone bool
}

func (s *state) imageSwapped() string {
	if !s.swappedDone {
		s.swapped, s.swappedDone = strings.ReplaceAll(s.image(), "/", slashSwap), true
	}
	return s.swapped
}

// message returns the ANSI-stripped message text.
func (s *state) message() string {
	if !s.cached || s.src != s.m.Data {
		s.src, s.text, s.cached = s.m.Data, stripANSI(s.m.Data), true
	}
	return s.text
}

func (s *state) container() string {
	if s.m.Container == nil {
		return ""
	}
	return strings.TrimPrefix(s.m.Container.Name, "/")
}

func (s *state) image() string {
	if s.m.Container == nil || s.m.Container.Config == nil {
		return ""
	}
	return s.m.Container.Config.Image
}

// level is the message level, or the one the adapters derive from the source.
func (s *state) level() string {
	if s.m.Level != "" {
		return s.m.Level
	}
	if s.m.Source == "stderr" {
		return router.LevelError
	}
	return router.LevelInfo
}

func globsMatch(globs []string, name string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, name); ok {
			return true
		}
	}
	return false
}

// eval reports whether all conditions match. Cheap conditions run before the
// regex and the expression. groups is only returned for named-group regexes.
func (c *compiledCond) eval(s *state) (ok bool, groups []string, err error) {
	if len(c.containers) > 0 && !globsMatch(c.containers, s.container()) {
		return false, nil, nil
	}
	if len(c.images) > 0 && !globsMatch(c.images, s.imageSwapped()) {
		return false, nil, nil
	}
	if c.source != "" && s.m.Source != c.source {
		return false, nil, nil
	}
	if c.level != nil && !c.level.matches(s.level()) {
		return false, nil, nil
	}
	if c.match != nil {
		if c.groupNames != nil {
			if groups = c.match.FindStringSubmatch(s.message()); groups == nil {
				return false, nil, nil
			}
		} else if !c.match.MatchString(s.message()) {
			return false, nil, nil
		}
	}
	if c.expr != nil {
		fields := s.m.Fields
		if fields == nil {
			fields = emptyFields
		}
		out, err := expr.Run(c.expr, exprEnv{
			Container: s.container(),
			Image:     s.image(),
			Source:    s.m.Source,
			Level:     s.level(),
			Message:   s.message(),
			Fields:    fields,
		})
		if err != nil {
			return false, nil, err
		}
		if b, _ := out.(bool); !b {
			return false, nil, nil
		}
	}
	return true, groups, nil
}

// vars resolves ${var} references.
type vars struct {
	s      *state
	names  []string
	groups []string
}

func (v *vars) lookup(name string) string {
	for i, n := range v.names {
		if n == name && i < len(v.groups) {
			return v.groups[i]
		}
	}
	switch name {
	case "container":
		return v.s.container()
	case "image":
		return v.s.image()
	case "source":
		return v.s.m.Source
	case "level":
		return v.s.level()
	case "message":
		return v.s.message()
	}
	return ""
}

func (t template) expand(v *vars) string {
	if len(t.parts) == 1 && !t.parts[0].isName {
		return t.parts[0].lit
	}
	var b strings.Builder
	for _, p := range t.parts {
		if p.isName {
			b.WriteString(v.lookup(p.name))
		} else {
			b.WriteString(p.lit)
		}
	}
	return b.String()
}

// Apply runs the rules on m and mutates it in place; callers that must not
// affect other consumers pass a CloneMessage copy. It reports whether the
// message is dropped. trace may be nil.
func (c *Compiled) Apply(m *router.Message, trace *Trace) (dropped bool) {
	if c != nil {
		s := state{m: m}
		for i := range c.rules {
			r := &c.rules[i]
			var rt *RuleTrace
			if trace != nil {
				trace.Rules = append(trace.Rules, RuleTrace{List: c.name, Index: r.index, Name: r.name})
				rt = &trace.Rules[len(trace.Rules)-1]
			}
			var groups []string
			if r.when != nil {
				ok, g, err := r.when.eval(&s)
				if err != nil {
					if rt != nil {
						rt.Error = "when: " + err.Error()
					}
					continue
				}
				if !ok {
					continue
				}
				groups = g
			}
			if r.unless != nil {
				ok, _, err := r.unless.eval(&s)
				if err != nil {
					if rt != nil {
						rt.Error = "unless: " + err.Error()
					}
					continue
				}
				if ok {
					continue
				}
			}
			if rt != nil {
				rt.Matched = true
				if groups != nil {
					rt.Groups = groupMap(r.when.groupNames, groups)
				}
			}
			var names []string
			if r.when != nil {
				names = r.when.groupNames
			}
			r.run(&s, &vars{s: &s, names: names, groups: groups}, rt)
			if r.drop {
				dropped = true
				break
			}
			if r.stop {
				break
			}
		}
	}
	if trace != nil {
		trace.Level, trace.Message, trace.Dropped = m.Level, m.Data, dropped
		trace.EffectiveLevel = (&state{m: m}).level()
		trace.Fields = nil
		if m.Fields != nil {
			trace.Fields = make(map[string]string, len(m.Fields))
			for k, v := range m.Fields {
				trace.Fields[k] = v
			}
		}
	}
	return dropped
}

func groupMap(names, groups []string) map[string]string {
	out := make(map[string]string)
	for i, n := range names {
		if n != "" && i < len(groups) {
			out[n] = groups[i]
		}
	}
	return out
}

func (r *compiledRule) run(s *state, v *vars, rt *RuleTrace) {
	note := func(a string) {
		if rt != nil {
			rt.Actions = append(rt.Actions, a)
		}
	}
	m := s.m
	if r.parse != nil {
		if res, ok := r.parse(s.message()); ok {
			m.Level = res.level
			for _, kv := range res.fields {
				setMessageField(m, kv[0], kv[1])
			}
			note("parse " + r.parser + " level=" + res.level)
		} else {
			note("parse " + r.parser + " no match")
		}
	}
	// Resolve all values first so that set actions do not see each other.
	if len(r.set) > 0 {
		vals := make([]string, len(r.set))
		for i := range r.set {
			vals[i] = r.set[i].tmpl.expand(v)
		}
		for i, a := range r.set {
			switch a.kind {
			case setLevel:
				if l, ok := router.NormalizeLevel(vals[i]); ok {
					m.Level = l
					note("set level=" + l)
				} else {
					note("set level ignored, unknown value " + quote(vals[i]))
				}
			case setMessage:
				m.Data = vals[i]
				note("set message")
			case setField:
				setMessageField(m, a.field, vals[i])
				note("set " + a.key)
			}
		}
	}
	if r.drop {
		note("drop")
	}
	if r.stop {
		note("stop")
	}
}

func setMessageField(m *router.Message, k, v string) {
	if m.Fields == nil {
		m.Fields = make(map[string]string)
	}
	m.Fields[k] = v
}

func quote(s string) string {
	return `"` + s + `"`
}
