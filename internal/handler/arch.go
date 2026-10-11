package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
)

const (
	archEcosystem             = "arch"
	defaultArchRepositoryName = "archlinux"
	// defaultArchUpstream is the official Arch Linux GeoIP mirror.
	defaultArchUpstream  = "https://geo.mirror.pkgbuild.com"
	archMatchCount       = 4 // full match + name + version + arch
	archSignatureSuffix  = ".sig"
	archAccept           = "*/*"
	archPackageMediaType = "application/octet-stream"
)

// ArchHandler serves pacman repositories at /arch/{repository}/. The rest of
// the path goes to the mirror unchanged, so the Arch Linux ARM layout works
// too. Databases are served byte for byte through the metadata cache, packages
// and their signatures through the artifact cache.
type ArchHandler struct {
	proxy        *Proxy
	proxyURL     string
	repositories map[string]string
}

// NewArchHandler creates an Arch Linux handler. Without repositories it serves
// the official GeoIP mirror as "archlinux".
func NewArchHandler(proxy *Proxy, proxyURL string, repositories map[string]string) *ArchHandler {
	h := &ArchHandler{
		proxy:        proxy,
		proxyURL:     strings.TrimSuffix(proxyURL, "/"),
		repositories: make(map[string]string, len(repositories)),
	}
	for name, repositoryURL := range repositories {
		h.repositories[name] = strings.TrimSuffix(repositoryURL, "/")
	}
	if len(h.repositories) == 0 {
		h.repositories[defaultArchRepositoryName] = defaultArchUpstream
	}
	return h
}

// Routes returns the HTTP handler for pacman requests.
// Mount this at /arch on your router.
func (h *ArchHandler) Routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/")

		if containsPathTraversal(path) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}

		repository, rest, ok := strings.Cut(path, "/")
		upstreamURL, found := h.repositories[repository]
		if !ok || rest == "" || !found {
			http.NotFound(w, r)
			return
		}

		switch {
		case isArchDatabase(rest):
			h.handleMetadata(w, r, repository, upstreamURL, rest)
		case isArchDatabaseSignature(rest):
			h.handleDatabaseSignature(w, r, repository, upstreamURL, rest)
		case isArchPackage(rest) || isArchPackageSignature(rest):
			h.handlePackageDownload(w, r, repository, upstreamURL, rest)
		default:
			h.proxyFile(w, r, upstreamURL, rest)
		}
	})
}

// archDatabasePattern matches {repo}.db and {repo}.files, bare or as .tar.*.
var archDatabasePattern = regexp.MustCompile(`^[^/]+\.(?:db|files)(?:\.tar(?:\.(?:gz|xz|zst|bz2|lz4|lzo|lrz|lz|Z))?)?$`)

func archBase(path string) string {
	return path[strings.LastIndex(path, "/")+1:]
}

func isArchDatabase(path string) bool {
	return archDatabasePattern.MatchString(archBase(path))
}

func isArchDatabaseSignature(path string) bool {
	base, ok := strings.CutSuffix(archBase(path), archSignatureSuffix)
	return ok && archDatabasePattern.MatchString(base)
}

// archPackagePattern matches {pkgname}-{pkgver}-{pkgrel}-{arch}.pkg.tar[.ext],
// e.g. lz4-1:1.10.0-2-x86_64.pkg.tar.zst. pkgname may contain '-', and pkgver
// may carry an epoch.
var archPackagePattern = regexp.MustCompile(`^(.+)-((?:\d+:)?[^-/:]+-[^-/:]+)-([^-/:]+)\.pkg\.tar(?:\.(?:zst|xz|gz|bz2|lz4|lzo|lrz|lz|Z))?$`)

func isArchPackage(path string) bool {
	return archPackagePattern.MatchString(archBase(path))
}

func isArchPackageSignature(path string) bool {
	base, ok := strings.CutSuffix(archBase(path), archSignatureSuffix)
	return ok && archPackagePattern.MatchString(base)
}

// parseArchPackage returns name, pkgver-pkgrel and arch of a package or .sig.
func parseArchPackage(filename string) (name, version, arch string) {
	matches := archPackagePattern.FindStringSubmatch(strings.TrimSuffix(filename, archSignatureSuffix))
	if len(matches) != archMatchCount {
		return "", "", ""
	}
	return matches[1], matches[2], matches[3]
}

// handlePackageDownload caches packages and their signatures. The cache key
// holds repository and full path, since two repositories may ship different
// bytes under one filename.
func (h *ArchHandler) handlePackageDownload(w http.ResponseWriter, r *http.Request, repository, upstreamURL, path string) {
	name, version, arch := parseArchPackage(archBase(path))
	if name == "" {
		h.proxyFile(w, r, upstreamURL, path)
		return
	}

	downloadURL := upstreamURL + "/" + path
	cacheFilename := repository + "/" + path

	h.proxy.Logger.Info("arch package download",
		"repository", repository, "name", name, "version", version, "arch", arch,
		"signature", strings.HasSuffix(path, archSignatureSuffix))

	result, err := h.proxy.GetOrFetchArtifactFromURL(
		r.Context(), archEcosystem, name, version, cacheFilename, downloadURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch package")
		return
	}

	if result.Artifact.MediaType == "" {
		result.Artifact.MediaType = archPackageMediaType
	}
	ServeArtifactRequest(w, r, result)
}

// handleMetadata serves databases unchanged through the metadata cache.
func (h *ArchHandler) handleMetadata(w http.ResponseWriter, r *http.Request, repository, upstreamURL, path string) {
	h.proxy.ProxyCached(w, r, upstreamURL+"/"+path, archEcosystem,
		h.metadataCacheKey(repository, upstreamURL, path), archAccept)
}

// handleDatabaseSignature lets a database's .sig follow the database instead
// of its own TTL, since pacman rejects a mismatched pair. A signature older
// than the cached database is fetched again.
func (h *ArchHandler) handleDatabaseSignature(w http.ResponseWriter, r *http.Request, repository, upstreamURL, path string) {
	p := h.proxy
	sigKey := h.metadataCacheKey(repository, upstreamURL, path)
	if !p.CacheMetadata || p.DB == nil {
		h.handleMetadata(w, r, repository, upstreamURL, path)
		return
	}

	dbKey := h.metadataCacheKey(repository, upstreamURL, strings.TrimSuffix(path, archSignatureSuffix))
	dbEntry, _ := p.DB.GetMetadataCache(archEcosystem, dbKey)
	sigEntry, _ := p.DB.GetMetadataCache(archEcosystem, sigKey)
	if dbEntry == nil || sigEntry == nil || !dbEntry.FetchedAt.Valid || !sigEntry.FetchedAt.Valid {
		h.handleMetadata(w, r, repository, upstreamURL, path)
		return
	}

	if !sigEntry.FetchedAt.Time.Before(dbEntry.FetchedAt.Time) {
		body, contentType, err := p.readCachedMetadata(r.Context(), sigEntry, nil)
		if err == nil {
			p.writeMetadataCachedResponseWithEncoding(w, r, archEcosystem, sigKey, body, contentType, sigEntry.ContentEncoding.String)
			return
		}
		// The cached bytes are gone; refetch below.
	}

	signatureURL := upstreamURL + "/" + path
	// Own key: a regular fetch would return the cached copy within its TTL.
	key := metadataCoalesceKey(archEcosystem, sigKey, signatureURL, archAccept, "identity", false) + "\x00follow-database"
	res, _ := p.coalesceMetadata(r.Context(), key, func(fetchCtx context.Context) metadataResult {
		return p.fetchMetadataFromUpstream(fetchCtx, archEcosystem, sigKey, signatureURL, archAccept, "identity", nil, sigEntry)
	})
	if res.err != nil {
		if errors.Is(res.err, ErrUpstreamNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		p.Logger.Error("metadata fetch failed", "error", res.err)
		http.Error(w, "failed to fetch from upstream", http.StatusBadGateway)
		return
	}
	p.writeMetadataCachedResponseWithEncoding(w, r, archEcosystem, sigKey, res.body, res.contentType, res.contentEncoding)
}

// metadataCacheKey hashes repository, upstream URL and path, as APKHandler does.
func (h *ArchHandler) metadataCacheKey(repository, upstreamURL, path string) string {
	identity := repository + "\x00" + upstreamURL + "\x00" + path
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

// proxyFile passes other files (lastupdate, ISOs) through uncached.
func (h *ArchHandler) proxyFile(w http.ResponseWriter, r *http.Request, upstreamURL, path string) {
	h.proxy.ProxyFile(w, r, upstreamURL+"/"+path)
}
