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
	hexPhoenixPath = "/tarballs/phoenix-1.7.10.tar"
	hexPlugPath    = "/tarballs/plug-1.15.3.tar"
)

// hexRetentionUpstream serves package tarballs the way repo.hex.pm does,
// counting downloads.
func hexRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case hexPhoenixPath, hexPlugPath:
			downloads.Add(1)
			_, _ = io.WriteString(w, "hex tarball bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func newHexRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream is a plain-HTTP loopback server; the default fetcher
	// refuses loopback, a stock client reaches it.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewHexHandlerWithUpstreams(proxy, "http://proxy.test", upstream.URL, upstream.URL)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func hexGet(t *testing.T, url string) {
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

// hexArtifact returns the record of the tarball named filename.
func hexArtifact(t *testing.T, db *database.DB, filename string) database.Artifact {
	t.Helper()
	var art database.Artifact
	if err := db.Get(&art, db.Rebind(`SELECT * FROM artifacts WHERE filename = ?`), filename); err != nil {
		t.Fatalf("reading artifact %s: %v", filename, err)
	}
	return art
}

func hexBackdate(t *testing.T, db *database.DB, filename string, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, filename); err != nil {
		t.Fatalf("backdating %s: %v", filename, err)
	}
}

func TestHexRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("hex"); !ok {
		t.Fatal("hex is not registered for retention")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("hex"); !ok || key != "hex" {
		t.Errorf("KeyForDBEcosystem(hex) = %q, %v, want hex, true", key, ok)
	}
}

// A tarball served from the cache stays while it is used, is evicted once
// its rule runs out, comes back from upstream on the next download, and
// stays after that even with the evicted copy's old access time on the
// record.
func TestHexRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := hexRetentionUpstream(t)
	srv, db := newHexRetentionProxy(t, upstream)
	const tarball = "phoenix-1.7.10.tar"

	hexGet(t, srv.URL+hexPhoenixPath)
	hexGet(t, srv.URL+hexPhoenixPath)
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1", got)
	}
	if !hexArtifact(t, db, tarball).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"hex": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !hexArtifact(t, db, tarball).StoragePath.Valid {
		t.Fatal("tarball used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	hexBackdate(t, db, tarball, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if hexArtifact(t, db, tarball).StoragePath.Valid {
		t.Fatal("tarball unused for two days kept under a one-day rule")
	}

	hexGet(t, srv.URL+hexPhoenixPath)
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after the eviction", got)
	}
	art := hexArtifact(t, db, tarball)
	hexBackdate(t, db, tarball, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !hexArtifact(t, db, tarball).StoragePath.Valid {
		t.Error("refetched tarball evicted again by the next sweep")
	}
}

// A key with a namespace parses as a hex PURL but can never match a stored
// package, so it is refused at validation. A plain key is accepted.
func TestHexRetentionRefusesNamespacedKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{"pkg:hex/phoenix": "0"}
	if _, err := cfg.RetentionRules(retention.Default); err != nil {
		t.Fatalf("RetentionRules(pkg:hex/phoenix): %v", err)
	}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:hex/acme/phoenix": "0"}
	_, err := cfg.RetentionRules(retention.Default)
	if err == nil || !strings.Contains(err.Error(), "does not match how hex packages are stored") {
		t.Errorf("RetentionRules(pkg:hex/acme/phoenix) error = %v, want the key refused", err)
	}
}

// A package rule written as a hex PURL reaches the stored package.
func TestHexRetentionPackageRule(t *testing.T) {
	upstream, _ := hexRetentionUpstream(t)
	srv, db := newHexRetentionProxy(t, upstream)
	const phoenixTarball, plugTarball = "phoenix-1.7.10.tar", "plug-1.15.3.tar"
	hexGet(t, srv.URL+hexPhoenixPath)
	hexGet(t, srv.URL+hexPlugPath)
	old := time.Now().Add(-30 * retentionDay)
	hexBackdate(t, db, phoenixTarball, old, old)
	hexBackdate(t, db, plugTarball, old, old)

	cfg := config.Default()
	cfg.Storage.Retention.Ecosystems = map[string]string{"hex": "1d"}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:hex/phoenix": "0"}
	rules, err := cfg.RetentionRules(retention.Default)
	if err != nil {
		t.Fatalf("RetentionRules: %v", err)
	}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

	if !hexArtifact(t, db, phoenixTarball).StoragePath.Valid {
		t.Error("phoenix evicted despite its never-evict rule")
	}
	if hexArtifact(t, db, plugTarball).StoragePath.Valid {
		t.Error("plug kept past the hex rule")
	}
}

// hexRewriteTransport sends every request to the test upstream, so the
// mirror command's fixed repo.hex.pm URLs reach it.
type hexRewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (r hexRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = r.target.Scheme
	req.URL.Host = r.target.Host
	req.Host = r.target.Host
	return r.base.RoundTrip(req)
}

// A package preloaded with the mirror command falls under the hex rule.
func TestHexRetentionCoversMirroredPackages(t *testing.T) {
	upstream, downloads := hexRetentionUpstream(t)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	db, store := setupEvictionTest(t)
	client := &http.Client{Transport: hexRewriteTransport{target: target, base: upstream.Client().Transport}}
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(client), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())

	m := mirror.New(proxy, db, store, discardLogger(), 1)
	progress, err := m.Run(context.Background(), &mirror.PURLSource{PURLs: []string{"pkg:hex/phoenix@1.7.10"}})
	if err != nil || progress.Failed != 0 || downloads.Load() != 1 {
		t.Fatalf("mirror run = %+v, %v, downloads %d; want one successful download", progress, err, downloads.Load())
	}

	const tarball = "phoenix-1.7.10.tar"
	if hexArtifact(t, db, tarball).LastAccessedAt.Valid {
		t.Fatal("mirrored tarball has an access recorded before anyone downloaded it")
	}
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, time.Now().Add(-30*retentionDay), tarball); err != nil {
		t.Fatalf("backdating %s: %v", tarball, err)
	}
	rules := retention.Rules{Ecosystems: map[string]time.Duration{"hex": retentionDay}}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if hexArtifact(t, db, tarball).StoragePath.Valid {
		t.Error("never-served mirrored tarball kept past the hex rule")
	}
}
