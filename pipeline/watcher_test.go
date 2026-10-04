package pipeline

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/gliderlabs/logspout/router"
)

const (
	dropNoisy = "rules:\n  - name: noisy\n    when: { match: noisy }\n    drop: true\n"
	dropQuiet = "rules:\n  - name: quiet\n    when: { match: quiet }\n    drop: true\n  - name: second\n    when: { match: other }\n    drop: true\n"
)

// captureLog returns the log output so far.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	log.SetOutput(&lockedWriter{&mu, &buf})
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// testWatcher watches a file in a temp dir and records what is applied.
type testWatcher struct {
	*Watcher
	mu      sync.Mutex
	applied []router.Processor
	n       int // writes, for distinct modification times
}

func newTestWatcher(t *testing.T, content *string, base Options) *testWatcher {
	t.Helper()
	tw := &testWatcher{}
	tw.Watcher = &Watcher{
		Path: filepath.Join(t.TempDir(), "logspout.yaml"),
		Env:  FileEnv{Routes: []string{"syslog"}},
		Base: base,
		Apply: func(p router.Processor) {
			tw.mu.Lock()
			defer tw.mu.Unlock()
			tw.applied = append(tw.applied, p)
		},
	}
	t.Cleanup(tw.Stop)
	if content != nil {
		tw.write(t, *content)
	}
	return tw
}

// write replaces the file and gives it a modification time that differs from the last write.
func (tw *testWatcher) write(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(tw.Path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	tw.n++
	mt := time.Unix(1_700_000_000+int64(tw.n), 0)
	if err := os.Chtimes(tw.Path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func (tw *testWatcher) count() int {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	return len(tw.applied)
}

// last returns the last applied processor; nil when none or when it was removed.
func (tw *testWatcher) last() *Pipeline {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if len(tw.applied) == 0 {
		return nil
	}
	p, _ := tw.applied[len(tw.applied)-1].(*Pipeline)
	return p
}

func wmsg(data string) *router.Message {
	return &router.Message{
		Container: &docker.Container{Name: "/c1", Config: &docker.Config{}},
		Source:    "stdout", Data: data,
	}
}

func dropped(p *Pipeline, data string) bool {
	return p != nil && p.Global(wmsg(data))
}

func TestWatcherReload(t *testing.T) {
	logs := captureLog(t)
	content := dropNoisy
	tw := newTestWatcher(t, &content, Options{})

	if res := tw.Reload(); !res.Applied || !strings.Contains(res.Summary, "1 user rules") {
		t.Fatalf("start-up: %+v", res)
	}
	if !dropped(tw.last(), "noisy") || dropped(tw.last(), "quiet") {
		t.Fatal("start-up rules not active")
	}
	if n := strings.Count(logs(), "pipeline:"); n != 1 {
		t.Fatalf("summary lines at start-up: %d\n%s", n, logs())
	}

	t.Run("no change, no log, no apply", func(t *testing.T) {
		before, applied := logs(), tw.count()
		for i := 0; i < 3; i++ {
			if tw.Check() {
				t.Fatal("reloaded without change")
			}
		}
		if logs() != before || tw.count() != applied {
			t.Errorf("activity without change:\n%s", logs()[len(before):])
		}
	})

	t.Run("valid change is swapped in", func(t *testing.T) {
		tw.write(t, dropQuiet)
		if !tw.Check() {
			t.Fatal("change not noticed")
		}
		if dropped(tw.last(), "noisy") || !dropped(tw.last(), "quiet") {
			t.Error("new rules not active")
		}
		if !strings.Contains(logs(), "2 user rules (2 global)") {
			t.Errorf("no summary of the reload:\n%s", logs())
		}
	})

	t.Run("invalid change keeps the last valid pipeline", func(t *testing.T) {
		applied, before := tw.count(), logs()
		tw.write(t, "rulez: []\nrules:\n  - name: x\n    when: { match: '(' }\n    drop: true\n")
		if !tw.Check() {
			t.Fatal("change not noticed")
		}
		if tw.count() != applied || !dropped(tw.last(), "quiet") {
			t.Error("invalid file replaced the pipeline")
		}
		out := logs()[len(before):]
		for _, want := range []string{"ERROR:", "1:1 unknown key", "NOT used", "previous rules stay active", "reloaded automatically"} {
			if !strings.Contains(out, want) {
				t.Errorf("log lacks %q:\n%s", want, out)
			}
		}
		// The error is logged once, not every poll.
		tw.Check()
		tw.Check()
		if strings.Count(logs(), "is invalid") != 1 {
			t.Errorf("error repeated:\n%s", logs())
		}
	})

	t.Run("fixing the file recovers", func(t *testing.T) {
		tw.write(t, dropNoisy)
		tw.Check()
		if !dropped(tw.last(), "noisy") {
			t.Error("fixed file not loaded")
		}
	})

	t.Run("delete removes the file rules", func(t *testing.T) {
		if err := os.Remove(tw.Path); err != nil {
			t.Fatal(err)
		}
		if !tw.Check() {
			t.Fatal("delete not noticed")
		}
		if tw.last() != nil || tw.applied[len(tw.applied)-1] != nil {
			t.Error("processor not removed")
		}
		if !strings.Contains(logs(), "no rules") {
			t.Errorf("no log line for the removal:\n%s", logs())
		}
		if tw.Check() {
			t.Error("reloaded again without change")
		}
	})

	t.Run("create loads it", func(t *testing.T) {
		tw.write(t, dropQuiet)
		if !tw.Check() || !dropped(tw.last(), "quiet") {
			t.Error("re-created file not loaded")
		}
	})
}

// A quick edit can keep the modification time; the size still differs.
func TestWatcherSameMtimeDifferentSize(t *testing.T) {
	captureLog(t)
	content := dropNoisy
	tw := newTestWatcher(t, &content, Options{})
	tw.Reload()
	info, err := os.Stat(tw.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tw.Path, []byte(dropQuiet), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tw.Path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if !tw.Check() || !dropped(tw.last(), "quiet") {
		t.Error("edit with unchanged mtime missed")
	}
}

func TestWatcherPolls(t *testing.T) {
	captureLog(t)
	content := dropNoisy
	tw := newTestWatcher(t, &content, Options{})
	tw.Interval = 5 * time.Millisecond
	tw.Reload()
	tw.Start()
	tw.Start() // second start is a no-op
	tw.write(t, dropQuiet)
	deadline := time.Now().Add(5 * time.Second)
	for !dropped(tw.last(), "quiet") {
		if time.Now().After(deadline) {
			t.Fatal("change not picked up by the poll")
		}
		time.Sleep(5 * time.Millisecond)
	}

	tw.Stop()
	tw.Stop() // safe twice
	applied := tw.count()
	tw.write(t, dropNoisy)
	time.Sleep(50 * time.Millisecond)
	if tw.count() != applied {
		t.Error("watcher still polls after Stop")
	}
}

// Without file and options nothing is installed, also when the poll runs.
// A file that appears later is loaded.
func TestWatcherNoFileNoOptions(t *testing.T) {
	logs := captureLog(t)
	tw := newTestWatcher(t, nil, Options{})
	tw.Reload()
	if tw.last() != nil || tw.applied[len(tw.applied)-1] != nil {
		t.Fatal("processor installed without rules")
	}
	for i := 0; i < 3; i++ {
		if tw.Check() {
			t.Fatal("reloaded without file")
		}
	}
	if logs() != "" {
		t.Errorf("log noise:\n%s", logs())
	}

	tw.write(t, dropNoisy)
	if !tw.Check() || !dropped(tw.last(), "noisy") {
		t.Error("late file not loaded")
	}
}

// Invalid at start-up: the level 1 options stay active, and the file is picked up once fixed.
func TestWatcherInvalidAtStartup(t *testing.T) {
	logs := captureLog(t)
	bad := "rulez: []\n"
	tw := newTestWatcher(t, &bad, Options{ExcludeContainers: []string{"c1"}})
	res := tw.Reload()
	if res.Applied || len(res.Errors) != 1 || res.Errors[0].Line != 1 {
		t.Fatalf("result %+v", res)
	}
	if !dropped(tw.last(), "x") {
		t.Error("level 1 options not active")
	}
	for _, want := range []string{"is invalid", "NOT used", "1:1", "reloaded automatically", "excluded containers: c1"} {
		if !strings.Contains(logs(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs())
		}
	}
	if strings.Contains(logs(), "restart the add-on") {
		t.Error("old restart advice")
	}
	tw.write(t, dropNoisy)
	tw.Check()
	if tw.last().Summary() == "" || !strings.Contains(tw.last().Summary(), "1 user rules") {
		t.Error("fixed file not loaded")
	}
}

// Build can fail on options that ParseFile does not see; logging must go on.
func TestWatcherBuildFailureFallsBack(t *testing.T) {
	logs := captureLog(t)
	content := dropNoisy
	tw := newTestWatcher(t, &content, Options{DefaultRules: "bogus"})
	res := tw.Reload()
	if res.Applied || len(res.Errors) == 0 {
		t.Fatalf("result %+v", res)
	}
	if tw.last() != nil || !strings.Contains(logs(), "ERROR") {
		t.Errorf("log:\n%s", logs())
	}
}

func TestWatcherReloadReturnsIssues(t *testing.T) {
	captureLog(t)
	bad := "targets:\n  nope: {}\n"
	tw := newTestWatcher(t, &bad, Options{})
	res := tw.Reload()
	if res.Applied || len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Message, "unknown target") {
		t.Errorf("result %+v", res)
	}
}

func TestWatcherConcurrentUse(t *testing.T) {
	captureLog(t)
	content := dropNoisy
	tw := newTestWatcher(t, &content, Options{})
	tw.Interval = time.Millisecond
	tw.Reload()
	tw.Start()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				tw.Reload()
			}
		}()
	}
	for j := 0; j < 50; j++ {
		tw.write(t, dropQuiet)
		tw.write(t, dropNoisy)
	}
	wg.Wait()
	tw.Stop()
}

func TestDebugTrace(t *testing.T) {
	logs := captureLog(t)
	opts := Options{
		DefaultRules: "v1",
		Rules:        mustRules(t, "- name: noisy\n  when: { match: noisy }\n  drop: true\n"),
		Targets:      map[string]RuleSet{"syslog": mustRules(t, "- name: warn\n  when: { match: slow }\n  set: { level: warning }\n")},
	}
	opts.Debug = true
	p, _, err := Build(opts)
	if err != nil {
		t.Fatal(err)
	}

	m := wmsg("hello slow")
	if p.Global(m) {
		t.Fatal("dropped")
	}
	out, drop := p.Target("syslog", m)
	if drop || out.Level != "warning" {
		t.Fatalf("target: %+v %v", out, drop)
	}
	p.Target("gelf", m)
	if !p.Global(wmsg("noisy line")) {
		t.Fatal("not dropped")
	}
	got := logs()
	for _, want := range []string{
		"pipeline trace: global container=c1 source=stdout lists=[defaults/v1,rules]",
		"level=info dropped=false",
		`rules:"noisy"[drop]`, "dropped=true",
		`pipeline trace: target=syslog container=c1 lists=[targets/syslog] matched=[targets/syslog:"warn"[set level=warning]`,
		"level=warning sent",
		"pipeline trace: target=gelf container=c1 no rules sent",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("trace lacks %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "pipeline trace:"); n != 4 {
		t.Errorf("%d trace lines:\n%s", n, got)
	}

	// Target drop
	dropAll := mustRules(t, "- name: nope\n  drop: true\n")
	p, _, _ = Build(Options{Debug: true, Targets: map[string]RuleSet{"syslog": dropAll}})
	if out, drop := p.Target("syslog", wmsg("x")); !drop || out != nil {
		t.Error("target drop lost")
	}
	if !strings.Contains(logs(), `[targets/syslog:"nope"[drop]`) || !strings.HasSuffix(strings.TrimSpace(logs()), "dropped") {
		t.Errorf("no drop trace:\n%s", logs())
	}
}

func TestNoTraceWhenDebugOff(t *testing.T) {
	logs := captureLog(t)
	p, _, err := Build(Options{
		Rules:   mustRules(t, "- name: a\n  set: { level: warning }\n"),
		Targets: map[string]RuleSet{"syslog": mustRules(t, "- name: b\n  set: { level: error }\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := wmsg("x")
	p.Global(m)
	p.Target("syslog", m)
	if logs() != "" {
		t.Errorf("trace without debug:\n%s", logs())
	}
}

// Debug alone is not a rule: no pipeline is active.
func TestDebugAloneIsEmpty(t *testing.T) {
	p, _, err := Build(Options{Debug: true})
	if err != nil || !p.Empty() {
		t.Fatalf("empty=%v err=%v", p.Empty(), err)
	}
	tw := newTestWatcher(t, nil, Options{Debug: true})
	tw.Reload()
	if tw.applied[len(tw.applied)-1] != nil {
		t.Error("processor installed for DEBUG_PIPELINE alone")
	}
}

func mustRules(t *testing.T, yaml string) RuleSet {
	t.Helper()
	rs, err := ParseRuleSet([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// A file that was just modified may be half written: it is loaded when the next poll sees no change.
func TestWatcherDebounce(t *testing.T) {
	captureLog(t)
	content := dropNoisy
	tw := newTestWatcher(t, &content, Options{})
	tw.Reload()
	must(t, os.WriteFile(tw.Path, []byte(dropQuiet), 0o600)) // mtime is now
	if tw.Check() || !dropped(tw.last(), "noisy") {
		t.Fatal("fresh change loaded at first sight")
	}
	must(t, os.WriteFile(tw.Path, []byte(dropQuiet+"# more\n"), 0o600))
	if tw.Check() {
		t.Fatal("changed again, still not settled")
	}
	if !tw.Check() || !dropped(tw.last(), "quiet") {
		t.Fatal("unchanged over two polls, not loaded")
	}
	// Reload is immediate.
	must(t, os.WriteFile(tw.Path, []byte(dropNoisy), 0o600))
	if res := tw.Reload(); !res.Applied || !dropped(tw.last(), "noisy") {
		t.Error("explicit Reload is not immediate")
	}
}

func TestLoadFileFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("no mkfifo: %v", err)
	}
	done := make(chan *FileResult, 1)
	go func() { done <- LoadFile(path, FileEnv{}) }()
	select {
	case res := <-done:
		if res.Err() == nil || !strings.Contains(res.Err().Error(), "not a regular file") {
			t.Errorf("result %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadFile blocks on a FIFO")
	}
}

func TestInvalidFileMessageWithoutEarlierVersion(t *testing.T) {
	logs := captureLog(t)
	bad := "rulez: []\n"
	tw := newTestWatcher(t, &bad, Options{})
	tw.Reload()
	tw.write(t, "rulez: [1]\n")
	tw.Check()
	if strings.Contains(logs(), "previous rules stay active") || strings.Count(logs(), "add-on options only") != 2 {
		t.Errorf("log:\n%s", logs())
	}
}

// Logspout's own log is shipped as a container log, so a trace line comes back
// as a message. It must not be traced again.
func TestDebugTraceDoesNotFeedItself(t *testing.T) {
	logs := captureLog(t)
	p, _, err := Build(Options{
		Debug:   true,
		Rules:   mustRules(t, "- name: a\n  set: { level: warning }\n"),
		Targets: map[string]RuleSet{"syslog": mustRules(t, "- name: b\n  set: { level: error }\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := wmsg("hello")
	p.Global(m)
	p.Target("syslog", m)
	p.Target("gelf", m)
	lines := strings.Split(strings.TrimSpace(logs()), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines:\n%s", len(lines), logs())
	}
	before := logs()
	for _, l := range lines {
		echo := wmsg(l)
		p.Global(echo)
		if echo.Level != "warning" {
			t.Errorf("echoed trace line is not processed: level %q", echo.Level)
		}
		if out, _ := p.Target("syslog", echo); out.Level != "error" {
			t.Errorf("echoed trace line is not processed by the target: %q", out.Level)
		}
		p.Target("gelf", echo)
	}
	if logs() != before {
		t.Errorf("echoed trace lines were traced:\n%s", logs()[len(before):])
	}
}

func TestDebugTraceRateLimit(t *testing.T) {
	logs := captureLog(t)
	p, _, err := Build(Options{Debug: true, Rules: mustRules(t, "- name: a\n  set: { level: warning }\n")})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000, 0)
	p.limiter.now = func() time.Time { return now }
	for i := 0; i < 500; i++ {
		p.Global(wmsg("x"))
	}
	if n := strings.Count(logs(), "pipeline trace: global"); n != maxTraceLinesPerSecond {
		t.Fatalf("%d lines in the first second", n)
	}
	now = now.Add(1100 * time.Millisecond)
	p.Global(wmsg("x"))
	got := logs()
	if !strings.Contains(got, "pipeline trace: suppressed 450 lines") || strings.Count(got, "pipeline trace: global") != maxTraceLinesPerSecond+1 {
		t.Errorf("after the window:\n%s", got[len(got)-300:])
	}
}
