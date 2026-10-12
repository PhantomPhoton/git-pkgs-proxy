package handler

import (
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/git-pkgs/proxy/internal/metrics"
)

// rewriteCache keeps metadata documents after a handler has rewritten them.
// Rewriting means decoding the whole document into generic maps, changing its
// download URLs and encoding it again, which for a large npm packument or an
// expanded Composer document costs milliseconds to hundreds of milliseconds
// and megabytes of allocations. It ran on every request, cached metadata
// included. The cache keeps one rewrite per distinct upstream document, and
// concurrent requests for a document that is not cached yet share a single
// rewrite.
//
// Entries are keyed by a hash of the raw document, so new bytes from upstream
// are rewritten again rather than served stale. Everything else a rewrite
// depends on is fixed for the life of the process: the proxy URL is part of
// the key, and the denylist is loaded at startup.
//
// Each entry keeps the ETag of its output, so clients can revalidate their
// copy without the output being hashed again on every request. The cache also
// dates what it serves for each package; see lastModified.
type rewriteCache struct {
	maxBytes int64
	now      func() time.Time

	mu       sync.Mutex
	size     int64
	order    *list.List // most recently used at the front
	entries  map[string]*list.Element
	inFlight map[string]*inflightRewrite
	// serving holds the rewrite each package is serving, keyed by its cache
	// key without the digest, while that rewrite is cached.
	serving map[string]servedRewrite
	// forgotten is the latest second in which the cache may have dated a
	// rewrite of a package it no longer has in serving: the second it started,
	// as the previous process may have dated one then, or a later one.
	forgotten time.Time
}

// servedRewrite is the rewrite a package is serving and since when.
type servedRewrite struct {
	etag string
	// switched is the second the package started serving it.
	switched time.Time
	// since is its Last-Modified: the first second, from switched on, that
	// can have dated no other rewrite of the package. Zero until there is one.
	since time.Time
}

type rewriteEntry struct {
	key  string
	out  []byte
	etag string
}

// inflightRewrite is one rewrite that concurrent callers share. out, etag
// and err are written before done closes and read only after, so the close is
// the handoff.
type inflightRewrite struct {
	done chan struct{}
	out  []byte
	etag string
	err  error
}

// metadataValidators are what a client revalidates its copy of rewritten
// metadata with. lastModified is zero when there is none to give.
type metadataValidators struct {
	etag         string
	lastModified time.Time
}

// rewrittenMetadata is a rewritten metadata document with its validators.
type rewrittenMetadata struct {
	body []byte
	metadataValidators
}

// metadataETag returns the strong ETag of a rewritten document.
func metadataETag(out []byte) string {
	sum := sha256.Sum256(out)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// rewriteConfigNonce stands for the configuration a rewrite reads at startup,
// such as the denylist and cooldown rules, in ETags derived from a rewrite's
// inputs. It changes on every start, so no ETag outlives the configuration it
// was derived under.
var rewriteConfigNonce = rand.Text()

// keptVersionsETag returns the strong ETag of a rewrite from what fixes its
// output: the upstream document's digest, the proxy URL and package name its
// download URLs carry, the versions cooldown and the denylist kept, and the
// startup configuration. Hashing these costs far less than hashing the
// rewritten document. Each field is length-prefixed, so different inputs never
// hash the same bytes.
func keptVersionsETag(digest, proxyURL, name string, kept []string) string {
	h := sha256.New()
	var length []byte
	for _, field := range append([]string{rewriteConfigNonce, digest, proxyURL, name}, slices.Sorted(slices.Values(kept))...) {
		length = binary.AppendUvarint(length[:0], uint64(len(field)))
		_, _ = h.Write(length)
		_, _ = io.WriteString(h, field)
	}
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`
}

// keepingRewrite rewrites a metadata document and reports the versions it
// kept. Together with the document they fix the rewrite's output.
type keepingRewrite func([]byte) ([]byte, []string, error)

// errSharedRewriteAbandoned is what waiters see if the caller running a
// shared rewrite panicked out of it.
var errSharedRewriteAbandoned = errors.New("shared metadata rewrite did not complete")

// newRewriteCache returns a cache holding up to maxBytes of rewritten output.
// A size of zero or less disables it.
func newRewriteCache(maxBytes int64) *rewriteCache {
	if maxBytes <= 0 {
		return nil
	}
	return &rewriteCache{
		maxBytes:  maxBytes,
		now:       time.Now,
		order:     list.New(),
		entries:   make(map[string]*list.Element),
		inFlight:  make(map[string]*inflightRewrite),
		serving:   make(map[string]servedRewrite),
		forgotten: time.Now().Truncate(time.Second),
	}
}

// rewriteCacheKey identifies one rewrite: the ecosystem and proxy URL the
// handler rewrites for, the package, and the exact upstream bytes.
func rewriteCacheKey(ecosystem, proxyURL, name string, in []byte) string {
	sum := sha256.Sum256(in)
	return rewriteCacheKeyForDigest(ecosystem, proxyURL, name, "sha256:"+hex.EncodeToString(sum[:]))
}

// rewriteCacheKeyForDigest is rewriteCacheKey for upstream bytes known only by
// their digest, in the form the metadata cache records it. The digest comes
// last, so the key without it names the package the rewrite is for.
func rewriteCacheKeyForDigest(ecosystem, proxyURL, name, digest string) string {
	return strings.Join([]string{ecosystem, proxyURL, name, digest}, "\x00")
}

// get returns the cached rewrite for key and its validators, if there is one.
func (c *rewriteCache) get(key string) ([]byte, metadataValidators, bool) {
	if c == nil {
		return nil, metadataValidators{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil, metadataValidators{}, false
	}
	c.order.MoveToFront(el)
	e := el.Value.(*rewriteEntry)
	return e.out, c.validators(e), true
}

// rewrite returns rewrite(in) and its validators, from the cache when it can.
// Callers must treat the returned bytes as read-only: a cached result is
// shared. A caller waiting on another's rewrite leaves when ctx ends; the
// rewrite itself always runs to completion, so the result is cached for the
// next request.
func (c *rewriteCache) rewrite(ctx context.Context, key string, in []byte, rewrite func([]byte) ([]byte, error)) (out []byte, v metadataValidators, err error) {
	if c == nil {
		return rewriteWithETag(in, rewrite)
	}

	c.mu.Lock()
	if el, ok := c.entries[key]; ok {
		c.order.MoveToFront(el)
		v := c.validators(el.Value.(*rewriteEntry))
		c.mu.Unlock()
		return el.Value.(*rewriteEntry).out, v, nil
	}
	if f, ok := c.inFlight[key]; ok {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, metadataValidators{}, ctx.Err()
		case <-f.done:
			return f.out, c.servedValidators(key, f.etag), f.err
		}
	}
	f := &inflightRewrite{done: make(chan struct{}), err: errSharedRewriteAbandoned}
	c.inFlight[key] = f
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.inFlight, key)
		if f.err == nil {
			c.add(&rewriteEntry{key: key, out: f.out, etag: f.etag})
		}
		c.mu.Unlock()
		close(f.done)
		out, v, err = f.out, c.servedValidators(key, f.etag), f.err
	}()
	var fv metadataValidators
	f.out, fv, f.err = rewriteWithETag(in, rewrite)
	f.etag = fv.etag
	return f.out, fv, f.err
}

// servedValidators returns the validators to serve the rewrite made for key
// with, if it was cached, and its bare ETag otherwise.
func (c *rewriteCache) servedValidators(key, etag string) metadataValidators {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok && el.Value.(*rewriteEntry).etag == etag {
		return c.validators(el.Value.(*rewriteEntry))
	}
	return metadataValidators{etag: etag}
}

// validators returns the validators to serve cached rewrite e with, and
// records that its package is serving it. Called with mu held.
func (c *rewriteCache) validators(e *rewriteEntry) metadataValidators {
	return metadataValidators{etag: e.etag, lastModified: c.lastModified(rewritePackage(e.key), e.etag)}
}

// rewritePackage returns the part of a rewrite cache key that names the
// package, without the digest of the upstream document.
func rewritePackage(key string) string {
	return key[:strings.LastIndexByte(key, 0)]
}

// lastModified returns the Last-Modified to serve the rewrite of pkg with
// the given ETag with, or zero for none, and records that pkg is serving it.
//
// The date is the second pkg started serving the rewrite, which is later than
// any date given to a rewrite pkg served before, so a client holding one never
// gets 304 for another. Upstream's date cannot serve, since the rewrite also
// depends on configuration read at startup, and a rewrite cached earlier and
// served again keeps no date of its own. Dates are never ahead of the clock,
// so they hold across a restart. When pkg switches rewrites in a second that
// already dated one, the new one shares that second with a date a client may
// hold, so it goes undated until the next second. Called with mu held.
func (c *rewriteCache) lastModified(pkg, etag string) time.Time {
	now := c.now().Truncate(time.Second)
	cur, ok := c.serving[pkg]
	switch {
	case ok && cur.etag == etag:
		if cur.since.IsZero() && now.After(cur.switched) {
			cur.since = now
		}
	case ok:
		since := now
		if !cur.switched.Before(now) || !cur.since.Before(now) {
			since = time.Time{}
		}
		cur = servedRewrite{etag: etag, switched: now, since: since}
	default:
		since := now
		if !now.After(c.forgotten) {
			since = time.Time{}
		}
		cur = servedRewrite{etag: etag, switched: now, since: since}
	}
	c.serving[pkg] = cur
	return cur.since
}

// forget drops what serving records for e's package when e is the rewrite
// the package is serving, so the map holds only packages with a cached
// rewrite. Called with mu held, as e is evicted.
func (c *rewriteCache) forget(e *rewriteEntry) {
	pkg := rewritePackage(e.key)
	cur, ok := c.serving[pkg]
	if !ok || cur.etag != e.etag {
		return
	}
	delete(c.serving, pkg)
	now := c.now().Truncate(time.Second)
	if !cur.switched.Before(now) || !cur.since.Before(now) {
		c.forgotten = now
	}
}

// rewriteWithETag runs rewrite and computes the ETag of what it returns. It
// gives no Last-Modified: nothing records when an uncached rewrite was served.
func rewriteWithETag(in []byte, rewrite func([]byte) ([]byte, error)) ([]byte, metadataValidators, error) {
	out, err := rewrite(in)
	if err != nil {
		return nil, metadataValidators{}, err
	}
	return out, metadataValidators{etag: metadataETag(out)}, nil
}

// add stores e and evicts least recently used entries until the cache is
// back under its limit. Output larger than the whole cache is not stored.
// Called with mu held.
func (c *rewriteCache) add(e *rewriteEntry) {
	n := int64(len(e.out))
	if n > c.maxBytes {
		return
	}
	c.entries[e.key] = c.order.PushFront(e)
	c.size += n
	for c.size > c.maxBytes {
		oldest := c.order.Back()
		e := oldest.Value.(*rewriteEntry)
		c.order.Remove(oldest)
		delete(c.entries, e.key)
		c.forget(e)
		c.size -= int64(len(e.out))
	}
}

// len reports how many rewrites are cached.
func (c *rewriteCache) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// cachedRewrite rewrites a metadata document through the proxy's rewrite
// cache and returns it with its validators. digest identifies in when the
// metadata cache stored it, and is empty otherwise. Cooldown filtering depends
// on the current time, so with cooldown on a cached rewrite could keep hiding
// a version past its cooldown; those rewrites always run, with no
// Last-Modified, and when digest is known their ETag comes from their inputs
// rather than from hashing each output again.
func (p *Proxy) cachedRewrite(ctx context.Context, ecosystem, proxyURL, name string, in []byte, digest string, rewrite keepingRewrite) (rewrittenMetadata, error) {
	if p.cooldownEnabled() && digest != "" {
		out, kept, err := rewrite(in)
		if err != nil {
			return rewrittenMetadata{}, err
		}
		return rewrittenMetadata{out, metadataValidators{etag: keptVersionsETag(digest, proxyURL, name, kept)}}, nil
	}
	rewriteOnly := func(b []byte) ([]byte, error) {
		out, _, err := rewrite(b)
		return out, err
	}
	var out []byte
	var v metadataValidators
	var err error
	if p.rewrites == nil || p.cooldownEnabled() {
		out, v, err = rewriteWithETag(in, rewriteOnly)
	} else {
		out, v, err = p.rewrites.rewrite(ctx, rewriteCacheKey(ecosystem, proxyURL, name, in), in, rewriteOnly)
	}
	return rewrittenMetadata{out, v}, err
}

func (p *Proxy) cooldownEnabled() bool {
	return p.Cooldown != nil && p.Cooldown.Enabled()
}

// storedRewrite returns the cached rewrite of the metadata stored for
// ecosystem and cacheKey, when that metadata is still within its TTL and a
// rewrite of it is cached. It reads only the cache row, never the stored
// document, so a repeated request for a large packument costs a database
// lookup instead of reading and hashing the whole document again. When it
// reports false the caller takes the usual fetch and rewrite path.
func (p *Proxy) storedRewrite(ecosystem, cacheKey, proxyURL, name string) (rewrittenMetadata, bool) {
	if p.rewrites == nil || p.cooldownEnabled() {
		return rewrittenMetadata{}, false
	}
	if !p.CacheMetadata || p.DB == nil || p.MetadataTTL <= 0 {
		return rewrittenMetadata{}, false
	}
	entry, err := p.DB.GetMetadataCache(ecosystem, cacheKey)
	if err != nil || entry == nil || !entry.ContentDigest.Valid || entry.ContentEncoding.String != "" {
		return rewrittenMetadata{}, false
	}
	if !entry.FetchedAt.Valid || time.Since(entry.FetchedAt.Time) >= p.MetadataTTL {
		return rewrittenMetadata{}, false
	}
	out, v, ok := p.rewrites.get(rewriteCacheKeyForDigest(ecosystem, proxyURL, name, entry.ContentDigest.String))
	if !ok {
		return rewrittenMetadata{}, false
	}
	metrics.RecordCacheHit(ecosystem)
	return rewrittenMetadata{out, v}, true
}

// serveRewrittenMetadata writes rewritten metadata with its validators, or
// 304 Not Modified when they show the client's copy is current.
func serveRewrittenMetadata(w http.ResponseWriter, r *http.Request, m rewrittenMetadata) {
	w.Header().Set(headerETag, m.etag)
	if !m.lastModified.IsZero() {
		w.Header().Set(headerLastModified, m.lastModified.UTC().Format(http.TimeFormat))
	}
	if notModified(r, m.etag, m.lastModified) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(m.body)
}

// notModified reports whether a conditional request's validators match.
// If-Modified-Since only counts when the request has no If-None-Match.
func notModified(r *http.Request, etag string, lastModified time.Time) bool {
	if header := r.Header.Get("If-None-Match"); header != "" {
		return ifNoneMatchHits(header, etag)
	}
	if lastModified.IsZero() {
		return false
	}
	since, err := http.ParseTime(r.Header.Get("If-Modified-Since"))
	return err == nil && !lastModified.Truncate(time.Second).After(since)
}

// SetMetadataRewriteCacheSize enables the cache of rewritten metadata with
// room for maxBytes of output. Zero or less disables it.
func (p *Proxy) SetMetadataRewriteCacheSize(maxBytes int64) {
	p.rewrites = newRewriteCache(maxBytes)
}
