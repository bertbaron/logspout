package loki

import (
	"testing"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/gliderlabs/logspout/router"
)

// Journal messages have no image id, command or created time.
func TestLokiLabelsJournalCompat(t *testing.T) {
	m := &router.Message{
		Container: &docker.Container{ID: "abc", Name: "/c", Config: &docker.Config{Image: "img"}},
		Source:    "journal",
	}
	a := &LokiAdapter{hostname: "h"}
	got := a.labels(m)
	if len(got) != 4 {
		t.Errorf("expected 4 labels, got %v", got)
	}
	for _, k := range []string{"image_id", "command", "created", "level"} {
		if _, ok := got[k]; ok {
			t.Errorf("unexpected label %s", k)
		}
	}
	a.levelLabel = true
	m.Level = "bogus"
	if got := a.labels(m); len(got) != 5 {
		t.Errorf("level label expected verbatim when set, got %v", got)
	}
}
