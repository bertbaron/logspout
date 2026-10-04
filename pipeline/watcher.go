package pipeline

import (
	"log"
	"os"
	"sync"
	"time"

	"github.com/gliderlabs/logspout/router"
)

// DefaultWatchInterval is how often the rule file is checked.
const DefaultWatchInterval = 2 * time.Second

// Watcher loads the rule file, activates the resulting pipeline and reloads it
// when the file changes. A typo must never stop logging: an invalid change keeps
// the last valid pipeline.
type Watcher struct {
	// Path is the rule file. It does not have to exist.
	Path string
	// Env is what the file is validated against.
	Env FileEnv
	// Base are the level 1 options. The file is added on top.
	Base Options
	// Interval is the polling interval, DefaultWatchInterval when zero.
	Interval time.Duration
	// Apply activates a pipeline; nil removes it. Defaults to router.SetProcessor.
	Apply func(router.Processor)

	mu      sync.Mutex
	loaded  bool      // Reload ran at least once
	current *Pipeline // the active pipeline, nil or empty when none
	last    fileSig
	pending *fileSig // changed signature seen once, waiting to settle
	applied bool     // a pipeline from a valid file version was applied

	lastValid    bool // the last load of the file was valid
	lastErrors   []Issue
	lastWarnings []Issue
	stop         chan struct{}
	stopped      chan struct{}
}

// ReloadResult is the outcome of one Reload.
type ReloadResult struct {
	// Applied is false when the file is invalid; the previous pipeline stays active.
	Applied  bool
	Errors   []Issue
	Warnings []Issue
	// Summary describes the active pipeline, empty when there is none.
	Summary string
}

// fileSig is what the poll compares. The modification time alone can hide
// quick edits, so size and existence count too.
type fileSig struct {
	exists bool
	mtime  time.Time
	size   int64
}

func (a fileSig) equal(b fileSig) bool {
	return a.exists == b.exists && a.size == b.size && a.mtime.Equal(b.mtime)
}

func (w *Watcher) stat() fileSig {
	info, err := os.Stat(w.Path)
	if err != nil {
		return fileSig{}
	}
	return fileSig{exists: true, mtime: info.ModTime(), size: info.Size()}
}

func (w *Watcher) apply(p *Pipeline) {
	apply := w.Apply
	if apply == nil {
		apply = router.SetProcessor
	}
	if p.Empty() {
		apply(nil)
		return
	}
	apply(p)
}

// Reload loads the file now, and activates the result when it is valid. It logs
// one summary line, and the problems of an invalid file. The first call is the
// start-up: an invalid file then falls back to the level 1 options, because
// there is nothing earlier to keep.
func (w *Watcher) Reload() ReloadResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reload()
}

func (w *Watcher) reload() ReloadResult {
	startup := !w.loaded
	w.loaded = true
	// Stat before the read: an edit during the read shows up as a change at the next poll.
	w.last, w.pending = w.stat(), nil

	res := LoadFile(w.Path, w.Env)
	out := ReloadResult{Errors: res.Errors, Warnings: res.Warnings}
	for _, i := range res.Warnings {
		log.Printf("warning: %s: %s", w.Path, i)
	}

	defer func() {
		w.lastValid, w.lastErrors, w.lastWarnings = len(out.Errors) == 0, out.Errors, out.Warnings
	}()

	var p *Pipeline
	if res.Err() == nil {
		var err error
		if p, _, err = Build(res.Config.Apply(w.Base)); err != nil {
			// ParseFile compiled every rule, so this is an option problem; report it like a file problem.
			out.Errors = append(out.Errors, Issue{Message: err.Error()})
			p = nil
		}
	}
	if p == nil {
		w.logInvalid(out.Errors, !w.applied)
		if !startup {
			if !w.current.Empty() {
				out.Summary = w.current.Summary()
			}
			return out
		}
		var err error
		if p, _, err = Build(w.Base); err != nil {
			log.Printf("ERROR: pipeline disabled: %v", err)
			p, _, _ = Build(Options{Debug: w.Base.Debug})
		}
	} else {
		out.Applied = true
		w.applied = true
	}

	w.apply(p)
	if p.Empty() && w.Base.Debug {
		log.Println("DEBUG_PIPELINE: no rules active, nothing to trace")
	}
	hadRules := !w.current.Empty()
	w.current = p
	switch {
	case !p.Empty():
		out.Summary = p.Summary()
		log.Println(out.Summary)
	case hadRules:
		log.Println("pipeline: no rules, messages are sent unchanged")
	}
	return out
}

func (w *Watcher) logInvalid(errs []Issue, optionsOnly bool) {
	log.Printf("ERROR: ==================== %s is invalid ====================", w.Path)
	if optionsOnly {
		log.Printf("ERROR: the rule file is NOT used. Logging continues with the add-on options only.")
	} else {
		log.Printf("ERROR: the change is NOT used. The previous rules stay active.")
	}
	for _, e := range errs {
		log.Printf("ERROR:   %s", e)
	}
	log.Printf("ERROR: the file is reloaded automatically when it changes")
}

// Check reloads when the file changed since the last load and the change
// is the same on two consecutive polls, so a half written file or a save by
// rename (including a moment without file) is not loaded halfway. It reports
// whether it reloaded.
func (w *Watcher) Check() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	sig := w.stat()
	if sig.equal(w.last) {
		w.pending = nil
		return false
	}
	if w.pending == nil || !w.pending.equal(sig) {
		w.pending = &sig
		return false
	}
	w.reload()
	return true
}

// Status is a snapshot of the watcher for the status API.
type Status struct {
	// Loaded is false before the first Reload.
	Loaded bool
	// Valid is false when the last load of the file was invalid.
	Valid    bool
	Errors   []Issue
	Warnings []Issue
	// Pipeline is the active pipeline, nil when none.
	Pipeline *Pipeline
}

// Status returns the outcome of the last load and the active pipeline.
func (w *Watcher) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	return Status{Loaded: w.loaded, Valid: w.lastValid, Errors: w.lastErrors, Warnings: w.lastWarnings, Pipeline: w.current}
}

// Start polls the file until Stop. Reload must have run before.
func (w *Watcher) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop != nil {
		return
	}
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultWatchInterval
	}
	w.stop, w.stopped = make(chan struct{}), make(chan struct{})
	go func(stop <-chan struct{}, stopped chan<- struct{}) {
		defer close(stopped)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				w.Check()
			}
		}
	}(w.stop, w.stopped)
}

// Stop ends the polling and waits for it. It is safe to call more than once.
func (w *Watcher) Stop() {
	w.mu.Lock()
	stop, stopped := w.stop, w.stopped
	w.stop = nil
	w.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-stopped
}
