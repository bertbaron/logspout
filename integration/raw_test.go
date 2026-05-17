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

// TestRawIntegration verifies that logspout forwards container logs as raw
// newline-delimited messages over TCP.
func TestRawIntegration(t *testing.T) {
	addr, received := newTCPSink(t)
	marker := uniqueMarker()
	containerName := marker

	routeURI := fmt.Sprintf("raw+tcp://%s?filter.name=%s", addr, containerName)
	if err := router.Routes.AddFromURI(routeURI); err != nil {
		t.Fatalf("add raw+tcp route: %v", err)
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

	// The raw adapter sends "{{.Data}}\n" – the marker is the full .Data.
	waitForMessage(t, received, marker, 30*time.Second)
}
