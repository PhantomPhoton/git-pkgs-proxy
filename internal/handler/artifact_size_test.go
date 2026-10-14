package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/git-pkgs/registries/fetch"
)

type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error {
	c.closed = true
	return nil
}

func TestLimitArtifactSizeUnlimitedReturnsFetcher(t *testing.T) {
	fetcher := &mockFetcher{}
	if got := LimitArtifactSize(fetcher, 0); got != fetcher {
		t.Errorf("LimitArtifactSize(f, 0) = %T, want the fetcher itself", got)
	}
}

func TestLimitArtifactSizeRefusesDeclaredOversize(t *testing.T) {
	body := &closeRecorder{Reader: strings.NewReader("0123456789")}
	limited := LimitArtifactSize(&mockFetcher{artifact: &fetch.Artifact{Body: body, Size: 10}}, 4)

	_, err := limited.Fetch(context.Background(), "https://registry.example/a.tgz")
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("Fetch error = %v, want ErrArtifactTooLarge", err)
	}
	if !body.closed {
		t.Error("refused body was not closed")
	}
}

func TestLimitArtifactSizeFailsUndeclaredOversizeWhileReading(t *testing.T) {
	limited := LimitArtifactSize(&mockFetcher{artifact: &fetch.Artifact{
		Body: io.NopCloser(strings.NewReader("0123456789")), Size: -1,
	}}, 4)

	artifact, err := limited.Fetch(context.Background(), "https://registry.example/a.tgz")
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	if _, err := io.ReadAll(artifact.Body); !errors.Is(err, ErrArtifactTooLarge) {
		t.Errorf("reading body error = %v, want ErrArtifactTooLarge", err)
	}
}

func TestLimitArtifactSizeAllowsArtifactAtLimit(t *testing.T) {
	limited := LimitArtifactSize(&mockFetcher{artifact: &fetch.Artifact{
		Body: io.NopCloser(strings.NewReader("0123")), Size: -1,
	}}, 4)

	artifact, err := limited.FetchWithHeaders(context.Background(), "https://registry.example/a.tgz", nil)
	if err != nil {
		t.Fatalf("Fetch failed: %v", err)
	}
	data, err := io.ReadAll(artifact.Body)
	if err != nil || string(data) != "0123" {
		t.Errorf("body = %q, %v; want %q, nil", data, err, "0123")
	}
}

func TestOversizedArtifactIsNotCached(t *testing.T) {
	proxy, db, store, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("0123456789")), Size: -1}
	proxy.Fetcher = LimitArtifactSize(fetcher, 4)

	_, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "npm", "big", "1.0.0", "big-1.0.0.tgz", "https://registry.example/big-1.0.0.tgz")
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("GetOrFetchArtifactFromURL error = %v, want ErrArtifactTooLarge", err)
	}
	if len(store.files) != 0 {
		t.Errorf("storage holds %d objects after an oversized fetch, want 0", len(store.files))
	}
	record, err := db.GetArtifact("pkg:npm/big@1.0.0", "big-1.0.0.tgz")
	if err != nil {
		t.Fatalf("GetArtifact failed: %v", err)
	}
	if record != nil {
		t.Error("oversized artifact was recorded in the cache database")
	}

	rec := httptest.NewRecorder()
	proxy.serveArtifactError(rec, err, "failed to fetch package")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

func TestOversizedStreamedArtifactFailsTheStream(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.StreamArtifacts = true
	fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("0123456789")), Size: -1}
	proxy.Fetcher = LimitArtifactSize(fetcher, 4)

	result, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "npm", "big", "1.0.0", "big-1.0.0.tgz", "https://registry.example/big-1.0.0.tgz")
	if err != nil {
		t.Fatalf("GetOrFetchArtifactFromURL failed: %v", err)
	}
	defer func() { _ = result.Reader.Close() }()
	if _, err := io.ReadAll(result.Reader); !errors.Is(err, ErrArtifactTooLarge) {
		t.Errorf("streamed body error = %v, want ErrArtifactTooLarge", err)
	}
}
