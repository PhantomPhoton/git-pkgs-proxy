package mirror

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/git-pkgs/proxy/internal/denylist"
	"github.com/git-pkgs/proxy/internal/handler"
)

func TestImageRefFromPURL(t *testing.T) {
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	tests := []struct {
		purl          string
		platforms     []string
		wantRef       handler.ImageRef
		wantPlatforms []handler.Platform
		wantErr       string
	}{
		{
			purl:    "pkg:docker/library/alpine@3.20",
			wantRef: handler.ImageRef{Name: "library/alpine", References: []string{"3.20"}},
		},
		{
			purl:    "pkg:docker/ghcr.io/team/app@1.0",
			wantRef: handler.ImageRef{Registry: "ghcr.io", Name: "team/app", References: []string{"1.0"}},
		},
		{
			purl:    "pkg:docker/registry.example:5000/team/app@" + digest,
			wantRef: handler.ImageRef{Registry: "registry.example:5000", Name: "team/app", References: []string{digest}},
		},
		{
			purl:    "pkg:docker/localhost/app@1.0",
			wantRef: handler.ImageRef{Registry: "localhost", Name: "app", References: []string{"1.0"}},
		},
		{
			purl:    "pkg:docker/localhost:5000/team/app@1.0",
			wantRef: handler.ImageRef{Registry: "localhost:5000", Name: "team/app", References: []string{"1.0"}},
		},
		{
			purl:          "pkg:docker/alpine@3.20?repository_url=ghcr.io",
			platforms:     []string{"linux/arm64/v8"},
			wantRef:       handler.ImageRef{Registry: "ghcr.io", Name: "alpine", References: []string{"3.20"}},
			wantPlatforms: []handler.Platform{{OS: "linux", Architecture: "arm64", Variant: "v8"}},
		},
		{
			purl:    "pkg:docker/app@1.0?repository_url=https://registry.example/team",
			wantRef: handler.ImageRef{Registry: "registry.example", Name: "team/app", References: []string{"1.0"}},
		},
		{
			purl:          "pkg:oci/debian@" + digest + "?repository_url=docker.io/library/debian&tag=bookworm&arch=arm64",
			wantRef:       handler.ImageRef{Registry: "docker.io", Name: "library/debian", References: []string{digest, "bookworm"}},
			wantPlatforms: []handler.Platform{{Architecture: "arm64"}},
		},
		{
			purl:          "pkg:oci/debian@" + digest + "?repository_url=docker.io/library/debian&arch=arm64",
			platforms:     []string{"linux/amd64"},
			wantRef:       handler.ImageRef{Registry: "docker.io", Name: "library/debian", References: []string{digest}},
			wantPlatforms: []handler.Platform{{OS: "linux", Architecture: "amd64"}},
		},
		{
			purl:    "pkg:oci/debian@" + digest,
			wantErr: "repository_url",
		},
	}
	for _, tt := range tests {
		t.Run(tt.purl, func(t *testing.T) {
			var items []PackageVersion
			source := &PURLSource{PURLs: []string{tt.purl}, Platforms: tt.platforms}
			if err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
				items = append(items, pv)
				return nil
			}); err != nil {
				t.Fatalf("Enumerate: %v", err)
			}
			if len(items) != 1 {
				t.Fatalf("items = %d, want 1", len(items))
			}
			ref, platforms, err := imageRef(items[0])
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("imageRef: %v", err)
			}
			if !reflect.DeepEqual(ref, tt.wantRef) {
				t.Errorf("ref = %+v, want %+v", ref, tt.wantRef)
			}
			if !reflect.DeepEqual(platforms, tt.wantPlatforms) {
				t.Errorf("platforms = %+v, want %+v", platforms, tt.wantPlatforms)
			}
		})
	}
}

func TestUnversionedImagePURLIsNotExpanded(t *testing.T) {
	var items []PackageVersion
	source := &PURLSource{PURLs: []string{"pkg:docker/library/alpine"}}
	if err := source.Enumerate(context.Background(), func(pv PackageVersion) error {
		items = append(items, pv)
		return nil
	}); err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(items) != 1 || items[0].Version != "" {
		t.Fatalf("items = %+v, want one unversioned item", items)
	}
}

type fakeImages struct {
	mu     sync.Mutex
	refs   []handler.ImageRef
	result handler.ImageResult
	err    error
}

func (f *fakeImages) MirrorImage(_ context.Context, ref handler.ImageRef, _ []handler.Platform) (handler.ImageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs = append(f.refs, ref)
	return f.result, f.err
}

func TestMirrorRunCountsImages(t *testing.T) {
	tests := []struct {
		name          string
		result        handler.ImageResult
		err           error
		wantCompleted int64
		wantSkipped   int64
		wantFailed    int64
	}{
		{name: "fetched", result: handler.ImageResult{Fetched: 3, Bytes: 42}, wantCompleted: 1},
		{name: "cached", result: handler.ImageResult{}, wantSkipped: 1},
		{name: "failed", err: errors.New("boom"), wantFailed: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := setupTestMirror(t, 1)
			images := &fakeImages{result: tt.result, err: tt.err}
			m.Images = images
			progress, err := m.Run(context.Background(), &PURLSource{PURLs: []string{"pkg:docker/library/alpine@3.20"}})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if progress.Completed != tt.wantCompleted || progress.Skipped != tt.wantSkipped || progress.Failed != tt.wantFailed {
				t.Errorf("progress = %+v", progress)
			}
			if progress.Bytes != tt.result.Bytes {
				t.Errorf("bytes = %d, want %d", progress.Bytes, tt.result.Bytes)
			}
			if len(images.refs) != 1 || images.refs[0].Name != "library/alpine" {
				t.Errorf("refs = %+v", images.refs)
			}
		})
	}
}

func TestMirrorRunDeniedImage(t *testing.T) {
	m := setupTestMirror(t, 1)
	policy, err := denylist.New([]string{"pkg:docker/library/alpine@3.20"})
	if err != nil {
		t.Fatal(err)
	}
	m.proxy.Denylist = policy
	images := &fakeImages{}
	m.Images = images

	progress, err := m.Run(context.Background(), &PURLSource{PURLs: []string{"pkg:docker/library/alpine@3.20"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if progress.Failed != 1 || len(images.refs) != 0 {
		t.Errorf("denied image: failed = %d, mirror calls = %d", progress.Failed, len(images.refs))
	}
}

func TestJobRequestRejectsInvalidPlatform(t *testing.T) {
	js := &JobStore{}
	if _, err := js.sourceFromRequest(JobRequest{PURLs: []string{"pkg:docker/library/alpine@3.20"}, Platforms: []string{"linux"}}); err == nil {
		t.Error("invalid platform was accepted")
	}
}
