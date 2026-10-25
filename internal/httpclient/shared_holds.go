package httpclient

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	registryclient "github.com/git-pkgs/registries/client"
)

// DefaultHoldRefresh is how long a SharedRateLimitGate trusts its copy of
// the shared holds before reading them again. It bounds both the database
// reads, at one per interval per instance, and how long another instance
// may keep sending after a hold is placed.
const DefaultHoldRefresh = 2 * time.Second

// HoldStore keeps rate-limit holds where every instance sharing it sees them.
type HoldStore interface {
	PutUpstreamHold(host string, until time.Time) error
	UpstreamHolds(now time.Time) (map[string]time.Time, error)
}

// SharedRateLimitGate adds holds shared with other instances to a
// RateLimitGate. Holds the gate places are published to the store, and
// requests to a host another instance holds are refused like the gate's own,
// with a HeldError. Without a store it behaves as the gate alone.
type SharedRateLimitGate struct {
	gate   *registryclient.RateLimitGate
	store  HoldStore
	logger *slog.Logger
	now    func() time.Time

	// Refresh is how long the shared holds read from the store are used
	// before reading them again. Set it before use.
	Refresh time.Duration

	mu     sync.Mutex
	shared map[string]time.Time
	// published holds the holds this instance wrote, so a read that started
	// before a write does not drop it.
	published map[string]time.Time
	loaded    time.Time
	loading   bool
}

// NewSharedRateLimitGate wraps gate, publishing its holds to store and
// honouring those in store. It takes over gate.OnHold, calling the callback
// set before. A nil store shares nothing.
func NewSharedRateLimitGate(gate *registryclient.RateLimitGate, store HoldStore, logger *slog.Logger) *SharedRateLimitGate {
	s := &SharedRateLimitGate{
		gate:      gate,
		store:     store,
		logger:    logger,
		now:       time.Now,
		Refresh:   DefaultHoldRefresh,
		shared:    make(map[string]time.Time),
		published: make(map[string]time.Time),
	}
	if store != nil {
		onHold := gate.OnHold
		gate.OnHold = func(event registryclient.HoldEvent) {
			s.publish(event.Host, event.Until)
			if onHold != nil {
				onHold(event)
			}
		}
	}
	return s
}

// RoundTrip implements http.RoundTripper.
func (s *SharedRateLimitGate) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := s.sharedHeld(hostKey(req.URL)); err != nil {
		return nil, err
	}
	return s.gate.RoundTrip(req)
}

// Held returns a *registryclient.HeldError when requests to rawURL's host are
// held by this instance or another, and nil otherwise. It is safe to call on
// a nil gate.
func (s *SharedRateLimitGate) Held(rawURL string) error {
	if s == nil {
		return nil
	}
	if err := s.gate.Held(rawURL); err != nil {
		return err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	return s.sharedHeld(hostKey(u))
}

// Holds returns the hosts held by this instance or another, and when each
// hold ends. It is safe to call on a nil gate.
func (s *SharedRateLimitGate) Holds() map[string]time.Time {
	if s == nil {
		return nil
	}
	holds := s.gate.Holds()
	if s.store == nil {
		return holds
	}
	s.load(false)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for host, until := range s.shared {
		if now.Before(until) && until.After(holds[host]) {
			if holds == nil {
				holds = make(map[string]time.Time)
			}
			holds[host] = until
		}
	}
	return holds
}

// Reload reads the shared holds now rather than when the copy runs out. A
// caller that knows another instance has just failed, say on a 429, uses it
// to see that instance's hold at once.
func (s *SharedRateLimitGate) Reload() {
	if s == nil || s.store == nil {
		return
	}
	s.load(true)
}

func (s *SharedRateLimitGate) sharedHeld(host string) error {
	if s.store == nil {
		return nil
	}
	s.load(false)
	now := s.now()
	s.mu.Lock()
	until, ok := s.shared[host]
	s.mu.Unlock()
	if !ok || !now.Before(until) {
		return nil
	}
	return &registryclient.HeldError{Host: host, Until: until, RetryAfter: until.Sub(now)}
}

// load refreshes the copy of the shared holds once it is older than Refresh,
// or at once when force is set. One caller reads the store while concurrent
// callers go on with the copy they have; a forced read does not wait for
// one already running, which may have started before the hold it is after.
func (s *SharedRateLimitGate) load(force bool) {
	now := s.now()
	s.mu.Lock()
	if !force && (s.loading || now.Sub(s.loaded) < s.Refresh) {
		s.mu.Unlock()
		return
	}
	s.loading = true
	s.mu.Unlock()

	holds, err := s.store.UpstreamHolds(now)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.loading = false
	// A failed read keeps the last copy and waits a full interval before
	// trying again, so a database outage costs no more queries than usual.
	s.loaded = now
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("reading shared upstream holds failed", "error", err)
		}
		if holds == nil {
			return
		}
	}
	for host, until := range s.published {
		if !now.Before(until) {
			delete(s.published, host)
		} else if until.After(holds[host]) {
			holds[host] = until
		}
	}
	s.shared = holds
}

// publish records a hold this instance placed, locally and in the store. The
// responses already in flight when a hold starts each report it again; only a
// later end is written.
func (s *SharedRateLimitGate) publish(host string, until time.Time) {
	s.mu.Lock()
	if !until.After(s.published[host]) {
		s.mu.Unlock()
		return
	}
	s.published[host] = until
	if until.After(s.shared[host]) {
		s.shared[host] = until
	}
	s.mu.Unlock()
	if err := s.store.PutUpstreamHold(host, until); err != nil && s.logger != nil {
		s.logger.Warn("publishing upstream hold failed", "host", host, "error", err)
	}
}

func hostKey(u *url.URL) string {
	if u == nil {
		return ""
	}
	return strings.ToLower(u.Host)
}
