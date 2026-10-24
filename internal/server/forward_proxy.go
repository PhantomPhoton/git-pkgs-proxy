package server

import (
	"context"
	"net"
	"net/http"
	"net/url"

	"github.com/git-pkgs/registries/safehttp"
)

// upstreamForwardProxy picks the forward proxy for an upstream request. It is
// a variable so tests can route through a test proxy without changing the
// environment, which net/http reads only once.
var upstreamForwardProxy = http.ProxyFromEnvironment

// lookupUpstreamHost resolves a destination host for the forward proxy check.
var lookupUpstreamHost = net.DefaultResolver.LookupIPAddr

// guardForwardProxy wraps proxy so that a request sent through a forward
// proxy is held to the same address rules as a direct one. The safehttp dial
// gate only sees the address it connects to, which for a proxied request is
// the forward proxy, so without this a URL taken from upstream metadata could
// reach a link-local or private address through the forward proxy.
//
// The destination is resolved here and every address must pass. A host that
// cannot be resolved locally is passed on, since setups that route all
// traffic through a forward proxy often cannot resolve public names
// themselves; the forward proxy resolves it.
func guardForwardProxy(proxy func(*http.Request) (*url.URL, error), opts safehttp.Options) func(*http.Request) (*url.URL, error) {
	if proxy == nil {
		return nil
	}
	checker := safehttp.NewHostIPChecker(opts)
	return func(req *http.Request) (*url.URL, error) {
		proxyURL, err := proxy(req)
		if err != nil || proxyURL == nil {
			return proxyURL, err
		}
		if err := checkProxiedDestination(req.Context(), checker, req.URL.Hostname()); err != nil {
			return nil, err
		}
		return proxyURL, nil
	}
}

func checkProxiedDestination(ctx context.Context, checker *safehttp.HostIPChecker, host string) error {
	if ip := net.ParseIP(host); ip != nil {
		return checker.Check(host, ip)
	}
	addrs, err := lookupUpstreamHost(ctx, host)
	if err != nil {
		return nil //nolint:nilerr // the forward proxy resolves names this host cannot
	}
	for _, addr := range addrs {
		if err := checker.Check(host, addr.IP); err != nil {
			return err
		}
	}
	return nil
}
