//go:build integration

// Package integration provides end-to-end tests that spin up real Docker
// containers as log sources and verify that logspout's in-process router
// correctly forwards messages to in-process sinks.
//
// # Running locally
//
//	make test-integration
//
// The tests connect to the Docker socket from the DOCKER_HOST environment
// variable, or auto-detect the Colima socket at
// ~/.config/colima/default/docker.sock.
//
// # Journal tests (Phase 4)
//
// Tests tagged with the additional "journal" build tag are intended to run
// inside the Colima VM where systemd-journald is available.  They are
// cross-compiled on the Mac and executed via "colima ssh":
//
//	make test-integration-journal
package integration

import (
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/gliderlabs/logspout/adapters/gelf"
	_ "github.com/gliderlabs/logspout/adapters/multiline"
	_ "github.com/gliderlabs/logspout/adapters/raw"
	_ "github.com/gliderlabs/logspout/adapters/syslog"
	"github.com/gliderlabs/logspout/router"
	_ "github.com/gliderlabs/logspout/transports/tcp"
	_ "github.com/gliderlabs/logspout/transports/udp"
)

func TestMain(m *testing.M) {
	// Select the appropriate pump (docker vs journal) before any init() runs.
	setupLogSource()

	// Configure Docker endpoint.
	if h := dockerHost(); h != "" {
		os.Setenv("DOCKER_HOST", h) //nolint:errcheck
	}

	// Override DOCKER_CONFIG to prevent testcontainers-go from invoking the
	// macOS credential helper (docker-credential-osxkeychain) which hangs in
	// non-interactive environments.  Public images (alpine) need no auth.
	tmpDockerConfig, err := os.MkdirTemp("", "logspout-inttest-dockercfg")
	if err == nil {
		cfgPath := filepath.Join(tmpDockerConfig, "config.json")
		if writeErr := os.WriteFile(cfgPath, []byte(`{"auths":{}}`), 0600); writeErr == nil {
			os.Setenv("DOCKER_CONFIG", tmpDockerConfig) //nolint:errcheck
		}
		defer os.RemoveAll(tmpDockerConfig)
	}

	// Disable the Ryuk reaper container — it tries to bind-mount the Mac
	// Docker socket path into the colima VM where that path doesn't exist.
	// We use defer c.Terminate(ctx) in each test for cleanup instead.
	os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true") //nolint:errcheck

	// Set up and start the log pump.
	pump, found := router.Jobs.Lookup("pump")
	if !found {
		log.Fatal("integration: pump job not registered")
	}
	if err := pump.Setup(); err != nil {
		log.Fatalf("integration: pump setup failed: %v", err)
	}
	go func() {
		if err := pump.Run(); err != nil {
			log.Printf("integration: pump exited: %v", err)
		}
	}()

	// Start the route manager so that rm.routing=true; this makes
	// Routes.AddFromURI() immediately launch route goroutines in each test.
	// We call Run() directly (skip Setup) to avoid it parsing os.Args which
	// contains go test flags rather than route URIs.
	go func() {
		if err := router.Routes.Run(); err != nil {
			log.Printf("integration: routes exited: %v", err)
		}
	}()

	// Allow the pump to list running containers and begin listening for events.
	time.Sleep(500 * time.Millisecond)

	os.Exit(m.Run())
}
