package ingress

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"github.com/gliderlabs/logspout/pipeline"
)

func TestStaticFiles(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	tests := []struct {
		path, ctype, contains string
	}{
		{"/", "text/html; charset=utf-8", `src="app.js"`},
		{"/app.js", "text/javascript; charset=utf-8", "new URL('api/live'"},
		{"/style.css", "text/css; charset=utf-8", "prefers-color-scheme"},
	}
	for _, tt := range tests {
		for _, method := range []string{"GET", "HEAD"} {
			rec := f.do(method, tt.path, "")
			if rec.Code != 200 {
				t.Errorf("%s %s: %d", method, tt.path, rec.Code)
				continue
			}
			hd := rec.Header()
			if hd.Get("Content-Type") != tt.ctype {
				t.Errorf("%s %s: Content-Type %q", method, tt.path, hd.Get("Content-Type"))
			}
			if hd.Get("Cache-Control") != "no-store" || hd.Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("%s %s: Cache-Control %q, nosniff %q", method, tt.path, hd.Get("Cache-Control"), hd.Get("X-Content-Type-Options"))
			}
			csp := hd.Get("Content-Security-Policy")
			for _, want := range []string{"default-src 'self'", "style-src 'self'", "connect-src 'self' ws: wss:"} {
				if !strings.Contains(csp, want) {
					t.Errorf("%s %s: CSP %q lacks %q", method, tt.path, csp, want)
				}
			}
			if strings.Contains(csp, "unsafe") {
				t.Errorf("CSP allows unsafe content: %q", csp)
			}
			if method == "HEAD" && rec.Body.Len() != 0 {
				t.Errorf("HEAD %s has a body", tt.path)
			}
			if method == "GET" && !strings.Contains(rec.Body.String(), tt.contains) {
				t.Errorf("GET %s lacks %q", tt.path, tt.contains)
			}
		}
	}
}

func TestStaticFilesMethodsAndUnknown(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	tests := []struct {
		method, path string
		want         int
	}{
		{"POST", "/app.js", 405},
		{"PUT", "/style.css", 405},
		{"GET", "/index.html", 404},
		{"GET", "/ui/app.js", 404},
		{"GET", "/app.js/", 404},
		{"GET", "/../ingress.go", 404},
		{"GET", "/favicon.ico", 404},
	}
	for _, tt := range tests {
		if rec := f.do(tt.method, tt.path, ""); rec.Code != tt.want {
			t.Errorf("%s %s: %d, want %d", tt.method, tt.path, rec.Code, tt.want)
		}
	}
	rec := f.do("POST", "/app.js", "")
	if rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("Allow %q", rec.Header().Get("Allow"))
	}
}

func TestStaticFilesNeedSupervisor(t *testing.T) {
	f := newFixture(t, pipeline.Options{})
	for _, p := range []string{"/", "/app.js", "/style.css"} {
		for _, remote := range []string{"10.0.0.1:1", "127.0.0.1:1", "nonsense"} {
			rec := f.doFrom(remote, "GET", p, "", nil)
			if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "function") {
				t.Errorf("%s from %s: %d", p, remote, rec.Code)
			}
		}
	}
}

// The panel runs under the ingress prefix /api/hassio_ingress/<token>/: any absolute URL breaks it.
func TestUIHasNoAbsoluteURLs(t *testing.T) {
	bad := []string{"/api", `src="/`, `href="/`, `src='/`, `href='/`, `'/`, "http://", "https://", "//cdn", "url(/"}
	n := 0
	err := fs.WalkDir(uiFS, "ui", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		n++
		data, err := uiFS.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, b := range bad {
				if strings.Contains(line, b) {
					t.Errorf("%s:%d contains %q: %s", path, i+1, b, strings.TrimSpace(line))
				}
			}
		}
		return nil
	})
	if err != nil || n != len(assets) {
		t.Fatalf("walk: %v, %d files for %d assets", err, n, len(assets))
	}
}

// Untrusted data (log lines, rule names) must never be parsed as HTML.
func TestUINoHTMLInjection(t *testing.T) {
	js, err := uiFS.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
		if strings.Contains(string(js), bad) {
			t.Errorf("app.js uses %s", bad)
		}
	}
	html, _ := uiFS.ReadFile("ui/index.html")
	for _, bad := range []string{"<script>", " onclick=", "style="} {
		if strings.Contains(string(html), bad) {
			t.Errorf("index.html has inline code: %s", bad)
		}
	}
}
