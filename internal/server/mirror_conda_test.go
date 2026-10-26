package server

import (
	"crypto/md5" //nolint:gosec // anaconda.org gives older files only an MD5
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
	"github.com/git-pkgs/proxy/internal/mirror"
	"github.com/git-pkgs/registries"
)

const (
	condaLinuxFile  = "linux-64/example-pkg-1.0-h123_0.conda"
	condaNoarchFile = "noarch/example-pkg-1.0-pyh_0.tar.bz2"
)

// condaUpstream serves the anaconda.org package API for
// conda-forge/example-pkg 1.0, with a linux-64 build that has a SHA-256 and
// a noarch build that, like older uploads, has only an MD5. It counts file
// downloads by subdir and filename.
type condaUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	files map[string]string
	hits  map[string]int
}

func newCondaUpstream(t *testing.T) *condaUpstream {
	t.Helper()
	u := &condaUpstream{
		files: map[string]string{condaLinuxFile: "linux build", condaNoarchFile: "noarch build"},
		hits:  map[string]int{},
	}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/package/conda-forge/example-pkg" {
			linuxSum := sha256.Sum256([]byte(u.files[condaLinuxFile]))
			noarchSum := md5.Sum([]byte(u.files[condaNoarchFile])) //nolint:gosec // see import
			file := func(basename, build string) map[string]any {
				subdir, _, _ := strings.Cut(basename, "/")
				return map[string]any{
					"version":      "1.0",
					"basename":     basename,
					"download_url": u.URL + "/download/conda-forge/example-pkg/1.0/" + basename,
					"attrs":        map[string]any{"subdir": subdir, "build": build},
					"size":         len(u.files[basename]),
				}
			}
			linux := file(condaLinuxFile, "h123_0")
			linux["sha256"] = hex.EncodeToString(linuxSum[:])
			noarch := file(condaNoarchFile, "pyh_0")
			noarch["md5"] = hex.EncodeToString(noarchSum[:])
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":     "example-pkg",
				"versions": []string{"1.0"},
				"files":    []any{linux, noarch},
			})
			return
		}
		basename := strings.TrimPrefix(r.URL.Path, "/download/conda-forge/example-pkg/1.0/")
		body, ok := u.files[basename]
		if !ok {
			http.NotFound(w, r)
			return
		}
		u.mu.Lock()
		u.hits[basename]++
		u.mu.Unlock()
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *condaUpstream) hitsFor(basename string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[basename]
}

// mirrorConda mirrors purls with files listed from upstream. The proxy only
// lists conda files from anaconda.org itself, so the test points the lister
// at the fake directly.
func mirrorConda(t *testing.T, upstream *condaUpstream, purls ...string) (*mirror.Progress, *handler.Proxy) {
	t.Helper()
	versions := mirror.RegistryVersions{
		Client: registries.NewClient(registries.WithHTTPClient(upstream.Client())),
		URL:    func(string, string) (string, error) { return upstream.URL, nil },
	}
	return mirrorWithVersions(t, config.Default(), versions, purls...)
}

func TestMirrorCondaFetchesEveryFile(t *testing.T) {
	upstream := newCondaUpstream(t)

	progress, proxy := mirrorConda(t, upstream, "pkg:conda/conda-forge/example-pkg@1.0")

	if progress.Completed != 1 || progress.Failed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v", progress.Completed, progress.Failed, progress.Errors)
	}
	for _, basename := range []string{condaLinuxFile, condaNoarchFile} {
		if got := upstream.hitsFor(basename); got != 1 {
			t.Errorf("%s fetched %d times, want 1", basename, got)
		}
	}

	// conda gets both files from the cache once the upstream is down.
	upstream.Close()
	conda := handler.NewCondaHandlerWithUpstream(proxy, "http://proxy.test", upstream.URL)
	for basename, want := range upstream.files {
		rec := httptest.NewRecorder()
		conda.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/conda-forge/"+basename, nil))
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("%s from cache: status = %d, body = %q, want 200 and %q", basename, rec.Code, rec.Body, want)
		}
	}

	progress, _ = mirrorConda(t, upstream, "pkg:conda/conda-forge/example-pkg@1.0")
	if progress.Failed != 1 {
		t.Errorf("failed = %d after the upstream closed, want 1", progress.Failed)
	}
}

func TestMirrorCondaQualifiersSelectFiles(t *testing.T) {
	upstream := newCondaUpstream(t)

	progress, _ := mirrorConda(t, upstream, "pkg:conda/example-pkg@1.0?channel=conda-forge&subdir=noarch")

	if progress.Completed != 1 || upstream.hitsFor(condaNoarchFile) != 1 {
		t.Errorf("completed = %d, noarch hits = %d, errors = %v; want the noarch build", progress.Completed, upstream.hitsFor(condaNoarchFile), progress.Errors)
	}
	if upstream.hitsFor(condaLinuxFile) != 0 {
		t.Error("the linux-64 build was mirrored although subdir names noarch")
	}

	progress, _ = mirrorConda(t, upstream, "pkg:conda/conda-forge/example-pkg@1.0?subdir=linux-64&type=tar.bz2")
	if progress.Failed != 1 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0].Error, "subdir=linux-64 type=tar.bz2") {
		t.Errorf("failed = %d, errors = %v; want a failure naming the selection", progress.Failed, progress.Errors)
	}
}

func TestMirrorCondaNeedsChannel(t *testing.T) {
	upstream := newCondaUpstream(t)

	progress, _ := mirrorConda(t, upstream, "pkg:conda/example-pkg@1.0")

	if progress.Failed != 1 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0].Error, "channel") {
		t.Errorf("failed = %d, errors = %v; want a failure asking for a channel", progress.Failed, progress.Errors)
	}
	if upstream.hitsFor(condaLinuxFile)+upstream.hitsFor(condaNoarchFile) != 0 {
		t.Error("files were fetched for a package without a channel")
	}
}
