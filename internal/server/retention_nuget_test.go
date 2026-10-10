package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/mirror"
	"github.com/git-pkgs/proxy/internal/retention"
	"github.com/git-pkgs/registries/fetch"
)

const (
	nugetJSONPath    = "/v3-flatcontainer/newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg"
	nugetSerilogPath = "/v3-flatcontainer/serilog/3.1.1/serilog.3.1.1.nupkg"
)

// nugetRetentionUpstream serves packages the way the NuGet flat container
// does, counting downloads.
func nugetRetentionUpstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var downloads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case nugetJSONPath, nugetSerilogPath:
			downloads.Add(1)
			_, _ = io.WriteString(w, "nupkg bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func newNuGetRetentionProxy(t *testing.T, upstream *httptest.Server) (*httptest.Server, *database.DB) {
	t.Helper()
	db, store := setupEvictionTest(t)
	// The upstream is a plain-HTTP loopback server; the default fetcher
	// refuses loopback, a stock client reaches it.
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(upstream.Client()), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())
	proxy.HTTPClient = upstream.Client()
	h := handler.NewNuGetHandlerWithUpstreams(proxy, "http://proxy.test", upstream.URL, upstream.URL)
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv, db
}

func nugetGet(t *testing.T, url string) {
	t.Helper()
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

// nugetArtifact returns the record of the package file named filename.
func nugetArtifact(t *testing.T, db *database.DB, filename string) database.Artifact {
	t.Helper()
	var art database.Artifact
	if err := db.Get(&art, db.Rebind(`SELECT * FROM artifacts WHERE filename = ?`), filename); err != nil {
		t.Fatalf("reading artifact %s: %v", filename, err)
	}
	return art
}

func nugetBackdate(t *testing.T, db *database.DB, filename string, fetched, accessed time.Time) {
	t.Helper()
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ?, last_accessed_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, fetched, accessed, filename); err != nil {
		t.Fatalf("backdating %s: %v", filename, err)
	}
}

func TestNuGetRegistersForRetention(t *testing.T) {
	if _, ok := retention.Default.Lookup("nuget"); !ok {
		t.Fatal("nuget is not registered for retention")
	}
	if key, ok := retention.Default.KeyForDBEcosystem("nuget"); !ok || key != "nuget" {
		t.Errorf("KeyForDBEcosystem(nuget) = %q, %v, want nuget, true", key, ok)
	}
}

// A package served from the cache stays while it is used, is evicted once
// its rule runs out, comes back from upstream on the next download, and
// stays after that even with the evicted copy's old access time on the
// record.
func TestNuGetRetentionEvictsAndRefetches(t *testing.T) {
	upstream, downloads := nugetRetentionUpstream(t)
	srv, db := newNuGetRetentionProxy(t, upstream)
	const nupkg = "serilog.3.1.1.nupkg"

	nugetGet(t, srv.URL+nugetSerilogPath)
	nugetGet(t, srv.URL+nugetSerilogPath)
	if got := downloads.Load(); got != 1 {
		t.Fatalf("upstream downloads = %d, want 1", got)
	}
	if !nugetArtifact(t, db, nupkg).LastAccessedAt.Valid {
		t.Fatal("cache hit did not record an access")
	}

	rules := retention.Rules{Ecosystems: map[string]time.Duration{"nuget": retentionDay}}
	now := time.Now()
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if !nugetArtifact(t, db, nupkg).StoragePath.Valid {
		t.Fatal("package used today was evicted")
	}

	old := now.Add(-2 * retentionDay)
	nugetBackdate(t, db, nupkg, old, old)
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, now)
	if nugetArtifact(t, db, nupkg).StoragePath.Valid {
		t.Fatal("package unused for two days kept under a one-day rule")
	}

	nugetGet(t, srv.URL+nugetSerilogPath)
	if got := downloads.Load(); got != 2 {
		t.Errorf("upstream downloads = %d, want 2 after the eviction", got)
	}
	art := nugetArtifact(t, db, nupkg)
	nugetBackdate(t, db, nupkg, art.FetchedAt.Time, now.Add(-30*retentionDay))
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if !nugetArtifact(t, db, nupkg).StoragePath.Valid {
		t.Error("refetched package evicted again by the next sweep")
	}
}

// A key with a namespace parses as a nuget PURL but can never match a
// stored package, so it is refused at validation. A plain key is accepted.
func TestNuGetRetentionRefusesNamespacedKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Retention.Packages = map[string]string{"pkg:nuget/Serilog": "0"}
	if _, err := cfg.RetentionRules(retention.Default); err != nil {
		t.Fatalf("RetentionRules(pkg:nuget/Serilog): %v", err)
	}
	cfg.Storage.Retention.Packages = map[string]string{"pkg:nuget/serilog/serilog": "0"}
	_, err := cfg.RetentionRules(retention.Default)
	if err == nil || !strings.Contains(err.Error(), "does not match how nuget packages are stored") {
		t.Errorf("RetentionRules(pkg:nuget/serilog/serilog) error = %v, want the key refused", err)
	}
}

// The handler stores package ids lowercased, and a package rule reaches the
// package whatever case its id is written in.
func TestNuGetRetentionPackageRules(t *testing.T) {
	upstream, _ := nugetRetentionUpstream(t)
	srv, db := newNuGetRetentionProxy(t, upstream)
	const newtonsoft, serilog = "newtonsoft.json.13.0.3.nupkg", "serilog.3.1.1.nupkg"

	// The id in the URL is upper case here; nuget clients lowercase it, but
	// the handler stores it lowercased either way.
	nugetGet(t, srv.URL+"/v3-flatcontainer/Newtonsoft.Json/13.0.3/newtonsoft.json.13.0.3.nupkg")
	var stored string
	art := nugetArtifact(t, db, newtonsoft)
	if err := db.Get(&stored, db.Rebind(`SELECT package_purl FROM versions WHERE purl = ?`), art.VersionPURL); err != nil {
		t.Fatalf("reading stored package PURL: %v", err)
	}
	if stored != "pkg:nuget/newtonsoft.json" {
		t.Errorf("stored package PURL = %q, want pkg:nuget/newtonsoft.json", stored)
	}

	for _, key := range []string{"pkg:nuget/Newtonsoft.Json", "pkg:nuget/newtonsoft.json"} {
		nugetGet(t, srv.URL+nugetJSONPath)
		nugetGet(t, srv.URL+nugetSerilogPath)
		old := time.Now().Add(-30 * retentionDay)
		nugetBackdate(t, db, newtonsoft, old, old)
		nugetBackdate(t, db, serilog, old, old)

		cfg := config.Default()
		cfg.Storage.Retention.Ecosystems = map[string]string{"nuget": "1d"}
		cfg.Storage.Retention.Packages = map[string]string{key: "0"}
		rules, err := cfg.RetentionRules(retention.Default)
		if err != nil {
			t.Fatalf("RetentionRules(%s): %v", key, err)
		}
		sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())

		if !nugetArtifact(t, db, newtonsoft).StoragePath.Valid {
			t.Errorf("with %s: Newtonsoft.Json evicted despite its never-evict rule", key)
		}
		if nugetArtifact(t, db, serilog).StoragePath.Valid {
			t.Errorf("with %s: Serilog kept past the nuget rule", key)
		}
	}
}

// nugetRewriteTransport sends every request to the test upstream, so the
// mirror command's fixed api.nuget.org URLs reach it.
type nugetRewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (r nugetRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = r.target.Scheme
	req.URL.Host = r.target.Host
	req.Host = r.target.Host
	return r.base.RoundTrip(req)
}

// A package preloaded with the mirror command falls under the nuget rule.
func TestNuGetRetentionCoversMirroredPackages(t *testing.T) {
	upstream, downloads := nugetRetentionUpstream(t)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	db, store := setupEvictionTest(t)
	client := &http.Client{Transport: nugetRewriteTransport{target: target, base: upstream.Client().Transport}}
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(client), fetch.WithMaxRetries(0))
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy := handler.NewProxy(db, store, fetcher, fetch.NewResolver(), discardLogger())

	m := mirror.New(proxy, db, store, discardLogger(), 1)
	progress, err := m.Run(context.Background(), &mirror.PURLSource{PURLs: []string{"pkg:nuget/serilog@3.1.1"}})
	if err != nil || progress.Failed != 0 || downloads.Load() != 1 {
		t.Fatalf("mirror run = %+v, %v, downloads %d; want one successful download", progress, err, downloads.Load())
	}

	const nupkg = "serilog.3.1.1.nupkg"
	if nugetArtifact(t, db, nupkg).LastAccessedAt.Valid {
		t.Fatal("mirrored package has an access recorded before anyone downloaded it")
	}
	query := db.Rebind(`UPDATE artifacts SET fetched_at = ? WHERE filename = ?`)
	if _, err := db.Exec(query, time.Now().Add(-30*retentionDay), nupkg); err != nil {
		t.Fatalf("backdating %s: %v", nupkg, err)
	}
	rules := retention.Rules{Ecosystems: map[string]time.Duration{"nuget": retentionDay}}
	sweepRetention(context.Background(), db, discardLogger(), retention.Default, rules, time.Now())
	if nugetArtifact(t, db, nupkg).StoragePath.Valid {
		t.Error("never-served mirrored package kept past the nuget rule")
	}
}
