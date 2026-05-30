//go:build integration && !journal

package integration

// setupLogSource is a no-op for non-journal integration tests; the default
// Docker pump is used in that case.
func setupLogSource() {}
