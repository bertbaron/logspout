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
// of the same list are not evaluated and do not appear. A stop ends only the
// rest of its own list; later lists still run and add their rules. The
// top-level Level, Message, Fields and Dropped of Trace reflect the last Apply.
type RuleTrace struct {
	// List is the name of the rule list, for example "defaults/v1". Empty if unnamed.
	List    string            `json:"list,omitempty"`
	Index   int               `json:"index"`
	Name    string            `json:"name"`
	Matched bool              `json:"matched"`
	Groups  map[string]string `json:"groups,omitempty"`
	Actions []string          `json:"actions,omitempty"`
	// Changed is true when an action took effect: a parse that matched, a set
	// that was applied, or a drop. A matched gate with a failing parser is not a change.
	Changed bool `json:"changed,omitempty"`
	// Error is set when a condition failed at run time; the rule is then skipped.
	Error string `json:"error,omitempty"`
}
