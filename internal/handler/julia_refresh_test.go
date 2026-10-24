package handler

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func juliaRegistryTarball(t *testing.T, packageName string) []byte {
	t.Helper()
	body := "name = \"General\"\n[packages]\n295af30f-e4ad-537b-8983-00126c2a3abe = { name = \"" + packageName + "\", path = \"R/X\" }\n"
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: "Registry.toml", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = gw.Close()
	return buf.Bytes()
}

type fakeJuliaUpstream struct {
	mu         sync.Mutex
	advertised string
	tarballs   map[string][]byte
	downloads  map[string]int
}

func (u *fakeJuliaUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if r.URL.Path == "/registries" {
		_, _ = w.Write([]byte("/registry/" + juliaGeneralRegistryUUID + "/" + u.advertised + "\n"))
		return
	}
	hash := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	body, ok := u.tarballs[hash]
	if !ok {
		http.NotFound(w, r)
		return
	}
	u.downloads[hash]++
	_, _ = w.Write(body)
}

func (u *fakeJuliaUpstream) count(hash string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.downloads[hash]
}

func TestJuliaRefreshOnlyLoadsAdvertisedRegistry(t *testing.T) {
	oldHash, newHash := strings.Repeat("a", 40), strings.Repeat("b", 40)
	up := &fakeJuliaUpstream{
		advertised: newHash,
		tarballs: map[string][]byte{
			oldHash: juliaRegistryTarball(t, "OldName"),
			newHash: juliaRegistryTarball(t, "NewName"),
		},
		downloads: map[string]int{},
	}
	server := httptest.NewServer(up)
	defer server.Close()

	proxy, _, _, _ := setupTestProxy(t)
	h := NewJuliaHandlerWithUpstream(proxy, server.URL)
	ctx := context.Background()

	// An older snapshot neither replaces the map nor is downloaded for it.
	for range 5 {
		h.refreshNamesFromRegistry(ctx, juliaGeneralRegistryUUID, oldHash)
	}
	if n := up.count(oldHash); n != 0 {
		t.Fatalf("old registry downloaded %d times for the name map", n)
	}

	h.refreshNamesFromRegistry(ctx, juliaGeneralRegistryUUID, newHash)
	if got := h.resolveName("295af30f-e4ad-537b-8983-00126c2a3abe"); got != "NewName" {
		t.Fatalf("name = %q, want NewName", got)
	}

	// The loaded hash is not fetched again, and an old one doesn't roll back.
	h.refreshNamesFromRegistry(ctx, juliaGeneralRegistryUUID, newHash)
	h.refreshNamesFromRegistry(ctx, juliaGeneralRegistryUUID, oldHash)
	if n := up.count(newHash); n != 1 {
		t.Errorf("current registry downloaded %d times, want 1", n)
	}
	if got := h.resolveName("295af30f-e4ad-537b-8983-00126c2a3abe"); got != "NewName" {
		t.Errorf("name rolled back to %q", got)
	}
}

func TestJuliaRefreshSkipsWhileLoading(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewJuliaHandlerWithUpstream(proxy, "http://127.0.0.1:1")
	h.loadMu.Lock()
	done := make(chan struct{})
	go func() {
		h.refreshNamesFromRegistry(context.Background(), juliaGeneralRegistryUUID, strings.Repeat("c", 40))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh waited for a running load instead of skipping")
	}
	h.loadMu.Unlock()
}

func TestJuliaExtractRegistryNamesLimitsRegistryToml(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	size := int64(maxJuliaRegistryTomlSize + 1)
	if err := tw.WriteHeader(&tar.Header{Name: "Registry.toml", Mode: 0o644, Size: size}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(bytes.Repeat([]byte("#"), int(size))); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gw.Close()
	if _, err := extractRegistryNames(&buf); err == nil {
		t.Fatal("oversized Registry.toml was accepted")
	}
}
