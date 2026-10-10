package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/git-pkgs/proxy/internal/metrics"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/mod/sumdb/tlog"
)

const (
	// defaultGoSumDBName is the checksum database the go command uses when
	// GOSUMDB is not set.
	defaultGoSumDBName = "sum.golang.org"
	defaultGoSumDBURL  = "https://sum.golang.org"
	goEcosystem        = "golang"

	// goSumDBProbeBackoff is how long supported keeps answering 404 after the
	// proxy failed to reach a checksum database, before it tries again.
	goSumDBProbeBackoff = time.Minute
)

// goSumDBProbes remembers when the proxy last failed to reach each checksum
// database, so a database that is unreachable does not make every go command
// wait for an upstream timeout before it falls back.
type goSumDBProbes struct {
	mu     sync.Mutex
	failed map[string]time.Time
}

func (p *goSumDBProbes) recentlyFailed(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	failedAt, ok := p.failed[name]
	return ok && time.Since(failedAt) < goSumDBProbeBackoff
}

func (p *goSumDBProbes) record(name string, reachable bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if reachable {
		delete(p.failed, name)
		return
	}
	if p.failed == nil {
		p.failed = make(map[string]time.Time)
	}
	p.failed[name] = time.Now()
}

// SetSumDBs sets the checksum databases served at /sumdb/{name}/, mapping
// each database name to its base URL. An empty map keeps the default,
// sum.golang.org.
func (h *GoHandler) SetSumDBs(sumDBs map[string]string) {
	if len(sumDBs) == 0 {
		h.sumDBs = map[string]string{defaultGoSumDBName: defaultGoSumDBURL}
		return
	}
	h.sumDBs = make(map[string]string, len(sumDBs))
	for name, sumDBURL := range sumDBs {
		h.sumDBs[name] = strings.TrimSuffix(sumDBURL, "/")
	}
}

// handleSumDB serves checksum database requests that the go command sends
// through GOPROXY as /sumdb/{name}/{path}. The module proxy upstream does not
// serve them, so they are fetched from the configured database itself.
// Unknown names get 404, which makes the go command contact the database
// directly.
func (h *GoHandler) handleSumDB(w http.ResponseWriter, r *http.Request, rest string) {
	name, path, _ := strings.Cut(rest, "/")
	sumDBURL, ok := h.sumDBs[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if path == "supported" {
		if h.sumDBReachable(r, name, sumDBURL) {
			w.WriteHeader(http.StatusOK)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	immutable, valid := parseGoSumDBPath(path)
	if !valid {
		http.NotFound(w, r)
		return
	}

	cacheKey := goSumDBCacheKey(name, sumDBURL, path)
	if immutable && h.serveCachedSumDBTile(w, r, cacheKey) {
		return
	}
	h.proxySumDB(w, r, sumDBURL+"/"+path, cacheKey)
}

// sumDBReachable reports whether the proxy can serve a checksum database: it
// can fetch the latest signed tree, or has it cached. Once supported answers
// 200 the go command uses only the proxy for that database and never falls
// back to contacting it directly, so a database the proxy cannot reach gets
// 404 instead. The probe goes through the same cache entry as a latest
// request, so it costs at most one upstream request per metadata TTL.
func (h *GoHandler) sumDBReachable(r *http.Request, name, sumDBURL string) bool {
	if h.sumDBProbes.recentlyFailed(name) {
		return false
	}
	cacheKey := goSumDBCacheKey(name, sumDBURL, "latest")
	_, _, _, err := h.proxy.fetchOrCacheMetadata(r.Context(), goEcosystem, cacheKey, sumDBURL+"/latest", "identity", nil, "*/*")
	if err != nil && r.Context().Err() != nil {
		// The client went away; that says nothing about the database.
		return false
	}
	h.sumDBProbes.record(name, err == nil)
	if err != nil {
		h.proxy.Logger.Info("checksum database unreachable, sending go clients to it directly",
			"name", name, "url", sumDBURL, "error", err)
	}
	return err == nil
}

// parseGoSumDBPath reports whether path is a request the checksum database
// protocol defines and whether its response can never change. Only complete
// tiles are immutable; partial tiles, latest and lookups follow the metadata
// TTL.
func parseGoSumDBPath(path string) (immutable, valid bool) {
	if path == "latest" {
		return false, true
	}
	if mod, ok := strings.CutPrefix(path, "lookup/"); ok {
		return false, validGoSumDBLookup(mod)
	}
	tile, err := tlog.ParseTilePath(path)
	if err != nil {
		return false, false
	}
	return tile.W == 1<<tile.H, true
}

// validGoSumDBLookup checks an escaped module@version the way the checksum
// database does. Requiring a semantic version also keeps characters such as
// '#' and '%' out of the upstream URL.
func validGoSumDBLookup(mod string) bool {
	escPath, escVersion, ok := strings.Cut(mod, "@")
	if !ok {
		return false
	}
	if _, err := module.UnescapePath(escPath); err != nil {
		return false
	}
	version, err := module.UnescapeVersion(escVersion)
	return err == nil && semver.IsValid(version)
}

// goSumDBCacheKey hashes the database name, its URL and the request path, as
// APKHandler.metadataCacheKey does, so repointing a database at another URL
// does not serve the old database's entries.
func goSumDBCacheKey(name, sumDBURL, path string) string {
	digest := sha256.Sum256([]byte(name + "\x00" + sumDBURL + "\x00" + path))
	return "sumdb/" + hex.EncodeToString(digest[:])
}

// serveCachedSumDBTile serves a complete tile from the metadata cache
// regardless of its age, since its content never changes. It reports false
// when the tile is not cached.
func (h *GoHandler) serveCachedSumDBTile(w http.ResponseWriter, r *http.Request, cacheKey string) bool {
	p := h.proxy
	if !p.CacheMetadata || p.DB == nil {
		return false
	}
	entry, err := p.DB.GetMetadataCache(goEcosystem, cacheKey)
	if err != nil || entry == nil {
		return false
	}
	body, contentType, err := p.readCachedMetadata(r.Context(), entry, nil)
	if err != nil {
		return false
	}
	metrics.RecordCacheHit(goEcosystem)

	if entry.ETag.Valid {
		w.Header().Set(headerETag, entry.ETag.String)
		if ifNoneMatchHits(r.Header.Get("If-None-Match"), entry.ETag.String) {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	w.Header().Set(headerContentType, contentType)
	w.Header().Set(headerContentLength, strconv.Itoa(len(body)))
	if entry.ContentEncoding.Valid && entry.ContentEncoding.String != "" {
		w.Header().Set(headerContentEncoding, entry.ContentEncoding.String)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return true
}

// proxySumDB fetches a checksum database response through the metadata
// cache. Unlike ProxyCached it relays the start of an upstream 404 body: the
// go command prints it, and it says why a module version is missing.
func (h *GoHandler) proxySumDB(w http.ResponseWriter, r *http.Request, upstreamURL, cacheKey string) {
	p := h.proxy
	if !p.CacheMetadata {
		p.proxyMetadataStream(w, r, upstreamURL, "identity", "*/*")
		return
	}

	body, contentType, contentEncoding, err := p.fetchOrCacheMetadata(r.Context(), goEcosystem, cacheKey, upstreamURL, "identity", nil, "*/*")
	if err != nil {
		var notFound *upstreamNotFoundError
		switch {
		case errors.As(err, &notFound) && len(notFound.body) > 0:
			w.Header().Set(headerContentType, "text/plain; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(notFound.body)
		case errors.Is(err, ErrUpstreamNotFound):
			http.Error(w, "not found", http.StatusNotFound)
		default:
			p.Logger.Error("checksum database fetch failed", "url", upstreamURL, "error", err)
			http.Error(w, "failed to fetch from upstream", http.StatusBadGateway)
		}
		return
	}

	p.writeMetadataCachedResponseWithEncoding(w, r, goEcosystem, cacheKey, body, contentType, contentEncoding)
}
