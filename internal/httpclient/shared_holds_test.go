package httpclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	registryclient "github.com/git-pkgs/registries/client"
)

// memoryHoldStore is a HoldStore shared by gates in one test, counting reads.
type memoryHoldStore struct {
	mu    sync.Mutex
	holds map[string]time.Time
	reads atomic.Int32
}

func (m *memoryHoldStore) PutUpstreamHold(host string, until time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if until.After(m.holds[host]) {
		m.holds[host] = until
	}
	return nil
}

func (m *memoryHoldStore) UpstreamHolds(now time.Time) (map[string]time.Time, error) {
	m.reads.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]time.Time)
	for host, until := range m.holds {
		if now.Before(until) {
			out[host] = until
		}
	}
	return out, nil
}

func TestSharedRateLimitGateSharesHolds(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	store := &memoryHoldStore{holds: make(map[string]time.Time)}
	a := NewSharedRateLimitGate(registryclient.NewRateLimitGate(upstream.Client().Transport), store, nil)
	b := NewSharedRateLimitGate(registryclient.NewRateLimitGate(upstream.Client().Transport), store, nil)
	clock := time.Now()
	b.now = func() time.Time { return clock }

	// b reads the store before a is rate limited, and keeps that copy for
	// Refresh.
	if err := b.Held(upstream.URL); err != nil {
		t.Fatalf("b held before any 429: %v", err)
	}
	resp, err := a.RoundTrip(httptest.NewRequest(http.MethodGet, upstream.URL, nil))
	if err != nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a: %v, %v; want the 429", resp, err)
	}
	_ = resp.Body.Close()
	if err := b.Held(upstream.URL); err != nil {
		t.Errorf("b refreshed before Refresh ran out: %v", err)
	}

	clock = clock.Add(DefaultHoldRefresh)
	_, err = b.RoundTrip(httptest.NewRequest(http.MethodGet, upstream.URL, nil))
	var held *registryclient.HeldError
	if !errors.As(err, &held) || held.RetryAfter <= 100*time.Second {
		t.Fatalf("b after refresh: %v, want a hold of about two minutes", err)
	}
	if calls.Load() != 1 {
		t.Errorf("upstream requests = %d, want 1", calls.Load())
	}
	if holds := b.Holds(); len(holds) != 1 {
		t.Errorf("b.Holds() = %v, want a's hold", holds)
	}
	// One read by a before its request, two by b: the first and the one
	// after Refresh.
	if n := store.reads.Load(); n != 3 {
		t.Errorf("store reads = %d, want 3", n)
	}

	// Once the hold ends, b sends again.
	clock = clock.Add(2 * time.Minute)
	if err := b.Held(upstream.URL); err != nil {
		t.Errorf("b after the hold ended: %v", err)
	}
}

func TestSharedRateLimitGateReload(t *testing.T) {
	store := &memoryHoldStore{holds: make(map[string]time.Time)}
	g := NewSharedRateLimitGate(registryclient.NewRateLimitGate(nil), store, nil)
	g.Refresh = time.Hour
	if err := g.Held("https://registry.example/x"); err != nil {
		t.Fatal(err)
	}
	_ = store.PutUpstreamHold("registry.example", time.Now().Add(time.Minute))
	if err := g.Held("https://registry.example/x"); err != nil {
		t.Fatalf("held before reload: %v", err)
	}
	g.Reload()
	if !errors.Is(g.Held("https://registry.example/x"), registryclient.ErrUpstreamHeld) {
		t.Error("not held after reload")
	}
}

func TestSharedRateLimitGateWithoutStore(t *testing.T) {
	g := NewSharedRateLimitGate(registryclient.NewRateLimitGate(nil), nil, nil)
	if err := g.Held("https://registry.example/x"); err != nil {
		t.Fatal(err)
	}
	g.Reload()
	var nilGate *SharedRateLimitGate
	if nilGate.Held("https://registry.example/x") != nil || nilGate.Holds() != nil {
		t.Error("nil gate holds")
	}
}
