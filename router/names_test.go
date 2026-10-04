package router

import (
	"strings"
	"testing"
)

func newNameTestManager() *RouteManager {
	AdapterFactories.Register(func(route *Route) (LogAdapter, error) {
		return &DummyAdapter{}, nil
	}, "gelf")
	AdapterFactories.Register(newDummyAdapter, "syslog")
	return &RouteManager{routes: make(map[string]*Route)}
}

func TestRouteName(t *testing.T) {
	tests := []struct {
		uri      string
		name     string
		explicit bool
		wantErr  bool
	}{
		{"gelf://graylog:12201", "gelf", false, false},
		{"syslog+tcp://10.0.0.5:514", "syslog", false, false},
		{"syslog+tcp://10.0.0.5:514#backup", "backup", true, false},
		{"gelf://h:1?filter.name=x#my-name_1.a", "my-name_1.a", true, false},
		{"gelf://h:1#bad name", "", true, true},
		{"gelf://h:1#a/b", "", true, true},
	}
	for _, tt := range tests {
		name, explicit, err := RouteName(tt.uri)
		if (err != nil) != tt.wantErr || name != tt.name || explicit != tt.explicit {
			t.Errorf("RouteName(%q) = %q, %v, %v", tt.uri, name, explicit, err)
		}
	}
}

func TestAmbiguousRouteNames(t *testing.T) {
	tests := []struct {
		name          string
		uris          []string
		wantErr       bool
		wantAmbiguous []string
	}{
		{"unique defaults", []string{"gelf://a:1", "syslog://b:2"}, false, nil},
		{"duplicate default", []string{"syslog://a:1", "syslog+tcp://b:2"}, false, []string{"syslog"}},
		{"duplicate explicit", []string{"syslog://a:1#x", "gelf://b:2#x"}, false, []string{"x"}},
		{"explicit clashes with default", []string{"syslog://a:1", "gelf://b:2#syslog"}, false, []string{"syslog"}},
		{"invalid name counts as default", []string{"syslog://a:1#a b", "syslog://b:1"}, false, []string{"syslog"}},
		{"invalid name alone", []string{"syslog://a:1#a/b"}, false, nil},
		{"same type different names", []string{"syslog://a:1#a", "syslog://b:2#b"}, false, nil},
		{"unparsable", []string{"syslog://a:1/%zz"}, true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			amb, err := AmbiguousRouteNames(tt.uris)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if strings.Join(amb, ",") != strings.Join(tt.wantAmbiguous, ",") {
				t.Errorf("ambiguous = %v, want %v", amb, tt.wantAmbiguous)
			}
		})
	}
}

func TestAddFromURINames(t *testing.T) {
	tests := []struct {
		uri         string
		name        string
		explicit    bool
		address     string
		adapter     string
		wantErr     bool
		optionCount int
	}{
		{"syslog+tcp://h:514", "syslog", false, "h:514", "syslog+tcp", false, 0},
		{"syslog+tcp://h:514#backup", "backup", true, "h:514", "syslog+tcp", false, 0},
		{"gelf://h:1?foo=bar#g", "g", true, "h:1", "gelf", false, 1},
		{"gelf://h:1#bad%20name", "gelf", false, "h:1", "gelf", false, 0},
	}
	for _, tt := range tests {
		rm := newNameTestManager()
		err := rm.AddFromURI(tt.uri)
		if (err != nil) != tt.wantErr {
			t.Fatalf("%s: err = %v", tt.uri, err)
		}
		if tt.wantErr {
			continue
		}
		rts, _ := rm.GetAll()
		r := rts[0]
		if r.Name != tt.name || r.NameExplicit != tt.explicit || r.Address != tt.address ||
			r.Adapter != tt.adapter || r.Path != "" || len(r.Options) != tt.optionCount {
			t.Errorf("%s: got %+v", tt.uri, r)
		}
	}
}

func TestAddDuplicateNames(t *testing.T) {
	tests := []struct {
		name    string
		uris    []string
		wantErr bool
		ambig   bool
	}{
		{"two default same type", []string{"syslog://a:1", "syslog://b:2"}, false, true},
		{"two explicit same", []string{"syslog://a:1#x", "gelf://b:2#x"}, false, false},
		{"explicit vs default", []string{"syslog://a:1", "gelf://b:2#syslog"}, false, true},
		{"distinct explicit", []string{"syslog://a:1#a", "syslog://b:2#b"}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rm := newNameTestManager()
			var err error
			for _, u := range tt.uris {
				if e := rm.AddFromURI(u); e != nil {
					err = e
				}
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if got := rm.AmbiguousName("syslog"); got != tt.ambig {
				t.Errorf("AmbiguousName = %v, want %v", got, tt.ambig)
			}
		})
	}
}

func TestAddDefaultsNameForStoredRoute(t *testing.T) {
	rm := newNameTestManager()
	if err := rm.Add(&Route{ID: "a", Adapter: "syslog+udp", Address: "h:1"}); err != nil {
		t.Fatal(err)
	}
	r, _ := rm.Get("a")
	if r.Name != "syslog" || r.NameExplicit {
		t.Errorf("got %+v", r)
	}
}

func TestAddAPINameWithoutExplicitFlag(t *testing.T) {
	t.Run("two API routes same name", func(t *testing.T) {
		rm := newNameTestManager()
		if err := rm.Add(&Route{Adapter: "syslog", Address: "a:1", Name: "x"}); err != nil {
			t.Fatal(err)
		}
		if err := rm.Add(&Route{Adapter: "syslog", Address: "b:1", Name: "x"}); err != nil {
			t.Fatal(err)
		}
		if !rm.AmbiguousName("x") {
			t.Error("name used twice is not ambiguous")
		}
	})
	t.Run("API name equals other default", func(t *testing.T) {
		rm := newNameTestManager()
		if err := rm.AddFromURI("gelf://g:1"); err != nil {
			t.Fatal(err)
		}
		if err := rm.Add(&Route{Adapter: "syslog", Address: "a:1", Name: "gelf"}); err != nil {
			t.Fatal(err)
		}
		if !rm.AmbiguousName("gelf") {
			t.Error("clashing name is not ambiguous")
		}
	})
	t.Run("stored name equal to adapter type stays default", func(t *testing.T) {
		rm := newNameTestManager()
		for _, addr := range []string{"a:1", "b:1"} {
			if err := rm.Add(&Route{Adapter: "syslog", Address: addr, Name: "syslog"}); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("invalid API name is ignored with the default name", func(t *testing.T) {
		rm := newNameTestManager()
		r := &Route{Adapter: "syslog", Address: "a:1", Name: "a b"}
		if err := rm.Add(r); err != nil || r.Name != "syslog" || r.NameExplicit {
			t.Errorf("err = %v, route = %+v", err, r)
		}
	})
}
