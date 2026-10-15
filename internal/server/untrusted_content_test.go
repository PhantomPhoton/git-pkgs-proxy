package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
	"github.com/go-chi/chi/v5"
)

// TestPackageRoutesSandboxUpstreamContent checks that responses on package
// routes cannot run as pages on the proxy's origin, which also serves /ui.
// Their bodies and Content-Type come from upstreams, or from whoever pushed a
// hosted package, so an archive served as text/html would otherwise run its
// scripts with access to the UI.
func TestPackageRoutesSandboxUpstreamContent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<script>alert(document.domain)</script>")
	}))
	t.Cleanup(upstream.Close)

	db, err := database.Create(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("creating database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := storage.OpenBucket(context.Background(), "file://"+t.TempDir())
	if err != nil {
		t.Fatalf("opening storage: %v", err)
	}
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	proxy.HTTPClient = upstream.Client()

	cfg := config.Default()
	cfg.BaseURL = "http://proxy.example"
	cfg.Upstream.Generic = map[string]string{"site": upstream.URL}
	cfg.Upstream.APK = map[string]string{"alpine": upstream.URL}
	s := &Server{cfg: cfg}
	r := chi.NewRouter()
	s.mountProtocolHandlers(r, proxy)

	for _, target := range []string{
		"/generic/site/files/page-1.0.html",         // cached artifact
		"/generic/site/files/page-1.0.html",         // the same, from the cache
		"/apk/alpine/v3.22/main/x86_64/README.html", // relayed as is
		"/npm/does-not-exist%2F..",                  // an error response
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s (status %d): X-Content-Type-Options = %q, want nosniff", target, w.Code, got)
		}
		if got := w.Header().Get("Content-Security-Policy"); got != "sandbox" {
			t.Errorf("%s (status %d): Content-Security-Policy = %q, want sandbox", target, w.Code, got)
		}
	}
}
