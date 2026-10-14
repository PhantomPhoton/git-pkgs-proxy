package config

import (
	"fmt"
	"strings"
)

const defaultHostedMaxUploadSize = "100MB"

// HostedConfig configures packages pushed to the proxy through the
// /api/hosted endpoints. Pushed versions are served in place of upstream
// versions with the same number, so writes always require a token.
type HostedConfig struct {
	// Enabled registers the /api/hosted endpoints. Off by default.
	Enabled bool `json:"enabled" yaml:"enabled"`

	// Token is the bearer token required to push and delete packages.
	// Required when Enabled. Supports ${VAR_NAME} expansion like AuthConfig
	// fields.
	Token string `json:"token" yaml:"token"`

	// MaxUploadSize caps a single pushed archive (e.g., "100MB"). Must be > 0.
	// Default: "100MB".
	MaxUploadSize string `json:"max_upload_size" yaml:"max_upload_size"`

	// NPMScopes are npm scopes (e.g. "@acme") served only from pushed
	// packages. The upstream registry is never asked about them, so a package
	// published there under the same name can't be mixed in.
	NPMScopes []string `json:"npm_scopes" yaml:"npm_scopes"`

	// ComposerVendors are Composer vendors (e.g. "acme") served only from
	// pushed packages, like NPMScopes.
	ComposerVendors []string `json:"composer_vendors" yaml:"composer_vendors"`

	maxUploadBytes int64
}

// TokenExpanded returns Token with ${VAR_NAME} references expanded.
func (h *HostedConfig) TokenExpanded() string {
	return expandEnv(h.Token)
}

// MaxUploadBytes returns the parsed MaxUploadSize. Validate must run first.
func (h *HostedConfig) MaxUploadBytes() int64 {
	return h.maxUploadBytes
}

// Validate checks the hosted package settings, applying the default upload
// size if unset.
func (h *HostedConfig) Validate() error {
	if h.MaxUploadSize == "" {
		h.MaxUploadSize = defaultHostedMaxUploadSize
	}
	size, err := ParseSize(h.MaxUploadSize)
	if err != nil {
		return fmt.Errorf("invalid hosted.max_upload_size: %w", err)
	}
	if size <= 0 {
		return fmt.Errorf("invalid hosted.max_upload_size %q: must be > 0", h.MaxUploadSize)
	}
	h.maxUploadBytes = size

	if h.Enabled && h.TokenExpanded() == "" {
		return fmt.Errorf("hosted.token is required when hosted.enabled is true")
	}
	for _, scope := range h.NPMScopes {
		if len(scope) < 2 || scope[0] != '@' || strings.Contains(scope, "/") {
			return fmt.Errorf("invalid hosted.npm_scopes entry %q: must be a scope such as \"@acme\"", scope)
		}
	}
	for _, vendor := range h.ComposerVendors {
		if vendor == "" || strings.Contains(vendor, "/") {
			return fmt.Errorf("invalid hosted.composer_vendors entry %q: must be a vendor such as \"acme\"", vendor)
		}
	}
	return nil
}
