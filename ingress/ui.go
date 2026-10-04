package ingress

import (
	"embed"
	"net/http"
)

// The UI has no build step: plain files, embedded in the binary.
//
//go:embed ui/index.html ui/app.js ui/style.css
var uiFS embed.FS

type asset struct {
	file        string
	contentType string
}

// assets maps request paths to embedded files. Everything else is a 404.
var assets = map[string]asset{
	"/":          {"ui/index.html", "text/html; charset=utf-8"},
	"/app.js":    {"ui/app.js", "text/javascript; charset=utf-8"},
	"/style.css": {"ui/style.css", "text/css; charset=utf-8"},
}

// No external resources: the panel must work offline. connect-src lists ws: and wss: because
// older browsers do not treat 'self' as a match for a same-origin websocket.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self' ws: wss:; " +
	"img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"

// serveAsset writes a static file. The add-on is updated in place, so nothing is cached.
func serveAsset(w http.ResponseWriter, r *http.Request, a asset) {
	if !methodIs(w, r, http.MethodGet, http.MethodHead) {
		return
	}
	data, err := uiFS.ReadFile(a.file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "asset missing")
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.contentType)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	if r.Method == http.MethodGet {
		w.Write(data)
	}
}
