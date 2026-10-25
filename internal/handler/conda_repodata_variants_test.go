package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// The proxy filters only the plain JSON repodata. With cooldown on, any
// other form of it would list versions still in cooldown, so it is refused
// and clients fall back to repodata.json.
func TestCondaRepodataVariantsUnderCooldown(t *testing.T) {
	plain, err := json.Marshal(map[string]any{
		"packages": map[string]any{
			"old.tar.bz2": map[string]any{"name": "demo", "timestamp": time.Now().Add(-7 * 24 * time.Hour).UnixMilli()},
			"new.tar.bz2": map[string]any{"name": "demo", "timestamp": time.Now().UnixMilli()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write(plain)
	}))
	defer upstream.Close()

	variants := []string{
		"repodata.json.bz2",
		"repodata.json.zst",
		"repodata.jlap",
		"current_repodata.json.zst",
		"repodata_from_packages.json.zst",
	}
	for _, enabled := range []bool{true, false} {
		proxy := testProxy()
		proxy.HTTPClient = upstream.Client()
		if enabled {
			proxy.Cooldown = &cooldown.Config{Default: "3d"}
		}
		handler := NewCondaHandlerWithUpstream(proxy, "http://proxy.local", upstream.URL).Routes()
		for _, filename := range variants {
			requests.Store(0)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/conda-forge/linux-64/"+filename, nil))
			switch {
			case enabled && (w.Code != http.StatusNotFound || requests.Load() != 0):
				t.Errorf("cooldown on, %s: status %d, %d upstream requests; want 404 without asking upstream", filename, w.Code, requests.Load())
			case !enabled && (w.Code != http.StatusOK || requests.Load() != 1):
				t.Errorf("cooldown off, %s: status %d, %d upstream requests; want it passed through", filename, w.Code, requests.Load())
			}
		}
	}

	// repodata_from_packages.json is plain JSON and is filtered like
	// repodata.json.
	proxy := testProxy()
	proxy.HTTPClient = upstream.Client()
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	handler := NewCondaHandlerWithUpstream(proxy, "http://proxy.local", upstream.URL).Routes()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/conda-forge/linux-64/repodata_from_packages.json", nil))
	var result map[string]map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("status %d, body %s: %v", w.Code, w.Body, err)
	}
	if len(result["packages"]) != 1 || result["packages"]["old.tar.bz2"] == nil {
		t.Errorf("repodata_from_packages.json not filtered: %s", w.Body)
	}
}
