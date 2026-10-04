package ingress

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gliderlabs/logspout/pipeline"
	"golang.org/x/net/websocket"
)

// hasPath follows a dotted JSON path; "[]" takes the first array element.
func hasPath(v any, path string) bool {
	for _, p := range strings.Split(path, ".") {
		if p == "[*]" { // must be an array (it may be empty): app.js calls array methods on it
			_, ok := v.([]any)
			return ok
		}
		if p == "[]" {
			a, ok := v.([]any)
			if !ok || len(a) == 0 {
				return false
			}
			v = a[0]
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		if v, ok = m[p]; !ok {
			return false
		}
	}
	return true
}

func checkPaths(t *testing.T, what string, body []byte, paths ...string) {
	t.Helper()
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("%s: %v\n%s", what, err, body)
	}
	for _, p := range paths {
		if !hasPath(v, p) {
			t.Errorf("%s: app.js reads %q, the server does not send it\n%s", what, p, body)
		}
	}
}

var traceStagePaths = []string{"rules", "level", "effective_level", "message", "dropped"}

// The JSON paths that ui/app.js reads. A rename on the server side breaks this test.
func TestUIContractFieldNames(t *testing.T) {
	f := newFixture(t, pipeline.Options{})

	rec := f.do("GET", "/api/config", "")
	checkPaths(t, "GET /api/config", rec.Body.Bytes(), "path", "content")

	rec = f.do("GET", "/api/status", "")
	checkPaths(t, "GET /api/status", rec.Body.Bytes(),
		"defaults.active", "defaults.latest", "defaults.newer_available",
		"routes.[*]", "routes.[].name", "routes.[].ambiguous", "routes.[].rules",
		"rules.defaults", "rules.global", "rules.excluded", "rules.summary",
		"file.exists", "file.valid", "file.errors.[*]", "rules.excluded.[*]", "debug_pipeline")

	bad := "rules:\n  - name: x\n    when: { match: '(' }\n    drop: true\n"
	rec = f.do("POST", "/api/validate", jsonBody(t, bad))
	if rec.Code != 200 {
		t.Fatalf("validate %d", rec.Code)
	}
	checkPaths(t, "POST /api/validate", rec.Body.Bytes(), "valid", "errors.[].message", "errors.[].line", "errors.[].column", "warnings")

	rec = f.do("PUT", "/api/config", jsonBody(t, bad))
	if rec.Code != 422 {
		t.Fatalf("PUT invalid: %d", rec.Code)
	}
	checkPaths(t, "PUT /api/config 422", rec.Body.Bytes(), "errors.[].message", "errors.[].line", "warnings")

	rec = f.do("PUT", "/api/config", jsonBody(t, dropNoisy))
	if rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	// 409 uses the same body as 200 (putConfig).
	checkPaths(t, "PUT /api/config 200/409", rec.Body.Bytes(), "applied", "summary", "errors", "warnings")

	rec = f.do("PUT", "/api/config", `{}`)
	checkPaths(t, "error body", rec.Body.Bytes(), "error")

	msgs := []map[string]any{{"container": "c1", "data": "noisy"}, {"container": "c1", "data": "ERR 5"}}
	rec = f.do("POST", "/api/test", testBody(t, draftRules, msgs))
	if rec.Code != 200 {
		t.Fatalf("test: %d %s", rec.Code, rec.Body)
	}
	paths := []string{"warnings", "results.[*]", "results.[].targets.[*]", "results.[].global.rules.[*]", "results.[].targets.[].rules.[*]", "results.[].input.container", "results.[].input.source", "results.[].input.data",
		"results.[].dropped", "results.[].global.excluded", "results.[].targets.[].name", "results.[].targets.[].sent"}
	for _, s := range traceStagePaths {
		paths = append(paths, "results.[].global."+s, "results.[].targets.[]."+s)
	}
	paths = append(paths, "results.[].global.rules.[].name", "results.[].global.rules.[].matched",
		"results.[].global.dropped_by.name", "results.[].global.rules.[].actions")
	checkPaths(t, "POST /api/test", rec.Body.Bytes(), paths...)

	var tr struct {
		Results []map[string]any `json:"results"`
	}
	must(t, json.Unmarshal(rec.Body.Bytes(), &tr))
	checkPaths(t, "match groups", mustMarshal(t, tr.Results[1]), "global.rules.[].name")
	grp := false
	for _, r := range tr.Results[1]["global"].(map[string]any)["rules"].([]any) {
		if r.(map[string]any)["groups"] != nil {
			grp = true
		}
	}
	if !grp {
		t.Error("rules[].groups missing for a matching named group (regex tester)")
	}

	rec = f.do("POST", "/api/test", testBody(t, bad, nil))
	if rec.Code != 422 {
		t.Fatalf("test invalid: %d", rec.Code)
	}
	checkPaths(t, "POST /api/test 422", rec.Body.Bytes(), "errors.[].message", "warnings")

	rec = f.do("GET", "/api/samples", "")
	checkPaths(t, "GET /api/samples", rec.Body.Bytes(), "samples.[*]")

	b, _ := json.Marshal(overflow{"overflow", 3})
	checkPaths(t, "overflow frame", b, "type", "dropped")
	if !strings.Contains(string(b), `"dropped":3`) || !strings.Contains(string(b), `"type":"overflow"`) {
		t.Errorf("overflow frame %s", b)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	return b
}

func TestUIContractLiveFrame(t *testing.T) {
	l := startLive(t, "127.0.0.1")
	ws := l.connect(t, "?show_dropped=true")
	pump(cmsg("c1", "stdout", "noisy line"))
	var raw json.RawMessage
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	must(t, websocket.JSON.Receive(ws, &raw))
	paths := []string{"input.container", "input.source", "input.data", "dropped", "global.message", "global.level",
		"global.effective_level", "targets.[].name", "targets.[].sent"}
	checkPaths(t, "live frame", raw, paths...)
	var f map[string]any
	must(t, json.Unmarshal(raw, &f))
	if _, ok := f["type"]; ok {
		t.Error("a result frame must not have a type: app.js tells overflow frames apart by it")
	}
}

// jsString is JSON.stringify of JavaScript: no HTML escaping, and U+2028/U+2029 stay raw.
func jsString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	out := strings.TrimSuffix(buf.String(), "\n") // U+2028 and U+2029 are already \u escapes
	var sb strings.Builder
	for _, r := range out {
		if (r >= 0x7f && r <= 0x9f) || r == 0xfffe || r == 0xffff {
			fmt.Fprintf(&sb, `\u%04x`, r)
		} else {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// The regex tester of app.js builds its draft like this.
func regexDraft(re string) string {
	return "defaults: off\nrules:\n  - name: t\n    when: { match: " + jsString(re) + " }\n    stop: true\n"
}

func TestUIRegexTesterDraftIsValidYAML(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	cases := []struct{ name, re, text string }{
		{"double quote", `say "(?P<w>\w+)"`, `say "hi"`},
		{"single quote", `it's (?P<w>\w+)`, `it's fine`},
		{"backslashes", `C:\\Users\\(?P<u>\w+)`, `C:\Users\bert`},
		{"colon space", `key: (?P<v>\w+)`, `key: val`},
		{"hash", `a #(?P<v>\w+)`, `a #tag`},
		{"hash after space in group", `(?P<v>x) # y`, `x # y`},
		{"flow chars", `{(?P<v>[a-z]+)}, x\]`, `{abc}, x]`},
		{"escaped newline", `a\nb(?P<v>c)`, "a\nbc"},
		{"literal newline", "a\nb(?P<v>c)", "a\nbc"},
		{"tab", "a\tb(?P<v>c)", "a\tbc"},
		{"leading special", `- (?P<v>x)`, `- x`},
		{"percent star amp", `%*&!@(?P<v>x)`, `%*&!@x`},
		{"unicode", `é(?P<v>ü)`, `éü`},
		{"line separator", "a\u2028(?P<v>b)", "a\u2028b"},
		{"paragraph separator", "a\u2029(?P<v>b)", "a\u2029b"},
		{"html chars", `<b>(?P<v>x)&`, `<b>x&`},
		{"control char", "a\x01(?P<v>b)", "a\x01b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := f.do("POST", "/api/test", testBody(t, regexDraft(c.re), []map[string]any{{"data": c.text}}))
			if rec.Code != 200 {
				t.Fatalf("draft %q: %d %s", regexDraft(c.re), rec.Code, rec.Body)
			}
			var r testResponse
			must(t, json.Unmarshal(rec.Body.Bytes(), &r))
			var got *pipeline.RuleTrace
			for i, rt := range r.Results[0].Global.Rules {
				if rt.Name == "t" {
					got = &r.Results[0].Global.Rules[i]
				}
			}
			if got == nil || !got.Matched {
				t.Fatalf("rule t did not match %q with regex %q: %+v", c.text, c.re, r.Results[0].Global.Rules)
			}
			if strings.Contains(c.re, "(?P<") {
				for _, v := range got.Groups {
					if v == "" {
						t.Errorf("empty group in %v", got.Groups)
					}
				}
			}
		})
	}
}

func TestUIRegexTesterInvalidRegexIs422WithMessage(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	rec := f.do("POST", "/api/test", testBody(t, regexDraft(`(unclosed`), []map[string]any{{"data": "x"}}))
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"message"`) {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

// The tester shows the 422 of the draft; these inputs cannot make a valid draft.
func TestUIRegexTesterRejectedInputs(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	for name, re := range map[string]string{"empty": ""} {
		rec := f.do("POST", "/api/test", testBody(t, regexDraft(re), []map[string]any{{"data": "x"}}))
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"message"`) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

// A raw U+0085 in a YAML double quoted scalar is folded, so app.js escapes U+007F-U+009F.
func TestUIRegexTesterRawNEL(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	for _, c := range []string{"\u0085", "\x7f", "\u009f", "\u2028", "\u2029", "\ufffe", "\uffff"} {
		rec := f.do("POST", "/api/test", testBody(t, regexDraft("a"+c+"(?P<v>b)"), []map[string]any{{"data": "a" + c + "b"}}))
		var r testResponse
		must(t, json.Unmarshal(rec.Body.Bytes(), &r))
		if rec.Code != 200 || len(r.Results[0].Global.Rules) == 0 || !r.Results[0].Global.Rules[0].Matched {
			t.Errorf("regex with %U did not match its own text: %d %s", []rune(c)[0], rec.Code, rec.Body)
		}
	}
}

// A rule whose expr fails at run time is not matched but carries an error; the Playground must show it.
func TestUIContractRuleRuntimeError(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	draft := "defaults: off\nrules:\n  - name: boom\n    when: { expr: 'int(message) > 1' }\n    drop: true\n"
	rec := f.do("POST", "/api/test", testBody(t, draft, []map[string]any{{"data": "not a number"}}))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	checkPaths(t, "rule error", rec.Body.Bytes(), "results.[].global.rules.[].error")
}
