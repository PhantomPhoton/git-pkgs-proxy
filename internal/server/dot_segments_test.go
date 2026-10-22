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

// TestProtocolHandlersRejectDotSegments sends paths with encoded '.' and '..'
// segments to every mounted ecosystem. Several handlers append the decoded
// path to the upstream URL, where a '..' leaves the configured upstream path.
func TestProtocolHandlersRejectDotSegments(t *testing.T) {
	db, err := database.Create(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("creating database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := storage.OpenBucket(context.Background(), "file://"+t.TempDir())
	if err != nil {
		t.Fatalf("opening storage: %v", err)
	}

	transport := &countingTransport{}
	client := &http.Client{Transport: transport}
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(client), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	proxy.HTTPClient = client

	cfg := config.Default()
	cfg.BaseURL = "http://proxy.example"
	s := &Server{cfg: cfg}
	r := chi.NewRouter()
	s.mountProtocolHandlers(r, proxy)

	for _, target := range []string{
		"/maven/%2e%2e/%2e%2e/secret",
		"/maven/com/%2e%2e/%2e%2e/x/maven-metadata.xml",
		"/maven/com/%2E%2E/x/1.0/x-1.0.jar",
		"/maven/com/%2e/x/1.0/x-1.0.jar",
		"/maven/com/%252e%252e/x/maven-metadata.xml",
		"/conda/c/%2e%2e/x.json",
		"/cran/src/contrib/%2e%2e%2fx.tar.gz",
		"/cran/src/contrib/..%5cx.tar.gz",
		"/go/a/%2e%2e/%2e%2e/x/@v/list",
		"/gem/info/%2e%2e%2f%2e%2e%2fx",
		"/gem/gems/%2e%2e%2fx-1.0.gem",
		"/nuget/v3-flatcontainer/%2e%2e/x/index.json",
		"/debian/pool/%2e%2e/x.deb",
		"/rpm/%2e%2e/x.rpm",
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, w.Code)
		}
	}
	if n := transport.hits.Load(); n != 0 {
		t.Errorf("upstream was contacted %d times", n)
	}
}

// TestDotSegmentGuardKeepsDottedNames keeps names that only contain dots,
// such as versions and file extensions, working.
func TestDotSegmentGuardKeepsDottedNames(t *testing.T) {
	for _, target := range []string{
		"/maven/com/example/x/1.0/x-1.0.jar",
		"/pypi/simple/zope.interface/",
		"/npm/.bin",
		"/generic/github/o/r/releases/download/v1/...tar.gz",
		"/maven/com/example/x/1..0/x-1..0.jar",
	} {
		var reached bool
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
		w := httptest.NewRecorder()
		rejectDotSegments(next).ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		if !reached {
			t.Errorf("%s: status = %d, want the request passed on", target, w.Code)
		}
	}
}
