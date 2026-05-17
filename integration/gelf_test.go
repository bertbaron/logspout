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

// TestGELFIntegration verifies that logspout forwards container logs to a
// GELF (UDP) destination and that the messages are correctly encoded.
func TestGELFIntegration(t *testing.T) {
	addr, received := newGELFSink(t)
	marker := uniqueMarker()
	containerName := marker

	routeURI := fmt.Sprintf("gelf://%s?filter.name=%s", addr, containerName)
	if err := router.Routes.AddFromURI(routeURI); err != nil {
		t.Fatalf("add gelf route: %v", err)
	}
	defer removeRouteByAddr(t, addr)

	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image: "alpine:3",
		Name:  containerName,
		Cmd:   []string{"sh", "-c", fmt.Sprintf("while true; do echo %s; sleep 0.3; done", marker)},
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start source container: %v", err)
	}
	defer c.Terminate(ctx) //nolint:errcheck

	// GELF messages are JSON; check the "short_message" field contains our marker.
	waitForMessage(t, received, marker, 30*time.Second)
}
