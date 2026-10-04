package router

import (
	"crypto/sha1" //nolint:gosec
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gliderlabs/logspout/cfg"
)

// Routes is all the configured routes
var Routes *RouteManager

func init() {
	Routes = &RouteManager{routes: make(map[string]*Route)}
	Jobs.Register(Routes, "routes")
}

// RouteManager is responsible for maintaining route state
type RouteManager struct {
	sync.Mutex
	persistor RouteStore
	routes    map[string]*Route
	routing   bool
	wg        sync.WaitGroup
	// ambiguous is the set of names that more than one route has. The pumps
	// read it for every message, so it is kept up to date by Add and Remove.
	ambiguous atomic.Pointer[map[string]bool]
	warned    sync.Map
}

// Load loads all route from a RouteStore
func (rm *RouteManager) Load(persistor RouteStore) error {
	routes, err := persistor.GetAll()
	if err != nil {
		return err
	}
	for _, route := range routes {
		if err = rm.Add(route); err != nil {
			return err
		}
	}
	rm.persistor = persistor
	return nil
}

// Get returns a Route based on id
func (rm *RouteManager) Get(id string) (*Route, error) {
	rm.Lock()
	defer rm.Unlock()
	route, ok := rm.routes[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return route, nil
}

// GetAll returns all routes in the RouteManager
func (rm *RouteManager) GetAll() ([]*Route, error) {
	rm.Lock()
	defer rm.Unlock()
	routes := make([]*Route, 0)
	for _, route := range rm.routes {
		routes = append(routes, route)
	}
	return routes, nil
}

// Remove removes a route from a RouteManager based on id
func (rm *RouteManager) Remove(id string) bool {
	rm.Lock()
	defer rm.Unlock()
	route, ok := rm.routes[id]
	if ok && route.closer != nil {
		route.closer <- struct{}{}
	}
	delete(rm.routes, id)
	rm.updateAmbiguous()
	if rm.persistor != nil {
		rm.persistor.Remove(id)
	}
	return ok
}

// AddFromURI creates a new route from an URI string and adds it to the RouteManager
func (rm *RouteManager) AddFromURI(uri string) error {
	expandedRoute := os.ExpandEnv(uri)
	u, err := url.Parse(expandedRoute)
	if err != nil {
		return err
	}
	name, explicit, invalid := effectiveName(u)
	if invalid != nil {
		log.Printf("warning: route %s: %v; the #fragment is ignored", u.Redacted(), invalid)
	}
	r := &Route{
		Name:         name,
		NameExplicit: explicit,
		Address:      u.Host,
		Path:         u.Path,
		Adapter:      u.Scheme,
		User:         u.User,
		Options:      make(map[string]string),
	}
	if u.RawQuery != "" {
		params, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return err
		}
		for key := range params {
			value := params.Get(key)
			switch key {
			case "filter.id":
				r.FilterID = value
			case "filter.name":
				r.FilterName = value
			case "filter.labels":
				r.FilterLabels = strings.Split(value, ",")
			case "filter.sources":
				r.FilterSources = strings.Split(value, ",")
			default:
				r.Options[key] = value
			}
		}
	}
	return rm.Add(r)
}

// Add adds a route to the RouteManager
func (rm *RouteManager) Add(route *Route) error {
	rm.Lock()
	defer rm.Unlock()
	factory, found := AdapterFactories.Lookup(route.AdapterType())
	if !found {
		return errors.New("bad adapter: " + route.Adapter)
	}
	name, explicit := route.Name, route.NameExplicit
	if name == "" {
		name, explicit = route.AdapterType(), false
	} else if !ValidRouteName(name) {
		log.Printf("warning: route %s (%s %s) has an invalid name %q; the name is ignored and the default name is used", route.ID, route.Adapter, route.Address, name)
		name, explicit = route.AdapterType(), false
	} else if name != route.AdapterType() {
		// API clients send a name without name_explicit.
		explicit = true
	}
	adapter, err := factory(route)
	if err != nil {
		return err
	}
	rm.warnClash(route.ID, name, explicit)
	route.Name, route.NameExplicit = name, explicit
	if route.ID == "" {
		h := sha1.New() //nolint:gosec
		io.WriteString(h, strconv.Itoa(int(time.Now().UnixNano())))
		route.ID = fmt.Sprintf("%x", h.Sum(nil))[:12]
	}
	route.closer = make(chan struct{})
	route.adapter = adapter
	// Stop any existing route with this ID:
	if rm.routes[route.ID] != nil {
		rm.routes[route.ID].closer <- struct{}{}
	}

	rm.routes[route.ID] = route
	rm.updateAmbiguous()
	if rm.persistor != nil {
		if err := rm.persistor.Add(route); err != nil {
			log.Println("persistor:", err)
		}
	}
	if rm.routing {
		go rm.route(route)
	}
	return nil
}

// warnClash logs when other routes already have the same name and at least one
// of the two names is explicit. Two default names stay quiet: existing installs
// often have two routes of one type. The route is still added: the name becomes
// ambiguous and a rule file cannot target it. The caller must hold the lock.
func (rm *RouteManager) warnClash(routeID, name string, explicit bool) {
	for id, other := range rm.routes {
		if other.Name == name && id != routeID && (explicit || other.NameExplicit) {
			log.Printf("warning: more than one route has the name %q; add a unique #name to the route URI so rules can target it", name)
			return
		}
	}
}

// updateAmbiguous recomputes the ambiguous names. The caller must hold the lock.
func (rm *RouteManager) updateAmbiguous() {
	count := map[string]int{}
	set := map[string]bool{}
	for _, r := range rm.routes {
		if count[r.Name]++; count[r.Name] == 2 {
			set[r.Name] = true
		}
	}
	rm.ambiguous.Store(&set)
	rm.warned.Range(func(k, _ any) bool {
		if !set[k.(string)] {
			rm.warned.Delete(k)
		}
		return true
	})
}

// AmbiguousName reports whether more than one route has the given name.
func (rm *RouteManager) AmbiguousName(name string) bool {
	set := rm.ambiguous.Load()
	return set != nil && (*set)[name]
}

// targetMessage applies the target rules of the route. Target rules are
// validated against the configured routes only, so a rule for a name that
// more than one route has (for example an API route that reuses a name) must
// not apply to any of them: the message goes out unchanged by target rules.
func (rm *RouteManager) targetMessage(proc Processor, route *Route, msg *Message) (*Message, bool) {
	if !rm.AmbiguousName(route.Name) {
		return proc.Target(route.Name, msg)
	}
	if rm.hasSkippedTarget(proc, route.Name, msg) {
		if _, seen := rm.warned.LoadOrStore(route.Name, true); !seen {
			log.Printf("warning: more than one route has the name %q, so the target rules for this name are not applied", route.Name)
		}
	}
	return msg, false
}

// targetSkipper is implemented by a processor that can tell, without running
// the rules, that target rules exist for a name (and trace that they are skipped).
type targetSkipper interface {
	SkipTarget(routeName string, m *Message) (hasRules bool)
}

func (rm *RouteManager) hasSkippedTarget(proc Processor, name string, msg *Message) bool {
	if s, ok := proc.(targetSkipper); ok {
		return s.SkipTarget(name, msg)
	}
	// Target does not change msg (copy on write); a different result means rules exist for this name.
	out, dropped := proc.Target(name, msg)
	return dropped || out != msg
}

func (rm *RouteManager) route(route *Route) {
	logstream := make(chan *Message)
	defer route.Close()
	rm.Route(route, logstream)
	route.adapter.Stream(logstream)
}

// Route takes a logstream and route and passes them off to all configure LogRouters
func (rm *RouteManager) Route(route *Route, logstream chan *Message) {
	for _, router := range LogRouters.All() {
		go router.Route(route, logstream)
	}
}

// RoutingFrom returns whether a given container is routing through the RouteManager
func (rm *RouteManager) RoutingFrom(containerID string) bool {
	for _, router := range LogRouters.All() {
		if router.RoutingFrom(containerID) {
			return true
		}
	}
	return false
}

// Run executes the RouteManager
func (rm *RouteManager) Run() error {
	rm.Lock()
	for _, route := range rm.routes {
		rm.wg.Add(1)
		go func(route *Route) {
			rm.route(route)
			rm.wg.Done()
		}(route)
	}
	rm.routing = true
	rm.Unlock()
	rm.wg.Wait()
	// Temp fix to allow logspout to run without routes defined.
	if len(rm.routes) == 0 {
		select {}
	}
	return nil
}

// Name returns the name of the RouteManager
func (rm *RouteManager) Name() string {
	return "routes"
}

// Setup configures the RouteManager
func (rm *RouteManager) Setup() error {
	return rm.SetupWithArgs(os.Args[1:], nil)
}

// SetupWithArgs configures routes from either explicit route URIs or the
// legacy environment/CLI inputs when no explicit routes are provided.
func (rm *RouteManager) SetupWithArgs(args []string, explicitURIs []string) error {
	for _, uri := range resolveRouteURIs(args, explicitURIs) {
		if err := rm.AddFromURI(uri); err != nil {
			return err
		}
	}

	persistPath := cfg.GetEnvDefault("ROUTESPATH", "/mnt/routes")
	if _, err := os.Stat(persistPath); err == nil {
		return rm.Load(RouteFileStore(persistPath))
	}
	return nil
}

func resolveRouteURIs(args []string, explicitURIs []string) []string {
	if explicitURIs != nil {
		return explicitURIs
	}

	var uris string
	if env := os.Getenv("ROUTE_URIS"); env != "" {
		uris = env
	}
	if len(args) > 0 {
		uris = args[0]
	}
	if uris == "" {
		return nil
	}
	return strings.Split(uris, ",")
}
