package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/registries/fetch"
)

// pypiUploadTimeSetup serves the JSON API for newpkg from releases, which the
// test may change, and counts its requests. cache_metadata is off, so every
// lookup reaches upstream.
func pypiUploadTimeSetup(t *testing.T) (h http.Handler, setReleases func(string), jsonRequests *atomic.Int64, fetcher *mockFetcher) {
	t.Helper()
	var mu sync.Mutex
	releases := ""
	jsonRequests = &atomic.Int64{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pypi/newpkg/json" {
			http.NotFound(w, r)
			return
		}
		jsonRequests.Add(1)
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", contentTypeJSON)
		_, _ = io.WriteString(w, releases)
	}))
	t.Cleanup(upstream.Close)

	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.Cooldown = &cooldown.Config{Default: "7d"}
	setReleases = func(s string) {
		mu.Lock()
		defer mu.Unlock()
		releases = s
	}
	return (&PyPIHandler{proxy: proxy, upstreamURL: upstream.URL, downloadURL: upstream.URL, proxyURL: "http://localhost"}).Routes(),
		setReleases, jsonRequests, fetcher
}

func uploaded(ago time.Duration) string {
	return time.Now().Add(-ago).Format(time.RFC3339)
}

// TestPyPICachedDownloadUsesStoredUploadTime checks that once a release's
// upload time is stored, downloads of its cached files don't read the JSON
// API again.
func TestPyPICachedDownloadUsesStoredUploadTime(t *testing.T) {
	h, setReleases, jsonRequests, fetcher := pypiUploadTimeSetup(t)
	setReleases(`{"releases": {"1.0.0": [{"upload_time_iso_8601": "` + uploaded(30*24*time.Hour) + `"}]}}`)
	fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("sdist")), ContentType: "application/octet-stream"}

	for range 3 {
		w := serveGet(t, h, "/packages/packages/ab/cd/ef0123456789/newpkg-1.0.0.tar.gz")
		wantStatus(t, w, http.StatusOK, "")
		if w.Body.String() != "sdist" {
			t.Fatalf("body = %q", w.Body.String())
		}
	}
	if got := jsonRequests.Load(); got != 1 {
		t.Errorf("JSON API requests = %d, want 1", got)
	}
}

// TestPyPIFileAddedToReleaseIsChecked checks that a file uploaded to a
// release after its time was stored is not let through on the stored time:
// it isn't cached, so the JSON API is read, the newer time is stored, and
// from then on the whole release is withheld until it is out of cooldown.
func TestPyPIFileAddedToReleaseIsChecked(t *testing.T) {
	h, setReleases, jsonRequests, fetcher := pypiUploadTimeSetup(t)
	sdist := `{"upload_time_iso_8601": "` + uploaded(30*24*time.Hour) + `"}`
	setReleases(`{"releases": {"1.0.0": [` + sdist + `]}}`)
	fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("sdist")), ContentType: "application/octet-stream"}

	const (
		sdistPath = "/packages/packages/ab/cd/ef0123456789/newpkg-1.0.0.tar.gz"
		wheelPath = "/packages/packages/ab/cd/ef0123456789/newpkg-1.0.0-py3-none-any.whl"
	)
	wantStatus(t, serveGet(t, h, sdistPath), http.StatusOK, "")

	// A wheel is added to the release an hour ago.
	setReleases(`{"releases": {"1.0.0": [` + sdist + `, {"upload_time_iso_8601": "` + uploaded(time.Hour) + `"}]}}`)
	fetcher.fetchCalled = false
	wantStatus(t, serveGet(t, h, wheelPath), http.StatusNotFound, "")
	if fetcher.fetchCalled {
		t.Error("fetched a file that is inside the cooldown window")
	}
	if got := jsonRequests.Load(); got != 2 {
		t.Errorf("JSON API requests = %d, want 2 (the new file is looked up)", got)
	}

	// The stored time is now the wheel's, so the cached sdist is withheld
	// too, as the JSON API lookup would do, without asking it.
	wantStatus(t, serveGet(t, h, sdistPath), http.StatusNotFound, "")
	if got := jsonRequests.Load(); got != 2 {
		t.Errorf("JSON API requests = %d, want 2", got)
	}
}
