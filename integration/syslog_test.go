//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gliderlabs/logspout/router"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestSyslogIntegration verifies that logspout forwards container logs to a
// syslog (UDP) destination.
func TestSyslogIntegration(t *testing.T) {
	addr, received := newUDPSink(t)
	marker := uniqueMarker()
	containerName := marker

	routeURI := fmt.Sprintf("syslog://%s?filter.name=%s", addr, containerName)
	if err := router.Routes.AddFromURI(routeURI); err != nil {
		t.Fatalf("add syslog route: %v", err)
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

	waitForMessage(t, received, marker, 30*time.Second)
}

// TestSyslogTCPIntegration verifies logspout with the syslog+tcp transport.
func TestSyslogTCPIntegration(t *testing.T) {
	addr, received := newTCPSink(t)
	marker := uniqueMarker()
	containerName := marker

	routeURI := fmt.Sprintf("syslog+tcp://%s?filter.name=%s", addr, containerName)
	if err := router.Routes.AddFromURI(routeURI); err != nil {
		t.Fatalf("add syslog+tcp route: %v", err)
	}
	defer removeRouteByAddr(t, addr)

	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image: "alpine:3",
		Name:  containerName,
		Cmd:   []string{"sh", "-c", fmt.Sprintf("while true; do echo %s; sleep 0.3; done", marker)},
		WaitingFor: wait.ForLog(marker),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start source container: %v", err)
	}
	defer c.Terminate(ctx) //nolint:errcheck

	waitForMessage(t, received, marker, 30*time.Second)
}

// removeRouteByAddr is a cleanup helper that removes the route whose Address
// matches addr.  Called via defer so that the sink goroutine outlives the test.
func removeRouteByAddr(t *testing.T, addr string) {
	t.Helper()
	routes, err := router.Routes.GetAll()
	if err != nil {
		return
	}
	for _, r := range routes {
		if r.Address == addr {
			router.Routes.Remove(r.ID)
			return
		}
	}
}
