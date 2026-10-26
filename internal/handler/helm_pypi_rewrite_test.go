package handler

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

// getRewritten requests target and checks the response status.
func getRewritten(t *testing.T, h http.Handler, target string, header http.Header, want int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("GET %s: status %d, want %d: %s", target, rec.Code, want, rec.Body.String())
	}
	return rec
}

// checkServedFromRewriteCache requests target twice more after a first
// request, with storage failing, and checks the repeat is the cached rewrite:
// same body and ETag, 304 for the ETag, gzip for a client accepting it.
func checkServedFromRewriteCache(t *testing.T, h http.Handler, store *mockStorage, target string, header http.Header) string {
	t.Helper()
	first := getRewritten(t, h, target, header, http.StatusOK)
	etag := first.Header().Get(headerETag)
	if etag == "" {
		t.Fatalf("no ETag on rewritten metadata; headers %v", first.Header())
	}

	// The repeat must not read the stored document back.
	store.mu.Lock()
	store.openErr = errors.New("storage must not be read")
	store.mu.Unlock()
	t.Cleanup(func() {
		store.mu.Lock()
		store.openErr = nil
		store.mu.Unlock()
	})

	second := getRewritten(t, h, target, header, http.StatusOK)
	if second.Body.String() != first.Body.String() || second.Header().Get(headerETag) != etag {
		t.Errorf("repeat differs: ETag %q vs %q", second.Header().Get(headerETag), etag)
	}
	if got, want := second.Header().Get(headerContentType), first.Header().Get(headerContentType); got != want {
		t.Errorf("repeat Content-Type = %q, want %q", got, want)
	}

	conditional := header.Clone()
	if conditional == nil {
		conditional = http.Header{}
	}
	conditional.Set("If-None-Match", etag)
	getRewritten(t, h, target, conditional, http.StatusNotModified)

	gzipped := header.Clone()
	if gzipped == nil {
		gzipped = http.Header{}
	}
	gzipped.Set(headerAcceptEncoding, "gzip")
	compressed := getRewritten(t, h, target, gzipped, http.StatusOK)
	if got := compressed.Header().Get(headerContentEncoding); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
	return first.Body.String()
}

func TestHelmIndexServedFromRewriteCache(t *testing.T) {
	digest := strings.Repeat("a", 64)
	var indexRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		indexRequests.Add(1)
		w.Header().Set("Content-Type", "application/x-yaml")
		_, _ = fmt.Fprintf(w, "apiVersion: v1\nentries:\n  demo:\n%s", strings.Repeat(fmt.Sprintf(
			"    - digest: %s\n      name: demo\n      urls: [demo-1.0.0.tgz]\n      version: 1.0.0\n", digest), 20))
	}))
	defer upstream.Close()

	proxy, _, store, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.SetMetadataRewriteCacheSize(1 << 20)
	h := NewHelmHandler(proxy, "http://proxy.example", map[string]string{"stable": upstream.URL}).Routes()

	body := checkServedFromRewriteCache(t, h, store, "/stable/index.yaml", nil)
	if !strings.Contains(body, "http://proxy.example/helm/stable/charts/"+digest+"/demo-1.0.0.tgz") {
		t.Errorf("index not rewritten: %s", body)
	}
	if got := indexRequests.Load(); got != 1 {
		t.Errorf("upstream requests = %d, want 1", got)
	}
	if got := proxy.rewrites.len(); got != 1 {
		t.Errorf("cached rewrites = %d, want 1", got)
	}
}

// TestHelmChartLookupFollowsIndex checks that chart downloads are looked up
// in charts kept from the last parse of the same index, and that a changed
// index is parsed again.
func TestHelmChartLookupFollowsIndex(t *testing.T) {
	chartA, chartB := []byte("chart a"), []byte("chart b")
	digestA, digestB := helmSHA256Hex(chartA), helmSHA256Hex(chartB)
	var withB atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.yaml":
			w.Header().Set("Content-Type", "application/x-yaml")
			_, _ = fmt.Fprintf(w, "apiVersion: v1\nentries:\n  a:\n    - digest: %s\n      urls: [a-1.0.0.tgz]\n", digestA)
			if withB.Load() {
				_, _ = fmt.Fprintf(w, "  b:\n    - digest: %s\n      urls: [b-1.0.0.tgz]\n", digestB)
			}
		case "/a-1.0.0.tgz":
			_, _ = w.Write(chartA)
		case "/b-1.0.0.tgz":
			_, _ = w.Write(chartB)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	h := NewHelmHandler(proxy, "http://proxy.example", map[string]string{"stable": upstream.URL})
	index := func() metadataResult {
		return h.fetchIndex(httptest.NewRequest(http.MethodGet, "/", nil), "stable", upstream.URL)
	}

	first, err := h.findChartDownload("stable", upstream.URL, index(), digestA, "a-1.0.0.tgz")
	if err != nil || first != upstream.URL+"/a-1.0.0.tgz" {
		t.Fatalf("lookup = %q, %v", first, err)
	}
	lookup := h.lookups["stable\x00"+upstream.URL]
	if lookup == nil {
		t.Fatal("chart lookup was not kept")
	}
	if _, err := h.findChartDownload("stable", upstream.URL, index(), digestB, "b-1.0.0.tgz"); !errors.Is(err, errHelmChartNotFound) {
		t.Fatalf("lookup of unlisted chart: %v", err)
	}
	if h.lookups["stable\x00"+upstream.URL] != lookup {
		t.Error("same index was parsed again")
	}

	// A new index from upstream lists a new chart.
	withB.Store(true)
	proxy.MetadataTTL = 0
	got, err := h.findChartDownload("stable", upstream.URL, index(), digestB, "b-1.0.0.tgz")
	if err != nil || got != upstream.URL+"/b-1.0.0.tgz" {
		t.Fatalf("lookup in new index = %q, %v", got, err)
	}
}

func TestPyPIMetadataServedFromRewriteCache(t *testing.T) {
	const files = "https://files.pythonhosted.org/packages/ab/cd/"
	simpleHTML := `<a href="` + files + `demo-1.0.0.tar.gz#sha256=aa">demo-1.0.0.tar.gz</a><br/>` +
		`<a href="` + files + `demo-2.0.0.tar.gz#sha256=bb">demo-2.0.0.tar.gz</a><br/>` +
		strings.Repeat("<!-- padding so the page is worth compressing -->\n", 20)
	simpleJSON := `{"meta":{"api-version":"1.1"},"name":"demo","files":[` +
		`{"filename":"demo-1.0.0.tar.gz","url":"` + files + `demo-1.0.0.tar.gz"},` +
		`{"filename":"demo-2.0.0.tar.gz","url":"` + files + `demo-2.0.0.tar.gz"}],` +
		`"padding":"` + strings.Repeat("x", 1000) + `"}`
	jsonAPI := `{"info":{"name":"demo","version":"1.0.0","description":"` + strings.Repeat("x", 1000) + `"},` +
		`"releases":{"1.0.0":[{"url":"` + files + `demo-1.0.0.tar.gz"}],"2.0.0":[{"url":"` + files + `demo-2.0.0.tar.gz"}]},` +
		`"urls":[{"url":"` + files + `demo-1.0.0.tar.gz"}]}`

	cases := []struct {
		name, target, accept string
	}{
		{"simple html", "/simple/demo/", ""},
		{"simple json", "/simple/demo/", pypiSimpleJSON},
		{"json api", "/pypi/demo/json", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			proxy, _, store, _ := setupTestProxy(t)
			proxy.HTTPClient = &http.Client{Transport: pypiRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				upstreamCalls.Add(1)
				switch {
				case r.URL.Path == "/pypi/demo/json":
					return pypiHTTPResponse(r, "application/json", jsonAPI), nil
				case r.Header.Get("Accept") == pypiSimpleJSON:
					return pypiHTTPResponse(r, pypiSimpleJSON, simpleJSON), nil
				default:
					return pypiHTTPResponse(r, "text/html", simpleHTML), nil
				}
			})}
			proxy.CacheMetadata = true
			proxy.MetadataTTL = time.Hour
			proxy.SetMetadataRewriteCacheSize(1 << 20)
			setTestDenylist(t, proxy, "pkg:pypi/demo@2.0.0")
			h := NewPyPIHandlerWithUpstreams(proxy, "http://proxy.test", "https://pypi.test", "")

			var header http.Header
			if c.accept != "" {
				header = http.Header{"Accept": {c.accept}}
			}
			body := checkServedFromRewriteCache(t, h.Routes(), store, c.target, header)
			if !strings.Contains(body, "http://proxy.test/pypi/packages/packages/ab/cd/demo-1.0.0.tar.gz") {
				t.Errorf("download URL not rewritten: %s", body)
			}
			if strings.Contains(body, "demo-2.0.0") {
				t.Errorf("denied version served: %s", body)
			}
			if got := upstreamCalls.Load(); got != 1 {
				t.Errorf("upstream calls = %d, want 1", got)
			}

			if c.target == "/simple/demo/" {
				rec := getRewritten(t, h.Routes(), c.target, header, http.StatusOK)
				vary := strings.Join(rec.Header().Values("Vary"), ", ")
				if !strings.Contains(vary, "Accept-Encoding") || !strings.Contains(strings.ReplaceAll(vary, "Accept-Encoding", ""), "Accept") {
					t.Errorf("Vary = %q, want Accept and Accept-Encoding", vary)
				}
			}
		})
	}
}

// TestPyPISimpleRewriteFollowsCooldown checks that a cached rewrite of a
// simple page is not served once the versions cooldown filters out change,
// though the page itself is the same.
func TestPyPISimpleRewriteFollowsCooldown(t *testing.T) {
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	var released atomic.Value
	released.Store(now.Add(-time.Hour).Format(time.RFC3339))

	h, proxy := setupPyPIHandler(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/simple/ruff/":
			return pypiHTTPResponse(r, "text/html",
				`<a href="https://files.pythonhosted.org/packages/ab/ruff-1.0.0.tar.gz">ruff-1.0.0.tar.gz</a>`+
					`<a href="https://files.pythonhosted.org/packages/cd/ruff-2.0.0.tar.gz">ruff-2.0.0.tar.gz</a>`), nil
		case "/pypi/ruff/json":
			return pypiHTTPResponse(r, "application/json", `{"releases":{`+
				`"1.0.0":[{"upload_time_iso_8601":"`+old+`"}],`+
				`"2.0.0":[{"upload_time_iso_8601":"`+released.Load().(string)+`"}]}}`), nil
		default:
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
			return nil, nil
		}
	})
	proxy.Cooldown = &cooldown.Config{Default: "7d"}
	proxy.SetMetadataRewriteCacheSize(1 << 20)

	first := getRewritten(t, h.Routes(), "/simple/ruff/", nil, http.StatusOK).Body.String()
	if strings.Contains(first, "ruff-2.0.0") || !strings.Contains(first, "ruff-1.0.0") {
		t.Fatalf("first page = %s, want only ruff-1.0.0", first)
	}

	released.Store(old)
	second := getRewritten(t, h.Routes(), "/simple/ruff/", nil, http.StatusOK).Body.String()
	if !strings.Contains(second, "ruff-2.0.0") {
		t.Errorf("version out of cooldown still filtered: %s", second)
	}
}

// rewriteDownloadHrefsRegexp is the regular expression rewrite that
// rewriteDownloadHrefs replaces.
func rewriteDownloadHrefsRegexp(h *PyPIHandler, body []byte) []byte {
	re := regexp.MustCompile(`href="(` + regexp.QuoteMeta(h.downloadURL) + `/packages/[^"]+)"`)
	return re.ReplaceAllFunc(body, func(match []byte) []byte {
		submatch := re.FindSubmatch(match)
		return []byte(`href="` + h.proxyURL + "/pypi/packages" + strings.TrimPrefix(string(submatch[1]), h.downloadURL) + `"`)
	})
}

func FuzzPyPIRewriteDownloadHrefs(f *testing.F) {
	const dl = "https://files.pythonhosted.org"
	for _, seed := range []string{
		`<a href="` + dl + `/packages/ab/cd/x-1.0.tar.gz#sha256=00">x</a>`,
		`<a href="` + dl + `/packages/">empty</a>`,
		`<a href="` + dl + `/packages/unterminated`,
		`<a href="` + dl + `/packages/a"` + `href="` + dl + `/packages/b">`,
		`<a href="` + dl + `/packages/` + `href="` + dl + `/packages/b">`,
		`<a href="https://elsewhere.example/packages/x.whl">x</a>`,
		`<a href='` + dl + `/packages/single-quoted.whl'>x</a>`,
		`<a href="` + dl + `/packagesx/y.whl">x</a>`,
		`href="` + dl + `/packages/` + "\n" + `multi line"`,
		`href="href="` + dl + `/packages/x"`,
	} {
		f.Add(seed)
	}
	h := NewPyPIHandlerWithUpstreams(testProxy(), "http://proxy.test", "", dl)
	f.Fuzz(func(t *testing.T, body string) {
		want := rewriteDownloadHrefsRegexp(h, []byte(body))
		if got := h.rewriteDownloadHrefs([]byte(body)); string(got) != string(want) {
			t.Errorf("rewriteDownloadHrefs(%q)\n = %q\nwant %q", body, got, want)
		}
	})
}
