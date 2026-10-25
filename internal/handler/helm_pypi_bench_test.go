package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// benchHelmIndex builds an index.yaml shaped like a large public chart
// repository: many charts, each with many releases.
func benchHelmIndex(charts, releases int) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nentries:\n")
	for c := range charts {
		fmt.Fprintf(&b, "  chart-%d:\n", c)
		for r := range releases {
			fmt.Fprintf(&b, `    - apiVersion: v2
      appVersion: 1.%d.0
      created: "2021-01-02T03:04:05.123456789Z"
      dependencies:
        - name: common
          repository: oci://registry.example/charts
          version: 2.x.x
      description: A chart for benchmarking the index rewrite with a realistic amount of text.
      digest: %064x
      home: https://example.com/chart-%d
      keywords: [web, server, benchmark]
      maintainers:
        - name: Maintainer
          url: https://example.com
      name: chart-%d
      sources:
        - https://github.com/example/chart-%d
      urls:
        - https://charts.example/chart-%d-1.%d.0.tgz
      version: 1.%d.0
`, r, c*releases+r, c, c, c, c, r, r)
		}
	}
	b.WriteString("generated: \"2021-01-02T03:04:05Z\"\n")
	return b.String()
}

func benchServe(b *testing.B, h http.Handler, target, accept string) {
	b.Helper()
	b.ReportAllocs()
	for b.Loop() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}
}

// BenchmarkHelmIndexRequest serves a cached 2.5 MB index repeatedly.
func BenchmarkHelmIndexRequest(b *testing.B) {
	index := benchHelmIndex(250, 20)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-yaml")
		_, _ = w.Write([]byte(index))
	}))
	defer upstream.Close()
	proxy, _, _, _ := setupTestProxy(b)
	proxy.HTTPClient = upstream.Client()
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.SetMetadataRewriteCacheSize(256 << 20)
	h := NewHelmHandler(proxy, "http://proxy.example", map[string]string{"stable": upstream.URL}).Routes()
	b.SetBytes(int64(len(index)))
	benchServe(b, h, "/stable/index.yaml", "")
}

// benchPyPIProject builds simple HTML, simple JSON and JSON API documents
// for a project with many files, like botocore.
func benchPyPIProject(versions, filesPerVersion int) (simpleHTML, simpleJSON, jsonAPI string) {
	var hb, sb, jb strings.Builder
	hb.WriteString("<!DOCTYPE html>\n<html>\n  <head>\n    <meta name=\"pypi:repository-version\" content=\"1.1\">\n    <title>Links for big</title>\n  </head>\n  <body>\n    <h1>Links for big</h1>\n")
	sb.WriteString(`{"meta":{"api-version":"1.1"},"name":"big","files":[`)
	jb.WriteString(`{"info":{"name":"big","version":"1.0.0"},"releases":{`)
	n := 0
	for v := range versions {
		version := fmt.Sprintf("1.%d.0", v)
		if v > 0 {
			jb.WriteByte(',')
		}
		fmt.Fprintf(&jb, "%q:[", version)
		for f := range filesPerVersion {
			filename := fmt.Sprintf("big-%s-py3-none-any%d.whl", version, f)
			if f == 1 {
				filename = fmt.Sprintf("big-%s.tar.gz", version)
			}
			url := fmt.Sprintf("https://files.pythonhosted.org/packages/%02x/%02x/%060x/%s", v%256, f, v*filesPerVersion+f, filename)
			fmt.Fprintf(&hb, "    <a href=\"%s#sha256=%064x\" data-requires-python=\"&gt;= 3.8\" data-dist-info-metadata=\"sha256=%064x\" data-core-metadata=\"sha256=%064x\">%s</a><br />\n", url, n, n, n, filename)
			if n > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `{"filename":%q,"url":%q,"hashes":{"sha256":"%064x"},"requires-python":">=3.8","core-metadata":{"sha256":"%064x"},"size":12345,"upload-time":"2021-01-02T03:04:05.123456Z","yanked":false}`, filename, url, n, n)
			if f > 0 {
				jb.WriteByte(',')
			}
			fmt.Fprintf(&jb, `{"filename":%q,"url":%q,"digests":{"sha256":"%064x","md5":"%032x"},"packagetype":"bdist_wheel","python_version":"py3","size":12345,"upload_time_iso_8601":"2021-01-02T03:04:05.123456Z","yanked":false}`, filename, url, n, n)
			n++
		}
		jb.WriteByte(']')
	}
	hb.WriteString("  </body>\n</html>\n<!--SERIAL 1-->\n")
	sb.WriteString("]}")
	jb.WriteString(`},"urls":[]}`)
	return hb.String(), sb.String(), jb.String()
}

func benchPyPIHandler(b *testing.B, deny ...string) http.Handler {
	b.Helper()
	simpleHTML, simpleJSON, jsonAPI := benchPyPIProject(2000, 2)
	h, proxy := setupPyPIHandler(b, func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/pypi/big/json":
			return pypiHTTPResponse(r, "application/json", jsonAPI), nil
		case r.Header.Get("Accept") == pypiSimpleJSON:
			return pypiHTTPResponse(r, pypiSimpleJSON, simpleJSON), nil
		default:
			return pypiHTTPResponse(r, "text/html", simpleHTML), nil
		}
	})
	proxy.CacheMetadata = true
	proxy.MetadataTTL = time.Hour
	proxy.SetMetadataRewriteCacheSize(256 << 20)
	if len(deny) > 0 {
		setTestDenylist(b, proxy, deny...)
	}
	return h.Routes()
}

func BenchmarkPyPISimpleHTMLRequest(b *testing.B) {
	benchServe(b, benchPyPIHandler(b), "/simple/big/", "")
}

// BenchmarkPyPISimpleHTMLFilteredRequest has a version denied, so the page
// is filtered as well as rewritten.
func BenchmarkPyPISimpleHTMLFilteredRequest(b *testing.B) {
	benchServe(b, benchPyPIHandler(b, "pkg:pypi/big@1.5.0"), "/simple/big/", "")
}

func BenchmarkPyPISimpleJSONRequest(b *testing.B) {
	benchServe(b, benchPyPIHandler(b), "/simple/big/", pypiSimpleJSON)
}

func BenchmarkPyPIJSONAPIRequest(b *testing.B) {
	benchServe(b, benchPyPIHandler(b), "/pypi/big/json", "")
}

// BenchmarkPyPIRewriteSimpleHTML is the rewrite a cache miss pays.
func BenchmarkPyPIRewriteSimpleHTML(b *testing.B) {
	simpleHTML, _, _ := benchPyPIProject(2000, 2)
	body := []byte(simpleHTML)
	h := NewPyPIHandlerWithUpstreams(testProxy(), "http://proxy.example", "", "")
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		_ = h.rewriteSimpleHTML(body, nil)
	}
}

// BenchmarkPyPIRewriteSimpleHTMLFiltered is the same with a version filtered
// out.
func BenchmarkPyPIRewriteSimpleHTMLFiltered(b *testing.B) {
	simpleHTML, _, _ := benchPyPIProject(2000, 2)
	body := []byte(simpleHTML)
	h := NewPyPIHandlerWithUpstreams(testProxy(), "http://proxy.example", "", "")
	filtered := map[string]bool{"1.5.0": true}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		_ = h.rewriteSimpleHTML(h.filterSimpleHTMLLinks(body, filtered), filtered)
	}
}
