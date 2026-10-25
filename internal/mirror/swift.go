package mirror

import (
	"context"
	"fmt"
	"strings"

	"github.com/git-pkgs/proxy/internal/handler"
)

// mirrorSwift mirrors the source archive of a Swift registry release, with
// its release metadata, from the registry the proxy serves Swift packages
// from.
func (m *Mirror) mirrorSwift(ctx context.Context, pv PackageVersion, tracker *progressTracker) {
	scope, name, err := swiftScopeAndName(pv.Name)
	if err != nil {
		m.recordFailure(tracker, pv, err)
		return
	}
	upstream, err := m.registryURL("swift", pv.Name)
	if err != nil {
		m.recordFailure(tracker, pv, err)
		return
	}
	result, err := handler.NewSwiftHandler(m.proxy, "", upstream).MirrorRelease(ctx, scope, name, pv.Version)
	if err != nil {
		m.recordFailure(tracker, pv, err)
		return
	}
	m.recordResult(tracker, pv, result)
}

// registryURL returns the registry base URL Versions lists purlType packages
// from, which is the configured upstream, or empty for the default one.
func (m *Mirror) registryURL(purlType, name string) (string, error) {
	if r, ok := m.Versions.(RegistryVersions); ok && r.URL != nil {
		return r.URL(purlType, name)
	}
	return "", nil
}

// swiftScopeAndName splits the name of a pkg:swift PURL, its namespace and
// name, into a registry scope and package name. The namespace of a registry
// package is its scope, as in pkg:swift/apple/swift-nio for the identity
// apple.swift-nio. A namespace naming a source repository, such as
// pkg:swift/github.com/apple/swift-nio, isn't a registry identity and is
// refused.
func swiftScopeAndName(name string) (scope, pkg string, _ error) {
	scope, pkg, ok := strings.Cut(name, "/")
	if !ok || strings.ContainsAny(scope, ".:") || strings.Contains(pkg, "/") {
		return "", "", fmt.Errorf("pkg:swift/%s names a source repository; mirroring needs a registry scope as the namespace, such as pkg:swift/apple/swift-nio for apple.swift-nio", name)
	}
	return scope, pkg, nil
}

// swiftIdentity returns the registry identity, scope.name, of the name of a
// pkg:swift PURL.
func swiftIdentity(name string) (string, error) {
	scope, pkg, err := swiftScopeAndName(name)
	if err != nil {
		return "", err
	}
	return scope + "." + pkg, nil
}
