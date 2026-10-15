package httpclient

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// holdWithoutHeader is the first hold for a 429 that carries no usable
	// Retry-After; each further such 429 after a hold expires doubles it.
	holdWithoutHeader    = time.Minute
	maxHoldWithoutHeader = 15 * time.Minute //nolint:mnd // doubling stops here
	// maxHold caps what an upstream's Retry-After can ask for.
	maxHold = time.Hour
)

// ErrUpstreamHeld marks a request the gate did not send because the upstream
// is rate limiting this proxy. Match it with errors.Is; the *HeldError in
// the chain carries the host and how long to wait.
var ErrUpstreamHeld = errors.New("upstream is rate limiting this proxy")

// HeldError is returned in place of a response while a host is held.
type HeldError struct {
	Host       string
	Until      time.Time
	RetryAfter time.Duration
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("%s is rate limiting this proxy, holding requests for %s", e.Host, e.RetryAfter.Round(time.Second))
}

func (e *HeldError) Is(target error) bool { return target == ErrUpstreamHeld }

type hostHold struct {
	until   time.Time
	strikes int
}

// RateLimitGate stops sending to a host that answered 429, or 503 with a
// Retry-After, for as long as it asked, and fails those requests locally
// with a *HeldError instead. Clients and fetchers above it see an error
// rather than a slow upstream, so they fall back to cache or fail fast
// without adding to the traffic the upstream is already refusing. The first
// non-limited response after the hold ends releases it.
type RateLimitGate struct {
	base   http.RoundTripper
	Logger *slog.Logger
	// OnHold is called with the host each time a hold starts.
	OnHold func(host string)
	now    func() time.Time

	mu    sync.Mutex
	holds map[string]*hostHold
}

// NewRateLimitGate wraps base. A nil base uses http.DefaultTransport.
func NewRateLimitGate(base http.RoundTripper) *RateLimitGate {
	if base == nil {
		base = http.DefaultTransport
	}
	return &RateLimitGate{
		base:  base,
		now:   time.Now,
		holds: make(map[string]*hostHold),
	}
}

// RoundTrip implements http.RoundTripper.
func (g *RateLimitGate) RoundTrip(req *http.Request) (*http.Response, error) {
	host := hostKey(req.URL)
	if until, held := g.heldUntil(host); held {
		return nil, &HeldError{Host: host, Until: until, RetryAfter: until.Sub(g.now())}
	}

	resp, err := g.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	retryAfter := resp.Header.Get("Retry-After")
	switch {
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusServiceUnavailable && retryAfter != "":
		g.hold(host, retryAfter)
	default:
		g.release(host)
	}
	return resp, nil
}

// Held returns a *HeldError when requests to rawURL's host are being held,
// and nil otherwise. It is safe to call on a nil gate.
func (g *RateLimitGate) Held(rawURL string) error {
	if g == nil {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	host := hostKey(u)
	until, held := g.heldUntil(host)
	if !held {
		return nil
	}
	return &HeldError{Host: host, Until: until, RetryAfter: until.Sub(g.now())}
}

// Holds returns the hosts currently held and when each hold ends. It is
// safe to call on a nil gate.
func (g *RateLimitGate) Holds() map[string]time.Time {
	if g == nil {
		return nil
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	held := make(map[string]time.Time)
	for host, h := range g.holds {
		if now.Before(h.until) {
			held[host] = h.until
		}
	}
	return held
}

func (g *RateLimitGate) heldUntil(host string) (time.Time, bool) {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	h, ok := g.holds[host]
	if !ok || !now.Before(h.until) {
		return time.Time{}, false
	}
	return h.until, true
}

func (g *RateLimitGate) hold(host, header string) {
	now := g.now()

	g.mu.Lock()
	h, ok := g.holds[host]
	if !ok {
		h = &hostHold{}
		g.holds[host] = h
	}
	// Responses already in flight when a hold starts all report the same
	// rate limiting; only a 429 after the hold ran out is a further strike.
	if !now.Before(h.until) {
		h.strikes++
	}
	d, fromHeader := parseRetryAfter(header, now)
	if !fromHeader {
		d = min(holdWithoutHeader<<(h.strikes-1), maxHoldWithoutHeader)
	}
	d = min(d, maxHold)
	until := now.Add(d)
	if until.After(h.until) {
		h.until = until
	}
	g.mu.Unlock()

	if g.Logger != nil {
		source := "default"
		if fromHeader {
			source = "retry-after"
		}
		g.Logger.Warn("upstream rate limited this proxy, holding its requests",
			"host", host, "hold", d.Round(time.Second).String(), "source", source,
			"until", until.UTC().Format(time.RFC3339))
	}
	if g.OnHold != nil {
		g.OnHold(host)
	}
}

// release drops host's hold once it has ended. Nothing is sent during a
// hold, so a success arriving while one is active was sent before it began.
func (g *RateLimitGate) release(host string) {
	now := g.now()
	g.mu.Lock()
	h, had := g.holds[host]
	if had && now.Before(h.until) {
		g.mu.Unlock()
		return
	}
	delete(g.holds, host)
	g.mu.Unlock()
	if had && g.Logger != nil {
		g.Logger.Info("upstream rate limit cleared", "host", host)
	}
}

// parseRetryAfter reads a Retry-After value as delay seconds or an HTTP
// date. It reports false for anything unusable, including a zero or past
// value, so the caller applies its default instead.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(value, 10, 64); err == nil {
		if secs <= 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	at, err := http.ParseTime(value)
	if err != nil || !at.After(now) {
		return 0, false
	}
	return at.Sub(now), true
}

func hostKey(u *url.URL) string {
	if u == nil {
		return ""
	}
	return strings.ToLower(u.Host)
}
