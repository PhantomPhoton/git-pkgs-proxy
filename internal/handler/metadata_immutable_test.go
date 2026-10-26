package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// countingUpstream answers every path with body and counts requests per path.
type countingUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	hits   map[string]int
	body   func(path string) (string, bool)
}

func newCountingUpstream(t *testing.T, body func(path string) (string, bool)) *countingUpstream {
	t.Helper()
	u := &countingUpstream{hits: map[string]int{}, body: body}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits[r.URL.Path]++
		u.mu.Unlock()
		b, ok := u.body(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(b))
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *countingUpstream) count(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[path]
}

const goPseudoVersion = "v0.0.0-20240102150405-abcdefabcdef"

func goImmutableUpstream(t *testing.T) *countingUpstream {
	return newCountingUpstream(t, func(path string) (string, bool) {
		rest, ok := strings.CutPrefix(path, "/example.com/!mod/")
		if !ok {
			return "", false
		}
		switch {
		case rest == "@v/list":
			return "v1.0.0\n", true
		case rest == "@latest", rest == "@v/main.info":
			return `{"Version":"v1.0.0","Time":"2020-01-01T00:00:00Z"}`, true
		case rest == "@v/v1.0.0.info":
			return `{"Version":"v1.0.0","Time":"2020-01-01T00:00:00Z"}`, true
		case rest == "@v/"+goPseudoVersion+".info":
			return `{"Version":"` + goPseudoVersion + `","Time":"2024-01-02T15:04:05Z"}`, true
		case strings.HasSuffix(rest, ".mod"):
			return "module example.com/Mod\n", true
		}
		return "", false
	})
}

// TestGoVersionFilesAreNotRevalidated checks that a canonical version's .mod
// and .info are fetched once and then served from the cache, with or without
// cache_metadata and whatever the metadata TTL, while queries, @v/list and
// @latest still go upstream.
func TestGoVersionFilesAreNotRevalidated(t *testing.T) {
	for _, tt := range []struct {
		name                    string
		cacheMetadata, cooldown bool
	}{
		{"cache_metadata off", false, false},
		{"cache_metadata on", true, false},
		{"cache_metadata off, cooldown", false, true},
		{"cache_metadata on, cooldown", true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			testGoVersionFilesAreNotRevalidated(t, tt.cacheMetadata, tt.cooldown)
		})
	}
}

func testGoVersionFilesAreNotRevalidated(t *testing.T, cacheMetadata, withCooldown bool) {
	u := goImmutableUpstream(t)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = cacheMetadata
	proxy.MetadataTTL = time.Nanosecond // every TTL-bound entry is already expired
	if withCooldown {
		proxy.Cooldown = &cooldown.Config{Default: "3d"}
	}
	h := NewGoHandlerWithUpstream(proxy, "http://localhost:8080", u.server.URL).Routes()

	once := []string{
		"/example.com/!mod/@v/v1.0.0.mod",
		"/example.com/!mod/@v/v1.0.0.info",
		"/example.com/!mod/@v/" + goPseudoVersion + ".mod",
		"/example.com/!mod/@v/" + goPseudoVersion + ".info",
	}
	every := []string{
		"/example.com/!mod/@v/main.info",
		"/example.com/!mod/@v/list",
		"/example.com/!mod/@latest",
	}
	for range 3 {
		for _, p := range append(append([]string{}, once...), every...) {
			if w := goGet(h, p); w.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", p, w.Code, w.Body.String())
			}
		}
	}
	for _, p := range once {
		if w := goGet(h, p); w.Header().Get("Warning") != "" {
			t.Errorf("GET %s marked stale", p)
		}
		if got := u.count(p); got != 1 {
			t.Errorf("upstream requests for %s = %d, want 1", p, got)
		}
	}
	for _, p := range every {
		if got := u.count(p); got < 2 {
			t.Errorf("upstream requests for %s = %d, want one per request", p, got)
		}
	}
}

func TestGoVersionImmutable(t *testing.T) {
	tests := map[string]bool{
		"v1.0.0":                  true,
		"v1.0.0-!r!c1":            true, // v1.0.0-RC1
		"v2.0.0+incompatible":     true,
		goPseudoVersion:           true,
		"v1.2":                    false, // resolves to v1.2.0
		"v1.0.0+build":            false,
		"main":                    false,
		"latest":                  false,
		"v1.0.0-RC1":              false, // not a valid escaped version
		"":                        false,
		"master-20240102150405-x": false,
	}
	for v, want := range tests {
		if got := goVersionImmutable(v); got != want {
			t.Errorf("goVersionImmutable(%q) = %v, want %v", v, got, want)
		}
	}
}

// TestImmutableMetadataRefetchedWhenCorrupt checks that a stored immutable
// document whose bytes no longer match their digest is fetched again rather
// than returned for good. (A streamed hit of such a document aborts, as
// TestCachedMetadataHitAbortsOnDigestMismatch checks.)
func TestImmutableMetadataRefetchedWhenCorrupt(t *testing.T) {
	u := goImmutableUpstream(t)
	proxy, db, store, _ := setupTestProxy(t)
	const (
		path = "/example.com/!mod/@v/v1.0.0.info"
		key  = "example.com/Mod/@v/v1.0.0.info"
		want = `{"Version":"v1.0.0","Time":"2020-01-01T00:00:00Z"}`
	)
	fetch := func() string {
		t.Helper()
		res := proxy.fetchImmutableMetadata(context.Background(), "golang", key, u.server.URL+path, "*/*")
		if res.err != nil {
			t.Fatalf("fetch: %v", res.err)
		}
		return string(res.body)
	}

	fetch()
	entry, err := db.GetMetadataCache("golang", key)
	if err != nil || entry == nil {
		t.Fatalf("document not stored: %v", err)
	}
	if _, _, err := store.Store(context.Background(), entry.StoragePath, strings.NewReader(`{"Version":"v6.6.6"}`)); err != nil {
		t.Fatal(err)
	}

	if got := fetch(); got != want {
		t.Fatalf("got %q, want the upstream document", got)
	}
	if got := u.count(path); got != 2 {
		t.Errorf("upstream requests = %d, want 2", got)
	}
	if got := fetch(); got != want {
		t.Fatalf("got %q after refetch, want the upstream document", got)
	}
	if got := u.count(path); got != 2 {
		t.Errorf("upstream requests after refetch = %d, want 2", got)
	}
}

// TestRPMContentAddressedRepodataIsNotRevalidated checks that repodata files
// named after their checksum are fetched once, while repomd.xml and plainly
// named files follow the TTL.
func TestRPMContentAddressedRepodataIsNotRevalidated(t *testing.T) {
	const (
		sum     = "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae"
		primary = "/releases/40/Everything/x86_64/os/repodata/" + sum + "-primary.xml.zst"
		repomd  = "/releases/40/Everything/x86_64/os/repodata/repomd.xml"
		plain   = "/releases/40/Everything/x86_64/os/repodata/primary.xml.gz"
	)
	for _, cacheMetadata := range []bool{false, true} {
		u := newCountingUpstream(t, func(string) (string, bool) { return "data", true })
		proxy, _, _, _ := setupTestProxy(t)
		proxy.CacheMetadata = cacheMetadata
		proxy.MetadataTTL = time.Nanosecond
		h := NewRPMHandlerWithUpstream(proxy, "http://localhost:8080", u.server.URL).Routes()

		for range 3 {
			for _, p := range []string{primary, repomd, plain} {
				if w := goGet(h, p); w.Code != http.StatusOK || w.Body.String() != "data" {
					t.Fatalf("GET %s = %d %q", p, w.Code, w.Body.String())
				}
			}
		}
		if got := u.count(primary); got != 1 {
			t.Errorf("cache_metadata=%v: upstream requests for checksum-named file = %d, want 1", cacheMetadata, got)
		}
		for _, p := range []string{repomd, plain} {
			if got := u.count(p); got != 3 {
				t.Errorf("cache_metadata=%v: upstream requests for %s = %d, want 3", cacheMetadata, p, got)
			}
		}
	}
}

func TestRPMContentAddressed(t *testing.T) {
	sha256 := strings.Repeat("a", 64)
	tests := map[string]bool{
		"os/repodata/" + sha256 + "-primary.xml.gz":                  true,
		"os/repodata/" + strings.Repeat("0", 40) + "-other.xml.gz":   true,
		"os/repodata/" + strings.Repeat("f", 128) + "-comps.xml":     true,
		"os/repodata/repomd.xml":                                     false,
		"os/repodata/repomd.xml.asc":                                 false,
		"os/repodata/primary.xml.gz":                                 false,
		"os/repodata/" + strings.Repeat("a", 63) + "-primary.xml.gz": false,
		"os/repodata/" + strings.Repeat("A", 64) + "-primary.xml.gz": false,
		"os/repodata/" + sha256:                                      false,
	}
	for p, want := range tests {
		if got := rpmContentAddressed(p); got != want {
			t.Errorf("rpmContentAddressed(%q) = %v, want %v", p, got, want)
		}
	}
}

// TestImmutableMetadataOverSizeLimitIsStreamed checks that a checksum-named
// repodata file too large to buffer is streamed from upstream rather than
// refused.
func TestImmutableMetadataOverSizeLimitIsStreamed(t *testing.T) {
	const path = "/os/repodata/" + "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae" + "-filelists.xml.gz"
	u := newCountingUpstream(t, func(string) (string, bool) { return "large document", true })
	proxy, db, _, _ := setupTestProxy(t)
	proxy.MetadataMaxSize = 4
	h := NewRPMHandlerWithUpstream(proxy, "http://localhost:8080", u.server.URL).Routes()

	if w := goGet(h, path); w.Code != http.StatusOK || w.Body.String() != "large document" {
		t.Fatalf("got %d %q, want the upstream document", w.Code, w.Body.String())
	}
	if entry, _ := db.GetMetadataCache("rpm", hashedCacheKey(u.server.URL, strings.TrimPrefix(path, "/"))); entry != nil {
		t.Error("stored a document over the size limit")
	}
}
