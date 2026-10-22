package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"

	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/purl"
)

const (
	freebsdEcosystem = "freebsd"
	// defaultFreeBSDRepositoryName is the repository name used when no
	// upstream.freebsd repositories are configured.
	defaultFreeBSDRepositoryName = "freebsd"
	// defaultFreeBSDUpstream is the official FreeBSD package mirror.
	defaultFreeBSDUpstream = "https://pkg.freebsd.org"
	freebsdNamespace       = "freebsd"
	freebsdMatchCount      = 3 // full match + name + version
	freebsdHashedDir       = "Hashed"
	freebsdPackageMedia    = "application/octet-stream"
)

// Packages are stored as pkg:bsd/freebsd/{name}, whichever configured
// repository they came from.
func init() {
	retention.Default.Register(retention.Spec{Key: freebsdEcosystem, CanonicalPackage: freebsdRetentionKey})
}

// freebsdRetentionKey maps pkg:bsd/freebsd/{name} to the stored package PURL.
// Another namespace, or a name with a slash, can never match a stored package
// and is refused.
func freebsdRetentionKey(p *purl.PURL) (string, bool) {
	if p == nil || p.Type != "bsd" || p.Namespace != freebsdNamespace || strings.Contains(p.Name, "/") {
		return "", false
	}
	canonical := purl.MakePURLString(freebsdEcosystem, p.Name, "")
	return canonical, canonical != ""
}

// FreeBSDHandler serves FreeBSD pkg repositories, ports and pkgbase alike.
// Each configured upstream is mounted at /freebsd/{repository}/ and the rest
// of the path mirrors the upstream layout ({ABI}/{repo}/{file}).
//
// Repository catalogues (meta.conf, packagesite.pkg, data.pkg) carry their
// signature inside the archive, so they are served byte for byte through the
// metadata cache. Packages under Hashed/ have the content hash in their name
// and never change, so they go to the artifact cache. Everything else,
// including the unversioned bootstrap files under Latest/ whose detached
// signature is replaced on every repository build, is passed through.
type FreeBSDHandler struct {
	proxy        *Proxy
	proxyURL     string
	repositories map[string]string
}

// NewFreeBSDHandler creates a FreeBSD pkg repository handler. Without
// repositories it serves the official mirror as "freebsd".
func NewFreeBSDHandler(proxy *Proxy, proxyURL string, repositories map[string]string) *FreeBSDHandler {
	h := &FreeBSDHandler{
		proxy:        proxy,
		proxyURL:     strings.TrimSuffix(proxyURL, "/"),
		repositories: make(map[string]string, len(repositories)),
	}
	for name, repositoryURL := range repositories {
		h.repositories[name] = strings.TrimSuffix(repositoryURL, "/")
	}
	if len(h.repositories) == 0 {
		h.repositories[defaultFreeBSDRepositoryName] = defaultFreeBSDUpstream
	}
	return h
}

// Routes returns the HTTP handler for pkg requests.
// Mount this at /freebsd on your router.
func (h *FreeBSDHandler) Routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/")

		// A decoded '?' or '#' would cut the upstream path short and cache one
		// file under another file's name.
		if containsPathTraversal(path) || strings.ContainsAny(path, "?#") {
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
		case isFreeBSDCatalogue(rest):
			h.handleMetadata(w, r, repository, upstreamURL, rest)
		case isFreeBSDHashedPackage(rest):
			h.handlePackageDownload(w, r, repository, upstreamURL, rest)
		default:
			h.proxyFile(w, r, upstreamURL, rest)
		}
	})
}

// freebsdCataloguePattern matches the files pkg reads to update a repository:
// meta and meta.conf, and the packagesite and data catalogues in the current
// (.pkg) and older (.txz, .tzst) packing formats.
var freebsdCataloguePattern = regexp.MustCompile(`^(?:meta(?:\.conf|\.txz|\.tzst|\.pkg)?|(?:packagesite|data)\.(?:pkg|txz|tzst))$`)

func freebsdBase(path string) string {
	return path[strings.LastIndex(path, "/")+1:]
}

// isFreeBSDCatalogue reports whether path names a repository catalogue. The
// depth isn't checked, so repositories built with poudriere and served under
// another layout work too; package files are always {name}-{version}.pkg and
// can't take these names.
func isFreeBSDCatalogue(path string) bool {
	return freebsdCataloguePattern.MatchString(freebsdBase(path))
}

// freebsdHashedPattern matches {name}-{version}~{hash}.pkg. Port versions
// never contain '-', so the version is what follows the last one, e.g.
// zsh-navigation-tools-2.2.7~2$imxi7k7w.pkg or
// FreeBSD-syslogd-man-14.snap20261019150821~2$k6xtent4.pkg.
var freebsdHashedPattern = regexp.MustCompile(`^(.+)-([^-~/]+)~[^~/]+\.pkg$`)

// isFreeBSDHashedPackage reports whether path names a package in a Hashed/
// directory: All/Hashed/ for ports, Hashed/ for pkgbase.
func isFreeBSDHashedPackage(path string) bool {
	dir := strings.TrimSuffix(path, "/"+freebsdBase(path))
	return freebsdBase(dir) == freebsdHashedDir && freebsdHashedPattern.MatchString(freebsdBase(path))
}

// parseFreeBSDPackage returns the name and version of a hashed package file.
func parseFreeBSDPackage(filename string) (name, version string) {
	matches := freebsdHashedPattern.FindStringSubmatch(filename)
	if len(matches) != freebsdMatchCount {
		return "", ""
	}
	return matches[1], matches[2]
}

// handlePackageDownload caches hashed packages. The cache key holds the
// repository and full path, since every ABI and repository has its own
// builds.
func (h *FreeBSDHandler) handlePackageDownload(w http.ResponseWriter, r *http.Request, repository, upstreamURL, path string) {
	name, version := parseFreeBSDPackage(freebsdBase(path))
	if name == "" {
		h.proxyFile(w, r, upstreamURL, path)
		return
	}

	downloadURL := upstreamURL + "/" + path
	cacheFilename := repository + "/" + path

	h.proxy.Logger.Info("freebsd package download",
		"repository", repository, "name", name, "version", version)

	result, err := h.proxy.GetOrFetchArtifactFromURL(
		r.Context(), freebsdEcosystem, name, version, cacheFilename, downloadURL)
	if err != nil {
		h.proxy.serveArtifactError(w, err, "failed to fetch package")
		return
	}

	if result.Artifact.MediaType == "" {
		result.Artifact.MediaType = freebsdPackageMedia
	}
	ServeArtifactRequest(w, r, result)
}

// handleMetadata serves catalogues unchanged through the metadata cache,
// which keeps their embedded signatures valid.
func (h *FreeBSDHandler) handleMetadata(w http.ResponseWriter, r *http.Request, repository, upstreamURL, path string) {
	h.proxy.ProxyCached(w, r, upstreamURL+"/"+path, freebsdEcosystem,
		h.metadataCacheKey(repository, upstreamURL, path), "*/*")
}

// metadataCacheKey hashes repository, upstream URL and path, as APKHandler does.
func (h *FreeBSDHandler) metadataCacheKey(repository, upstreamURL, path string) string {
	identity := repository + "\x00" + upstreamURL + "\x00" + path
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

// proxyFile passes other files through uncached.
func (h *FreeBSDHandler) proxyFile(w http.ResponseWriter, r *http.Request, upstreamURL, path string) {
	h.proxy.ProxyFile(w, r, upstreamURL+"/"+path)
}
