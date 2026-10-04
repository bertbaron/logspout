package ingress

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gliderlabs/logspout/pipeline"
)

func tmpFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, ".logspout-*.tmp"))
	return m
}

func TestPutWhilePollLoopRuns(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	w := f.srv.Watcher
	w.Interval = time.Millisecond
	w.Start()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 40; i++ {
			c := dropNoisy
			if i%2 == 1 {
				c = dropQuiet
			}
			if rec := f.do("PUT", "/api/config", jsonBody(t, c)); rec.Code != 200 {
				t.Errorf("put %d: %d %s", i, rec.Code, rec.Body)
				return
			}
			f.do("GET", "/api/status", "")
		}
	}()
	<-done
	time.Sleep(50 * time.Millisecond)
	// Last write was dropQuiet and must be the active state, also after the poller ran.
	if !activeDrops("quiet") || activeDrops("noisy") {
		t.Error("final state not active")
	}
}

func TestPutTargetIsDirectory(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	must(t, os.MkdirAll(filepath.Join(f.path, "inner"), 0o755))
	rec := f.do("PUT", "/api/config", jsonBody(t, dropNoisy))
	if rec.Code != 500 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if fi, err := os.Stat(f.path); err != nil || !fi.IsDir() {
		t.Error("directory replaced")
	}
	if n := tmpFiles(t, filepath.Dir(f.path)); len(n) != 0 {
		t.Errorf("temp files left: %v", n)
	}
}

func TestPutReadOnlyDirectory(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newFixture(t, pipeline.Options{})
	if rec := f.do("PUT", "/api/config", jsonBody(t, dropNoisy)); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	dir := filepath.Dir(f.path)
	must(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	rec := f.do("PUT", "/api/config", jsonBody(t, dropQuiet))
	if rec.Code != 500 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	got, _ := os.ReadFile(f.path)
	if string(got) != dropNoisy {
		t.Errorf("original changed: %q", got)
	}
	if n := tmpFiles(t, dir); len(n) != 0 {
		t.Errorf("temp files left: %v", n)
	}
	if !activeDrops("noisy") {
		t.Error("old rules lost")
	}
}

func TestPutExactAndOneOverLimit(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	exact := strings.Repeat("#", pipeline.MaxFileSize-1) + "\n"
	if rec := f.do("PUT", "/api/config", jsonBody(t, exact)); rec.Code != 200 {
		t.Fatalf("exact: %d %.200s", rec.Code, rec.Body)
	}
	if fi, _ := os.Stat(f.path); fi == nil || fi.Size() != pipeline.MaxFileSize {
		t.Errorf("stored size %v", fi)
	}
	// Same size must be readable again.
	if rec := f.do("GET", "/api/config", ""); rec.Code != 200 {
		t.Errorf("get at limit: %d", rec.Code)
	}
	over := exact + "x"
	if rec := f.do("PUT", "/api/config", jsonBody(t, over)); rec.Code != 413 {
		t.Errorf("limit+1: %d", rec.Code)
	}
	if rec := f.do("POST", "/api/validate", jsonBody(t, over)); rec.Code != 413 {
		t.Errorf("validate limit+1: %d", rec.Code)
	}
	// Escapes make the body about 6x bigger but the content is exactly at the limit.
	esc := strings.Repeat("\x01", 0) + "# " + strings.Repeat("\t", pipeline.MaxFileSize-3) + "\n"
	if rec := f.do("PUT", "/api/config", jsonBody(t, esc)); rec.Code != 200 {
		t.Errorf("escaped at limit: %d %.200s", rec.Code, rec.Body)
	}
}

func TestStatusInvalidFileOnDisk(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	if rec := f.do("PUT", "/api/config", jsonBody(t, dropNoisy)); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	must(t, os.WriteFile(f.path, []byte("rules:\n  - name: x\n    when: { match: '(' }\n    drop: true\n"), 0o644))
	f.srv.Watcher.Reload()
	m := decode(t, f.do("GET", "/api/status", ""))
	file := m["file"].(map[string]any)
	errs := file["errors"].([]any)
	if file["valid"] != false || len(errs) == 0 || file["exists"] != true {
		t.Errorf("file: %v", file)
	}
	if e := errs[0].(map[string]any); e["message"] == "" {
		t.Errorf("empty message: %v", e)
	}
	if !activeDrops("noisy") {
		t.Error("previous rules not active")
	}
	if m["rules"].(map[string]any)["global"] != float64(1) {
		t.Error("counts do not show previous rules")
	}
	// A GET returns the bad file as is, so the editor can fix it.
	if c := decode(t, f.do("GET", "/api/config", ""))["content"].(string); !strings.Contains(c, "match: '('") {
		t.Errorf("content: %q", c)
	}
}

func TestStatusNewerAvailable(t *testing.T) {
	latest := pipeline.LatestDefaults()
	for _, sel := range []string{"off", "latest", "v1"} {
		f := newFixture(t, pipeline.Options{DefaultRules: sel})
		d := decode(t, f.do("GET", "/api/status", ""))["defaults"].(map[string]any)
		want := sel == "v1" && latest != "v1"
		if d["newer_available"] != want {
			t.Errorf("%s: %v (latest %s)", sel, d, latest)
		}
		switch sel {
		case "off":
			if d["active"] != "off" {
				t.Errorf("%v", d)
			}
		case "latest":
			if d["active"] != latest {
				t.Errorf("%v", d)
			}
		}
	}
}

func TestPathVariants(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	tests := []struct {
		method, target string
		want           int
	}{
		{"HEAD", "/", 200},
		{"HEAD", "/api/status", 405},
		{"HEAD", "/api/config", 405},
		{"GET", "/?x=1", 200},
		{"GET", "/api/status?x=1", 200},
		{"GET", "/api/config?x=1", 200},
		{"GET", "/api/nothing", 404},
		{"GET", "/api/", 404},
		{"GET", "/api", 404},
		{"GET", "/api/config/", 404},
		{"PUT", "/api/config/", 404},
		{"GET", "/api/status/", 404},
		{"POST", "/api/validate/", 404},
		{"GET", "/index.html", 404},
		{"DELETE", "/", 405},
		{"POST", "/api/status", 405},
		{"GET", "/api/validate", 405},
		{"DELETE", "/api/config", 405},
		{"OPTIONS", "/api/config", 405},
	}
	for _, tt := range tests {
		if rec := f.do(tt.method, tt.target, ""); rec.Code != tt.want {
			t.Errorf("%s %s: %d, want %d", tt.method, tt.target, rec.Code, tt.want)
		}
	}
	// A double slash: httptest parses "//api/config" as host "api", so build the request by hand.
	r := httptestRequest(t, "GET", "//api/config")
	rec := serveRaw(f, r)
	if rec.Code == 200 {
		t.Errorf("//api/config served: %d", rec.Code)
	}
}

func TestAllowedIPForms(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	for remote, want := range map[string]int{
		"[::ffff:172.30.32.2]:1":       200,
		"[0:0:0:0:0:ffff:ac1e:2002]:1": 200,
		"[::ffff:172.30.32.2%eth0]:1":  403, // a zone makes ParseIP fail: refused is the safe side
		"[fe80::1%eth0]:1":             403,
		"172.30.32.2%eth0:1":           403,
		"172.030.032.002:1":            403,
		"":                             403,
		":1":                           403,
	} {
		if rec := f.doFrom(remote, "GET", "/api/status", "", nil); rec.Code != want {
			t.Errorf("%q: %d, want %d", remote, rec.Code, want)
		}
	}
	// ac1e:2002 is 172.30.32.2 in hex.
	if rec := f.doFrom("[::ffff:ac1e:2002]:1", "GET", "/api/status", "", nil); rec.Code != 200 {
		t.Errorf("hex mapped form: %d", rec.Code)
	}
}

func httptestRequest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, "/x", nil)
	r.URL.Path = target
	r.RequestURI = target
	r.RemoteAddr = testIP + ":1"
	return r
}

func serveRaw(f *fixture, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, r)
	return rec
}
