package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/registries/fetch"
)

// gemRetentionUpstream serves gem files the way rubygems.org does,
// counting downloads.
func gemRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gems/rack-3.0.8.gem", "/gems/RedCloth-4.3.3.gem":
			downloads.Add(1)
			_, _ = io.WriteString(w, "gem bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func newGemRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream is a plain-HTTP loopback server; the default fetcher
	// refuses loopback, a stock client reaches it.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewGemHandlerWithUpstream(proxy, "http://proxy.test", upstream.URL)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func gemGet(t *testing.T, url string) {
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

// gemArtifact returns the record of the gem file named filename.
func gemArtifact(t *testing.T, db *database.DB, filename string) database.Artifact {
	t.Helper()
	var art database.Artifact
	if err := db.Get(&art, db.Rebind(`SELECT * FROM artifacts WHERE filename = ?`), filename); err != nil {
		t.Fatalf("reading artifact %s: %v", filename, err)
	}
	return art
}

func gemBackdate(t *testing.T, db *database.DB, filename string, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, filename); err != nil {
		t.Fatalf("backdating %s: %v", filename, err)
	}
}

func TestGemRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("gem"); !ok {
		t.Fatal("gem is not registered for retention")
	}
	for _, value := range []string{"gem", "rubygems"} {
		if key, ok := retention.Default.KeyForDBEcosystem(value); !ok || key != "gem" {
			t.Errorf("KeyForDBEcosystem(%s) = %q, %v, want gem, true", value, key, ok)
		}
	}
}

// A gem served from the cache stays while it is used, is evicted once its
// rule runs out, comes back from upstream on the next download, and stays
// after that even with the evicted copy's old access time on the record.
func TestGemRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := gemRetentionUpstream(t)
	srv, db := newGemRetentionProxy(t, upstream)
	const gem = "rack-3.0.8.gem"

	gemGet(t, srv.URL+"/gems/rack-3.0.8.gem")
	gemGet(t, srv.URL+"/gems/rack-3.0.8.gem")
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1", got)
	}
	if !gemArtifact(t, db, gem).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"gem": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !gemArtifact(t, db, gem).StoragePath.Valid {
		t.Fatal("gem used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	gemBackdate(t, db, gem, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if gemArtifact(t, db, gem).StoragePath.Valid {
		t.Fatal("gem unused for two days kept under a one-day rule")
	}

	gemGet(t, srv.URL+"/gems/rack-3.0.8.gem")
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after the eviction", got)
	}
	art := gemArtifact(t, db, gem)
	gemBackdate(t, db, gem, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !gemArtifact(t, db, gem).StoragePath.Valid {
		t.Error("refetched gem evicted again by the next sweep")
	}
}

// A key with a namespace parses as a gem PURL but can never match a
// stored gem, so it is refused at validation. A plain key is accepted.
func TestGemRetentionRefusesNamespacedKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{"pkg:gem/rack": "0"}
	if _, err := cfg.RetentionRules(retention.Default); err != nil {
		t.Fatalf("RetentionRules(pkg:gem/rack): %v", err)
	}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:gem/rails/rack": "0"}
	_, err := cfg.RetentionRules(retention.Default)
	if err == nil || !strings.Contains(err.Error(), "does not match how gem packages are stored") {
		t.Errorf("RetentionRules(pkg:gem/rails/rack) error = %v, want the key refused", err)
	}
}

// A package rule reaches a gem under its published name, case included;
// a rule with the name in another case covers nothing.
func TestGemRetentionPackageRules(t *testing.T) {
	upstream, _ := gemRetentionUpstream(t)
	srv, db := newGemRetentionProxy(t, upstream)
	const redcloth, rack = "RedCloth-4.3.3.gem", "rack-3.0.8.gem"

	tests := []struct {
		key          string
		keepRedCloth bool
	}{
		{"pkg:gem/RedCloth", true},
		{"pkg:gem/redcloth", false},
	}
	for _, tt := range tests {
		gemGet(t, srv.URL+"/gems/RedCloth-4.3.3.gem")
		gemGet(t, srv.URL+"/gems/rack-3.0.8.gem")
		old := time.Now().Add(-30 * retentionDay)
		gemBackdate(t, db, redcloth, old, old)
		gemBackdate(t, db, rack, old, old)

		cfg := config.Default()
		cfg.Storage.Retention.Ecosystems = map[string]string{"gem": "1d"}
		cfg.Storage.Retention.Packages = map[string]string{tt.key: "0"}
		rules, err := cfg.RetentionRules(retention.Default)
		if err != nil {
			t.Fatalf("RetentionRules(%s): %v", tt.key, err)
		}
		sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

		if got := gemArtifact(t, db, redcloth).StoragePath.Valid; got != tt.keepRedCloth {
			t.Errorf("with %s: RedCloth cached = %v, want %v", tt.key, got, tt.keepRedCloth)
		}
		if gemArtifact(t, db, rack).StoragePath.Valid {
			t.Errorf("with %s: rack kept past the gem rule", tt.key)
		}
	}
}

// git-pkgs writes gem packages with ecosystem "rubygems", and the record
// keeps the ecosystem of whoever wrote it first. A gem the proxy then caches
// under such a record still falls under the gem rule and its package rules.
func TestGemRetentionCoversRubygemsSpelling(t *testing.T) {
	upstream, _ := gemRetentionUpstream(t)
	srv, db := newGemRetentionProxy(t, upstream)
	if err := db.UpsertPackage(&database.Package{PURL: "pkg:gem/rack", Ecosystem: "rubygems", Name: "rack"}); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	if err := db.UpsertPackage(&database.Package{PURL: "pkg:gem/RedCloth", Ecosystem: "rubygems", Name: "RedCloth"}); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	const redcloth, rack = "RedCloth-4.3.3.gem", "rack-3.0.8.gem"
	gemGet(t, srv.URL+"/gems/rack-3.0.8.gem")
	gemGet(t, srv.URL+"/gems/RedCloth-4.3.3.gem")
	var ecosystem string
	if err := db.Get(&ecosystem, db.Rebind(`SELECT ecosystem FROM packages WHERE purl = ?`), "pkg:gem/rack"); err != nil || ecosystem != "rubygems" {
		t.Fatalf("rack package ecosystem = %q, %v, want rubygems", ecosystem, err)
	}

	old := time.Now().Add(-30 * retentionDay)
	gemBackdate(t, db, redcloth, old, old)
	gemBackdate(t, db, rack, old, old)
	cfg := config.Default()
	cfg.Storage.Retention.Ecosystems = map[string]string{"gem": "1d"}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:gem/RedCloth": "0"}
	rules, err := cfg.RetentionRules(retention.Default)
	if err != nil {
		t.Fatalf("RetentionRules: %v", err)
	}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

	if gemArtifact(t, db, rack).StoragePath.Valid {
		t.Error("rack under a rubygems record kept past the gem rule")
	}
	if !gemArtifact(t, db, redcloth).StoragePath.Valid {
		t.Error("RedCloth under a rubygems record evicted despite its never-evict rule")
	}
}
