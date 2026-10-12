package server

import (
	"context"
	"fmt"
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

const (
	composerConsolePath = "/files/symfony/console/v6.4.0/symfony-console-v6.4.0.zip"
	composerMonologPath = "/files/monolog/monolog/3.5.0/monolog-monolog-3.5.0.zip"
)

// composerRetentionUpstream serves Packagist metadata for symfony/console and
// monolog/monolog and their dist archives, counting archive downloads.
func composerRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta := func(name, version string) {
			_, _ = fmt.Fprintf(w, `{"packages":{%q:[{"name":%q,"version":%q,"dist":{"type":"zip","url":%q}}]}}`,
				name, name, version, srv.URL+"/dist/"+strings.ReplaceAll(name, "/", "-")+"-"+version+".zip")
		}
		switch r.URL.Path {
		case "/p2/symfony/console.json":
			meta("symfony/console", "v6.4.0")
		case "/p2/monolog/monolog.json":
			meta("monolog/monolog", "3.5.0")
		case "/dist/symfony-console-v6.4.0.zip", "/dist/monolog-monolog-3.5.0.zip":
			downloads.Add(1)
			_, _ = io.WriteString(w, "zip bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func newComposerRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream is a plain-HTTP loopback server; the default fetcher
	// refuses loopback, a stock client reaches it.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewComposerHandlerWithUpstreams(proxy, "http://proxy.test", upstream.URL, upstream.URL)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func composerGet(t *testing.T, url string) {
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

// composerArtifact returns the record of the archive named filename.
func composerArtifact(t *testing.T, db *database.DB, filename string) database.Artifact {
	t.Helper()
	var art database.Artifact
	if err := db.Get(&art, db.Rebind(`SELECT * FROM artifacts WHERE filename = ?`), filename); err != nil {
		t.Fatalf("reading artifact %s: %v", filename, err)
	}
	return art
}

func composerBackdate(t *testing.T, db *database.DB, filename string, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, filename); err != nil {
		t.Fatalf("backdating %s: %v", filename, err)
	}
}

func TestComposerRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("composer"); !ok {
		t.Fatal("composer is not registered for retention")
	}
	for _, value := range []string{"composer", "packagist"} {
		if key, ok := retention.Default.KeyForDBEcosystem(value); !ok || key != "composer" {
			t.Errorf("KeyForDBEcosystem(%s) = %q, %v, want composer, true", value, key, ok)
		}
	}
}

// An archive served from the cache stays while it is used, is evicted once
// its rule runs out, comes back from upstream on the next download, and
// stays after that even with the evicted copy's old access time on the
// record.
func TestComposerRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := composerRetentionUpstream(t)
	srv, db := newComposerRetentionProxy(t, upstream)
	const archive = "symfony-console-v6.4.0.zip"

	composerGet(t, srv.URL+composerConsolePath)
	composerGet(t, srv.URL+composerConsolePath)
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1", got)
	}
	if !composerArtifact(t, db, archive).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"composer": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !composerArtifact(t, db, archive).StoragePath.Valid {
		t.Fatal("archive used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	composerBackdate(t, db, archive, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if composerArtifact(t, db, archive).StoragePath.Valid {
		t.Fatal("archive unused for two days kept under a one-day rule")
	}

	composerGet(t, srv.URL+composerConsolePath)
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after the eviction", got)
	}
	art := composerArtifact(t, db, archive)
	composerBackdate(t, db, archive, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !composerArtifact(t, db, archive).StoragePath.Valid {
		t.Error("refetched archive evicted again by the next sweep")
	}
}

// A key without a vendor parses as a composer PURL but can never match a
// stored package, so it is refused at validation. A key with a vendor is
// accepted.
func TestComposerRetentionRefusesKeysWithoutVendor(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{"pkg:composer/symfony/console": "0"}
	if _, err := cfg.RetentionRules(retention.Default); err != nil {
		t.Fatalf("RetentionRules(pkg:composer/symfony/console): %v", err)
	}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:composer/console": "0"}
	_, err := cfg.RetentionRules(retention.Default)
	if err == nil || !strings.Contains(err.Error(), "does not match how composer packages are stored") {
		t.Errorf("RetentionRules(pkg:composer/console) error = %v, want the key refused", err)
	}
}

// A package rule reaches the stored package whatever case it is written in,
// also when git-pkgs created the package record as "packagist".
func TestComposerRetentionPackageRules(t *testing.T) {
	upstream, _ := composerRetentionUpstream(t)
	srv, db := newComposerRetentionProxy(t, upstream)
	if err := db.UpsertPackage(&database.Package{PURL: "pkg:composer/monolog/monolog", Ecosystem: "packagist", Name: "monolog/monolog"}); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	const console, monolog = "symfony-console-v6.4.0.zip", "monolog-monolog-3.5.0.zip"

	tests := []struct {
		keys        map[string]string
		keepConsole bool
		keepMonolog bool
	}{
		{map[string]string{"pkg:composer/Symfony/Console": "0"}, true, false},
		{map[string]string{"pkg:composer/monolog/monolog": "0"}, false, true},
	}
	for _, tt := range tests {
		composerGet(t, srv.URL+composerConsolePath)
		composerGet(t, srv.URL+composerMonologPath)
		old := time.Now().Add(-30 * retentionDay)
		composerBackdate(t, db, console, old, old)
		composerBackdate(t, db, monolog, old, old)

		cfg := config.Default()
		cfg.Storage.Retention.Ecosystems = map[string]string{"composer": "1d"}
		cfg.Storage.Retention.Packages = tt.keys
		rules, err := cfg.RetentionRules(retention.Default)
		if err != nil {
			t.Fatalf("RetentionRules(%v): %v", tt.keys, err)
		}
		sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

		if got := composerArtifact(t, db, console).StoragePath.Valid; got != tt.keepConsole {
			t.Errorf("with %v: symfony/console cached = %v, want %v", tt.keys, got, tt.keepConsole)
		}
		if got := composerArtifact(t, db, monolog).StoragePath.Valid; got != tt.keepMonolog {
			t.Errorf("with %v: monolog/monolog (packagist record) cached = %v, want %v", tt.keys, got, tt.keepMonolog)
		}
	}

	var ecosystem string
	if err := db.Get(&ecosystem, db.Rebind(`SELECT ecosystem FROM packages WHERE purl = ?`), "pkg:composer/monolog/monolog"); err != nil || ecosystem != "packagist" {
		t.Errorf("monolog package ecosystem = %q, %v, want packagist", ecosystem, err)
	}
}
