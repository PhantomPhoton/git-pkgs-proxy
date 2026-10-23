package handler

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/scanner"
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

func TestParseFreeBSDPackageFile(t *testing.T) {
	tests := []struct {
		filename, name, version string
		pkgfile                 bool
	}{
		{"zsh-5.9_5~2$3rwh1rgp.pkg", "zsh", "5.9_5", true},
		{"zsh-5.9_5.pkg", "zsh", "5.9_5", true},
		{"zsh-navigation-tools-2.2.7.pkg", "zsh-navigation-tools", "2.2.7", true},
		{"py311-requests-2.32.3_1,1.txz", "py311-requests", "2.32.3_1,1", true},
		{"FreeBSD-acct-15.snap20261021214449.tzst", "FreeBSD-acct", "15.snap20261021214449", true},
		{"pkg.pkg", "", "", true},
		{"pkg.txz", "", "", true},
		{"pkg.pkg.sig", "", "", false},
		{"pkg.pkg.pub", "", "", false},
		{"zsh-5.9_5.pkg.sig", "", "", false},
		{"zsh-5.9_5.tar", "", "", false},
	}
	for _, tt := range tests {
		name, version := parseFreeBSDPackageFile(tt.filename)
		if name != tt.name || version != tt.version {
			t.Errorf("parseFreeBSDPackageFile(%q) = %q, %q, want %q, %q", tt.filename, name, version, tt.name, tt.version)
		}
		if got := isFreeBSDPackageFile("FreeBSD:14:amd64/latest/All/" + tt.filename); got != tt.pkgfile {
			t.Errorf("isFreeBSDPackageFile(%q) = %v, want %v", tt.filename, got, tt.pkgfile)
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
// Without a denylist entry or a scanner they are passed through.
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

// Package files without a hash in their name are checked against the
// denylist like hashed ones, before anything is fetched.
func TestFreeBSDHandler_DenylistCoversEveryPackageFile(t *testing.T) {
	denied := []string{
		freebsdABI + "/latest/All/Hashed/zsh-5.9_5~2$3rwh1rgp.pkg",
		freebsdABI + "/latest/All/zsh-5.9_5.pkg",
		freebsdABI + "/latest/All/zsh-5.9_5~2$3rwh1rgp.pkg",
		"/142amd64-default/All/zsh-5.9_5.txz",
		"/142amd64-default/All/zsh-5.9_5.tzst",
	}
	allowed := map[string]string{
		freebsdABI + "/latest/All/zsh-5.9_4.pkg":                  "older build",
		freebsdABI + "/latest/All/zsh-navigation-tools-5.9_5.pkg": "another package",
	}
	files := map[string]string{}
	for _, path := range denied {
		files[path] = "denied package"
	}
	for path, body := range allowed {
		files[path] = body
	}
	mirror := newFakeFreeBSDMirror(t, files)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = mirror.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(mirror.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	setTestDenylist(t, proxy, "pkg:bsd/freebsd/zsh@5.9_5")
	h := NewFreeBSDHandler(proxy, "http://proxy.example", map[string]string{"freebsd": mirror.URL})

	for _, path := range denied {
		if w := serveFreeBSDRequest(h, "/freebsd"+path); w.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", path, w.Code)
		}
		if got := mirror.count(path); got != 0 {
			t.Errorf("%s fetched %d times, want 0", path, got)
		}
	}
	for path, want := range allowed {
		if w := serveFreeBSDRequest(h, "/freebsd"+path); w.Code != http.StatusOK || w.Body.String() != want {
			t.Errorf("%s: status = %d, body %q, want 200 and %q", path, w.Code, w.Body, want)
		}
	}
}

// Package files the proxy can't cache can't be scanned either, so they are
// refused while a scanner covers freebsd. Catalogues, signatures and hashed
// packages are unaffected.
func TestFreeBSDHandler_ScanningRefusesUncachablePackages(t *testing.T) {
	refused := []string{
		freebsdABI + "/latest/All/zsh-5.9_5.pkg",
		freebsdABI + "/latest/Latest/pkg.pkg",
		"/142amd64-default/Latest/pkg.txz",
		"/142amd64-default/All/zsh-5.9_5.tzst",
	}
	served := map[string]string{
		freebsdABI + "/latest/meta.conf":          "version = 2;\n",
		freebsdABI + "/latest/Latest/pkg.pkg.sig": "bootstrap signature",
		freebsdABI + "/latest/Latest/pkg.pkg.pub": "bootstrap key",
	}
	files := map[string]string{}
	for _, path := range refused {
		files[path] = "unscanned package"
	}
	for path, body := range served {
		files[path] = body
	}

	for _, ecosystems := range [][]string{nil, {"freebsd"}} {
		mirror := newFakeFreeBSDMirror(t, files)
		proxy, _, _, _ := setupTestProxy(t)
		proxy.HTTPClient = mirror.Client()
		proxy.Scanners = newFreeBSDScanGroup(t, ecosystems)
		h := NewFreeBSDHandler(proxy, "http://proxy.example", map[string]string{"freebsd": mirror.URL})

		for _, path := range refused {
			w := serveFreeBSDRequest(h, "/freebsd"+path)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "scan") {
				t.Errorf("ecosystems %v %s: status = %d, body %q, want 403 naming the scan", ecosystems, path, w.Code, w.Body)
			}
			if got := mirror.count(path); got != 0 {
				t.Errorf("ecosystems %v %s fetched %d times, want 0", ecosystems, path, got)
			}
		}
		for path, want := range served {
			if w := serveFreeBSDRequest(h, "/freebsd"+path); w.Code != http.StatusOK || w.Body.String() != want {
				t.Errorf("ecosystems %v %s: status = %d, body %q, want 200 and %q", ecosystems, path, w.Code, w.Body, want)
			}
		}
	}
}

// A scanner restricted to other ecosystems never sees FreeBSD packages, so
// unhashed files are passed through as without scanning.
func TestFreeBSDHandler_ScannerForOtherEcosystemsPassesThrough(t *testing.T) {
	const path = freebsdABI + "/latest/All/zsh-5.9_5.pkg"
	mirror := newFakeFreeBSDMirror(t, map[string]string{path: "unhashed package"})
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = mirror.Client()
	proxy.Scanners = newFreeBSDScanGroup(t, []string{"npm"})
	h := NewFreeBSDHandler(proxy, "http://proxy.example", map[string]string{"freebsd": mirror.URL})

	if w := serveFreeBSDRequest(h, "/freebsd"+path); w.Code != http.StatusOK || w.Body.String() != "unhashed package" {
		t.Errorf("status = %d, body %q, want 200 and the upstream bytes", w.Code, w.Body)
	}
}

func newFreeBSDScanGroup(t *testing.T, ecosystems []string) *scanner.Group {
	t.Helper()
	g, err := scanner.NewGroup(config.ScanningConfig{
		Enabled:    true,
		SigningKey: "test-signing-key",
		Scanners: []config.ScannerConfig{
			{Name: "test-scanner", URL: "http://scanner.invalid/scan", Ecosystems: ecosystems},
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("scanner.NewGroup() error: %v", err)
	}
	return g
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
