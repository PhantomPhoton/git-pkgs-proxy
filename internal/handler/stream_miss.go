package handler

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/git-pkgs/artifacts"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/registries/fetch"
	"github.com/opencontainers/go-digest"
)

// On a cache miss the proxy used to store the whole artifact before sending
// any of it, so a client with an idle timeout (git-lfs gives up after 30
// seconds without data, pacman after 10) could give up on a large one. A
// marked request instead gets the bytes as they arrive from upstream while
// they are stored: the caller that starts the shared fetch is handed a
// stream, and callers joining it still wait for the stored artifact.

type streamCacheMissKey struct{}

// StreamCacheMiss marks ctx so that on an artifact cache miss the bytes are
// sent to the client while they are being stored. The server sets it on GET
// requests without a Range header; a range of a stream that is still
// arriving can't be served.
func StreamCacheMiss(ctx context.Context) context.Context {
	return context.WithValue(ctx, streamCacheMissKey{}, true)
}

func streamsCacheMiss(ctx context.Context) bool {
	stream, _ := ctx.Value(streamCacheMissKey{}).(bool)
	return stream && !servingHead(ctx)
}

// missHandoff passes the client's stream from the shared fetch, which runs on
// its own goroutine, to the caller that started it. The caller can leave
// before a stream is offered, and the fetch must then not write to a stream
// nobody reads.
type missHandoff struct {
	mu        sync.Mutex
	ready     chan struct{}
	stream    *CacheResult
	taken     bool
	abandoned bool
}

type missHandoffKey struct{}

func handoffFrom(ctx context.Context) *missHandoff {
	h, _ := ctx.Value(missHandoffKey{}).(*missHandoff)
	return h
}

// offer hands stream to the caller and reports whether it took it.
func (h *missHandoff) offer(stream *CacheResult) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.abandoned {
		return false
	}
	h.stream, h.taken = stream, true
	close(h.ready)
	return true
}

// handedOver reports whether the caller took a stream.
func (h *missHandoff) handedOver() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.taken
}

// runSharedFetchStreaming runs the shared fetch on its own goroutine and
// returns as soon as it offers a stream, or with its result if it finishes
// without one: on a recheck hit, an artifact another instance stored, an
// error, or an artifact a block-mode scanner has to see first. Waiters and
// the fetch lease still see the fetch end only once the artifact is stored.
func (p *Proxy) runSharedFetchStreaming(ctx context.Context, key string, f *inflightFetch, recheck func() (artifacts.Artifact, string, bool), commit func(context.Context) (artifacts.Artifact, string, error)) (*CacheResult, error) {
	h := &missHandoff{ready: make(chan struct{})}
	type outcome struct {
		res *CacheResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := p.runSharedFetch(context.WithValue(ctx, missHandoffKey{}, h), key, f, recheck, commit)
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.taken || h.abandoned {
			// The caller has the stream, or has gone; nobody reads this.
			if res != nil && res.Reader != nil {
				_ = res.Reader.Close()
			}
			return
		}
		done <- outcome{res, err}
	}()

	select {
	case <-h.ready:
		return h.stream, nil
	case o := <-done:
		return o.res, o.err
	case <-ctx.Done():
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.taken {
			return h.stream, nil
		}
		h.abandoned = true
		select {
		case o := <-done:
			if o.res != nil && o.res.Reader != nil {
				_ = o.res.Reader.Close()
			}
		default:
		}
		return nil, ctx.Err()
	}
}

// storeFetched stores an artifact fetched from upstream. When the caller
// that started the fetch is waiting for a stream, it is sent the bytes as
// they are stored; see storeWhileServing.
func (p *Proxy) storeFetched(ctx context.Context, ecosystem, name, version, filename, pkgPURL, versionPURL, upstreamURL, upstreamHash, declared string, artifact *fetch.Artifact) (artifacts.Artifact, string, error) {
	h := handoffFrom(ctx)
	if h == nil || p.Scanners.Blocks(ecosystem) {
		return p.storeArtifact(ctx, ecosystem, name, version, filename, pkgPURL, versionPURL, upstreamURL, upstreamHash, declared, artifact)
	}
	return p.storeWhileServing(ctx, h, ecosystem, name, version, filename, pkgPURL, versionPURL, upstreamURL, upstreamHash, declared, artifact)
}

// errNoReaderLeft stops reading upstream once neither storage nor the
// client takes the bytes any more.
var errNoReaderLeft = errors.New("neither storage nor the client is reading the artifact")

// storeWhileServing copies the upstream body to storage and to the client at
// once. The client's copy is checked against upstreamHash and declared as it
// is read, and a mismatch fails it before its last bytes, so the response is
// aborted and the client discards it; storeArtifact checks the stored copy
// the same way and doesn't cache it. A client that goes away leaves the copy
// to storage running. A storage error leaves the client's copy running, so
// the client still gets the artifact; that is logged and counted, since every
// request would otherwise succeed while nothing is cached. The client's
// stream ends once the last byte is checked, before storeArtifact's scan and
// database update.
func (p *Proxy) storeWhileServing(ctx context.Context, h *missHandoff, ecosystem, name, version, filename, pkgPURL, versionPURL, upstreamURL, upstreamHash, declared string, artifact *fetch.Artifact) (artifacts.Artifact, string, error) {
	hash := strings.ToLower(upstreamHash)
	checks, err := newIntegrityChecks(hash, declared)
	if err != nil {
		_ = artifact.Body.Close()
		return artifacts.Artifact{}, "", err
	}

	clientR, clientW := io.Pipe()
	var client io.ReadCloser = clientR
	if hash != "" || declared != "" {
		if client, err = checks.wrapFailOnMismatch(clientR, func(reason string) {
			p.Logger.Warn("artifact sent on a cache miss failed its integrity check; aborting the response",
				"purl", versionPURL, "filename", filename, "reason", reason)
		}); err != nil {
			_ = artifact.Body.Close()
			return artifacts.Artifact{}, "", err
		}
	}
	stream := &CacheResult{
		Reader: client,
		Artifact: artifacts.Artifact{
			PURL:      versionPURL,
			Filename:  filename,
			MediaType: artifact.ContentType,
			// Unknown until stored, so the response goes out chunked and
			// can still be aborted.
			Size: -1,
		},
	}
	if hash != "" {
		stream.Artifact.Digest = digest.Digest("sha256:" + hash)
	}
	if !h.offer(stream) {
		_ = clientR.Close()
		return p.storeArtifact(ctx, ecosystem, name, version, filename, pkgPURL, versionPURL, upstreamURL, upstreamHash, declared, artifact)
	}

	storeR, storeW := io.Pipe()
	type stored struct {
		artifact artifacts.Artifact
		path     string
		err      error
	}
	storeDone := make(chan stored, 1)
	go func() {
		// Not cancelled with ctx: the client's request ends once it has the
		// last byte, while storage may still be finishing the upload and the
		// database write. Reading the upstream still stops with ctx, which
		// ends the copy to storage too.
		a, path, err := p.storeArtifact(context.WithoutCancel(ctx), ecosystem, name, version, filename, pkgPURL, versionPURL, upstreamURL, upstreamHash, declared,
			&fetch.Artifact{Body: storeR, Size: artifact.Size, ContentType: artifact.ContentType, ETag: artifact.ETag})
		// storeArtifact closes storeR; this covers a return before it read
		// anything, so the copy below never blocks on it.
		_ = storeR.Close()
		storeDone <- stored{a, path, err}
	}()

	readErr, clientOK := copyToBoth(artifact.Body, storeW, clientW)
	_ = artifact.Body.Close()
	_ = storeW.CloseWithError(readErr)
	_ = clientW.CloseWithError(readErr)

	out := <-storeDone
	if out.err != nil && readErr == nil && clientOK {
		p.Logger.Error("artifact sent to the client but not cached",
			"purl", versionPURL, "filename", filename, "url", upstreamURL, "error", out.err)
		metrics.RecordArtifactServedUncached(ecosystem, metrics.FailureDetail{
			PURL: versionPURL, Filename: filename, URL: upstreamURL, Err: out.err})
	}
	return out.artifact, out.path, out.err
}

// copyToBoth copies src to store and client until src ends, dropping either
// destination once a write to it fails. It returns the error that ended the
// copy, nil at the end of src, and whether the client took every byte.
func copyToBoth(src io.Reader, store, client io.Writer) (error, bool) {
	buffer := artifactCopyBufferPool.Get().(*[]byte)
	defer artifactCopyBufferPool.Put(buffer)
	buf := *buffer
	storeOK, clientOK := true, true
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if storeOK {
				if _, werr := store.Write(buf[:n]); werr != nil {
					storeOK = false
				}
			}
			if clientOK {
				if _, werr := client.Write(buf[:n]); werr != nil {
					clientOK = false
				}
			}
			if !storeOK && !clientOK {
				return errNoReaderLeft, false
			}
		}
		if errors.Is(err, io.EOF) {
			return nil, clientOK
		}
		if err != nil {
			return err, false
		}
	}
}
