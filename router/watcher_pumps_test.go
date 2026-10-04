package router_test

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gliderlabs/logspout/pipeline"
	"github.com/gliderlabs/logspout/router"
)

// A reloaded rule file takes effect in both pumps without a restart.
func TestWatcherReloadThroughPumps(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	for name, mk := range pumpHarnesses {
		t.Run(name, func(t *testing.T) {
			router.SetProcessor(nil)
			t.Cleanup(func() { router.SetProcessor(nil) })
			path := filepath.Join(t.TempDir(), "logspout.yaml")
			n := 0
			write := func(content string) {
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				n++
				mt := time.Unix(1_700_000_000+int64(n), 0)
				_ = os.Chtimes(path, mt, mt)
			}
			w := &pipeline.Watcher{Path: path, Env: pipeline.FileEnv{Routes: []string{"syslog"}}}
			t.Cleanup(w.Stop)

			h := mk(t)
			ch := h.route("syslog")
			run := func(lines ...string) []string {
				for _, l := range lines {
					h.feed("c1", "stdout", l)
				}
				h.feed("c1", "stdout", sentinel)
				return datas(collect(t, ch, 1))
			}
			all := []string{"noisy", "quiet", "plain"}

			// No file: the plain path.
			w.Reload()
			if router.CurrentProcessor() != nil {
				t.Fatal("processor without rules")
			}
			if got := run(all...); !reflect.DeepEqual(got, all) {
				t.Errorf("no rules: %v", got)
			}

			write("rules:\n  - {name: a, when: {match: noisy}, drop: true}\n")
			w.Check()
			if !w.Check() {
				t.Fatal("new file not noticed")
			}
			if got, want := run(all...), []string{"quiet", "plain"}; !reflect.DeepEqual(got, want) {
				t.Errorf("after create: %v", got)
			}

			write("rules:\n  - {name: b, when: {match: quiet}, drop: true}\n")
			w.Check()
			w.Check()
			if got, want := run(all...), []string{"noisy", "plain"}; !reflect.DeepEqual(got, want) {
				t.Errorf("after edit: %v", got)
			}

			write("rules: [oops\n")
			w.Check()
			w.Check()
			if got, want := run(all...), []string{"noisy", "plain"}; !reflect.DeepEqual(got, want) {
				t.Errorf("after invalid edit: %v", got)
			}

			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			w.Check()
			w.Check()
			if router.CurrentProcessor() != nil {
				t.Error("processor kept after delete")
			}
			if got := run(all...); !reflect.DeepEqual(got, all) {
				t.Errorf("after delete: %v", got)
			}
		})
	}
}

// Messages keep flowing while the pipeline is swapped. Run with -race.
func TestWatcherSwapUnderLoad(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	router.SetProcessor(nil)
	t.Cleanup(func() { router.SetProcessor(nil) })

	path := filepath.Join(t.TempDir(), "logspout.yaml")
	files := []string{
		"rules:\n  - {name: a, when: {match: x}, set: {level: warning}}\ntargets:\n  syslog:\n    rules:\n      - {name: t, set: {level: error}}\n",
		"rules:\n  - {name: b, when: {match: x}, drop: true}\n  - {name: c, set: {level: debug}}\n",
	}
	w := &pipeline.Watcher{Path: path, Env: pipeline.FileEnv{Routes: []string{"syslog"}}}
	t.Cleanup(w.Stop)
	if err := os.WriteFile(path, []byte(files[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	w.Reload()

	p := router.NewJournalPump()
	ch := make(chan *router.Message, 100)
	p.AddStream(ch, newRoute("syslog"))
	stop := make(chan struct{})
	var drain sync.WaitGroup
	drain.Add(1)
	go func() {
		defer drain.Done()
		for {
			select {
			case <-ch:
			case <-stop:
				return
			}
		}
	}()

	var senders sync.WaitGroup
	for i := 0; i < 4; i++ {
		senders.Add(1)
		go func() {
			defer senders.Done()
			for j := 0; j < 500; j++ {
				p.Feed(&router.JournalEntry{
					Fields:   map[string]string{"CONTAINER_NAME": "c1", "CONTAINER_ID_FULL": "id", "MESSAGE": "x", "PRIORITY": "6"},
					Realtime: time.Unix(100, 0),
				})
			}
		}()
	}
	for i := 0; i < 40; i++ {
		if err := os.WriteFile(path, []byte(files[i%2]), 0o600); err != nil {
			t.Fatal(err)
		}
		w.Reload()
	}
	senders.Wait()
	close(stop)
	drain.Wait()
}
