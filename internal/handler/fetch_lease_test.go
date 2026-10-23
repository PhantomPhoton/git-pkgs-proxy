package handler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	return fetchLeftPadCtx(context.Background(), p)
}

func fetchLeftPadCtx(ctx context.Context, p *Proxy) (string, error) {
	res, err := p.GetOrFetchArtifactFromURL(ctx, "npm", "left-pad", "1.3.0", "left-pad-1.3.0.tgz", leaseTestURL)
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

func goFetchLeftPad(ctx context.Context, p *Proxy) chan fetchResult {
	ch := make(chan fetchResult, 1)
	go func() {
		body, err := fetchLeftPadCtx(ctx, p)
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

// waitForArtifactWaiters returns once n callers have joined p's fetch of
// left-pad.
func waitForArtifactWaiters(t *testing.T, p *Proxy, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.fetchMu.Lock()
		joined := 0
		for _, f := range p.inFlight {
			joined += f.waiters
		}
		p.fetchMu.Unlock()
		if joined >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for a caller to join the fetch")
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
	resA := goFetchLeftPad(context.Background(), a)
	<-fa.entered
	resB := goFetchLeftPad(context.Background(), b)
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
	resA := goFetchLeftPad(context.Background(), a)
	<-fa.entered
	resB := goFetchLeftPad(context.Background(), b)
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

// TestFetchLeaseWaitOutlivesDetachedCaller checks that a caller waiting on
// another instance's fetch keeps waiting after its client goes away when its
// fetch is detached, so callers that joined it still get the artifact.
func TestFetchLeaseWaitOutlivesDetachedCaller(t *testing.T) {
	a, b, _ := sharedInstances(t)
	const content = "left-pad tarball"
	fa := newGateFetcher(t, content, nil)
	fb := newGateFetcher(t, content, nil)
	fb.open()
	a.Fetcher, b.Fetcher = fa, fb

	waits := testutil.ToFloat64(metrics.FetchLeaseWaits.WithLabelValues(metrics.CacheKindArtifact))
	resA := goFetchLeftPad(context.Background(), a)
	<-fa.entered
	leadCtx, cancelLead := context.WithCancel(DetachUpstreamFetch(context.Background()))
	defer cancelLead()
	resLead := goFetchLeftPad(leadCtx, b)
	waitForLeaseWaits(t, metrics.CacheKindArtifact, waits)
	resJoined := goFetchLeftPad(context.Background(), b)
	waitForArtifactWaiters(t, b, 1)
	cancelLead()
	fa.open()
	<-resLead

	for name, ch := range map[string]chan fetchResult{"a": resA, "b's joined caller": resJoined} {
		r := <-ch
		if r.err != nil || r.body != content {
			t.Errorf("%s: body %q, err %v", name, r.body, r.err)
		}
	}
	if got := fb.calls.Load(); got != 0 {
		t.Errorf("b fetched %d times, want 0", got)
	}
}

// TestFetchLeaseFailedFetchWaitersFetchInParallel checks that once a fetch
// fails, the instances waiting on it fetch at the same time rather than one
// after another, so a hanging upstream costs each one timeout.
func TestFetchLeaseFailedFetchWaitersFetchInParallel(t *testing.T) {
	a, b, db := sharedInstances(t)
	c := NewProxy(db, b.Storage, &mockFetcher{}, fetch.NewResolver(), b.Logger)
	const content = "left-pad tarball"
	fa := newGateFetcher(t, "", errors.New("upstream down"))
	fb := newGateFetcher(t, content, nil)
	fc := newGateFetcher(t, content, nil)
	a.Fetcher, b.Fetcher, c.Fetcher = fa, fb, fc

	waits := testutil.ToFloat64(metrics.FetchLeaseWaits.WithLabelValues(metrics.CacheKindArtifact))
	resA := goFetchLeftPad(context.Background(), a)
	<-fa.entered
	resB := goFetchLeftPad(context.Background(), b)
	waitForLeaseWaits(t, metrics.CacheKindArtifact, waits)
	resC := goFetchLeftPad(context.Background(), c)
	waitForLeaseWaits(t, metrics.CacheKindArtifact, waits+1)
	fa.open()
	if r := <-resA; r.err == nil {
		t.Fatal("instance a: want the upstream error")
	}

	for name, entered := range map[string]chan struct{}{"b": fb.entered, "c": fc.entered} {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not fetch while the other waiting instance was fetching", name)
		}
	}
	fb.open()
	fc.open()
	for name, ch := range map[string]chan fetchResult{"b": resB, "c": resC} {
		if r := <-ch; r.err != nil || r.body != content {
			t.Errorf("instance %s: body %q, err %v", name, r.body, r.err)
		}
	}
}

// TestFetchLeaseFailedMetadataServesStale checks that an instance waiting on
// a metadata fetch that fails serves its stale copy at once, as the holder
// does, instead of asking the failing upstream again.
func TestFetchLeaseFailedMetadataServesStale(t *testing.T) {
	a, b, _ := sharedInstances(t)
	var status atomic.Int32
	status.Store(http.StatusOK)
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var released sync.Once
	open := func() { released.Do(func() { close(release) }) }
	t.Cleanup(open)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if code := int(status.Load()); code != http.StatusOK {
			entered <- struct{}{}
			<-release
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"left-pad"}`))
	}))
	t.Cleanup(upstream.Close)
	for _, p := range []*Proxy{a, b} {
		p.HTTPClient = upstream.Client()
		p.CacheMetadata = true
		p.MetadataTTL = 50 * time.Millisecond
	}
	fetchMeta := func(p *Proxy) chan metadataCall {
		ch := make(chan metadataCall, 1)
		go func() {
			body, _, err := p.FetchOrCacheMetadata(context.Background(), "npm", "left-pad", upstream.URL+"/left-pad", "application/json")
			ch <- metadataCall{body, err}
		}()
		return ch
	}
	if r := <-fetchMeta(a); r.err != nil {
		t.Fatalf("seeding the cache: %v", r.err)
	}
	time.Sleep(100 * time.Millisecond)

	status.Store(http.StatusServiceUnavailable)
	waits := testutil.ToFloat64(metrics.FetchLeaseWaits.WithLabelValues(metrics.CacheKindMetadata))
	resA := fetchMeta(a)
	<-entered
	resB := fetchMeta(b)
	waitForLeaseWaits(t, metrics.CacheKindMetadata, waits)
	open()

	for name, ch := range map[string]chan metadataCall{"a": resA, "b": resB} {
		if r := <-ch; r.err != nil || string(r.body) != `{"name":"left-pad"}` {
			t.Errorf("instance %s: body %q, err %v; want the stale copy", name, r.body, r.err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("upstream requests = %d, want 2: the seed and a's failed refresh", got)
	}
}

func TestFetchLeaseNotUsedOnSQLite(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	release, outcome, err := proxy.awaitFetchLease(context.Background(), metrics.CacheKindArtifact, "k", func() bool {
		t.Error("ready polled on SQLite")
		return false
	})
	if err != nil || outcome != leaseFetch {
		t.Fatalf("awaitFetchLease = %v, %v; want to fetch at once", outcome, err)
	}
	release(false)
}
