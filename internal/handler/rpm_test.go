package handler

import (
	"testing"

	"github.com/git-pkgs/purl"
)

func TestRPMHandler_parseRPMPath(t *testing.T) {
	h := &RPMHandler{}

	assertPathParser(t, "parseRPMPath", h.parseRPMPath, []pathParseCase{
		{"releases/39/Everything/x86_64/os/Packages/n/nginx-1.24.0-1.fc39.x86_64.rpm", "nginx", "1.24.0-1.fc39", "x86_64"},
		{"Packages/kernel-core-6.5.5-200.fc38.x86_64.rpm", "kernel-core", "6.5.5-200.fc38", "x86_64"},
		{"updates/39/Everything/aarch64/Packages/g/git-2.42.0-1.fc39.aarch64.rpm", "git", "2.42.0-1.fc39", "aarch64"},
		{"vim-enhanced-9.0.1000-1.fc38.noarch.rpm", "vim-enhanced", "9.0.1000-1.fc38", "noarch"},
		{"invalid.rpm", "", "", ""},
		{"not-an-rpm-file", "", "", ""},
	})
}

func TestRPMHandler_Routes(t *testing.T) {
	h := NewRPMHandler(nil, "http://localhost:8080")
	assertRoutesBasics(t, h.Routes(), "/repodata/repomd.xml", "/releases/../../../etc/passwd")
}

func TestRPMRetentionKey(t *testing.T) {
	tests := []struct {
		key  string
		want string
		ok   bool
	}{
		{"pkg:rpm/curl", "pkg:rpm/curl", true},
		{"pkg:rpm/fedora/curl", "pkg:rpm/curl", true},
		{"pkg:rpm/Fedora/NetworkManager", "pkg:rpm/NetworkManager", true},
		{"pkg:rpm/fedora/libstdc++", "pkg:rpm/libstdc%2B%2B", true},
		{"pkg:rpm/fedora/40/curl", "", false},
		{"pkg:rpm/fedora/Packages%2Fcurl", "", false},
		{"pkg:deb/fedora/curl", "", false},
	}
	for _, tt := range tests {
		p, err := purl.Parse(tt.key)
		if err != nil {
			t.Fatalf("Parse(%s): %v", tt.key, err)
		}
		got, ok := rpmRetentionKey(p)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("rpmRetentionKey(%s) = %q, %v, want %q, %v", tt.key, got, ok, tt.want, tt.ok)
		}
	}
	if _, ok := rpmRetentionKey(nil); ok {
		t.Error("rpmRetentionKey(nil) accepted")
	}
}
