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
	cranRcppSource   = "/src/contrib/Rcpp_1.0.13.tar.gz"
	cranRcppArchive  = "/src/contrib/Archive/Rcpp/Rcpp_1.0.12.tar.gz"
	cranRcppBinary   = "/bin/windows/contrib/4.4/Rcpp_1.0.13.zip"
	cranGgplotSource = "/src/contrib/ggplot2_3.5.1.tar.gz"
)

// cranRetentionUpstream serves source, archived and binary packages the way
// a CRAN mirror does, counting downloads.
func cranRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case cranRcppSource, cranRcppArchive, cranRcppBinary, cranGgplotSource:
			downloads.Add(1)
			_, _ = io.WriteString(w, "cran package bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func newCRANRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream is a plain-HTTP loopback server; the default fetcher
	// refuses loopback, a stock client reaches it.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewCRANHandlerWithUpstream(proxy, "http://proxy.test", upstream.URL)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func cranGet(t *testing.T, url string) {
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

// cranArtifact returns the record of the file named after the last path
// segment of path.
func cranArtifact(t *testing.T, db *database.DB, path string) database.Artifact {
	t.Helper()
	filename := path[strings.LastIndex(path, "/")+1:]
	var art database.Artifact
	if err := db.Get(&art, db.Rebind(`SELECT * FROM artifacts WHERE filename = ?`), filename); err != nil {
		t.Fatalf("reading artifact %s: %v", filename, err)
	}
	return art
}

func cranBackdate(t *testing.T, db *database.DB, path string, fetched, accessed time.Time) {
	t.Helper()
	filename := path[strings.LastIndex(path, "/")+1:]
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, filename); err != nil {
		t.Fatalf("backdating %s: %v", filename, err)
	}
}

func TestCRANRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("cran"); !ok {
		t.Fatal("cran is not registered for retention")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("cran"); !ok || key != "cran" {
		t.Errorf("KeyForDBEcosystem(cran) = %q, %v, want cran, true", key, ok)
	}
}

// A package served from the cache stays while it is used, is evicted once
// its rule runs out, comes back from upstream on the next download, and
// stays after that even with the evicted copy's old access time on the
// record.
func TestCRANRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := cranRetentionUpstream(t)
	srv, db := newCRANRetentionProxy(t, upstream)

	cranGet(t, srv.URL+cranGgplotSource)
	cranGet(t, srv.URL+cranGgplotSource)
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1", got)
	}
	if !cranArtifact(t, db, cranGgplotSource).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"cran": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !cranArtifact(t, db, cranGgplotSource).StoragePath.Valid {
		t.Fatal("package used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	cranBackdate(t, db, cranGgplotSource, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if cranArtifact(t, db, cranGgplotSource).StoragePath.Valid {
		t.Fatal("package unused for two days kept under a one-day rule")
	}

	cranGet(t, srv.URL+cranGgplotSource)
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after the eviction", got)
	}
	art := cranArtifact(t, db, cranGgplotSource)
	cranBackdate(t, db, cranGgplotSource, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !cranArtifact(t, db, cranGgplotSource).StoragePath.Valid {
		t.Error("refetched package evicted again by the next sweep")
	}
}

// A key with a namespace parses as a cran PURL but can never match a stored
// package, so it is refused at validation. A plain key is accepted.
func TestCRANRetentionRefusesNamespacedKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{"pkg:cran/Rcpp": "0"}
	if _, err := cfg.RetentionRules(retention.Default); err != nil {
		t.Fatalf("RetentionRules(pkg:cran/Rcpp): %v", err)
	}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:cran/contrib/Rcpp": "0"}
	_, err := cfg.RetentionRules(retention.Default)
	if err == nil || !strings.Contains(err.Error(), "does not match how cran packages are stored") {
		t.Errorf("RetentionRules(pkg:cran/contrib/Rcpp) error = %v, want the key refused", err)
	}
}

// A package rule reaches the package's current source, archived source and
// binary builds, and only under its published case.
func TestCRANRetentionPackageRules(t *testing.T) {
	upstream, _ := cranRetentionUpstream(t)
	srv, db := newCRANRetentionProxy(t, upstream)
	rcpp := []string{cranRcppSource, cranRcppArchive, cranRcppBinary}

	for _, tt := range []struct {
		key      string
		keepRcpp bool
	}{
		{"pkg:cran/Rcpp", true},
		{"pkg:cran/rcpp", false},
	} {
		old := time.Now().Add(-30 * retentionDay)
		for _, path := range append([]string{cranGgplotSource}, rcpp...) {
			cranGet(t, srv.URL+path)
			cranBackdate(t, db, path, old, old)
		}
		cfg := config.Default()
		cfg.Storage.Retention.Ecosystems = map[string]string{"cran": "1d"}
		cfg.Storage.Retention.Packages = map[string]string{tt.key: "0"}
		rules, err := cfg.RetentionRules(retention.Default)
		if err != nil {
			t.Fatalf("RetentionRules(%s): %v", tt.key, err)
		}
		sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

		for _, path := range rcpp {
			if got := cranArtifact(t, db, path).StoragePath.Valid; got != tt.keepRcpp {
				t.Errorf("with %s: %s cached = %v, want %v", tt.key, path, got, tt.keepRcpp)
			}
		}
		if cranArtifact(t, db, cranGgplotSource).StoragePath.Valid {
			t.Errorf("with %s: ggplot2 kept past the cran rule", tt.key)
		}
	}
}
