package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// swiftRetentionUpstream serves release metadata and source archives for
// apple/swift-nio 2.65.0 and apple/swift-log 1.5.4 the way a Swift package
// registry does, counting archive downloads.
func swiftRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	archives := map[string]string{
		"/apple/swift-nio/2.65.0": "swift-nio archive",
		"/apple/swift-log/1.5.4":  "swift-log archive",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if archive, ok := archives[strings.TrimSuffix(r.URL.Path, ".zip")]; ok {
			if strings.HasSuffix(r.URL.Path, ".zip") {
				downloads.Add(1)
				w.Header().Set("Content-Type", "application/zip")
				_, _ = io.WriteString(w, archive)
				return
			}
			sum := sha256.Sum256([]byte(archive))
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Version", "1")
			_, _ = fmt.Fprintf(w, `{"resources":[{"name":"source-archive","type":"application/zip","checksum":%q}]}`, hex.EncodeToString(sum[:]))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func newSwiftRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream is a plain-HTTP loopback server; the default fetcher
	// refuses loopback, a stock client reaches it.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewSwiftHandler(proxy, "http://proxy.test", upstream.URL)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func swiftGet(t *testing.T, url string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/vnd.swift.registry.v1+zip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s, want 200", url, resp.StatusCode, body)
	}
}

// swiftArtifact returns the record of the archive named filename.
func swiftArtifact(t *testing.T, db *database.DB, filename string) database.Artifact {
	t.Helper()
	var art database.Artifact
	if err := db.Get(&art, db.Rebind(`SELECT * FROM artifacts WHERE filename = ?`), filename); err != nil {
		t.Fatalf("reading artifact %s: %v", filename, err)
	}
	return art
}

func swiftBackdate(t *testing.T, db *database.DB, filename string, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, filename); err != nil {
		t.Fatalf("backdating %s: %v", filename, err)
	}
}

func TestSwiftRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("swift"); !ok {
		t.Fatal("swift is not registered for retention")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("swift"); !ok || key != "swift" {
		t.Errorf("KeyForDBEcosystem(swift) = %q, %v, want swift, true", key, ok)
	}
}

// An archive served from the cache stays while it is used, is evicted once
// its rule runs out, comes back from upstream on the next download, and
// stays after that even with the evicted copy's old access time on the
// record.
func TestSwiftRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := swiftRetentionUpstream(t)
	srv, db := newSwiftRetentionProxy(t, upstream)
	const archive = "swift-nio-2.65.0.zip"

	swiftGet(t, srv.URL+"/apple/swift-nio/2.65.0.zip")
	swiftGet(t, srv.URL+"/apple/swift-nio/2.65.0.zip")
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1", got)
	}
	if !swiftArtifact(t, db, archive).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"swift": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !swiftArtifact(t, db, archive).StoragePath.Valid {
		t.Fatal("archive used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	swiftBackdate(t, db, archive, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if swiftArtifact(t, db, archive).StoragePath.Valid {
		t.Fatal("archive unused for two days kept under a one-day rule")
	}

	swiftGet(t, srv.URL+"/apple/swift-nio/2.65.0.zip")
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after the eviction", got)
	}
	art := swiftArtifact(t, db, archive)
	swiftBackdate(t, db, archive, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !swiftArtifact(t, db, archive).StoragePath.Valid {
		t.Error("refetched archive evicted again by the next sweep")
	}
}

// A source repository key parses as a swift PURL but names no registry
// identity, so it is refused at validation. A scope/name key is accepted.
func TestSwiftRetentionRefusesRepositoryKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{"pkg:swift/apple/swift-nio": "0"}
	if _, err := cfg.RetentionRules(retention.Default); err != nil {
		t.Fatalf("RetentionRules(pkg:swift/apple/swift-nio): %v", err)
	}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:swift/github.com/apple/swift-nio": "0"}
	_, err := cfg.RetentionRules(retention.Default)
	if err == nil || !strings.Contains(err.Error(), "does not match how swift packages are stored") {
		t.Errorf("RetentionRules(pkg:swift/github.com/apple/swift-nio) error = %v, want the key refused", err)
	}
}

// A package rule reaches the stored archive in either documented form and
// whatever case it is written in, even when the client asked in another case.
func TestSwiftRetentionPackageRules(t *testing.T) {
	upstream, _ := swiftRetentionUpstream(t)
	srv, db := newSwiftRetentionProxy(t, upstream)
	const nio, log = "swift-nio-2.65.0.zip", "swift-log-1.5.4.zip"

	swiftGet(t, srv.URL+"/Apple/Swift-NIO/2.65.0.zip")
	var stored string
	art := swiftArtifact(t, db, nio)
	if err := db.Get(&stored, db.Rebind(`SELECT package_purl FROM versions WHERE purl = ?`), art.VersionPURL); err != nil {
		t.Fatalf("reading stored package PURL: %v", err)
	}
	if stored != "pkg:generic/swift-registry/apple.swift-nio" {
		t.Errorf("stored package PURL = %q, want pkg:generic/swift-registry/apple.swift-nio", stored)
	}

	for _, key := range []string{"pkg:swift/Apple/Swift-NIO", "pkg:generic/swift-registry/apple.swift-nio"} {
		swiftGet(t, srv.URL+"/apple/swift-nio/2.65.0.zip")
		swiftGet(t, srv.URL+"/apple/swift-log/1.5.4.zip")
		old := time.Now().Add(-30 * retentionDay)
		swiftBackdate(t, db, nio, old, old)
		swiftBackdate(t, db, log, old, old)

		cfg := config.Default()
		cfg.Storage.Retention.Ecosystems = map[string]string{"swift": "1d"}
		cfg.Storage.Retention.Packages = map[string]string{key: "0"}
		rules, err := cfg.RetentionRules(retention.Default)
		if err != nil {
			t.Fatalf("RetentionRules(%s): %v", key, err)
		}
		sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

		if !swiftArtifact(t, db, nio).StoragePath.Valid {
			t.Errorf("with %s: swift-nio evicted despite its never-evict rule", key)
		}
		if swiftArtifact(t, db, log).StoragePath.Valid {
			t.Errorf("with %s: swift-log kept past the swift rule", key)
		}
	}
}
