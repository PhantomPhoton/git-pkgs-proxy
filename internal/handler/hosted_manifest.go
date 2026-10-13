package handler

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

var (
	ErrHostedInvalidArchive   = errors.New("archive has no readable package manifest")
	ErrHostedManifestMismatch = errors.New("package manifest does not match the pushed name and version")
)

// maxManifestSize bounds how much of a manifest file is read. Real manifests
// are a few kilobytes; the bound keeps a hostile archive from claiming memory.
const maxManifestSize = 1 << 20

// readHostedManifest reads the package manifest out of a pushed archive and
// checks that it describes target. The manifest is what the ecosystem's
// metadata lists the version with, so its dependencies reach the client.
func readHostedManifest(ecosystem string, archive io.Reader, target HostedTarget) (json.RawMessage, error) {
	switch ecosystem {
	case "npm":
		return readNPMManifest(archive, target)
	case "composer":
		return readComposerManifest(archive, target)
	default:
		return nil, fmt.Errorf("%w: %s", ErrHostedUnsupported, ecosystem)
	}
}

// readNPMManifest reads package.json from the top directory of an npm
// tarball. npm packs it under package/, but published tarballs use other
// directory names too.
func readNPMManifest(archive io.Reader, target HostedTarget) (json.RawMessage, error) {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHostedInvalidArchive, err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err != nil {
			return nil, fmt.Errorf("%w: no package.json in tarball", ErrHostedInvalidArchive)
		}
		if header.Typeflag != tar.TypeReg || !isTopLevelManifest(header.Name, "package.json") {
			continue
		}
		manifest, fields, err := decodeManifest(tr)
		if err != nil {
			return nil, err
		}
		if fields.Name != target.Name || fields.Version != target.Version {
			return nil, fmt.Errorf("%w: package.json is %s@%s", ErrHostedManifestMismatch, fields.Name, fields.Version)
		}
		return manifest, nil
	}
}

// readComposerManifest reads composer.json from the root of a Composer dist
// zip, or from its single top directory as GitHub zipballs have it. Package
// names are case-insensitive, and composer.json only names a version when
// the package pins one.
func readComposerManifest(archive io.Reader, target HostedTarget) (json.RawMessage, error) {
	file, err := os.CreateTemp("", "hosted-composer-*.zip")
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}()
	size, err := io.Copy(file, archive)
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(file, size)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHostedInvalidArchive, err)
	}

	entry, err := composerManifestEntry(zr.File)
	if err != nil {
		return nil, err
	}
	rc, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHostedInvalidArchive, err)
	}
	manifest, fields, err := decodeManifest(rc)
	_ = rc.Close()
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(fields.Name, target.Name) || (fields.Version != "" && fields.Version != target.Version) {
		return nil, fmt.Errorf("%w: composer.json is %s %s", ErrHostedManifestMismatch, fields.Name, fields.Version)
	}
	return manifest, nil
}

// composerManifestEntry picks the package's composer.json: the one at the
// root, or else the only one directly inside a top directory. Archives keep
// other composer.json files for tests and examples, in any order, so the
// first one found isn't necessarily the package's.
func composerManifestEntry(files []*zip.File) (*zip.File, error) {
	var topLevel []*zip.File
	for _, entry := range files {
		if entry.FileInfo().IsDir() {
			continue
		}
		if entry.Name == "composer.json" {
			return entry, nil
		}
		if isTopLevelManifest(entry.Name, "composer.json") {
			topLevel = append(topLevel, entry)
		}
	}
	switch len(topLevel) {
	case 0:
		return nil, fmt.Errorf("%w: no composer.json in zip", ErrHostedInvalidArchive)
	case 1:
		return topLevel[0], nil
	default:
		return nil, fmt.Errorf("%w: no composer.json at the root and %d in top directories", ErrHostedInvalidArchive, len(topLevel))
	}
}

// isTopLevelManifest reports whether name is filename inside one top
// directory, such as package/package.json.
func isTopLevelManifest(name, filename string) bool {
	dir, file := path.Split(path.Clean(name))
	return file == filename && dir != "" && !strings.Contains(strings.TrimSuffix(dir, "/"), "/")
}

type manifestFields struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// decodeManifest reads a JSON object manifest and returns it compacted along
// with its name and version.
func decodeManifest(r io.Reader) (json.RawMessage, manifestFields, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxManifestSize+1))
	if err != nil {
		return nil, manifestFields{}, fmt.Errorf("%w: %w", ErrHostedInvalidArchive, err)
	}
	if len(raw) > maxManifestSize {
		return nil, manifestFields{}, fmt.Errorf("%w: manifest exceeds %d bytes", ErrHostedInvalidArchive, maxManifestSize)
	}
	var fields manifestFields
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, manifestFields{}, fmt.Errorf("%w: manifest is not a JSON object: %w", ErrHostedInvalidArchive, err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, manifestFields{}, fmt.Errorf("%w: %w", ErrHostedInvalidArchive, err)
	}
	return compact.Bytes(), fields, nil
}
