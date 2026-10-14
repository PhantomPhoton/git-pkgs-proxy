package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/mirror"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/registries/fetch"
)

const (
	pubHTTPPath = "/packages/http/versions/1.2.0.tar.gz"
	pubPathPath = "/packages/path/versions/1.9.0.tar.gz"
)

// pubRetentionUpstream serves package archives the way pub.dev does,
// counting downloads.
func pubRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pubHTTPPath, pubPathPath:
			downloads.Add(1)
			_, _ = io.WriteString(w, "pub archive bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func newPubRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream is a plain-HTTP loopback server; the default fetcher
	// refuses loopback, a stock client reaches it.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewPubHandlerWithUpstream(proxy, "http://proxy.test", upstream.URL)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func pubGet(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s, want 200", url, resp.StatusCode, body)
	}
}

// pubArtifact returns the record of the archive named filename.
func pubArtifact(t *testing.T, db *database.DB, filename string) database.Artifact {
	t.Helper()
	var art database.Artifact
	if err := db.Get(&art, db.Rebind(`SELECT * FROM artifacts WHERE filename = ?`), filename); err != nil {
		t.Fatalf("reading artifact %s: %v", filename, err)
	}
	return art
}

func pubBackdate(t *testing.T, db *database.DB, filename string, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, filename); err != nil {
		t.Fatalf("backdating %s: %v", filename, err)
	}
}

func TestPubRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("pub"); !ok {
		t.Fatal("pub is not registered for retention")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("pub"); !ok || key != "pub" {
		t.Errorf("KeyForDBEcosystem(pub) = %q, %v, want pub, true", key, ok)
	}
}

// An archive served from the cache stays while it is used, is evicted once
// its rule runs out, comes back from upstream on the next download, and
// stays after that even with the evicted copy's old access time on the
// record.
func TestPubRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := pubRetentionUpstream(t)
	srv, db := newPubRetentionProxy(t, upstream)
	const archive = "http-1.2.0.tar.gz"

	pubGet(t, srv.URL+pubHTTPPath)
	pubGet(t, srv.URL+pubHTTPPath)
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1", got)
	}
	if !pubArtifact(t, db, archive).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"pub": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !pubArtifact(t, db, archive).StoragePath.Valid {
		t.Fatal("archive used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	pubBackdate(t, db, archive, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if pubArtifact(t, db, archive).StoragePath.Valid {
		t.Fatal("archive unused for two days kept under a one-day rule")
	}

	pubGet(t, srv.URL+pubHTTPPath)
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after the eviction", got)
	}
	art := pubArtifact(t, db, archive)
	pubBackdate(t, db, archive, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !pubArtifact(t, db, archive).StoragePath.Valid {
		t.Error("refetched archive evicted again by the next sweep")
	}
}

// A key with a namespace parses as a pub PURL but can never match a stored
// package, so it is refused at validation. A plain key is accepted.
func TestPubRetentionRefusesNamespacedKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{"pkg:pub/http": "0"}
	if _, err := cfg.RetentionRules(retention.Default); err != nil {
		t.Fatalf("RetentionRules(pkg:pub/http): %v", err)
	}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:pub/dart-lang/http": "0"}
	_, err := cfg.RetentionRules(retention.Default)
	if err == nil || !strings.Contains(err.Error(), "does not match how pub packages are stored") {
		t.Errorf("RetentionRules(pkg:pub/dart-lang/http) error = %v, want the key refused", err)
	}
}

// A package rule written as a pub PURL reaches the stored package.
func TestPubRetentionPackageRule(t *testing.T) {
	upstream, _ := pubRetentionUpstream(t)
	srv, db := newPubRetentionProxy(t, upstream)
	const httpArchive, pathArchive = "http-1.2.0.tar.gz", "path-1.9.0.tar.gz"
	pubGet(t, srv.URL+pubHTTPPath)
	pubGet(t, srv.URL+pubPathPath)
	old := time.Now().Add(-30 * retentionDay)
	pubBackdate(t, db, httpArchive, old, old)
	pubBackdate(t, db, pathArchive, old, old)

	cfg := config.Default()
	cfg.Storage.Retention.Ecosystems = map[string]string{"pub": "1d"}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:pub/http": "0"}
	rules, err := cfg.RetentionRules(retention.Default)
	if err != nil {
		t.Fatalf("RetentionRules: %v", err)
	}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

	if !pubArtifact(t, db, httpArchive).StoragePath.Valid {
		t.Error("http evicted despite its never-evict rule")
	}
	if pubArtifact(t, db, pathArchive).StoragePath.Valid {
		t.Error("path kept past the pub rule")
	}
}

// pubRewriteTransport sends every request to the test upstream, so the
// mirror command's fixed pub.dev URLs reach it.
type pubRewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (r pubRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = r.target.Scheme
	req.URL.Host = r.target.Host
	req.Host = r.target.Host
	return r.base.RoundTrip(req)
}

// A package preloaded with the mirror command falls under the pub rule.
func TestPubRetentionCoversMirroredPackages(t *testing.T) {
	upstream, downloads := pubRetentionUpstream(t)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	db, store := setupEvictionTest(t)
	client := &http.Client{Transport: pubRewriteTransport{target: target, base: upstream.Client().Transport}}
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(client), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())

	m := mirror.New(proxy, db, store, discardLogger(), 1)
	progress, err := m.Run(context.Background(), &mirror.PURLSource{PURLs: []string{"pkg:pub/http@1.2.0"}})
	if err != nil || progress.Failed != 0 || downloads.Load() != 1 {
		t.Fatalf("mirror run = %+v, %v, downloads %d; want one successful download", progress, err, downloads.Load())
	}

	const archive = "http-1.2.0.tar.gz"
	if pubArtifact(t, db, archive).LastAccessedAt.Valid {
		t.Fatal("mirrored archive has an access recorded before anyone downloaded it")
	}
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, time.Now().Add(-30*retentionDay), archive); err != nil {
		t.Fatalf("backdating %s: %v", archive, err)
	}
	rules := retention.Rules{Ecosystems: map[string]time.Duration{"pub": retentionDay}}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if pubArtifact(t, db, archive).StoragePath.Valid {
		t.Error("never-served mirrored archive kept past the pub rule")
	}
}
