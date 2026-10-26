package handler

import (
	"errors"
	"math"
	"net/http"
	"strconv"

	registryclient "github.com/git-pkgs/registries/client"
)

// writeUpstreamHeld answers a request the rate limit gate refused to send
// with 503 and the Retry-After the upstream asked for, and reports whether
// err was such a refusal.
func writeUpstreamHeld(w http.ResponseWriter, err error) bool {
	var held *registryclient.HeldError
	if !errors.As(err, &held) {
		return false
	}
	secs := int(math.Ceil(held.RetryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	http.Error(w, held.Error(), http.StatusServiceUnavailable)
	return true
}

// UpstreamHolds reports whether requests to a URL's host are held by rate
// limiting. *registryclient.RateLimitGate and the shared gate in
// internal/httpclient implement it.
type UpstreamHolds interface {
	// Held returns a *registryclient.HeldError while rawURL's host is held.
	Held(rawURL string) error
}

// upstreamHeld returns the hold on rawURL's host, if any.
func (p *Proxy) upstreamHeld(rawURL string) error {
	if p.RateLimits == nil {
		return nil
	}
	return p.RateLimits.Held(rawURL)
}

// reloadUpstreamHolds rereads holds shared with other instances, when the
// gate shares them. It is called after another instance's fetch failed,
// since a 429 there will have placed a hold this instance may not have read
// yet.
func (p *Proxy) reloadUpstreamHolds() {
	if r, ok := p.RateLimits.(interface{ Reload() }); ok {
		r.Reload()
	}
}
