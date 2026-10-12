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

const (
	alpineCurlPath    = "/v3.22/main/x86_64/curl-8.14.1-r1.apk"
	alpineBusyboxPath = "/v3.22/main/x86_64/busybox-1.37.0-r12.apk"
)

// alpineRetentionUpstream serves package files the way an Alpine mirror
// does, counting downloads.
func alpineRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case alpineCurlPath, alpineBusyboxPath:
			downloads.Add(1)
			_, _ = io.WriteString(w, "apk bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

// newAlpineRetentionProxy mounts the upstream twice, as the default "alpine"
// repository and as a named "edge" repository.
func newAlpineRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream is a plain-HTTP loopback server; the default fetcher
	// refuses loopback, a stock client reaches it.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewAPKHandler(proxy, "http://proxy.test", map[string]string{"alpine": upstream.URL, "edge": upstream.URL})
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func alpineGet(t *testing.T, url string) {
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

// alpineArtifact returns the record of the file cached under filename,
// which for apk is the repository-qualified request path.
func alpineArtifact(t *testing.T, db *database.DB, filename string) database.Artifact {
	t.Helper()
	var art database.Artifact
	if err := db.Get(&art, db.Rebind(`SELECT * FROM artifacts WHERE filename = ?`), filename); err != nil {
		t.Fatalf("reading artifact %s: %v", filename, err)
	}
	return art
}

func alpineBackdate(t *testing.T, db *database.DB, filename string, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, filename); err != nil {
		t.Fatalf("backdating %s: %v", filename, err)
	}
}

func TestAlpineRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("alpine"); !ok {
		t.Fatal("alpine is not registered for retention")
	}
	for _, value := range []string{"alpine", "apk"} {
		if key, ok := retention.Default.KeyForDBEcosystem(value); !ok || key != "alpine" {
			t.Errorf("KeyForDBEcosystem(%s) = %q, %v, want alpine, true", value, key, ok)
		}
	}
}

// A package served from the cache stays while it is used, is evicted once
// its rule runs out, comes back from upstream on the next download, and
// stays after that even with the evicted copy's old access time on the
// record.
func TestAlpineRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := alpineRetentionUpstream(t)
	srv, db := newAlpineRetentionProxy(t, upstream)
	const file = "alpine" + alpineCurlPath

	alpineGet(t, srv.URL+"/alpine"+alpineCurlPath)
	alpineGet(t, srv.URL+"/alpine"+alpineCurlPath)
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1", got)
	}
	if !alpineArtifact(t, db, file).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"alpine": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !alpineArtifact(t, db, file).StoragePath.Valid {
		t.Fatal("package used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	alpineBackdate(t, db, file, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if alpineArtifact(t, db, file).StoragePath.Valid {
		t.Fatal("package unused for two days kept under a one-day rule")
	}

	alpineGet(t, srv.URL+"/alpine"+alpineCurlPath)
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after the eviction", got)
	}
	art := alpineArtifact(t, db, file)
	alpineBackdate(t, db, file, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !alpineArtifact(t, db, file).StoragePath.Valid {
		t.Error("refetched package evicted again by the next sweep")
	}
}

// A key in another namespace parses as an apk PURL but can never match a
// stored package, so it is refused at validation. Both documented forms are
// accepted.
func TestAlpineRetentionRefusesOtherNamespaces(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{"pkg:apk/curl": "0", "pkg:apk/alpine/busybox": "0"}
	if _, err := cfg.RetentionRules(retention.Default); err != nil {
		t.Fatalf("RetentionRules: %v", err)
	}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:apk/wolfi/curl": "0"}
	_, err := cfg.RetentionRules(retention.Default)
	if err == nil || !strings.Contains(err.Error(), "does not match how alpine packages are stored") {
		t.Errorf("RetentionRules(pkg:apk/wolfi/curl) error = %v, want the key refused", err)
	}
}

// A package rule reaches the package in every configured repository, in
// either documented form.
func TestAlpineRetentionPackageRules(t *testing.T) {
	upstream, _ := alpineRetentionUpstream(t)
	srv, db := newAlpineRetentionProxy(t, upstream)
	files := map[string]bool{
		"alpine" + alpineCurlPath:    true,
		"edge" + alpineCurlPath:      true,
		"alpine" + alpineBusyboxPath: false,
	}

	for _, key := range []string{"pkg:apk/curl", "pkg:apk/alpine/curl"} {
		old := time.Now().Add(-30 * retentionDay)
		for file := range files {
			alpineGet(t, srv.URL+"/"+file)
			alpineBackdate(t, db, file, old, old)
		}
		cfg := config.Default()
		cfg.Storage.Retention.Ecosystems = map[string]string{"alpine": "1d"}
		cfg.Storage.Retention.Packages = map[string]string{key: "0"}
		rules, err := cfg.RetentionRules(retention.Default)
		if err != nil {
			t.Fatalf("RetentionRules(%s): %v", key, err)
		}
		sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

		for file, want := range files {
			if got := alpineArtifact(t, db, file).StoragePath.Valid; got != want {
				t.Errorf("with %s: %s cached = %v, want %v", key, file, got, want)
			}
		}
	}
}
