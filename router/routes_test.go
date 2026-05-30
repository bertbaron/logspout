package router

import (
	"reflect"
	"testing"
)

type DummyAdapter struct{}

func (a *DummyAdapter) Stream(logstream chan *Message) {
	for message := range logstream {
		print("message passed to DummyAdapter: ", message)
	}
}

func newDummyAdapter(route *Route) (LogAdapter, error) {
	return &DummyAdapter{}, nil
}

func TestRouterGetAll(t *testing.T) {
	rts, err := Routes.GetAll()
	if err != nil {
		t.Error("error getting all routes")
	}
	routes := append(marshal(rts), '\n')
	emptyRoutes := append(marshal(make([]*Route, 0)), '\n')
	if !reflect.DeepEqual(routes, emptyRoutes) {
		t.Error("expected '[]' got:", routes)
	}
}

func TestRouterNoDuplicateIds(t *testing.T) {
	AdapterFactories.Register(newDummyAdapter, "syslog")

	// Mock "running" so routes actually start running when added.
	Routes.routing = true

	// Start the first route.
	route1 := &Route{
		ID:      "abc",
		Address: "someUrl",
		Adapter: "syslog",
	}
	if err := Routes.Add(route1); err != nil {
		t.Error("Error adding route:", err)
	}

	// Start a second route with the same ID.
	var route2 = &Route{
		ID:      "abc",
		Address: "someUrl2",
		Adapter: "syslog",
	}
	Routes.Add(route2)

	if !route1.closed.Load() {
		t.Errorf("route1 was not closed after route2 added.")
	}
}

func TestRouteManagerSetupWithArgsUsesExplicitRoutes(t *testing.T) {
	AdapterFactories.Register(newDummyAdapter, "setupadapter")
	defer AdapterFactories.Unregister("setupadapter")

	t.Setenv("ROUTE_URIS", "setupadapter://env-only")
	t.Setenv("ROUTESPATH", t.TempDir()+"/missing")

	rm := &RouteManager{routes: make(map[string]*Route)}
	err := rm.SetupWithArgs([]string{"setupadapter://cli-only"}, []string{
		"setupadapter://explicit-target?filter.name=comma,value",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	routes, err := rm.GetAll()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(routes))
	}
	if routes[0].Address != "explicit-target" {
		t.Fatalf("expected explicit route address, got %q", routes[0].Address)
	}
	if routes[0].FilterName != "comma,value" {
		t.Fatalf("expected unsplit filter.name, got %q", routes[0].FilterName)
	}
}
