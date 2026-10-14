package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/git-pkgs/registries/fetch"
)

// ErrArtifactTooLarge is returned for an upstream artifact larger than the
// configured artifact size limit.
var ErrArtifactTooLarge = errors.New("artifact exceeds size limit")

// LimitArtifactSize wraps fetcher so no artifact over maxSize bytes is
// fetched. An artifact whose declared size is over the limit is refused
// before its body is read; one of unknown size fails once reading passes the
// limit, so the store or stream consuming it fails and nothing is cached.
// A maxSize of zero or less returns fetcher unchanged.
func LimitArtifactSize(fetcher fetch.FetcherInterface, maxSize int64) fetch.FetcherInterface { //nolint:ireturn // returns fetcher itself when there is no limit
	if maxSize <= 0 {
		return fetcher
	}
	return &sizeLimitedFetcher{FetcherInterface: fetcher, maxSize: maxSize}
}

type sizeLimitedFetcher struct {
	fetch.FetcherInterface
	maxSize int64
}

func (f *sizeLimitedFetcher) Fetch(ctx context.Context, url string) (*fetch.Artifact, error) {
	return f.limit(f.FetcherInterface.Fetch(ctx, url))
}

func (f *sizeLimitedFetcher) FetchWithHeaders(ctx context.Context, url string, headers http.Header) (*fetch.Artifact, error) {
	return f.limit(f.FetcherInterface.FetchWithHeaders(ctx, url, headers))
}

func (f *sizeLimitedFetcher) limit(artifact *fetch.Artifact, err error) (*fetch.Artifact, error) {
	if err != nil {
		return nil, err
	}
	if artifact.Size > f.maxSize {
		_ = artifact.Body.Close()
		return nil, fmt.Errorf("%w: upstream declared %d bytes, limit is %d", ErrArtifactTooLarge, artifact.Size, f.maxSize)
	}
	limited := *artifact
	limited.Body = &sizeLimitedBody{ReadCloser: artifact.Body, remaining: f.maxSize}
	return &limited, nil
}

// sizeLimitedBody fails with ErrArtifactTooLarge once more than the limit has
// been read. It reads one byte past the limit to tell an artifact of exactly
// the limit from a larger one.
type sizeLimitedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *sizeLimitedBody) Read(p []byte) (int, error) {
	if b.remaining < 0 {
		return 0, ErrArtifactTooLarge
	}
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		return n + int(b.remaining), ErrArtifactTooLarge
	}
	return n, err
}
