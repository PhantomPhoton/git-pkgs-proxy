package handler

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/git-pkgs/purl"
)

// A HEAD on a cache hit is answered from the artifact's database record. It
// must not open the stored object (a GET on S3 or GCS), and its headers must
// be the ones the GET sends.
func TestHeadCacheHitSkipsStorageOpen(t *testing.T) {
	const digest = "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
	for _, seekable := range []bool{false, true} {
		t.Run(fmt.Sprintf("seekable=%t", seekable), func(t *testing.T) {
			proxy, db, store, fetcher := setupTestProxy(t)
			store.seekable = seekable
			seedPackage(t, db, store, "oci", "library/nginx", digest, digest, "cached blob")
			h := &ContainerHandler{
				proxy:       proxy,
				registryURL: "https://registry.invalid",
				proxyURL:    "http://localhost:8080",
			}
			target := "/library/nginx/blobs/" + digest

			get := httptest.NewRecorder()
			h.Routes().ServeHTTP(get, httptest.NewRequest(http.MethodGet, target, nil))
			if get.Code != http.StatusOK || get.Body.String() != "cached blob" {
				t.Fatalf("GET: status %d, body %q", get.Code, get.Body.String())
			}
			opensAfterGet := store.opens

			req := httptest.NewRequest(http.MethodHead, target, nil)
			req = req.WithContext(ServingHead(req.Context()))
			head := httptest.NewRecorder()
			h.Routes().ServeHTTP(head, req)

			if head.Code != http.StatusOK {
				t.Fatalf("HEAD: status %d, body %q", head.Code, head.Body.String())
			}
			if got := store.opens - opensAfterGet; got != 0 {
				t.Errorf("HEAD opened the stored object %d times, want 0", got)
			}
			if head.Body.Len() != 0 {
				t.Errorf("HEAD body length = %d, want 0", head.Body.Len())
			}
			if !maps.EqualFunc(get.Header(), head.Header(), func(a, b []string) bool {
				return fmt.Sprint(a) == fmt.Sprint(b)
			}) {
				t.Errorf("HEAD headers differ from GET:\n GET  %v\n HEAD %v", get.Header(), head.Header())
			}
			if got := get.Header().Get("Accept-Ranges") == "bytes"; got != seekable {
				t.Errorf("GET Accept-Ranges set = %t, want %t", got, seekable)
			}
			if fetcher.fetchCalled {
				t.Error("fetcher called on a cache hit")
			}

			// A HEAD still counts as a hit, as it did when it opened the object.
			art, err := db.GetArtifact(purl.MakePURLString("oci", "library/nginx", digest), digest)
			if err != nil || art == nil {
				t.Fatalf("artifact record: %v", err)
			}
			if art.HitCount != 2 {
				t.Errorf("hit count = %d, want 2 (GET and HEAD)", art.HitCount)
			}
		})
	}
}

// A request that is not marked as HEAD reads the object as before.
func TestCacheHitWithoutHeadMarkOpensObject(t *testing.T) {
	proxy, db, store, _ := setupTestProxy(t)
	seedPackage(t, db, store, "npm", "demo", "1.0.0", "demo.tgz", "content")
	result, err := proxy.GetCachedArtifact(context.Background(), "npm", "demo", "1.0.0", "demo.tgz")
	if err != nil || result == nil || result.Reader == nil {
		t.Fatalf("result=%+v err=%v; want an open reader", result, err)
	}
	_ = result.Reader.Close()
	if store.opens != 1 {
		t.Errorf("opens = %d, want 1", store.opens)
	}
}
