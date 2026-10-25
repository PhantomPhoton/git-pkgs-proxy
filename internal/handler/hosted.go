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
	ErrHostedFileNotFound    = errors.New("no pushed file of this name for this version")

	// ErrHostedExists refuses a second push of the same file. Clients pin
	// pushed files by version and often by hash, so a published file is never
	// silently replaced; delete it first to push it again.
	ErrHostedExists = errors.New("this file was already pushed; delete it first to push it again")

	// ErrHostedChecksumMismatch reports a push whose bytes don't match the
	// sha256 the client sent along.
	ErrHostedChecksumMismatch = errors.New("pushed bytes don't match the expected sha256")

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

// hostedEcosystem is what an ecosystem defines for pushed packages. Storing,
// listing, deleting and the push rules are shared; adding an ecosystem means
// adding an entry here and, if its clients read version lists, listing pushed
// versions in its metadata.
type hostedEcosystem struct {
	// validName reports whether a package name, split on "/", is one of the
	// ecosystem's.
	validName func(segments []string) bool

	// filename is the name clients request the archive by, derived from the
	// package name and version. When nil, the filename is the last segment of
	// the push path, the package name must be a single segment so the path is
	// unambiguous, and a version can hold several files (see multiFile).
	filename func(name, version string) string

	// readManifest reads the package manifest out of a pushed archive and
	// checks that it describes target. When nil, pushes carry no manifest.
	readManifest func(archive io.Reader, target HostedTarget) (json.RawMessage, error)

	// hostedOnly reports whether a namespace (an npm scope, a Composer
	// vendor) is served from pushed packages alone. When nil, the ecosystem
	// has no such namespaces.
	hostedOnly func(p *Proxy, namespace string) bool
}

var hostedEcosystems = map[string]hostedEcosystem{
	"npm": {
		validName: func(segments []string) bool {
			return len(segments) == 1 || (len(segments) == 2 && strings.HasPrefix(segments[0], "@"))
		},
		filename:     func(name, version string) string { return lastSegment(name) + "-" + version + ".tgz" },
		readManifest: readNPMManifest,
		hostedOnly: func(p *Proxy, scope string) bool {
			return slices.Contains(p.HostedNPMScopes, scope)
		},
	},
	"composer": {
		validName:    func(segments []string) bool { return len(segments) == vendorPackageParts },
		filename:     func(name, version string) string { return lastSegment(name) + "-" + version + ".zip" },
		readManifest: readComposerManifest,
		hostedOnly: func(p *Proxy, vendor string) bool {
			return slices.ContainsFunc(p.HostedComposerVendors, func(v string) bool {
				return strings.EqualFold(v, vendor)
			})
		},
	},
	// generic holds files that aren't packages, such as prebuilt binaries.
	// They are served at /generic/hosted/{name}/{version}/{file} and never
	// come from an upstream.
	genericEcosystem: {
		validName: func(segments []string) bool { return len(segments) == 1 },
	},
}

// multiFile reports whether a version can hold several files: those named in
// the push path. Such versions get no version-level integrity, which could
// only describe one of them.
func (e hostedEcosystem) multiFile() bool {
	return e.filename == nil
}

// HostedSupported reports whether packages can be pushed for ecosystem.
func HostedSupported(ecosystem string) bool {
	_, ok := hostedEcosystems[ecosystem]
	return ok
}

// HostedFileInPath reports whether pushes for ecosystem name the file in the
// path, {name}/{version}/{file}, rather than deriving it from the name and
// version.
func HostedFileInPath(ecosystem string) bool {
	eco, ok := hostedEcosystems[ecosystem]
	return ok && eco.multiFile()
}

func lastSegment(name string) string {
	return name[strings.LastIndex(name, "/")+1:]
}

// HostedTargetFor validates a push of name@version to ecosystem and returns
// where it is stored. filename is required for ecosystems whose pushes name
// the file (see HostedFileInPath) and must be empty for the others.
func HostedTargetFor(ecosystem, name, version, filename string) (HostedTarget, error) {
	if err := ValidateHostedPath(ecosystem, name, version, filename); err != nil {
		return HostedTarget{}, err
	}
	eco := hostedEcosystems[ecosystem]
	if eco.filename != nil {
		filename = eco.filename(name, version)
	} else if filename == "" {
		return HostedTarget{}, ErrHostedInvalidName
	}
	return HostedTarget{Name: name, Version: version, Filename: filename}, nil
}

// ValidateHostedPath checks the parts of a hosted API path: a supported
// ecosystem, one of its package names, and path segments that are safe to
// store. filename may be empty, as in a delete of a whole version, but is
// only allowed for ecosystems whose pushes name the file.
func ValidateHostedPath(ecosystem, name, version, filename string) error {
	eco, ok := hostedEcosystems[ecosystem]
	if !ok {
		return fmt.Errorf("%w: %s", ErrHostedUnsupported, ecosystem)
	}
	segments := strings.Split(name, "/")
	if !eco.validName(segments) {
		return ErrHostedInvalidName
	}
	checked := append(slices.Clone(segments), version)
	if filename != "" {
		if !eco.multiFile() {
			return ErrHostedInvalidName
		}
		checked = append(checked, filename)
	}
	for _, segment := range checked {
		if !validHostedSegment(segment) {
			return ErrHostedInvalidName
		}
	}
	return nil
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
// A file that was already pushed is refused with ErrHostedExists. When
// wantSHA256 is set, the pushed bytes must hash to it. If the ecosystem has
// manifests, the archive's manifest must name target; it is kept so the
// ecosystem's metadata can list the version with its dependencies.
func (p *Proxy) PushLocalArtifact(ctx context.Context, ecosystem string, target HostedTarget, body io.Reader, contentType, wantSHA256 string) (HostedArtifact, error) {
	if err := p.CheckPush(ecosystem, target); err != nil {
		return HostedArtifact{}, err
	}
	pkgPURL, versionPURL, err := packagePURLStrings(ecosystem, target.Name, target.Version)
	if err != nil {
		return HostedArtifact{}, err
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

	if wantSHA256 != "" && !strings.EqualFold(wantSHA256, push.sha256) {
		p.discardStored(ctx, push.storagePath)
		return HostedArtifact{}, fmt.Errorf("%w: got %s", ErrHostedChecksumMismatch, push.sha256)
	}

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

// CheckPush refuses a push of a denied version or of a file that was already
// pushed, before any bytes are read, so a client can be told without
// uploading the file. For ecosystems with one file per version, any pushed
// file of the version counts, so a differently cased alias of the name is
// refused too. A cached upstream copy is fine to replace. Recording the push
// checks again atomically, for concurrent pushes.
func (p *Proxy) CheckPush(ecosystem string, target HostedTarget) error {
	_, versionPURL, err := packagePURLStrings(ecosystem, target.Name, target.Version)
	if err != nil {
		return err
	}
	if p.versionDenied(ecosystem, target.Name, target.Version) {
		return fmt.Errorf("%w: %s", ErrVersionDenied, versionPURL)
	}
	var existing []database.Artifact
	if hostedEcosystems[ecosystem].multiFile() {
		record, err := p.DB.GetArtifact(versionPURL, target.Filename)
		if err != nil {
			return fmt.Errorf("checking existing artifact: %w", err)
		}
		if record != nil {
			existing = append(existing, *record)
		}
	} else if existing, err = p.DB.GetArtifactsByVersionPURL(versionPURL); err != nil {
		return fmt.Errorf("checking existing artifacts: %w", err)
	}
	for _, a := range existing {
		if a.Source == database.SourceLocal {
			return ErrHostedExists
		}
	}
	return nil
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
	if hostedEcosystems[ecosystem].readManifest != nil {
		stored, err := p.Storage.Open(ctx, push.storagePath)
		if err != nil {
			return fmt.Errorf("reading pushed archive: %w", err)
		}
		push.manifest, err = readHostedManifest(ecosystem, stored, target)
		_ = stored.Close()
		if err != nil {
			return err
		}
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

// recordLocalArtifact writes the package, version and artifact records in
// one transaction. An existing package record is kept as it is, since it may
// describe the upstream package. For ecosystems with one file per version,
// the version's integrity is set to the pushed bytes, so the cache's
// integrity check verifies them rather than an upstream archive's digest.
// Multi-file versions can't have one integrity, so an existing version record
// is left alone and each file is checked against its own sha256.
func (p *Proxy) recordLocalArtifact(ecosystem string, target HostedTarget, pkgPURL, versionPURL string, push storedPush) error {
	oneFilePerVersion := !hostedEcosystems[ecosystem].multiFile()
	err := p.DB.RecordLocalArtifact(
		&database.Package{PURL: pkgPURL, Ecosystem: ecosystem, Name: target.Name},
		&database.Version{
			PURL:        versionPURL,
			PackagePURL: pkgPURL,
			Integrity:   sql.NullString{String: push.integrity, Valid: oneFilePerVersion},
		},
		oneFilePerVersion,
		&database.Artifact{
			VersionPURL: versionPURL,
			Filename:    target.Filename,
			StoragePath: sql.NullString{String: push.storagePath, Valid: true},
			ContentHash: sql.NullString{String: push.sha256, Valid: true},
			Size:        sql.NullInt64{Int64: push.size, Valid: true},
			ContentType: sql.NullString{String: push.contentType, Valid: push.contentType != ""},
			FetchedAt:   sql.NullTime{Time: push.pushedAt, Valid: true},
			Manifest:    sql.NullString{String: string(push.manifest), Valid: push.manifest != nil},
		})
	if errors.Is(err, database.ErrLocalArtifactExists) {
		return ErrHostedExists
	}
	if err != nil {
		return fmt.Errorf("recording pushed artifact: %w", err)
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
	eco := hostedEcosystems[ecosystem]
	return namespaced && eco.hostedOnly != nil && eco.hostedOnly(p, namespace)
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
	return p.deleteLocal(ecosystem, name, version, "", ErrHostedVersionNotFound)
}

// DeleteLocalFile removes one pushed file of name@version.
func (p *Proxy) DeleteLocalFile(ecosystem, name, version, filename string) error {
	return p.deleteLocal(ecosystem, name, version, filename, ErrHostedFileNotFound)
}

func (p *Proxy) deleteLocal(ecosystem, name, version, filename string, notFound error) error {
	_, versionPURL, err := packagePURLStrings(ecosystem, name, version)
	if err != nil {
		return err
	}
	deleted, err := p.DB.DeleteLocalArtifacts(versionPURL, filename)
	if err != nil {
		return fmt.Errorf("deleting local artifacts: %w", err)
	}
	if deleted == 0 {
		return notFound
	}
	return nil
}

// GetLocalArtifact serves a pushed artifact from storage, or returns nil
// when the artifact was not pushed. Handlers that must consult the upstream
// before serving a cached copy call it first, so pushed versions the upstream
// does not list are still served. Pushed artifacts are not a cache, so they
// are served even when StreamArtifacts is set.
//
// With hosted packages disabled it returns nil without a lookup, as
// localVersions does: pushes are then left out of metadata, so they are not
// served ahead of the upstream either.
func (p *Proxy) GetLocalArtifact(ctx context.Context, ecosystem, name, version, filename string) (*CacheResult, error) {
	if !p.HostedEnabled {
		return nil, nil
	}
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
	if p.Denylist.Denied(versionPURL) {
		return nil, fmt.Errorf("%w: %s", ErrVersionDenied, versionPURL)
	}
	result, err := p.readStored(ctx, pkgPURL, versionPURL, filename)
	if err != nil || result != nil {
		return result, err
	}
	return nil, p.localArtifactMiss(versionPURL, filename)
}
