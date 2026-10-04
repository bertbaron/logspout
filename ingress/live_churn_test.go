package ingress

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gliderlabs/logspout/pipeline"
	"github.com/gliderlabs/logspout/router"
	"golang.org/x/net/websocket"
)

func (l *liveFixture) put(method, target, body string) *httptest.ResponseRecorder {
	return l.doFrom("127.0.0.1:4000", method, target, body, nil)
}

func goroutinesSettle(t *testing.T, max int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= max {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("goroutine leak: %d, want at most %d\n%s", n, max, buf)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Clients come and go and the pipeline is replaced (PUT) while the pumps run.
func TestLiveChurnWithReloadAndPump(t *testing.T) {
	l := startLive(t, "127.0.0.1")
	h := l.srv.samples()
	base := runtime.NumGoroutine()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var pumped atomic.Int64
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				pump(cmsg(fmt.Sprint("c", i), "stdout", fmt.Sprint("line noisy ", j)))
				pump(cmsg("c1", "stderr", fmt.Sprint("ERR ", j)))
				pumped.Add(2)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			c := dropNoisy
			if i%2 == 1 {
				c = dropQuiet
			}
			if rec := l.put("PUT", "/api/config", jsonBody(t, c)); rec.Code != 200 {
				t.Errorf("PUT: %d %s", rec.Code, rec.Body)
				return
			}
		}
	}()

	var cw sync.WaitGroup
	slots := make(chan struct{}, maxLiveClients) // more at once get 503
	for i := 0; i < 40; i++ {
		cw.Add(1)
		go func() {
			defer cw.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			q := ""
			if i%2 == 0 {
				q = "?show_dropped=true"
			}
			// The server frees a slot a moment after the client closed: retry a 503.
			var ws *websocket.Conn
			var err error
			for try := 0; try < 200; try++ {
				if ws, err = l.dial(q); err == nil {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err != nil {
				t.Errorf("dial: %v", err)
				return
			}
			if i%3 != 0 {
				ws.SetReadDeadline(time.Now().Add(5 * time.Second))
				var r result
				if err := websocket.JSON.Receive(ws, &r); err != nil {
					t.Errorf("receive: %v", err)
				}
			}
			ws.Close()
		}()
	}
	cw.Wait()
	close(stop)
	wg.Wait()
	if pumped.Load() == 0 {
		t.Fatal("nothing pumped")
	}

	waitFor(t, func() bool { return h.nlive.Load() == 0 && h.slots.Load() == 0 })
	h.mu.RLock()
	n := len(h.clients)
	h.mu.RUnlock()
	if n != 0 {
		t.Errorf("%d clients left in the hub", n)
	}
	goroutinesSettle(t, base+2)
}

func TestServerCloseTwiceAndWithLiveClientsMidWrite(t *testing.T) {
	l := startLive(t, "127.0.0.1")
	h := l.srv.samples()
	base := runtime.NumGoroutine()
	var clients []*websocket.Conn
	for i := 0; i < 5; i++ {
		clients = append(clients, l.connect(t, "?show_dropped=true"))
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				h.Observe(cmsg("c1", "stdout", strings.Repeat("x", 4000)), router.CurrentProcessor())
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	closed := make(chan struct{})
	t0 := time.Now()
	go func() { l.srv.Close(); l.srv.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocks with live clients mid-write")
	}
	t.Logf("Close took %v", time.Since(t0))
	close(stop)
	<-done
	for _, ws := range clients {
		ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			var r result
			if err := websocket.JSON.Receive(ws, &r); err != nil {
				break
			}
		}
	}
	if n := l.srv.samples().nlive.Load(); n != 0 {
		t.Errorf("nlive %d after Close", n)
	}
	waitFor(t, func() bool { return l.srv.samples().slots.Load() == 0 })
	if _, err := l.dial(""); err == nil {
		t.Error("dial works after Close")
	}
	goroutinesSettle(t, base+2)
}

func TestCloseTwiceWithoutServe(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	f.srv.Close()
	f.srv.Close()
	if router.CurrentTap() != nil {
		t.Error("tap installed")
	}
}

func testResults(t *testing.T, f *fixture, content string, msgs []map[string]any) []result {
	t.Helper()
	rec := f.do("POST", "/api/test", testBody(t, content, msgs))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var r testResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("%v\n%s", err, rec.Body)
	}
	return r.Results
}

func TestTestEndpointOddMessages(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	long := strings.Repeat("é", MaxSampleData) // 2 bytes per rune: cut must stay on a rune boundary
	msgs := []map[string]any{
		{"data": ""},
		{"data": "ctrl \x00\x01\x07 \r\n\t bell"},
		{"data": "bad utf8 \xff\xfe end"},
		{"data": long, "container": "c1"},
		{"data": "no container"},
		{"data": "zero time", "time": "0001-01-01T00:00:00Z"},
		{"data": "slash", "container": "/c1"},
	}
	res := testResults(t, f, draftRules, msgs)
	if len(res) != len(msgs) {
		t.Fatalf("%d results", len(res))
	}
	if res[0].Input.Data != "" || res[0].Global.Message != "" {
		t.Errorf("empty message: %+v", res[0])
	}
	if !strings.Contains(res[1].Input.Data, "\x00") {
		t.Errorf("control characters lost: %q", res[1].Input.Data)
	}
	if res[3].Input.Truncated != true || len(res[3].Input.Data) > MaxSampleData || !utf8.ValidString(res[3].Input.Data) {
		t.Errorf("long data: truncated=%v len=%d valid=%v", res[3].Input.Truncated, len(res[3].Input.Data), utf8.ValidString(res[3].Input.Data))
	}
	if res[4].Input.Container != "" {
		t.Errorf("container %q", res[4].Input.Container)
	}
	if res[6].Input.Container != "c1" {
		t.Errorf("leading slash kept: %q", res[6].Input.Container)
	}
	for i, r := range res {
		if len(r.Targets) != 3 {
			t.Errorf("result %d: %d targets", i, len(r.Targets))
		}
	}
	// The response must be valid JSON with invalid UTF-8 in the input.
	rec := f.do("POST", "/api/test", testBody(t, "", []map[string]any{{"data": "bad \xff"}}))
	if rec.Code != 200 || !utf8.Valid(rec.Body.Bytes()) {
		t.Errorf("response not valid UTF-8 JSON: %d", rec.Code)
	}
}

func TestTestEndpointDefaultsOffVersusLatest(t *testing.T) {
	line := "2026-10-04 12:00:00.123 WARNING (MainThread) [homeassistant.components.x] Something odd"
	msgs := []map[string]any{{"container": "homeassistant", "data": line}}
	off := newFixture(t, pipeline.Options{DefaultRules: "off"})
	if got := testResults(t, off, "", msgs)[0].Global.EffectiveLevel; got != "info" {
		t.Errorf("defaults off: level %q", got)
	}
	latest := newFixture(t, pipeline.Options{DefaultRules: "latest"})
	if got := testResults(t, latest, "", msgs)[0].Global.Level; got != "warning" {
		t.Errorf("defaults latest: level %q", got)
	}
	// The draft can choose its own defaults.
	if got := testResults(t, off, "defaults: latest\n", msgs)[0].Global.Level; got != "warning" {
		t.Errorf("draft defaults: level %q", got)
	}
	if got := testResults(t, latest, "defaults: off\n", msgs)[0].Global.Level; got == "warning" {
		t.Errorf("draft defaults off still classified: %q", got)
	}
}

func TestTestEndpointAmbiguousRouteIsReportedNotTargetable(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	res := testResults(t, f, "", []map[string]any{{"data": "x"}})
	names := []string{}
	for _, tt := range res[0].Targets {
		names = append(names, tt.Name)
	}
	if strings.Join(names, ",") != "gelf,syslog,dup" {
		t.Errorf("targets %v", names)
	}
	rec := f.do("POST", "/api/test", testBody(t, "targets:\n  dup:\n    rules:\n      - { name: a, when: { match: x }, drop: true }\n", nil))
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "ambiguous") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestTestEndpointIdenticalDraftTouchesNothing(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	if rec := f.do("PUT", "/api/config", jsonBody(t, dropNoisy)); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	before := router.CurrentProcessor()
	fi1 := fileInfo(t, f.path)
	testResults(t, f, dropNoisy, []map[string]any{{"data": "noisy"}})
	if router.CurrentProcessor() != before {
		t.Error("processor replaced by an identical draft")
	}
	if fi2 := fileInfo(t, f.path); fi1 != fi2 {
		t.Errorf("file touched: %v -> %v", fi1, fi2)
	}
}

func fileInfo(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Stat(path)
	must(t, err)
	return fmt.Sprint(fi.Size(), fi.ModTime().UnixNano())
}

// A POST /api/test racing with PUT and with the pumps must be race free.
func TestTestEndpointConcurrentWithPutAndTap(t *testing.T) {
	l := startLive(t, "127.0.0.1")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				if rec := l.put("POST", "/api/test", testBody(t, draftRules, nil)); rec.Code != 200 && rec.Code != 429 { // 429: another test is running
					t.Errorf("%d %s", rec.Code, rec.Body)
					return
				}
				l.put("GET", "/api/samples", "")
				pump(cmsg("c1", "stdout", "noisy"))
				if j%5 == 0 {
					l.put("PUT", "/api/config", jsonBody(t, dropQuiet))
				}
			}
		}()
	}
	wg.Wait()
}

// Close must not wait for the 10 s write timeout of a client that does not read.
func TestServerCloseDoesNotWaitForStalledClient(t *testing.T) {
	l := startLive(t, "127.0.0.1")
	h := l.srv.samples()
	l.connect(t, "?show_dropped=true") // never reads
	big := strings.Repeat("x", MaxSampleData)
	for i := 0; i < 600; i++ {
		h.Observe(cmsg("c1", "stdout", big), nil)
	}
	time.Sleep(300 * time.Millisecond) // the writer is now blocked on a full socket
	start := time.Now()
	l.srv.Close()
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Close took %v with a stalled live client", d)
	}
}
