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
	condaForgeNumpyPath = "/conda-forge/linux-64/numpy-1.24.0-py311h64a7726_0.conda"
	condaAnacondaNumpy  = "/anaconda/linux-64/numpy-1.24.0-py311h08b1b3b_0.conda"
	condaRequestsPath   = "/conda-forge/noarch/requests-2.31.0-pyhd8ed1ab_0.conda"
)

// condaRetentionUpstream serves package files the way conda.anaconda.org
// does, counting downloads.
func condaRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case condaForgeNumpyPath, condaAnacondaNumpy, condaRequestsPath:
			downloads.Add(1)
			_, _ = io.WriteString(w, "conda package bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func newCondaRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream is a plain-HTTP loopback server; the default fetcher
	// refuses loopback, a stock client reaches it.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewCondaHandlerWithUpstream(proxy, "http://proxy.test", upstream.URL)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func condaGet(t *testing.T, url string) {
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

// condaArtifact returns the record of the package file named filename.
func condaArtifact(t *testing.T, db *database.DB, filename string) database.Artifact {
	t.Helper()
	var art database.Artifact
	if err := db.Get(&art, db.Rebind(`SELECT * FROM artifacts WHERE filename = ?`), filename); err != nil {
		t.Fatalf("reading artifact %s: %v", filename, err)
	}
	return art
}

func condaBackdate(t *testing.T, db *database.DB, filename string, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, filename); err != nil {
		t.Fatalf("backdating %s: %v", filename, err)
	}
}

func TestCondaRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("conda"); !ok {
		t.Fatal("conda is not registered for retention")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("conda"); !ok || key != "conda" {
		t.Errorf("KeyForDBEcosystem(conda) = %q, %v, want conda, true", key, ok)
	}
}

// A package file served from the cache stays while it is used, is evicted
// once its rule runs out, comes back from upstream on the next download,
// and stays after that even with the evicted copy's old access time on the
// record.
func TestCondaRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := condaRetentionUpstream(t)
	srv, db := newCondaRetentionProxy(t, upstream)
	const file = "requests-2.31.0-pyhd8ed1ab_0.conda"

	condaGet(t, srv.URL+condaRequestsPath)
	condaGet(t, srv.URL+condaRequestsPath)
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1", got)
	}
	if !condaArtifact(t, db, file).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"conda": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !condaArtifact(t, db, file).StoragePath.Valid {
		t.Fatal("package used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	condaBackdate(t, db, file, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if condaArtifact(t, db, file).StoragePath.Valid {
		t.Fatal("package unused for two days kept under a one-day rule")
	}

	condaGet(t, srv.URL+condaRequestsPath)
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after the eviction", got)
	}
	art := condaArtifact(t, db, file)
	condaBackdate(t, db, file, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !condaArtifact(t, db, file).StoragePath.Valid {
		t.Error("refetched package evicted again by the next sweep")
	}
}

// A key without a channel parses as a conda PURL but can never match a
// stored package, so it is refused at validation. A key with the channel
// as the namespace is accepted.
func TestCondaRetentionRefusesKeysWithoutChannel(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{"pkg:conda/conda-forge/numpy": "0"}
	if _, err := cfg.RetentionRules(retention.Default); err != nil {
		t.Fatalf("RetentionRules(pkg:conda/conda-forge/numpy): %v", err)
	}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:conda/numpy": "0"}
	_, err := cfg.RetentionRules(retention.Default)
	if err == nil || !strings.Contains(err.Error(), "does not match how conda packages are stored") {
		t.Errorf("RetentionRules(pkg:conda/numpy) error = %v, want the key refused", err)
	}
}

// A package rule with the channel as the namespace reaches the package in
// that channel only; the same package from another channel and other
// packages fall under the conda rule.
func TestCondaRetentionPackageRule(t *testing.T) {
	upstream, _ := condaRetentionUpstream(t)
	srv, db := newCondaRetentionProxy(t, upstream)
	const forgeNumpy, anacondaNumpy, requests = "numpy-1.24.0-py311h64a7726_0.conda", "numpy-1.24.0-py311h08b1b3b_0.conda", "requests-2.31.0-pyhd8ed1ab_0.conda"
	condaGet(t, srv.URL+condaForgeNumpyPath)
	condaGet(t, srv.URL+condaAnacondaNumpy)
	condaGet(t, srv.URL+condaRequestsPath)

	var stored string
	art := condaArtifact(t, db, forgeNumpy)
	if err := db.Get(&stored, db.Rebind(`SELECT package_purl FROM versions WHERE purl = ?`), art.VersionPURL); err != nil {
		t.Fatalf("reading stored package PURL: %v", err)
	}
	if stored != "pkg:conda/conda-forge%2Fnumpy" {
		t.Errorf("stored package PURL = %q, want pkg:conda/conda-forge%%2Fnumpy", stored)
	}

	old := time.Now().Add(-30 * retentionDay)
	for _, filename := range []string{forgeNumpy, anacondaNumpy, requests} {
		condaBackdate(t, db, filename, old, old)
	}
	cfg := config.Default()
	cfg.Storage.Retention.Ecosystems = map[string]string{"conda": "1d"}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:conda/conda-forge/numpy": "0"}
	rules, err := cfg.RetentionRules(retention.Default)
	if err != nil {
		t.Fatalf("RetentionRules: %v", err)
	}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

	if !condaArtifact(t, db, forgeNumpy).StoragePath.Valid {
		t.Error("conda-forge numpy evicted despite its never-evict rule")
	}
	if condaArtifact(t, db, anacondaNumpy).StoragePath.Valid {
		t.Error("anaconda numpy kept by the rule for conda-forge")
	}
	if condaArtifact(t, db, requests).StoragePath.Valid {
		t.Error("requests kept past the conda rule")
	}
}
