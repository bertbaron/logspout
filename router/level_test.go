package router

import "testing"

func TestNormalizeLevel(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"debug", "debug", true},
		{"INFO", "info", true},
		{"Notice", "notice", true},
		{"warning", "warning", true},
		{"error", "error", true},
		{"critical", "critical", true},
		{"warn", "warning", true},
		{"WARN", "warning", true},
		{"err", "error", true},
		{"fatal", "critical", true},
		{"crit", "critical", true},
		{"trace", "debug", true},
		{"Verbose", "debug", true},
		{" info ", "info", true},
		{"", "", false},
		{"bogus", "", false},
	}
	for _, tt := range tests {
		got, ok := NormalizeLevel(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("NormalizeLevel(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestCompareLevels(t *testing.T) {
	tests := []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"debug", "info", -1, true},
		{"info", "info", 0, true},
		{"critical", "warning", 1, true},
		{"notice", "warning", -1, true},
		{"info", "bogus", 0, false},
		{"", "info", 0, false},
	}
	for _, tt := range tests {
		cmp, ok := CompareLevels(tt.a, tt.b)
		if cmp != tt.cmp || ok != tt.ok {
			t.Errorf("CompareLevels(%q, %q) = %d, %v; want %d, %v", tt.a, tt.b, cmp, ok, tt.cmp, tt.ok)
		}
	}
}

func TestLevelRankOrder(t *testing.T) {
	order := []string{LevelDebug, LevelInfo, LevelNotice, LevelWarning, LevelError, LevelCritical}
	for i, l := range order {
		r, ok := LevelRank(l)
		if !ok || r != i {
			t.Errorf("LevelRank(%q) = %d, %v; want %d, true", l, r, ok, i)
		}
	}
	if _, ok := LevelRank("bogus"); ok {
		t.Error("LevelRank(bogus) should not be ok")
	}
}
