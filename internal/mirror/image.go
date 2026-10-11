package mirror

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/git-pkgs/proxy/internal/handler"
)

// ImageMirrorer fetches container images into the cache.
type ImageMirrorer interface {
	MirrorImage(ctx context.Context, ref handler.ImageRef, platforms []handler.Platform) (handler.ImageResult, error)
}

// UnavailableImages refuses every image with the given reason.
type UnavailableImages struct {
	Reason string
}

func (u UnavailableImages) MirrorImage(context.Context, handler.ImageRef, []handler.Platform) (handler.ImageResult, error) {
	return handler.ImageResult{}, errors.New(u.Reason)
}

func isImageEcosystem(ecosystem string) bool {
	return ecosystem == "docker" || ecosystem == "oci"
}

// ParsePlatforms parses os/architecture[/variant] values.
func ParsePlatforms(values []string) ([]handler.Platform, error) {
	var platforms []handler.Platform
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			platform, err := handler.ParsePlatform(item)
			if err != nil {
				return nil, err
			}
			platforms = append(platforms, platform)
		}
	}
	return platforms, nil
}

// imageRef maps a docker or oci PURL to the image reference and platforms to
// mirror.
//
//   - pkg:docker/<namespace>/<name>@<tag or digest>, optionally with
//     repository_url=<registry host>[/<path>].
//   - pkg:oci/<name>@<digest>?repository_url=<host>/<repository>, optionally
//     with tag=<tag> and arch=<architecture>. OCI PURLs have no default
//     registry, so repository_url is required.
func imageRef(pv PackageVersion) (handler.ImageRef, []handler.Platform, error) {
	platforms, err := ParsePlatforms(pv.Platforms)
	if err != nil {
		return handler.ImageRef{}, nil, err
	}
	ref := handler.ImageRef{Name: pv.Name}
	if pv.Version != "" {
		ref.References = []string{pv.Version}
	}
	repositoryURL := pv.Qualifiers["repository_url"]
	if pv.Ecosystem == "oci" {
		if repositoryURL == "" {
			return ref, nil, errors.New("pkg:oci needs a repository_url qualifier")
		}
		if tag := pv.Qualifiers["tag"]; tag != "" && tag != pv.Version {
			ref.References = append(ref.References, tag)
		}
		if arch := pv.Qualifiers["arch"]; arch != "" && len(platforms) == 0 {
			platforms = []handler.Platform{{Architecture: arch}}
		}
	}
	if repositoryURL != "" {
		repositoryURL = strings.TrimPrefix(strings.TrimPrefix(repositoryURL, "https://"), "http://")
		host, repository, _ := strings.Cut(strings.Trim(repositoryURL, "/"), "/")
		ref.Registry = host
		switch {
		case pv.Ecosystem == "oci" && repository != "":
			// The repository URL names the repository itself.
			ref.Name = repository
		case repository != "":
			ref.Name = repository + "/" + pv.Name
		}
	}
	return ref, platforms, nil
}

func (m *Mirror) mirrorImage(ctx context.Context, pv PackageVersion, tracker *progressTracker) {
	fail := func(err error) {
		tracker.failed.Add(1)
		tracker.addError(pv.Ecosystem, pv.Name, pv.Version, err.Error())
		m.logger.Warn("mirror failed", "ecosystem", pv.Ecosystem, "name", pv.Name, "version", pv.Version, "error", err)
	}
	// pv.String keeps the namespace separator, which MakeString escapes for
	// docker names.
	if m.proxy != nil && m.proxy.Denylist.Denied(pv.String()) {
		fail(fmt.Errorf("%w: %s", handler.ErrVersionDenied, pv))
		return
	}
	ref, platforms, err := imageRef(pv)
	if err != nil {
		fail(err)
		return
	}
	result, err := m.Images.MirrorImage(ctx, ref, platforms)
	for _, warning := range result.Warnings {
		m.logger.Warn("mirror warning", "ecosystem", pv.Ecosystem, "name", pv.Name, "version", pv.Version, "warning", warning)
	}
	tracker.bytes.Add(result.Bytes)
	if err != nil {
		fail(err)
		return
	}
	if result.Fetched == 0 {
		tracker.skipped.Add(1)
		m.logger.Debug("already cached", "ecosystem", pv.Ecosystem, "name", pv.Name, "version", pv.Version)
		return
	}
	tracker.completed.Add(1)
	m.logger.Info("mirrored", "ecosystem", pv.Ecosystem, "name", pv.Name, "version", pv.Version, "size", result.Bytes)
}
