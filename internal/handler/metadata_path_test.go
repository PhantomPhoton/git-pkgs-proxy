package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
)

func TestMetadataStoragePathKeepsCaseApart(t *testing.T) {
	for _, pair := range [][2]string{
		{"github.com/Sirupsen/logrus", "github.com/sirupsen/logrus"},
		{"JSONStream", "jsonstream"},
	} {
		a, b := metadataStoragePath("go", pair[0]), metadataStoragePath("go", pair[1])
		if strings.EqualFold(a, b) {
			t.Errorf("%q and %q map to paths %q and %q, which a case-insensitive filesystem treats as one", pair[0], pair[1], a, b)
		}
	}
	for _, key := range []string{"CON", "nul", "trailing.", "a/../b"} {
		p := metadataStoragePath("npm", key)
		if strings.Contains(p, key) {
			t.Errorf("path %q contains the raw key %q", p, key)
		}
	}
	// Keys that already are digests keep their path, so existing entries
	// stay valid.
	sum := sha256.Sum256([]byte("generic"))
	digest := hex.EncodeToString(sum[:])
	if got, want := metadataStoragePath("generic", digest), "_metadata/generic/"+digest+"/metadata"; got != want {
		t.Errorf("digest key path = %q, want %q", got, want)
	}
}

// fileStorageProxy returns a proxy whose metadata is cached on the local
// filesystem, as with file:// storage.
func fileStorageProxy(t *testing.T, client *http.Client) (*Proxy, *database.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Create(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := storage.OpenBucket(context.Background(), "file://"+filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	proxy := NewProxy(db, store, &mockFetcher{}, fetch.NewResolver(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	proxy.HTTPClient = client
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	return proxy, db
}

// TestMetadataKeysDifferingInCaseCachedApart caches metadata for two keys
// that differ only in case on file storage. On a case-insensitive filesystem
// they shared one file, and the first key was served the second's document.
func TestMetadataKeysDifferingInCaseCachedApart(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, "document for "+r.URL.Path)
	}))
	defer upstream.Close()
	proxy, _ := fileStorageProxy(t, upstream.Client())

	keys := []string{"github.com/Sirupsen/logrus", "github.com/sirupsen/logrus"}
	for range 2 {
		for _, key := range keys {
			body, _, err := proxy.FetchOrCacheMetadata(context.Background(), "go", key, upstream.URL+"/"+key)
			if err != nil {
				t.Fatal(err)
			}
			if want := "document for /" + key; string(body) != want {
				t.Errorf("%s: served %q, want %q", key, body, want)
			}
		}
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("upstream requests = %d, want 2", got)
	}
}

// TestLegacyMetadataPathRefetched checks that a row still pointing at the
// unhashed path is fetched again, moved to the hashed path, and its old blob
// queued for deletion.
func TestLegacyMetadataPathRefetched(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, "fresh")
	}))
	defer upstream.Close()
	proxy, db := fileStorageProxy(t, upstream.Client())

	const key = "github.com/Sirupsen/logrus"
	legacy := legacyMetadataStoragePath("go", key)
	if _, _, err := proxy.Storage.Store(context.Background(), legacy, bytes.NewReader([]byte("old"))); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMetadataCache(&database.MetadataCacheEntry{
		Ecosystem:   "go",
		Name:        key,
		StoragePath: legacy,
		Size:        sql.NullInt64{Int64: 3, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	body, _, err := proxy.FetchOrCacheMetadata(context.Background(), "go", key, upstream.URL+"/x")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "fresh" || requests.Load() != 1 {
		t.Fatalf("served %q after %d upstream requests, want a fresh fetch", body, requests.Load())
	}
	entry, err := db.GetMetadataCache("go", key)
	if err != nil || entry == nil {
		t.Fatalf("cache row: %v %v", entry, err)
	}
	if entry.StoragePath != metadataStoragePath("go", key) {
		t.Errorf("storage path = %q, want %q", entry.StoragePath, metadataStoragePath("go", key))
	}
	due, err := db.GetDuePendingDeletes(time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(due, legacy) {
		t.Errorf("pending deletes = %v, want %q", due, legacy)
	}
}
