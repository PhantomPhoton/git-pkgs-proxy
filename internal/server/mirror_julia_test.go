package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/mirror"
	"github.com/git-pkgs/registries"
)

const (
	juliaExampleUUID  = "7876af07-990d-54b4-ab0e-23690620f79a"
	juliaExampleTree  = "e1f0e1a832ccd8e97d6d0348dec33ee139a5aeaf"
	juliaGeneralUUID  = "23338594-aafe-5451-b93e-139f81909106"
	juliaRegistryTree = "465d1e55da75e226473a37c40bf2c20aee19234d"
)

// juliaUpstream serves the General registry's files for Example, which has
// version 0.5.5, and as a Pkg server /registries, the General registry
// tarball and Example's 0.5.5 source tarball. It counts package downloads.
type juliaUpstream struct {
	*httptest.Server
	tarball      string
	packageHits  atomic.Int32
	registryHits atomic.Int32
}

func newJuliaUpstream(t *testing.T) *juliaUpstream {
	t.Helper()
	u := &juliaUpstream{tarball: "Example 0.5.5 source"}
	registry := juliaRegistryTarball(t, "[packages]\n"+juliaExampleUUID+` = { name = "Example", path = "E/Example" }`+"\n")
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/E/Example/Package.toml":
			_, _ = io.WriteString(w, "name = \"Example\"\nuuid = \""+juliaExampleUUID+"\"\nrepo = \"https://github.com/JuliaLang/Example.jl.git\"\n")
		case "/E/Example/Versions.toml":
			_, _ = io.WriteString(w, "[\"0.5.4\"]\ngit-tree-sha1 = \"11820aa9c229fd3833d4bd69e5e75ef4e7273bf1\"\n\n[\"0.5.5\"]\ngit-tree-sha1 = \""+juliaExampleTree+"\"\n")
		case "/registries":
			_, _ = io.WriteString(w, "/registry/"+juliaGeneralUUID+"/"+juliaRegistryTree+"\n")
		case "/registry/" + juliaGeneralUUID + "/" + juliaRegistryTree:
			u.registryHits.Add(1)
			_, _ = w.Write(registry)
		case "/package/" + juliaExampleUUID + "/" + juliaExampleTree:
			u.packageHits.Add(1)
			_, _ = io.WriteString(w, u.tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

// juliaRegistryTarball returns a gzipped registry tarball holding registryToml.
func juliaRegistryTarball(t *testing.T, registryToml string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "Registry.toml", Mode: 0o644, Size: int64(len(registryToml))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, registryToml); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// mirrorJulia mirrors purls from upstream as the Pkg server, with the
// registry's files read from it too.
func mirrorJulia(t *testing.T, upstream *juliaUpstream, purls ...string) (*mirror.Progress, *handler.Proxy) {
	t.Helper()
	cfg := config.Default()
	cfg.Upstream.Julia = upstream.URL
	versions := mirror.RegistryVersions{
		Client: registries.NewClient(registries.WithHTTPClient(upstream.Client())),
		URL:    func(string, string) (string, error) { return upstream.URL, nil },
	}
	return mirrorWithVersions(t, cfg, versions, purls...)
}

func getJuliaPackage(julia *handler.JuliaHandler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	julia.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/package/"+juliaExampleUUID+"/"+juliaExampleTree, nil))
	return rec
}

func TestMirrorJuliaPackage(t *testing.T) {
	upstream := newJuliaUpstream(t)

	progress, proxy := mirrorJulia(t, upstream, "pkg:julia/Example@0.5.5?uuid="+juliaExampleUUID)

	if progress.Completed != 1 || progress.Failed != 0 || upstream.packageHits.Load() != 1 {
		t.Fatalf("completed = %d, failed = %d, package hits = %d, errors = %v", progress.Completed, progress.Failed, upstream.packageHits.Load(), progress.Errors)
	}
	if cached, _ := proxy.GetCachedArtifact(t.Context(), "julia", "Example", juliaExampleTree, juliaExampleTree+".tar.gz"); cached == nil {
		t.Fatal("the tarball isn't cached under Example and its tree hash")
	} else {
		_ = cached.Reader.Close()
	}

	// With the registry's names loaded, the handler finds the tarball under
	// the package's name without fetching it again.
	julia := handler.NewJuliaHandlerWithUpstream(proxy, upstream.URL)
	if rec := getJuliaPackage(julia); rec.Code != http.StatusOK || rec.Body.String() != upstream.tarball {
		t.Errorf("status = %d, body = %q, want 200 and the tarball", rec.Code, rec.Body)
	}
	if upstream.registryHits.Load() == 0 {
		t.Error("the handler didn't load the registry's names")
	}

	// Without the upstream, a new handler can't load the names, and finds
	// the tarball by its tree hash.
	upstream.Close()
	if rec := getJuliaPackage(handler.NewJuliaHandlerWithUpstream(proxy, upstream.URL)); rec.Code != http.StatusOK || rec.Body.String() != upstream.tarball {
		t.Errorf("from cache: status = %d, body = %q, want 200 and the tarball", rec.Code, rec.Body)
	}
	if got := upstream.packageHits.Load(); got != 1 {
		t.Errorf("package fetched %d times, want 1", got)
	}
}

func TestMirrorJuliaRefusesOtherUUID(t *testing.T) {
	upstream := newJuliaUpstream(t)

	progress, _ := mirrorJulia(t, upstream, "pkg:julia/Example@0.5.5?uuid=00000000-0000-0000-0000-000000000000")

	if progress.Failed != 1 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0].Error, juliaExampleUUID) {
		t.Errorf("failed = %d, errors = %v; want a failure naming General's uuid", progress.Failed, progress.Errors)
	}
	if upstream.packageHits.Load() != 0 {
		t.Error("a tarball was fetched for a package General doesn't have")
	}
}

func TestServerMirrorFetchesJuliaFromUpstream(t *testing.T) {
	cfg := config.Default()
	cfg.Upstream.Julia = "https://julia.example.com"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy, _ := NewMirrorProxy(cfg, nil, nil, logger)
	s := &Server{cfg: cfg, logger: logger, ecr: newECRTokens(logger)}

	if got := s.newMirror(proxy, 1).JuliaUpstream; got != cfg.Upstream.Julia {
		t.Errorf("JuliaUpstream = %q, want upstream.julia", got)
	}
}
