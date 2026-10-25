package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
)

// openCountingBlob counts the objects a request opens.
type openCountingBlob struct {
	*storage.Blob
	opens atomic.Int32
}

func (b *openCountingBlob) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	b.opens.Add(1)
	return b.Blob.Open(ctx, path)
}

// The server marks HEAD requests, so a HEAD on a cached artifact is answered
// without opening the stored object and with the headers the GET sends.
func TestMarkRequestContextHeadSkipsStorageOpen(t *testing.T) {
	const pkgPath = "/core/os/x86_64/demo-1.0-1-x86_64.pkg.tar.zst"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pkgPath {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "demo package")
	}))
	defer upstream.Close()

	dir := t.TempDir()
	db, err := database.Create(filepath.Join(dir, "proxy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	opened, err := storage.OpenBucket(context.Background(), "file://"+filepath.Join(dir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	blob, ok := opened.(*storage.Blob)
	if !ok {
		t.Fatalf("OpenBucket returned %T, want *storage.Blob", opened)
	}
	store := &openCountingBlob{Blob: blob}
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	routes := markRequestContext(handler.NewArchHandler(proxy, "http://proxy.example",
		map[string]string{"archlinux": upstream.URL}).Routes())

	serve := func(method string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, httptest.NewRequest(method, "/archlinux"+pkgPath, nil))
		return w
	}
	if w := serve(http.MethodGet); w.Code != http.StatusOK {
		t.Fatalf("seeding GET: status %d", w.Code)
	}
	get := serve(http.MethodGet)
	if get.Code != http.StatusOK || get.Body.String() != "demo package" {
		t.Fatalf("cached GET: status %d, body %q", get.Code, get.Body.String())
	}

	before := store.opens.Load()
	head := serve(http.MethodHead)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD: status %d, body %q", head.Code, head.Body.String())
	}
	if got := store.opens.Load() - before; got != 0 {
		t.Errorf("HEAD opened the stored object %d times, want 0", got)
	}
	for _, name := range []string{"Content-Type", "Content-Length", "ETag", "Accept-Ranges"} {
		if g, h := get.Header().Get(name), head.Header().Get(name); g != h {
			t.Errorf("%s: GET %q, HEAD %q", name, g, h)
		}
	}
	if head.Header().Get("Accept-Ranges") != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", head.Header().Get("Accept-Ranges"))
	}
}
