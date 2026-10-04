package pipeline

import (
	"strings"

	"github.com/gliderlabs/logspout/router"
)

// RuleRef names the rule that dropped a message.
type RuleRef struct {
	List  string `json:"list,omitempty"`
	Index int    `json:"index"`
	Name  string `json:"name"`
}

// StageTrace is the outcome of the global stage or of one target.
type StageTrace struct {
	Rules          []RuleTrace       `json:"rules"`
	Level          string            `json:"level"`
	EffectiveLevel string            `json:"effective_level"`
	Message        string            `json:"message"`
	Fields         map[string]string `json:"fields,omitempty"`
	Dropped        bool              `json:"dropped"`
	// DroppedBy is the rule that dropped the message, when Dropped is set.
	DroppedBy *RuleRef `json:"dropped_by,omitempty"`
}

// GlobalTrace is the global stage: excluded containers, defaults, global rules.
type GlobalTrace struct {
	StageTrace
	// Excluded is set when exclude_containers dropped the message.
	Excluded bool `json:"excluded"`
}

// TargetTrace is one route. Sent is false when the message is dropped
// globally (then Rules is empty) or by the rules of this target.
type TargetTrace struct {
	Name string `json:"name"`
	Sent bool   `json:"sent"`
	StageTrace
}

// MessageTrace is the full journey of one message through the pipeline.
type MessageTrace struct {
	Global  GlobalTrace   `json:"global"`
	Targets []TargetTrace `json:"targets"`
	// Dropped means no target gets the message. With no routes only a global drop counts.
	Dropped bool `json:"dropped"`
}

// IsTraceLine reports whether data is one of the DEBUG_PIPELINE trace lines of logspout itself.
func IsTraceLine(data string) bool {
	return strings.Contains(data, traceMarker)
}

func stageTrace(t *Trace, dropped bool) StageTrace {
	s := StageTrace{
		Rules: t.Rules, Level: t.Level, EffectiveLevel: t.EffectiveLevel,
		Message: t.Message, Fields: t.Fields, Dropped: dropped,
	}
	if s.Rules == nil {
		s.Rules = []RuleTrace{}
	}
	if dropped && len(t.Rules) > 0 {
		// A drop ends its list and the stage, so the dropping rule is the last one traced.
		r := t.Rules[len(t.Rules)-1]
		s.DroppedBy = &RuleRef{List: r.List, Index: r.Index, Name: r.Name}
	}
	return s
}

// TraceMessage runs m through the stages of Global and Target and records
// every step, for routes (route names). It does not change m or log, so it can
// also run on a pipeline that is not installed. A nil pipeline has no rules.
func (p *Pipeline) TraceMessage(m *router.Message, routes []string) MessageTrace {
	if p == nil {
		p = &Pipeline{}
	}
	work := CloneMessage(m)

	var gt Trace
	var out MessageTrace
	switch {
	case p.exclude.Apply(work, &gt):
		out.Global.Excluded = true
		out.Global.StageTrace = stageTrace(&gt, true)
	case p.defaults.Apply(work, &gt), p.global.Apply(work, &gt):
		out.Global.StageTrace = stageTrace(&gt, true)
	default:
		out.Global.StageTrace = stageTrace(&gt, false)
	}

	out.Targets = make([]TargetTrace, 0, len(routes))
	sent := 0
	for _, name := range routes {
		tt := TargetTrace{Name: name}
		if out.Global.Dropped {
			tt.StageTrace = StageTrace{Rules: []RuleTrace{}}
			out.Targets = append(out.Targets, tt)
			continue
		}
		var t Trace
		dropped := p.targets[name].Apply(CloneMessage(work), &t)
		tt.StageTrace = stageTrace(&t, dropped)
		tt.Sent = !dropped
		if tt.Sent {
			sent++
		}
		out.Targets = append(out.Targets, tt)
	}
	out.Dropped = out.Global.Dropped || (len(routes) > 0 && sent == 0)
	return out
}
