package launcher

import (
	"bytes"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gliderlabs/logspout/router"
	"github.com/gliderlabs/logspout/runner"
)

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func runWithIngress(t *testing.T, addr string, during func()) (started bool, logs string) {
	t.Helper()
	optionsPath := filepath.Join(t.TempDir(), "options.json")
	if err := os.WriteFile(optionsPath, []byte(withPipelineFile(t, filepath.Join(t.TempDir(), "r.yaml"), `{"routes":["gelf://g:12201"]}`)), 0o600); err != nil {
		t.Fatal(err)
	}
	router.SetProcessor(nil)
	t.Cleanup(func() { router.SetProcessor(nil) })
	var mu sync.Mutex
	var buf bytes.Buffer
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	err := RunWithRunner(Options{
		DockerSocketPath: filepath.Join(t.TempDir(), "missing.sock"),
		OptionsPath:      optionsPath,
		UseJournal:       func() {},
		IngressAddr:      addr,
	}, func(runner.Options) error {
		started = true
		if during != nil {
			during()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return started, buf.String()
}

func TestIngressListenerStartsAndStops(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()

	var status int
	started, _ := runWithIngress(t, addr, func() {
		for i := 0; i < 100; i++ {
			resp, err := http.Get("http://" + addr + "/api/status")
			if err == nil {
				resp.Body.Close()
				status = resp.StatusCode
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	// The test client is not the Supervisor: reachable, but refused.
	if !started || status != http.StatusForbidden {
		t.Fatalf("started=%v status=%d", started, status)
	}
	if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		conn.Close()
		t.Error("listener still open after Run")
	}
}

func TestIngressBindFailureDoesNotStopLogging(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	started, logs := runWithIngress(t, busy.Addr().String(), func() { time.Sleep(200 * time.Millisecond) })
	if !started {
		t.Fatal("logging not started")
	}
	if !strings.Contains(logs, "ERROR: web interface (ingress) not available") {
		t.Errorf("no error logged:\n%s", logs)
	}
}

func TestNoIngressListenerByDefault(t *testing.T) {
	_, logs := runWithIngress(t, "", nil)
	if strings.Contains(logs, "ingress") {
		t.Errorf("ingress activity without address:\n%s", logs)
	}
}
