package gelf

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Graylog2/go-gelf/gelf"
	"github.com/gliderlabs/logspout/router"
)

func gelfOne(t *testing.T, m *router.Message) (int32, map[string]interface{}) {
	t.Helper()
	mock := &mockGelfWriter{}
	streamAndWait(&Adapter{writer: mock}, m)
	if len(mock.messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(mock.messages))
	}
	var extra map[string]interface{}
	if err := json.Unmarshal(mock.messages[0].RawExtra, &extra); err != nil {
		t.Fatal(err)
	}
	return mock.messages[0].Level, extra
}

func TestGelfUnknownLevelFallsBackToSource(t *testing.T) {
	for _, lvl := range []string{"WARN", "Warning", "bogus", " info"} {
		for src, want := range map[string]int32{"stdout": int32(gelf.LOG_INFO), "stderr": int32(gelf.LOG_ERR)} {
			got, _ := gelfOne(t, &router.Message{Container: newTestContainer(), Data: "x", Source: src, Time: time.Now(), Level: lvl})
			if got != want {
				t.Errorf("level %q source %s: got %d, want %d", lvl, src, got, want)
			}
		}
	}
}

func TestGelfEmptyFieldsEqualsNil(t *testing.T) {
	_, a := gelfOne(t, &router.Message{Container: newTestContainer(), Data: "x", Source: "stdout", Time: time.Now()})
	_, b := gelfOne(t, &router.Message{Container: newTestContainer(), Data: "x", Source: "stdout", Time: time.Now(), Fields: map[string]string{}})
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("nil %s != empty %s", ja, jb)
	}
}

func TestGelfFieldsCollideWithAllBuiltins(t *testing.T) {
	fields := map[string]string{}
	for _, k := range []string{"container_id", "container_name", "image_name", "command"} {
		fields[k] = "evil"
	}
	fields["logger"] = "ok"
	_, extra := gelfOne(t, &router.Message{Container: newTestContainer(), Data: "x", Source: "stdout", Time: time.Now(), Fields: fields})
	for k, v := range extra {
		if v == "evil" {
			t.Errorf("built-in %s overridden by rule field", k)
		}
	}
	if extra["_logger"] != "ok" {
		t.Errorf("_logger missing: %v", extra)
	}
}

func TestGelfFieldsAlreadyUnderscored(t *testing.T) {
	_, extra := gelfOne(t, &router.Message{Container: newTestContainer(), Data: "x", Source: "stdout", Time: time.Now(), Fields: map[string]string{"_x": "1"}})
	if extra["__x"] != "1" {
		t.Errorf("extras: %v", extra)
	}
}
