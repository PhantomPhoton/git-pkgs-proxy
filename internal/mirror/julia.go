package mirror

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/registries"
)

// PackageFinder finds a package's registry entry, which for Julia gives the
// UUID the package is registered under.
type PackageFinder interface {
	FindPackage(ctx context.Context, ecosystem, name string) (*registries.Package, error)
}

// FindPackage implements PackageFinder.
func (r RegistryVersions) FindPackage(ctx context.Context, ecosystem, name string) (*registries.Package, error) {
	reg, err := r.registry(ecosystem, name)
	if err != nil {
		return nil, err
	}
	return reg.FetchPackage(ctx, name)
}

// mirrorJulia fetches the tarball unchecked because Pkg checks the tree hash
// of what it extracts.
func (m *Mirror) mirrorJulia(ctx context.Context, pv PackageVersion, tracker *progressTracker) {
	fail := func(err error) { m.recordFailure(tracker, pv, err) }
	uuid := strings.ToLower(pv.Qualifiers["uuid"])
	if uuid == "" {
		fail(fmt.Errorf("julia packages need a uuid qualifier, such as pkg:julia/%s@%s?uuid=<uuid>", pv.Name, pv.Version))
		return
	}

	pkg, err := m.packageFinder().FindPackage(ctx, pv.Ecosystem, pv.Name)
	if err != nil {
		fail(err)
		return
	}
	registered, _ := pkg.Metadata["uuid"].(string)
	if !strings.EqualFold(registered, uuid) {
		fail(fmt.Errorf("%s in the General registry has uuid %s, not %s; only General packages can be mirrored", pv.Name, registered, uuid))
		return
	}
	// The handler names packages as the registry does.
	name := pkg.Name
	if name == "" {
		name = pv.Name
	}

	v, err := m.versionFinder().FindVersion(ctx, pv.Ecosystem, name, pv.Version)
	if err != nil {
		fail(err)
		return
	}
	treeHash, _ := v.Metadata["git-tree-sha1"].(string)
	if treeHash == "" {
		fail(errors.New("the registry lists no tree hash for this version"))
		return
	}
	url, filename, err := handler.JuliaPackageDownload(m.JuliaUpstream, uuid, treeHash)
	if err != nil {
		fail(err)
		return
	}
	result, err := m.proxy.GetOrFetchArtifactFromURL(ctx, pv.Ecosystem, name, treeHash, filename, url)
	if err != nil {
		fail(err)
		return
	}
	m.recordResult(tracker, pv, result)
}

// packageFinder finds packages where Versions lists versions, so both come
// from the same registry.
func (m *Mirror) packageFinder() PackageFinder { //nolint:ireturn // any finder will do
	if finder, ok := m.Versions.(PackageFinder); ok {
		return finder
	}
	return RegistryVersions{}
}
