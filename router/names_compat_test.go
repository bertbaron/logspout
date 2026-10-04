package router

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestNamesNoFragmentKeepsOldRouteFields(t *testing.T) {
	var seen []*Route
	AdapterFactories.Unregister("capta") // a previous run (-count) must not leave its closure behind
	AdapterFactories.Register(func(r *Route) (LogAdapter, error) {
		seen = append(seen, r)
		return &DummyAdapter{}, nil
	}, "capta")
	t.Cleanup(func() { AdapterFactories.Unregister("capta") })
	rm := &RouteManager{routes: make(map[string]*Route)}
	for _, u := range []string{"capta+tcp://h:514?filter.name=web&structured_data=x", "capta://h2:514/p"} {
		if err := rm.AddFromURI(u); err != nil {
			t.Fatalf("%s: %v", u, err)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("adapters created = %d", len(seen))
	}
	r := seen[0]
	if r.Address != "h:514" || r.Adapter != "capta+tcp" || r.FilterName != "web" ||
		r.Options["structured_data"] != "x" || len(r.Options) != 1 || r.Name != "capta" || r.NameExplicit {
		t.Errorf("route 1: %+v", r)
	}
	if seen[1].Path != "/p" || seen[1].Address != "h2:514" {
		t.Errorf("route 2: %+v", seen[1])
	}
	if !rm.AmbiguousName("capta") {
		t.Error("two capta routes must be ambiguous")
	}
}

func TestNamesFragmentInvisibleToAdapter(t *testing.T) {
	var got *Route
	AdapterFactories.Unregister("captb") // a previous run (-count) must not leave its closure behind
	AdapterFactories.Register(func(r *Route) (LogAdapter, error) {
		got = r
		return &DummyAdapter{}, nil
	}, "captb")
	t.Cleanup(func() { AdapterFactories.Unregister("captb") })
	rm := &RouteManager{routes: make(map[string]*Route)}
	if err := rm.AddFromURI("captb+tcp://h:514/p?a=b#backup"); err != nil {
		t.Fatal(err)
	}
	if got.Address != "h:514" || got.Path != "/p" || got.Adapter != "captb+tcp" ||
		len(got.Options) != 1 || got.Options["a"] != "b" || got.Name != "backup" {
		t.Errorf("%+v", got)
	}
}

func TestNamesFragmentEdgeCases(t *testing.T) {
	t.Setenv("ROUTE_NAME_T", "fromenv")
	t.Setenv("ROUTE_BAD_T", "a b")
	os.Unsetenv("ROUTE_UNSET_T")
	tests := []struct {
		uri      string
		name     string
		explicit bool
		wantErr  bool
	}{
		{"gelf://h:1#${ROUTE_NAME_T}", "fromenv", true, false},
		{"gelf://h:1#$ROUTE_NAME_T", "fromenv", true, false},
		{"gelf://h:1#${ROUTE_BAD_T}", "", true, true},
		{"gelf://h:1#${ROUTE_UNSET_T}", "gelf", false, false},
		{"gelf://h:1#", "gelf", false, false},
		{"gelf+tcp://h:1#", "gelf", false, false},
		{"gelf://h:1#a%2Db", "a-b", true, false},
		{"gelf://h:1#a%20b", "", true, true},
		{"gelf://h:1#a%2Fb", "", true, true},
		{"gelf://h:1#a,b", "", true, true},
		{"gelf://h:1#a#b", "", true, true},
		{"gelf://h:1#a?b", "", true, true},
		{"gelf://h:1#café", "", true, true},
		{"gelf://h:1#%", "", false, true},
		{"gelf://h:1#_", "_", true, false},
		{"gelf://h:1#.", ".", true, false},
		{"gelf://h:1#UPPER", "UPPER", true, false},
	}
	for _, tt := range tests {
		name, explicit, err := RouteName(tt.uri)
		if (err != nil) != tt.wantErr || (!tt.wantErr && (name != tt.name || explicit != tt.explicit)) {
			t.Errorf("RouteName(%q) = %q, %v, %v", tt.uri, name, explicit, err)
		}
		rm := newNameTestManager()
		if err := rm.AddFromURI(tt.uri); (err != nil) != tt.wantErr {
			t.Errorf("AddFromURI(%q) err = %v", tt.uri, err)
		}
	}
}

func writeStoredRoutes(t *testing.T, files map[string]string) RouteFileStore {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return RouteFileStore(dir)
}

func TestNamesLoadOldPersistedRoutesWithoutNameFields(t *testing.T) {
	store := writeStoredRoutes(t, map[string]string{
		"aaa.json": `{"id":"aaa","adapter":"syslog+tcp","address":"h1:514"}`,
		"bbb.json": `{"id":"bbb","adapter":"syslog","address":"h2:514","filter_name":"x"}`,
		"ccc.json": `{"id":"ccc","adapter":"gelf","address":"g:1"}`,
	})
	rm := newNameTestManager()
	if err := rm.Load(store); err != nil {
		t.Fatalf("Load: %v", err)
	}
	all, _ := rm.GetAll()
	if len(all) != 3 {
		t.Fatalf("routes = %d", len(all))
	}
	for _, r := range all {
		want := r.AdapterType()
		if r.Name != want || r.NameExplicit {
			t.Errorf("%s: %+v", r.ID, r)
		}
	}
	if !rm.AmbiguousName("syslog") || rm.AmbiguousName("gelf") {
		t.Error("ambiguity wrong")
	}
}

func TestNamesPersistedRoundTripKeepsName(t *testing.T) {
	store := writeStoredRoutes(t, nil)
	rm := newNameTestManager()
	rm.persistor = store
	if err := rm.AddFromURI("syslog://h:1#keep"); err != nil {
		t.Fatal(err)
	}
	rm2 := newNameTestManager()
	if err := rm2.Load(store); err != nil {
		t.Fatal(err)
	}
	all, _ := rm2.GetAll()
	if len(all) != 1 || all[0].Name != "keep" || !all[0].NameExplicit {
		t.Errorf("%+v", all)
	}
}

// Start-up must survive a stored (API-added) route whose name collides with a URI route.
func TestNamesStoredRouteCollidingWithURIRouteDoesNotBreakStartup(t *testing.T) {
	store := writeStoredRoutes(t, map[string]string{
		"aaa.json": `{"id":"aaa","adapter":"syslog","address":"h1:514"}`,
	})
	rm := newNameTestManager()
	if err := rm.AddFromURI("gelf://g:1#syslog"); err != nil {
		t.Fatal(err)
	}
	if err := rm.Load(store); err != nil {
		t.Fatalf("Load failed, stored route would be lost and start-up aborted: %v", err)
	}
}

func TestNamesRoutesAPIAdd(t *testing.T) {
	rm := newNameTestManager()
	rm.routing = true
	if err := rm.Add(&Route{Adapter: "syslog", Address: "a:1"}); err != nil {
		t.Fatal(err)
	}
	if err := rm.Add(&Route{Adapter: "syslog", Address: "b:1"}); err != nil {
		t.Fatalf("second default route: %v", err)
	}
	if err := rm.Add(&Route{Adapter: "syslog", Address: "c:1", Name: "bad name"}); err == nil {
		t.Error("invalid name accepted")
	}
	if err := rm.Add(&Route{Adapter: "syslog", Address: "c:1", Name: "x", NameExplicit: true}); err != nil {
		t.Fatal(err)
	}
	if err := rm.Add(&Route{Adapter: "gelf", Address: "d:1", Name: "x", NameExplicit: true}); err == nil {
		t.Error("duplicate explicit name accepted")
	}
	// Same ID replaces itself, also with the same explicit name.
	if err := rm.Add(&Route{ID: "r1", Adapter: "syslog", Address: "e:1", Name: "y", NameExplicit: true}); err != nil {
		t.Fatal(err)
	}
	if err := rm.Add(&Route{ID: "r1", Adapter: "syslog", Address: "e:2", Name: "y", NameExplicit: true}); err != nil {
		t.Errorf("re-adding same id: %v", err)
	}
	// JSON API clients send name without name_explicit.
	if err := rm.Add(&Route{Adapter: "gelf", Address: "f:1", Name: "y"}); err == nil {
		t.Error("API route reusing an explicit name was accepted")
	}
}

func TestNamesConcurrentAddAndQuery(t *testing.T) {
	rm := newNameTestManager()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			rm.Add(&Route{Adapter: "syslog", Address: "a:1"})
		}()
		go func() {
			defer wg.Done()
			rm.AmbiguousName("syslog")
			rm.GetAll()
		}()
	}
	wg.Wait()
	if !rm.AmbiguousName("syslog") {
		t.Error("expected ambiguous")
	}
}

func TestValidateRouteNamesEdgeCases(t *testing.T) {
	t.Setenv("ROUTE_NAME_T", "dup")
	if _, err := ValidateRouteNames([]string{"gelf://a:1#${ROUTE_NAME_T}", "syslog://b:1#dup"}); err == nil {
		t.Error("env-expanded duplicate not detected")
	}
	if amb, err := ValidateRouteNames(nil); err != nil || len(amb) != 0 {
		t.Errorf("%v %v", amb, err)
	}
	// default first, then explicit with same name, and the other order
	for _, uris := range [][]string{{"syslog://a:1", "gelf://b:1#syslog"}, {"gelf://b:1#syslog", "syslog://a:1"}} {
		if _, err := ValidateRouteNames(uris); err == nil {
			t.Errorf("%v: no error", uris)
		}
	}
	amb, err := ValidateRouteNames([]string{"syslog://a:1", "syslog://b:1", "syslog://c:1", "gelf://d:1", "gelf://e:1"})
	if err != nil || len(amb) != 2 || amb[0] != "syslog" || amb[1] != "gelf" {
		t.Errorf("ambiguous = %v, err = %v", amb, err)
	}
}
