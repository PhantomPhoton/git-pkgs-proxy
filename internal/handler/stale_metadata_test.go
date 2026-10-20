package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const stalePackument = `{"name":"left-pad","versions":{"1.0.0":{"name":"left-pad","version":"1.0.0",` +
	`"dist":{"tarball":"https://registry.npmjs.org/left-pad/-/left-pad-1.0.0.tgz"}}}}`

// npmUpstreamThen serves the packument once, with an ETag, and answers every
// later request with then.
func npmUpstreamThen(t *testing.T, then http.HandlerFunc) *httptest.Server {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"v1"`)
			_, _ = w.Write([]byte(stalePackument))
			return
		}
		then(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNPMMetadataAfterUpstreamAnswer(t *testing.T) {
	tests := []struct {
		name       string
		then       http.HandlerFunc
		wantStatus int
		wantStale  bool
	}{
		{name: "upstream error", then: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}, wantStatus: http.StatusOK, wantStale: true},
		{name: "timeout", then: func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		}, wantStatus: http.StatusOK, wantStale: true},
		{name: "not modified", then: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotModified)
		}, wantStatus: http.StatusOK},
		{name: "unpublished", then: func(w http.ResponseWriter, _ *http.Request) {
			http.NotFound(w, nil)
		}, wantStatus: http.StatusNotFound},
		{name: "credentials rejected", then: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}, wantStatus: http.StatusBadGateway},
		{name: "access forbidden", then: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}, wantStatus: http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := npmUpstreamThen(t, tt.then)
			proxy, _, _, _ := setupTestProxy(t)
			proxy.CacheMetadata = true
			proxy.MetadataTTL = time.Millisecond
			proxy.HTTPClient = &http.Client{Timeout: 200 * time.Millisecond}
			h := NewNPMHandler(proxy, "http://proxy.test", upstream.URL+"/").Routes()

			first := httptest.NewRecorder()
			h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/left-pad", nil))
			if first.Code != http.StatusOK {
				t.Fatalf("first request: status = %d", first.Code)
			}
			time.Sleep(5 * time.Millisecond)

			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/left-pad", nil))
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body %s", w.Code, tt.wantStatus, w.Body)
			}
			if stale := w.Header().Get("Warning") == staleWarning; stale != tt.wantStale {
				t.Errorf("Warning = %q, want stale %v", w.Header().Get("Warning"), tt.wantStale)
			}
			servedCopy := strings.Contains(w.Body.String(), "left-pad-1.0.0.tgz")
			if wantCopy := tt.wantStatus == http.StatusOK; servedCopy != wantCopy {
				t.Errorf("served the cached packument = %v, want %v", servedCopy, wantCopy)
			}
		})
	}
}

func TestProxyCachedPassesDefiniteAnswersThrough(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := npmUpstreamThen(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
			proxy, _, _, _ := setupTestProxy(t)
			proxy.CacheMetadata = true
			proxy.MetadataTTL = time.Millisecond
			proxy.HTTPClient = upstream.Client()

			w := httptest.NewRecorder()
			proxy.ProxyCached(w, httptest.NewRequest(http.MethodGet, "/doc", nil), upstream.URL+"/doc", "test-eco", "doc")
			time.Sleep(5 * time.Millisecond)
			w = httptest.NewRecorder()
			proxy.ProxyCached(w, httptest.NewRequest(http.MethodGet, "/doc", nil), upstream.URL+"/doc", "test-eco", "doc")

			if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "left-pad") {
				t.Errorf("status = %d, body %q, want the upstream's answer instead of the cached copy", w.Code, w.Body)
			}
		})
	}
}

const staleComposerDocument = `{"packages":{"acme/widget":[{"name":"acme/widget","version":"1.0.0",` +
	`"dist":{"type":"zip","url":"https://repo.packagist.org/files/acme/widget/1.0.0/widget.zip"}}]},"minified":"composer/2.0"}`

func TestComposerMetadataAfterUpstreamAnswer(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantStatus int
		wantStale  bool
	}{
		{name: "upstream error", status: http.StatusBadGateway, wantStatus: http.StatusOK, wantStale: true},
		{name: "removed", status: http.StatusNotFound, wantStatus: http.StatusNotFound},
		{name: "credentials rejected", status: http.StatusUnauthorized, wantStatus: http.StatusBadGateway},
		{name: "access forbidden", status: http.StatusForbidden, wantStatus: http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if requests.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(staleComposerDocument))
					return
				}
				w.WriteHeader(tt.status)
			}))
			t.Cleanup(upstream.Close)
			proxy, _, _, _ := setupTestProxy(t)
			proxy.CacheMetadata = true
			proxy.MetadataTTL = time.Millisecond
			proxy.HTTPClient = upstream.Client()
			h := NewComposerHandlerWithUpstreams(proxy, "http://proxy.test", upstream.URL, upstream.URL).Routes()

			first := httptest.NewRecorder()
			h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/p2/acme/widget.json", nil))
			if first.Code != http.StatusOK {
				t.Fatalf("first request: status = %d, body %s", first.Code, first.Body)
			}
			time.Sleep(5 * time.Millisecond)

			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/p2/acme/widget.json", nil))
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body %s", w.Code, tt.wantStatus, w.Body)
			}
			if stale := w.Header().Get("Warning") == staleWarning; stale != tt.wantStale {
				t.Errorf("Warning = %q, want stale %v", w.Header().Get("Warning"), tt.wantStale)
			}
		})
	}
}
