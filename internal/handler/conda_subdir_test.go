package handler

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/git-pkgs/registries/fetch"
)

// The same conda filename can hold different bytes in each subdir, so each
// subdir must get its own cache entry.
func TestCondaDownloadCachesPerSubdir(t *testing.T) {
	const filename = "python_abi-3.11-4_cp311.conda"
	packages := map[string]string{
		"/conda-forge/linux-64/" + filename:  "linux-64 bytes",
		"/conda-forge/osx-arm64/" + filename: "osx-arm64 bytes",
	}

	var available atomic.Bool
	available.Store(true)
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := packages[r.URL.Path]
		if !available.Load() || !ok {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		_, _ = w.Write([]byte(data))
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	routes := NewCondaHandlerWithUpstream(proxy, "http://proxy.local", upstream.URL).Routes()

	get := func(path string) string {
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	for _, round := range []string{"fetch", "cached"} {
		for path, want := range packages {
			if got := get(path); got != want {
				t.Errorf("%s %s: body = %q, want %q", round, path, got, want)
			}
		}
		available.Store(false)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("upstream requests = %d, want 2 (one per subdir)", got)
	}
}
