package splunk

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	docker "github.com/fsouza/go-dockerclient"

	"github.com/gliderlabs/logspout/router"
)

func newTestContainer() *docker.Container {
	return &docker.Container{
		ID:   "abc123def456",
		Name: "/testcontainer",
		Config: &docker.Config{
			Hostname: "testhost",
			Labels:   map[string]string{"app": "myapp"},
		},
	}
}

func TestGetStringParameter(t *testing.T) {
	opts := map[string]string{"key": "value"}
	if got := getStringParameter(opts, "key", "default"); got != "value" {
		t.Errorf("expected 'value', got '%s'", got)
	}
	if got := getStringParameter(opts, "missing", "default"); got != "default" {
		t.Errorf("expected 'default', got '%s'", got)
	}
}

func TestGetIntParameter(t *testing.T) {
	opts := map[string]string{"count": "42", "bad": "notanint"}
	if got := getIntParameter(opts, "count", 0); got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
	if got := getIntParameter(opts, "bad", 10); got != 10 {
		t.Errorf("expected default 10 for bad value, got %d", got)
	}
	if got := getIntParameter(opts, "missing", 7); got != 7 {
		t.Errorf("expected default 7 for missing key, got %d", got)
	}
}

func TestGetDurationParameter(t *testing.T) {
	opts := map[string]string{"timeout": "500ms", "bad": "notaduration"}
	expected := 500 * time.Millisecond
	if got := getDurationParameter(opts, "timeout", time.Second); got != expected {
		t.Errorf("expected %v, got %v", expected, got)
	}
	if got := getDurationParameter(opts, "bad", time.Second); got != time.Second {
		t.Errorf("expected default 1s for bad value, got %v", got)
	}
	if got := getDurationParameter(opts, "missing", 2*time.Second); got != 2*time.Second {
		t.Errorf("expected default 2s for missing key, got %v", got)
	}
}

func TestCreateRequestNoGzip(t *testing.T) {
	payload := `{"event":"test"}`
	req := createRequest("http://localhost/collect", false, "mytoken", payload)
	if req == nil {
		t.Fatal("expected non-nil request")
	}
	if req.Header.Get("Authorization") != "Splunk mytoken" {
		t.Errorf("expected Authorization header 'Splunk mytoken', got '%s'", req.Header.Get("Authorization"))
	}
	body, _ := io.ReadAll(req.Body)
	if string(body) != payload {
		t.Errorf("expected body '%s', got '%s'", payload, string(body))
	}
}

func TestCreateRequestWithGzip(t *testing.T) {
	req := createRequest("http://localhost/collect", true, "", `{"event":"test"}`)
	if req == nil {
		t.Fatal("expected non-nil request")
	}
	if req.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("expected Content-Encoding 'gzip', got '%s'", req.Header.Get("Content-Encoding"))
	}
}

func TestCreateRequestNoToken(t *testing.T) {
	req := createRequest("http://localhost/collect", false, "", `{}`)
	if req.Header.Get("Authorization") != "" {
		t.Errorf("expected empty Authorization header when no token, got '%s'", req.Header.Get("Authorization"))
	}
}

func TestSplunkAdapterSendsMessages(t *testing.T) {
	received := make(chan []byte, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Build an adapter that points to the test server (HTTP, not HTTPS).
	// We bypass NewSplunkAdapter to avoid the hardcoded https:// and use
	// a short flush timeout so the test completes quickly.
	timeout := 50 * time.Millisecond
	a := &SplunkAdapter{
		route:            &router.Route{Adapter: "splunk", Address: srv.Listener.Addr().String()},
		url:              srv.URL + "/services/collector",
		client:           srv.Client(),
		buffer:           make([]*router.Message, 0, 1),
		timer:            time.NewTimer(timeout),
		capacity:         1,
		timeout:          timeout,
		useGzip:          false,
		crash:            false,
		splunkToken:      "testtoken",
		splunkIndex:      "main",
		splunkSourcetype: "docker",
	}

	stream := make(chan *router.Message, 1)
	go a.Stream(stream)

	stream <- &router.Message{
		Container: newTestContainer(),
		Data:      "splunk test message",
		Source:    "stdout",
		Time:      time.Now(),
	}

	select {
	case body := <-received:
		var msg SplunkMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			t.Fatalf("failed to unmarshal received body: %v (body was: %s)", err, string(body))
		}
		if msg.Event.Message != "splunk test message" {
			t.Errorf("expected message 'splunk test message', got '%s'", msg.Event.Message)
		}
		if msg.SourceType != "docker" {
			t.Errorf("expected sourcetype 'docker', got '%s'", msg.SourceType)
		}
		if msg.Index != "main" {
			t.Errorf("expected index 'main', got '%s'", msg.Index)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message to be received by mock server")
	}
}

func TestSplunkMessageJSONStructure(t *testing.T) {
	event := SplunkMessageEvent{Message: "test log line"}
	msg := SplunkMessage{
		Time:       12345,
		Source:     "stdout",
		SourceType: "docker",
		Index:      "main",
		Hostname:   "myhost",
		Event:      event,
	}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal SplunkMessage: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"test log line"`) {
		t.Errorf("JSON should contain event message, got: %s", s)
	}
	if !strings.Contains(s, `"sourcetype":"docker"`) {
		t.Errorf("JSON should contain sourcetype field, got: %s", s)
	}
}
