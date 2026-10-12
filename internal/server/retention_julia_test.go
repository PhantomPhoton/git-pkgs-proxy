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
	juliaPackageUUID = "7876af07-990d-54b4-ab0e-23690620f79a"
	juliaPackageHash = "46e44e2b1ab5b2e0b7a2b0b63c87a4b0a3f6b2a1"
	juliaArtifactHsh = "0c61b4f2e0a4b0c3e8f1a2b3c4d5e6f708192a3b"
	juliaRegistryUID = "23338594-aafe-5451-b93e-139f81909106"
	juliaRegistryHsh = "9a1f0e2d3c4b5a69788796a5b4c3d2e1f0a9b8c7"
)

// juliaRetentionUpstream serves a package tarball, an artifact and a
// registry snapshot the way a Pkg server does, counting downloads. The
// snapshot isn't a real registry, so the handler can't load names and files
// the package under its uuid.
func juliaRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/package/" + juliaPackageUUID + "/" + juliaPackageHash, "/artifact/" + juliaArtifactHsh,
			"/registry/" + juliaRegistryUID + "/" + juliaRegistryHsh:
			downloads.Add(1)
			_, _ = io.WriteString(w, "tarball bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func newJuliaRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream is a plain-HTTP loopback server; the default fetcher
	// refuses loopback, a stock client reaches it.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewJuliaHandlerWithUpstream(proxy, upstream.URL)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func juliaGet(t *testing.T, url string) {
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

// juliaArtifact returns the record of the tarball named after hash.
func juliaArtifact(t *testing.T, db *database.DB, hash string) database.Artifact {
	t.Helper()
	var art database.Artifact
	if err := db.Get(&art, db.Rebind(`SELECT * FROM artifacts WHERE filename = ?`), hash+".tar.gz"); err != nil {
		t.Fatalf("reading artifact %s: %v", hash, err)
	}
	return art
}

func juliaBackdate(t *testing.T, db *database.DB, hash string, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, hash+".tar.gz"); err != nil {
		t.Fatalf("backdating %s: %v", hash, err)
	}
}

func TestJuliaRegistersForRetention(t *testing.T) {
	spec, ok := retention.Default.Lookup("julia")
	if !ok {
		t.Fatal("julia is not registered for retention")
	}
	if spec.CanonicalPackage != nil {
		t.Error("julia accepts package rules, but its PURLs can never match a stored package")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("julia"); !ok || key != "julia" {
		t.Errorf("KeyForDBEcosystem(julia) = %q, %v, want julia, true", key, ok)
	}
}

// An artifact served from the cache stays while it is used, is evicted once
// its rule runs out, comes back from upstream on the next download, and
// stays after that even with the evicted copy's old access time on the
// record.
func TestJuliaRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := juliaRetentionUpstream(t)
	srv, db := newJuliaRetentionProxy(t, upstream)
	artifactURL := srv.URL + "/artifact/" + juliaArtifactHsh

	juliaGet(t, artifactURL)
	juliaGet(t, artifactURL)
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1", got)
	}
	if !juliaArtifact(t, db, juliaArtifactHsh).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"julia": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !juliaArtifact(t, db, juliaArtifactHsh).StoragePath.Valid {
		t.Fatal("artifact used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	juliaBackdate(t, db, juliaArtifactHsh, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if juliaArtifact(t, db, juliaArtifactHsh).StoragePath.Valid {
		t.Fatal("artifact unused for two days kept under a one-day rule")
	}

	juliaGet(t, artifactURL)
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after the eviction", got)
	}
	art := juliaArtifact(t, db, juliaArtifactHsh)
	juliaBackdate(t, db, juliaArtifactHsh, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !juliaArtifact(t, db, juliaArtifactHsh).StoragePath.Valid {
		t.Error("refetched artifact evicted again by the next sweep")
	}
}

// Package rules for julia are refused, while the julia rule and the default
// both reach package tarballs, artifacts and registry snapshots.
func TestJuliaRetentionEcosystemRuleAndDefault(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{"pkg:julia/Example?uuid=" + juliaPackageUUID: "0"}
	if _, err := cfg.RetentionRules(retention.Default); err == nil || !strings.Contains(err.Error(), "not supported for julia") {
		t.Errorf("RetentionRules(julia package) error = %v, want package rules refused", err)
	}

	upstream, _ := juliaRetentionUpstream(t)
	srv, db := newJuliaRetentionProxy(t, upstream)
	for _, rules := range []retention.Rules{
		{Ecosystems: map[string]time.Duration{"julia": retentionDay}},
		{Default: retentionDay},
	} {
		juliaGet(t, srv.URL+"/package/"+juliaPackageUUID+"/"+juliaPackageHash)
		juliaGet(t, srv.URL+"/artifact/"+juliaArtifactHsh)
		juliaGet(t, srv.URL+"/registry/"+juliaRegistryUID+"/"+juliaRegistryHsh)
		old := time.Now().Add(-30 * retentionDay)
		juliaBackdate(t, db, juliaPackageHash, old, old)
		juliaBackdate(t, db, juliaArtifactHsh, old, old)
		juliaBackdate(t, db, juliaRegistryHsh, old, old)
		sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

		if juliaArtifact(t, db, juliaPackageHash).StoragePath.Valid {
			t.Errorf("with %+v: package tarball kept", rules)
		}
		if juliaArtifact(t, db, juliaArtifactHsh).StoragePath.Valid {
			t.Errorf("with %+v: artifact kept", rules)
		}
		if juliaArtifact(t, db, juliaRegistryHsh).StoragePath.Valid {
			t.Errorf("with %+v: registry snapshot kept", rules)
		}
	}
}
