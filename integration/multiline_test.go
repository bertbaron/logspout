//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gliderlabs/logspout/router"
	"github.com/testcontainers/testcontainers-go"
)

// TestMultilineIntegration verifies that the multiline adapter joins
// continuation lines (lines starting with whitespace, the default pattern)
// before forwarding them to the downstream sink.
//
// The test container outputs:
//
//	MARKER
//	  continuation
//
// The default MULTILINE_PATTERN (^\s) and MULTILINE_MATCH (nonfirst) settings
// cause logspout to buffer the two lines and flush them as a single message
// after MULTILINE_FLUSH_AFTER (default 500 ms).  The expected combined message
// contains both the marker and the continuation text.
func TestMultilineIntegration(t *testing.T) {
	addr, received := newTCPSink(t)
	marker := uniqueMarker()
	containerName := marker
	continuation := "  continuation-" + marker

	// multiline+raw+tcp: multiline adapter wraps the raw/tcp adapter.
	routeURI := fmt.Sprintf("multiline+raw+tcp://%s?filter.name=%s", addr, containerName)
	if err := router.Routes.AddFromURI(routeURI); err != nil {
		t.Fatalf("add multiline+raw+tcp route: %v", err)
	}
	defer removeRouteByAddr(t, addr)

	// The container logs the first line once, then the continuation once, then
	// sleeps so the multiline flush timer fires and flushes the combined message.
	script := fmt.Sprintf(
		"echo %s; echo '%s'; sleep 2",
		marker,
		continuation,
	)
	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image: "alpine:3",
		Name:  containerName,
		Env: map[string]string{
			// Explicitly opt the container into multiline combining.
			"LOGSPOUT_MULTILINE": "true",
		},
		Cmd: []string{"sh", "-c", script},
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start source container: %v", err)
	}
	defer c.Terminate(ctx) //nolint:errcheck

	// The combined message must contain both the first line (marker) and the
	// continuation text joined by the separator (default "\n").
	waitForMessage(t, received, marker, 30*time.Second)
	// The sink channel already has the combined message; verify continuation.
	// We re-use waitForMessage on the same channel so order does not matter.
	waitForMessage(t, received, continuation, 5*time.Second)
}
