package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/registries/fetch"
)

// gatedFetcher holds every fetch until release is closed, and gives up early
// when the fetch context is cancelled, the way a real HTTP fetch does.
type gatedFetcher struct {
	calls   atomic.Int64
	content string
	entered chan struct{}
	release chan struct{}
}

func newGatedFetcher(content string) *gatedFetcher {
	return &gatedFetcher{content: content, entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (f *gatedFetcher) Fetch(ctx context.Context, url string) (*fetch.Artifact, error) {
	return f.FetchWithHeaders(ctx, url, nil)
}

func (f *gatedFetcher) FetchWithHeaders(ctx context.Context, _ string, _ http.Header) (*fetch.Artifact, error) {
	f.calls.Add(1)
	select {
	case f.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.release:
	}
	return &fetch.Artifact{
		Body:        io.NopCloser(strings.NewReader(f.content)),
		ContentType: "application/octet-stream",
	}, nil
}

func (f *gatedFetcher) Head(context.Context, string) (int64, string, error) {
	return 0, "", nil
}

const detachedFetchURL = "https://geo.mirror.pkgbuild.com/extra/os/x86_64/big-1.0-1-x86_64.pkg.tar.zst"

// startCancelledFetch starts a fetch under ctx, waits until it reached the
// upstream, cancels ctx, lets the upstream answer, and returns the caller's
// error.
func startCancelledFetch(t *testing.T, proxy *Proxy, fetcher *gatedFetcher, ctx context.Context) error {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		res, err := proxy.GetOrFetchArtifactFromURL(ctx, "arch", "big", "1.0-1", "big-1.0-1-x86_64.pkg.tar.zst", detachedFetchURL)
		drain(res)
		done <- err
	}()

	select {
	case <-fetcher.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("fetch never reached the upstream")
	}
	cancel()
	// Give a non-detached fetch the chance to observe the cancellation before
	// the upstream answers.
	time.Sleep(50 * time.Millisecond)
	close(fetcher.release)

	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("fetch did not return")
		return nil
	}
}

// TestDetachedFetch_ClientCancelStillCaches covers a client that gives up on a
// slow cache miss. pacman aborts after 10 s without data; if that cancelled
// the upstream fetch, every retry would start from zero and a large package
// would never reach the cache.
func TestDetachedFetch_ClientCancelStillCaches(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	fetcher := newGatedFetcher("package bytes")
	proxy.Fetcher = fetcher

	_ = startCancelledFetch(t, proxy, fetcher, DetachUpstreamFetch(context.Background()))

	res, err := proxy.GetOrFetchArtifactFromURL(context.Background(),
		"arch", "big", "1.0-1", "big-1.0-1-x86_64.pkg.tar.zst", detachedFetchURL)
	if err != nil {
		t.Fatalf("follow-up request: %v", err)
	}
	body, _ := io.ReadAll(res.Reader)
	_ = res.Reader.Close()
	if !res.Cached {
		t.Error("follow-up was not a cache hit: the cancelled client aborted the fetch")
	}
	if string(body) != "package bytes" {
		t.Errorf("body = %q, want %q", body, "package bytes")
	}
	if got := fetcher.calls.Load(); got != 1 {
		t.Errorf("upstream fetches = %d, want 1", got)
	}
}

// TestDetachedFetch_UnmarkedContextStillCancels keeps mirror jobs cancellable:
// without the marker, cancelling the context aborts the fetch and nothing is
// cached.
func TestDetachedFetch_UnmarkedContextStillCancels(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	fetcher := newGatedFetcher("package bytes")
	proxy.Fetcher = fetcher

	err := startCancelledFetch(t, proxy, fetcher, context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}

	cached, err := proxy.GetCachedArtifact(context.Background(), "arch", "big", "1.0-1", "big-1.0-1-x86_64.pkg.tar.zst")
	if err != nil {
		t.Fatalf("cache lookup: %v", err)
	}
	if cached != nil {
		_ = cached.Reader.Close()
		t.Error("cancelled undetached fetch was still committed to the cache")
	}
}
