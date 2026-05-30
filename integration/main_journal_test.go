//go:build integration && journal

package integration

import "os"

// setupLogSource sets LOG_SOURCE=journal so that JournalPump is registered
// instead of the default Docker pump.  Called from TestMain.
func setupLogSource() {
	os.Setenv("LOG_SOURCE", "journal") //nolint:errcheck
}
