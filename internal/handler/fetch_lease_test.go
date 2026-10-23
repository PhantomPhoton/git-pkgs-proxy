package handler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/registries/fetch"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// gateFetcher counts upstream fetches, signals entered when the first one
// starts and holds every fetch until release is closed.
type gateFetcher struct {
	calls   atomic.Int64
	content string
	err     error
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	opened  sync.Once
}

// newGateFetcher returns a gateFetcher that is released when the test ends
// at the latest, so a failing test does not leave fetches hanging.
func newGateFetcher(t *testing.T, content string, err error) *gateFetcher {
	f := &gateFetcher{content: content, err: err, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(f.open)
	return f
}

func (f *gateFetcher) open() { f.opened.Do(func() { close(f.release) }) }

func (f *gateFetcher) Fetch(ctx context.Context, url string) (*fetch.Artifact, error) {
	return f.FetchWithHeaders(ctx, url, nil)
}

func (f *gateFetcher) FetchWithHeaders(ctx context.Context, _ string, _ http.Header) (*fetch.Artifact, error) {
	f.calls.Add(1)
	f.once.Do(func() { close(f.entered) })
	select {
	case <-f.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	return &fetch.Artifact{Body: io.NopCloser(strings.NewReader(f.content)), ContentType: "application/gzip"}, nil
}

func (f *gateFetcher) Head(context.Context, string) (int64, string, error) { return 0, "", nil }

// sharedInstances returns two proxies sharing one Postgres database and one
// storage, as two replicas would. It skips without PROXY_DATABASE_URL.
func sharedInstances(t *testing.T) (*Proxy, *Proxy, *database.DB) {
	t.Helper()
	db := openTestPostgres(t)
	if db == nil {
		t.Skip("PROXY_DATABASE_URL not set")
	}
	store := newMockStorage()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := NewProxy(db, store, &mockFetcher{}, fetch.NewResolver(), logger)
	b := NewProxy(db, store, &mockFetcher{}, fetch.NewResolver(), logger)
	return a, b, db
}

const leaseTestURL = "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"

func fetchLeftPad(p *Proxy) (string, error) {
	res, err := p.GetOrFetchArtifactFromURL(context.Background(), "npm", "left-pad", "1.3.0", "left-pad-1.3.0.tgz", leaseTestURL)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Reader.Close() }()
	b, err := io.ReadAll(res.Reader)
	return string(b), err
}

type fetchResult struct {
	body string
	err  error
}

func goFetchLeftPad(p *Proxy) chan fetchResult {
	ch := make(chan fetchResult, 1)
	go func() {
		body, err := fetchLeftPad(p)
		ch <- fetchResult{body, err}
	}()
	return ch
}

// waitForLeaseWaits returns once the lease wait counter for kind has passed
// from.
func waitForLeaseWaits(t *testing.T, kind string, from float64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for testutil.ToFloat64(metrics.FetchLeaseWaits.WithLabelValues(kind)) <= from {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the second instance to wait on the lease")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFetchLeaseCoalescesArtifactAcrossInstances(t *testing.T) {
	a, b, _ := sharedInstances(t)
	const content = "left-pad tarball"
	fa := newGateFetcher(t, content, nil)
	fb := newGateFetcher(t, content, nil)
	fb.open()
	a.Fetcher, b.Fetcher = fa, fb

	waits := testutil.ToFloat64(metrics.FetchLeaseWaits.WithLabelValues(metrics.CacheKindArtifact))
	resA := goFetchLeftPad(a)
	<-fa.entered
	resB := goFetchLeftPad(b)
	waitForLeaseWaits(t, metrics.CacheKindArtifact, waits)
	fa.open()

	for name, ch := range map[string]chan fetchResult{"a": resA, "b": resB} {
		r := <-ch
		if r.err != nil || r.body != content {
			t.Errorf("instance %s: body %q, err %v", name, r.body, r.err)
		}
	}
	if got := fa.calls.Load() + fb.calls.Load(); got != 1 {
		t.Errorf("upstream fetches = %d (a %d, b %d), want 1", got, fa.calls.Load(), fb.calls.Load())
	}
}

// TestFetchLeaseFailedFetchLetsOtherInstanceFetch checks that an instance
// waiting on a fetch that fails takes the lease over and fetches itself,
// rather than waiting until the lease expires.
func TestFetchLeaseFailedFetchLetsOtherInstanceFetch(t *testing.T) {
	a, b, _ := sharedInstances(t)
	const content = "left-pad tarball"
	fa := newGateFetcher(t, "", errors.New("upstream down"))
	fb := newGateFetcher(t, content, nil)
	fb.open()
	a.Fetcher, b.Fetcher = fa, fb

	waits := testutil.ToFloat64(metrics.FetchLeaseWaits.WithLabelValues(metrics.CacheKindArtifact))
	resA := goFetchLeftPad(a)
	<-fa.entered
	resB := goFetchLeftPad(b)
	waitForLeaseWaits(t, metrics.CacheKindArtifact, waits)
	started := time.Now()
	fa.open()

	if r := <-resA; r.err == nil {
		t.Error("instance a: want the upstream error")
	}
	r := <-resB
	if r.err != nil || r.body != content {
		t.Fatalf("instance b: body %q, err %v", r.body, r.err)
	}
	if fb.calls.Load() != 1 {
		t.Errorf("b's upstream fetches = %d, want 1", fb.calls.Load())
	}
	if waited := time.Since(started); waited >= fetchLeaseTTL {
		t.Errorf("b waited %s, as long as the lease lasts", waited)
	}
}

// TestFetchLeaseExpiredIsTakenOver checks that a lease left by an instance
// that stopped while fetching holds the others up only until it expires.
func TestFetchLeaseExpiredIsTakenOver(t *testing.T) {
	_, b, db := sharedInstances(t)
	const content = "left-pad tarball"
	fb := newGateFetcher(t, content, nil)
	fb.open()
	b.Fetcher = fb

	key := artifactCoalesceKey("pkg:npm/left-pad@1.3.0", "left-pad-1.3.0.tgz", leaseTestURL, "", "")
	if ok, err := db.AcquireFetchLease(fetchLeaseKey(metrics.CacheKindArtifact, key), "stopped-instance", 300*time.Millisecond); err != nil || !ok {
		t.Fatalf("taking the lease for the stopped instance: %v, %v", ok, err)
	}
	started := time.Now()
	body, err := fetchLeftPad(b)
	if err != nil || body != content {
		t.Fatalf("body %q, err %v", body, err)
	}
	if waited := time.Since(started); waited < 250*time.Millisecond {
		t.Errorf("fetched after %s, before the lease expired; is the test key right?", waited)
	}
}

func TestFetchLeaseCoalescesMetadataAcrossInstances(t *testing.T) {
	a, b, _ := sharedInstances(t)
	u := newMetadataUpstream(t, http.StatusOK)
	var released sync.Once
	release := func() { released.Do(func() { close(u.release) }) }
	t.Cleanup(release)
	for _, p := range []*Proxy{a, b} {
		p.HTTPClient = u.Client()
		p.CacheMetadata = true
		p.MetadataTTL = time.Minute
	}

	waits := testutil.ToFloat64(metrics.FetchLeaseWaits.WithLabelValues(metrics.CacheKindMetadata))
	resA := startMetadataCalls(a, context.Background(), 1, u.URL+"/left-pad", "application/json")[0]
	<-u.entered
	resB := startMetadataCalls(b, context.Background(), 1, u.URL+"/left-pad", "application/json")[0]
	waitForLeaseWaits(t, metrics.CacheKindMetadata, waits)
	release()

	for name, ch := range map[string]chan metadataCall{"a": resA, "b": resB} {
		r := <-ch
		if r.err != nil || !strings.Contains(string(r.body), "application/json") {
			t.Errorf("instance %s: body %q, err %v", name, r.body, r.err)
		}
	}
	if got := u.calls.Load(); got != 1 {
		t.Errorf("upstream metadata requests = %d, want 1", got)
	}
}

// TestFetchLeaseNotUsedWithoutMetadataCache checks that metadata misses are
// not held up when there is no stored copy to wait for.
func TestFetchLeaseNotUsedWithoutMetadataCache(t *testing.T) {
	a, b, _ := sharedInstances(t)
	u := newMetadataUpstream(t, http.StatusOK)
	a.HTTPClient, b.HTTPClient = u.Client(), u.Client()

	resA := startMetadataCalls(a, context.Background(), 1, u.URL+"/left-pad", "application/json")[0]
	<-u.entered
	resB := startMetadataCalls(b, context.Background(), 1, u.URL+"/left-pad", "application/json")[0]
	deadline := time.Now().Add(5 * time.Second)
	for u.calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("b did not fetch while a was fetching")
		}
		time.Sleep(time.Millisecond)
	}
	close(u.release)
	for _, ch := range []chan metadataCall{resA, resB} {
		if r := <-ch; r.err != nil {
			t.Errorf("unexpected error: %v", r.err)
		}
	}
}

func TestFetchLeaseNotUsedOnSQLite(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	release, stored, err := proxy.awaitFetchLease(context.Background(), metrics.CacheKindArtifact, "k", func() bool {
		t.Error("ready polled on SQLite")
		return false
	})
	if err != nil || stored {
		t.Fatalf("awaitFetchLease = %v, %v; want to fetch at once", stored, err)
	}
	release()
}
