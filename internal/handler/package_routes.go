package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"sort"
	"strings"
)

// routeIDLength is the number of hex characters of the upstream URL digest
// that qualify cache entries of a routed package.
const routeIDLength = 16

// packageRoutes sends packages whose names match a pattern to a dedicated
// upstream instead of the ecosystem's default registry. A matched package is
// never looked up anywhere else, so a public package with the same name cannot
// replace it.
type packageRoutes struct {
	routes []packageRoute // longest pattern first
}

type packageRoute struct {
	pattern string
	url     string
	id      string
}

func newPackageRoutes(patterns map[string]string) packageRoutes {
	routes := make([]packageRoute, 0, len(patterns))
	for pattern, upstreamURL := range patterns {
		upstreamURL = strings.TrimSuffix(upstreamURL, "/")
		routes = append(routes, packageRoute{
			pattern: strings.ToLower(pattern),
			url:     upstreamURL,
			id:      routeID(upstreamURL),
		})
	}
	sort.Slice(routes, func(i, j int) bool {
		if len(routes[i].pattern) != len(routes[j].pattern) {
			return len(routes[i].pattern) > len(routes[j].pattern)
		}
		return routes[i].pattern < routes[j].pattern
	})
	return packageRoutes{routes: routes}
}

// routeID identifies a route's upstream in cache keys and filenames.
func routeID(upstreamURL string) string {
	digest := sha256.Sum256([]byte(strings.TrimSuffix(upstreamURL, "/")))
	return hex.EncodeToString(digest[:])[:routeIDLength]
}

// RoutedFilenamePrefix returns what the npm and Composer handlers put in
// front of the artifact filenames of a package routed to routeURL.
func RoutedFilenamePrefix(routeURL string) string {
	return routeID(routeURL) + "/"
}

// match returns the route for a package name, if any pattern matches it.
func (r packageRoutes) match(packageName string) (packageRoute, bool) {
	name := strings.ToLower(packageName)
	for _, route := range r.routes {
		if ok, _ := path.Match(route.pattern, name); ok {
			return route, true
		}
	}
	return packageRoute{}, false
}

// cacheKey qualifies a package's metadata cache key with the route's upstream,
// so entries cached from the default registry, or from a route's previous
// upstream, are never served for a routed package.
func (r packageRoute) cacheKey(packageName string) string {
	return "routes/" + r.id + "/" + packageName
}

// cacheFilename qualifies a routed package's artifact filename the same way as
// cacheKey, and leaves other packages' filenames unchanged.
func (r packageRoutes) cacheFilename(packageName, filename string) string {
	if route, routed := r.match(packageName); routed {
		return route.id + "/" + filename
	}
	return filename
}

// RoutedPackageURL returns the upstream URL a route in routes sends
// packageName to, as the npm and Composer handlers pick it.
func RoutedPackageURL(routes map[string]string, packageName string) (string, bool) {
	route, ok := newPackageRoutes(routes).match(packageName)
	return route.url, ok
}
