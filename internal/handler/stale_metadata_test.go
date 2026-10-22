package handler

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// upstreamThen serves body once and answers every later request with status.
func upstreamThen(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// staleProxy returns a proxy whose cached metadata expires right away.
func staleProxy(t *testing.T, upstream *httptest.Server) *Proxy {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Millisecond
	proxy.HTTPClient = upstream.Client()
	return proxy
}

// requestTwice requests path, lets the cached copy expire, and requests it
// again.
func requestTwice(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, path, nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, body %s", first.Code, first.Body)
	}
	time.Sleep(5 * time.Millisecond)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// staleStatuses are upstream answers the metadata cache falls back on.
var staleStatuses = []int{http.StatusBadGateway, http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden}

const staleComposerDocument = `{"packages":{"acme/widget":[{"name":"acme/widget","version":"1.0.0",` +
	`"dist":{"type":"zip","url":"https://repo.packagist.org/files/acme/widget/1.0.0/widget.zip"}}]},"minified":"composer/2.0"}`

func TestComposerStaleMetadataIsMarked(t *testing.T) {
	for _, status := range staleStatuses {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := upstreamThen(t, staleComposerDocument, status)
			h := NewComposerHandlerWithUpstreams(staleProxy(t, upstream), "http://proxy.test", upstream.URL, upstream.URL).Routes()

			w := requestTwice(t, h, "/p2/acme/widget.json")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 from the cache, body %s", w.Code, w.Body)
			}
			if got := w.Header().Get("Warning"); got != staleWarning {
				t.Errorf("Warning = %q, want %q", got, staleWarning)
			}
		})
	}
}

const staleChefUniverse = `{"apt":{"7.5.0":{"location_type":"supermarket",` +
	`"location_path":"https://supermarket.chef.io/api/v1","download_url":` +
	`"https://supermarket.chef.io/api/v1/cookbooks/apt/versions/7.5.0/download","dependencies":{}}}}`

func TestChefStaleMetadataIsMarked(t *testing.T) {
	for _, status := range staleStatuses {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := upstreamThen(t, staleChefUniverse, status)
			h := NewChefHandler(staleProxy(t, upstream), chefTestProxyURL, upstream.URL).Routes()

			w := requestTwice(t, h, "/universe")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 from the cache, body %s", w.Code, w.Body)
			}
			if got := w.Header().Get("Warning"); got != staleWarning {
				t.Errorf("Warning = %q, want %q", got, staleWarning)
			}
		})
	}
}
