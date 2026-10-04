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

func routeNameWithEnv(uri string, getenv func(string) string) (string, bool, error) {
	u, err := url.Parse(os.Expand(uri, getenv))
	if err != nil {
		return "", false, err
	}
	return nameFromURL(u)
}

func nameFromURL(u *url.URL) (string, bool, error) {
	if u.Fragment == "" {
		return strings.Split(u.Scheme, "+")[0], false, nil
	}
	if !ValidRouteName(u.Fragment) {
		return "", true, fmt.Errorf("invalid route name %q: use only letters, digits, '_', '.' and '-'", u.Fragment)
	}
	return u.Fragment, true, nil
}

// ValidateRouteNames checks the route names of a list of URIs. A duplicate
// explicit name is an error. Duplicate default names (adapter types) are
// returned in ambiguous; they stay allowed for backward compatibility.
func ValidateRouteNames(uris []string) (ambiguous []string, err error) {
	return ValidateRouteNamesWithEnv(uris, os.Getenv)
}

// ValidateRouteNamesWithEnv is like ValidateRouteNames but expands ${VAR}
// in the URIs with getenv, so callers can match the environment that will
// be in place when the routes are added.
func ValidateRouteNamesWithEnv(uris []string, getenv func(string) string) (ambiguous []string, err error) {
	explicitCount := map[string]int{}
	defaultCount := map[string]int{}
	var order []string
	for i, uri := range uris {
		name, explicit, err := routeNameWithEnv(uri, getenv)
		if err != nil {
			return nil, fmt.Errorf("routes[%d]: %w", i, err)
		}
		if explicit {
			explicitCount[name]++
		} else {
			if defaultCount[name] == 1 {
				order = append(order, name)
			}
			defaultCount[name]++
		}
		if explicitCount[name] > 1 || (explicitCount[name] > 0 && defaultCount[name] > 0) {
			return nil, fmt.Errorf("routes[%d]: route name %q is used more than once, add a unique #name to the route URI", i, name)
		}
	}
	return order, nil
}
