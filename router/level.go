package router

import "strings"

// Normalized log levels, ordered from least to most severe.
const (
	LevelDebug    = "debug"
	LevelInfo     = "info"
	LevelNotice   = "notice"
	LevelWarning  = "warning"
	LevelError    = "error"
	LevelCritical = "critical"
)

var levelRanks = map[string]int{
	LevelDebug:    0,
	LevelInfo:     1,
	LevelNotice:   2,
	LevelWarning:  3,
	LevelError:    4,
	LevelCritical: 5,
}

var levelAliases = map[string]string{
	"warn":    LevelWarning,
	"err":     LevelError,
	"fatal":   LevelCritical,
	"crit":    LevelCritical,
	"trace":   LevelDebug,
	"verbose": LevelDebug,
}

// NormalizeLevel maps a level name or alias, case-insensitively, to one of the
// normalized levels. The bool is false for unknown names.
func NormalizeLevel(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if _, ok := levelRanks[s]; ok {
		return s, true
	}
	if l, ok := levelAliases[s]; ok {
		return l, true
	}
	return "", false
}

// LevelRank returns the severity rank of a normalized level (higher is more
// severe). The bool is false for unknown levels.
func LevelRank(level string) (int, bool) {
	r, ok := levelRanks[level]
	return r, ok
}

// CompareLevels returns -1, 0 or 1 when a is less, equal or more severe than b.
// Both must be normalized levels; ok is false if either is unknown.
func CompareLevels(a, b string) (cmp int, ok bool) {
	ra, okA := levelRanks[a]
	rb, okB := levelRanks[b]
	if !okA || !okB {
		return 0, false
	}
	switch {
	case ra < rb:
		return -1, true
	case ra > rb:
		return 1, true
	}
	return 0, true
}
