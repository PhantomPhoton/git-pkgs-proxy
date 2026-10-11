package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/git-pkgs/registries/fetch"
)

// fakeImageRegistry serves manifests and blobs of a small multi-arch image,
// a single-arch image and a moved tag. It fails with 503 once offline.
type fakeImageRegistry struct {
	server    *httptest.Server
	offline   atomic.Bool
	manifests map[string]fakeManifest
	blobs     map[string]string
	blobGets  atomic.Int32
}

type fakeImage struct {
	indexDigest  string
	amd64Digest  string
	arm64Digest  string
	amd64Layer   string
	arm64Layer   string
	singleDigest string
	singleLayer  string
	// attestationLayers holds the amd64 and arm64 provenance statements.
	attestationLayers []string
}

func newFakeImageRegistry(t *testing.T) (*fakeImageRegistry, fakeImage) {
	t.Helper()
	const repository = "team/app"
	r := &fakeImageRegistry{manifests: make(map[string]fakeManifest), blobs: make(map[string]string)}
	var image fakeImage

	addBlob := func(content string) string {
		digest := sha256Digest([]byte(content))
		r.blobs["/v2/"+repository+"/blobs/"+digest] = content
		return digest
	}
	addManifest := func(contentType string, doc any, tags ...string) string {
		body, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256Digest(body)
		for _, reference := range append(tags, digest) {
			r.manifests["/v2/"+repository+"/manifests/"+reference] = fakeManifest{contentType: contentType, body: string(body)}
		}
		return digest
	}
	imageManifest := func(platform string) (string, string) {
		config := addBlob(`{"architecture":"` + platform + `"}`)
		layer := addBlob("layer for " + platform)
		digest := addManifest(ociManifestMediaType, map[string]any{
			"schemaVersion": 2, "mediaType": ociManifestMediaType,
			"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": config},
			"layers": []map[string]any{
				{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": layer},
				{"mediaType": "application/vnd.oci.image.layer.nondistributable.v1.tar+gzip", "digest": sha256Digest([]byte("foreign")), "urls": []string{"https://example.invalid/layer"}},
			},
		})
		return digest, layer
	}

	image.amd64Digest, image.amd64Layer = imageManifest("amd64")
	image.arm64Digest, image.arm64Layer = imageManifest("arm64")
	attestation := func(platformDigest, platform string) map[string]any {
		statement := addBlob("provenance for " + platform)
		digest := addManifest(ociManifestMediaType, map[string]any{
			"schemaVersion": 2, "mediaType": ociManifestMediaType,
			"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": addBlob("{}")},
			"layers": []map[string]any{{"mediaType": "application/vnd.in-toto+json", "digest": statement}},
		})
		image.attestationLayers = append(image.attestationLayers, statement)
		return map[string]any{
			"mediaType": ociManifestMediaType, "digest": digest,
			"platform": map[string]string{"os": "unknown", "architecture": "unknown"},
			"annotations": map[string]string{
				dockerReferenceTypeAnnotation:   attestationManifestReference,
				dockerReferenceDigestAnnotation: platformDigest,
			},
		}
	}
	image.indexDigest = addManifest(ociImageIndexMediaType, map[string]any{
		"schemaVersion": 2, "mediaType": ociImageIndexMediaType,
		"manifests": []map[string]any{
			{"mediaType": ociManifestMediaType, "digest": image.amd64Digest, "platform": map[string]string{"os": "linux", "architecture": "amd64"}},
			{"mediaType": ociManifestMediaType, "digest": image.arm64Digest, "platform": map[string]string{"os": "linux", "architecture": "arm64", "variant": "v8"}},
			attestation(image.amd64Digest, "amd64"),
			attestation(image.arm64Digest, "arm64"),
		},
	}, "multi")

	image.singleDigest, image.singleLayer = imageManifest("single")
	r.manifests["/v2/"+repository+"/manifests/single"] = r.manifests["/v2/"+repository+"/manifests/"+image.singleDigest]

	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.offline.Load() {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		if doc, ok := r.manifests[req.URL.Path]; ok {
			w.Header().Set("Content-Type", doc.contentType)
			w.Header().Set("Docker-Content-Digest", sha256Digest([]byte(doc.body)))
			_, _ = io.WriteString(w, doc.body)
			return
		}
		if blob, ok := r.blobs[req.URL.Path]; ok {
			r.blobGets.Add(1)
			_, _ = io.WriteString(w, blob)
			return
		}
		http.NotFound(w, req)
	}))
	t.Cleanup(r.server.Close)
	return r, image
}

func newMirrorTestProxy(t *testing.T, registry *fakeImageRegistry) *Proxy {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = registry.server.Client()
	proxy.Fetcher = fetch.NewFetcher(fetch.WithHTTPClient(registry.server.Client()), fetch.WithMaxRetries(0))
	proxy.MetadataTTL = 0
	return proxy
}

func pull(t *testing.T, h *ContainerHandler, target, accept string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	if accept != "" {
		request.Header.Set("Accept", accept)
	}
	response := httptest.NewRecorder()
	h.Routes().ServeHTTP(response, request)
	return response
}

func TestMirrorImageThenPullOffline(t *testing.T) {
	registry, image := newFakeImageRegistry(t)
	proxy := newMirrorTestProxy(t, registry)
	mirrorHandler := NewContainerHandlerWithRegistry(proxy, "http://proxy.example", registry.server.URL)

	result, err := mirrorHandler.MirrorImage(context.Background(), ImageRef{Name: "team/app", References: []string{"multi", "single"}}, nil)
	if err != nil {
		t.Fatalf("MirrorImage: %v", err)
	}
	if len(result.Warnings) == 0 || !strings.Contains(strings.Join(result.Warnings, "\n"), "non-distributable") {
		t.Errorf("warnings = %v, want the skipped foreign layer", result.Warnings)
	}
	registry.offline.Store(true)

	// A fresh handler instance, as after a restart, serves the pull.
	h := NewContainerHandlerWithRegistry(proxy, "http://proxy.example", registry.server.URL)
	for _, accept := range []string{dockerManifestAccept, containerdManifestAccept} {
		for _, reference := range []string{"multi", image.indexDigest, image.amd64Digest, image.arm64Digest, "single", image.singleDigest} {
			if response := pull(t, h, "/team/app/manifests/"+reference, accept); response.Code != http.StatusOK {
				t.Errorf("offline manifest %s with %.30q: status %d: %s", reference, accept, response.Code, response.Body.String())
			}
		}
	}
	for _, layer := range append([]string{image.amd64Layer, image.arm64Layer, image.singleLayer}, image.attestationLayers...) {
		if response := pull(t, h, "/team/app/blobs/"+layer, ""); response.Code != http.StatusOK {
			t.Errorf("offline blob %s: status %d", layer, response.Code)
		}
	}

	registry.offline.Store(false)
	gets := registry.blobGets.Load()
	again, err := mirrorHandler.MirrorImage(context.Background(), ImageRef{Name: "team/app", References: []string{"multi"}}, nil)
	if err != nil {
		t.Fatalf("second MirrorImage: %v", err)
	}
	if again.Fetched != 0 || registry.blobGets.Load() != gets {
		t.Errorf("second run fetched %d items and %d blobs, want nothing new", again.Fetched, registry.blobGets.Load()-gets)
	}
}

func TestMirrorImagePlatformFilter(t *testing.T) {
	registry, image := newFakeImageRegistry(t)
	proxy := newMirrorTestProxy(t, registry)
	h := NewContainerHandlerWithRegistry(proxy, "http://proxy.example", registry.server.URL)

	platform, err := ParsePlatform("linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.MirrorImage(context.Background(), ImageRef{Name: "team/app", References: []string{"multi"}}, []Platform{platform}); err != nil {
		t.Fatalf("MirrorImage: %v", err)
	}
	registry.offline.Store(true)
	if response := pull(t, h, "/team/app/blobs/"+image.arm64Layer, ""); response.Code != http.StatusOK {
		t.Errorf("arm64 layer status = %d, want 200", response.Code)
	}
	if response := pull(t, h, "/team/app/blobs/"+image.attestationLayers[1], ""); response.Code != http.StatusOK {
		t.Errorf("arm64 attestation status = %d, want 200", response.Code)
	}
	for _, layer := range []string{image.amd64Layer, image.attestationLayers[0]} {
		if response := pull(t, h, "/team/app/blobs/"+layer, ""); response.Code == http.StatusOK {
			t.Errorf("amd64 blob %s was mirrored despite the linux/arm64 filter", layer)
		}
	}

	registry.offline.Store(false)
	riscv, _ := ParsePlatform("linux/riscv64")
	if _, err := h.MirrorImage(context.Background(), ImageRef{Name: "team/app", References: []string{"multi"}}, []Platform{riscv}); err == nil {
		t.Error("a filter matching no index entry did not fail")
	}
}

func TestMirrorImageMovedTagMirrorsBothTargets(t *testing.T) {
	registry, image := newFakeImageRegistry(t)
	proxy := newMirrorTestProxy(t, registry)
	h := NewContainerHandlerWithRegistry(proxy, "http://proxy.example", registry.server.URL)

	result, err := h.MirrorImage(context.Background(), ImageRef{Name: "team/app", References: []string{image.singleDigest, "multi"}}, nil)
	if err != nil {
		t.Fatalf("MirrorImage: %v", err)
	}
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "now points to") {
		t.Errorf("warnings = %v, want the moved-tag warning", result.Warnings)
	}
	registry.offline.Store(true)
	for _, layer := range []string{image.singleLayer, image.amd64Layer} {
		if response := pull(t, h, "/team/app/blobs/"+layer, ""); response.Code != http.StatusOK {
			t.Errorf("blob %s status = %d, want 200", layer, response.Code)
		}
	}
}

func TestMirrorImageNamespaceRouteMatchesContainerdPull(t *testing.T) {
	registry, image := newFakeImageRegistry(t)
	proxy := newMirrorTestProxy(t, registry)
	named := map[string]string{"internal": registry.server.URL}
	h := NewContainerHandlerWithRegistry(proxy, "http://proxy.example", dockerHubRegistry, named)
	host := strings.TrimPrefix(registry.server.URL, "http://")

	if _, err := h.MirrorImage(context.Background(), ImageRef{Registry: host, Name: "team/app", References: []string{"single"}}, nil); err != nil {
		t.Fatalf("MirrorImage: %v", err)
	}
	registry.offline.Store(true)
	ns := "?" + url.Values{namespaceQueryParam: {host}}.Encode()
	if response := pull(t, h, "/team/app/manifests/single"+ns, containerdManifestAccept); response.Code != http.StatusOK {
		t.Errorf("containerd ns manifest status = %d: %s", response.Code, response.Body.String())
	}
	if response := pull(t, h, "/team/app/blobs/"+image.singleLayer+ns, ""); response.Code != http.StatusOK {
		t.Errorf("containerd ns blob status = %d", response.Code)
	}
	if response := pull(t, h, "/upstream/internal/team/app/blobs/"+image.singleLayer, ""); response.Code != http.StatusOK {
		t.Errorf("prefix-route blob status = %d", response.Code)
	}

	if _, err := h.MirrorImage(context.Background(), ImageRef{Registry: "unknown.example", Name: "team/app", References: []string{"single"}}, nil); err == nil {
		t.Error("an unconfigured registry host was accepted")
	}
}

func TestNormalizeImageRef(t *testing.T) {
	tests := []struct {
		name      string
		ref       ImageRef
		defaultRT string
		wantName  string
		wantErr   bool
	}{
		{name: "docker hub default adds library", ref: ImageRef{Name: "alpine", References: []string{"3.20"}}, defaultRT: dockerHubRegistry, wantName: "library/alpine"},
		{name: "docker hub ns adds library", ref: ImageRef{Registry: "hub.docker.com", Name: "alpine", References: []string{"3.20"}}, defaultRT: "https://registry.example", wantName: "library/alpine"},
		{name: "other default keeps name", ref: ImageRef{Name: "alpine", References: []string{"3.20"}}, defaultRT: "https://registry.example", wantName: "alpine"},
		{name: "path traversal", ref: ImageRef{Name: "../../x", References: []string{"1"}}, defaultRT: dockerHubRegistry, wantErr: true},
		{name: "query in tag", ref: ImageRef{Name: "library/alpine", References: []string{"3.20?x#y"}}, defaultRT: dockerHubRegistry, wantErr: true},
		{name: "short digest", ref: ImageRef{Name: "library/alpine", References: []string{"sha256:abcd"}}, defaultRT: dockerHubRegistry, wantErr: true},
		{name: "no reference", ref: ImageRef{Name: "library/alpine"}, defaultRT: dockerHubRegistry, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeImageRef(tt.ref, tt.defaultRT)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got.Name != tt.wantName {
				t.Errorf("name = %q, want %q", got.Name, tt.wantName)
			}
		})
	}
}
