package splunk

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gliderlabs/logspout/router"
)

func sendHEC(t *testing.T, m *router.Message) string {
	t.Helper()
	received := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
	}))
	defer srv.Close()

	timeout := 50 * time.Millisecond
	a := &SplunkAdapter{
		route:            &router.Route{Adapter: "splunk", Address: srv.Listener.Addr().String()},
		url:              srv.URL,
		client:           srv.Client(),
		buffer:           make([]*router.Message, 0, 1),
		timer:            time.NewTimer(timeout),
		capacity:         1,
		timeout:          timeout,
		splunkIndex:      "main",
		splunkSourcetype: "docker",
	}
	stream := make(chan *router.Message, 1)
	go a.Stream(stream)
	stream <- m

	select {
	case body := <-received:
		return body
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HEC request")
	}
	return ""
}

func hecMessage(level string, fields map[string]string) *router.Message {
	c := newTestContainer()
	c.Config.Hostname = "h"
	return &router.Message{
		Container: c, Data: "x", Source: "stdout",
		Time: time.Unix(1700000000, 0), Level: level, Fields: fields,
	}
}

// Byte-for-byte output of the pre-pipeline struct: no level and no fields keys.
func TestSplunkCompatNoLevelNoFields(t *testing.T) {
	want := `{"time":1700000000,"source":"stdout","sourcetype":"docker","index":"main","host":"h","event":{"message":"x","labels":null}}`
	for name, f := range map[string]map[string]string{"nil": nil, "empty": {}} {
		if got := sendHEC(t, hecMessage("", f)); got != want {
			t.Errorf("%s fields: got %s\nwant %s", name, got, want)
		}
	}
}

func TestSplunkLevelAndFields(t *testing.T) {
	want := `{"time":1700000000,"source":"stdout","sourcetype":"docker","index":"main","host":"h","event":{"message":"x","labels":null,"level":"warning"},"fields":{"logger":"core"}}`
	if got := sendHEC(t, hecMessage("warning", map[string]string{"logger": "core"})); got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}
