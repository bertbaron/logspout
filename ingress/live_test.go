package ingress

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/gliderlabs/logspout/pipeline"
	"github.com/gliderlabs/logspout/router"
	"golang.org/x/net/websocket"
)

type liveFixture struct {
	*fixture
	addr string
}

// startLive serves on loopback, which the fixture allows, so the tap is installed like in production.
func startLive(t *testing.T, allowed string) *liveFixture {
	t.Helper()
	f := newFixture(t, pipeline.Options{})
	if rec := f.do("PUT", "/api/config", jsonBody(t, dropNoisy)); rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	f.srv.AllowedIP = allowed
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	done := make(chan struct{})
	go func() { f.srv.Serve(ln); close(done) }()
	t.Cleanup(func() { f.srv.Close(); <-done })
	waitFor(t, func() bool { return router.CurrentTap() != nil })
	return &liveFixture{f, ln.Addr().String()}
}

func (l *liveFixture) dial(query string) (*websocket.Conn, error) {
	return websocket.Dial("ws://"+l.addr+"/api/live"+query, "", "http://ha.example/")
}

func (l *liveFixture) connect(t *testing.T, query string) *websocket.Conn {
	t.Helper()
	n := l.srv.samples().nlive.Load()
	ws, err := l.dial(query)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	waitFor(t, func() bool { return l.srv.samples().nlive.Load() == n+1 })
	return ws
}

// pump does what the pumps do: the tap sees the raw message first.
func pump(m *router.Message) {
	router.CurrentTap().Observe(m, router.CurrentProcessor())
}

func recv(t *testing.T, ws *websocket.Conn) (r result) {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := websocket.JSON.Receive(ws, &r); err != nil {
		t.Fatalf("receive: %v", err)
	}
	return r
}

func TestLiveStreamsTracedMessages(t *testing.T) {
	l := startLive(t, "127.0.0.1")
	ws := l.connect(t, "")
	pump(cmsg("c1", "stderr", "something quiet"))
	r := recv(t, ws)
	if r.Input.Data != "something quiet" || r.Input.Container != "c1" || r.Input.Source != "stderr" || r.Dropped {
		t.Fatalf("%+v", r)
	}
	if len(r.Global.Rules) != 1 || r.Global.Rules[0].Name != "noisy" || r.Global.Rules[0].Matched || r.Global.EffectiveLevel != "error" {
		t.Errorf("global: %+v", r.Global)
	}
	if len(r.Targets) != 3 || !r.target("gelf").Sent {
		t.Errorf("targets: %+v", r.Targets)
	}
}

func TestLiveShowDropped(t *testing.T) {
	l := startLive(t, "127.0.0.1")
	plain := l.connect(t, "")
	all := l.connect(t, "?show_dropped=true")
	pump(cmsg("c1", "stdout", "noisy line"))
	pump(cmsg("c1", "stdout", "next"))

	if r := recv(t, plain); r.Input.Data != "next" {
		t.Errorf("without show_dropped got %q", r.Input.Data)
	}
	r := recv(t, all)
	if r.Input.Data != "noisy line" || !r.Dropped || !r.Global.Dropped || r.Global.DroppedBy == nil || r.Global.DroppedBy.Name != "noisy" {
		t.Errorf("show_dropped: %+v", r)
	}
	if r := recv(t, all); r.Input.Data != "next" {
		t.Errorf("then %q", r.Input.Data)
	}
}

func TestLiveContainerFilter(t *testing.T) {
	l := startLive(t, "127.0.0.1")
	exact := l.connect(t, "?container=c2")
	glob := l.connect(t, "?container=db*")
	for _, c := range []string{"c1", "c2", "db1", "c2"} {
		pump(cmsg(c, "stdout", "from "+c))
	}
	for i, want := range []string{"from c2", "from c2"} {
		if r := recv(t, exact); r.Input.Data != want {
			t.Errorf("exact %d: %q", i, r.Input.Data)
		}
	}
	if r := recv(t, glob); r.Input.Data != "from db1" {
		t.Errorf("glob: %q", r.Input.Data)
	}
}

func TestLiveSeesRewrittenResult(t *testing.T) {
	l := startLive(t, "127.0.0.1")
	rules := "rules:\n  - name: lvl\n    when: { match: boom }\n    set: { level: critical }\n"
	if rec := l.doFrom("127.0.0.1:1", "PUT", "/api/config", jsonBody(t, rules), nil); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	ws := l.connect(t, "")
	m := cmsg("c1", "stdout", "boom")
	pump(m)
	r := recv(t, ws)
	if r.Global.Level != "critical" || r.Input.Level != "" || m.Level != "" {
		t.Errorf("trace level %q, input %q, shared message %q", r.Global.Level, r.Input.Level, m.Level)
	}
}

func TestLiveSlowClientNeverBlocksPump(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	h := f.srv.samples()
	slow := &client{showAll: true, ch: make(chan *result, clientQueue), done: make(chan struct{})}
	if !h.add(slow) {
		t.Fatal("add")
	}
	const n = 5000
	start := time.Now()
	for i := 0; i < n; i++ {
		h.Observe(cmsg("c1", "stdout", fmt.Sprint("line ", i)), nil)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("pump blocked: %v", d)
	}
	if got := slow.dropped.Load(); got != n-clientQueue {
		t.Errorf("dropped %d, want %d", got, n-clientQueue)
	}
	if len(slow.ch) != clientQueue || (<-slow.ch).Input.Data != "line 0" {
		t.Error("queue keeps the oldest messages")
	}
	if got := len(h.ring.snapshot()); got != SampleCount {
		t.Errorf("samples %d", got)
	}
	h.remove(slow)
	if h.nlive.Load() != 0 {
		t.Error("client not removed")
	}
}

func TestLiveSlowClientOverWebsocket(t *testing.T) {
	l := startLive(t, "127.0.0.1")
	ws := l.connect(t, "")
	// The client does not read. Large lines fill the socket buffers, then the queue.
	big := string(make([]byte, MaxSampleData))
	done := make(chan struct{})
	go func() {
		for i := 0; i < 3000; i++ {
			pump(cmsg("c1", "stdout", big))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("pump blocked by a client that does not read")
	}
	ws.Close()
	waitFor(t, func() bool { return l.srv.samples().nlive.Load() == 0 })
}

func TestObserveCostWithoutLiveClients(t *testing.T) {
	h := &hub{routes: func() []string { return []string{"gelf"} }}
	m := cmsg("c1", "stdout", "line")
	if n := testing.AllocsPerRun(100, func() { h.Observe(m, nil) }); n != 0 {
		t.Errorf("%v allocations per message without live clients", n)
	}
}

func TestLiveClosesOnServerClose(t *testing.T) {
	l := startLive(t, "127.0.0.1")
	ws := l.connect(t, "")
	l.srv.Close()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var r result
	if err := websocket.JSON.Receive(ws, &r); err == nil {
		t.Fatal("connection still open after Close")
	}
	waitFor(t, func() bool { return l.srv.samples().nlive.Load() == 0 })
	// No new clients after Close.
	if ws, err := l.dial(""); err == nil {
		ws.SetReadDeadline(time.Now().Add(2 * time.Second))
		if err := websocket.JSON.Receive(ws, &r); err == nil {
			t.Error("client accepted after Close")
		}
		ws.Close()
	}
}

func TestLiveForbiddenFromOtherIP(t *testing.T) {
	l := startLive(t, "172.30.32.2") // the test client connects from 127.0.0.1
	if ws, err := l.dial(""); err == nil {
		ws.Close()
		t.Fatal("websocket upgrade allowed from another IP")
	}
	if l.srv.samples().nlive.Load() != 0 {
		t.Error("client registered")
	}
	if rec := l.doFrom("10.1.1.1:5", "GET", "/api/live", "", map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"}); rec.Code != 403 {
		t.Errorf("handler: %d", rec.Code)
	}
}

func TestLiveRequestChecks(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	if rec := f.do("GET", "/api/live", ""); rec.Code != 400 {
		t.Errorf("no upgrade: %d", rec.Code)
	}
	if rec := f.do("POST", "/api/live", ""); rec.Code != 405 || rec.Header().Get("Allow") != "GET" {
		t.Errorf("POST: %d", rec.Code)
	}
}
