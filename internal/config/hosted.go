package config

import "fmt"

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
	return nil
}
