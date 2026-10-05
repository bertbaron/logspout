package ingress

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/gliderlabs/logspout/pipeline"
	"github.com/gliderlabs/logspout/router"
)

const demoRules = `defaults: v1
rules:
  - name: drop-health-checks
    when: {container: web, match: 'GET /health'}
    drop: true
  - name: mark-timeouts
    when: {match: 'timed? ?out'}
    set: {level: warning}
targets:
  gelf:
    rules:
      - name: gelf-no-debug
        when: {level: debug}
        drop: true
`

var demoLines = []struct{ container, source, data string }{
	{"homeassistant", "stdout", "2026-10-05 12:00:00.123 WARNING (MainThread) [homeassistant.components.mqtt] Connection lost"},
	{"homeassistant", "stdout", "2026-10-05 12:00:01.001 ERROR (MainThread) [custom_components.foo] Update failed"},
	{"homeassistant", "stdout", "2026-10-05 12:00:02.500 INFO (MainThread) [homeassistant.core] Starting"},
	{"web", "stdout", `10.0.0.1 - - "GET /health HTTP/1.1" 200 2`},
	{"web", "stdout", `10.0.0.2 - - "GET /index.html HTTP/1.1" 200 512`},
	{"web", "stderr", "upstream request timed out"},
	{"db", "stdout", "level=debug msg=\"checkpoint complete\""},
	{"db", "stderr", "level=error msg=\"disk almost full\""},
	{"hassio_dns", "stdout", "[INFO] 127.0.0.1:5353 - 4711 A IN example.com"},
	{"mosquitto", "stdout", "1759665600: New client connected from 10.0.0.9"},
}

// feedDemo sends a demo log line every 300 ms through the real tap and active processor.
func feedDemo(tap router.Tap, stop <-chan struct{}) {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for i := 0; ; i++ {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		l := demoLines[i%len(demoLines)]
		name := "/" + l.container
		tap.Observe(&router.Message{
			Container: &docker.Container{Name: name, Config: &docker.Config{}},
			Source:    l.source,
			Data:      fmt.Sprintf("%s #%d", l.data, i),
			Time:      time.Now(),
		}, router.CurrentProcessor())
	}
}

// TestServeUI serves the panel for a manual browser test. It does nothing unless
// LOGSPOUT_UI_ADDR is set, for example:
//
//	LOGSPOUT_UI_ADDR=127.0.0.1:8099 GOFLAGS=-tags=http2legacy go test -run TestServeUI -timeout 0 ./ingress
//
// The server loads a demo rule file and feeds demo log lines into the live view.
// Then open http://127.0.0.1:8099/. Edits to ui/ need a restart (files are embedded).
func TestServeUI(t *testing.T) {
	addr := os.Getenv("LOGSPOUT_UI_ADDR")
	if addr == "" {
		t.Skip("set LOGSPOUT_UI_ADDR to serve the UI")
	}
	path := filepath.Join(t.TempDir(), "logspout.yaml")
	if err := os.WriteFile(path, []byte(demoRules), 0o644); err != nil {
		t.Fatal(err)
	}
	w := &pipeline.Watcher{
		Path: path,
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
	stop := make(chan struct{})
	defer close(stop)
	go feedDemo(s.samples(), stop)
	// Runs until the test is interrupted.
	if err := s.Serve(ln); err != nil {
		t.Fatal(err)
	}
}
