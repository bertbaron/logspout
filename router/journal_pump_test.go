package router

import (
	"strings"
	"testing"
	"time"
)

func journalEntry(fields map[string]string) *JournalEntry {
	return &JournalEntry{Fields: fields, Realtime: time.Unix(100, 0)}
}

func TestJournalPumpMapsContainerFields(t *testing.T) {
	p := newJournalPump(nil)
	msg := p.toMessage(journalEntry(map[string]string{
		"CONTAINER_NAME":    "addon_core_ssh",
		"CONTAINER_ID":      "0123456789ab",
		"CONTAINER_ID_FULL": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"IMAGE_NAME":        "ghcr.io/example/ssh:1.0",
		"MESSAGE":           "hello",
		"PRIORITY":          "6",
	}))
	if msg == nil {
		t.Fatal("expected message")
	}
	c := msg.Container
	if c.Name != "/addon_core_ssh" || normalName(c.Name) != "addon_core_ssh" {
		t.Errorf("unexpected name %q", c.Name)
	}
	if len(c.ID) != 64 || normalID(c.ID) != "0123456789ab" {
		t.Errorf("unexpected id %q", c.ID)
	}
	if c.Config.Image != "ghcr.io/example/ssh:1.0" || c.Config.Hostname != "0123456789ab" {
		t.Errorf("unexpected config %+v", c.Config)
	}
	if msg.Source != "stdout" || msg.Data != "hello" || !msg.Time.Equal(time.Unix(100, 0)) {
		t.Errorf("unexpected message %+v", msg)
	}
}

func TestJournalPumpSourceFromPriority(t *testing.T) {
	for priority, want := range map[string]string{"3": "stderr", "6": "stdout", "": "stdout"} {
		if got := journalSource(priority); got != want {
			t.Errorf("priority %q: got %s, want %s", priority, got, want)
		}
	}
}

func TestJournalPumpSkipsNonContainerAndEmptyEntries(t *testing.T) {
	p := newJournalPump(nil)
	if p.toMessage(journalEntry(map[string]string{"MESSAGE": "no container"})) != nil {
		t.Error("expected entry without container to be skipped")
	}
	if p.toMessage(journalEntry(map[string]string{"CONTAINER_NAME": "a", "CONTAINER_ID_FULL": "1", "MESSAGE": ""})) != nil {
		t.Error("expected empty message to be skipped")
	}
}

func TestJournalPumpJoinsPartialMessages(t *testing.T) {
	p := newJournalPump(nil)
	base := map[string]string{"CONTAINER_NAME": "a", "CONTAINER_ID_FULL": "1"}
	entry := func(data string, partial bool) *JournalEntry {
		f := map[string]string{"MESSAGE": data}
		for k, v := range base {
			f[k] = v
		}
		if partial {
			f["CONTAINER_PARTIAL_MESSAGE"] = "true"
		}
		return journalEntry(f)
	}
	if p.toMessage(entry("part1-", true)) != nil || p.toMessage(entry("part2-", true)) != nil {
		t.Fatal("partial messages must not be sent")
	}
	msg := p.toMessage(entry("end", false))
	if msg == nil || msg.Data != "part1-part2-end" {
		t.Fatalf("unexpected joined message %+v", msg)
	}
	if next := p.toMessage(entry("next", false)); next == nil || next.Data != "next" {
		t.Fatalf("partial buffer leaked into next message: %+v", next)
	}
}

func TestJournalPumpStripsANSI(t *testing.T) {
	t.Setenv(stripANSIEnvKey, "true")
	p := newJournalPump(nil)
	msg := p.toMessage(journalEntry(map[string]string{
		"CONTAINER_NAME": "a", "CONTAINER_ID_FULL": "1", "MESSAGE": "\x1b[31mred\x1b[0m",
	}))
	if msg == nil || msg.Data != "red" {
		t.Fatalf("unexpected message %+v", msg)
	}
}

func TestJournalPumpDispatchHonoursRouteFilter(t *testing.T) {
	p := newJournalPump(nil)
	route := &Route{FilterName: "wanted*"}
	stream := make(chan *Message, 2)
	p.logstreams[stream] = route

	for _, name := range []string{"wanted_one", "other"} {
		msg := p.toMessage(journalEntry(map[string]string{
			"CONTAINER_NAME": name, "CONTAINER_ID_FULL": "1", "MESSAGE": "x",
		}))
		p.dispatch(msg)
	}
	if len(stream) != 1 {
		t.Fatalf("expected 1 dispatched message, got %d", len(stream))
	}
	if got := (<-stream).Container.Name; !strings.HasSuffix(got, "wanted_one") {
		t.Errorf("unexpected container %q", got)
	}
}
