//go:build integration && journal

// Package integration — journal integration tests
//
// These tests require a Linux host with systemd-journald available and
// containers started with --log-driver=journald.  They are intended to run
// inside the Colima VM:
//
//	make test-integration-journal
//
// The tests are stubs that document the expected behaviour for Phase 4
// (journald pump).  Implement them after the JournalPump is written.
package integration

import "testing"

// TestJournaldBasic verifies that the journal pump picks up log lines from a
// container that uses --log-driver=journald and forwards them to a sink.
//
// TODO (Phase 4): implement when JournalPump is available.
func TestJournaldBasic(t *testing.T) {
	t.Skip("TODO Phase 4: implement JournalPump and enable this test")
}

// TestJournaldFilter verifies that the journal pump respects container name
// filtering when multiple containers emit logs simultaneously.
//
// TODO (Phase 4): implement when JournalPump is available.
func TestJournaldFilter(t *testing.T) {
	t.Skip("TODO Phase 4: implement JournalPump and enable this test")
}
