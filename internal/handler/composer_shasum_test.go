package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/git-pkgs/registries/fetch"
)

func TestComposerDownloadVerifiesDeclaredShasum(t *testing.T) {
	const archive = "archive bytes"

	tests := []struct {
		name       string
		shasum     string
		wantStatus int
	}{
		{"matching shasum", sha1Hex(archive), http.StatusOK},
		{"mismatched shasum", sha1Hex("other bytes"), http.StatusBadGateway},
		{"empty shasum", "", http.StatusOK},
		{"unparseable shasum", "not-a-sha1", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, proxy := setupComposerShasum(t, "1.0.0", "/zip/p.zip", tt.shasum, map[string]string{"/zip/p.zip": archive})

			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/v/p/1.0.0/p.zip", nil))
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tt.wantStatus, w.Body.String())
			}
			if wantCached := tt.wantStatus == http.StatusOK; isComposerArchiveCached(t, proxy, "1.0.0", "p.zip") != wantCached {
				t.Errorf("cached = %v, want %v", !wantCached, wantCached)
			}
		})
	}
}

// TestComposerDownloadPinnedCommitIgnoresListedShasum checks that the shasum
// listed for a branch's head is not held against an older commit a lock file
// pins: it describes another archive.
func TestComposerDownloadPinnedCommitIgnoresListedShasum(t *testing.T) {
	h, _ := setupComposerShasum(t, "dev-master", "/zip/"+composerCommitB, sha1Hex("commit B"), map[string]string{
		"/zip/" + composerCommitA: "commit A",
		"/zip/" + composerCommitB: "commit B",
	})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/files/v/p/dev-master/"+composerCommitA+".zip", nil))
	if w.Code != http.StatusOK || w.Body.String() != "commit A" {
		t.Errorf("status %d, body %q, want 200 %q", w.Code, w.Body.String(), "commit A")
	}
}

func setupComposerShasum(t *testing.T, version, distPath, shasum string, archives map[string]string) (http.Handler, *Proxy) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/p2/v/p.json" || r.URL.Path == "/p2/v/p~dev.json" {
			_, _ = fmt.Fprintf(w, `{"packages":{"v/p":[{"version":%q,"dist":{"url":%q,"type":"zip","shasum":%q}}]}}`,
				version, srv.URL+distPath, shasum)
			return
		}
		body, ok := archives[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	proxy, _, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(srv.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	return NewComposerHandlerWithUpstreams(proxy, "http://proxy", srv.URL, srv.URL).Routes(), proxy
}

func isComposerArchiveCached(t *testing.T, proxy *Proxy, version, filename string) bool {
	t.Helper()
	cached, err := proxy.GetCachedArtifact(context.Background(), "composer", "v/p", version, filename)
	if err != nil {
		t.Fatalf("reading cache: %v", err)
	}
	if cached == nil {
		return false
	}
	_ = cached.Reader.Close()
	return true
}
