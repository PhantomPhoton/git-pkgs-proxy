package handler

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// countingUpstream serves fixed documents by path with ETags, answers a
// matching If-None-Match with 304, and counts what it was asked. Setting
// down makes it answer 500, and closing gate holds requests until opened.
type countingUpstream struct {
	*httptest.Server
	mu          sync.Mutex
	docs        map[string][]byte
	gzipped     map[string]bool
	requests    map[string]int
	notModified map[string]int
	down        map[string]bool
	gate        chan struct{}
}

func newCountingUpstream(t *testing.T) *countingUpstream {
	t.Helper()
	u := &countingUpstream{
		docs: map[string][]byte{}, gzipped: map[string]bool{},
		requests: map[string]int{}, notModified: map[string]int{}, down: map[string]bool{},
	}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.requests[r.URL.Path]++
		gate := u.gate
		doc, ok := u.docs[r.URL.Path]
		down := u.down[r.URL.Path]
		gz := u.gzipped[r.URL.Path]
		u.mu.Unlock()
		if gate != nil {
			<-gate
		}
		switch {
		case down:
			w.WriteHeader(http.StatusInternalServerError)
			return
		case !ok:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		sum := sha256.Sum256(doc)
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			u.mu.Lock()
			u.notModified[r.URL.Path]++
			u.mu.Unlock()
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if gz && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			_, _ = zw.Write(doc)
			_ = zw.Close()
			return
		}
		_, _ = w.Write(doc)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *countingUpstream) set(path string, doc []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.docs[path] = doc
}

func (u *countingUpstream) setDown(path string, down bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.down[path] = down
}

func (u *countingUpstream) counts(path string) (requests, notModified int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests[path], u.notModified[path]
}

// cooldownCacheProxy returns a proxy with cooldown and the metadata cache on.
func cooldownCacheProxy(t *testing.T, u *countingUpstream) *Proxy {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = u.Client()
	config := &cooldown.Config{Default: "3d"}
	// Config parses itself on first use, which isn't safe to race.
	config.Enabled()
	proxy.Cooldown = config
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	return proxy
}

// expireCachedMetadata moves a metadata cache row's fetch time past the TTL.
func expireCachedMetadata(t *testing.T, proxy *Proxy, ecosystem, key string) {
	t.Helper()
	entry, err := proxy.DB.GetMetadataCache(ecosystem, key)
	if err != nil || entry == nil {
		t.Fatalf("metadata entry %s/%s: %v", ecosystem, key, err)
	}
	entry.FetchedAt.Time = time.Now().Add(-2 * proxy.MetadataTTL)
	if err := proxy.DB.UpsertMetadataCache(entry); err != nil {
		t.Fatalf("expiring metadata entry: %v", err)
	}
}

func getWith(t *testing.T, h http.Handler, path string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, vv := range header {
		req.Header[k] = vv
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

const (
	gemInfoPath     = "/info/testgem"
	gemVersionsPath = "/api/v1/versions/testgem.json"
)

func gemVersionsJSON(t *testing.T, versions map[string]time.Duration) []byte {
	t.Helper()
	var list []gemVersion
	for number, age := range versions {
		list = append(list, gemVersion{Number: number, Platform: "ruby", CreatedAt: time.Now().Add(-age).Format(time.RFC3339)})
	}
	b, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// gemCacheSetup serves testgem with 1.0.0 published a week ago and 2.0.0 an
// hour ago.
func gemCacheSetup(t *testing.T) (*countingUpstream, *Proxy, http.Handler) {
	t.Helper()
	u := newCountingUpstream(t)
	u.set(gemInfoPath, []byte("---\n1.0.0 |checksum:a\n2.0.0 |checksum:b\n"))
	u.set(gemVersionsPath, gemVersionsJSON(t, map[string]time.Duration{"1.0.0": 7 * 24 * time.Hour, "2.0.0": time.Hour}))
	proxy := cooldownCacheProxy(t, u)
	return u, proxy, NewGemHandlerWithUpstream(proxy, "http://proxy.test", u.URL).Routes()
}

const gemFiltered = "---\n1.0.0 |checksum:a\n"

func TestGemCooldownServesFromMetadataCache(t *testing.T) {
	u, _, h := gemCacheSetup(t)
	for i := range 3 {
		w := getWith(t, h, gemInfoPath, nil)
		if w.Code != http.StatusOK || w.Body.String() != gemFiltered {
			t.Fatalf("request %d: status %d, body %q", i, w.Code, w.Body)
		}
	}
	for _, path := range []string{gemInfoPath, gemVersionsPath} {
		if n, _ := u.counts(path); n != 1 {
			t.Errorf("%s requested %d times, want 1 within the TTL", path, n)
		}
	}
}

func TestGemCooldownRevalidatesAfterTTL(t *testing.T) {
	u, proxy, h := gemCacheSetup(t)
	getWith(t, h, gemInfoPath, nil)
	expireCachedMetadata(t, proxy, "gem", "info/testgem")
	expireCachedMetadata(t, proxy, "gem", "api/v1/versions/testgem.json")

	w := getWith(t, h, gemInfoPath, nil)
	if w.Code != http.StatusOK || w.Body.String() != gemFiltered {
		t.Fatalf("status %d, body %q", w.Code, w.Body)
	}
	for _, path := range []string{gemInfoPath, gemVersionsPath} {
		if n, nm := u.counts(path); n != 2 || nm != 1 {
			t.Errorf("%s: %d requests, %d answered 304; want 2 and 1", path, n, nm)
		}
	}
}

func TestGemCooldownConcurrentRequestsShareFetch(t *testing.T) {
	u, proxy, h := gemCacheSetup(t)
	u.gate = make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(u.gate) }) })

	const clients = 5
	var wg sync.WaitGroup
	codes := make([]int, clients)
	bodies := make([]string, clients)
	for i := range clients {
		wg.Go(func() {
			w := getWith(t, h, gemInfoPath, nil)
			codes[i], bodies[i] = w.Code, w.Body.String()
		})
	}
	waitForMetadataWaiters(t, proxy, metadataCoalesceKey("gem", "info/testgem", u.URL+gemInfoPath, "*/*", "identity", false), clients-1)
	waitForMetadataWaiters(t, proxy, metadataCoalesceKey("gem", "api/v1/versions/testgem.json", u.URL+gemVersionsPath, contentTypeJSON, "", true), clients-1)
	release.Do(func() { close(u.gate) })
	wg.Wait()

	for i := range clients {
		if codes[i] != http.StatusOK || bodies[i] != gemFiltered {
			t.Errorf("client %d: status %d, body %q", i, codes[i], bodies[i])
		}
	}
	for _, path := range []string{gemInfoPath, gemVersionsPath} {
		if n, _ := u.counts(path); n != 1 {
			t.Errorf("%s requested %d times, want 1", path, n)
		}
	}
}

func TestGemCooldownStaleCopyIsFiltered(t *testing.T) {
	u, proxy, h := gemCacheSetup(t)
	getWith(t, h, gemInfoPath, nil)
	expireCachedMetadata(t, proxy, "gem", "info/testgem")
	expireCachedMetadata(t, proxy, "gem", "api/v1/versions/testgem.json")
	u.setDown(gemInfoPath, true)
	u.setDown(gemVersionsPath, true)

	w := getWith(t, h, gemInfoPath, nil)
	if w.Code != http.StatusOK || w.Body.String() != gemFiltered {
		t.Fatalf("status %d, body %q; want the stale copy, filtered", w.Code, w.Body)
	}
	if got := w.Header().Get("Warning"); got != staleWarning {
		t.Errorf("Warning = %q, want %q", got, staleWarning)
	}
}

func TestGemCooldownFailsClosedWithoutTimes(t *testing.T) {
	u, _, h := gemCacheSetup(t)
	u.setDown(gemVersionsPath, true)

	w := getWith(t, h, gemInfoPath, nil)
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "1.0.0") {
		t.Fatalf("status %d, body %q; want 502 without the index", w.Code, w.Body)
	}
	// The index alone was cached; it must not be served unfiltered either.
	w = getWith(t, h, gemInfoPath, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("second request: status %d, want 502", w.Code)
	}
}

func TestGemCooldownClientRevalidates(t *testing.T) {
	_, _, h := gemCacheSetup(t)
	first := getWith(t, h, gemInfoPath, nil)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("filtered index served without an ETag")
	}
	w := getWith(t, h, gemInfoPath, http.Header{"If-None-Match": {etag}})
	if w.Code != http.StatusNotModified {
		t.Fatalf("status %d, want 304", w.Code)
	}
}

// The index can be fetched again while the versions API response is still
// within its TTL. A version published in between is then in the index but
// has no publish time, and must not be let through for that.
func TestGemCooldownRefreshesTimesOlderThanIndex(t *testing.T) {
	for _, timesDown := range []bool{false, true} {
		name := "times refreshed"
		if timesDown {
			name = "times upstream down"
		}
		t.Run(name, func(t *testing.T) {
			u, proxy, h := gemCacheSetup(t)
			getWith(t, h, gemInfoPath, nil)

			u.set(gemInfoPath, []byte("---\n1.0.0 |checksum:a\n2.0.0 |checksum:b\n3.0.0 |checksum:c\n"))
			u.set(gemVersionsPath, gemVersionsJSON(t, map[string]time.Duration{
				"1.0.0": 7 * 24 * time.Hour, "2.0.0": time.Hour, "3.0.0": time.Minute,
			}))
			u.setDown(gemVersionsPath, timesDown)
			expireCachedMetadata(t, proxy, "gem", "info/testgem")

			w := getWith(t, h, gemInfoPath, nil)
			if w.Code != http.StatusOK || w.Body.String() != gemFiltered {
				t.Fatalf("status %d, body %q; want %q", w.Code, w.Body, gemFiltered)
			}
			if n, _ := u.counts(gemVersionsPath); n != 2 {
				t.Errorf("versions API requested %d times, want 2", n)
			}
			if got := w.Header().Get("Warning"); (got != "") != timesDown {
				t.Errorf("Warning = %q with times upstream down = %v", got, timesDown)
			}
		})
	}
}

const (
	hexPackagePath = "/packages/testpkg"
	hexAPIPath     = "/api/packages/testpkg"
)

func hexAPIJSON(t *testing.T, releases map[string]time.Duration) []byte {
	t.Helper()
	var api hexPackageAPI
	for version, age := range releases {
		api.Releases = append(api.Releases, hexRelease{Version: version, InsertedAt: time.Now().Add(-age).Format(time.RFC3339Nano)})
	}
	b, err := json.Marshal(api)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hexCacheSetup(t *testing.T, recent time.Duration) (*countingUpstream, *Proxy, http.Handler, []byte) {
	t.Helper()
	u := newCountingUpstream(t)
	signed := buildHexSigned(buildHexPackage("testpkg", []string{testVersion100, "2.0.0"}))
	u.set(hexPackagePath, signed)
	u.set(hexAPIPath, hexAPIJSON(t, map[string]time.Duration{testVersion100: 7 * 24 * time.Hour, "2.0.0": recent}))
	proxy := cooldownCacheProxy(t, u)
	return u, proxy, NewHexHandlerWithUpstreams(proxy, "http://proxy.test", u.URL, u.URL).Routes(), signed
}

// hexVersions decodes the release versions of a served package.
func hexVersions(t *testing.T, body []byte) []string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	signed, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := extractProtobufBytes(signed, 1)
	if err != nil {
		t.Fatal(err)
	}
	versions, err := packageReleaseVersions(payload)
	if err != nil {
		t.Fatal(err)
	}
	return versions
}

func TestHexCooldownServesFromMetadataCache(t *testing.T) {
	u, proxy, h, _ := hexCacheSetup(t, time.Hour)
	for i := range 3 {
		w := getWith(t, h, hexPackagePath, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status %d", i, w.Code)
		}
		if got := hexVersions(t, w.Body.Bytes()); len(got) != 1 || got[0] != testVersion100 {
			t.Fatalf("request %d: versions %v, want [%s]", i, got, testVersion100)
		}
		if ce := w.Header().Get("Content-Encoding"); ce != "gzip" {
			t.Errorf("Content-Encoding = %q, want gzip", ce)
		}
	}
	for _, path := range []string{hexPackagePath, hexAPIPath} {
		if n, _ := u.counts(path); n != 1 {
			t.Errorf("%s requested %d times, want 1 within the TTL", path, n)
		}
	}

	expireCachedMetadata(t, proxy, "hex", "packages/testpkg")
	expireCachedMetadata(t, proxy, "hex", hashedCacheKey(u.URL, "api/packages/testpkg"))
	if w := getWith(t, h, hexPackagePath, nil); w.Code != http.StatusOK || len(hexVersions(t, w.Body.Bytes())) != 1 {
		t.Fatalf("after TTL: status %d", w.Code)
	}
	for _, path := range []string{hexPackagePath, hexAPIPath} {
		if n, nm := u.counts(path); n != 2 || nm != 1 {
			t.Errorf("%s: %d requests, %d answered 304; want 2 and 1", path, n, nm)
		}
	}
}

func TestHexCooldownStaleCopyIsFiltered(t *testing.T) {
	u, proxy, h, _ := hexCacheSetup(t, time.Hour)
	getWith(t, h, hexPackagePath, nil)
	expireCachedMetadata(t, proxy, "hex", "packages/testpkg")
	expireCachedMetadata(t, proxy, "hex", hashedCacheKey(u.URL, "api/packages/testpkg"))
	u.setDown(hexPackagePath, true)
	u.setDown(hexAPIPath, true)

	w := getWith(t, h, hexPackagePath, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want the stale copy", w.Code)
	}
	if got := hexVersions(t, w.Body.Bytes()); len(got) != 1 || got[0] != testVersion100 {
		t.Errorf("versions %v, want [%s]", got, testVersion100)
	}
	if got := w.Header().Get("Warning"); got != staleWarning {
		t.Errorf("Warning = %q, want %q", got, staleWarning)
	}
}

func TestHexCooldownFailsClosedWithoutTimes(t *testing.T) {
	u, _, h, _ := hexCacheSetup(t, time.Hour)
	u.setDown(hexAPIPath, true)
	for i := range 2 {
		if w := getWith(t, h, hexPackagePath, nil); w.Code != http.StatusBadGateway {
			t.Fatalf("request %d: status %d, want 502", i, w.Code)
		}
	}
}

func TestHexCooldownUnfilteredKeepsSignature(t *testing.T) {
	_, _, h, signed := hexCacheSetup(t, 7*24*time.Hour)
	w := getWith(t, h, hexPackagePath, nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), signed) {
		t.Fatalf("status %d; want upstream's signed bytes when nothing is in cooldown", w.Code)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("package served without an ETag")
	}
	if w := getWith(t, h, hexPackagePath, http.Header{"If-None-Match": {etag}}); w.Code != http.StatusNotModified {
		t.Fatalf("revalidation: status %d, want 304", w.Code)
	}
}

const condaRepodataPath = "/conda-forge/noarch/repodata.json"

func condaCacheSetup(t *testing.T) (*countingUpstream, *Proxy, *CondaHandler, http.Handler) {
	t.Helper()
	u := newCountingUpstream(t)
	repodata, err := json.Marshal(map[string]any{
		"info": map[string]any{"subdir": "noarch"},
		"packages": map[string]any{
			"old-1.0.0-0.tar.bz2": map[string]any{"name": "old", "version": "1.0.0", "timestamp": time.Now().Add(-7 * 24 * time.Hour).UnixMilli()},
			"new-2.0.0-0.tar.bz2": map[string]any{"name": "new", "version": "2.0.0", "timestamp": time.Now().Add(-time.Hour).UnixMilli()},
		},
		"packages.conda": map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	u.set(condaRepodataPath, repodata)
	u.gzipped[condaRepodataPath] = true
	proxy := cooldownCacheProxy(t, u)
	h := NewCondaHandlerWithUpstream(proxy, "http://proxy.test", u.URL)
	return u, proxy, h, h.Routes()
}

func condaPackages(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %q", w.Code, w.Body)
	}
	var doc struct {
		Packages map[string]json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decoding repodata: %v", err)
	}
	var names []string
	for name := range doc.Packages {
		names = append(names, name)
	}
	return names
}

func assertCondaFiltered(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if got := condaPackages(t, w); len(got) != 1 || got[0] != "old-1.0.0-0.tar.bz2" {
		t.Fatalf("packages %v, want only old-1.0.0-0.tar.bz2", got)
	}
}

func TestCondaCooldownServesFromMetadataCache(t *testing.T) {
	u, proxy, h, routes := condaCacheSetup(t)
	for range 3 {
		assertCondaFiltered(t, getWith(t, routes, condaRepodataPath, nil))
	}
	if n, _ := u.counts(condaRepodataPath); n != 1 {
		t.Errorf("repodata requested %d times, want 1 within the TTL", n)
	}

	expireCachedMetadata(t, proxy, "conda", h.metadataCacheKey(condaRepodataPath))
	assertCondaFiltered(t, getWith(t, routes, condaRepodataPath, nil))
	if n, nm := u.counts(condaRepodataPath); n != 2 || nm != 1 {
		t.Errorf("%d requests, %d answered 304; want 2 and 1", n, nm)
	}
}

func TestCondaCooldownStaleCopyIsFiltered(t *testing.T) {
	u, proxy, h, routes := condaCacheSetup(t)
	assertCondaFiltered(t, getWith(t, routes, condaRepodataPath, nil))
	expireCachedMetadata(t, proxy, "conda", h.metadataCacheKey(condaRepodataPath))
	u.setDown(condaRepodataPath, true)

	w := getWith(t, routes, condaRepodataPath, nil)
	assertCondaFiltered(t, w)
	if got := w.Header().Get("Warning"); got != staleWarning {
		t.Errorf("Warning = %q, want %q", got, staleWarning)
	}
}

func TestCondaCooldownConcurrentRequestsShareFetch(t *testing.T) {
	u, proxy, h, routes := condaCacheSetup(t)
	u.gate = make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(u.gate) }) })

	const clients = 5
	var wg sync.WaitGroup
	recorders := make([]*httptest.ResponseRecorder, clients)
	for i := range clients {
		wg.Go(func() { recorders[i] = getWith(t, routes, condaRepodataPath, nil) })
	}
	key := metadataCoalesceKey("conda", h.metadataCacheKey(condaRepodataPath), u.URL+condaRepodataPath, "*/*", "gzip", false)
	waitForMetadataWaiters(t, proxy, key, clients-1)
	release.Do(func() { close(u.gate) })
	wg.Wait()

	for _, w := range recorders {
		assertCondaFiltered(t, w)
	}
	if n, _ := u.counts(condaRepodataPath); n != 1 {
		t.Errorf("repodata requested %d times, want 1", n)
	}
}

func TestCondaCooldownClientRevalidates(t *testing.T) {
	_, _, h, routes := condaCacheSetup(t)
	// The first copy is streamed as it is filtered, before its ETag is known.
	assertCondaFiltered(t, getWith(t, routes, condaRepodataPath, nil))
	w := getWith(t, routes, condaRepodataPath, nil)
	assertCondaFiltered(t, w)
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("filtered repodata served without an ETag")
	}
	if w := getWith(t, routes, condaRepodataPath, http.Header{"If-None-Match": {etag}}); w.Code != http.StatusNotModified {
		t.Fatalf("revalidation: status %d, want 304", w.Code)
	}

	// Once the entry left out leaves cooldown, the copy changes and so does
	// its ETag.
	key := h.metadataCacheKey(condaRepodataPath)
	h.repodataMu.Lock()
	state := h.repodataStates[key]
	h.repodataMu.Unlock()
	if want := time.Now().Add(-time.Hour + 3*24*time.Hour); state.until.Sub(want).Abs() > time.Minute {
		t.Errorf("ETag kept until %v, want about %v", state.until, want)
	}
	req := httptest.NewRequest(http.MethodGet, condaRepodataPath, nil)
	req.Header.Set("If-None-Match", etag)
	if h.repodataNotModified(httptest.NewRecorder(), req, key, state.digest, state.until) {
		t.Error("ETag still matched after the withheld entry left cooldown")
	}
}

func TestCondaCooldownWithoutMetadataCacheStreams(t *testing.T) {
	u, proxy, _, routes := condaCacheSetup(t)
	proxy.CacheMetadata = false
	for range 2 {
		assertCondaFiltered(t, getWith(t, routes, condaRepodataPath, nil))
	}
	if n, _ := u.counts(condaRepodataPath); n != 2 {
		t.Errorf("repodata requested %d times, want 2 without cache_metadata", n)
	}
}

// A cached repodata.json is filtered as it is read from storage: the
// response starts while the stored document is still being read.
func TestCondaCooldownStreamsCachedRepodata(t *testing.T) {
	u, proxy, _, routes := condaCacheSetup(t)
	packages := map[string]any{}
	for i := range 20000 {
		packages[fmt.Sprintf("pkg%d-1.0.0-0.tar.bz2", i)] = map[string]any{
			"name": fmt.Sprintf("pkg%d", i), "version": "1.0.0", "timestamp": time.Now().Add(-7 * 24 * time.Hour).UnixMilli(),
		}
	}
	repodata, err := json.Marshal(map[string]any{"info": map[string]any{}, "packages": packages})
	if err != nil {
		t.Fatal(err)
	}
	u.set(condaRepodataPath, repodata)
	// Stored uncompressed, so the document is larger than the part the
	// storage hands out before stalling.
	u.gzipped[condaRepodataPath] = false
	if w := getWith(t, routes, condaRepodataPath, nil); w.Code != http.StatusOK {
		t.Fatalf("populating cache: status %d", w.Code)
	}

	gate := &stallingStorage{Storage: proxy.Storage, head: 256 << 10, release: make(chan struct{})}
	proxy.Storage = gate
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(gate.release) }) })
	srv := httptest.NewServer(routes)
	t.Cleanup(srv.Close)

	type result struct {
		resp *http.Response
		err  error
	}
	started := make(chan result, 1)
	go func() {
		resp, err := http.Get(srv.URL + condaRepodataPath)
		if err == nil {
			_, err = io.ReadFull(resp.Body, make([]byte, 1024))
		}
		started <- result{resp, err}
	}()
	var res result
	select {
	case res = <-started:
	case <-time.After(5 * time.Second):
		release.Do(func() { close(gate.release) })
		if res := <-started; res.resp != nil {
			_ = res.resp.Body.Close()
		}
		t.Fatal("response did not start before the cached document was read to its end")
	}
	release.Do(func() { close(gate.release) })
	if res.err != nil {
		t.Fatal(res.err)
	}
	_, _ = io.Copy(io.Discard, res.resp.Body)
	_ = res.resp.Body.Close()
	if n, _ := u.counts(condaRepodataPath); n != 1 {
		t.Errorf("repodata requested %d times, want 1", n)
	}
}
