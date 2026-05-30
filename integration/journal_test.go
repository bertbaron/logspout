//go:build integration && journal

// Package integration — journal integration tests
//
// These tests require a Linux host with systemd-journald available and
// containers started with --log-driver=journald.  They are intended to run
// inside the Colima VM:
//
//	make test-integration-journal
package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gliderlabs/logspout/router"
	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
)

// journaldContainerRequest returns a ContainerRequest that uses
// --log-driver=journald so its output is written to the system journal.
func journaldContainerRequest(name, cmd string) testcontainers.ContainerRequest {
	return testcontainers.ContainerRequest{
		Image: "alpine:3",
		Name:  name,
		Cmd:   []string{"sh", "-c", cmd},
		HostConfigModifier: func(hc *container.HostConfig) {
			hc.LogConfig = container.LogConfig{Type: "journald"}
		},
	}
}

// TestJournaldBasic verifies that the journal pump picks up log lines from a
// container that uses --log-driver=journald and forwards them to a syslog sink.
func TestJournaldBasic(t *testing.T) {
	addr, received := newUDPSink(t)
	marker := uniqueMarker()
	containerName := "jtest-basic-" + marker

	routeURI := fmt.Sprintf("syslog://%s?filter.name=%s", addr, containerName)
	if err := router.Routes.AddFromURI(routeURI); err != nil {
		t.Fatalf("add syslog route: %v", err)
	}
	defer removeRouteByAddr(t, addr)

	ctx := context.Background()
	req := journaldContainerRequest(
		containerName,
		fmt.Sprintf("while true; do echo %s; sleep 0.3; done", marker),
	)
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

// TestJournaldFilter verifies that the journal pump respects container name
// filtering when multiple containers emit logs simultaneously.
func TestJournaldFilter(t *testing.T) {
	marker1 := uniqueMarker()
	marker2 := uniqueMarker()
	name1 := "jtest-filter-a-" + marker1
	name2 := "jtest-filter-b-" + marker2

	// Sink for container 1 only — route filtered by name1.
	addr1, received1 := newUDPSink(t)
	routeURI := fmt.Sprintf("syslog://%s?filter.name=%s", addr1, name1)
	if err := router.Routes.AddFromURI(routeURI); err != nil {
		t.Fatalf("add syslog route: %v", err)
	}
	defer removeRouteByAddr(t, addr1)

	ctx := context.Background()

	// Start container 1 (should be routed).
	c1, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: journaldContainerRequest(
			name1,
			fmt.Sprintf("while true; do echo %s; sleep 0.3; done", marker1),
		),
		Started: true,
	})
	if err != nil {
		t.Fatalf("start container 1: %v", err)
	}
	defer c1.Terminate(ctx) //nolint:errcheck

	// Start container 2 (should NOT be routed to sink1).
	c2, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: journaldContainerRequest(
			name2,
			fmt.Sprintf("while true; do echo %s; sleep 0.3; done", marker2),
		),
		Started: true,
	})
	if err != nil {
		t.Fatalf("start container 2: %v", err)
	}
	defer c2.Terminate(ctx) //nolint:errcheck

	// Container 1's messages must arrive.
	waitForMessage(t, received1, marker1, 30*time.Second)

	// Container 2's marker must NOT appear on sink1 within a short window.
	assertNoMessage(t, received1, marker2, 2*time.Second)
}

// assertNoMessage checks that no message containing marker arrives on received
// within the given duration.
func assertNoMessage(t *testing.T, received <-chan string, marker string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case msg := <-received:
			if strings.Contains(msg, marker) {
				t.Errorf("unexpected message containing %q arrived at filtered sink: %s", marker, msg)
				return
			}
		case <-deadline:
			return
		}
	}
}
