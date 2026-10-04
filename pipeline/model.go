// Package pipeline implements the rule engine that classifies, rewrites and
// drops log messages. See logspout/docs/design-pipeline.md.
package pipeline

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// RuleSet is an ordered list of rules.
type RuleSet []Rule

// Rule is one entry of a rule list. Actions run in the order parse, set, drop, stop.
type Rule struct {
	Name   string            `yaml:"name"`
	When   *Condition        `yaml:"when"`
	Unless *Condition        `yaml:"unless"`
	Parse  string            `yaml:"parse"`
	Set    map[string]string `yaml:"set"`
	Drop   bool              `yaml:"drop"`
	Stop   bool              `yaml:"stop"`

	line int // YAML line of the rule, 0 if not decoded from YAML
}

var ruleKeys = map[string]bool{
	"name": true, "when": true, "unless": true, "parse": true, "set": true, "drop": true, "stop": true,
}

// UnmarshalYAML rejects unknown keys, null `when`/`unless` (which would match
// every message) and null `set` values. It checks these itself because
// Node.Decode does not inherit KnownFields and skips the decoder for null.
func (r *Rule) UnmarshalYAML(n *yaml.Node) error {
	n = resolveAlias(n)
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: rule must be a mapping", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], resolveAlias(n.Content[i+1])
		if !ruleKeys[k.Value] {
			return fmt.Errorf("line %d, column %d: unknown rule key %q", k.Line, k.Column, k.Value)
		}
		switch k.Value {
		case "when", "unless":
			if v.Tag == "!!null" {
				return fmt.Errorf("line %d, column %d: %s: empty condition (leave out `%s` to match all messages)", k.Line, k.Column, k.Value, k.Value)
			}
		case "set":
			if v.Kind != yaml.MappingNode {
				break
			}
			for j := 0; j+1 < len(v.Content); j += 2 {
				if resolveAlias(v.Content[j+1]).Tag == "!!null" {
					return fmt.Errorf("line %d, column %d: set %s: value is null (use '' for an empty value)", v.Content[j].Line, v.Content[j].Column, v.Content[j].Value)
				}
			}
		}
	}
	type plain Rule
	if err := n.Decode((*plain)(r)); err != nil {
		return err
	}
	r.line = n.Line
	return nil
}

func resolveAlias(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

// Condition is an AND of all keys that are present.
type Condition struct {
	Container StringList `yaml:"container"`
	Image     StringList `yaml:"image"`
	Source    string     `yaml:"source"`
	Level     string     `yaml:"level"`
	Match     string     `yaml:"match"`
	Expr      string     `yaml:"expr"`
}

// StringList decodes from a single YAML string or a list of strings.
type StringList []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (l *StringList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var s string
		if err := n.Decode(&s); err != nil {
			return err
		}
		*l = StringList{s}
		return nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return err
	}
	*l = list
	return nil
}

func (c *Condition) empty() bool {
	return len(c.Container) == 0 && len(c.Image) == 0 && c.Source == "" &&
		c.Level == "" && c.Match == "" && c.Expr == ""
}

// conditionKeys are the keys allowed in `when` and `unless`.
var conditionKeys = map[string]bool{
	"container": true, "image": true, "source": true, "level": true, "match": true, "expr": true,
}

// UnmarshalYAML rejects unknown keys and empty or null values. It
// checks these itself because Node.Decode does not inherit KnownFields.
func (c *Condition) UnmarshalYAML(n *yaml.Node) error {
	n = resolveAlias(n)
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: condition must be a mapping", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if !conditionKeys[k.Value] {
			return fmt.Errorf("line %d, column %d: unknown condition key %q", k.Line, k.Column, k.Value)
		}
		v = resolveAlias(v)
		if v.Tag == "!!null" || (v.Kind == yaml.SequenceNode && len(v.Content) == 0) ||
			(v.Kind == yaml.ScalarNode && v.Value == "") {
			return fmt.Errorf("line %d, column %d: %s: value is empty", k.Line, k.Column, k.Value)
		}
	}
	type plain Condition
	return n.Decode((*plain)(c))
}

// ParseRuleSet decodes a YAML rule list. Unknown keys are an error, so that
// a typo such as `dorp` cannot silently change what a rule does. An empty
// document gives an empty set.
func ParseRuleSet(data []byte) (RuleSet, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var rules RuleSet
	if err := dec.Decode(&rules); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("line %d: more than one YAML document (`---`) in the rules", extra.Line)
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	return rules, nil
}
