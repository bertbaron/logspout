package syslog

import (
	"log/syslog"
	"testing"

	"github.com/gliderlabs/logspout/router"
)

func TestPriorityUnknownLevelFallsBackToSource(t *testing.T) {
	for _, lvl := range []string{"WARN", "Warning", "bogus", " info"} {
		for src, want := range map[string]syslog.Priority{
			"stdout":  syslog.LOG_USER | syslog.LOG_INFO,
			"stderr":  syslog.LOG_USER | syslog.LOG_ERR,
			"journal": syslog.LOG_DAEMON | syslog.LOG_INFO,
		} {
			m := &Message{&router.Message{Source: src, Level: lvl}}
			if got := m.Priority(); got != want {
				t.Errorf("level %q source %s: got %d, want %d", lvl, src, got, want)
			}
		}
	}
}
