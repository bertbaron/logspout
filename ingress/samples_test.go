package ingress

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gliderlabs/logspout/pipeline"
	"github.com/gliderlabs/logspout/router"
)

func cmsg(container, source, data string) *router.Message {
	m := wmsg(data)
	m.Container.Name = "/" + container
	m.Source = source
	return m
}

func TestRingWrapsAndKeepsOrder(t *testing.T) {
	var r ring
	if got := r.snapshot(); len(got) != 0 {
		t.Fatalf("empty ring: %d", len(got))
	}
	for i := 0; i < 3; i++ {
		r.add(Sample{Data: fmt.Sprint(i)})
	}
	if got := r.snapshot(); len(got) != 3 || got[0].Data != "0" || got[2].Data != "2" {
		t.Fatalf("partial: %+v", got)
	}
	for i := 3; i < 1234; i++ {
		r.add(Sample{Data: fmt.Sprint(i)})
	}
	got := r.snapshot()
	if len(got) != SampleCount {
		t.Fatalf("len %d, want %d", len(got), SampleCount)
	}
	for i, s := range got {
		if want := fmt.Sprint(1234 - SampleCount + i); s.Data != want {
			t.Fatalf("index %d: %q, want %q", i, s.Data, want)
		}
	}
	// Exactly full, next == 0.
	var r2 ring
	for i := 0; i < SampleCount; i++ {
		r2.add(Sample{Data: fmt.Sprint(i)})
	}
	if got := r2.snapshot(); got[0].Data != "0" || got[SampleCount-1].Data != fmt.Sprint(SampleCount-1) {
		t.Fatalf("full ring order: %q .. %q", got[0].Data, got[SampleCount-1].Data)
	}
}

func TestSampleDataCapAndCopy(t *testing.T) {
	long := strings.Repeat("é", MaxSampleData) // 2 bytes per rune, cut falls inside a rune for odd limits
	tests := []struct {
		name      string
		data      string
		truncated bool
	}{
		{"short", "hello", false},
		{"exact", strings.Repeat("a", MaxSampleData), false},
		{"one over", strings.Repeat("a", MaxSampleData+1), true},
		{"multibyte", long, true},
		{"cut inside rune", "a" + long, true},
	}
	for _, tt := range tests {
		s := newSample(&router.Message{Data: tt.data})
		if len(s.Data) > MaxSampleData || s.Truncated != tt.truncated || !utf8.ValidString(s.Data) {
			t.Errorf("%s: len %d truncated %t valid %t", tt.name, len(s.Data), s.Truncated, utf8.ValidString(s.Data))
		}
		if !tt.truncated && s.Data != tt.data {
			t.Errorf("%s: data changed", tt.name)
		}
	}

	// The buffer keeps its own copy: later changes to the shared message do not show.
	h := &hub{routes: func() []string { return nil }}
	m := cmsg("c1", "stdout", "original")
	m.Fields = map[string]string{"a": "b"}
	h.Observe(m, nil)
	m.Data, m.Level, m.Container.Name = "changed", "error", "/other"
	got := h.ring.snapshot()
	if len(got) != 1 || got[0].Data != "original" || got[0].Level != "" || got[0].Container != "c1" {
		t.Fatalf("sample is not a copy: %+v", got)
	}
}

func TestTraceLinesAreNotCaptured(t *testing.T) {
	h := &hub{routes: func() []string { return nil }}
	h.Observe(cmsg("logspout", "stdout", "2025/01/01 pipeline trace: global container=x"), nil)
	h.Observe(cmsg("c1", "stdout", "real"), nil)
	if got := h.ring.snapshot(); len(got) != 1 || got[0].Data != "real" {
		t.Fatalf("%+v", got)
	}
}

func TestCaptureOnlyWithRunningListener(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	router.SetTap(nil)
	t.Cleanup(func() { router.SetTap(nil) })
	// Serving requests through Handler does not install the tap.
	f.do("GET", "/api/samples", "")
	f.do("POST", "/api/test", jsonBody(t, ""))
	if router.CurrentTap() != nil {
		t.Fatal("tap installed without a listener")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	done := make(chan error, 1)
	go func() { done <- f.srv.Serve(ln) }()
	waitFor(t, func() bool { return router.CurrentTap() != nil })
	f.srv.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if router.CurrentTap() != nil {
		t.Fatal("tap still installed after Close")
	}
	// A Serve after Close must not install it again.
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	must(t, f.srv.Serve(ln2))
	if router.CurrentTap() != nil {
		t.Fatal("tap installed by a Serve after Close")
	}
}

func TestSamplesEndpoint(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	h := f.srv.samples()
	m := cmsg("c1", "stderr", "first")
	m.Container.Config.Image = "img:1"
	h.Observe(m, nil)
	h.Observe(cmsg("c2", "stdout", "second"), nil)

	rec := f.do("GET", "/api/samples", "")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	body := decode(t, rec)
	if body["capacity"] != float64(SampleCount) {
		t.Errorf("capacity %v", body["capacity"])
	}
	list := body["samples"].([]any)
	if len(list) != 2 {
		t.Fatalf("%v", list)
	}
	first := list[0].(map[string]any)
	if first["container"] != "c1" || first["source"] != "stderr" || first["data"] != "first" || first["image"] != "img:1" || first["level"] != "" || first["time"] == nil {
		t.Errorf("first: %v", first)
	}
	if list[1].(map[string]any)["data"] != "second" {
		t.Errorf("order: %v", list)
	}
	if rec := f.do("POST", "/api/samples", ""); rec.Code != 405 {
		t.Errorf("POST: %d", rec.Code)
	}
	if rec := f.doFrom("127.0.0.1:1", "GET", "/api/samples", "", nil); rec.Code != 403 {
		t.Errorf("forbidden: %d", rec.Code)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	return string(b)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func testBody(t *testing.T, content string, msgs []map[string]any) string {
	t.Helper()
	b := map[string]any{"content": content}
	if msgs != nil {
		b["messages"] = msgs
	}
	return mustJSON(t, b)
}

type testResponse struct {
	Routes  []string `json:"routes"`
	Results []result `json:"results"`
}

func (r result) target(name string) pipeline.TargetTrace {
	for _, tt := range r.Targets {
		if tt.Name == name {
			return tt
		}
	}
	return pipeline.TargetTrace{}
}

const draftRules = `
rules:
  - name: drop-noisy
    when: { match: noisy }
    drop: true
  - name: errors
    when: { match: 'ERR (?P<code>\d+)' }
    set: { level: error, message: 'failed ${code}' }
targets:
  gelf:
    rules:
      - name: gelf-no-debug
        when: { level: debug }
        drop: true
  syslog:
    rules:
      - name: tag
        when: { match: failed }
        set: { fields.tag: yes }
`

func TestTestEndpoint(t *testing.T) {
	f := newFixture(t, pipeline.Options{ExcludeContainers: []string{"excl*"}})
	msgs := []map[string]any{
		{"container": "c1", "data": "this is noisy"},
		{"container": "c1", "data": "ERR 42 boom", "source": "stderr"},
		{"container": "c1", "data": "plain", "level": "debug"},
		{"container": "excluded1", "data": "hello"},
	}
	rec := f.do("POST", "/api/test", testBody(t, draftRules, msgs))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v\n%s", rec.Code, rec.Header(), rec.Body)
	}
	var resp testResponse
	must(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	if !reflect.DeepEqual(resp.Routes, []string{"gelf", "syslog", "dup"}) || len(resp.Results) != 4 {
		t.Fatalf("routes %v, %d results", resp.Routes, len(resp.Results))
	}

	// Dropped by a global rule: no target gets it.
	r := resp.Results[0]
	if !r.Dropped || !r.Global.Dropped || r.Global.Excluded || r.Global.DroppedBy == nil || r.Global.DroppedBy.Name != "drop-noisy" || r.Global.DroppedBy.Index != 0 {
		t.Errorf("noisy: %+v", r.Global)
	}
	for _, tt := range r.Targets {
		if tt.Sent {
			t.Errorf("noisy sent to %s", tt.Name)
		}
	}

	// Rewritten by a global rule, groups, per target outcome.
	r = resp.Results[1]
	if r.Input.Data != "ERR 42 boom" || r.Input.Container != "c1" || r.Dropped || r.Global.Dropped {
		t.Fatalf("err: %+v", r)
	}
	if r.Global.Level != "error" || r.Global.Message != "failed 42" {
		t.Errorf("global result: level %q message %q", r.Global.Level, r.Global.Message)
	}
	var matched []pipeline.RuleTrace
	for _, rt := range r.Global.Rules {
		if rt.Matched {
			matched = append(matched, rt)
		}
	}
	if len(matched) != 1 || matched[0].Name != "errors" || matched[0].List != "rules" || matched[0].Groups["code"] != "42" {
		t.Errorf("matched rules: %+v", matched)
	}
	for _, name := range []string{"gelf", "syslog", "dup"} {
		if !r.target(name).Sent {
			t.Errorf("err not sent to %s", name)
		}
	}
	if sys := r.target("syslog"); sys.Fields["tag"] != "yes" || sys.Message != "failed 42" || sys.Level != "error" {
		t.Errorf("syslog: %+v", sys)
	}
	if gelf := r.target("gelf"); gelf.Fields != nil {
		t.Errorf("gelf got fields of syslog: %+v", gelf.Fields)
	}

	// Dropped by one target only.
	r = resp.Results[2]
	gelf := r.target("gelf")
	if r.Dropped || gelf.Sent || !gelf.Dropped || gelf.DroppedBy == nil || gelf.DroppedBy.Name != "gelf-no-debug" || gelf.DroppedBy.List != "targets/gelf" {
		t.Errorf("gelf: %+v", gelf)
	}
	if !r.target("syslog").Sent || !r.target("dup").Sent {
		t.Errorf("other targets: %+v", r.Targets)
	}

	// Excluded by the add-on option.
	r = resp.Results[3]
	if !r.Global.Excluded || !r.Dropped || r.Global.DroppedBy == nil || r.Global.DroppedBy.List != "exclude_containers" {
		t.Errorf("excluded: %+v", r.Global)
	}
}

func TestTestEndpointUsesSamples(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	f.srv.samples().Observe(cmsg("c1", "stdout", "noisy one"), nil)
	f.srv.samples().Observe(cmsg("c1", "stdout", "fine"), nil)
	for name, body := range map[string]string{
		"absent": testBody(t, draftRules, nil),
		"null":   mustJSON(t, map[string]any{"content": draftRules, "messages": nil}),
	} {
		var resp testResponse
		rec := f.do("POST", "/api/test", body)
		must(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		if rec.Code != 200 || len(resp.Results) != 2 || !resp.Results[0].Dropped || resp.Results[1].Dropped {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	// An empty list is not "use the samples".
	var resp testResponse
	rec := f.do("POST", "/api/test", testBody(t, draftRules, []map[string]any{}))
	must(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	if rec.Code != 200 || len(resp.Results) != 0 {
		t.Errorf("empty list: %d %s", rec.Code, rec.Body)
	}
}

func TestTestEndpointErrors(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	many := make([]map[string]any, MaxTestMessages+1)
	for i := range many {
		many[i] = map[string]any{"data": "x"}
	}
	atLimit := many[:MaxTestMessages]
	tests := []struct {
		name string
		body string
		want int
	}{
		{"invalid draft", testBody(t, "rules:\n  - when: { match: '(' }\n    drop: true\n", nil), 422},
		{"unknown target", testBody(t, "targets:\n  nope:\n    rules:\n      - when: { match: x }\n        drop: true\n", nil), 422},
		{"not JSON", "nope", 400},
		{"content missing", `{"messages": []}`, 400},
		{"data missing", testBody(t, "", []map[string]any{{"container": "c"}}), 400},
		{"too many messages", testBody(t, "", many), 413},
		{"at the limit", testBody(t, "", atLimit), 200},
		{"draft too large", testBody(t, strings.Repeat("#", pipeline.MaxFileSize+1), nil), 413},
		{"body too large", `{"content": "` + strings.Repeat("a", maxTestBody) + `"}`, 413},
	}
	for _, tt := range tests {
		rec := f.do("POST", "/api/test", tt.body)
		if rec.Code != tt.want {
			t.Errorf("%s: %d, want %d: %.200s", tt.name, rec.Code, tt.want, rec.Body)
		}
	}
	m := decode(t, f.do("POST", "/api/test", tests[0].body))
	errs, _ := m["errors"].([]any)
	if len(errs) == 0 || errs[0].(map[string]any)["message"] == "" {
		t.Errorf("422 without errors: %v", m)
	}
	// Same verdict as validate and PUT.
	if rec := f.do("PUT", "/api/config", jsonBody(t, "rules:\n  - when: { match: '(' }\n    drop: true\n")); rec.Code != 422 {
		t.Errorf("PUT of the same draft: %d", rec.Code)
	}
	for _, method := range []string{"GET", "PUT"} {
		if rec := f.do(method, "/api/test", ""); rec.Code != 405 || rec.Header().Get("Allow") != "POST" {
			t.Errorf("%s: %d %q", method, rec.Code, rec.Header().Get("Allow"))
		}
	}
	if rec := f.doFrom("10.1.1.1:1", "POST", "/api/test", testBody(t, "", nil), nil); rec.Code != 403 {
		t.Errorf("forbidden: %d", rec.Code)
	}
}

func TestTestEndpointLeavesActivePipelineAndFile(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	if rec := f.do("PUT", "/api/config", jsonBody(t, dropNoisy)); rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	before := router.CurrentProcessor()
	file, err := os.ReadFile(f.path)
	must(t, err)
	st := f.srv.Watcher.Status()

	rec := f.do("POST", "/api/test", testBody(t, draftRules, []map[string]any{{"container": "c1", "data": "quiet"}, {"container": "c1", "data": "noisy"}}))
	if rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	if router.CurrentProcessor() != before || f.srv.Watcher.Status().Pipeline != st.Pipeline {
		t.Error("the active pipeline changed")
	}
	if after, _ := os.ReadFile(f.path); string(after) != string(file) {
		t.Errorf("file changed: %q", after)
	}
	// The active rules still drop "noisy"; the draft only dropped it in the test.
	if !activeDrops("noisy") || activeDrops("quiet") {
		t.Error("active rules changed")
	}
	if f.do("POST", "/api/test", testBody(t, "", nil)).Code != 200 || router.CurrentProcessor() != before {
		t.Error("empty draft changed the pipeline")
	}
}
