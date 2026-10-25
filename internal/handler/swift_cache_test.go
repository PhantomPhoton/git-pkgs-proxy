package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSwiftReleaseListAndManifestServedFromCacheWhenUpstreamFails(t *testing.T) {
	var down atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Link", `</registry/apple/example?page=2>; rel="next"`)
		if strings.HasSuffix(r.URL.Path, "/Package.swift") {
			w.Header().Set("Content-Type", "text/x-swift")
			_, _ = io.WriteString(w, "// swift-tools-version:5.9")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"releases":{"1.0.0":{}}}`)
	}))
	defer upstream.Close()

	for _, cacheMetadata := range []bool{true, false} {
		proxy, _, _, _ := setupTestProxy(t)
		proxy.CacheMetadata = cacheMetadata
		routes := NewSwiftHandler(proxy, "https://proxy.example", upstream.URL+"/registry").Routes()
		get := func(path string) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			routes.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			return w
		}
		down.Store(false)
		for _, path := range []string{"/apple/example", "/apple/example/1.0.0/Package.swift"} {
			if w := get(path); w.Code != http.StatusOK {
				t.Fatalf("%s: status = %d with the upstream up", path, w.Code)
			}
		}

		down.Store(true)
		for path, body := range map[string]string{"/apple/example": `"1.0.0"`, "/apple/example/1.0.0/Package.swift": "swift-tools-version:5.9"} {
			w := get(path)
			if !cacheMetadata {
				if w.Code == http.StatusOK {
					t.Errorf("%s: served without cache_metadata while the upstream is down", path)
				}
				continue
			}
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), body) {
				t.Errorf("%s: status = %d, body %q; want the cached copy", path, w.Code, w.Body.String())
			}
			if link := w.Header().Get("Link"); link != `<https://proxy.example/swift/apple/example?page=2>; rel="next"` {
				t.Errorf("%s: Link = %q, want the cached Link rewritten", path, link)
			}
		}
	}
}

func TestSwiftManifestCacheKeyIgnoresToolsVersionZeros(t *testing.T) {
	key := swiftManifestCacheKey("apple", "example", "1.0.0", "swift-version=5.8")
	if got := swiftManifestCacheKey("apple", "example", "1.0.0", "swift-version=5.8.0"); got != key {
		t.Error("5.8.0 and 5.8 have different keys")
	}
	if got := swiftManifestCacheKey("apple", "example", "1.0.0", "swift-version=5.9"); got == key {
		t.Error("5.9 and 5.8 share a key")
	}
	if got := swiftManifestCacheKey("apple", "example", "1.0.0", ""); got == key {
		t.Error("Package.swift and the 5.8 manifest share a key")
	}
}
