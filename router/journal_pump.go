package router

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	docker "github.com/fsouza/go-dockerclient"
)

const (
	journalPumpName    = "journal"
	journalRetryDelay  = 5 * time.Second
	journalPriorityErr = 3
)

// JournalEntry is a journal record, reduced to what the pump needs.
type JournalEntry struct {
	Fields   map[string]string
	Realtime time.Time
}

// JournalSource yields journal entries written by the Docker journald log driver.
type JournalSource interface {
	// Next blocks until the next entry is available.
	Next() (*JournalEntry, error)
	Close() error
}

// UseJournalPump replaces the Docker pump by a pump that reads from the journal.
// It must be called before the jobs are set up.
func UseJournalPump(open func() (JournalSource, error)) {
	Jobs.Unregister(defaultPumpName)
	LogRouters.Unregister(defaultPumpName)
	pump := newJournalPump(open)
	LogRouters.Register(pump, journalPumpName)
	Jobs.Register(pump, journalPumpName)
}

// JournalPump reads container logs from the journal and routes them like LogsPump does.
type JournalPump struct {
	open       func() (JournalSource, error)
	mu         sync.Mutex
	logstreams map[chan *Message]*Route
	partials   map[string]string
}

func newJournalPump(open func() (JournalSource, error)) *JournalPump {
	return &JournalPump{
		open:       open,
		logstreams: make(map[chan *Message]*Route),
		partials:   make(map[string]string),
	}
}

// Name returns the name of the pump
func (p *JournalPump) Name() string {
	return journalPumpName
}

// Setup verifies that the journal can be opened
func (p *JournalPump) Setup() error {
	src, err := p.open()
	if err != nil {
		return err
	}
	return src.Close()
}

// Run reads the journal until it is no longer readable
func (p *JournalPump) Run() error {
	for {
		src, err := p.open()
		if err != nil {
			return err
		}
		err = p.pump(src)
		_ = src.Close()
		log.Printf("journal pump: %v, reopening in %s", err, journalRetryDelay)
		time.Sleep(journalRetryDelay)
	}
}

func (p *JournalPump) pump(src JournalSource) error {
	for {
		entry, err := src.Next()
		if err != nil {
			return err
		}
		if msg := p.toMessage(entry); msg != nil {
			p.dispatch(msg)
		}
	}
}

// toMessage converts an entry to a message, or returns nil when there is nothing to send yet.
func (p *JournalPump) toMessage(entry *JournalEntry) *Message {
	f := entry.Fields
	container := syntheticContainer(f)
	if container == nil {
		return nil
	}
	data := f["MESSAGE"]
	// Docker splits lines over 16KB, the Docker API pump delivers them as one line
	if f["CONTAINER_PARTIAL_MESSAGE"] == trueString {
		p.partials[container.ID] += data
		return nil
	}
	if prefix, ok := p.partials[container.ID]; ok {
		data = prefix + data
		delete(p.partials, container.ID)
	}
	if data == "" {
		return nil
	}
	if stripANSIEnabled() {
		data = stripAnsiCodes(data)
	}
	return &Message{
		Container: container,
		Source:    journalSource(f["PRIORITY"]),
		Data:      data,
		Time:      entry.Realtime,
	}
}

// syntheticContainer provides the container info that is available in the journal.
// Fields that are only known to the Docker API are left empty.
func syntheticContainer(f map[string]string) *docker.Container {
	name := f["CONTAINER_NAME"]
	if name == "" {
		return nil
	}
	id := f["CONTAINER_ID_FULL"]
	if id == "" {
		id = f["CONTAINER_ID"]
	}
	return &docker.Container{
		ID:   id,
		Name: "/" + strings.TrimPrefix(name, "/"),
		Config: &docker.Config{
			Hostname: f["CONTAINER_ID"],
			Image:    f["IMAGE_NAME"],
			Labels:   map[string]string{},
		},
		HostConfig: &docker.HostConfig{},
	}
}

func journalSource(priority string) string {
	if p, err := strconv.Atoi(priority); err == nil && p == journalPriorityErr {
		return "stderr"
	}
	return "stdout"
}

// RoutingFrom returns whether a container id is routing from this pump
func (p *JournalPump) RoutingFrom(_ string) bool {
	return true
}

// Route registers the logstream until the route is closed
func (p *JournalPump) Route(route *Route, logstream chan *Message) {
	p.mu.Lock()
	p.logstreams[logstream] = route
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.logstreams, logstream)
		p.mu.Unlock()
		route.closed.Store(true)
	}()
	<-route.Closer()
}

func (p *JournalPump) dispatch(msg *Message) {
	proc := CurrentProcessor()
	if t := CurrentTap(); t != nil {
		t.Observe(msg, proc)
	}
	if proc != nil && proc.Global(msg) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for logstream, route := range p.logstreams {
		if !route.MatchContainer(normalID(msg.Container.ID), normalName(msg.Container.Name), msg.Container.Config.Labels) {
			continue
		}
		if !route.MatchMessage(msg) {
			continue
		}
		out := msg
		if proc != nil {
			var dropped bool
			if out, dropped = proc.Target(route.Name, msg); dropped {
				continue
			}
		}
		logstream <- out
	}
}
