package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gliderlabs/logspout/router"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func activeDrops(tw *testWatcher, data string) bool { return dropped(tw.last(), data) }

// Every editor save pattern ends with the full file active.
func TestVerifySavePatterns(t *testing.T) {
	captureLog(t)
	content := dropNoisy
	tw := newTestWatcher(t, &content, Options{})
	tw.Reload()
	bump := func(i int) {
		mt := time.Unix(1_600_000_000+int64(i), 0)
		must(t, os.Chtimes(tw.Path, mt, mt))
	}

	// truncate, observe the empty file, then write
	f, err := os.OpenFile(tw.Path, os.O_TRUNC|os.O_WRONLY, 0o600)
	must(t, err)
	bump(1)
	tw.poll()
	// An empty file is a valid file without rules.
	if activeDrops(tw, "noisy") {
		t.Error("empty file keeps the old rules")
	}
	_, err = f.WriteString(dropQuiet)
	must(t, err)
	must(t, f.Close())
	bump(2)
	if !tw.poll() || !activeDrops(tw, "quiet") || !activeDrops(tw, "other") {
		t.Fatal("after truncate+write the full file is not active")
	}

	// partial write (cut in the middle of a rule), then the rest
	f, err = os.OpenFile(tw.Path, os.O_TRUNC|os.O_WRONLY, 0o600)
	must(t, err)
	_, err = f.WriteString(dropNoisy[:30])
	must(t, err)
	bump(3)
	tw.poll()
	_, err = f.WriteString(dropNoisy[30:])
	must(t, err)
	must(t, f.Close())
	bump(4)
	tw.poll()
	if !activeDrops(tw, "noisy") || activeDrops(tw, "quiet") {
		t.Fatal("after partial write the full file is not active")
	}

	// temp + rename
	tmp := tw.Path + ".tmp"
	must(t, os.WriteFile(tmp, []byte(dropQuiet), 0o600))
	must(t, os.Rename(tmp, tw.Path))
	bump(5)
	if !tw.poll() || !activeDrops(tw, "quiet") {
		t.Fatal("rename replace not picked up")
	}

	// rename replace with identical mtime and size: documented limit of the poll
	same := strings.Replace(dropQuiet, "quiet", "QUIET", 1)
	must(t, os.WriteFile(tmp, []byte(same), 0o600))
	must(t, os.Rename(tmp, tw.Path))
	bump(5)
	if tw.Check() || !activeDrops(tw, "quiet") || activeDrops(tw, "QUIET") {
		t.Error("a replace with the same mtime and size is a known limit, but it was handled differently")
	}
}

func TestVerifyFileKinds(t *testing.T) {
	captureLog(t)
	content := dropNoisy
	tw := newTestWatcher(t, &content, Options{})
	tw.Reload()

	// valid -> directory: keep last valid
	must(t, os.Remove(tw.Path))
	must(t, os.Mkdir(tw.Path, 0o700))
	tw.poll()
	if !activeDrops(tw, "noisy") {
		t.Fatal("directory in place of the file lost the last valid rules")
	}
	must(t, os.Remove(tw.Path))

	// symlink to a valid file
	real := filepath.Join(filepath.Dir(tw.Path), "real.yaml")
	must(t, os.WriteFile(real, []byte(dropQuiet), 0o600))
	old := time.Unix(1_600_000_000, 0)
	must(t, os.Chtimes(real, old, old))
	must(t, os.Symlink(real, tw.Path))
	if !tw.poll() || !activeDrops(tw, "quiet") {
		t.Fatal("symlink not followed")
	}
	// dangling symlink
	// A dangling symlink is a missing file: the file rules are removed.
	must(t, os.Remove(real))
	tw.poll()
	if tw.last() != nil {
		t.Error("dangling symlink keeps the file rules")
	}
}

func TestVerifyUnreadableKeepsLast(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	captureLog(t)
	content := dropNoisy
	tw := newTestWatcher(t, &content, Options{})
	tw.Reload()
	must(t, os.Chmod(tw.Path, 0))
	t.Cleanup(func() { _ = os.Chmod(tw.Path, 0o600) })
	// size/mtime unchanged: chmod alone does not trigger a reload
	tw.poll()
	// force change signature
	mt := time.Unix(1_650_000_000, 0)
	must(t, os.Chtimes(tw.Path, mt, mt))
	if !tw.poll() {
		t.Fatal("expected reload")
	}
	if !activeDrops(tw, "noisy") {
		t.Fatal("unreadable file lost the last valid rules")
	}
}

func TestVerifyTooBigKeepsLast(t *testing.T) {
	logs := captureLog(t)
	content := dropNoisy
	tw := newTestWatcher(t, &content, Options{})
	tw.Reload()
	big := dropQuiet + "# " + strings.Repeat("x", MaxFileSize) + "\n"
	tw.write(t, big)
	res := tw.Reload()
	if res.Applied || !activeDrops(tw, "noisy") {
		t.Fatalf("big file: applied=%v\n%s", res.Applied, logs())
	}
	tw.write(t, dropQuiet)
	if !tw.poll() || !activeDrops(tw, "quiet") {
		t.Fatal("shrunk file not loaded")
	}
}

func TestVerifyFlip(t *testing.T) {
	logs := captureLog(t)
	content := dropNoisy
	tw := newTestWatcher(t, &content, Options{})
	tw.Reload()
	for i := 0; i < 50; i++ {
		tw.write(t, "rulez: []\n")
		tw.poll()
		if !activeDrops(tw, "noisy") {
			t.Fatal("lost rules on invalid")
		}
		tw.write(t, dropQuiet)
		tw.poll()
		if !activeDrops(tw, "quiet") || activeDrops(tw, "noisy") {
			t.Fatal("valid not active")
		}
		tw.write(t, dropNoisy)
		tw.poll()
	}
	if n := strings.Count(logs(), "is invalid"); n != 50 {
		t.Errorf("invalid blocks=%d, want 50", n)
	}
}

// Real router processor, poller, UI-like Reload and message flow together.
func TestVerifyConcurrentReloadWithRouter(t *testing.T) {
	captureLog(t)
	router.SetProcessor(nil)
	t.Cleanup(func() { router.SetProcessor(nil) })
	path := filepath.Join(t.TempDir(), "logspout.yaml")
	must(t, os.WriteFile(path, []byte(dropNoisy), 0o600))
	w := &Watcher{Path: path, Env: FileEnv{Routes: []string{"syslog"}}, Interval: time.Millisecond}
	w.Reload()
	w.Start()

	var stop atomic.Bool
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				if p := router.CurrentProcessor(); p != nil {
					m := wmsg("noisy quiet other")
					if !p.Global(m) {
						p.Target("syslog", m)
					}
				}
			}
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				w.Reload()
			}
		}()
	}
	for j := 0; j < 100; j++ {
		for _, c := range []string{"", "rulez: []\n", dropQuiet, dropNoisy} {
			must(t, os.WriteFile(path, []byte(c), 0o600))
			if j%2 == 0 {
				must(t, os.WriteFile(path+".tmp", []byte(c), 0o600))
				must(t, os.Rename(path+".tmp", path))
			}
		}
	}
	stop.Store(true)
	wg.Wait()
	must(t, os.WriteFile(path, []byte(dropQuiet), 0o600))
	w.Reload()
	w.Stop()
	p := router.CurrentProcessor()
	if p == nil || !p.Global(wmsg("quiet")) {
		t.Fatal("final file not active")
	}
}

func TestVerifyStopDuringReload(t *testing.T) {
	captureLog(t)
	content := dropNoisy
	tw := newTestWatcher(t, &content, Options{})
	slow := make(chan struct{})
	var once sync.Once
	tw.Watcher.Apply = func(router.Processor) { once.Do(func() { close(slow) }); time.Sleep(20 * time.Millisecond) }
	tw.Interval = time.Millisecond
	tw.Start()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5; i++ {
			tw.write(t, dropQuiet)
			tw.write(t, dropNoisy)
		}
		tw.Reload()
		close(done)
	}()
	<-slow
	finished := make(chan struct{})
	go func() { tw.Stop(); tw.Stop(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hangs")
	}
	<-done
}

// No file, no options: no processor and no log over several polls, with the real router.
func TestVerifyNoFileNoProcessorPolling(t *testing.T) {
	logs := captureLog(t)
	router.SetProcessor(nil)
	w := &Watcher{Path: filepath.Join(t.TempDir(), "none.yaml"), Interval: 2 * time.Millisecond}
	w.Reload()
	w.Start()
	for i := 0; i < 20; i++ {
		time.Sleep(3 * time.Millisecond)
		if router.CurrentProcessor() != nil {
			t.Fatal("processor installed")
		}
	}
	w.Stop()
	if logs() != "" {
		t.Errorf("log lines:\n%s", logs())
	}
}

func TestVerifyDebugExprError(t *testing.T) {
	logs := captureLog(t)
	p, _, err := Build(Options{
		Debug: true,
		Rules: mustRules(t, "- name: boom\n  when: { expr: 'int(message) > 1' }\n  drop: true\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Global(wmsg("not a number")) {
		t.Error("expr error must not drop")
	}
	got := logs()
	if !strings.Contains(got, `rules:"boom"`) || !strings.Contains(got, "error=") || !strings.Contains(got, "dropped=false") {
		t.Errorf("trace lacks expr error:\n%s", got)
	}
	// empty message
	p.Global(wmsg(""))
}
