package router

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

var routeNameRegexp = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ValidRouteName reports whether name is usable as an explicit route name.
func ValidRouteName(name string) bool {
	return routeNameRegexp.MatchString(name)
}

// RouteName resolves the name of a route URI: the `#name` fragment, or the
// adapter type when there is no fragment. explicit tells which one applied.
func RouteName(uri string) (name string, explicit bool, err error) {
	return routeNameWithEnv(uri, os.Getenv)
}

// RouteNameWithEnv is like RouteName but expands ${VAR} with getenv.
func RouteNameWithEnv(uri string, getenv func(string) string) (name string, explicit bool, err error) {
	return routeNameWithEnv(uri, getenv)
}

func routeNameWithEnv(uri string, getenv func(string) string) (string, bool, error) {
	u, err := url.Parse(os.Expand(uri, getenv))
	if err != nil {
		return "", false, err
	}
	return nameFromURL(u)
}

func nameFromURL(u *url.URL) (string, bool, error) {
	if u.Fragment == "" {
		return defaultRouteName(u), false, nil
	}
	if !ValidRouteName(u.Fragment) {
		return "", true, fmt.Errorf("invalid route name %q: use only letters, digits, '_', '.' and '-'", u.Fragment)
	}
	return u.Fragment, true, nil
}

func defaultRouteName(u *url.URL) string {
	return strings.Split(u.Scheme, "+")[0]
}

// effectiveName is the name a route gets: a valid fragment, else the default
// name. invalid is set when a fragment was ignored. Before route names existed
// a fragment was ignored, so a bad one must not stop a route.
func effectiveName(u *url.URL) (name string, explicit bool, invalid error) {
	name, explicit, err := nameFromURL(u)
	if err != nil {
		return defaultRouteName(u), false, err
	}
	return name, explicit, nil
}

// EffectiveRouteNameWithEnv is the name that AddFromURI gives the route of
// uri, after expanding ${VAR} with getenv. Only an unparsable URI gives an error.
func EffectiveRouteNameWithEnv(uri string, getenv func(string) string) (string, error) {
	u, err := url.Parse(os.Expand(uri, getenv))
	if err != nil {
		return "", err
	}
	name, _, _ := effectiveName(u)
	return name, nil
}

// AmbiguousRouteNames returns the names that more than one route of the URIs
// has. Such a name cannot be a target in the rule file. It never fails because
// of a name; only a URI that cannot be parsed gives an error.
func AmbiguousRouteNames(uris []string) ([]string, error) {
	return AmbiguousRouteNamesWithEnv(uris, os.Getenv)
}

// AmbiguousRouteNamesWithEnv is like AmbiguousRouteNames but expands ${VAR}
// in the URIs with getenv, so callers can match the environment that will
// be in place when the routes are added.
func AmbiguousRouteNamesWithEnv(uris []string, getenv func(string) string) ([]string, error) {
	count := map[string]int{}
	var ambiguous []string
	for i, uri := range uris {
		name, err := EffectiveRouteNameWithEnv(uri, getenv)
		if err != nil {
			return nil, fmt.Errorf("routes[%d]: %w", i, err)
		}
		if count[name]++; count[name] == 2 {
			ambiguous = append(ambiguous, name)
		}
	}
	return ambiguous, nil
}
