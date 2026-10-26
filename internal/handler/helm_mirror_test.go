package handler

import "testing"

func TestOCIChartPURL(t *testing.T) {
	tests := []struct{ reference, want string }{
		{"oci://ghcr.io/owner/demo:2.0.0", "pkg:oci/demo@2.0.0?repository_url=ghcr.io/owner/demo"},
		{"oci://ghcr.io/owner/demo", "pkg:oci/demo@1.0.0?repository_url=ghcr.io/owner/demo"},
		{"oci://localhost:5000/demo", "pkg:oci/demo@1.0.0?repository_url=localhost:5000/demo"},
		{"oci://ghcr.io/owner/demo@sha256:abc", "pkg:oci/demo@sha256:abc?repository_url=ghcr.io/owner/demo"},
	}
	for _, tt := range tests {
		if got := ociChartPURL(tt.reference, "1.0.0"); got != tt.want {
			t.Errorf("ociChartPURL(%q) = %q, want %q", tt.reference, got, tt.want)
		}
	}
}
