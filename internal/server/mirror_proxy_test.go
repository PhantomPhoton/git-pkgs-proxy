package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
)

// newTokenRegistry answers 401 with a Bearer challenge until the request
// carries the token its realm hands out.
func newTokenRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	var registry *httptest.Server
	registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token":"pull-token"}`)
		case "/v2/library/app/manifests/1.0":
			if r.Header.Get("Authorization") != "Bearer pull-token" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+registry.URL+`/token",service="registry",scope="repository:library/app:pull"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = io.WriteString(w, `{"schemaVersion":2}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(registry.Close)
	return registry
}

func TestNewMirrorProxyFollowsBearerChallenge(t *testing.T) {
	registry := newTokenRegistry(t)
	cfg := config.Default()
	cfg.Upstream.AllowLoopback = true
	proxy, _ := NewMirrorProxy(cfg, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	resp, err := proxy.HTTPClient.Get(registry.URL + "/v2/library/app/manifests/1.0")
	if err != nil {
		t.Fatalf("GET manifest: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after the Bearer challenge", resp.StatusCode)
	}
}

func TestNewMirrorProxyRefusesLoopbackByDefault(t *testing.T) {
	registry := newTokenRegistry(t)
	proxy, _ := NewMirrorProxy(config.Default(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	resp, err := proxy.HTTPClient.Get(registry.URL + "/v2/library/app/manifests/1.0")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("GET to a loopback registry succeeded without upstream.allow_loopback")
	}
}
