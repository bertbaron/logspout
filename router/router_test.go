package router

import (
	"os"
	"testing"
	"time"

	docker "github.com/fsouza/go-dockerclient"
)

// ---- types.go tests ----

func TestRouteAdapterType(t *testing.T) {
	cases := []struct {
		adapter  string
		expected string
	}{
		{"syslog", "syslog"},
		{"syslog+tcp", "syslog"},
		{"gelf+udp", "gelf"},
	}
	for _, c := range cases {
		r := &Route{Adapter: c.adapter}
		if got := r.AdapterType(); got != c.expected {
			t.Errorf("AdapterType(%q) = %q, want %q", c.adapter, got, c.expected)
		}
	}
}

func TestRouteAdapterTransport(t *testing.T) {
	cases := []struct {
		adapter  string
		dfault   string
		expected string
	}{
		{"syslog", "udp", "udp"},
		{"syslog+tcp", "udp", "tcp"},
		{"gelf+tls", "udp", "tls"},
	}
	for _, c := range cases {
		r := &Route{Adapter: c.adapter}
		if got := r.AdapterTransport(c.dfault); got != c.expected {
			t.Errorf("AdapterTransport(%q, %q) = %q, want %q", c.adapter, c.dfault, got, c.expected)
		}
	}
}

func TestRouteCloser(t *testing.T) {
	ch := make(chan struct{}, 1)
	r := &Route{closer: ch}
	if r.Closer() != ch {
		t.Error("Closer() should return the closer channel")
	}
}

func TestRouteOverrideCloser(t *testing.T) {
	ch := make(chan struct{}, 1)
	r := &Route{}
	r.OverrideCloser(ch)
	if r.Closer() != ch {
		t.Error("Closer() should return the overridden closer channel")
	}
}

func TestRouteMultiContainer(t *testing.T) {
	// matchAll → no filters → MultiContainer is true
	r := &Route{}
	if !r.MultiContainer() {
		t.Error("expected MultiContainer() = true for empty route")
	}

	// FilterName with wildcard → still multi
	r2 := &Route{FilterName: "prefix*"}
	if !r2.MultiContainer() {
		t.Error("expected MultiContainer() = true for wildcard filter name")
	}

	// Specific FilterID → not multi
	r3 := &Route{FilterID: "abc123"}
	if r3.MultiContainer() {
		t.Error("expected MultiContainer() = false for specific FilterID")
	}
}

func TestRouteMatchContainer(t *testing.T) {
	// matchAll
	r := &Route{}
	if !r.MatchContainer("id1", "myapp", nil) {
		t.Error("matchAll route should match any container")
	}

	// FilterID prefix match
	r2 := &Route{FilterID: "abc"}
	if !r2.MatchContainer("abc123", "anything", nil) {
		t.Error("expected match for FilterID prefix")
	}
	if r2.MatchContainer("xyz123", "anything", nil) {
		t.Error("expected no match for non-matching FilterID")
	}

	// FilterName exact match
	r3 := &Route{FilterName: "web"}
	if !r3.MatchContainer("id1", "web", nil) {
		t.Error("expected match for FilterName")
	}
	if r3.MatchContainer("id1", "api", nil) {
		t.Error("expected no match for non-matching FilterName")
	}

	// FilterLabels
	r4 := &Route{FilterLabels: []string{"env:prod"}}
	if !r4.MatchContainer("id1", "", map[string]string{"env": "prod"}) {
		t.Error("expected match for matching label")
	}
	if r4.MatchContainer("id1", "", map[string]string{"env": "dev"}) {
		t.Error("expected no match for non-matching label value")
	}
}

func TestRouteMatchMessage(t *testing.T) {
	r := &Route{}
	msg := &Message{Source: "stdout"}
	if !r.MatchMessage(msg) {
		t.Error("matchAll route should match any message")
	}

	r2 := &Route{FilterSources: []string{"stdout"}}
	if !r2.MatchMessage(&Message{Source: "stdout"}) {
		t.Error("expected match for matching source")
	}
	if r2.MatchMessage(&Message{Source: "stderr"}) {
		t.Error("expected no match for non-matching source")
	}
}

// ---- pump.go tests ----

func TestPumpNormalName(t *testing.T) {
	if got := normalName("/mycontainer"); got != "mycontainer" {
		t.Errorf("normalName('/mycontainer') = %q, want 'mycontainer'", got)
	}
}

func TestPumpNormalID(t *testing.T) {
	long := "abcdefghijklmnopqrstuvwxyz"
	if got := normalID(long); got != long[:pumpMaxIDLen] {
		t.Errorf("normalID should truncate long IDs, got %q", got)
	}
	short := "abc"
	if got := normalID(short); got != short {
		t.Errorf("normalID should return short IDs unchanged, got %q", got)
	}
}

func TestLogDriverSupported(t *testing.T) {
	cases := []struct {
		logType  string
		expected bool
	}{
		{"json-file", true},
		{"journald", true},
		{"db", true},
		{"syslog", false},
		{"none", false},
		{"", false},
	}
	for _, c := range cases {
		container := &docker.Container{
			HostConfig: &docker.HostConfig{
				LogConfig: docker.LogConfig{Type: c.logType},
			},
		}
		if got := logDriverSupported(container); got != c.expected {
			t.Errorf("logDriverSupported(%q) = %v, want %v", c.logType, got, c.expected)
		}
	}
}

func TestGetInactivityTimeoutFromEnv(t *testing.T) {
	os.Unsetenv("INACTIVITY_TIMEOUT")
	if got := getInactivityTimeoutFromEnv(); got != 0 {
		t.Errorf("expected 0 when env not set, got %v", got)
	}

	os.Setenv("INACTIVITY_TIMEOUT", "30s")
	defer os.Unsetenv("INACTIVITY_TIMEOUT")
	if got := getInactivityTimeoutFromEnv(); got != 30*time.Second {
		t.Errorf("expected 30s, got %v", got)
	}
}

// ---- routes.go tests ----

func TestRouteManagerGet(t *testing.T) {
	rm := &RouteManager{routes: make(map[string]*Route)}
	rm.routes["test-id"] = &Route{ID: "test-id"}

	r, err := rm.Get("test-id")
	if err != nil || r.ID != "test-id" {
		t.Errorf("expected route with id 'test-id', got err=%v", err)
	}

	_, err = rm.Get("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent route")
	}
}

func TestRouteManagerRemove(t *testing.T) {
	rm := &RouteManager{routes: make(map[string]*Route)}
	// Route with no closer (just added directly, not via Add)
	rm.routes["r1"] = &Route{ID: "r1"}

	if !rm.Remove("r1") {
		t.Error("expected Remove to return true for existing route")
	}
	if _, ok := rm.routes["r1"]; ok {
		t.Error("expected route to be removed from map")
	}
	if rm.Remove("nonexistent") {
		t.Error("expected Remove to return false for nonexistent route")
	}
}

func TestRouteManagerAddFromURIBasic(t *testing.T) {
	AdapterFactories.Register(func(route *Route) (LogAdapter, error) {
		return &DummyAdapter{}, nil
	}, "testadapter")
	defer AdapterFactories.Unregister("testadapter")

	rm := &RouteManager{routes: make(map[string]*Route)}
	err := rm.AddFromURI("testadapter://localhost:9999")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rm.routes) != 1 {
		t.Errorf("expected 1 route, got %d", len(rm.routes))
	}
	for _, r := range rm.routes {
		if r.Address != "localhost:9999" {
			t.Errorf("expected address 'localhost:9999', got '%s'", r.Address)
		}
	}
}

func TestRouteManagerAddFromURIWithFilters(t *testing.T) {
	AdapterFactories.Register(func(route *Route) (LogAdapter, error) {
		return &DummyAdapter{}, nil
	}, "testadapter2")
	defer AdapterFactories.Unregister("testadapter2")

	rm := &RouteManager{routes: make(map[string]*Route)}
	err := rm.AddFromURI("testadapter2://localhost:9999?filter.name=myapp&filter.sources=stdout,stderr")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, r := range rm.routes {
		if r.FilterName != "myapp" {
			t.Errorf("expected FilterName='myapp', got '%s'", r.FilterName)
		}
		if len(r.FilterSources) != 2 {
			t.Errorf("expected 2 filter sources, got %d", len(r.FilterSources))
		}
	}
}

func TestRouteManagerAddBadAdapter(t *testing.T) {
	rm := &RouteManager{routes: make(map[string]*Route)}
	err := rm.AddFromURI("unknownadapter://localhost:9999")
	if err == nil {
		t.Error("expected error for unknown adapter")
	}
}
