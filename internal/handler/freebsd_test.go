package handler

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/purl"
	"github.com/git-pkgs/registries/fetch"
)

const freebsdABI = "/FreeBSD:14:amd64"

func TestFreeBSDPathClassification(t *testing.T) {
	tests := []struct {
		path      string
		catalogue bool
		hashed    bool
	}{
		{"FreeBSD:14:amd64/latest/meta.conf", true, false},
		{"FreeBSD:14:amd64/latest/meta", true, false},
		{"FreeBSD:14:amd64/latest/packagesite.pkg", true, false},
		{"FreeBSD:14:amd64/latest/data.pkg", true, false},
		{"FreeBSD:13:amd64/quarterly/packagesite.txz", true, false},
		{"142amd64-default/packagesite.tzst", true, false},
		{"FreeBSD:14:amd64/latest/All/Hashed/zsh-5.9_5~2$3rwh1rgp.pkg", false, true},
		{"FreeBSD:15:amd64/base_latest/Hashed/FreeBSD-acct-15.snap20261021214449~2$k6xtent4.pkg", false, true},
		{"FreeBSD:14:amd64/latest/All/zsh-5.9_5.pkg", false, false},
		{"FreeBSD:15:amd64/base_latest/FreeBSD-acct-15.snap20261021214449.pkg", false, false},
		{"FreeBSD:14:amd64/latest/Latest/pkg.pkg", false, false},
		{"FreeBSD:14:amd64/latest/Latest/pkg.pkg.sig", false, false},
		{"FreeBSD:14:amd64/latest/All/Hashed/no-hash.pkg", false, false},
	}
	for _, tt := range tests {
		if got := isFreeBSDCatalogue(tt.path); got != tt.catalogue {
			t.Errorf("isFreeBSDCatalogue(%q) = %v, want %v", tt.path, got, tt.catalogue)
		}
		if got := isFreeBSDHashedPackage(tt.path); got != tt.hashed {
			t.Errorf("isFreeBSDHashedPackage(%q) = %v, want %v", tt.path, got, tt.hashed)
		}
	}
}

func TestParseFreeBSDPackage(t *testing.T) {
	tests := []struct {
		filename, name, version string
	}{
		{"zsh-5.9_5~2$3rwh1rgp.pkg", "zsh", "5.9_5"},
		{"zsh-navigation-tools-2.2.7~2$imxi7k7w.pkg", "zsh-navigation-tools", "2.2.7"},
		{"py311-requests-2.32.3_1,1~2$abcdefgh.pkg", "py311-requests", "2.32.3_1,1"},
		{"FreeBSD-syslogd-man-14.snap20261019150821~2$k6xtent4.pkg", "FreeBSD-syslogd-man", "14.snap20261019150821"},
		{"nohash-1.0.pkg", "", ""},
	}
	for _, tt := range tests {
		name, version := parseFreeBSDPackage(tt.filename)
		if name != tt.name || version != tt.version {
			t.Errorf("parseFreeBSDPackage(%q) = %q, %q, want %q, %q", tt.filename, name, version, tt.name, tt.version)
		}
	}
}

func TestFreeBSDHandler_Routes(t *testing.T) {
	h := NewFreeBSDHandler(nil, "http://localhost:8080", nil)
	assertRoutesBasics(t, h.Routes(), "/freebsd"+freebsdABI+"/latest/meta.conf", "/freebsd"+freebsdABI+"/../../../etc/passwd")
}

func TestFreeBSDHandler_DefaultsToOfficialMirror(t *testing.T) {
	h := NewFreeBSDHandler(nil, "http://localhost:8080", nil)
	if got := h.repositories[defaultFreeBSDRepositoryName]; got != defaultFreeBSDUpstream {
		t.Errorf("default repository = %q, want %q", got, defaultFreeBSDUpstream)
	}
}

func TestFreeBSDHandler_UnknownRepositoryReturns404(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	h := NewFreeBSDHandler(proxy, "http://localhost:8080", map[string]string{"freebsd": "https://example.test"})
	for _, target := range []string{"/unknown" + freebsdABI + "/latest/meta.conf", "/freebsd", "/"} {
		if w := serveFreeBSDRequest(h, target); w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", target, w.Code)
		}
	}
}

// fakeFreeBSDMirror serves files and counts requests per path. It answers
// 503 for everything while down is set.
type fakeFreeBSDMirror struct {
	*httptest.Server
	down     atomic.Bool
	mu       sync.Mutex
	requests map[string]int
}

func newFakeFreeBSDMirror(t *testing.T, files map[string]string) *fakeFreeBSDMirror {
	t.Helper()
	m := &fakeFreeBSDMirror{requests: map[string]int{}}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.down.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		m.mu.Lock()
		m.requests[r.URL.Path]++
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *fakeFreeBSDMirror) count(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests[path]
}

func TestFreeBSDHandler_CataloguesServedUnchanged(t *testing.T) {
	files := map[string]string{
		freebsdABI + "/latest/meta.conf":       "version = 2;\npacking_format = \"tzst\";\n",
		freebsdABI + "/latest/packagesite.pkg": "\x28\xb5\x2f\xfdpackagesite.yaml and its signature",
		freebsdABI + "/latest/data.pkg":        "\x28\xb5\x2f\xfddata and its signature",
	}
	mirror := newFakeFreeBSDMirror(t, files)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = mirror.Client()
	h := NewFreeBSDHandler(proxy, "http://proxy.example", map[string]string{"freebsd": mirror.URL})

	for round, down := range []bool{false, true} {
		mirror.down.Store(down)
		for path, want := range files {
			w := serveFreeBSDRequest(h, "/freebsd"+path)
			if w.Code != http.StatusOK || w.Body.String() != want {
				t.Fatalf("round %d %s: status = %d, body %q, want 200 and the upstream bytes", round, path, w.Code, w.Body)
			}
		}
	}
	for path := range files {
		if got := mirror.count(path); got != 1 {
			t.Errorf("%s fetched %d times, want 1", path, got)
		}
	}
}

func TestFreeBSDHandler_HashedPackagesCachedPerABI(t *testing.T) {
	const file = "/latest/All/Hashed/zsh-5.9_5~2$3rwh1rgp.pkg"
	files := map[string]string{
		"/FreeBSD:14:amd64" + file:   "amd64 package",
		"/FreeBSD:14:aarch64" + file: "aarch64 package",
	}
	mirror := newFakeFreeBSDMirror(t, files)
	proxy, db, _, _ := setupTestProxy(t)
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(mirror.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	h := NewFreeBSDHandler(proxy, "http://proxy.example", map[string]string{"freebsd": mirror.URL})

	for round, down := range []bool{false, true} {
		mirror.down.Store(down)
		for path, want := range files {
			w := serveFreeBSDRequest(h, "/freebsd"+path)
			if w.Code != http.StatusOK || w.Body.String() != want {
				t.Fatalf("round %d %s: status = %d, body %q, want 200 and %q", round, path, w.Code, w.Body, want)
			}
		}
	}
	for path := range files {
		if got := mirror.count(path); got != 1 {
			t.Errorf("%s fetched %d times, want 1", path, got)
		}
	}

	record, err := db.GetArtifact("pkg:bsd/freebsd/zsh@5.9_5", "freebsd/FreeBSD:14:amd64"+file)
	if err != nil || record == nil {
		t.Errorf("artifact record for pkg:bsd/freebsd/zsh@5.9_5 = %v, %v", record, err)
	}
}

// Unhashed packages can be rebuilt under the same name, and the bootstrap
// files under Latest/ change with every build, so none of them are cached.
func TestFreeBSDHandler_UnhashedFilesPassedThrough(t *testing.T) {
	files := map[string]string{
		freebsdABI + "/latest/Latest/pkg.pkg":     "bootstrap package",
		freebsdABI + "/latest/Latest/pkg.pkg.sig": "bootstrap signature",
		freebsdABI + "/latest/All/zsh-5.9_5.pkg":  "unhashed package",
	}
	mirror := newFakeFreeBSDMirror(t, files)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.HTTPClient = mirror.Client()
	h := NewFreeBSDHandler(proxy, "http://proxy.example", map[string]string{"freebsd": mirror.URL})

	for range 2 {
		for path, want := range files {
			w := serveFreeBSDRequest(h, "/freebsd"+path)
			if w.Code != http.StatusOK || w.Body.String() != want {
				t.Fatalf("%s: status = %d, body %q, want 200 and %q", path, w.Code, w.Body, want)
			}
		}
	}
	for path := range files {
		if got := mirror.count(path); got != 2 {
			t.Errorf("%s fetched %d times, want 2 (not cached)", path, got)
		}
	}
}

func serveFreeBSDRequest(h *FreeBSDHandler, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

func TestFreeBSDRetentionKey(t *testing.T) {
	tests := []struct {
		key  string
		want string
		ok   bool
	}{
		{"pkg:bsd/freebsd/zsh", "pkg:bsd/freebsd/zsh", true},
		{"pkg:bsd/FreeBSD/FreeBSD-acct", "pkg:bsd/freebsd/FreeBSD-acct", true},
		{"pkg:bsd/freebsd/libstdc++", "pkg:bsd/freebsd/libstdc%2B%2B", true},
		{"pkg:bsd/netbsd/zsh", "", false},
		{"pkg:bsd/zsh", "", false},
		{"pkg:bsd/freebsd/latest%2Fzsh", "", false},
		{"pkg:apk/freebsd/zsh", "", false},
	}
	for _, tt := range tests {
		p, err := purl.Parse(tt.key)
		if err != nil {
			t.Fatalf("Parse(%s): %v", tt.key, err)
		}
		got, ok := freebsdRetentionKey(p)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("freebsdRetentionKey(%s) = %q, %v, want %q, %v", tt.key, got, ok, tt.want, tt.ok)
		}
	}
	if _, ok := freebsdRetentionKey(nil); ok {
		t.Error("freebsdRetentionKey(nil) accepted")
	}
}
