package router

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	docker "github.com/fsouza/go-dockerclient"
)

const journalPumpName = "journal"

func init() {
	if os.Getenv("LOG_SOURCE") != "journal" {
		return
	}
	pump := &JournalPump{
		logstreams: make(map[chan *Message]*Route),
	}
	LogRouters.Register(pump, defaultPumpName)
	Jobs.Register(pump, defaultPumpName)
}

// journalEntry holds the subset of journald JSON fields we care about.
type journalEntry struct {
	ContainerName   string `json:"CONTAINER_NAME"`
	ContainerIDFull string `json:"CONTAINER_ID_FULL"`
	ContainerID     string `json:"CONTAINER_ID"`
	Message         string `json:"MESSAGE"`
	// Priority: 0=emerg … 6=info, 7=debug (syslog levels).
	Priority             string `json:"PRIORITY"`
	RealtimeTimestampStr string `json:"__REALTIME_TIMESTAMP"`
}

func (e *journalEntry) source() string {
	// Priority 3 and lower map to stderr; 6+ to stdout.
	if p, err := strconv.Atoi(e.Priority); err == nil && p <= 4 {
		return "stderr"
	}
	return "stdout"
}

func (e *journalEntry) time() time.Time {
	if e.RealtimeTimestampStr == "" {
		return time.Now()
	}
	us, err := strconv.ParseInt(e.RealtimeTimestampStr, 10, 64)
	if err != nil {
		return time.Now()
	}
	return time.Unix(us/1e6, (us%1e6)*1000)
}

// syntheticContainer builds a minimal *docker.Container from journal fields so
// that existing adapters (syslog, gelf, loki, …) can access .Name and .ID
// without modification.
func syntheticContainer(e *journalEntry) *docker.Container {
	id := e.ContainerIDFull
	if id == "" {
		id = e.ContainerID
	}
	// Docker container names have a leading "/"; match that convention.
	name := "/" + e.ContainerName
	return &docker.Container{
		ID:   id,
		Name: name,
		Config: &docker.Config{
			Hostname: e.ContainerName,
			Labels:   map[string]string{},
		},
		HostConfig: &docker.HostConfig{},
		State:      docker.State{},
	}
}

// JournalPump reads container logs from systemd-journald via journalctl and
// routes them as router.Message objects.  It implements both the Job and
// LogRouter interfaces so it can replace LogsPump when LOG_SOURCE=journal.
type JournalPump struct {
	mu         sync.Mutex
	logstreams map[chan *Message]*Route
}

// Name returns the pump name.
func (p *JournalPump) Name() string {
	return journalPumpName
}

// Setup verifies that journalctl is available on the system.
func (p *JournalPump) Setup() error {
	if _, err := exec.LookPath("journalctl"); err != nil {
		return errors.New("journald pump: journalctl not found in PATH: " + err.Error())
	}
	return nil
}

// Run tails the system journal and forwards container log entries to all
// registered routes.  It blocks until the journalctl process exits.
func (p *JournalPump) Run() error {
	cmd := exec.Command("journalctl", "-f", "-o", "json",
		"--output-fields=CONTAINER_ID,CONTAINER_ID_FULL,CONTAINER_NAME,CONTAINER_TAG,MESSAGE,PRIORITY,__REALTIME_TIMESTAMP")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	scanner := bufio.NewScanner(stdout)
	// Journal entries can be large; increase the scanner buffer.
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var entry journalEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.ContainerName == "" {
			continue
		}
		container := syntheticContainer(&entry)
		msg := &Message{
			Container: container,
			Source:    entry.source(),
			Data:      entry.Message,
			Time:      entry.time(),
		}
		if stripAnsi {
			msg.Data = stripAnsiCodes(msg.Data)
		}
		p.dispatch(msg)
	}

	_ = cmd.Wait()
	return errors.New("journalctl stream ended")
}

// RoutingFrom returns whether the pump is currently handling the given
// container ID.  For the journal pump every container whose logs appear in
// the journal is considered "routing from" this pump.
func (p *JournalPump) RoutingFrom(_ string) bool {
	return true
}

// Route registers logstream as a destination for messages matching route and
// blocks until the route is closed.
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

// dispatch sends msg to every logstream whose route matches the message.
func (p *JournalPump) dispatch(msg *Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for logstream, route := range p.logstreams {
		if !route.MatchContainer(
			normalID(msg.Container.ID),
			normalName(msg.Container.Name),
			msg.Container.Config.Labels,
		) {
			continue
		}
		if !route.MatchMessage(msg) {
			continue
		}
		select {
		case logstream <- msg:
		default:
			debug("journal_pump.dispatch(): logstream full, dropping message")
		}
	}
}
