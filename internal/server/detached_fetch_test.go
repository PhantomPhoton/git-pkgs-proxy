package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
)

// TestServerKeepsFetchAfterClientHangsUp checks that the server marks client
// requests with handler.DetachUpstreamFetch: a pacman client that gives up on
// a slow cache miss must not cancel the upstream fetch, so the next request
// is served from the cache without fetching again.
func TestServerKeepsFetchAfterClientHangsUp(t *testing.T) {
	if os.Getenv("PROXY_TEST_DETACHED_FETCH") == "1" {
		testServerKeepsFetchAfterClientHangsUp(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestServerKeepsFetchAfterClientHangsUp$")
	cmd.Env = append(os.Environ(), "PROXY_TEST_DETACHED_FETCH=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("detached fetch test failed: %v\n%s", err, output)
	}
}

func testServerKeepsFetchAfterClientHangsUp(t *testing.T) {
	const pkgPath = "/core/os/x86_64/big-1.0-1-x86_64.pkg.tar.zst"
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pkgPath {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		<-release
		_, _ = io.WriteString(w, "big package")
	}))
	defer upstream.Close()

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
	cfg.Upstream.Arch = map[string]string{"archlinux": upstream.URL}
	cfg.Upstream.AllowLoopback = true
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
		releaseOnce.Do(func() { close(release) })
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := proxyServer.Shutdown(ctx); err != nil {
			t.Errorf("shutting down server: %v", err)
		}
		if err := <-startErr; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Start() error = %v, want %v", err, http.ErrServerClosed)
		}
	}()

	target := cfg.BaseURL + "/arch/archlinux" + pkgPath

	// The client gives up while the upstream is still sending nothing.
	impatient := &http.Client{Timeout: 300 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for hits.Load() == 0 {
		if _, err := impatient.Get(target); err == nil {
			t.Fatal("request succeeded while the upstream was held")
		}
		if time.Now().After(deadline) {
			t.Fatal("upstream fetch never started")
		}
	}
	// Let the server notice the hang-up before the upstream answers.
	time.Sleep(100 * time.Millisecond)
	releaseOnce.Do(func() { close(release) })

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("follow-up request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "big package" {
		t.Fatalf("follow-up: status %d, body %q", resp.StatusCode, body)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream fetches = %d, want 1: the client hanging up cancelled the first fetch", got)
	}
}
