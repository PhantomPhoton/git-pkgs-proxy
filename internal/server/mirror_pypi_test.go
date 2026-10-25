package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
)

const (
	mirrorWheelPath      = "/packages/ab/cd/example_pkg-1.0-py3-none-any.whl"
	mirrorLinuxWheelPath = "/packages/23/45/example_pkg-1.0-cp312-cp312-manylinux_2_17_x86_64.whl"
	mirrorSdistPath      = "/packages/ef/01/example.pkg-1.0.tar.gz"
)

// pypiUpstream serves the JSON API for example-pkg 1.0 with two wheels, which
// are cached under the same name and version, and an sdist, whose filename
// spells the name differently. It counts file downloads by path.
type pypiUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	files map[string]string
	hits  map[string]int
}

func newPyPIUpstream(t *testing.T, digests map[string]string) *pypiUpstream {
	t.Helper()
	u := &pypiUpstream{
		files: map[string]string{mirrorWheelPath: "wheel", mirrorLinuxWheelPath: "linux wheel", mirrorSdistPath: "sdist"},
		hits:  map[string]int{},
	}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pypi/example-pkg/json" {
			var files []map[string]any
			for _, path := range []string{mirrorWheelPath, mirrorLinuxWheelPath, mirrorSdistPath} {
				sum := sha256.Sum256([]byte(u.files[path]))
				digest := hex.EncodeToString(sum[:])
				if d, ok := digests[path]; ok {
					digest = d
				}
				files = append(files, map[string]any{
					"url":     u.URL + path,
					"digests": map[string]string{"sha256": digest},
					"size":    len(u.files[path]),
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"info":     map[string]any{"name": "example-pkg"},
				"releases": map[string]any{"1.0": files},
			})
			return
		}
		body, ok := u.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		u.mu.Lock()
		u.hits[r.URL.Path]++
		u.mu.Unlock()
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *pypiUpstream) hitsFor(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[path]
}

func pypiMirrorConfig(upstream *pypiUpstream) *config.Config {
	cfg := config.Default()
	cfg.Upstream.PyPI = upstream.URL
	cfg.Upstream.PyPIDownload = upstream.URL
	return cfg
}

func TestMirrorPyPIFetchesEveryFile(t *testing.T) {
	upstream := newPyPIUpstream(t, nil)
	cfg := pypiMirrorConfig(upstream)

	progress, proxy := mirrorWithConfig(t, cfg, "pkg:pypi/example-pkg@1.0")

	if progress.Completed != 1 || progress.Failed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v", progress.Completed, progress.Failed, progress.Errors)
	}
	for path := range upstream.files {
		if got := upstream.hitsFor(path); got != 1 {
			t.Errorf("%s fetched %d times, want 1", path, got)
		}
	}

	// pip gets every file from the cache once the upstream is down.
	upstream.Close()
	pypi := handler.NewPyPIHandlerWithUpstreams(proxy, "http://proxy.test", cfg.Upstream.PyPI, cfg.Upstream.PyPIDownload)
	for path, want := range upstream.files {
		rec := httptest.NewRecorder()
		pypi.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("%s from cache: status = %d, body = %q, want 200 and %q", path, rec.Code, rec.Body, want)
		}
	}
}

func TestMirrorPyPIFileNameQualifier(t *testing.T) {
	upstream := newPyPIUpstream(t, nil)

	progress, _ := mirrorWithConfig(t, pypiMirrorConfig(upstream), "pkg:pypi/example-pkg@1.0?file_name=example.pkg-1.0.tar.gz")

	if progress.Completed != 1 || upstream.hitsFor(mirrorSdistPath) != 1 {
		t.Errorf("completed = %d, sdist hits = %d, errors = %v; want the sdist", progress.Completed, upstream.hitsFor(mirrorSdistPath), progress.Errors)
	}
	if upstream.hitsFor(mirrorWheelPath) != 0 {
		t.Error("the wheel was mirrored although file_name names the sdist")
	}

	progress, _ = mirrorWithConfig(t, pypiMirrorConfig(upstream), "pkg:pypi/example-pkg@1.0?file_name=missing-1.0.tar.gz")
	if progress.Failed != 1 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0].Error, "missing-1.0.tar.gz") {
		t.Errorf("failed = %d, errors = %v; want a failure naming the missing file", progress.Failed, progress.Errors)
	}
}

func TestMirrorPyPIRefusesDigestMismatch(t *testing.T) {
	upstream := newPyPIUpstream(t, map[string]string{mirrorWheelPath: strings.Repeat("0", 64)})

	progress, _ := mirrorWithConfig(t, pypiMirrorConfig(upstream), "pkg:pypi/example-pkg@1.0?file_name=example_pkg-1.0-py3-none-any.whl")

	if progress.Failed != 1 || progress.Completed != 0 {
		t.Errorf("completed = %d, failed = %d, errors = %v; want the mismatching wheel refused", progress.Completed, progress.Failed, progress.Errors)
	}
}
