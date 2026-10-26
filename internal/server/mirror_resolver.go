package server

import (
	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/registries/fetch"
)

// newMirrorResolver builds the resolver proxy mirror and mirror jobs find
// download URLs with, using the configured upstreams. Handlers serving
// clients don't use it; they take the URLs from upstream metadata.
func newMirrorResolver(u config.UpstreamConfig) *fetch.Resolver {
	return fetch.NewResolver(fetch.WithBaseURLs(map[string]string{
		"npm":    u.NPM,
		"cargo":  u.CargoDownload,
		"gem":    u.Gem,
		"golang": u.Go,
		"hex":    u.Hex,
		"pub":    u.Pub,
		"maven":  u.Maven,
		"nuget":  u.NuGet,
	}))
}

// configureMirrorResolvers points proxy's resolver at the configured
// upstreams, and npm packages in upstream.npm_routes at their route, so a
// public package can't stand in for a routed one.
func configureMirrorResolvers(proxy *handler.Proxy, u config.UpstreamConfig) {
	proxy.Resolver = newMirrorResolver(u)
	if len(u.NPMRoutes) == 0 {
		return
	}
	routed := make(map[string]*handler.ArtifactRoute, len(u.NPMRoutes))
	for _, routeURL := range u.NPMRoutes {
		routeUpstream := u
		routeUpstream.NPM = routeURL
		routed[routeURL] = &handler.ArtifactRoute{
			Resolver:       newMirrorResolver(routeUpstream),
			FilenamePrefix: handler.RoutedFilenamePrefix(routeURL),
		}
	}
	proxy.RouteFor = func(ecosystem, name string) *handler.ArtifactRoute {
		if ecosystem != "npm" {
			return nil
		}
		routeURL, ok := handler.RoutedPackageURL(u.NPMRoutes, name)
		if !ok {
			return nil
		}
		return routed[routeURL]
	}
}
