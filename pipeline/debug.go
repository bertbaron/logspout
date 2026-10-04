package pipeline

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gliderlabs/logspout/router"
)

// The debug paths allocate a Trace per message; they only run with Options.Debug.

// traceMarker starts every trace line. Logspout's own log is shipped like any
// other container log, so a trace line comes back as a message. Such a message
// is processed but never traced, or every line would cause new lines.
const traceMarker = "pipeline trace: "

const maxTraceLinesPerSecond = 50

// traceLimiter caps the trace output. Other container logs can still be busy.
type traceLimiter struct {
	mu         sync.Mutex
	now        func() time.Time
	windowEnd  time.Time
	count      int
	suppressed int
}

// allow reports whether a trace line may be written. When a window with
// suppressed lines ends, it logs one line about them.
func (l *traceLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.After(l.windowEnd) {
		if l.suppressed > 0 {
			log.Printf("%ssuppressed %d lines", traceMarker, l.suppressed)
		}
		l.windowEnd, l.count, l.suppressed = now.Add(time.Second), 0, 0
	}
	if l.count >= maxTraceLinesPerSecond {
		l.suppressed++
		return false
	}
	l.count++
	return true
}

func (p *Pipeline) tracef(format string, args ...any) {
	if p.limiter.allow() {
		log.Printf(traceMarker+format, args...)
	}
}

func (p *Pipeline) globalDebug(m *router.Message) bool {
	if strings.Contains(m.Data, traceMarker) {
		return p.exclude.Apply(m, nil) || p.defaults.Apply(m, nil) || p.global.Apply(m, nil)
	}
	var t Trace
	dropped := p.exclude.Apply(m, &t) || p.defaults.Apply(m, &t) || p.global.Apply(m, &t)
	p.tracef("global container=%s source=%s %s level=%s dropped=%t",
		containerName(m), m.Source, formatRules(t.Rules), (&state{m: m}).level(), dropped)
	return dropped
}

func (p *Pipeline) targetDebug(routeName string, m *router.Message) (*router.Message, bool) {
	c := p.targets[routeName]
	quiet := strings.Contains(m.Data, traceMarker)
	if c == nil {
		if !quiet {
			p.tracef("target=%s container=%s no rules sent", routeName, containerName(m))
		}
		return m, false
	}
	var t Trace
	out := CloneMessage(m)
	dropped := c.Apply(out, &t)
	result := "sent"
	if dropped {
		result = "dropped"
	}
	if !quiet {
		p.tracef("target=%s container=%s %s level=%s %s",
			routeName, containerName(m), formatRules(t.Rules), t.EffectiveLevel, result)
	}
	if dropped {
		return nil, true
	}
	return out, false
}

func containerName(m *router.Message) string {
	if m.Container == nil {
		return ""
	}
	return strings.TrimPrefix(m.Container.Name, "/")
}

// formatRules lists the lists that ran and the rules that matched or failed.
func formatRules(rules []RuleTrace) string {
	var lists, matched []string
	for _, r := range rules {
		if len(lists) == 0 || lists[len(lists)-1] != r.List {
			lists = append(lists, r.List)
		}
		if !r.Matched && r.Error == "" {
			continue
		}
		s := fmt.Sprintf("%s:%q", r.List, r.Name)
		if len(r.Actions) > 0 {
			s += "[" + strings.Join(r.Actions, ",") + "]"
		}
		if r.Error != "" {
			s += " error=" + r.Error
		}
		matched = append(matched, s)
	}
	return fmt.Sprintf("lists=[%s] matched=[%s]", strings.Join(lists, ","), strings.Join(matched, "; "))
}
