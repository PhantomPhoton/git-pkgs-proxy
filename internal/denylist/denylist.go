// Package denylist implements an immutable, exact-version package policy.
package denylist

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/git-pkgs/purl"
)

// Policy is safe for concurrent reads. A nil policy allows every version.
type Policy struct {
	packages map[string]map[string]bool
}

// New validates and canonicalizes versioned PURLs. Qualifiers and subpaths are
// rejected because the proxy cannot reliably distinguish them at every endpoint,
// except a conda channel, which download URLs always name.
func New(packages []string) (*Policy, error) {
	if len(packages) == 0 {
		return nil, nil
	}
	p := &Policy{packages: make(map[string]map[string]bool)}
	for _, value := range packages {
		pkg, err := purl.Parse(value)
		if err != nil {
			return nil, fmt.Errorf("invalid denylist package %q: %w", value, err)
		}
		if pkg.Version == "" || !allowedQualifiers(pkg) || pkg.Subpath != "" ||
			strings.ContainsAny(pkg.Namespace+pkg.Name+pkg.Version, "*?[]<>=| \t\r\n") {
			return nil, fmt.Errorf("invalid denylist package %q: expected an exact versioned PURL without qualifiers or subpath", value)
		}
		id := identify(pkg)
		key := id.key
		if id.channel != "" {
			key = id.channelKey()
		}
		if p.packages[key] == nil {
			p.packages[key] = make(map[string]bool)
		}
		p.packages[key][id.version] = true
	}
	return p, nil
}

func allowedQualifiers(pkg *purl.PURL) bool {
	for _, q := range pkg.Qualifiers {
		if pkg.Type != "conda" || q.Key != "channel" {
			return false
		}
	}
	return true
}

// Denied reports whether a versioned PURL is denied.
func (p *Policy) Denied(value string) bool {
	if p == nil || len(p.packages) == 0 {
		return false
	}
	pkg, err := purl.Parse(value)
	if err != nil {
		return false
	}
	id := identify(pkg)
	versions := []string{id.version}
	// A platform gem's version carries the platform ("1.16.0-x86_64-linux"),
	// so also match the plain version.
	if pkg.Type == "gem" {
		if base, _, found := strings.Cut(id.version, "-"); found && base != "" {
			versions = append(versions, base)
		}
	}
	for _, version := range versions {
		if p.packages[id.key][version] || (id.channel != "" && p.packages[id.channelKey()][version]) {
			return true
		}
	}
	return false
}

// Versions returns a caller-owned set of denied versions for a package PURL.
func (p *Policy) Versions(packagePURL string) map[string]bool {
	if p == nil || len(p.packages) == 0 {
		return nil
	}
	pkg, err := purl.Parse(packagePURL)
	if err != nil {
		return nil
	}
	id := identify(pkg)
	result := make(map[string]bool)
	for version := range p.packages[id.key] {
		result[version] = true
	}
	if id.channel != "" {
		for version := range p.packages[id.channelKey()] {
			result[version] = true
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// identity is the form a PURL is compared in: the same for denylist entries
// and for the PURLs handlers build, however each spells the package.
type identity struct {
	key     string // versionless PURL without channel
	channel string // conda channel, if any
	version string
}

func (id identity) channelKey() string {
	return id.key + "?channel=" + id.channel
}

var pypiSeparators = regexp.MustCompile(`[-_.]+`)

// swiftRegistryIdentity returns the registry scope and name of a Swift
// package. A source coordinate such as pkg:swift/github.com/apple/swift-nio
// names no registry identity.
func swiftRegistryIdentity(pkg *purl.PURL) (scope, name string, ok bool) {
	switch {
	case pkg.Type == "swift" && pkg.Namespace != "" && !strings.ContainsAny(pkg.Namespace, "./"):
		return pkg.Namespace, pkg.Name, true
	case pkg.Type == "generic" && strings.EqualFold(pkg.Namespace, "swift-registry"):
		scope, name, found := strings.Cut(pkg.Name, ".")
		return scope, name, found && scope != "" && name != ""
	}
	return "", "", false
}

func identify(pkg *purl.PURL) identity {
	namespace, name, version := pkg.Namespace, pkg.Name, pkg.Version
	var channel string
	switch pkg.Type {
	case "pypi":
		// PEP 503: case-insensitive, runs of -, _ and . are equivalent.
		name = pypiSeparators.ReplaceAllString(strings.ToLower(name), "-")
	case "nuget":
		// NuGet ids and versions are case-insensitive.
		name, version = strings.ToLower(name), strings.ToLower(version)
	case "conda":
		// The conda handler names packages "channel/name"; entries may give the
		// channel as a qualifier or namespace. An entry without one matches
		// every channel.
		channel = pkg.Qualifier("channel")
		if namespace != "" {
			channel, namespace = namespace, ""
		}
		if i := strings.LastIndex(name, "/"); i >= 0 {
			channel, name = name[:i], name[i+1:]
		}
	case "composer":
		// Packagist keeps a tag's leading v ("v6.1.7"); Composer treats it
		// as the same version.
		if len(version) > 1 && (version[0] == 'v' || version[0] == 'V') && version[1] >= '0' && version[1] <= '9' {
			version = version[1:]
		}
	case "oci":
		// Handlers name the repository as one name, library%2Falpine;
		// entries write it as a path, library/alpine.
		if namespace != "" {
			name, namespace = namespace+"/"+name, ""
		}
	case "swift", "generic":
		// Swift registry archives are stored as
		// pkg:generic/swift-registry/{scope}.{name}; entries may write
		// pkg:swift/{scope}/{name}. Registry identities ignore case.
		if scope, pkgName, ok := swiftRegistryIdentity(pkg); ok {
			namespace, name = strings.ToLower(scope), strings.ToLower(pkgName)
			key := purl.New("swift", namespace, name, "", nil).String()
			return identity{key: key, version: version}
		}
	}
	key := purl.New(pkg.Type, namespace, name, "", nil).String()
	return identity{key: key, channel: channel, version: version}
}
