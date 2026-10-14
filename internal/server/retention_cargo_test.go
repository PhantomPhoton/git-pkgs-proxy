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

// cargoRetentionUpstream serves crate files the way static.crates.io does,
// counting downloads.
func cargoRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/crates/serde/serde-1.0.0.crate", "/crates/Inflector/Inflector-0.11.4.crate":
			downloads.Add(1)
			_, _ = io.WriteString(w, "crate bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func newCargoRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream is a plain-HTTP loopback server; the default fetcher
	// refuses loopback, a stock client reaches it.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewCargoHandler(proxy, "http://proxy.test", upstream.URL, upstream.URL+"/crates")
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func cargoGet(t *testing.T, url string) {
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

// cargoArtifact returns the record of the crate file named filename.
func cargoArtifact(t *testing.T, db *database.DB, filename string) database.Artifact {
	t.Helper()
	var art database.Artifact
	if err := db.Get(&art, db.Rebind(`SELECT * FROM artifacts WHERE filename = ?`), filename); err != nil {
		t.Fatalf("reading artifact %s: %v", filename, err)
	}
	return art
}

func cargoBackdate(t *testing.T, db *database.DB, filename string, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, filename); err != nil {
		t.Fatalf("backdating %s: %v", filename, err)
	}
}

func TestCargoRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("cargo"); !ok {
		t.Fatal("cargo is not registered for retention")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("cargo"); !ok || key != "cargo" {
		t.Errorf("KeyForDBEcosystem(cargo) = %q, %v, want cargo, true", key, ok)
	}
}

// A crate served from the cache stays while it is used, is evicted once its
// rule runs out, comes back from upstream on the next download, and stays
// after that even with the evicted copy's old access time on the record.
func TestCargoRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := cargoRetentionUpstream(t)
	srv, db := newCargoRetentionProxy(t, upstream)
	const crate = "serde-1.0.0.crate"

	cargoGet(t, srv.URL+"/crates/serde/1.0.0/download")
	cargoGet(t, srv.URL+"/crates/serde/1.0.0/download")
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1", got)
	}
	if !cargoArtifact(t, db, crate).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"cargo": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !cargoArtifact(t, db, crate).StoragePath.Valid {
		t.Fatal("crate used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	cargoBackdate(t, db, crate, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if cargoArtifact(t, db, crate).StoragePath.Valid {
		t.Fatal("crate unused for two days kept under a one-day rule")
	}

	cargoGet(t, srv.URL+"/crates/serde/1.0.0/download")
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after the eviction", got)
	}
	art := cargoArtifact(t, db, crate)
	cargoBackdate(t, db, crate, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !cargoArtifact(t, db, crate).StoragePath.Valid {
		t.Error("refetched crate evicted again by the next sweep")
	}
}

// A key with a namespace parses as a cargo PURL but can never match a
// stored crate, so it is refused at validation. A plain key is accepted.
func TestCargoRetentionRefusesNamespacedKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{"pkg:cargo/serde": "0"}
	if _, err := cfg.RetentionRules(retention.Default); err != nil {
		t.Fatalf("RetentionRules(pkg:cargo/serde): %v", err)
	}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:cargo/rust-lang/serde": "0"}
	_, err := cfg.RetentionRules(retention.Default)
	if err == nil || !strings.Contains(err.Error(), "does not match how cargo packages are stored") {
		t.Errorf("RetentionRules(pkg:cargo/rust-lang/serde) error = %v, want the key refused", err)
	}
}

// A package rule reaches a crate under its published name, case included;
// a rule with the name in another case covers nothing.
func TestCargoRetentionPackageRules(t *testing.T) {
	upstream, _ := cargoRetentionUpstream(t)
	srv, db := newCargoRetentionProxy(t, upstream)
	const inflector, serde = "Inflector-0.11.4.crate", "serde-1.0.0.crate"

	tests := []struct {
		key           string
		keepInflector bool
	}{
		{"pkg:cargo/Inflector", true},
		{"pkg:cargo/inflector", false},
	}
	for _, tt := range tests {
		cargoGet(t, srv.URL+"/crates/Inflector/0.11.4/download")
		cargoGet(t, srv.URL+"/crates/serde/1.0.0/download")
		old := time.Now().Add(-30 * retentionDay)
		cargoBackdate(t, db, inflector, old, old)
		cargoBackdate(t, db, serde, old, old)

		cfg := config.Default()
		cfg.Storage.Retention.Ecosystems = map[string]string{"cargo": "1d"}
		cfg.Storage.Retention.Packages = map[string]string{tt.key: "0"}
		rules, err := cfg.RetentionRules(retention.Default)
		if err != nil {
			t.Fatalf("RetentionRules(%s): %v", tt.key, err)
		}
		sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

		if got := cargoArtifact(t, db, inflector).StoragePath.Valid; got != tt.keepInflector {
			t.Errorf("with %s: Inflector cached = %v, want %v", tt.key, got, tt.keepInflector)
		}
		if cargoArtifact(t, db, serde).StoragePath.Valid {
			t.Errorf("with %s: serde kept past the cargo rule", tt.key)
		}
	}
}

// cargoRewriteTransport sends every request to the test upstream, so the
// mirror command's fixed static.crates.io URLs reach it.
type cargoRewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (r cargoRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = r.target.Scheme
	req.URL.Host = r.target.Host
	req.Host = r.target.Host
	return r.base.RoundTrip(req)
}

// A crate preloaded with the mirror command falls under the cargo rule.
func TestCargoRetentionCoversMirroredCrates(t *testing.T) {
	upstream, downloads := cargoRetentionUpstream(t)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	db, store := setupEvictionTest(t)
	client := &http.Client{Transport: cargoRewriteTransport{target: target, base: upstream.Client().Transport}}
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(client), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())

	m := mirror.New(proxy, db, store, discardLogger(), 1)
	progress, err := m.Run(context.Background(), &mirror.PURLSource{PURLs: []string{"pkg:cargo/serde@1.0.0"}})
	if err != nil || progress.Failed != 0 || downloads.Load() != 1 {
		t.Fatalf("mirror run = %+v, %v, downloads %d; want one successful download", progress, err, downloads.Load())
	}

	const crate = "serde-1.0.0.crate"
	if cargoArtifact(t, db, crate).LastAccessedAt.Valid {
		t.Fatal("mirrored crate has an access recorded before anyone downloaded it")
	}
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, time.Now().Add(-30*retentionDay), crate); err != nil {
		t.Fatalf("backdating %s: %v", crate, err)
	}
	rules := retention.Rules{Ecosystems: map[string]time.Duration{"cargo": retentionDay}}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if cargoArtifact(t, db, crate).StoragePath.Valid {
		t.Error("never-served mirrored crate kept past the cargo rule")
	}
}
