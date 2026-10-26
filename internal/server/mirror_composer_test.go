package server

import (
	"crypto/sha1" //nolint:gosec // Packagist lists SHA-1 shasums
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/git-pkgs/cooldown"
	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
)

const composerCommit = "0123456789abcdef0123456789abcdef01234567"

// composerUpstream serves the Packagist API for acme/widget with release
// 1.0.0, published in 2020, and branch dev-main, and the 1.0.0 archive.
type composerUpstream struct {
	*httptest.Server
	archive  string
	shasum   string
	apiHits  atomic.Int32
	distHits atomic.Int32
}

func newComposerUpstream(t *testing.T) *composerUpstream {
	t.Helper()
	u := &composerUpstream{archive: composerZip(`{"name":"acme/widget"}`)}
	sum := sha1.Sum([]byte(u.archive)) //nolint:gosec // see import
	u.shasum = hex.EncodeToString(sum[:])
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/packages/acme/widget.json":
			u.apiHits.Add(1)
			dist := func(shasum string) map[string]string {
				return map[string]string{"type": "zip", "url": u.URL + "/zipball/" + composerCommit, "shasum": shasum}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"package": map[string]any{
				"name": "acme/widget",
				"versions": map[string]any{
					"1.0.0":    map[string]any{"version": "1.0.0", "time": "2020-01-02T03:04:05+00:00", "dist": dist(u.shasum)},
					"dev-main": map[string]any{"version": "dev-main", "time": "2026-10-01T00:00:00+00:00", "dist": dist("")},
				},
			}})
		case "/zipball/" + composerCommit:
			u.distHits.Add(1)
			_, _ = io.WriteString(w, u.archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func composerMirrorConfig(upstream *composerUpstream) *config.Config {
	cfg := config.Default()
	cfg.Upstream.Composer = upstream.URL
	cfg.Upstream.ComposerRepository = upstream.URL
	return cfg
}

func TestMirrorComposerReleaseIsServedFromCache(t *testing.T) {
	upstream := newComposerUpstream(t)
	cfg := composerMirrorConfig(upstream)

	progress, proxy := mirrorWithConfig(t, cfg, "pkg:composer/Acme/Widget@1.0.0")

	if progress.Completed != 1 || upstream.distHits.Load() != 1 {
		t.Fatalf("completed = %d, dist hits = %d, errors = %v", progress.Completed, upstream.distHits.Load(), progress.Errors)
	}

	// Composer gets the archive from the cache once the upstream is down,
	// with cooldown on as well: the mirror recorded the publish time.
	upstream.Close()
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	composer := handler.NewComposerHandlerWithUpstreams(proxy, "http://proxy.test", cfg.Upstream.Composer, cfg.Upstream.ComposerRepository)
	rec := httptest.NewRecorder()
	composer.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/files/acme/widget/1.0.0/"+composerCommit+".zip", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != upstream.archive {
		t.Errorf("archive from cache: status = %d, body %q; want 200 and the mirrored archive", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

func TestMirrorComposerRefusesShasumMismatch(t *testing.T) {
	upstream := newComposerUpstream(t)
	upstream.shasum = strings.Repeat("0", 40)

	progress, _ := mirrorWithConfig(t, composerMirrorConfig(upstream), "pkg:composer/acme/widget@1.0.0")

	if progress.Failed != 1 || progress.Completed != 0 {
		t.Errorf("completed = %d, failed = %d, errors = %v; want the archive refused", progress.Completed, progress.Failed, progress.Errors)
	}
}

func TestMirrorComposerRefusesBranches(t *testing.T) {
	upstream := newComposerUpstream(t)

	progress, _ := mirrorWithConfig(t, composerMirrorConfig(upstream), "pkg:composer/acme/widget@dev-main")

	if progress.Failed != 1 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0].Error, "branch") {
		t.Errorf("failed = %d, errors = %v; want the branch refused", progress.Failed, progress.Errors)
	}
	if upstream.distHits.Load() != 0 {
		t.Error("the branch archive was fetched")
	}
}

func TestMirrorComposerRefusesRoutedPackages(t *testing.T) {
	upstream := newComposerUpstream(t)
	cfg := composerMirrorConfig(upstream)
	cfg.Upstream.ComposerRoutes = map[string]string{"acme/*": "https://composer.example.com"}

	progress, _ := mirrorWithConfig(t, cfg, "pkg:composer/acme/widget@1.0.0")

	if progress.Failed != 1 || upstream.apiHits.Load() != 0 || upstream.distHits.Load() != 0 {
		t.Errorf("failed = %d, api hits = %d, dist hits = %d; want a routed package refused without asking Packagist",
			progress.Failed, upstream.apiHits.Load(), upstream.distHits.Load())
	}
}
