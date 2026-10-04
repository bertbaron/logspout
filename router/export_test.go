package router

import (
	"io"

	docker "github.com/fsouza/go-dockerclient"
)

// Hooks for the external tests in this directory, which can import pipeline.

type ContainerPump = containerPump

func NewContainerPump(c *docker.Container, stdout, stderr io.Reader) *ContainerPump {
	return newContainerPump(c, stdout, stderr)
}

func (cp *containerPump) Add(ch chan *Message, r *Route) { cp.add(ch, r) }

func NewJournalPump() *JournalPump { return newJournalPump(nil) }

func (p *JournalPump) AddStream(ch chan *Message, r *Route) {
	p.mu.Lock()
	p.logstreams[ch] = r
	p.mu.Unlock()
}

func (p *JournalPump) Feed(e *JournalEntry) {
	if m := p.toMessage(e); m != nil {
		p.dispatch(m)
	}
}

func (cp *containerPump) Remove(ch chan *Message) { cp.remove(ch) }

func (cp *containerPump) Send(m *Message) { cp.send(m) }

func (p *JournalPump) RemoveStream(ch chan *Message) {
	p.mu.Lock()
	delete(p.logstreams, ch)
	p.mu.Unlock()
}

func (p *JournalPump) Dispatch(m *Message) { p.dispatch(m) }
