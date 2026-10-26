package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
)

// helmUpstream serves an HTTP Helm repository listing demo 1.0.0 at a
// relative URL and demo 2.0.0 in an OCI registry, and the 1.0.0 chart.
type helmUpstream struct {
	*httptest.Server
	chart     string
	digest    string
	chartHits atomic.Int32
}

func newHelmUpstream(t *testing.T) *helmUpstream {
	t.Helper()
	u := &helmUpstream{chart: "demo chart archive"}
	sum := sha256.Sum256([]byte(u.chart))
	u.digest = hex.EncodeToString(sum[:])
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.yaml":
			_, _ = fmt.Fprintf(w, `apiVersion: v1
entries:
  demo:
    - name: demo
      version: 2.0.0
      digest: %s
      urls: [oci://ghcr.io/owner/demo:2.0.0]
    - name: demo
      version: 1.0.0
      digest: %s
      urls: [charts/demo-1.0.0.tgz]
`, strings.Repeat("b", 64), u.digest)
		case "/charts/demo-1.0.0.tgz":
			u.chartHits.Add(1)
			_, _ = io.WriteString(w, u.chart)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func helmMirrorConfig(upstream *helmUpstream) *config.Config {
	cfg := config.Default()
	cfg.Upstream.Helm = map[string]string{"stable": upstream.URL}
	return cfg
}

func TestMirrorHelmChartIsServedFromCache(t *testing.T) {
	upstream := newHelmUpstream(t)
	cfg := helmMirrorConfig(upstream)

	progress, proxy := mirrorWithConfig(t, cfg, "pkg:helm/stable/demo@1.0.0")

	if progress.Completed != 1 || upstream.chartHits.Load() != 1 {
		t.Fatalf("completed = %d, chart hits = %d, errors = %v", progress.Completed, upstream.chartHits.Load(), progress.Errors)
	}

	// helm reads the index and then the chart it points to, both from the
	// cache once the repository is down.
	upstream.Close()
	helm := handler.NewHelmHandler(proxy, "http://proxy.test", cfg.Upstream.Helm).Routes()
	chartPath := "/stable/charts/" + upstream.digest + "/demo-1.0.0.tgz"
	rec := httptest.NewRecorder()
	helm.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stable/index.yaml", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "http://proxy.test/helm"+chartPath) {
		t.Fatalf("index from cache: status = %d, body %q; want 200 listing %s", rec.Code, rec.Body.String(), chartPath)
	}
	rec = httptest.NewRecorder()
	helm.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, chartPath, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != upstream.chart {
		t.Errorf("chart from cache: status = %d, body %q; want 200 and the mirrored chart", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

func TestMirrorHelmListsVersions(t *testing.T) {
	upstream := newHelmUpstream(t)

	progress, _ := mirrorWithConfig(t, helmMirrorConfig(upstream), "pkg:helm/stable/demo")

	// 2.0.0 is in an OCI registry and refused.
	if progress.Total != 2 || progress.Completed != 1 || progress.Failed != 1 || upstream.chartHits.Load() != 1 {
		t.Errorf("total = %d, completed = %d, failed = %d, chart hits = %d, errors = %v; want both versions listed and 1.0.0 mirrored",
			progress.Total, progress.Completed, progress.Failed, upstream.chartHits.Load(), progress.Errors)
	}
}

func TestMirrorHelmRefusesDigestMismatch(t *testing.T) {
	upstream := newHelmUpstream(t)
	want := upstream.chart
	upstream.chart = "tampered chart archive"
	cfg := helmMirrorConfig(upstream)

	progress, proxy := mirrorWithConfig(t, cfg, "pkg:helm/stable/demo@1.0.0")

	if progress.Failed != 1 || progress.Completed != 0 {
		t.Fatalf("completed = %d, failed = %d, errors = %v; want the chart refused", progress.Completed, progress.Failed, progress.Errors)
	}
	sum := sha256.Sum256([]byte(want))
	upstream.Close()
	rec := httptest.NewRecorder()
	handler.NewHelmHandler(proxy, "http://proxy.test", cfg.Upstream.Helm).Routes().ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/stable/charts/"+hex.EncodeToString(sum[:])+"/demo-1.0.0.tgz", nil))
	if rec.Code == http.StatusOK {
		t.Error("the tampered chart was cached")
	}
}

func TestMirrorHelmRefusals(t *testing.T) {
	tests := []struct {
		name, purl, want string
	}{
		{"OCI chart", "pkg:helm/stable/demo@2.0.0", "pkg:oci/demo@2.0.0?repository_url=ghcr.io/owner/demo"},
		{"unknown repository", "pkg:helm/other/demo@1.0.0", `unknown Helm repository "other": upstream.helm configures stable`},
		{"no repository", "pkg:helm/demo@1.0.0", "pkg:helm/{repository}/{chart}@{version}"},
		{"unknown version", "pkg:helm/stable/demo@3.0.0", "not in the stable index"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := newHelmUpstream(t)

			progress, _ := mirrorWithConfig(t, helmMirrorConfig(upstream), tt.purl)

			if progress.Failed != 1 || len(progress.Errors) != 1 || !strings.Contains(progress.Errors[0].Error, tt.want) {
				t.Errorf("failed = %d, errors = %v; want an error mentioning %q", progress.Failed, progress.Errors, tt.want)
			}
			if upstream.chartHits.Load() != 0 {
				t.Error("a chart was fetched")
			}
		})
	}
}
