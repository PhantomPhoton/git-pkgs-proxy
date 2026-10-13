package config

import "testing"

func TestHostedConfigValidate(t *testing.T) {
	t.Setenv("HOSTED_TEST_TOKEN", "from-env")

	tests := []struct {
		name    string
		hosted  HostedConfig
		wantErr bool
	}{
		{name: "disabled without token", hosted: HostedConfig{}},
		{name: "enabled without token", hosted: HostedConfig{Enabled: true}, wantErr: true},
		{name: "enabled with token", hosted: HostedConfig{Enabled: true, Token: "s3cret"}},
		{name: "token from env reference", hosted: HostedConfig{Enabled: true, Token: "${HOSTED_TEST_TOKEN}"}},
		{name: "token env reference unset", hosted: HostedConfig{Enabled: true, Token: "${HOSTED_TEST_UNSET}"}, wantErr: true},
		{name: "invalid upload size", hosted: HostedConfig{Enabled: true, Token: "s3cret", MaxUploadSize: "lots"}, wantErr: true},
		{name: "zero upload size", hosted: HostedConfig{Enabled: true, Token: "s3cret", MaxUploadSize: "0"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.hosted.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHostedConfigDefaults(t *testing.T) {
	h := HostedConfig{Enabled: true, Token: "${HOSTED_TEST_TOKEN}"}
	t.Setenv("HOSTED_TEST_TOKEN", "from-env")
	if err := h.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := h.TokenExpanded(); got != "from-env" {
		t.Errorf("TokenExpanded() = %q, want from-env", got)
	}
	if got := h.MaxUploadBytes(); got != 100<<20 {
		t.Errorf("MaxUploadBytes() = %d, want %d", got, 100<<20)
	}

	h.MaxUploadSize = "2GB"
	if err := h.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := h.MaxUploadBytes(); got != 2<<30 {
		t.Errorf("MaxUploadBytes() = %d, want %d", got, int64(2)<<30)
	}
}

func TestValidateRejectsHostedWithoutCachedArtifacts(t *testing.T) {
	cfg := Default()
	cfg.Hosted = HostedConfig{Enabled: true, Token: "s3cret"}
	cfg.Storage.CacheArtifacts = false
	if err := cfg.Validate(); err == nil {
		t.Error("Validate() accepted hosted.enabled with storage.cache_artifacts: false")
	}
}

func TestLoadFromEnvHosted(t *testing.T) {
	cfg := Default()
	t.Setenv("PROXY_HOSTED_ENABLED", "true")
	t.Setenv("PROXY_HOSTED_TOKEN", "env-token")
	t.Setenv("PROXY_HOSTED_MAX_UPLOAD_SIZE", "50MB")

	cfg.LoadFromEnv()

	if !cfg.Hosted.Enabled {
		t.Error("Hosted.Enabled not loaded from PROXY_HOSTED_ENABLED")
	}
	if cfg.Hosted.Token != "env-token" {
		t.Errorf("Hosted.Token = %q, want env-token", cfg.Hosted.Token)
	}
	if cfg.Hosted.MaxUploadSize != "50MB" {
		t.Errorf("Hosted.MaxUploadSize = %q, want 50MB", cfg.Hosted.MaxUploadSize)
	}
}
