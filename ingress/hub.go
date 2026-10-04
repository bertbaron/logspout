package ingress

import (
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/gliderlabs/logspout/pipeline"
	"github.com/gliderlabs/logspout/router"
)

const (
	// SampleCount is the number of raw messages that are kept for the Playground.
	SampleCount = 500
	// MaxSampleData caps the data of a stored message, so the buffer holds at most
	// SampleCount x MaxSampleData bytes (8 MiB). The Playground and the live
	// view work on the capped text.
	MaxSampleData = 16 << 10
	// clientQueue is the number of traced messages a live client may lag behind.
	clientQueue = 256
)

// Sample is a raw message as it came from the log source, before the pipeline.
type Sample struct {
	Container string    `json:"container"`
	Image     string    `json:"image,omitempty"`
	Source    string    `json:"source"`
	Time      time.Time `json:"time"`
	Data      string    `json:"data"`
	Level     string    `json:"level"`
	// Truncated is set when Data was cut at MaxSampleData bytes.
	Truncated bool `json:"truncated,omitempty"`
}

func newSample(m *router.Message) Sample {
	s := Sample{Source: m.Source, Time: m.Time, Data: m.Data, Level: m.Level}
	if c := m.Container; c != nil {
		s.Container = strings.TrimPrefix(c.Name, "/")
		if c.Config != nil {
			s.Image = c.Config.Image
		}
	}
	s.cap()
	return s
}

// cap cuts Data on a rune boundary. Clone releases the rest of a long line.
func (s *Sample) cap() {
	if len(s.Data) <= MaxSampleData {
		return
	}
	n := MaxSampleData
	for n > 0 && !utf8.RuneStart(s.Data[n]) {
		n--
	}
	s.Data, s.Truncated = strings.Clone(s.Data[:n]), true
}

// message is the form the pipeline works on.
func (s Sample) message() *router.Message {
	m := &router.Message{Source: s.Source, Data: s.Data, Time: s.Time, Level: s.Level}
	if s.Container != "" || s.Image != "" {
		m.Container = &docker.Container{Name: "/" + s.Container, Config: &docker.Config{Image: s.Image}}
	}
	return m
}

// result is one traced message: the raw input and what the pipeline did.
type result struct {
	Input Sample `json:"input"`
	pipeline.MessageTrace
}

// ring keeps the last SampleCount samples.
type ring struct {
	mu   sync.Mutex
	buf  [SampleCount]Sample
	next int
	n    int
}

func (r *ring) add(s Sample) {
	r.mu.Lock()
	r.buf[r.next] = s
	r.next = (r.next + 1) % SampleCount
	if r.n < SampleCount {
		r.n++
	}
	r.mu.Unlock()
}

// snapshot returns the samples, oldest first.
func (r *ring) snapshot() []Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Sample, r.n)
	start := (r.next - r.n + SampleCount) % SampleCount
	for i := range out {
		out[i] = r.buf[(start+i)%SampleCount]
	}
	return out
}

// hub is the router.Tap of the server: it fills the ring and feeds live clients.
type hub struct {
	routes func() []string // route names, for the target traces
	ring   ring

	slots   atomic.Int32 // clients that passed the cap check, also while still in the handshake
	nlive   atomic.Int32 // number of clients, so the pumps skip the trace when 0
	mu      sync.RWMutex
	clients map[*client]struct{}
	closed  bool
}

var _ router.Tap = (*hub)(nil)

// client is one live websocket. The queue is bounded: a client that is too slow
// loses messages (counted in dropped) and never holds up the pumps.
type client struct {
	container string // glob, empty for all
	showAll   bool   // also messages that no target gets
	ch        chan *result
	dropped   atomic.Int64
	done      chan struct{}
	once      sync.Once
	close     func() // closes the connection
}

func (c *client) stop() {
	c.once.Do(func() {
		close(c.done)
		if c.close != nil {
			c.close()
		}
	})
}

func (c *client) matchesContainer(name string) bool {
	if c.container == "" {
		return true
	}
	ok, _ := path.Match(c.container, name)
	return ok
}

func (c *client) wants(r *result) bool {
	return (c.showAll || !r.Dropped) && c.matchesContainer(r.Input.Container)
}

// Observe runs in the pump. Trace lines of logspout itself are skipped: they
// would fill the buffer and, in the live view, feed back on themselves.
func (h *hub) Observe(m *router.Message, p router.Processor) {
	if pipeline.IsTraceLine(m.Data) {
		return
	}
	s := newSample(m)
	h.ring.add(s)
	if h.nlive.Load() == 0 {
		return
	}
	// Trace only when a client may want this container; the lock is not held during the trace.
	h.mu.RLock()
	want := false
	for c := range h.clients {
		if c.matchesContainer(s.Container) {
			want = true
			break
		}
	}
	h.mu.RUnlock()
	if !want {
		return
	}
	pl, _ := p.(*pipeline.Pipeline)
	r := &result{Input: s, MessageTrace: pl.TraceMessage(s.message(), h.routes())}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if !c.wants(r) {
			continue
		}
		select {
		case c.ch <- r:
		default:
			c.dropped.Add(1)
		}
	}
}

// add registers a client; nil when the hub is closed.
func (h *hub) add(c *client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	// The cap is a backstop: reserve() is the real limit.
	if h.closed || len(h.clients) >= maxLiveClients {
		return false
	}
	if h.clients == nil {
		h.clients = map[*client]struct{}{}
	}
	h.clients[c] = struct{}{}
	h.nlive.Add(1)
	return true
}

func (h *hub) remove(c *client) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		h.nlive.Add(-1)
	}
	h.mu.Unlock()
	c.stop()
}

// closeAll ends all clients and refuses new ones.
func (h *hub) closeAll() {
	h.mu.Lock()
	h.closed = true
	cs := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		cs = append(cs, c)
	}
	h.mu.Unlock()
	for _, c := range cs {
		c.stop()
	}
}

// reserve takes a client slot before the upgrade, so that an over-cap client gets a 503.
func (h *hub) reserve() bool {
	for {
		n := h.slots.Load()
		if n >= maxLiveClients {
			return false
		}
		if h.slots.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

func (h *hub) release() { h.slots.Add(-1) }
