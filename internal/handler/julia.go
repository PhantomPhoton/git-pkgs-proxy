package handler

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/git-pkgs/proxy/internal/retention"
)

const (
	juliaUpstream            = "https://pkg.julialang.org"
	juliaGeneralRegistryUUID = "23338594-aafe-5451-b93e-139f81909106"
	juliaArtifactName        = "_artifact"
	juliaRegistryName        = "_registry"

	// juliaRefreshTimeout bounds a background reload of the name map.
	juliaRefreshTimeout = 5 * time.Minute
	// maxJuliaRegistryTomlSize bounds Registry.toml. General's is a few MB.
	maxJuliaRegistryTomlSize = 64 << 20
)

var (
	juliaHexPattern  = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
	juliaUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// Julia takes no package rules: its PURLs need a uuid qualifier the cache
// doesn't store. The julia rule and the default cover package tarballs,
// artifacts and registry snapshots alike; all are content-addressed, so an
// evicted one comes back unchanged.
func init() {
	retention.Default.Register(retention.Spec{Key: "julia"})
}

// JuliaHandler handles Julia Pkg server protocol requests.
//
// See https://pkgdocs.julialang.org/v1/registries/ and the PkgServer.jl
// reference implementation. The protocol is content-addressed: registry,
// package and artifact resources are all identified by git tree hashes
// and are immutable once published.
type JuliaHandler struct {
	proxy       *Proxy
	upstreamURL string

	mu        sync.RWMutex
	names     map[string]string
	namesHash string
	loadMu    sync.Mutex
}

// NewJuliaHandler creates a new Julia Pkg server handler.
func NewJuliaHandler(proxy *Proxy, _ string) *JuliaHandler {
	return &JuliaHandler{
		proxy:       proxy,
		upstreamURL: juliaUpstream,
		names:       make(map[string]string),
	}
}

// NewJuliaHandlerWithUpstream creates a Julia handler with a custom upstream.
func NewJuliaHandlerWithUpstream(proxy *Proxy, upstreamURL string) *JuliaHandler {
	h := NewJuliaHandler(proxy, "")
	h.upstreamURL = configuredUpstreamURL(upstreamURL, juliaUpstream)
	return h
}

// Routes returns the HTTP handler for Julia requests.
func (h *JuliaHandler) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /registries", h.handleRegistries)
	mux.HandleFunc("GET /registries.eager", h.handleRegistries)
	mux.HandleFunc("GET /registries.conservative", h.handleRegistries)
	mux.HandleFunc("GET /registry/{uuid}/{hash}", h.handleRegistry)
	mux.HandleFunc("GET /package/{uuid}/{hash}", h.handlePackage)
	mux.HandleFunc("GET /artifact/{hash}", h.handleArtifact)
	mux.HandleFunc("GET /meta", h.proxyUpstream)

	return mux
}

// handleRegistries serves the list of available registries. This is the only
// mutable endpoint in the protocol so it goes through the metadata cache.
func (h *JuliaHandler) handleRegistries(w http.ResponseWriter, r *http.Request) {
	cacheKey := strings.TrimPrefix(r.URL.Path, "/")
	h.proxy.ProxyCached(w, r, h.upstreamURL+r.URL.Path, "julia", cacheKey, "*/*")
}

// handleRegistry serves an immutable registry tarball and refreshes the
// UUID→name map from its Registry.toml.
func (h *JuliaHandler) handleRegistry(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	hash := r.PathValue("hash")
	if !validJuliaUUID(uuid) || !juliaHexPattern.MatchString(hash) {
		http.Error(w, "invalid registry reference", http.StatusBadRequest)
		return
	}

	h.proxy.Logger.Debug("julia registry request", "uuid", uuid, "hash", hash)

	upstreamURL := h.upstreamURL + r.URL.Path
	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "julia", juliaRegistryName, hash, hash+".tar.gz", upstreamURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch registry")
		return
	}

	go h.refreshNamesFromRegistry(context.WithoutCancel(r.Context()), uuid, hash)

	ServeArtifactRequest(w, r, result)
}

// handlePackage serves an immutable package source tarball.
func (h *JuliaHandler) handlePackage(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	hash := r.PathValue("hash")
	if !validJuliaUUID(uuid) || !juliaHexPattern.MatchString(hash) {
		http.Error(w, "invalid package reference", http.StatusBadRequest)
		return
	}

	if err := h.ensureNames(r.Context()); err != nil {
		h.proxy.Logger.Warn("julia name map unavailable, using uuid", "error", err)
	}
	name := h.resolveName(uuid)

	h.proxy.Logger.Info("julia package request", "name", name, "uuid", uuid, "hash", hash)

	upstreamURL := h.upstreamURL + r.URL.Path
	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "julia", name, hash, hash+".tar.gz", upstreamURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch package")
		return
	}

	ServeArtifactRequest(w, r, result)
}

// handleArtifact serves an immutable binary artifact tarball. Artifacts are
// anonymous content-addressed blobs with no associated package name.
func (h *JuliaHandler) handleArtifact(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !juliaHexPattern.MatchString(hash) {
		http.Error(w, "invalid artifact hash", http.StatusBadRequest)
		return
	}

	h.proxy.Logger.Debug("julia artifact request", "hash", hash)

	upstreamURL := h.upstreamURL + r.URL.Path
	result, err := h.proxy.GetOrFetchArtifactFromURL(r.Context(), "julia", juliaArtifactName, hash, hash+".tar.gz", upstreamURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch artifact")
		return
	}

	ServeArtifactRequest(w, r, result)
}

// proxyUpstream forwards a request to the upstream Pkg server without caching.
func (h *JuliaHandler) proxyUpstream(w http.ResponseWriter, r *http.Request) {
	h.proxy.ProxyUpstream(w, r, h.upstreamURL+r.URL.Path, nil)
}

// resolveName returns the human-readable package name for a UUID, falling
// back to the UUID itself if it is not present in the loaded registry.
func (h *JuliaHandler) resolveName(uuid string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if name, ok := h.names[uuid]; ok {
		return name
	}
	return uuid
}

// ensureNames lazily populates the UUID→name map from the General registry.
// Returns immediately if the map is already populated; otherwise blocks until
// a single in-flight load completes. Failed loads are retried on the next call.
func (h *JuliaHandler) ensureNames(ctx context.Context) error {
	if h.namesLoaded() {
		return nil
	}

	h.loadMu.Lock()
	defer h.loadMu.Unlock()

	if h.namesLoaded() {
		return nil
	}
	return h.loadNamesFromUpstream(ctx)
}

func (h *JuliaHandler) namesLoaded() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.names) > 0
}

// loadNamesFromUpstream fetches the current /registries listing, downloads the
// General registry tarball at its current hash, and parses Registry.toml.
func (h *JuliaHandler) loadNamesFromUpstream(ctx context.Context) error {
	hash, err := h.fetchGeneralRegistryHash(ctx)
	if err != nil {
		return err
	}
	return h.loadRegistryTarball(ctx, juliaGeneralRegistryUUID, hash)
}

// fetchGeneralRegistryHash reads /registries, through the metadata cache that
// also serves it to clients, and returns the current tree hash for the
// General registry.
func (h *JuliaHandler) fetchGeneralRegistryHash(ctx context.Context) (string, error) {
	body, _, err := h.proxy.FetchOrCacheMetadata(ctx, "julia", "registries", h.upstreamURL+"/registries", "*/*")
	if err != nil {
		return "", fmt.Errorf("reading /registries: %w", err)
	}

	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		uuid, hash, ok := parseRegistryLine(scanner.Text())
		if ok && uuid == juliaGeneralRegistryUUID {
			return hash, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("general registry not listed in /registries")
}

// refreshNamesFromRegistry reloads the UUID→name map after a General registry
// tarball has been served. Only the hash /registries currently lists replaces
// the map, so requests for older snapshots neither roll it back nor trigger a
// download each. One reload runs at a time; a request that finds one running
// skips its own. Errors are logged but do not affect the response.
func (h *JuliaHandler) refreshNamesFromRegistry(ctx context.Context, uuid, hash string) {
	if uuid != juliaGeneralRegistryUUID || h.currentNamesHash() == hash {
		return
	}
	if !h.loadMu.TryLock() {
		return
	}
	defer h.loadMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, juliaRefreshTimeout)
	defer cancel()
	advertised, err := h.fetchGeneralRegistryHash(ctx)
	if err != nil {
		h.proxy.Logger.Warn("failed to refresh julia name map", "error", err)
		return
	}
	if advertised != hash || h.currentNamesHash() == hash {
		return
	}
	if err := h.loadRegistryTarball(ctx, uuid, hash); err != nil {
		h.proxy.Logger.Warn("failed to refresh julia name map", "error", err)
	}
}

func (h *JuliaHandler) currentNamesHash() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.namesHash
}

// loadRegistryTarball downloads a registry tarball and replaces the name map
// with the contents of its Registry.toml.
func (h *JuliaHandler) loadRegistryTarball(ctx context.Context, uuid, hash string) error {
	url := fmt.Sprintf("%s/registry/%s/%s", h.upstreamURL, uuid, hash)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := h.proxy.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream registry returned %d", resp.StatusCode)
	}

	names, err := extractRegistryNames(resp.Body)
	if err != nil {
		return err
	}

	h.mu.Lock()
	h.names = names
	h.namesHash = hash
	h.mu.Unlock()

	h.proxy.Logger.Info("loaded julia registry name map", "packages", len(names), "hash", hash)
	return nil
}

// extractRegistryNames reads a gzipped registry tarball, finds Registry.toml
// at the root, and returns its [packages] table as a UUID→name map.
func extractRegistryNames(r io.Reader) (map[string]string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("opening gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("no Registry.toml in tarball")
		}
		if err != nil {
			return nil, err
		}
		if strings.TrimPrefix(hdr.Name, "./") != "Registry.toml" {
			continue
		}

		data, err := io.ReadAll(io.LimitReader(tr, maxJuliaRegistryTomlSize+1))
		if err != nil {
			return nil, err
		}
		if len(data) > maxJuliaRegistryTomlSize {
			return nil, fmt.Errorf("registry file Registry.toml exceeds %d bytes", maxJuliaRegistryTomlSize)
		}
		return parseRegistryToml(data)
	}
}

type juliaRegistryFile struct {
	Packages map[string]struct {
		Name string `toml:"name"`
	} `toml:"packages"`
}

// parseRegistryToml decodes the [packages] table of a Registry.toml file.
func parseRegistryToml(data []byte) (map[string]string, error) {
	var reg juliaRegistryFile
	if _, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&reg); err != nil {
		return nil, fmt.Errorf("parsing Registry.toml: %w", err)
	}

	names := make(map[string]string, len(reg.Packages))
	for uuid, pkg := range reg.Packages {
		if pkg.Name != "" {
			names[uuid] = pkg.Name
		}
	}
	return names, nil
}

// parseRegistryLine parses a single line from /registries of the form
// "/registry/{uuid}/{hash}" and returns the uuid and hash.
func parseRegistryLine(line string) (uuid, hash string, ok bool) {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "/registry/")
	uuid, hash, found := strings.Cut(line, "/")
	if !found || !validJuliaUUID(uuid) || !juliaHexPattern.MatchString(hash) {
		return "", "", false
	}
	return uuid, hash, true
}

// validJuliaUUID reports whether s looks like a lowercase RFC 4122 UUID.
func validJuliaUUID(s string) bool {
	return juliaUUIDPattern.MatchString(s)
}
