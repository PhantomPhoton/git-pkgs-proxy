package database

import (
	"slices"
	"strings"

	"github.com/git-pkgs/purl"
)

// ecosystemSpellings returns the names an ecosystem's packages can be stored
// under. The proxy's handlers write "gem" and "composer" where git-pkgs, and
// proxy mirror before it used the handler names, write "rubygems" and
// "packagist".
func ecosystemSpellings(ecosystem string) []string {
	purlType := purl.EcosystemToPURLType(ecosystem)
	spellings := []string{ecosystem}
	for _, s := range []string{purlType, purl.PURLTypeToEcosystem(purlType)} {
		if s != "" && !slices.Contains(spellings, s) {
			spellings = append(spellings, s)
		}
	}
	return spellings
}

// ecosystemFilter returns a condition matching p.ecosystem against every
// spelling of ecosystem, and its arguments.
func ecosystemFilter(ecosystem string) (string, []any) {
	spellings := ecosystemSpellings(ecosystem)
	args := make([]any, len(spellings))
	for i, s := range spellings {
		args[i] = s
	}
	return "p.ecosystem IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(spellings)), ", ") + ")", args
}
