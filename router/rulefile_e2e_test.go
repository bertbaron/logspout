package router_test

import (
	"reflect"
	"testing"

	"github.com/gliderlabs/logspout/pipeline"
	"github.com/gliderlabs/logspout/router"
)

// The example of design section 7, with the targets renamed to routes that exist.
const designExample = `
defaults: v1
disable_defaults: [mosquitto-levels]

rules:
  - name: my add-on uses the bashio format
    when: { container: addon_local_myaddon }
    parse: bashio

  - name: zigbee2mqtt levels
    when:
      container: addon_*_zigbee2mqtt
      match: '^\[(?P<time>[^\]]+)\] (?P<level>\w+): (?P<msg>.*)$'
    set:
      level: ${level}
      message: ${msg}

  - name: mosquitto noise
    when:
      container: addon_core_mosquitto
      match: 'New connection from'
    drop: true

targets:
  syslog:
    rules:
      - name: core goes via remote_logger
        when: { container: homeassistant }
        drop: true
  loki:
    rules:
      - name: no debug in loki
        when: { level: '<info' }
        drop: true
      - name: stderr info is a warning
        when: { expr: 'source == "stderr" && level == "info"' }
        set: { level: warning }
`

func TestRuleFileDesignExampleThroughPumps(t *testing.T) {
	res := pipeline.ParseFile([]byte(designExample), pipeline.FileEnv{Routes: []string{"syslog", "loki"}})
	if res.Err() != nil {
		t.Fatal(res.Err())
	}
	for _, w := range res.Warnings {
		t.Logf("warning: %v", w)
	}
	inputs := []input{
		{"addon_local_myaddon", "stdout", "[12:00:00] DEBUG: chatty"},
		{"addon_local_myaddon", "stdout", "[12:00:01] WARNING: careful"},
		{"addon_local_myaddon", "stderr", "[12:00:02] INFO: on stderr"},
		{"addon_45df7312_zigbee2mqtt", "stdout", "[2026-10-04 12:00:00] warn: Device went offline"},
		{"addon_core_mosquitto", "stdout", "1759575600: New connection from 10.0.0.5:4455 on port 1883."},
		{"addon_core_mosquitto", "stdout", "1759575601: Client closed its connection."},
		{"homeassistant", "stdout", haInfo},
	}
	type want struct{ data, level string }
	wantSyslog := []want{
		{"[12:00:00] DEBUG: chatty", "debug"},
		{"[12:00:01] WARNING: careful", "warning"},
		{"[12:00:02] INFO: on stderr", "info"},
		{"Device went offline", "warning"},
		{"1759575601: Client closed its connection.", ""},
	}
	wantLoki := []want{
		{"[12:00:01] WARNING: careful", "warning"},
		{"[12:00:02] INFO: on stderr", "warning"},
		{"Device went offline", "warning"},
		{"1759575601: Client closed its connection.", ""},
		{haInfo, "info"},
	}
	for name, mk := range pumpHarnesses {
		t.Run(name, func(t *testing.T) {
			install(t, res.Config.Apply(pipeline.Options{}))
			h := mk(t)
			chans := map[string]chan *router.Message{"syslog": h.route("syslog"), "loki": h.route("loki")}
			for _, in := range inputs {
				h.feed(in.container, in.source, in.line)
			}
			type key struct{ c, s string }
			seen := map[key]bool{}
			for _, in := range inputs {
				if k := (key{in.container, in.source}); !seen[k] {
					seen[k] = true
					h.feed(in.container, in.source, sentinel)
				}
			}
			// The syslog target drops everything of homeassistant, also the sentinel.
			got := map[string][]*router.Message{
				"syslog": collect(t, chans["syslog"], len(seen)-1),
				"loki":   collect(t, chans["loki"], len(seen)),
			}
			conv := func(ms []*router.Message) []want {
				var out []want
				for _, m := range flatten(ms) {
					out = append(out, want{m.Data, m.Level})
				}
				return out
			}
			asSet := func(w []want) map[want]bool {
				s := map[want]bool{}
				for _, x := range w {
					s[x] = true
				}
				return s
			}
			if g := conv(got["syslog"]); !reflect.DeepEqual(asSet(g), asSet(wantSyslog)) || len(g) != len(wantSyslog) {
				t.Errorf("syslog:\n got %+v\nwant %+v", g, wantSyslog)
			}
			if g := conv(got["loki"]); !reflect.DeepEqual(asSet(g), asSet(wantLoki)) || len(g) != len(wantLoki) {
				t.Errorf("loki:\n got %+v\nwant %+v", g, wantLoki)
			}
		})
	}
}
