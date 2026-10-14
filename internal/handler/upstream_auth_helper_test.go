package handler

import (
	"net/http"

	upstreamhttp "github.com/git-pkgs/proxy/internal/httpclient"
)

// authorizeUpstream sends authorization on every upstream request the way the
// server does: through the auth transport, which scopes it to each hop.
func authorizeUpstream(proxy *Proxy, authorization string) {
	base := proxy.HTTPClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client := *proxy.HTTPClient
	client.Transport = upstreamhttp.NewTransport(base, func(string) (string, string) {
		return "Authorization", authorization
	})
	proxy.HTTPClient = &client
}
