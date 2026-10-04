package gelf

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Graylog2/go-gelf/gelf"
	docker "github.com/fsouza/go-dockerclient"

	"github.com/gliderlabs/logspout/router"
)

// mockGelfWriter records written messages for inspection in tests.
type mockGelfWriter struct {
	messages []*gelf.Message
}

func (m *mockGelfWriter) Close() error { return nil }

func (m *mockGelfWriter) Write(b []byte) (int, error) { return len(b), nil }

func (m *mockGelfWriter) WriteMessage(msg *gelf.Message) error {
	m.messages = append(m.messages, msg)
	return nil
}

func newTestContainer() *docker.Container {
	return &docker.Container{
		ID:   "abc123def456",
		Name: "/testcontainer",
		Config: &docker.Config{
			Hostname: "testhost",
			Image:    "testimage:latest",
			Cmd:      []string{"sh", "-c", "echo hello"},
		},
	}
}

// streamAndWait runs adapter.Stream in a goroutine, sends msgs, closes the
// stream, and waits for Stream to return. Reading mock after this call is safe.
func streamAndWait(adapter *Adapter, msgs ...*router.Message) {
	stream := make(chan *router.Message, len(msgs))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		adapter.Stream(stream)
	}()
	for _, m := range msgs {
		stream <- m
	}
	close(stream)
	wg.Wait()
}

func TestGelfStreamStdout(t *testing.T) {
	mock := &mockGelfWriter{}
	adapter := &Adapter{writer: mock}

	streamAndWait(adapter, &router.Message{
		Container: newTestContainer(),
		Data:      "hello world",
		Source:    "stdout",
		Time:      time.Now(),
	})

	if len(mock.messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(mock.messages))
	}
	msg := mock.messages[0]
	if msg.Short != "hello world" {
		t.Errorf("expected Short='hello world', got '%s'", msg.Short)
	}
	if msg.Level != int32(gelf.LOG_INFO) {
		t.Errorf("expected level LOG_INFO (%d), got %d", gelf.LOG_INFO, msg.Level)
	}
	if msg.Version != "1.1" {
		t.Errorf("expected version '1.1', got '%s'", msg.Version)
	}
}

func TestGelfStreamStderr(t *testing.T) {
	mock := &mockGelfWriter{}
	adapter := &Adapter{writer: mock}

	streamAndWait(adapter, &router.Message{
		Container: newTestContainer(),
		Data:      "error output",
		Source:    "stderr",
		Time:      time.Now(),
	})

	if len(mock.messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(mock.messages))
	}
	if mock.messages[0].Level != int32(gelf.LOG_ERR) {
		t.Errorf("expected level LOG_ERR for stderr, got %d", mock.messages[0].Level)
	}
}

func TestGelfStreamSkipsEmptyMessages(t *testing.T) {
	mock := &mockGelfWriter{}
	adapter := &Adapter{writer: mock}

	streamAndWait(adapter, &router.Message{
		Container: newTestContainer(),
		Data:      "",
		Source:    "stdout",
		Time:      time.Now(),
	})

	if len(mock.messages) != 0 {
		t.Fatalf("expected no GELF message for empty log line, got %d", len(mock.messages))
	}
}

func TestGelfGetExtraFields(t *testing.T) {
	container := newTestContainer()
	msg := Message{&router.Message{
		Container: container,
		Data:      "test",
		Source:    "stdout",
		Time:      time.Now(),
	}}

	raw, err := msg.getExtraFields()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var extra map[string]interface{}
	if err := json.Unmarshal(raw, &extra); err != nil {
		t.Fatalf("failed to unmarshal extra fields: %v", err)
	}

	if extra["_container_id"] != "abc123def456" {
		t.Errorf("expected _container_id='abc123def456', got '%v'", extra["_container_id"])
	}
	if extra["_container_name"] != "testcontainer" {
		t.Errorf("expected _container_name='testcontainer', got '%v'", extra["_container_name"])
	}
	if extra["_image_name"] != "testimage:latest" {
		t.Errorf("expected _image_name='testimage:latest', got '%v'", extra["_image_name"])
	}
}

func TestGelfGetExtraFieldsWithGelfLabels(t *testing.T) {
	container := newTestContainer()
	container.Config.Labels = map[string]string{
		"gelf_env":  "production",
		"other_key": "ignored",
	}
	msg := Message{&router.Message{
		Container: container,
		Data:      "test",
		Source:    "stdout",
		Time:      time.Now(),
	}}

	raw, err := msg.getExtraFields()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var extra map[string]interface{}
	if err := json.Unmarshal(raw, &extra); err != nil {
		t.Fatalf("failed to unmarshal extra fields: %v", err)
	}

	// Labels with "gelf_" prefix should be added with "_" prefix (stripping "gelf")
	if extra["_env"] != "production" {
		t.Errorf("expected _env='production', got '%v'", extra["_env"])
	}
	if _, ok := extra["other_key"]; ok {
		t.Error("other_key should not be in extra fields")
	}
}

func TestGelfNewAdapterUnknownTransport(t *testing.T) {
	route := &router.Route{
		Adapter: "gelf+invalid",
		Address: "localhost:12201",
	}
	_, err := NewGelfAdapter(route)
	if err == nil {
		t.Error("expected error for unknown transport, got nil")
	}
}

func TestGelfNewAdapterUsesRuntimeHostname(t *testing.T) {
	if _, err := os.Stat("/etc/host_hostname"); err == nil {
		t.Skip("/etc/host_hostname exists; runtime env precedence cannot be asserted on this host")
	}

	t.Setenv("SYSLOG_HOSTNAME", "runtime.example.com")

	adapter, err := NewGelfAdapter(&router.Route{
		Adapter: "gelf+udp",
		Address: "127.0.0.1:12201",
	})
	if err != nil {
		t.Fatalf("unexpected error creating adapter: %v", err)
	}

	mock := &mockGelfWriter{}
	gelfAdapter := adapter.(*Adapter)
	gelfAdapter.writer = mock

	streamAndWait(gelfAdapter, &router.Message{
		Container: newTestContainer(),
		Data:      "hello world",
		Source:    "stdout",
		Time:      time.Now(),
	})

	if len(mock.messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(mock.messages))
	}
	if got := mock.messages[0].Host; got != "runtime.example.com" {
		t.Fatalf("expected runtime hostname, got %q", got)
	}
}

func TestGelfGetExtraFieldsOmitsUnknownDockerFields(t *testing.T) {
	container := newTestContainer()
	container.Image = ""
	container.Config.Cmd = nil
	container.Created = time.Time{}
	msg := Message{&router.Message{Container: container, Data: "test", Source: "stdout", Time: time.Now()}}

	raw, err := msg.getExtraFields()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var extra map[string]interface{}
	if err := json.Unmarshal(raw, &extra); err != nil {
		t.Fatalf("failed to unmarshal extra fields: %v", err)
	}
	for _, key := range []string{"_image_id", "_command", "_created"} {
		if _, ok := extra[key]; ok {
			t.Errorf("expected %s to be omitted", key)
		}
	}
	if extra["_container_name"] != "testcontainer" {
		t.Errorf("expected _container_name to be kept, got '%v'", extra["_container_name"])
	}
}

// Without Level and Fields the output must equal the pre-pipeline behavior.
func TestGelfCompatEmptyLevelAndFields(t *testing.T) {
	tests := []struct {
		source string
		want   int32
	}{
		{"stdout", int32(gelf.LOG_INFO)},
		{"stderr", int32(gelf.LOG_ERR)},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			mock := &mockGelfWriter{}
			streamAndWait(&Adapter{writer: mock}, &router.Message{
				Container: newTestContainer(),
				Data:      "line",
				Source:    tt.source,
				Time:      time.Now(),
			})
			if len(mock.messages) != 1 {
				t.Fatalf("expected 1 message, got %d", len(mock.messages))
			}
			if mock.messages[0].Level != tt.want {
				t.Errorf("level = %d, want %d", mock.messages[0].Level, tt.want)
			}
			var extra map[string]interface{}
			if err := json.Unmarshal(mock.messages[0].RawExtra, &extra); err != nil {
				t.Fatal(err)
			}
			want := []string{"_container_id", "_container_name", "_image_name", "_command"}
			if len(extra) != len(want) {
				t.Errorf("extra keys = %v, want exactly %v", extra, want)
			}
			for _, k := range want {
				if _, ok := extra[k]; !ok {
					t.Errorf("missing %s", k)
				}
			}
		})
	}
}

func TestGelfLevelAndFields(t *testing.T) {
	tests := []struct {
		level  string
		source string
		want   int32
	}{
		{"debug", "stdout", int32(gelf.LOG_DEBUG)},
		{"info", "stderr", int32(gelf.LOG_INFO)},
		{"notice", "stdout", int32(gelf.LOG_NOTICE)},
		{"warning", "stdout", int32(gelf.LOG_WARNING)},
		{"error", "stdout", int32(gelf.LOG_ERR)},
		{"critical", "stdout", int32(gelf.LOG_CRIT)},
	}
	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			mock := &mockGelfWriter{}
			streamAndWait(&Adapter{writer: mock}, &router.Message{
				Container: newTestContainer(),
				Data:      "line",
				Source:    tt.source,
				Time:      time.Now(),
				Level:     tt.level,
				Fields:    map[string]string{"logger": "homeassistant.core", "container_id": "evil"},
			})
			msg := mock.messages[0]
			if msg.Level != tt.want {
				t.Errorf("level = %d, want %d", msg.Level, tt.want)
			}
			var extra map[string]interface{}
			if err := json.Unmarshal(msg.RawExtra, &extra); err != nil {
				t.Fatal(err)
			}
			if extra["_logger"] != "homeassistant.core" {
				t.Errorf("_logger = %v", extra["_logger"])
			}
			if extra["_container_id"] != "abc123def456" {
				t.Errorf("built-in _container_id was overridden: %v", extra["_container_id"])
			}
		})
	}
}
