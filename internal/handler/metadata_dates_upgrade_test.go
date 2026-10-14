package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/registries/fetch"
)

// upgradeDatabases opens an empty database with the current schema for each
// dialect the proxy supports. A nil database means the dialect is unavailable.
var upgradeDatabases = []struct {
	dialect string
	open    func(t *testing.T) *database.DB
}{
	{"sqlite", func(t *testing.T) *database.DB {
		db, err := database.Create(t.TempDir() + "/test.db")
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}},
	{"postgres", openTestPostgres},
}

// openTestPostgres creates a database of its own on the server at
// PROXY_DATABASE_URL, so the test shares no tables with tests elsewhere that
// use the same server, and drops it afterwards.
func openTestPostgres(t *testing.T) *database.DB {
	t.Helper()
	serverURL := os.Getenv("PROXY_DATABASE_URL")
	if serverURL == "" {
		return nil
	}
	server, err := database.OpenPostgres(serverURL)
	if err != nil {
		t.Fatalf("OpenPostgres failed: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	name := "proxy_upgrade_" + hex.EncodeToString(suffix)
	if _, err := server.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("creating database %s: %v", name, err)
	}
	t.Cleanup(func() { _, _ = server.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)") })

	dbURL, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	dbURL.Path = "/" + name
	db, err := database.OpenPostgres(dbURL.String())
	if err != nil {
		t.Fatalf("OpenPostgres(%s) failed: %v", name, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateSchema(); err != nil {
		t.Fatalf("CreateSchema failed: %v", err)
	}
	return db
}

// rollBackMetadataDates returns db to the schema from before migration 011,
// which only added the metadata_dates table.
func rollBackMetadataDates(t *testing.T, db *database.DB) {
	t.Helper()
	if _, err := db.Exec("DROP TABLE metadata_dates"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM migrations WHERE name = '011_add_metadata_dates'"); err != nil {
		t.Fatal(err)
	}
}

// upgradeProxy returns a proxy on db that dates metadata by a clock the test
// moves, a second after the process start it models.
func upgradeProxy(t *testing.T, db *database.DB, u *validatorUpstream) (*Proxy, *testClock) {
	t.Helper()
	proxy := NewProxy(db, newMockStorage(), &mockFetcher{}, fetch.NewResolver(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	proxy.HTTPClient = u.Client()
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	clock := &testClock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	restartProxy(proxy, clock)
	clock.advance(time.Second)
	return proxy, clock
}

// cachedMetadata is a metadata cache row and the bytes stored for it.
type cachedMetadata struct {
	entry database.MetadataCacheEntry
	body  string
}

func readCachedMetadata(t *testing.T, proxy *Proxy, ecosystem, name string) cachedMetadata {
	t.Helper()
	entry, err := proxy.DB.GetMetadataCache(ecosystem, name)
	if err != nil || entry == nil {
		t.Fatalf("metadata row %s/%s = %v, %v", ecosystem, name, entry, err)
	}
	r, err := proxy.Storage.Open(context.Background(), entry.StoragePath)
	if err != nil {
		t.Fatalf("opening stored metadata %s: %v", entry.StoragePath, err)
	}
	defer func() { _ = r.Close() }()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return cachedMetadata{entry: *entry, body: string(body)}
}

// TestRewrittenMetadataAfterMetadataDatesMigration upgrades a database from
// before migration 011 that holds cached npm and Composer metadata. The cached
// rows and stored bytes must come through unchanged, and two replicas sharing
// the upgraded database must serve the cached document with validators that
// revalidate, then date a changed document alike.
func TestRewrittenMetadataAfterMetadataDatesMigration(t *testing.T) {
	for _, d := range upgradeDatabases {
		for _, c := range validatorCases {
			t.Run(d.dialect+"/"+c.ecosystem, func(t *testing.T) {
				db := d.open(t)
				if db == nil {
					t.Skip("PROXY_DATABASE_URL not set, skipping postgres test")
				}
				u := newValidatorUpstream(t)
				a, clock := upgradeProxy(t, db, u)
				upgradeWithCachedMetadata(t, a, u, c)

				replicas := map[string]http.Handler{"A": c.handler(a, u), "B": c.handler(validatorReplica(a, clock), u)}
				cached := serve(replicas["A"], c.path, nil)
				if cached.Code != http.StatusOK || strings.Contains(cached.Body.String(), "2.0.0") {
					t.Fatalf("first request after the upgrade: status %d, body %s; want 200 with the document cached before it", cached.Code, cached.Body)
				}
				expectRevalidates(t, replicas, c.path, cached)

				expireMetadata(t, db, c.ecosystem, c.name)
				clock.advance(time.Second)
				changed := serve(replicas["A"], c.path, nil)
				expectChangedDocument(t, cached, changed)
				expectChangedForCachedValidators(t, replicas, c.path, cached, changed)
				expectRevalidates(t, replicas, c.path, changed)
			})
		}
	}
}

// upgradeWithCachedMetadata rolls proxy's database back to before migration
// 011, caches c's document as the proxy did then, moves upstream on to version
// 2.0.0 and migrates. The cached row and stored bytes must survive.
func upgradeWithCachedMetadata(t *testing.T, proxy *Proxy, u *validatorUpstream, c validatorCase) {
	t.Helper()
	rollBackMetadataDates(t, proxy.DB)
	if _, _, err := proxy.FetchOrCacheMetadata(context.Background(), c.ecosystem, c.name, u.URL+c.path); err != nil {
		t.Fatalf("seeding metadata before the upgrade: %v", err)
	}
	seeded := readCachedMetadata(t, proxy, c.ecosystem, c.name)
	u.set(npmV("2.0.0"), composerV("2.0.0"))

	if err := proxy.DB.MigrateSchema(); err != nil {
		t.Fatalf("MigrateSchema() error = %v", err)
	}
	if upgraded := readCachedMetadata(t, proxy, c.ecosystem, c.name); upgraded != seeded {
		t.Fatalf("cached metadata after the upgrade = %+v, want %+v", upgraded, seeded)
	}
}

// expectChangedDocument checks that changed is the new upstream document,
// with an ETag of its own and a later date than cached.
func expectChangedDocument(t *testing.T, cached, changed *httptest.ResponseRecorder) {
	t.Helper()
	if changed.Code != http.StatusOK || !strings.Contains(changed.Body.String(), "2.0.0") {
		t.Fatalf("after upstream changed: status %d, body %s; want 200 with the new document", changed.Code, changed.Body)
	}
	if changed.Header().Get("ETag") == cached.Header().Get("ETag") {
		t.Error("the changed document has the cached document's ETag")
	}
	was, _ := http.ParseTime(cached.Header().Get("Last-Modified"))
	if now, err := http.ParseTime(changed.Header().Get("Last-Modified")); err != nil || !now.After(was) {
		t.Errorf("changed document Last-Modified %q, want later than %q", changed.Header().Get("Last-Modified"), cached.Header().Get("Last-Modified"))
	}
}

// expectChangedForCachedValidators checks that no replica answers a client
// holding the cached document's validators with 304 once it changed.
func expectChangedForCachedValidators(t *testing.T, replicas map[string]http.Handler, path string, cached, changed *httptest.ResponseRecorder) {
	t.Helper()
	held := []http.Header{
		{"If-None-Match": {cached.Header().Get("ETag")}},
		{"If-Modified-Since": {cached.Header().Get("Last-Modified")}},
	}
	for name, handler := range replicas {
		for _, header := range held {
			if rec := serve(handler, path, header); rec.Code != http.StatusOK || rec.Body.String() != changed.Body.String() {
				t.Errorf("replica %s, %v of the cached document: status %d; want 200 with the changed document", name, header, rec.Code)
			}
		}
	}
}

// expectRevalidates checks that every replica serves want's validators and
// answers a conditional request with either of them with 304.
func expectRevalidates(t *testing.T, replicas map[string]http.Handler, path string, want *httptest.ResponseRecorder) {
	t.Helper()
	etag, lastModified := want.Header().Get("ETag"), want.Header().Get("Last-Modified")
	if etag == "" || lastModified == "" {
		t.Fatalf("ETag %q, Last-Modified %q; want both", etag, lastModified)
	}
	for name, handler := range replicas {
		rec := serve(handler, path, nil)
		if rec.Header().Get("ETag") != etag || rec.Header().Get("Last-Modified") != lastModified {
			t.Errorf("replica %s: ETag %q, Last-Modified %q; want %q, %q", name, rec.Header().Get("ETag"), rec.Header().Get("Last-Modified"), etag, lastModified)
		}
		for _, header := range []http.Header{{"If-None-Match": {etag}}, {"If-Modified-Since": {lastModified}}} {
			if rec := serve(handler, path, header); rec.Code != http.StatusNotModified {
				t.Errorf("replica %s, %v: status %d, want 304", name, header, rec.Code)
			}
		}
	}
}
