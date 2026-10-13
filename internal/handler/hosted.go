package handler

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
func (p *Proxy) PushLocalArtifact(ctx context.Context, ecosystem string, target HostedTarget, body io.Reader, contentType string) (HostedArtifact, error) {
	pkgPURL, versionPURL, err := packagePURLStrings(ecosystem, target.Name, target.Version)
	if err != nil {
		return HostedArtifact{}, err
	}
	if p.versionDenied(ecosystem, target.Name, target.Version) {
		return HostedArtifact{}, fmt.Errorf("%w: %s", ErrVersionDenied, versionPURL)
	}

	storagePath := storage.FetchPath(ecosystem, target.Name, target.Version, storage.NewFetchID(), target.Filename)
	storeStart := time.Now()
	size, hash, err := p.Storage.Store(ctx, storagePath, body)
	metrics.RecordStorageOperation("write", time.Since(storeStart))
	if err != nil {
		metrics.RecordStorageError("write", metrics.FailureDetail{
			PURL: versionPURL, Filename: target.Filename, Path: storagePath, Err: err})
		return HostedArtifact{}, fmt.Errorf("storing artifact: %w", err)
	}

	if p.Scanners != nil && p.Scanners.Enabled() {
		if err := p.runScan(ctx, ecosystem, target.Name, target.Version, target.Filename, versionPURL, storagePath, size, contentType); err != nil {
			p.discardStored(ctx, storagePath)
			return HostedArtifact{}, err
		}
	}

	pushedAt := time.Now()
	if err := p.recordLocalArtifact(ecosystem, target, pkgPURL, versionPURL, storagePath, hash, size, contentType, pushedAt); err != nil {
		p.discardStored(ctx, storagePath)
		return HostedArtifact{}, err
	}

	return HostedArtifact{
		Version:  target.Version,
		Filename: target.Filename,
		SHA256:   hash,
		Size:     size,
		PushedAt: pushedAt.UTC(),
	}, nil
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
func (p *Proxy) recordLocalArtifact(ecosystem string, target HostedTarget, pkgPURL, versionPURL, storagePath, hash string, size int64, contentType string, pushedAt time.Time) error {
	existing, err := p.DB.GetPackageByPURL(pkgPURL)
	if err != nil {
		return fmt.Errorf("reading package: %w", err)
	}
	if existing == nil {
		if err := p.DB.UpsertPackage(&database.Package{PURL: pkgPURL, Ecosystem: ecosystem, Name: target.Name}); err != nil {
			return fmt.Errorf("upserting package: %w", err)
		}
	}

	sri, err := hexDigestSRI(hash)
	if err != nil {
		return err
	}
	if err := p.DB.UpsertVersion(&database.Version{
		PURL:        versionPURL,
		PackagePURL: pkgPURL,
		Integrity:   sql.NullString{String: sri, Valid: true},
	}); err != nil {
		return fmt.Errorf("upserting version: %w", err)
	}

	if err := p.DB.UpsertArtifact(&database.Artifact{
		VersionPURL: versionPURL,
		Filename:    target.Filename,
		StoragePath: sql.NullString{String: storagePath, Valid: true},
		ContentHash: sql.NullString{String: hash, Valid: true},
		Size:        sql.NullInt64{Int64: size, Valid: true},
		ContentType: sql.NullString{String: contentType, Valid: contentType != ""},
		FetchedAt:   sql.NullTime{Time: pushedAt, Valid: true},
		Source:      database.SourceLocal,
	}); err != nil {
		return fmt.Errorf("upserting artifact: %w", err)
	}
	return nil
}

func hexDigestSRI(hexDigest string) (string, error) {
	raw, err := hex.DecodeString(hexDigest)
	if err != nil {
		return "", fmt.Errorf("decoding stored digest: %w", err)
	}
	return "sha256-" + base64.StdEncoding.EncodeToString(raw), nil
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
