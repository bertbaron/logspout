//go:build integration

package integration

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dockerHost returns the Docker endpoint to use for integration tests.
// It prefers DOCKER_HOST from the environment and falls back to the colima
// socket, checking both the XDG config path and the legacy ~/.colima path.
func dockerHost() string {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return h
	}
	for _, candidate := range []string{
		filepath.Join(os.Getenv("HOME"), ".config/colima/default/docker.sock"),
		filepath.Join(os.Getenv("HOME"), ".colima/default/docker.sock"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return "unix://" + candidate
		}
	}
	return ""
}

// uniqueMarker returns a unique string that can be embedded in container log
// output and searched for at the sink.
func uniqueMarker() string {
	return fmt.Sprintf("inttest-%d", time.Now().UnixNano())
}

// newUDPSink starts an in-process UDP listener and returns its address plus
// a channel that receives every datagram as a string.
// The listener is closed when the test ends via t.Cleanup.
func newUDPSink(t *testing.T) (addr string, received <-chan string) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("newUDPSink: %v", err)
	}
	ch := make(chan string, 512)
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			ch <- string(buf[:n])
		}
	}()
	return conn.LocalAddr().String(), ch
}

// newGELFSink starts an in-process UDP listener that decompresses GZIP GELF
// packets and returns raw JSON strings.
func newGELFSink(t *testing.T) (addr string, received <-chan string) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("newGELFSink: %v", err)
	}
	ch := make(chan string, 512)
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			data := buf[:n]
			if len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b {
				r, err := gzip.NewReader(bytes.NewReader(data))
				if err != nil {
					continue
				}
				decompressed, err := io.ReadAll(r)
				r.Close() //nolint:errcheck
				if err != nil {
					continue
				}
				data = decompressed
			}
			ch <- string(data)
		}
	}()
	return conn.LocalAddr().String(), ch
}

// newTCPSink starts an in-process TCP server and returns its address plus
// a channel that receives every newline-delimited message.
func newTCPSink(t *testing.T) (addr string, received <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("newTCPSink: %v", err)
	}
	ch := make(chan string, 512)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close() //nolint:errcheck
				buf := make([]byte, 65535)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						for _, line := range strings.Split(string(buf[:n]), "\n") {
							if line != "" {
								ch <- line
							}
						}
					}
					if err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), ch
}

// waitForMessage waits until a message containing marker arrives on received,
// or fails the test after timeout.
func waitForMessage(t *testing.T, received <-chan string, marker string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case msg := <-received:
			t.Logf("sink received: %s", msg)
			if strings.Contains(msg, marker) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out after %v waiting for marker %q", timeout, marker)
		}
	}
}
