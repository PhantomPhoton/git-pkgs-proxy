package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const monologMetadata = `{"minified":"composer/2.0","packages":{"monolog/monolog":[
	{"name":"monolog/monolog","version":"3.5.0","version_normalized":"3.5.0.0",
	 "require":{"php":">=8.1","psr/log":"^2.0"},"support":{"issues":"https://github.com/Seldaek/monolog/issues"},
	 "dist":{"type":"zip","url":"https://api.github.com/repos/Seldaek/monolog/zipball/aaa","reference":"aaa","shasum":""},
	 "time":"2023-10-27T15:32:31+00:00"},
	{"version":"3.4.0","version_normalized":"3.4.0.0",
	 "dist":{"type":"zip","url":"https://api.github.com/repos/Seldaek/monolog/zipball/bbb","reference":"bbb","shasum":""}}
]}}`

const monologDevMetadata = `{"minified":"composer/2.0","packages":{"monolog/monolog":[
	{"name":"monolog/monolog","version":"dev-main","version_normalized":"dev-main",
	 "dist":{"type":"zip","url":"https://api.github.com/repos/Seldaek/monolog/zipball/ccc","reference":"ccc","shasum":""}}
]}}`

type composerHostedFixture struct {
	proxy   *Proxy
	handler http.Handler
}

func newComposerHostedFixture(t *testing.T, upstream map[string]string) composerHostedFixture {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := upstream[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	proxy, _, _, _ := setupTestProxy(t)
	proxy.HostedEnabled = true
	proxy.HTTPClient = srv.Client()
	h := NewComposerHandlerWithUpstreams(proxy, "http://proxy.local", srv.URL, srv.URL)
	return composerHostedFixture{proxy: proxy, handler: h.Routes()}
}

func (f composerHostedFixture) push(t *testing.T, name, version, manifest string) HostedArtifact {
	t.Helper()
	target, err := HostedTargetFor("composer", name, version)
	if err != nil {
		t.Fatalf("HostedTargetFor: %v", err)
	}
	pushed, err := f.proxy.PushLocalArtifact(context.Background(), "composer", target, bytes.NewReader(composerZip("", manifest)), "")
	if err != nil {
		t.Fatalf("PushLocalArtifact: %v", err)
	}
	return pushed
}

func (f composerHostedFixture) versions(t *testing.T, path, packageName string) map[string]map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, body %s", path, rec.Code, rec.Body)
	}
	byVersion := map[string]map[string]any{}
	for _, v := range composerVersions(t, rec.Body.Bytes(), packageName) {
		version, _ := v["version"].(string)
		if _, dup := byVersion[version]; dup {
			t.Fatalf("version %s listed twice:\n%s", version, rec.Body)
		}
		byVersion[version] = v
	}
	return byVersion
}

func TestComposerMetadataListsPushedVersion(t *testing.T) {
	f := newComposerHostedFixture(t, map[string]string{"/p2/monolog/monolog.json": monologMetadata})
	pushed := f.push(t, "monolog/monolog", "3.6.0", `{"name":"monolog/monolog","require":{"php":">=8.1","psr/log":"^3.0"},"repositories":[{"type":"vcs","url":"x"}]}`)

	versions := f.versions(t, "/p2/monolog/monolog.json", "monolog/monolog")
	local, ok := versions["3.6.0"]
	if !ok {
		t.Fatalf("pushed version missing: %v", versions)
	}
	if require, _ := local["require"].(map[string]any); require["psr/log"] != "^3.0" {
		t.Errorf("require = %v, want the pushed composer.json's", local["require"])
	}
	for _, inherited := range []string{"version_normalized", "support", "repositories"} {
		if _, ok := local[inherited]; ok {
			t.Errorf("pushed version carries %s = %v", inherited, local[inherited])
		}
	}
	dist, _ := local["dist"].(map[string]any)
	if dist["url"] != "http://proxy.local/composer/files/monolog/monolog/3.6.0/monolog-3.6.0.zip" ||
		dist["type"] != "zip" || dist["reference"] != pushed.SHA256 || dist["shasum"] != "" {
		t.Errorf("dist = %v", dist)
	}
	if local["time"] != pushed.PushedAt.Format(time.RFC3339) {
		t.Errorf("time = %v, want %s", local["time"], pushed.PushedAt.Format(time.RFC3339))
	}
	if _, ok := versions["3.4.0"]; !ok {
		t.Error("upstream version 3.4.0 missing")
	}
}

func TestComposerMetadataPushedVersionReplacesUpstream(t *testing.T) {
	f := newComposerHostedFixture(t, map[string]string{"/p2/monolog/monolog.json": monologMetadata})
	f.push(t, "monolog/monolog", "3.5.0", `{"name":"monolog/monolog","require":{"php":">=8.1","psr/log":"^3.0"}}`)

	versions := f.versions(t, "/p2/monolog/monolog.json", "monolog/monolog")
	local := versions["3.5.0"]
	if require, _ := local["require"].(map[string]any); require["psr/log"] != "^3.0" {
		t.Errorf("require = %v, want the pushed composer.json's", local["require"])
	}
	if dist, _ := local["dist"].(map[string]any); dist["url"] != "http://proxy.local/composer/files/monolog/monolog/3.5.0/monolog-3.5.0.zip" {
		t.Errorf("dist = %v", dist)
	}
	if v34 := versions["3.4.0"]; v34["version_normalized"] != "3.4.0.0" {
		t.Errorf("upstream 3.4.0 expanded to %v", v34)
	}
}

func TestComposerMetadataSplitsPushedDevVersions(t *testing.T) {
	f := newComposerHostedFixture(t, map[string]string{
		"/p2/monolog/monolog.json":     monologMetadata,
		"/p2/monolog/monolog~dev.json": monologDevMetadata,
	})
	f.push(t, "monolog/monolog", "dev-patched", `{"name":"monolog/monolog"}`)
	f.push(t, "monolog/monolog", "3.6.0", `{"name":"monolog/monolog"}`)

	tagged := f.versions(t, "/p2/monolog/monolog.json", "monolog/monolog")
	dev := f.versions(t, "/p2/monolog/monolog~dev.json", "monolog/monolog")
	if _, ok := tagged["dev-patched"]; ok {
		t.Error("pushed dev version listed with the tagged versions")
	}
	if _, ok := dev["dev-patched"]; !ok {
		t.Errorf("pushed dev version missing from the dev file: %v", dev)
	}
	if _, ok := dev["3.6.0"]; ok {
		t.Error("pushed tagged version listed with the dev versions")
	}
	if _, ok := dev["dev-main"]; !ok {
		t.Error("upstream dev-main missing")
	}
}

func TestComposerMetadataForPackageOnlyPushed(t *testing.T) {
	f := newComposerHostedFixture(t, nil)
	f.push(t, "acme/private", "1.0.0", `{"name":"acme/private","require":{"php":">=8.1"}}`)

	versions := f.versions(t, "/p2/acme/private.json", "acme/private")
	if _, ok := versions["1.0.0"]; !ok || len(versions) != 1 {
		t.Errorf("versions = %v, want only 1.0.0", versions)
	}

	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/p2/acme/private~dev.json", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("dev file of a package with no pushed dev versions status = %d, want 404", rec.Code)
	}
}
