package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
)

const redirectAuthHeader = "X-Api-Key"

// headerRecorder remembers the auth header value each request carried.
type headerRecorder struct {
	mu     sync.Mutex
	values []string
}

func (h *headerRecorder) record(r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.values = append(h.values, r.Header.Get(redirectAuthHeader))
}

func (h *headerRecorder) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.values...)
}

func TestHeaderAuthIsNotForwardedAcrossRedirects(t *testing.T) {
	var atOther headerRecorder
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atOther.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"leftpad","versions":{}}`)
	}))
	defer other.Close()

	var atRegistry headerRecorder
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atRegistry.record(r)
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer registry.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving proxy address: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	tempDir := t.TempDir()
	cfg := config.Default()
	cfg.Listen = listener.Addr().String()
	cfg.BaseURL = "http://" + cfg.Listen
	cfg.Database.Path = filepath.Join(tempDir, "proxy.db")
	cfg.Storage.URL = "file://" + filepath.Join(tempDir, "artifacts")
	cfg.Upstream.AllowLoopback = true
	cfg.Upstream.NPM = registry.URL
	cfg.Upstream.Auth = map[string]config.AuthConfig{
		registry.URL: {Type: "header", HeaderName: redirectAuthHeader, HeaderValue: "registry-secret"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validating config: %v", err)
	}

	proxyServer, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), BuildInfo{Version: "test", Commit: "test"})
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}
	startErr := make(chan error, 1)
	go func() { startErr <- proxyServer.Start(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := proxyServer.Shutdown(ctx); err != nil {
			t.Errorf("shutting down server: %v", err)
		}
		if err := <-startErr; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Start() error = %v, want %v", err, http.ErrServerClosed)
		}
	}()

	resp := getWhenListening(t, cfg.BaseURL+"/npm/leftpad")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	registrySeen := atRegistry.seen()
	if len(registrySeen) == 0 || registrySeen[0] != "registry-secret" {
		t.Errorf("registry received %s = %q, want the configured value", redirectAuthHeader, registrySeen)
	}
	for _, value := range atOther.seen() {
		if value != "" {
			t.Errorf("redirect target received %s = %q, want none", redirectAuthHeader, value)
		}
	}
}

// getWhenListening retries a GET until the proxy accepts connections.
func getWhenListening(t *testing.T, url string) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(url)
		if err == nil {
			return resp
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy did not start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
