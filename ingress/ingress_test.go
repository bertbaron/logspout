package ingress

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/gliderlabs/logspout/pipeline"
	"github.com/gliderlabs/logspout/router"
)

const (
	dropNoisy = "rules:\n  - name: noisy\n    when: { match: noisy }\n    drop: true\n"
	dropQuiet = "rules:\n  - name: quiet\n    when: { match: quiet }\n    drop: true\n"
)

const testIP = "172.30.32.2"

type fixture struct {
	t    *testing.T
	srv  *Server
	path string
}

func newFixture(t *testing.T, base pipeline.Options) *fixture {
	t.Helper()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr); router.SetProcessor(nil) })
	router.SetProcessor(nil)
	path := filepath.Join(t.TempDir(), "sub", "logspout.yaml") // sub does not exist yet
	w := &pipeline.Watcher{
		Path: path,
		Env:  pipeline.FileEnv{Routes: []string{"gelf", "syslog", "dup"}, Ambiguous: []string{"dup"}},
		Base: base,
	}
	t.Cleanup(w.Stop)
	w.Reload()
	return &fixture{t: t, srv: &Server{Watcher: w, AllowedIP: testIP}, path: path}
}

func (f *fixture) do(method, target, body string) *httptest.ResponseRecorder {
	return f.doFrom(testIP+":4000", method, target, body, nil)
}

func (f *fixture) doFrom(remote, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.RemoteAddr = remote
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, r)
	return rec
}

func jsonBody(t *testing.T, content string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, rec.Body.String())
	}
	return m
}

func wmsg(data string) *router.Message {
	return &router.Message{
		Container: &docker.Container{Name: "/c1", Config: &docker.Config{}},
		Source:    "stdout", Data: data,
	}
}

func activeDrops(data string) bool {
	p := router.CurrentProcessor()
	return p != nil && p.Global(wmsg(data))
}

func TestRemoteAddressRestriction(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	tests := []struct {
		name   string
		remote string
		hdr    map[string]string
		want   int
	}{
		{"supervisor", testIP + ":1234", nil, 200},
		{"supervisor as IPv4-mapped IPv6", "[::ffff:172.30.32.2]:1234", nil, 200},
		{"other IP", "172.30.32.3:1234", nil, 403},
		{"loopback", "127.0.0.1:1234", nil, 403},
		{"IPv6", "[2001:db8::1]:1234", nil, 403},
		{"IPv6 loopback", "[::1]:1234", nil, 403},
		{"no port", testIP, nil, 403},
		{"garbage", "nonsense", nil, 403},
		{"X-Forwarded-For does not grant", "10.0.0.1:1234", map[string]string{"X-Forwarded-For": testIP}, 403},
		{"X-Real-IP does not grant", "10.0.0.1:1234", map[string]string{"X-Real-IP": testIP, "Forwarded": "for=" + testIP}, 403},
	}
	for _, tt := range tests {
		for _, path := range []string{"/", "/api/status", "/api/config", "/nothing"} {
			rec := f.doFrom(tt.remote, "GET", path, "", tt.hdr)
			want := tt.want
			if want == 200 && path == "/nothing" {
				want = 404
			}
			if rec.Code != want {
				t.Errorf("%s %s: %d, want %d", tt.name, path, rec.Code, want)
			}
		}
	}
	// A forbidden PUT must not touch the file.
	rec := f.doFrom("10.0.0.1:1", "PUT", "/api/config", jsonBody(t, dropNoisy), nil)
	if _, err := os.Stat(f.path); rec.Code != 403 || err == nil {
		t.Errorf("forbidden PUT: %d, file err %v", rec.Code, err)
	}
}

func TestDefaultAllowedIP(t *testing.T) {
	s := &Server{Watcher: &pipeline.Watcher{Path: filepath.Join(t.TempDir(), "x.yaml")}}
	for remote, want := range map[string]int{"172.30.32.2:1": 200, "172.30.32.4:1": 403} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", remote, rec.Code, want)
		}
	}
}

func TestRoutingAndHeaders(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	tests := []struct {
		method, path string
		want         int
		allow        string
	}{
		{"GET", "/", 200, ""},
		{"POST", "/", 405, "GET, HEAD"},
		{"GET", "/api/config", 200, ""},
		{"DELETE", "/api/config", 405, "GET, PUT"},
		{"POST", "/api/config", 405, "GET, PUT"},
		{"GET", "/api/validate", 405, "POST"},
		{"POST", "/api/status", 405, "GET"},
		{"PUT", "/api/status", 405, "GET"},
		{"GET", "/api/unknown", 404, ""},
		{"GET", "/api", 404, ""},
		{"GET", "/api/config/", 404, ""},
		{"GET", "/other", 404, ""},
	}
	for _, tt := range tests {
		rec := f.do(tt.method, tt.path, "")
		if rec.Code != tt.want || rec.Header().Get("Allow") != tt.allow {
			t.Errorf("%s %s: %d allow %q, want %d %q", tt.method, tt.path, rec.Code, rec.Header().Get("Allow"), tt.want, tt.allow)
		}
		if strings.HasPrefix(tt.path, "/api") && rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s: Cache-Control %q", tt.method, tt.path, rec.Header().Get("Cache-Control"))
		}
		if strings.HasPrefix(tt.path, "/api") && rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s %s: Content-Type %q", tt.method, tt.path, rec.Header().Get("Content-Type"))
		}
	}
	rec := f.do("GET", "/", "")
	if !strings.Contains(rec.Body.String(), "Logspout") || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("placeholder: %q %q", rec.Header().Get("Content-Type"), rec.Body.String())
	}
}

func TestGetConfig(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	m := decode(t, f.do("GET", "/api/config", ""))
	if m["path"] != f.path || m["exists"] != false || m["content"] != "" {
		t.Fatalf("missing file: %v", m)
	}
	must(t, os.MkdirAll(filepath.Dir(f.path), 0o755))
	must(t, os.WriteFile(f.path, []byte(dropNoisy), 0o644))
	m = decode(t, f.do("GET", "/api/config", ""))
	if m["exists"] != true || m["content"] != dropNoisy {
		t.Fatalf("existing file: %v", m)
	}
}

func TestGetConfigUnreadable(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	must(t, os.MkdirAll(f.path, 0o755)) // a directory where the file should be
	rec := f.do("GET", "/api/config", "")
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "not a regular file") {
		t.Errorf("directory: %d %s", rec.Code, rec.Body)
	}
	must(t, os.Remove(f.path))
	must(t, os.WriteFile(f.path, bytes.Repeat([]byte("#"), pipeline.MaxFileSize+1), 0o644))
	rec = f.do("GET", "/api/config", "")
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "too large") {
		t.Errorf("large file: %d %s", rec.Code, rec.Body)
	}
}

func TestValidate(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	tests := []struct {
		name, body string
		code       int
		valid      bool
		errs, warn int
	}{
		{"valid", jsonBody(t, dropNoisy), 200, true, 0, 0},
		{"empty is valid", jsonBody(t, ""), 200, true, 0, 0},
		{"unknown key", jsonBody(t, "rulez: []\n"), 200, false, 1, 0},
		{"unknown target", jsonBody(t, "targets:\n  nope:\n    rules: []\n"), 200, false, 1, 0},
		{"ambiguous target", jsonBody(t, "targets:\n  dup:\n    rules: []\n"), 200, false, 1, 0},
		{"known target", jsonBody(t, "targets:\n  gelf:\n    rules: []\n"), 200, true, 0, 0},
		{"warning only", jsonBody(t, "defaults: off\ndisable_defaults: [x]\n"), 200, true, 0, 1},
		{"bad json", "{", 400, false, 0, 0},
		{"no content", "{}", 400, false, 0, 0},
		{"null content", `{"content":null}`, 400, false, 0, 0},
	}
	for _, tt := range tests {
		rec := f.do("POST", "/api/validate", tt.body)
		if rec.Code != tt.code {
			t.Errorf("%s: %d\n%s", tt.name, rec.Code, rec.Body)
			continue
		}
		if tt.code != 200 {
			continue
		}
		m := decode(t, rec)
		errs, warn := m["errors"].([]any), m["warnings"].([]any)
		if m["valid"] != tt.valid || len(errs) != tt.errs || len(warn) != tt.warn {
			t.Errorf("%s: %v", tt.name, m)
		}
	}
	if _, err := os.Stat(f.path); err == nil {
		t.Error("validate wrote the file")
	}

	e := decode(t, f.do("POST", "/api/validate", jsonBody(t, "a: 1\nrulez: []\n")))["errors"].([]any)[0].(map[string]any)
	if e["line"] != float64(1) || e["column"] != float64(1) || !strings.Contains(e["message"].(string), "unknown key") {
		t.Errorf("error shape: %v", e)
	}
}

func TestPutInvalidDoesNotTouchFile(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	must(t, os.MkdirAll(filepath.Dir(f.path), 0o755))
	must(t, os.WriteFile(f.path, []byte(dropNoisy), 0o600))
	f.srv.Watcher.Reload()
	before, _ := os.Stat(f.path)

	rec := f.do("PUT", "/api/config", jsonBody(t, "rulez: []\nrules:\n  - name: x\n    when: { match: '(' }\n"))
	if rec.Code != 422 {
		t.Fatalf("%d\n%s", rec.Code, rec.Body)
	}
	m := decode(t, rec)
	if len(m["errors"].([]any)) < 2 {
		t.Errorf("all problems expected: %v", m)
	}
	if _, ok := m["warnings"].([]any); !ok {
		t.Errorf("warnings is not a list: %v", m)
	}
	data, _ := os.ReadFile(f.path)
	after, _ := os.Stat(f.path)
	if string(data) != dropNoisy || !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() {
		t.Error("file was touched")
	}
	if !activeDrops("noisy") {
		t.Error("active rules changed")
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(f.path), ".logspout-*"))
	if len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestPutValidWritesAndReloads(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	if activeDrops("noisy") {
		t.Fatal("rules active before")
	}
	rec := f.do("PUT", "/api/config", jsonBody(t, dropNoisy))
	if rec.Code != 200 {
		t.Fatalf("%d\n%s", rec.Code, rec.Body)
	}
	m := decode(t, rec)
	if m["applied"] != true || !strings.Contains(m["summary"].(string), "1 user rules") || len(m["errors"].([]any)) != 0 {
		t.Errorf("response: %v", m)
	}
	data, err := os.ReadFile(f.path)
	if err != nil || string(data) != dropNoisy {
		t.Fatalf("file: %q %v", data, err)
	}
	if info, _ := os.Stat(f.path); info.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", info.Mode())
	}
	if !activeDrops("noisy") || activeDrops("quiet") {
		t.Error("new rules not active through router.CurrentProcessor")
	}
	// A second save replaces the first and leaves no temp files.
	if rec := f.do("PUT", "/api/config", jsonBody(t, dropQuiet)); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	if activeDrops("noisy") || !activeDrops("quiet") {
		t.Error("second save not active")
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(f.path), ".logspout-*"))
	if len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
	// The watcher does not load it again.
	if f.srv.Watcher.Check() {
		t.Error("watcher reloaded the saved file")
	}
}

func TestPutNewFileModeAndWarnings(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	rec := f.do("PUT", "/api/config", jsonBody(t, "defaults: off\ndisable_defaults: [x]\n"))
	m := decode(t, rec)
	if rec.Code != 200 || len(m["warnings"].([]any)) != 1 {
		t.Errorf("%d %v", rec.Code, m)
	}
	info, err := os.Stat(f.path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("new file: %v %v", info, err)
	}
}

func TestPutKeepsExistingMode(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	must(t, os.MkdirAll(filepath.Dir(f.path), 0o755))
	must(t, os.WriteFile(f.path, []byte(dropNoisy), 0o600))
	must(t, os.Chmod(f.path, 0o600))
	if rec := f.do("PUT", "/api/config", jsonBody(t, dropQuiet)); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	if info, _ := os.Stat(f.path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", info.Mode())
	}
}

func TestPutThroughSymlinkKeepsSymlink(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	dir := t.TempDir()
	target := filepath.Join(dir, "real.yaml")
	must(t, os.WriteFile(target, []byte(dropNoisy), 0o640))
	must(t, os.MkdirAll(filepath.Dir(f.path), 0o755))
	must(t, os.Symlink(target, f.path))
	if rec := f.do("PUT", "/api/config", jsonBody(t, dropQuiet)); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	if info, err := os.Lstat(f.path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v %v", info, err)
	}
	data, _ := os.ReadFile(target)
	info, _ := os.Stat(target)
	if string(data) != dropQuiet || info.Mode().Perm() != 0o640 {
		t.Errorf("target: %q %v", data, info.Mode())
	}
	if !activeDrops("quiet") {
		t.Error("not active")
	}
}

// Content that parses can still fail the build with the add-on options.
func TestValidateAndPutRunTheBuild(t *testing.T) {
	f := newFixture(t, pipeline.Options{DefaultRules: "no-such-set"})
	m := decode(t, f.do("POST", "/api/validate", jsonBody(t, dropNoisy)))
	if m["valid"] != false || len(m["errors"].([]any)) != 1 {
		t.Errorf("validate: %v", m)
	}
	rec := f.do("PUT", "/api/config", jsonBody(t, dropNoisy))
	if rec.Code != 422 {
		t.Errorf("PUT: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(f.path); err == nil {
		t.Error("file written")
	}
	// A file that sets its own defaults does not use the broken option.
	if rec := f.do("PUT", "/api/config", jsonBody(t, "defaults: off\n"+dropNoisy)); rec.Code != 200 {
		t.Errorf("own defaults: %d %s", rec.Code, rec.Body)
	}
}

func TestCloseBeforeServe(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	addr := ln.Addr().String()
	f.srv.Close()
	if err := f.srv.Serve(ln); err != nil {
		t.Errorf("Serve after Close: %v", err)
	}
	ln2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port still bound: %v", err)
	}
	ln2.Close()
}

func TestPutBodyLimits(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	big := strings.Repeat("# x\n", pipeline.MaxFileSize/4+1) // one byte over the limit
	if rec := f.do("PUT", "/api/config", jsonBody(t, big)); rec.Code != 413 {
		t.Errorf("content over limit: %d", rec.Code)
	}
	huge := `{"content":"` + strings.Repeat("a", 7*pipeline.MaxFileSize) + `"}`
	if rec := f.do("PUT", "/api/config", huge); rec.Code != 413 {
		t.Errorf("huge body: %d", rec.Code)
	}
	if rec := f.do("POST", "/api/validate", jsonBody(t, big)); rec.Code != 413 {
		t.Errorf("validate over limit: %d", rec.Code)
	}
	if _, err := os.Stat(f.path); err == nil {
		t.Error("file written for a too large body")
	}
	// Exactly the limit is accepted, also when JSON escapes make the body longer.
	atLimit := strings.Repeat("# x\n", pipeline.MaxFileSize/4)
	if rec := f.do("PUT", "/api/config", jsonBody(t, atLimit)); rec.Code != 200 {
		t.Errorf("content at limit: %d %.200s", rec.Code, rec.Body)
	}
}

func TestPutBadBodies(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	for _, body := range []string{"", "not json", `{"content": 5}`, `{}`, `[]`} {
		if rec := f.do("PUT", "/api/config", body); rec.Code != 400 {
			t.Errorf("%q: %d", body, rec.Code)
		}
	}
}

// The file path is the watcher's, whatever the request says.
func TestPathNeverFromRequest(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	dir := filepath.Dir(filepath.Dir(f.path))
	evil := filepath.Join(dir, "evil.yaml")
	body := `{"content":` + jsonString(dropNoisy) + `,"path":"` + evil + `","file":"../evil.yaml"}`
	targets := []string{
		"/api/config?path=" + evil,
		"/api/config?file=../../evil.yaml",
		"/api/config/../../evil.yaml",
		"/api/%2e%2e/%2e%2e/etc/passwd",
		"/../../etc/passwd",
	}
	for _, target := range targets {
		f.do("PUT", target, body)
		f.do("GET", target, "")
	}
	for _, p := range []string{evil, filepath.Join(dir, "sub", "evil.yaml"), filepath.Join(filepath.Dir(dir), "evil.yaml")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s was written", p)
		}
	}
	m := decode(t, f.do("GET", "/api/config?path=/etc/passwd", ""))
	if m["path"] != f.path {
		t.Errorf("path %v", m["path"])
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestConcurrentPuts(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	contents := []string{dropNoisy, dropQuiet, "rules: []\n", "# nothing\n"}
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := contents[i%len(contents)]
			if rec := f.do("PUT", "/api/config", jsonBody(t, c)); rec.Code != 200 {
				t.Errorf("PUT %d: %d", i, rec.Code)
			}
			f.do("GET", "/api/status", "")
			f.do("GET", "/api/config", "")
		}(i)
	}
	wg.Wait()
	// The file and the active rules agree, and the file is one complete version.
	data, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	known := false
	for _, c := range contents {
		known = known || string(data) == c
	}
	if !known {
		t.Fatalf("torn file: %q", data)
	}
	if want := strings.Contains(string(data), "noisy"); activeDrops("noisy") != want {
		t.Errorf("file and active rules differ: file %q", data)
	}
	if want := strings.Contains(string(data), "quiet"); activeDrops("quiet") != want {
		t.Errorf("file and active rules differ: file %q", data)
	}
}

func TestStatus(t *testing.T) {
	f := newFixture(t, pipeline.Options{DefaultRules: "v1", Debug: true})
	latest := pipeline.LatestDefaults()

	m := decode(t, f.do("GET", "/api/status", ""))
	d := m["defaults"].(map[string]any)
	if d["active"] != "v1" || d["latest"] != latest || d["newer_available"] != (latest != "v1") {
		t.Errorf("defaults: %v", d)
	}
	if m["debug_pipeline"] != true {
		t.Error("debug_pipeline")
	}
	routes := m["routes"].([]any)
	if len(routes) != 3 {
		t.Fatalf("routes: %v", routes)
	}
	for _, r := range routes {
		r := r.(map[string]any)
		if (r["name"] == "dup") != (r["ambiguous"] == true) {
			t.Errorf("route: %v", r)
		}
	}
	rules := m["rules"].(map[string]any)
	if rules["defaults"].(float64) == 0 || rules["global"] != float64(0) || rules["summary"] == "" {
		t.Errorf("rules: %v", rules)
	}
	file := m["file"].(map[string]any)
	if file["path"] != f.path || file["exists"] != false || file["valid"] != true {
		t.Errorf("file: %v", file)
	}

	save := "rules:\n  - name: a\n    drop: true\ntargets:\n  gelf:\n    rules:\n      - name: g1\n        set: { level: error }\n      - name: g2\n        set: { level: error }\n"
	if rec := f.do("PUT", "/api/config", jsonBody(t, save)); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	m = decode(t, f.do("GET", "/api/status", ""))
	rules = m["rules"].(map[string]any)
	if rules["global"] != float64(1) || rules["targets"].(map[string]any)["gelf"] != float64(2) {
		t.Errorf("rules after save: %v", rules)
	}
	for _, r := range m["routes"].([]any) {
		if r := r.(map[string]any); r["name"] == "gelf" && r["rules"] != float64(2) {
			t.Errorf("route rules: %v", r)
		}
	}
	if file := m["file"].(map[string]any); file["exists"] != true || file["valid"] != true {
		t.Errorf("file after save: %v", file)
	}

	// An invalid file edited on disk: reported, the old rules stay.
	must(t, os.WriteFile(f.path, []byte("rulez: 1\n"), 0o644))
	f.srv.Watcher.Reload()
	m = decode(t, f.do("GET", "/api/status", ""))
	file = m["file"].(map[string]any)
	if file["valid"] != false || len(file["errors"].([]any)) != 1 {
		t.Errorf("invalid file: %v", file)
	}
	if m["rules"].(map[string]any)["global"] != float64(1) {
		t.Error("previous rules not kept")
	}
}

func TestStatusDefaultsOffAndLatest(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	d := decode(t, f.do("GET", "/api/status", ""))["defaults"].(map[string]any)
	if d["active"] != "off" || d["newer_available"] != false {
		t.Errorf("off: %v", d)
	}
	m := decode(t, f.do("GET", "/api/status", ""))
	if m["debug_pipeline"] != false || m["rules"].(map[string]any)["excluded"] == nil {
		t.Errorf("%v", m)
	}

	f = newFixture(t, pipeline.Options{DefaultRules: "latest"})
	d = decode(t, f.do("GET", "/api/status", ""))["defaults"].(map[string]any)
	if d["active"] != pipeline.LatestDefaults() || d["newer_available"] != false {
		t.Errorf("latest: %v", d)
	}
}

// Relative URLs only: no response may point at an absolute path.
func TestNoAbsoluteURLs(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	for _, p := range []string{"/", "/api/status", "/api/config"} {
		body := f.do("GET", p, "").Body.String()
		for _, bad := range []string{`href="/`, `src="/`, `"/api/`, "http://", "https://"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
	}
}

func TestServeOverTCP(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	done := make(chan error, 1)
	go func() { done <- f.srv.Serve(ln) }()
	// The peer is 127.0.0.1, not the Supervisor.
	resp, err := http.Get("http://" + ln.Addr().String() + "/api/status")
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("loopback: %d", resp.StatusCode)
	}
	f.srv.AllowedIP = "127.0.0.1"
	resp, err = http.Get("http://" + ln.Addr().String() + "/api/status")
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("allowed: %d", resp.StatusCode)
	}
	f.srv.Close()
	if err := <-done; err != nil {
		t.Errorf("Serve after Close: %v", err)
	}
}

func TestListenFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer ln.Close()
	s := &Server{Watcher: &pipeline.Watcher{}}
	if err := s.ListenAndServe(ln.Addr().String()); err == nil {
		t.Error("bind to a used port succeeded")
	}
	s.Close() // safe without a running server
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
