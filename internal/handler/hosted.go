package handler

import (
	"context"
	"crypto/sha512"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/proxy/internal/packageurl"
	"github.com/git-pkgs/proxy/internal/storage"
)

var (
	ErrHostedUnsupported     = errors.New("pushing packages is not supported for this ecosystem")
	ErrHostedInvalidName     = errors.New("invalid package name or version")
	ErrHostedVersionNotFound = errors.New("no pushed artifacts for this version")

	// ErrHostedArtifactUnavailable and ErrHostedArtifactMissing report a
	// pushed artifact the cache can't serve. They stop the caller from
	// fetching the upstream copy, which would replace the pushed one.
	ErrHostedArtifactUnavailable = errors.New("pushed artifact is stored but can't be read")
	ErrHostedArtifactMissing     = errors.New("pushed artifact is no longer stored")
)

// HostedTarget is where a pushed archive is stored: the package name and
// version it is cached under and the filename clients request it by. Download
// handlers look artifacts up by exactly these values, so a push under any
// other name would never be served.
type HostedTarget struct {
	Name     string
	Version  string
	Filename string
}

// HostedArtifact describes one pushed archive.
type HostedArtifact struct {
	Version  string    `json:"version"`
	Filename string    `json:"filename"`
	SHA256   string    `json:"sha256"`
	Size     int64     `json:"size"`
	PushedAt time.Time `json:"pushed_at"`
}

// hostedNaming describes how an ecosystem names pushed archives: which
// package names it has, split on "/", and the filename its clients request.
type hostedNaming struct {
	validName func(segments []string) bool
	filename  func(name, version string) string
}

var hostedNamings = map[string]hostedNaming{
	"npm": {
		validName: func(segments []string) bool {
			return len(segments) == 1 || (len(segments) == 2 && strings.HasPrefix(segments[0], "@"))
		},
		filename: func(name, version string) string { return lastSegment(name) + "-" + version + ".tgz" },
	},
	"composer": {
		validName: func(segments []string) bool { return len(segments) == vendorPackageParts },
		filename:  func(name, version string) string { return lastSegment(name) + "-" + version + ".zip" },
	},
}

// HostedSupported reports whether packages can be pushed for ecosystem.
func HostedSupported(ecosystem string) bool {
	_, ok := hostedNamings[ecosystem]
	return ok
}

func lastSegment(name string) string {
	return name[strings.LastIndex(name, "/")+1:]
}

// HostedTargetFor validates a push of name@version to ecosystem and returns
// where it is stored.
func HostedTargetFor(ecosystem, name, version string) (HostedTarget, error) {
	naming, ok := hostedNamings[ecosystem]
	if !ok {
		return HostedTarget{}, fmt.Errorf("%w: %s", ErrHostedUnsupported, ecosystem)
	}
	segments := strings.Split(name, "/")
	if !naming.validName(segments) {
		return HostedTarget{}, ErrHostedInvalidName
	}
	for _, segment := range append(segments, version) {
		if !validHostedSegment(segment) {
			return HostedTarget{}, ErrHostedInvalidName
		}
	}
	return HostedTarget{Name: name, Version: version, Filename: naming.filename(name, version)}, nil
}

func validHostedSegment(s string) bool {
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `/\`) {
		return false
	}
	for _, c := range s {
		if c < ' ' || c == 0x7f {
			return false
		}
	}
	return true
}

// PushLocalArtifact stores body as the artifact for target and records it as
// local, replacing any cached upstream copy. Download handlers serve it from
// the cache without contacting the upstream, and eviction never removes it.
// The archive's manifest must name target; it is kept so the ecosystem's
// metadata can list the version with its dependencies.
func (p *Proxy) PushLocalArtifact(ctx context.Context, ecosystem string, target HostedTarget, body io.Reader, contentType string) (HostedArtifact, error) {
	pkgPURL, versionPURL, err := packagePURLStrings(ecosystem, target.Name, target.Version)
	if err != nil {
		return HostedArtifact{}, err
	}
	if p.versionDenied(ecosystem, target.Name, target.Version) {
		return HostedArtifact{}, fmt.Errorf("%w: %s", ErrVersionDenied, versionPURL)
	}

	push := storedPush{
		storagePath: storage.FetchPath(ecosystem, target.Name, target.Version, storage.NewFetchID(), target.Filename),
		contentType: contentType,
	}
	sha512Hash := sha512.New()
	storeStart := time.Now()
	push.size, push.sha256, err = p.Storage.Store(ctx, push.storagePath, io.TeeReader(body, sha512Hash))
	metrics.RecordStorageOperation("write", time.Since(storeStart))
	if err != nil {
		metrics.RecordStorageError("write", metrics.FailureDetail{
			PURL: versionPURL, Filename: target.Filename, Path: push.storagePath, Err: err})
		return HostedArtifact{}, fmt.Errorf("storing artifact: %w", err)
	}
	push.integrity = "sha512-" + base64.StdEncoding.EncodeToString(sha512Hash.Sum(nil))

	if err := p.checkPush(ctx, ecosystem, target, versionPURL, &push); err != nil {
		p.discardStored(ctx, push.storagePath)
		return HostedArtifact{}, err
	}

	push.pushedAt = time.Now()
	if err := p.recordLocalArtifact(ecosystem, target, pkgPURL, versionPURL, push); err != nil {
		p.discardStored(ctx, push.storagePath)
		return HostedArtifact{}, err
	}

	return HostedArtifact{
		Version:  target.Version,
		Filename: target.Filename,
		SHA256:   push.sha256,
		Size:     push.size,
		PushedAt: push.pushedAt.UTC(),
	}, nil
}

// storedPush is a pushed archive once it is in storage.
type storedPush struct {
	storagePath string
	sha256      string
	integrity   string
	size        int64
	contentType string
	manifest    json.RawMessage
	pushedAt    time.Time
}

// checkPush reads the manifest of a stored push and runs the scanners on it.
func (p *Proxy) checkPush(ctx context.Context, ecosystem string, target HostedTarget, versionPURL string, push *storedPush) error {
	stored, err := p.Storage.Open(ctx, push.storagePath)
	if err != nil {
		return fmt.Errorf("reading pushed archive: %w", err)
	}
	push.manifest, err = readHostedManifest(ecosystem, stored, target)
	_ = stored.Close()
	if err != nil {
		return err
	}

	if p.Scanners != nil && p.Scanners.Enabled() {
		return p.runScan(ctx, ecosystem, target.Name, target.Version, target.Filename, versionPURL, push.storagePath, push.size, push.contentType)
	}
	return nil
}

func (p *Proxy) discardStored(ctx context.Context, storagePath string) {
	if err := p.Storage.Delete(context.WithoutCancel(ctx), storagePath); err != nil {
		p.Logger.Warn("failed to delete pushed artifact from storage", "path", storagePath, "error", err)
	}
}

// recordLocalArtifact writes the package, version and artifact records. The
// version's integrity is set to the pushed bytes, so the cache's integrity
// check verifies them rather than an upstream archive's digest. An existing
// package record is kept as it is, since it may describe the upstream package.
func (p *Proxy) recordLocalArtifact(ecosystem string, target HostedTarget, pkgPURL, versionPURL string, push storedPush) error {
	existing, err := p.DB.GetPackageByPURL(pkgPURL)
	if err != nil {
		return fmt.Errorf("reading package: %w", err)
	}
	if existing == nil {
		if err := p.DB.UpsertPackage(&database.Package{PURL: pkgPURL, Ecosystem: ecosystem, Name: target.Name}); err != nil {
			return fmt.Errorf("upserting package: %w", err)
		}
	}

	if err := p.DB.UpsertVersion(&database.Version{
		PURL:        versionPURL,
		PackagePURL: pkgPURL,
		Integrity:   sql.NullString{String: push.integrity, Valid: true},
	}); err != nil {
		return fmt.Errorf("upserting version: %w", err)
	}

	if err := p.DB.UpsertArtifact(&database.Artifact{
		VersionPURL: versionPURL,
		Filename:    target.Filename,
		StoragePath: sql.NullString{String: push.storagePath, Valid: true},
		ContentHash: sql.NullString{String: push.sha256, Valid: true},
		Size:        sql.NullInt64{Int64: push.size, Valid: true},
		ContentType: sql.NullString{String: push.contentType, Valid: push.contentType != ""},
		FetchedAt:   sql.NullTime{Time: push.pushedAt, Valid: true},
		Source:      database.SourceLocal,
		Manifest:    sql.NullString{String: string(push.manifest), Valid: true},
	}); err != nil {
		return fmt.Errorf("upserting artifact: %w", err)
	}
	return nil
}

// LocalArtifacts lists the archives pushed for a package.
func (p *Proxy) LocalArtifacts(ecosystem, name string) ([]HostedArtifact, error) {
	pkgPURL := packageurl.MakeString(ecosystem, name, "")
	if pkgPURL == "" {
		return nil, fmt.Errorf("%w: %s %q", errUnsupportedPackageIdentity, ecosystem, name)
	}
	rows, err := p.DB.GetLocalArtifacts(pkgPURL)
	if err != nil {
		return nil, fmt.Errorf("listing local artifacts: %w", err)
	}
	listed := make([]HostedArtifact, 0, len(rows))
	for _, row := range rows {
		listed = append(listed, HostedArtifact{
			Version:  database.VersionFromPURL(row.VersionPURL),
			Filename: row.Filename,
			SHA256:   row.ContentHash.String,
			Size:     row.Size.Int64,
			PushedAt: row.FetchedAt.Time.UTC(),
		})
	}
	return listed, nil
}

// hostedOnly reports whether a package is in an npm scope or Composer vendor
// that is served from pushed packages alone. The upstream is never asked
// about such a package, so nobody can mix in versions by publishing the same
// name there.
func (p *Proxy) hostedOnly(ecosystem, name string) bool {
	if !p.HostedEnabled {
		return false
	}
	namespace, _, namespaced := strings.Cut(name, "/")
	if !namespaced {
		return false
	}
	switch ecosystem {
	case "npm":
		return slices.Contains(p.HostedNPMScopes, namespace)
	case "composer":
		return slices.ContainsFunc(p.HostedComposerVendors, func(vendor string) bool {
			return strings.EqualFold(vendor, namespace)
		})
	default:
		return false
	}
}

// localVersion is a pushed version as ecosystem metadata lists it.
type localVersion struct {
	Version   string
	Filename  string
	SHA256    string
	Integrity string
	Manifest  json.RawMessage
	PushedAt  time.Time
}

// localVersions returns the versions pushed for a package, or nil when hosted
// packages are disabled, so metadata requests cost no extra lookup then.
// Denied versions are left out, so metadata never lists a version whose
// download is refused.
func (p *Proxy) localVersions(ecosystem, name string) ([]localVersion, error) {
	if !p.HostedEnabled {
		return nil, nil
	}
	pkgPURL := packageurl.MakeString(ecosystem, name, "")
	if pkgPURL == "" {
		return nil, nil
	}
	rows, err := p.DB.GetLocalArtifacts(pkgPURL)
	if err != nil {
		return nil, fmt.Errorf("listing local versions: %w", err)
	}
	versions := make([]localVersion, 0, len(rows))
	for _, row := range rows {
		version := database.VersionFromPURL(row.VersionPURL)
		if !row.Manifest.Valid || p.versionDenied(ecosystem, name, version) {
			continue
		}
		versions = append(versions, localVersion{
			Version:   version,
			Filename:  row.Filename,
			SHA256:    row.ContentHash.String,
			Integrity: row.Integrity.String,
			Manifest:  json.RawMessage(row.Manifest.String),
			PushedAt:  row.FetchedAt.Time.UTC(),
		})
	}
	return versions, nil
}

// DeleteLocalVersion removes the archives pushed for name@version. If the
// upstream has the version, clients get the upstream archive again.
func (p *Proxy) DeleteLocalVersion(ecosystem, name, version string) error {
	_, versionPURL, err := packagePURLStrings(ecosystem, name, version)
	if err != nil {
		return err
	}
	deleted, err := p.DB.DeleteLocalArtifacts(versionPURL)
	if err != nil {
		return fmt.Errorf("deleting local artifacts: %w", err)
	}
	if deleted == 0 {
		return ErrHostedVersionNotFound
	}
	return nil
}

// GetLocalArtifact serves a pushed artifact from the cache, or returns nil
// when the artifact was not pushed. Handlers that must consult the upstream
// before serving a cached copy call it first, so pushed versions the upstream
// does not list are still served.
func (p *Proxy) GetLocalArtifact(ctx context.Context, ecosystem, name, version, filename string) (*CacheResult, error) {
	pkgPURL, versionPURL, err := packagePURLStrings(ecosystem, name, version)
	if err != nil {
		return nil, err
	}
	record, err := p.DB.GetArtifact(versionPURL, filename)
	if err != nil {
		return nil, fmt.Errorf("checking artifact source: %w", err)
	}
	if record == nil || record.Source != database.SourceLocal {
		return nil, nil
	}
	return p.checkCache(ctx, pkgPURL, versionPURL, filename)
}
