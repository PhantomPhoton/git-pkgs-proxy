package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/registries/fetch"
)

// helmChart is a chart archive of a Helm repository, addressed by digest.
type helmChart struct {
	filename string
	content  string
}

func (c helmChart) digest() string {
	sum := sha256.Sum256([]byte(c.content))
	return hex.EncodeToString(sum[:])
}

var (
	helmNginxChart = helmChart{"nginx-15.0.0.tgz", "nginx chart"}
	helmRedisChart = helmChart{"redis-18.0.0.tgz", "redis chart"}
)

// helmRetentionUpstream serves an index.yaml listing both charts and the
// chart archives the way an HTTP Helm repository does, counting downloads.
func helmRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	charts := []helmChart{helmNginxChart, helmRedisChart}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.yaml" {
			var index strings.Builder
			index.WriteString("apiVersion: v1\nentries:\n")
			for _, c := range charts {
				name := strings.SplitN(c.filename, "-", 2)[0]
				_, _ = fmt.Fprintf(&index, "  %s:\n    - digest: %s\n      urls: [%s]\n", name, c.digest(), c.filename)
			}
			_, _ = io.WriteString(w, index.String())
			return
		}
		for _, c := range charts {
			if r.URL.Path == "/"+c.filename {
				downloads.Add(1)
				_, _ = io.WriteString(w, c.content)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

// newHelmRetentionProxy mounts the upstream as the repositories "stable"
// and "internal".
func newHelmRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream is a plain-HTTP loopback server; the default fetcher
	// refuses loopback, a stock client reaches it.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewHelmHandler(proxy, "http://proxy.test", map[string]string{"stable": upstream.URL, "internal": upstream.URL})
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func helmGet(t *testing.T, srv *httptest.Server, repository string, c helmChart) {
	t.Helper()
	url := srv.URL + "/" + repository + "/charts/" + c.digest() + "/" + c.filename
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d %s, want 200", url, resp.StatusCode, body)
	}
}

// helmArtifact returns the record of the chart cached for repository.
func helmArtifact(t *testing.T, db *database.DB, repository string, c helmChart) database.Artifact {
	t.Helper()
	var art database.Artifact
	query := db.Rebind(`SELECT a.* FROM artifacts a JOIN versions v ON v.purl = a.version_purl
		WHERE a.filename = ? AND v.package_purl = ?`)
	if err := db.Get(&art, query, c.filename, "pkg:helm/"+repository); err != nil {
		t.Fatalf("reading %s from %s: %v", c.filename, repository, err)
	}
	return art
}

func helmBackdate(t *testing.T, db *database.DB, c helmChart, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, c.filename); err != nil {
		t.Fatalf("backdating %s: %v", c.filename, err)
	}
}

func TestHelmRegistersForRetention(t *testing.T) {
	spec, ok := retention.Default.Lookup("helm")
	if !ok {
		t.Fatal("helm is not registered for retention")
	}
	if spec.CanonicalPackage != nil {
		t.Error("helm accepts package rules, but its records are keyed by repository, not chart")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("helm"); !ok || key != "helm" {
		t.Errorf("KeyForDBEcosystem(helm) = %q, %v, want helm, true", key, ok)
	}
}

// A chart served from the cache stays while it is used, is evicted once its
// rule runs out, comes back from upstream on the next download, and stays
// after that even with the evicted copy's old access time on the record.
func TestHelmRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := helmRetentionUpstream(t)
	srv, db := newHelmRetentionProxy(t, upstream)

	helmGet(t, srv, "stable", helmNginxChart)
	helmGet(t, srv, "stable", helmNginxChart)
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1", got)
	}
	if !helmArtifact(t, db, "stable", helmNginxChart).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"helm": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !helmArtifact(t, db, "stable", helmNginxChart).StoragePath.Valid {
		t.Fatal("chart used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	helmBackdate(t, db, helmNginxChart, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if helmArtifact(t, db, "stable", helmNginxChart).StoragePath.Valid {
		t.Fatal("chart unused for two days kept under a one-day rule")
	}

	helmGet(t, srv, "stable", helmNginxChart)
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after the eviction", got)
	}
	art := helmArtifact(t, db, "stable", helmNginxChart)
	helmBackdate(t, db, helmNginxChart, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !helmArtifact(t, db, "stable", helmNginxChart).StoragePath.Valid {
		t.Error("refetched chart evicted again by the next sweep")
	}
}

// Package rules for helm are refused, while the helm rule and the default
// both reach charts from every configured repository.
func TestHelmRetentionEcosystemRuleAndDefault(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{"pkg:helm/nginx": "0"}
	// Any refusal naming helm will do, except the one for ecosystems that
	// aren't registered at all.
	if _, err := cfg.RetentionRules(retention.Default); err == nil || !strings.Contains(err.Error(), "helm") ||
		strings.Contains(err.Error(), "in this version") {
		t.Errorf("RetentionRules(pkg:helm/nginx) error = %v, want package rules refused", err)
	}

	upstream, _ := helmRetentionUpstream(t)
	srv, db := newHelmRetentionProxy(t, upstream)
	for _, rules := range []retention.Rules{
		{Ecosystems: map[string]time.Duration{"helm": retentionDay}},
		{Default: retentionDay},
	} {
		helmGet(t, srv, "stable", helmNginxChart)
		helmGet(t, srv, "internal", helmRedisChart)
		old := time.Now().Add(-30 * retentionDay)
		helmBackdate(t, db, helmNginxChart, old, old)
		helmBackdate(t, db, helmRedisChart, old, old)
		sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

		if helmArtifact(t, db, "stable", helmNginxChart).StoragePath.Valid {
			t.Errorf("with %+v: nginx chart kept", rules)
		}
		if helmArtifact(t, db, "internal", helmRedisChart).StoragePath.Valid {
			t.Errorf("with %+v: redis chart kept", rules)
		}
	}
}
