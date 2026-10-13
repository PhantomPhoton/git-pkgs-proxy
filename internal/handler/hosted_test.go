package handler

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/registries/fetch"
)

func TestHostedTargetFor(t *testing.T) {
	tests := []struct {
		name                    string
		ecosystem, pkg, version string
		want                    HostedTarget
		wantErr                 error
	}{
		{name: "npm", ecosystem: "npm", pkg: "request", version: "2.88.3",
			want: HostedTarget{Name: "request", Version: "2.88.3", Filename: "request-2.88.3.tgz"}},
		{name: "npm scoped", ecosystem: "npm", pkg: "@types/node", version: "20.1.0",
			want: HostedTarget{Name: "@types/node", Version: "20.1.0", Filename: "node-20.1.0.tgz"}},
		{name: "composer", ecosystem: "composer", pkg: "monolog/monolog", version: "3.5.0",
			want: HostedTarget{Name: "monolog/monolog", Version: "3.5.0", Filename: "monolog-3.5.0.zip"}},
		{name: "composer without vendor", ecosystem: "composer", pkg: "monolog", version: "3.5.0",
			wantErr: ErrHostedInvalidName},
		{name: "composer with extra segment", ecosystem: "composer", pkg: "a/b/c", version: "1.0.0",
			wantErr: ErrHostedInvalidName},
		{name: "unsupported ecosystem", ecosystem: "cargo", pkg: "serde", version: "1.0.200",
			wantErr: ErrHostedUnsupported},
		{name: "unknown ecosystem", ecosystem: "maven", pkg: "junit", version: "4.13",
			wantErr: ErrHostedUnsupported},
		{name: "npm scope without @", ecosystem: "npm", pkg: "types/node", version: "1.0.0",
			wantErr: ErrHostedInvalidName},
		{name: "dot-dot name segment", ecosystem: "npm", pkg: "@scope/..", version: "1.0.0",
			wantErr: ErrHostedInvalidName},
		{name: "dot-dot version", ecosystem: "npm", pkg: "request", version: "..",
			wantErr: ErrHostedInvalidName},
		{name: "control character", ecosystem: "npm", pkg: "request", version: "1.0.0\n",
			wantErr: ErrHostedInvalidName},
		{name: "empty version", ecosystem: "npm", pkg: "request", version: "",
			wantErr: ErrHostedInvalidName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HostedTargetFor(tt.ecosystem, tt.pkg, tt.version)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("HostedTargetFor() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("HostedTargetFor() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("HostedTargetFor() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

const pushedTarball = "patched request tarball"

func pushRequest(t *testing.T, proxy *Proxy) HostedArtifact {
	t.Helper()
	target, err := HostedTargetFor("npm", "request", "2.88.3")
	if err != nil {
		t.Fatalf("HostedTargetFor: %v", err)
	}
	pushed, err := proxy.PushLocalArtifact(context.Background(), "npm", target, strings.NewReader(pushedTarball), "application/gzip")
	if err != nil {
		t.Fatalf("PushLocalArtifact: %v", err)
	}
	return pushed
}

func TestPushLocalArtifactIsServedWithoutUpstream(t *testing.T) {
	proxy, db, _, fetcher := setupTestProxy(t)

	pushed := pushRequest(t, proxy)
	if pushed.Version != "2.88.3" || pushed.Filename != "request-2.88.3.tgz" || pushed.Size != int64(len(pushedTarball)) || len(pushed.SHA256) != 64 {
		t.Errorf("pushed artifact = %+v", pushed)
	}

	result, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz",
		"https://registry.npmjs.org/request/-/request-2.88.3.tgz")
	if err != nil {
		t.Fatalf("GetOrFetchArtifactFromURL: %v", err)
	}
	if got := readResult(result); got != pushedTarball {
		t.Errorf("served body = %q, want the pushed tarball", got)
	}
	if fetcher.fetchCalled {
		t.Error("serving a pushed artifact contacted the upstream")
	}

	art, err := db.GetArtifact("pkg:npm/request@2.88.3", "request-2.88.3.tgz")
	if err != nil || art == nil {
		t.Fatalf("GetArtifact: %+v, %v", art, err)
	}
	if art.Source != database.SourceLocal || art.UpstreamURL != "" {
		t.Errorf("artifact source/upstream_url = %q/%q, want local/empty", art.Source, art.UpstreamURL)
	}
}

func TestPushLocalArtifactReplacesCachedUpstreamCopy(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("upstream tarball")), ContentType: "application/gzip"}

	const upstreamURL = "https://registry.npmjs.org/request/-/request-2.88.3.tgz"
	first, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz", upstreamURL)
	if err != nil {
		t.Fatalf("caching the upstream copy: %v", err)
	}
	_ = readResult(first)

	pushRequest(t, proxy)

	result, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz", upstreamURL)
	if err != nil {
		t.Fatalf("GetOrFetchArtifactFromURL: %v", err)
	}
	if got := readResult(result); got != pushedTarball {
		t.Errorf("served body = %q, want the pushed tarball", got)
	}
}

func TestLocalArtifactsAndDeleteLocalVersion(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	pushed := pushRequest(t, proxy)

	listed, err := proxy.LocalArtifacts("npm", "request")
	if err != nil {
		t.Fatalf("LocalArtifacts: %v", err)
	}
	if len(listed) != 1 || listed[0].Version != pushed.Version || listed[0].SHA256 != pushed.SHA256 {
		t.Fatalf("LocalArtifacts = %+v, want the pushed artifact %+v", listed, pushed)
	}

	if err := proxy.DeleteLocalVersion("npm", "request", "2.88.3"); err != nil {
		t.Fatalf("DeleteLocalVersion: %v", err)
	}
	if cached, err := proxy.GetCachedArtifact(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz"); err != nil || cached != nil {
		t.Errorf("GetCachedArtifact after delete = %+v, %v, want nil, nil", cached, err)
	}
	if listed, err := proxy.LocalArtifacts("npm", "request"); err != nil || len(listed) != 0 {
		t.Errorf("LocalArtifacts after delete = %+v, %v, want none", listed, err)
	}

	if err := proxy.DeleteLocalVersion("npm", "request", "2.88.3"); !errors.Is(err, ErrHostedVersionNotFound) {
		t.Errorf("second DeleteLocalVersion error = %v, want %v", err, ErrHostedVersionNotFound)
	}
}

func TestGetLocalArtifactIgnoresUpstreamCopies(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("upstream tarball")), ContentType: "application/gzip"}
	cached, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz",
		"https://registry.npmjs.org/request/-/request-2.88.3.tgz")
	if err != nil {
		t.Fatalf("caching the upstream copy: %v", err)
	}
	_ = readResult(cached)

	local, err := proxy.GetLocalArtifact(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz")
	if err != nil || local != nil {
		t.Fatalf("GetLocalArtifact for an upstream copy = %+v, %v, want nil, nil", local, err)
	}

	pushRequest(t, proxy)
	local, err = proxy.GetLocalArtifact(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz")
	if err != nil || local == nil {
		t.Fatalf("GetLocalArtifact for a pushed artifact = %+v, %v", local, err)
	}
	if got := readResult(local); got != pushedTarball {
		t.Errorf("served body = %q, want the pushed tarball", got)
	}
}
