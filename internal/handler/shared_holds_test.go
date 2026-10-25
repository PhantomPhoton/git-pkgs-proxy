package handler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	upstreamhttp "github.com/git-pkgs/proxy/internal/httpclient"
	"github.com/git-pkgs/proxy/internal/metrics"
	registryclient "github.com/git-pkgs/registries/client"
	"github.com/git-pkgs/registries/fetch"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// limitedUpstream answers every request with 429 and a Retry-After of ten
// minutes. Requests wait for release, which is open unless a test closes it
// later.
type limitedUpstream struct {
	server   *httptest.Server
	calls    atomic.Int32
	entered  chan struct{}
	release  chan struct{}
	released sync.Once
}

func newLimitedUpstream(t *testing.T, blocked bool) *limitedUpstream {
	u := &limitedUpstream{entered: make(chan struct{}, 8), release: make(chan struct{})}
	if !blocked {
		u.open()
	}
	t.Cleanup(u.open)
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		u.calls.Add(1)
		u.entered <- struct{}{}
		<-u.release
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *limitedUpstream) open() { u.released.Do(func() { close(u.release) }) }

func (u *limitedUpstream) artifactURL() string { return u.server.URL + "/left-pad-1.3.0.tgz" }

// useSharedGate routes p's upstream requests through its own rate limit gate,
// sharing holds through db as a replica does.
func useSharedGate(p *Proxy, db *database.DB, u *limitedUpstream) *upstreamhttp.SharedRateLimitGate {
	gate := upstreamhttp.NewSharedRateLimitGate(registryclient.NewRateLimitGate(u.server.Client().Transport), db, nil)
	client := &http.Client{Transport: gate}
	p.HTTPClient = client
	p.Fetcher = fetch.NewFetcher(fetch.WithHTTPClient(client), fetch.WithBaseDelay(time.Millisecond))
	p.RateLimits = gate
	return gate
}

func fetchLimited(ctx context.Context, p *Proxy, u *limitedUpstream) error {
	res, err := p.GetOrFetchArtifactFromURL(ctx, "npm", "left-pad", "1.3.0", "left-pad-1.3.0.tgz", u.artifactURL())
	if err == nil {
		_ = res.Reader.Close()
	}
	return err
}

// requireHeld checks that err is answered like any held upstream request,
// with 503 and a Retry-After.
func requireHeld(t *testing.T, name string, err error) {
	t.Helper()
	w := httptest.NewRecorder()
	if !writeUpstreamHeld(w, err) {
		t.Fatalf("%s: err = %v, want a held upstream", name, err)
	}
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("%s: status %d, Retry-After %q; want 503 with Retry-After", name, w.Code, w.Header().Get("Retry-After"))
	}
}

// TestSharedHoldStopsOtherInstance checks that once one instance is rate
// limited, another sharing its database refuses requests to that host
// without sending them, until the hold ends.
func TestSharedHoldStopsOtherInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	dbA, err := database.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbA.Close() })
	dbB, err := database.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbB.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := newMockStorage()
	a := NewProxy(dbA, store, &mockFetcher{}, fetch.NewResolver(), logger)
	b := NewProxy(dbB, store, &mockFetcher{}, fetch.NewResolver(), logger)

	u := newLimitedUpstream(t, false)
	useSharedGate(a, dbA, u)
	gateB := useSharedGate(b, dbB, u)
	gateB.Refresh = 0

	if err := fetchLimited(context.Background(), a, u); err == nil {
		t.Fatal("instance a: want the rate limit error")
	}
	if got := u.calls.Load(); got != 1 {
		t.Fatalf("upstream requests after a = %d, want 1", got)
	}

	requireHeld(t, "instance b artifact", fetchLimited(context.Background(), b, u))
	_, _, err = b.FetchOrCacheMetadata(context.Background(), "npm", "left-pad", u.server.URL+"/left-pad", "application/json")
	if !errors.Is(err, registryclient.ErrUpstreamHeld) {
		t.Errorf("instance b metadata: err = %v, want a held upstream", err)
	}
	if got := u.calls.Load(); got != 1 {
		t.Errorf("upstream requests after b = %d, want 1", got)
	}

	// The hold ends when the upstream said, and is then deleted.
	holds, err := dbB.UpstreamHolds(time.Now().Add(11 * time.Minute))
	if err != nil || len(holds) != 0 {
		t.Fatalf("holds after expiry = %v, %v; want none", holds, err)
	}
	if err := b.upstreamHeld(u.artifactURL()); err != nil {
		t.Errorf("instance b after expiry: %v, want no hold", err)
	}
}

// TestFetchLeaseRateLimitedHolderStopsWaiters checks that instances waiting
// on a fetch that was rate limited answer with the holder's Retry-After
// instead of each sending one more request to the limited host.
func TestFetchLeaseRateLimitedHolderStopsWaiters(t *testing.T) {
	a, b, db := sharedInstances(t)
	u := newLimitedUpstream(t, true)
	useSharedGate(a, db, u)
	gateB := useSharedGate(b, db, u)
	// b has read the shared holds just before, and would not read them
	// again for an hour on its own.
	gateB.Refresh = time.Hour
	if err := b.upstreamHeld(u.artifactURL()); err != nil {
		t.Fatal(err)
	}

	waits := testutil.ToFloat64(metrics.FetchLeaseWaits.WithLabelValues(metrics.CacheKindArtifact))
	resA := make(chan error, 1)
	go func() { resA <- fetchLimited(context.Background(), a, u) }()
	<-u.entered
	resB := make(chan error, 1)
	go func() { resB <- fetchLimited(context.Background(), b, u) }()
	waitForLeaseWaits(t, metrics.CacheKindArtifact, waits)
	u.open()

	if err := <-resA; err == nil {
		t.Fatal("instance a: want the rate limit error")
	}
	requireHeld(t, "instance b", <-resB)
	if got := u.calls.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want 1", got)
	}
}
