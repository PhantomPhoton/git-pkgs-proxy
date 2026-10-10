package handler

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/mod/sumdb"
	"golang.org/x/mod/sumdb/note"
)

const testSumDBName = "sum.example"

// fakeSumDB is a checksum database upstream serving fixed files. Unknown
// paths get 404 with the body from notFound, or a default text.
type fakeSumDB struct {
	server   *httptest.Server
	files    map[string]string
	notFound map[string]string

	mu   sync.Mutex
	hits map[string]int
	down atomic.Bool
}

func newFakeSumDB(t *testing.T, files map[string]string) *fakeSumDB {
	t.Helper()
	db := &fakeSumDB{files: files, notFound: map[string]string{}, hits: map[string]int{}}
	db.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		db.mu.Lock()
		db.hits[r.URL.Path]++
		db.mu.Unlock()
		if db.down.Load() {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		body, ok := db.files[r.URL.Path]
		if !ok {
			text, custom := db.notFound[r.URL.Path]
			if !custom {
				text = "not found: " + r.URL.Path
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, text)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(db.server.Close)
	return db
}

func (db *fakeSumDB) requests(path string) int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.hits[path]
}

func (db *fakeSumDB) totalRequests() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	total := 0
	for _, n := range db.hits {
		total += n
	}
	return total
}

func newSumDBTestHandler(t *testing.T, sumDBURL string, cacheMetadata bool, ttl time.Duration) *GoHandler {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	proxy.CacheMetadata = cacheMetadata
	proxy.MetadataTTL = ttl
	h := NewGoHandler(proxy, "http://proxy.example")
	h.SetSumDBs(map[string]string{testSumDBName: sumDBURL})
	return h
}

func serveGoRequest(h *GoHandler, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

func TestGoSumDB_SupportedProbe(t *testing.T) {
	t.Run("default database", func(t *testing.T) {
		proxy, _, _, _ := setupTestProxy(t)
		h := NewGoHandler(proxy, "http://proxy.example")
		if w := serveGoRequest(h, "/sumdb/sum.golang.org/supported"); w.Code != http.StatusOK {
			t.Errorf("supported for default sum.golang.org: status = %d, want 200", w.Code)
		}
	})

	t.Run("configured databases replace the default", func(t *testing.T) {
		db := newFakeSumDB(t, nil)
		h := newSumDBTestHandler(t, db.server.URL, true, time.Hour)

		for target, want := range map[string]int{
			"/sumdb/sum.example/supported":    http.StatusOK,
			"/sumdb/sum.golang.org/supported": http.StatusNotFound,
			"/sumdb/other.example/supported":  http.StatusNotFound,
		} {
			if w := serveGoRequest(h, target); w.Code != want {
				t.Errorf("%s: status = %d, want %d", target, w.Code, want)
			}
		}
		if n := db.totalRequests(); n != 0 {
			t.Errorf("upstream requests = %d, want 0: supported is answered by the proxy", n)
		}
	})
}

func TestGoSumDB_RequestsGoToTheDatabase(t *testing.T) {
	var moduleUpstreamRequests atomic.Int32
	moduleUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		moduleUpstreamRequests.Add(1)
		http.NotFound(w, r)
	}))
	defer moduleUpstream.Close()

	const lookup = "/lookup/golang.org/x/text@v0.14.0"
	db := newFakeSumDB(t, map[string]string{lookup: "lookup record"})

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = http.DefaultClient
	h := NewGoHandlerWithUpstream(proxy, "http://proxy.example", moduleUpstream.URL)
	h.SetSumDBs(map[string]string{testSumDBName: db.server.URL + "/"})

	w := serveGoRequest(h, "/sumdb/sum.example"+lookup)
	if w.Code != http.StatusOK || w.Body.String() != "lookup record" {
		t.Fatalf("status = %d, body = %q; want 200 with the database's response", w.Code, w.Body.String())
	}
	if n := db.requests(lookup); n != 1 {
		t.Errorf("database requests for %s = %d, want 1", lookup, n)
	}
	if n := moduleUpstreamRequests.Load(); n != 0 {
		t.Errorf("module upstream requests = %d, want 0", n)
	}
}

func TestGoSumDB_LookupCachedWithinTTL(t *testing.T) {
	const lookup = "/lookup/golang.org/x/text@v0.14.0"
	db := newFakeSumDB(t, map[string]string{lookup: "lookup record"})
	h := newSumDBTestHandler(t, db.server.URL, true, time.Hour)

	for i := range 2 {
		w := serveGoRequest(h, "/sumdb/sum.example"+lookup)
		if w.Code != http.StatusOK || w.Body.String() != "lookup record" {
			t.Fatalf("request %d: status = %d, body = %q", i, w.Code, w.Body.String())
		}
	}
	if n := db.requests(lookup); n != 1 {
		t.Errorf("database requests = %d, want 1: the second lookup is a cache hit", n)
	}
}

func TestGoSumDB_CompleteTilesSkipRevalidation(t *testing.T) {
	files := map[string]string{
		"/tile/8/0/000":        "complete hash tile",
		"/tile/8/data/000":     "complete data tile",
		"/tile/8/0/001.p/5":    "partial hash tile",
		"/tile/8/data/001.p/5": "partial data tile",
		"/latest":              "signed tree",
	}
	wantRequests := map[string]int{
		"/tile/8/0/000":        1,
		"/tile/8/data/000":     1,
		"/tile/8/0/001.p/5":    2,
		"/tile/8/data/001.p/5": 2,
		"/latest":              2,
	}
	db := newFakeSumDB(t, files)
	// Every cached entry is past its TTL by the second request.
	h := newSumDBTestHandler(t, db.server.URL, true, time.Nanosecond)

	for range 2 {
		for path, want := range files {
			w := serveGoRequest(h, "/sumdb/sum.example"+path)
			if w.Code != http.StatusOK || w.Body.String() != want {
				t.Fatalf("%s: status = %d, body = %q, want %q", path, w.Code, w.Body.String(), want)
			}
		}
	}
	for path, want := range wantRequests {
		if n := db.requests(path); n != want {
			t.Errorf("%s: database requests = %d, want %d", path, n, want)
		}
	}
}

func TestGoSumDB_CompleteTilesNotStoredWithoutMetadataCache(t *testing.T) {
	const tile = "/tile/8/0/000"
	db := newFakeSumDB(t, map[string]string{tile: "complete hash tile"})
	h := newSumDBTestHandler(t, db.server.URL, false, time.Hour)

	for range 2 {
		if w := serveGoRequest(h, "/sumdb/sum.example"+tile); w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	}
	if n := db.requests(tile); n != 2 {
		t.Errorf("database requests = %d, want 2 with cache_metadata off", n)
	}
}

func TestGoSumDB_OfflineFallback(t *testing.T) {
	files := map[string]string{
		"/lookup/golang.org/x/text@v0.14.0": "lookup record",
		"/latest":                           "signed tree",
		"/tile/8/0/000":                     "complete hash tile",
		"/tile/8/0/001.p/5":                 "partial hash tile",
	}
	db := newFakeSumDB(t, files)
	h := newSumDBTestHandler(t, db.server.URL, true, time.Nanosecond)

	for path := range files {
		if w := serveGoRequest(h, "/sumdb/sum.example"+path); w.Code != http.StatusOK {
			t.Fatalf("%s online: status = %d, want 200", path, w.Code)
		}
	}

	db.down.Store(true)
	for path, want := range files {
		w := serveGoRequest(h, "/sumdb/sum.example"+path)
		if w.Code != http.StatusOK || w.Body.String() != want {
			t.Errorf("%s offline: status = %d, body = %q, want 200 with %q", path, w.Code, w.Body.String(), want)
		}
	}
}

func TestGoSumDB_NotFoundRelaysUpstreamText(t *testing.T) {
	const lookup = "/lookup/github.com/google/uuid@v9.9.9"
	const reason = "not found: github.com/google/uuid@v9.9.9: invalid version: unknown revision v9.9.9"

	for _, cacheMetadata := range []bool{true, false} {
		t.Run(fmt.Sprintf("cache_metadata=%v", cacheMetadata), func(t *testing.T) {
			db := newFakeSumDB(t, nil)
			db.notFound[lookup] = reason
			h := newSumDBTestHandler(t, db.server.URL, cacheMetadata, time.Hour)

			w := serveGoRequest(h, "/sumdb/sum.example"+lookup)
			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", w.Code)
			}
			if got := strings.TrimSpace(w.Body.String()); got != reason {
				t.Errorf("body = %q, want the database's text %q", got, reason)
			}
		})
	}

	t.Run("long text is cut", func(t *testing.T) {
		db := newFakeSumDB(t, nil)
		db.notFound[lookup] = strings.Repeat("x", upstreamNotFoundBodyLimit+1000)
		h := newSumDBTestHandler(t, db.server.URL, true, time.Hour)

		w := serveGoRequest(h, "/sumdb/sum.example"+lookup)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
		if got := w.Body.Len(); got != upstreamNotFoundBodyLimit {
			t.Errorf("body length = %d, want %d", got, upstreamNotFoundBodyLimit)
		}
	})
}

func TestGoSumDB_InvalidPathsNeverReachUpstream(t *testing.T) {
	db := newFakeSumDB(t, nil)
	h := newSumDBTestHandler(t, db.server.URL, true, time.Hour)

	for _, target := range []string{
		"/sumdb/sum.example",
		"/sumdb/sum.example/",
		"/sumdb/sum.example/other",
		"/sumdb/sum.example/latest/extra",
		"/sumdb/sum.example/lookup/",
		"/sumdb/sum.example/lookup/golang.org/x/text",
		"/sumdb/sum.example/lookup/golang.org/x/text@",
		"/sumdb/sum.example/lookup/golang.org/x/text@master",
		"/sumdb/sum.example/lookup/github.com/Google/uuid@v1.6.0",
		"/sumdb/sum.example/lookup/../../etc/passwd@v1.0.0",
		"/sumdb/sum.example/tile/x/0/000",
		"/sumdb/sum.example/tile/8/0/000.p/0",
		"/sumdb/sum.example/tile/8/0/000.p/256",
		"/sumdb/sum.example/tile/8/0/../../../latest",
		"/sumdb/unknown.example/latest",
	} {
		if w := serveGoRequest(h, target); w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", target, w.Code)
		}
	}
	if n := db.totalRequests(); n != 0 {
		t.Errorf("upstream requests = %d, want 0", n)
	}
}

// sumDBClientOps lets the x/mod checksum database client read through the
// proxy, keeping its configuration and cache in memory.
type sumDBClientOps struct {
	t      *testing.T
	remote string
	key    string

	mu     sync.Mutex
	config map[string][]byte
	cache  map[string][]byte
}

func newSumDBClientOps(t *testing.T, remote, key string) *sumDBClientOps {
	return &sumDBClientOps{t: t, remote: remote, key: key, config: map[string][]byte{}, cache: map[string][]byte{}}
}

func (o *sumDBClientOps) ReadRemote(path string) ([]byte, error) {
	resp, err := http.Get(o.remote + path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", path, resp.Status, body)
	}
	return body, nil
}

func (o *sumDBClientOps) ReadConfig(file string) ([]byte, error) {
	if file == "key" {
		return []byte(o.key), nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.config[file], nil
}

func (o *sumDBClientOps) WriteConfig(file string, old, data []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !bytes.Equal(o.config[file], old) {
		return sumdb.ErrWriteConflict
	}
	o.config[file] = data
	return nil
}

func (o *sumDBClientOps) ReadCache(file string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	data, ok := o.cache[file]
	if !ok {
		return nil, errors.New("not cached")
	}
	return data, nil
}

func (o *sumDBClientOps) WriteCache(file string, data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cache[file] = data
}

func (o *sumDBClientOps) Log(msg string) { o.t.Log(msg) }

func (o *sumDBClientOps) SecurityError(msg string) {
	o.t.Errorf("checksum database security error: %s", msg)
}

// TestGoSumDB_ClientRoundTrip runs the go command's checksum database client
// against a real database implementation through the proxy, so signatures,
// record proofs and tile hashes are checked end to end. A second client then
// verifies the same lookup from the proxy's cache with the database down.
func TestGoSumDB_ClientRoundTrip(t *testing.T) {
	signerKey, verifierKey, err := note.GenerateKey(rand.Reader, testSumDBName)
	if err != nil {
		t.Fatal(err)
	}
	gosum := func(path, version string) ([]byte, error) {
		return fmt.Appendf(nil, "%s %s h1:Y29udGVudA==\n%s %s/go.mod h1:Z28ubW9k\n", path, version, path, version), nil
	}
	database := sumdb.NewServer(sumdb.NewTestServer(signerKey, gosum))
	var down atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		database.ServeHTTP(w, r)
	}))
	defer upstream.Close()

	h := newSumDBTestHandler(t, upstream.URL, true, time.Hour)
	proxy := httptest.NewServer(h.Routes())
	defer proxy.Close()
	remote := proxy.URL + "/sumdb/" + testSumDBName

	client := sumdb.NewClient(newSumDBClientOps(t, remote, verifierKey))
	for _, m := range [][2]string{{"rsc.io/quote", "v1.5.2"}, {"golang.org/x/text", "v0.3.0"}, {"github.com/Google/UUID", "v1.6.0"}} {
		lines, err := client.Lookup(m[0], m[1])
		if err != nil {
			t.Fatalf("lookup %s@%s through the proxy: %v", m[0], m[1], err)
		}
		if want := m[0] + " " + m[1] + " h1:Y29udGVudA=="; len(lines) != 1 || lines[0] != want {
			t.Errorf("lookup %s@%s = %q, want [%q]", m[0], m[1], lines, want)
		}
	}

	down.Store(true)
	offline := sumdb.NewClient(newSumDBClientOps(t, remote, verifierKey))
	if _, err := offline.Lookup("rsc.io/quote", "v1.5.2"); err != nil {
		t.Fatalf("lookup from the proxy cache with the database down: %v", err)
	}
}
