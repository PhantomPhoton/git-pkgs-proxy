package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Masterminds/semver/v3"
)

// npmLocalEntry is a pushed version as a packument lists it: the version
// entry built from its package.json, and the time it was pushed.
type npmLocalEntry struct {
	version string
	entry   json.RawMessage
	time    string
}

// npmLocalEntries returns the pushed versions of a package as packument
// entries, or nil when there are none or hosted packages are disabled.
func (h *NPMHandler) npmLocalEntries(packageName string) ([]npmLocalEntry, error) {
	local, err := h.proxy.localVersions("npm", packageName)
	if err != nil || len(local) == 0 {
		return nil, err
	}
	entries := make([]npmLocalEntry, 0, len(local))
	for _, lv := range local {
		var manifest map[string]any
		if err := json.Unmarshal(lv.Manifest, &manifest); err != nil {
			return nil, fmt.Errorf("decoding manifest of %s@%s: %w", packageName, lv.Version, err)
		}
		manifest["name"] = packageName
		manifest["version"] = lv.Version
		manifest["_id"] = packageName + "@" + lv.Version
		manifest["dist"] = map[string]string{
			"tarball":   h.proxyTarballURL(packageName, lv.Version, lv.Filename),
			"integrity": lv.Integrity,
		}
		entry, err := json.Marshal(manifest)
		if err != nil {
			return nil, err
		}
		entries = append(entries, npmLocalEntry{version: lv.Version, entry: entry, time: lv.PushedAt.Format(time.RFC3339)})
	}
	return entries, nil
}

func findNPMLocalEntry(local []npmLocalEntry, version string) (npmLocalEntry, bool) {
	for _, e := range local {
		if e.version == version {
			return e, true
		}
	}
	return npmLocalEntry{}, false
}

// serveMetadataWithLocal serves a packument with the pushed versions of the
// package added. It bypasses the rewrite cache, whose entries are keyed by the
// upstream document alone, so a push or delete shows up on the next request.
// When the upstream does not know the package, the packument holds only the
// pushed versions.
func (h *NPMHandler) serveMetadataWithLocal(w http.ResponseWriter, r *http.Request, packageName, upstreamURL, accept, metadataCacheKey string, local []npmLocalEntry) {
	upstream := h.proxy.fetchMetadataDocument(r.Context(), "npm", metadataCacheKey, upstreamURL, accept)
	var out []byte
	var err error
	switch {
	case errors.Is(upstream.err, ErrUpstreamNotFound):
		out, err = localNPMPackument(packageName, local)
	case upstream.err != nil:
		h.proxy.Logger.Error("failed to fetch npm metadata", "error", upstream.err)
		JSONError(w, http.StatusBadGateway, "failed to fetch from upstream")
		return
	default:
		out, _, err = h.rewriteMetadataWithLocal(packageName, upstream.body, local)
	}
	if err != nil {
		if errors.Is(err, ErrVersionDenied) {
			JSONError(w, http.StatusForbidden, err.Error())
			return
		}
		h.proxy.Logger.Error("failed to add pushed versions to npm metadata", "package", packageName, "error", err)
		JSONError(w, http.StatusBadGateway, "failed to rewrite package metadata")
		return
	}
	serveRewrittenMetadata(w, r, h.proxy.uncachedRewrite("npm", h.proxyURL, metadataCacheKey, out))
}

// localNPMPackument builds a packument from pushed versions alone. latest is
// the highest of them, or the last pushed when a version is not semver.
func localNPMPackument(packageName string, local []npmLocalEntry) ([]byte, error) {
	versions := make(map[string]json.RawMessage, len(local))
	times := make(map[string]string, len(local))
	latest, latestTime := "", ""
	var latestSemver *semver.Version
	semverOnly := true
	for _, e := range local {
		versions[e.version] = e.entry
		times[e.version] = e.time
		if e.time > latestTime {
			latest, latestTime = e.version, e.time
		}
		v, err := semver.NewVersion(e.version)
		if err != nil {
			semverOnly = false
			continue
		}
		if latestSemver == nil || v.GreaterThan(latestSemver) {
			latestSemver = v
		}
	}
	if semverOnly && latestSemver != nil {
		latest = latestSemver.Original()
	}
	return json.Marshal(map[string]any{
		"name":      packageName,
		"dist-tags": map[string]string{"latest": latest},
		"versions":  versions,
		"time":      times,
	})
}
