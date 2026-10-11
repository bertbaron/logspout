//go:build linux && cgo

package journal

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gliderlabs/logspout/router"
)

// Needs a running journald and a copy of logger named dockerd, see JOURNAL_TEST_DOCKERD.
func writeDockerLog(t *testing.T, msg string) {
	t.Helper()
	bin := os.Getenv("JOURNAL_TEST_DOCKERD")
	if bin == "" {
		t.Skip("JOURNAL_TEST_DOCKERD not set")
	}
	cmd := exec.Command(bin, "--journald")
	cmd.Stdin = strings.NewReader("MESSAGE=" + msg + "\nCONTAINER_NAME=test\nCONTAINER_ID=abc\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write: %v: %s", err, out)
	}
}

func next(t *testing.T, s router.JournalSource) string {
	t.Helper()
	type res struct {
		e   *router.JournalEntry
		err error
	}
	ch := make(chan res, 1)
	go func() { e, err := s.Next(); ch <- res{e, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.e.Fields["MESSAGE"]
	case <-time.After(5 * time.Second):
		return ""
	}
}

func TestStartupAndResume(t *testing.T) {
	if os.Getenv("JOURNAL_TEST_DOCKERD") == "" {
		t.Skip("JOURNAL_TEST_DOCKERD not set")
	}
	t.Setenv("JOURNAL_PATH", filepath.Clean(os.Getenv("JOURNAL_TEST_DIR")))
	writeDockerLog(t, "banner")
	time.Sleep(time.Second)

	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := next(t, s); got != "banner" {
		t.Fatalf("first entry = %q, want banner", got)
	}
	_ = s.Close()

	writeDockerLog(t, "while-closed")
	time.Sleep(time.Second)
	s, err = Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := next(t, s); got != "while-closed" {
		t.Fatalf("after reopen = %q, want while-closed", got)
	}
	writeDockerLog(t, "third")
	if got := next(t, s); got != "third" {
		t.Fatalf("next entry = %q, want third (no duplicate of earlier entries)", got)
	}
}
