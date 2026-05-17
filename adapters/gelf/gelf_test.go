package gelf

import (
	"encoding/json"
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
