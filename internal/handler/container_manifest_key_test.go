package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

const (
	dockerManifestAccept = "application/vnd.docker.distribution.manifest.v2+json, " +
		"application/vnd.docker.distribution.manifest.list.v2+json, " +
		"application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json"
	containerdManifestAccept = "application/vnd.docker.distribution.manifest.v2+json, " +
		"application/vnd.docker.distribution.manifest.list.v2+json, " +
		"application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, */*"
	ociManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
)

// fakeManifestRegistry serves one document per manifest path and fails with
// 503 once it is switched off.
type fakeManifestRegistry struct {
	server  *httptest.Server
	offline atomic.Bool
	docs    map[string]fakeManifest
}

type fakeManifest struct {
	contentType string
	body        string
}

func newFakeManifestRegistry(t *testing.T) *fakeManifestRegistry {
	t.Helper()
	registry := &fakeManifestRegistry{docs: make(map[string]fakeManifest)}
	registry.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if registry.offline.Load() {
			http.Error(w, "offline", http.StatusServiceUnavailable)
			return
		}
		doc, ok := registry.docs[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", doc.contentType)
		w.Header().Set("Docker-Content-Digest", sha256Digest([]byte(doc.body)))
		_, _ = io.WriteString(w, doc.body)
	}))
	t.Cleanup(registry.server.Close)
	return registry
}

func (r *fakeManifestRegistry) set(path, contentType, body string) {
	r.docs[path] = fakeManifest{contentType: contentType, body: body}
}

func newManifestKeyTestHandler(t *testing.T, registry *fakeManifestRegistry) *ContainerHandler {
	t.Helper()
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = registry.server.Client()
	// A zero TTL makes every tag request revalidate, so an offline upstream
	// exercises the stale path a real offline pull takes.
	proxy.MetadataTTL = 0
	return &ContainerHandler{proxy: proxy, registryURL: registry.server.URL, proxyURL: "http://localhost:8080"}
}

func getManifest(t *testing.T, h *ContainerHandler, path, accept string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Accept", accept)
	response := httptest.NewRecorder()
	h.Routes().ServeHTTP(response, request)
	return response
}

func TestContainerHandler_SingleArchTagSharedAcrossClients(t *testing.T) {
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`
	registry := newFakeManifestRegistry(t)
	registry.set("/v2/library/app/manifests/1.0", ociManifestMediaType, manifest)
	h := newManifestKeyTestHandler(t, registry)

	if response := getManifest(t, h, "/library/app/manifests/1.0", dockerManifestAccept); response.Code != http.StatusOK {
		t.Fatalf("docker pull status = %d: %s", response.Code, response.Body.String())
	}
	registry.offline.Store(true)

	response := getManifest(t, h, "/library/app/manifests/1.0", containerdManifestAccept)
	if response.Code != http.StatusOK {
		t.Fatalf("offline containerd pull status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if response.Body.String() != manifest {
		t.Errorf("body = %q, want %q", response.Body.String(), manifest)
	}
}

func TestContainerHandler_DigestManifestSharedAcrossClients(t *testing.T) {
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{}}`
	digest := sha256Digest([]byte(manifest))
	registry := newFakeManifestRegistry(t)
	registry.set("/v2/library/app/manifests/"+digest, ociManifestMediaType, manifest)
	h := newManifestKeyTestHandler(t, registry)

	if response := getManifest(t, h, "/library/app/manifests/"+digest, dockerManifestAccept); response.Code != http.StatusOK {
		t.Fatalf("docker pull status = %d: %s", response.Code, response.Body.String())
	}
	registry.offline.Store(true)

	response := getManifest(t, h, "/library/app/manifests/"+digest, ociManifestMediaType)
	if response.Code != http.StatusOK {
		t.Fatalf("offline pull by digest status = %d, want 200: %s", response.Code, response.Body.String())
	}
}

func TestContainerHandler_RepushedTagServesLatestDocument(t *testing.T) {
	index := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`
	registry := newFakeManifestRegistry(t)
	registry.set("/v2/library/app/manifests/latest", ociImageIndexMediaType, index)
	h := newManifestKeyTestHandler(t, registry)

	if response := getManifest(t, h, "/library/app/manifests/latest", dockerManifestAccept); response.Body.String() != index {
		t.Fatalf("first pull body = %q, want index", response.Body.String())
	}
	registry.set("/v2/library/app/manifests/latest", ociManifestMediaType, manifest)
	if response := getManifest(t, h, "/library/app/manifests/latest", containerdManifestAccept); response.Body.String() != manifest {
		t.Fatalf("pull after re-push body = %q, want manifest", response.Body.String())
	}
	registry.offline.Store(true)

	response := getManifest(t, h, "/library/app/manifests/latest", dockerManifestAccept)
	if response.Code != http.StatusOK {
		t.Fatalf("offline pull status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if response.Body.String() != manifest {
		t.Errorf("offline body = %q, want the re-pushed manifest", response.Body.String())
	}
}

func TestContainerHandler_IndexlessClientKeepsOwnTagEntry(t *testing.T) {
	index := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`
	converted := `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`
	const schema2 = "application/vnd.docker.distribution.manifest.v2+json"
	registry := newFakeManifestRegistry(t)
	registry.set("/v2/library/app/manifests/latest", schema2, converted)
	h := newManifestKeyTestHandler(t, registry)

	// A client without index support gets the converted platform manifest.
	if response := getManifest(t, h, "/library/app/manifests/latest", schema2); response.Body.String() != converted {
		t.Fatalf("index-less pull body = %q", response.Body.String())
	}
	registry.set("/v2/library/app/manifests/latest", ociImageIndexMediaType, index)
	if response := getManifest(t, h, "/library/app/manifests/latest", dockerManifestAccept); response.Body.String() != index {
		t.Fatalf("index-capable pull body = %q, want the index", response.Body.String())
	}
	registry.offline.Store(true)

	if response := getManifest(t, h, "/library/app/manifests/latest", containerdManifestAccept); response.Body.String() != index {
		t.Errorf("offline index-capable body = %q, want the index", response.Body.String())
	}
	if response := getManifest(t, h, "/library/app/manifests/latest", schema2); response.Body.String() != converted {
		t.Errorf("offline index-less body = %q, want the converted manifest", response.Body.String())
	}
}

func TestContainerHandler_IncompatibleSharedEntryFallsBackToAcceptEntry(t *testing.T) {
	list := `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.list.v2+json","manifests":[]}`
	index := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`
	registry := newFakeManifestRegistry(t)
	registry.set("/v2/library/app/manifests/latest", dockerManifestListMediaType, list)
	h := newManifestKeyTestHandler(t, registry)

	const dockerOnly = "application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"
	if response := getManifest(t, h, "/library/app/manifests/latest", dockerOnly); response.Body.String() != list {
		t.Fatalf("Docker-only pull body = %q, want the manifest list", response.Body.String())
	}
	registry.set("/v2/library/app/manifests/latest", ociImageIndexMediaType, index)
	if response := getManifest(t, h, "/library/app/manifests/latest", ociImageIndexMediaType); response.Body.String() != index {
		t.Fatalf("OCI pull body = %q, want the index", response.Body.String())
	}
	registry.offline.Store(true)

	response := getManifest(t, h, "/library/app/manifests/latest", dockerOnly)
	if response.Code != http.StatusOK {
		t.Fatalf("offline Docker-only pull status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if response.Body.String() != list {
		t.Errorf("offline Docker-only body = %q, want the manifest list", response.Body.String())
	}
	if response := getManifest(t, h, "/library/app/manifests/latest", ociImageIndexMediaType); response.Body.String() != index {
		t.Errorf("offline OCI body = %q, want the index still in the shared entry", response.Body.String())
	}
}
