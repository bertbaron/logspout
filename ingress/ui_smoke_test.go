package ingress

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/gliderlabs/logspout/pipeline"
)

// TestServeUI serves the panel for a manual browser test. It does nothing unless
// LOGSPOUT_UI_ADDR is set, for example:
//
//	LOGSPOUT_UI_ADDR=127.0.0.1:8099 GOFLAGS=-tags=http2legacy go test -run TestServeUI -timeout 0 ./ingress
//
// Then open http://127.0.0.1:8099/. Edits to ui/ need a restart (files are embedded).
func TestServeUI(t *testing.T) {
	addr := os.Getenv("LOGSPOUT_UI_ADDR")
	if addr == "" {
		t.Skip("set LOGSPOUT_UI_ADDR to serve the UI")
	}
	w := &pipeline.Watcher{
		Path: filepath.Join(t.TempDir(), "logspout.yaml"),
		Env:  pipeline.FileEnv{Routes: []string{"gelf", "syslog"}},
		Base: pipeline.Options{DefaultRules: "v1"},
	}
	w.Reload()
	defer w.Stop()
	s := &Server{Watcher: w, AllowedIP: "127.0.0.1"}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("serving on http://%s/ (rule file %s)", ln.Addr(), w.Path)
	defer s.Close()
	// Runs until the test is interrupted.
	if err := s.Serve(ln); err != nil {
		t.Fatal(err)
	}
}
