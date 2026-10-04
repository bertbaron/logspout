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

func TestValidateRouteNames(t *testing.T) {
	tests := []struct {
		name          string
		uris          []string
		wantErr       string
		wantAmbiguous []string
	}{
		{"unique defaults", []string{"gelf://a:1", "syslog://b:2"}, "", nil},
		{"duplicate default is allowed", []string{"syslog://a:1", "syslog+tcp://b:2"}, "", []string{"syslog"}},
		{"duplicate explicit", []string{"syslog://a:1#x", "gelf://b:2#x"}, "add a unique #name", nil},
		{"explicit clashes with default", []string{"syslog://a:1", "gelf://b:2#syslog"}, "add a unique #name", nil},
		{"invalid name", []string{"syslog://a:1#a b"}, "invalid route name", nil},
		{"same type different names", []string{"syslog://a:1#a", "syslog://b:2#b"}, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			amb, err := ValidateRouteNames(tt.uris)
			if tt.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
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
		{"gelf://h:1#bad%20name", "", false, "", "", true, 0},
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
		{"two explicit same", []string{"syslog://a:1#x", "gelf://b:2#x"}, true, false},
		{"explicit vs default", []string{"syslog://a:1", "gelf://b:2#syslog"}, true, false},
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
		if err := rm.Add(&Route{Adapter: "syslog", Address: "b:1", Name: "x"}); err == nil {
			t.Error("second route with same name accepted")
		}
	})
	t.Run("API name equals other default", func(t *testing.T) {
		rm := newNameTestManager()
		if err := rm.AddFromURI("gelf://g:1"); err != nil {
			t.Fatal(err)
		}
		if err := rm.Add(&Route{Adapter: "syslog", Address: "a:1", Name: "gelf"}); err == nil {
			t.Error("name clashing with default of other route accepted")
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
	t.Run("failed Add does not mutate route", func(t *testing.T) {
		rm := newNameTestManager()
		rm.AddFromURI("gelf://g:1#x")
		r := &Route{Adapter: "syslog", Address: "a:1", Name: "x"}
		if rm.Add(r) == nil || r.NameExplicit {
			t.Error("unexpected result")
		}
		r2 := &Route{Adapter: "syslog", Address: "a:1"}
		rm.Add(r2)
		if r2.Name != "syslog" {
			t.Errorf("name = %q", r2.Name)
		}
	})
}
