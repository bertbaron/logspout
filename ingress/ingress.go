// Package ingress serves the Home Assistant ingress web UI and its config API.
//
// It listens on its own port, apart from the port 80 server, so that it can
// restrict access to the Supervisor. All URLs are relative: the UI is served
// under an ingress path prefix that is not known here.
package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gliderlabs/logspout/pipeline"
	"github.com/gliderlabs/logspout/router"
)

const (
	// DefaultAddr is the ingress_port of the add-on.
	DefaultAddr = ":8099"
	// SupervisorIP is the only address that may use the listener.
	SupervisorIP = "172.30.32.2"

	// A JSON string can be six times as long as the text it holds (\u00XX).
	maxBody = 6*pipeline.MaxFileSize + 4096
)

// Server is the ingress HTTP handler and listener.
type Server struct {
	// Watcher owns the rule file. Its Path, Env and Base are used; the path never comes from a request.
	Watcher *pipeline.Watcher
	// AllowedIP is the only remote address that gets access, SupervisorIP when empty.
	AllowedIP string

	writeMu sync.Mutex // one save at a time: write and reload as a unit

	hubOnce  sync.Once
	hub      *hub
	testBusy atomic.Bool // a POST /api/test is running

	mu     sync.Mutex
	srv    *http.Server
	closed bool
}

// samples returns the message buffer and live hub, created on first use.
func (s *Server) samples() *hub {
	s.hubOnce.Do(func() {
		s.hub = &hub{routes: func() []string { return s.Watcher.Env.Routes }}
	})
	return s.hub
}

// Handler returns the handler for all requests.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serve)
}

// ListenAndServe serves on addr until Close. It returns nil after Close.
func (s *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Serve is like ListenAndServe on an existing listener.
func (s *Server) Serve(ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return nil
	}
	s.srv = srv
	// Only a running listener captures messages; otherwise the pumps are untouched.
	router.SetTap(s.samples())
	s.mu.Unlock()
	err := srv.Serve(ln)
	// After an unexpected error nothing serves the samples any more.
	s.mu.Lock()
	if router.CurrentTap() == router.Tap(s.samples()) {
		router.SetTap(nil)
	}
	s.mu.Unlock()
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Close stops the listener. A Serve that starts after Close returns at once.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	srv := s.srv
	if srv != nil && router.CurrentTap() == router.Tap(s.samples()) {
		router.SetTap(nil)
	}
	s.mu.Unlock()
	// Shutdown does not wait for hijacked (websocket) connections.
	defer s.samples().closeAll()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if srv.Shutdown(ctx) != nil {
		srv.Close()
	}
}

// allowed checks the TCP peer. Headers such as X-Forwarded-For are never used:
// the client could set them.
func (s *Server) allowed(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	want := net.ParseIP(s.AllowedIP)
	if s.AllowedIP == "" {
		want = net.ParseIP(SupervisorIP)
	}
	return ip != nil && want != nil && ip.Equal(want)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if !s.allowed(r) {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	if a, ok := assets[r.URL.Path]; ok {
		serveAsset(w, r, a)
		return
	}
	switch r.URL.Path {
	case "/api/config":
		switch r.Method {
		case http.MethodGet:
			s.getConfig(w)
		case http.MethodPut:
			s.putConfig(w, r)
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPut)
		}
	case "/api/validate":
		if methodIs(w, r, http.MethodPost) {
			s.validate(w, r)
		}
	case "/api/samples":
		if methodIs(w, r, http.MethodGet) {
			s.getSamples(w)
		}
	case "/api/test":
		if methodIs(w, r, http.MethodPost) {
			s.test(w, r)
		}
	case "/api/live":
		if methodIs(w, r, http.MethodGet) {
			s.live(w, r)
		}
	case "/api/status":
		if methodIs(w, r, http.MethodGet) {
			s.status(w)
		}
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func methodIs(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, m := range methods {
		if r.Method == m {
			return true
		}
	}
	methodNotAllowed(w, methods...)
	return false
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	allow := ""
	for i, m := range methods {
		if i > 0 {
			allow += ", "
		}
		allow += m
	}
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("ingress: write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

type issue struct {
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
}

// issues converts to the JSON form; the result is never nil, so it encodes as [].
func issues(in []pipeline.Issue) []issue {
	out := make([]issue, len(in))
	for i, e := range in {
		out[i] = issue{e.Line, e.Column, e.Message}
	}
	return out
}

func (s *Server) getConfig(w http.ResponseWriter) {
	path := s.Watcher.Path
	data, err := pipeline.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeJSON(w, http.StatusOK, map[string]any{"path": path, "exists": false, "content": ""})
	case errors.Is(err, os.ErrPermission):
		writeError(w, http.StatusInternalServerError, err.Error())
	case err != nil:
		// Too large or not a regular file: the file needs fixing outside the UI.
		writeError(w, http.StatusUnprocessableEntity, "cannot read the rule file: "+err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"path": path, "exists": true, "content": string(data)})
	}
}

type contentBody struct {
	Content *string `json:"content"`
}

// decodeBody decodes the JSON body into v. It writes the error response and returns false when the body is not usable.
// shape describes the expected body for the error message.
func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, shape string, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request too large")
		} else {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("body must be JSON like %s: %v", shape, err))
		}
		return false
	}
	return true
}

// checkContent writes the error response and returns false when the rule file text is missing or too large.
func checkContent(w http.ResponseWriter, content *string) bool {
	if content == nil {
		writeError(w, http.StatusBadRequest, "content is missing")
		return false
	}
	if len(*content) > pipeline.MaxFileSize {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the rule file may be at most %d bytes", pipeline.MaxFileSize))
		return false
	}
	return true
}

// readContent decodes {content}. It writes the error response and returns false when the body is not usable.
func readContent(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body contentBody
	if !decodeBody(w, r, maxBody, `{"content": "..."}`, &body) || !checkContent(w, body.Content) {
		return "", false
	}
	return *body.Content, true
}

// check validates like Reload does: the file, then the pipeline build with the add-on options.
func (s *Server) check(content string) (errs, warnings []pipeline.Issue) {
	_, errs, warnings = s.build(content)
	return errs, warnings
}

// build is check that also returns the pipeline, which is not installed.
func (s *Server) build(content string) (p *pipeline.Pipeline, errs, warnings []pipeline.Issue) {
	res := pipeline.ParseFile([]byte(content), s.Watcher.Env)
	if len(res.Errors) > 0 {
		return nil, res.Errors, res.Warnings
	}
	p, _, err := pipeline.Build(res.Config.Apply(s.Watcher.Base))
	if err != nil {
		return nil, []pipeline.Issue{{Message: err.Error()}}, res.Warnings
	}
	return p, nil, res.Warnings
}

func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	content, ok := readContent(w, r)
	if !ok {
		return
	}
	errs, warnings := s.check(content)
	writeJSON(w, http.StatusOK, map[string]any{
		"valid":    len(errs) == 0,
		"errors":   issues(errs),
		"warnings": issues(warnings),
	})
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	content, ok := readContent(w, r)
	if !ok {
		return
	}
	if errs, warnings := s.check(content); len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"errors":   issues(errs),
			"warnings": issues(warnings),
		})
		return
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := writeFileAtomic(s.Watcher.Path, []byte(content)); err != nil {
		log.Printf("ERROR: ingress: save %s: %v", s.Watcher.Path, err)
		writeError(w, http.StatusInternalServerError, "save failed: "+err.Error())
		return
	}
	log.Printf("%s saved from the web interface", s.Watcher.Path)
	rr := s.Watcher.Reload()
	code := http.StatusOK
	if !rr.Applied {
		// The file changed on disk between the check and the reload.
		code = http.StatusConflict
	}
	writeJSON(w, code, map[string]any{
		"applied":  rr.Applied,
		"summary":  rr.Summary,
		"errors":   issues(rr.Errors),
		"warnings": issues(rr.Warnings),
	})
}

// writeFileAtomic replaces path in one step, so a reader (or a crash) never sees half a file.
// A symlink stays a symlink (the target is replaced) and an existing file keeps its mode.
func writeFileAtomic(path string, data []byte) (err error) {
	mode := os.FileMode(0o644)
	if resolved, rerr := filepath.EvalSymlinks(path); rerr == nil {
		path = resolved
		if info, serr := os.Stat(path); serr == nil {
			mode = info.Mode().Perm()
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".logspout-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	// Best effort: make the rename durable.
	if d, derr := os.Open(dir); derr == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

type routeStatus struct {
	Name      string `json:"name"`
	Ambiguous bool   `json:"ambiguous"`
	Rules     int    `json:"rules"`
}

func (s *Server) status(w http.ResponseWriter) {
	wt := s.Watcher
	st := wt.Status()
	counts := st.Pipeline.Counts()

	latest := pipeline.LatestDefaults()
	active := counts.Defaults
	defaults := map[string]any{
		"active": "off",
		"latest": latest,
		// A pinned set that is not the newest one; "latest" and "off" never get the banner.
		"newer_available": active != "" && latest != "" && active != latest,
	}
	if active != "" {
		defaults["active"] = active
	}

	ambiguous := map[string]bool{}
	for _, n := range wt.Env.Ambiguous {
		ambiguous[n] = true
	}
	routes := make([]routeStatus, 0, len(wt.Env.Routes))
	for _, n := range wt.Env.Routes {
		routes = append(routes, routeStatus{Name: n, Ambiguous: ambiguous[n], Rules: counts.Targets[n]})
	}

	summary := ""
	if !st.Pipeline.Empty() {
		summary = st.Pipeline.Summary()
	}
	_, statErr := os.Stat(wt.Path)
	excluded := counts.Excluded
	if excluded == nil {
		excluded = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"defaults": defaults,
		"routes":   routes,
		"rules": map[string]any{
			"defaults": counts.DefaultsRules,
			"global":   counts.Global,
			"targets":  counts.Targets,
			"excluded": excluded,
			"summary":  summary,
		},
		"file": map[string]any{
			"path":     wt.Path,
			"exists":   statErr == nil,
			"valid":    !st.Loaded || st.Valid,
			"errors":   issues(st.Errors),
			"warnings": issues(st.Warnings),
		},
		"debug_pipeline": wt.Base.Debug,
	})
}
