package pipeline

import (
	"reflect"
	"strings"
	"testing"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/gliderlabs/logspout/router"
)

func compileV1(t *testing.T, disabled ...string) *Compiled {
	t.Helper()
	rules, err := defaultRules("v1", disabled)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Compile(rules)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func defMsg(container, source, data string) *router.Message {
	return &router.Message{Data: data, Source: source, Container: &docker.Container{Name: "/" + container}}
}

type defCase struct {
	container, source, line, level string
	fields                         map[string]string
}

func runDefCases(t *testing.T, c *Compiled, cases []defCase) {
	t.Helper()
	for _, tc := range cases {
		m := defMsg(tc.container, tc.source, tc.line)
		if c.Apply(m, nil) {
			t.Errorf("%s %q: dropped", tc.container, tc.line)
		}
		if m.Level != tc.level {
			t.Errorf("%s %q: level %q, want %q", tc.container, tc.line, m.Level, tc.level)
		}
		if m.Data != tc.line {
			t.Errorf("%s %q: data changed to %q", tc.container, tc.line, m.Data)
		}
		if !reflect.DeepEqual(m.Fields, tc.fields) {
			t.Errorf("%s %q: fields %v, want %v", tc.container, tc.line, m.Fields, tc.fields)
		}
	}
}

func TestDefaultsANSIAndTracebacks(t *testing.T) {
	c := compileV1(t)
	ha := map[string]string{"thread": "MainThread", "logger": "homeassistant.components.mqtt"}
	runDefCases(t, c, []defCase{
		{"homeassistant", "stdout", "\x1b[32m2026-10-04 12:00:00.123 WARNING (MainThread) [homeassistant.components.mqtt] x\x1b[0m", "warning", ha},
		{"homeassistant", "stdout", "\x1b[31m2026-10-04 12:00:00.123 ERROR (MainThread) [homeassistant.components.mqtt] x\x1b[0m", "error", ha},
		{"homeassistant", "stdout", "\x1b[0m\x1b[1m2026-10-04 12:00:00.123 INFO (MainThread) [homeassistant.components.mqtt] x", "info", ha},
		// Traceback continuation lines stay unclassified, on both streams.
		{"homeassistant", "stderr", "Traceback (most recent call last):", "", nil},
		{"homeassistant", "stderr", "  File \"/usr/src/homeassistant/homeassistant/core.py\", line 1, in run", "", nil},
		{"homeassistant", "stderr", "    await self.hass.async_add_executor_job(x)", "", nil},
		{"homeassistant", "stdout", "ValueError: invalid literal for int() with base 10: 'x'", "", nil},
		{"homeassistant", "stderr", "ERROR: something on stderr without timestamp", "", nil},
		{"homeassistant", "stdout", "", "", nil},
		{"hassio_supervisor", "stderr", "  File \"/usr/src/supervisor/supervisor/api/__init__.py\", line 5", "", nil},
		{"hassio_supervisor", "stdout", "26-10-04 12:00:00 CRITICAL (MainThread) [supervisor.core] Fatal", "critical", map[string]string{"thread": "MainThread", "logger": "supervisor.core"}},
		{"addon_core_whisper", "stderr", "  File \"/usr/lib/python3/x.py\", line 1, in <module>", "", nil},
		{"addon_core_whisper", "stderr", "ERROR:wyoming.server:Boom", "error", map[string]string{"logger": "wyoming.server"}},
	})
}

func TestDefaultsUncoveredContainersUntouched(t *testing.T) {
	c := compileV1(t)
	lines := []string{
		"2026-10-04 12:00:00.123 ERROR (MainThread) [x] y",
		"[12:00:00] ERROR: boom",
		"[ERROR] plugin/errors: x",
		"E: [pulseaudio] x",
		"s6-rc: error: service failed",
		"INFO Starting",
		"ERROR:root:x",
		"level=error msg=x",
		`{"level":"error","msg":"x"}`,
		"4 Oct 12:00:00 - [error] [a:b] x",
	}
	for _, name := range []string{"mydb", "nginx", "traefik", "homeassistant2", "my_homeassistant", "hassio", "hassio-dns", "addon", "addons_x", "xaddon_core_ssh", "hassio_unknown_but_not_matching_x", ""} {
		for _, l := range lines {
			m := defMsg(name, "stdout", l)
			if name == "hassio_unknown_but_not_matching_x" {
				// matches hassio_* glob only for s6 lines
				if strings.HasPrefix(l, "s6-") {
					continue
				}
			}
			if c.Apply(m, nil) || m.Level != "" || m.Fields != nil || m.Data != l {
				t.Errorf("%q %q touched: level %q fields %v data %q", name, l, m.Level, m.Fields, m.Data)
			}
		}
	}
	// No container at all.
	m := &router.Message{Data: "2026-10-04 12:00:00.123 ERROR (MainThread) [x] y", Source: "stdout"}
	c.Apply(m, nil)
	if m.Level != "" || m.Fields != nil {
		t.Errorf("nil container touched: %q %v", m.Level, m.Fields)
	}
}

func TestDefaultsCrossFormatFalsePositives(t *testing.T) {
	c := compileV1(t)
	ha := "2026-10-04 12:00:00.123 ERROR (MainThread) [x.y] text"
	bash := "[12:00:00] ERROR: text"
	coredns := "[ERROR] plugin/x: text"
	pulse := "E: [pulseaudio] text"
	nr := "4 Oct 12:00:00 - [error] [a:b] text"
	zw := "2026-10-04 12:00:00.123 ERROR APP: text"
	matter := "2026-10-04 12:00:00.123 (MainThread) ERROR [a.b] text"
	esp := "ERROR text"
	espdev := "[12:00:00][E][wifi:1]: text"
	wy := "ERROR:root:text"
	logfmt := "level=error msg=text"
	none := []defCase{}
	add := func(container string, lines ...string) {
		for _, l := range lines {
			none = append(none, defCase{container, "stdout", l, "", nil})
		}
	}
	// Formats that must NOT classify in these containers.
	add("hassio_dns", ha, pulse, zw, matter, esp, wy)
	add("hassio_audio", ha, coredns, zw, esp, wy)
	add("hassio_multicast", ha, coredns, pulse, zw, wy)
	add("addon_x_zigbee2mqtt", ha, coredns, nr, zw, matter, esp, espdev, wy, pulse)
	add("addon_x_nodered", ha, zw, matter, esp, espdev, wy, pulse, coredns)
	add("addon_x_zwavejs2mqtt", ha, nr, matter, esp, espdev, wy, pulse, coredns)
	add("addon_x_matter_server", ha, nr, zw, esp, espdev, wy, pulse, coredns)
	add("addon_x_whisper", ha, nr, zw, matter, esp, espdev, pulse, coredns)
	add("addon_x_grafana", ha, nr, zw, matter, esp, espdev, wy, pulse, coredns)
	add("addon_x_ssh", ha, nr, zw, matter, esp, espdev, wy, pulse, coredns, logfmt)
	runDefCases(t, c, none)

	// A parser-based rule must not misfire on a line of another format.
	m := defMsg("homeassistant", "stdout", bash)
	c.Apply(m, nil)
	if m.Level != "" {
		t.Errorf("bashio line in homeassistant classified: %q", m.Level)
	}
}

// Every fixture line, fed to every container name used in any fixture, must
// never be dropped and never change the message.
func TestDefaultsPropertyNoDropNoRewrite(t *testing.T) {
	c := compileV1(t)
	var lines, containers []string
	seen := map[string]bool{}
	for _, f := range readDefaultFixtures(t, "v1") {
		lines = append(lines, f.line)
		if !seen[f.container] {
			seen[f.container] = true
			containers = append(containers, f.container)
		}
	}
	lines = append(lines, "", " ", "\x1b[0m", "\x1b", strings.Repeat("x", 1<<16), "\xff\xfe", "[", "[]:", "level=", "\t")
	containers = append(containers, "other")
	for _, ct := range containers {
		for _, src := range []string{"stdout", "stderr"} {
			for _, l := range lines {
				m := defMsg(ct, src, l)
				if c.Apply(m, nil) {
					t.Fatalf("%s %q dropped", ct, l)
				}
				if m.Data != l {
					t.Fatalf("%s %q rewritten to %q", ct, l, m.Data)
				}
			}
		}
	}
}
