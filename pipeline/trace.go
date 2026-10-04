package pipeline

// Trace records what happened to one message. It is only filled when a
// non-nil Trace is passed to Apply. When Apply is called for several rule
// lists in sequence with the same Trace, the rule traces are appended.
type Trace struct {
	Rules []RuleTrace `json:"rules"`
	Level string      `json:"level"`
	// EffectiveLevel is Level, or the level derived from the source when Level is empty.
	EffectiveLevel string            `json:"effective_level"`
	Message        string            `json:"message"`
	Fields         map[string]string `json:"fields,omitempty"`
	Dropped        bool              `json:"dropped"`
}

// RuleTrace is the outcome of one evaluated rule. Rules after a drop or stop
// are not evaluated and do not appear.
type RuleTrace struct {
	Index   int               `json:"index"`
	Name    string            `json:"name"`
	Matched bool              `json:"matched"`
	Groups  map[string]string `json:"groups,omitempty"`
	Actions []string          `json:"actions,omitempty"`
	// Error is set when a condition failed at run time; the rule is then skipped.
	Error string `json:"error,omitempty"`
}
