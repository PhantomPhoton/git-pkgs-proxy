package handler

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/registries/fetch"
)

func TestHostedTargetForFileInPath(t *testing.T) {
	got, err := HostedTargetFor("generic", "tools", "1.2.0", "tool.tar.gz")
	if err != nil || got != (HostedTarget{Name: "tools", Version: "1.2.0", Filename: "tool.tar.gz"}) {
		t.Errorf("generic target = %+v, %v", got, err)
	}
	for _, tt := range []struct{ ecosystem, name, version, file string }{
		{"generic", "tools", "1.2.0", ""},
		{"generic", "tools", "1.2.0", ".."},
		{"generic", "acme/tools", "1.2.0", "tool.tar.gz"},
		{"npm", "request", "2.88.3", "request-2.88.3.tgz"},
	} {
		if _, err := HostedTargetFor(tt.ecosystem, tt.name, tt.version, tt.file); !errors.Is(err, ErrHostedInvalidName) {
			t.Errorf("HostedTargetFor(%q, %q, %q, %q) error = %v, want ErrHostedInvalidName",
				tt.ecosystem, tt.name, tt.version, tt.file, err)
		}
	}
	if !HostedFileInPath("generic") || HostedFileInPath("npm") || HostedFileInPath("cargo") {
		t.Error("HostedFileInPath should hold for generic only")
	}
}

func TestHostedTargetFor(t *testing.T) {
	tests := []struct {
		name                    string
		ecosystem, pkg, version string
		want                    HostedTarget
		wantErr                 error
	}{
		{name: "npm", ecosystem: "npm", pkg: "request", version: "2.88.3",
			want: HostedTarget{Name: "request", Version: "2.88.3", Filename: "request-2.88.3.tgz"}},
		{name: "npm scoped", ecosystem: "npm", pkg: "@types/node", version: "20.1.0",
			want: HostedTarget{Name: "@types/node", Version: "20.1.0", Filename: "node-20.1.0.tgz"}},
		{name: "composer", ecosystem: "composer", pkg: "monolog/monolog", version: "3.5.0",
			want: HostedTarget{Name: "monolog/monolog", Version: "3.5.0", Filename: "monolog-3.5.0.zip"}},
		{name: "composer without vendor", ecosystem: "composer", pkg: "monolog", version: "3.5.0",
			wantErr: ErrHostedInvalidName},
		{name: "composer with extra segment", ecosystem: "composer", pkg: "a/b/c", version: "1.0.0",
			wantErr: ErrHostedInvalidName},
		{name: "unsupported ecosystem", ecosystem: "cargo", pkg: "serde", version: "1.0.200",
			wantErr: ErrHostedUnsupported},
		{name: "unknown ecosystem", ecosystem: "maven", pkg: "junit", version: "4.13",
			wantErr: ErrHostedUnsupported},
		{name: "npm scope without @", ecosystem: "npm", pkg: "types/node", version: "1.0.0",
			wantErr: ErrHostedInvalidName},
		{name: "dot-dot name segment", ecosystem: "npm", pkg: "@scope/..", version: "1.0.0",
			wantErr: ErrHostedInvalidName},
		{name: "dot-dot version", ecosystem: "npm", pkg: "request", version: "..",
			wantErr: ErrHostedInvalidName},
		{name: "control character", ecosystem: "npm", pkg: "request", version: "1.0.0\n",
			wantErr: ErrHostedInvalidName},
		{name: "empty version", ecosystem: "npm", pkg: "request", version: "",
			wantErr: ErrHostedInvalidName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HostedTargetFor(tt.ecosystem, tt.pkg, tt.version, "")
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("HostedTargetFor() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("HostedTargetFor() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("HostedTargetFor() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// npmTarball builds an npm package tarball holding manifest as
// package/package.json. Building an archive in memory cannot fail, so errors
// panic.
func npmTarball(manifest string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range [][2]string{{"package/package.json", manifest}, {"package/index.js", "module.exports = 1\n"}} {
		must(tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o644, Size: int64(len(f[1]))}))
		_, err := io.WriteString(tw, f[1])
		must(err)
	}
	must(tw.Close())
	must(gz.Close())
	return buf.Bytes()
}

// composerZip builds a Composer dist archive holding manifest as composer.json
// under dir, or at the root when dir is empty.
func composerZip(dir, manifest string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	for _, f := range [][2]string{{prefix + "composer.json", manifest}, {prefix + "src/Logger.php", "<?php\n"}} {
		w, err := zw.Create(f[0])
		must(err)
		_, err = io.WriteString(w, f[1])
		must(err)
	}
	must(zw.Close())
	return buf.Bytes()
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

const requestManifest = `{"name":"request","version":"2.88.3","dependencies":{"qs":"~6.5.2"}}`

var pushedTarball = string(npmTarball(requestManifest))

func pushRequest(t *testing.T, proxy *Proxy) HostedArtifact {
	t.Helper()
	target, err := HostedTargetFor("npm", "request", "2.88.3", "")
	if err != nil {
		t.Fatalf("HostedTargetFor: %v", err)
	}
	pushed, err := proxy.PushLocalArtifact(context.Background(), "npm", target, strings.NewReader(pushedTarball), "application/gzip", "")
	if err != nil {
		t.Fatalf("PushLocalArtifact: %v", err)
	}
	return pushed
}

func TestPushLocalArtifactIsServedWithoutUpstream(t *testing.T) {
	proxy, db, _, fetcher := setupTestProxy(t)

	pushed := pushRequest(t, proxy)
	if pushed.Version != "2.88.3" || pushed.Filename != "request-2.88.3.tgz" || pushed.Size != int64(len(pushedTarball)) || len(pushed.SHA256) != 64 {
		t.Errorf("pushed artifact = %+v", pushed)
	}

	result, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz",
		"https://registry.npmjs.org/request/-/request-2.88.3.tgz")
	if err != nil {
		t.Fatalf("GetOrFetchArtifactFromURL: %v", err)
	}
	if got := readResult(result); got != pushedTarball {
		t.Errorf("served body = %q, want the pushed tarball", got)
	}
	if fetcher.fetchCalled {
		t.Error("serving a pushed artifact contacted the upstream")
	}

	art, err := db.GetArtifact("pkg:npm/request@2.88.3", "request-2.88.3.tgz")
	if err != nil || art == nil {
		t.Fatalf("GetArtifact: %+v, %v", art, err)
	}
	if art.Source != database.SourceLocal || art.UpstreamURL != "" {
		t.Errorf("artifact source/upstream_url = %q/%q, want local/empty", art.Source, art.UpstreamURL)
	}
}

func TestPushLocalArtifactReplacesCachedUpstreamCopy(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("upstream tarball")), ContentType: "application/gzip"}

	const upstreamURL = "https://registry.npmjs.org/request/-/request-2.88.3.tgz"
	first, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz", upstreamURL)
	if err != nil {
		t.Fatalf("caching the upstream copy: %v", err)
	}
	_ = readResult(first)

	pushRequest(t, proxy)

	result, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz", upstreamURL)
	if err != nil {
		t.Fatalf("GetOrFetchArtifactFromURL: %v", err)
	}
	if got := readResult(result); got != pushedTarball {
		t.Errorf("served body = %q, want the pushed tarball", got)
	}
}

func TestLocalArtifactsAndDeleteLocalVersion(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	pushed := pushRequest(t, proxy)

	listed, err := proxy.LocalArtifacts("npm", "request")
	if err != nil {
		t.Fatalf("LocalArtifacts: %v", err)
	}
	if len(listed) != 1 || listed[0].Version != pushed.Version || listed[0].SHA256 != pushed.SHA256 {
		t.Fatalf("LocalArtifacts = %+v, want the pushed artifact %+v", listed, pushed)
	}

	if err := proxy.DeleteLocalVersion("npm", "request", "2.88.3"); err != nil {
		t.Fatalf("DeleteLocalVersion: %v", err)
	}
	if cached, err := proxy.GetCachedArtifact(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz"); err != nil || cached != nil {
		t.Errorf("GetCachedArtifact after delete = %+v, %v, want nil, nil", cached, err)
	}
	if listed, err := proxy.LocalArtifacts("npm", "request"); err != nil || len(listed) != 0 {
		t.Errorf("LocalArtifacts after delete = %+v, %v, want none", listed, err)
	}

	if err := proxy.DeleteLocalVersion("npm", "request", "2.88.3"); !errors.Is(err, ErrHostedVersionNotFound) {
		t.Errorf("second DeleteLocalVersion error = %v, want %v", err, ErrHostedVersionNotFound)
	}
}

func TestGetLocalArtifactIgnoresUpstreamCopies(t *testing.T) {
	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.HostedEnabled = true
	fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("upstream tarball")), ContentType: "application/gzip"}
	cached, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz",
		"https://registry.npmjs.org/request/-/request-2.88.3.tgz")
	if err != nil {
		t.Fatalf("caching the upstream copy: %v", err)
	}
	_ = readResult(cached)

	local, err := proxy.GetLocalArtifact(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz")
	if err != nil || local != nil {
		t.Fatalf("GetLocalArtifact for an upstream copy = %+v, %v, want nil, nil", local, err)
	}

	pushRequest(t, proxy)
	local, err = proxy.GetLocalArtifact(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz")
	if err != nil || local == nil {
		t.Fatalf("GetLocalArtifact for a pushed artifact = %+v, %v", local, err)
	}
	if got := readResult(local); got != pushedTarball {
		t.Errorf("served body = %q, want the pushed tarball", got)
	}
}

// With hosted packages disabled, downloads don't look for a pushed artifact,
// as metadata requests don't.
func TestGetLocalArtifactNeedsHostedEnabled(t *testing.T) {
	proxy, db, _, _ := setupTestProxy(t)
	proxy.HostedEnabled = true
	pushRequest(t, proxy)
	if local, err := proxy.GetLocalArtifact(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz"); err != nil || local == nil {
		t.Fatalf("GetLocalArtifact with hosted enabled = %+v, %v, want the pushed artifact", local, err)
	} else {
		_ = local.Reader.Close()
	}

	proxy.HostedEnabled = false
	if local, err := proxy.GetLocalArtifact(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz"); err != nil || local != nil {
		t.Fatalf("GetLocalArtifact with hosted disabled = %+v, %v, want nil, nil", local, err)
	}

	// A closed database fails every query, so a nil error shows no lookup ran.
	_ = db.Close()
	if local, err := proxy.GetLocalArtifact(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz"); err != nil || local != nil {
		t.Errorf("GetLocalArtifact with hosted disabled = %+v, %v, want nil, nil without a database lookup", local, err)
	}
	proxy.HostedEnabled = true
	if _, err := proxy.GetLocalArtifact(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz"); err == nil {
		t.Error("GetLocalArtifact with hosted enabled did not query the closed database")
	}
}

func TestPushLocalArtifactRecordsManifest(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HostedEnabled = true
	pushed := pushRequest(t, proxy)

	versions, err := proxy.localVersions("npm", "request")
	if err != nil || len(versions) != 1 {
		t.Fatalf("localVersions = %+v, %v", versions, err)
	}
	got := versions[0]
	if got.Version != "2.88.3" || got.Filename != "request-2.88.3.tgz" || got.SHA256 != pushed.SHA256 {
		t.Errorf("local version = %+v", got)
	}
	if !strings.Contains(string(got.Manifest), `"qs":"~6.5.2"`) {
		t.Errorf("manifest = %s, want the dependencies from package.json", got.Manifest)
	}
	if !strings.HasPrefix(got.Integrity, "sha512-") {
		t.Errorf("integrity = %q, want sha512", got.Integrity)
	}
}

func TestLocalVersionsNeedHostedEnabled(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	pushRequest(t, proxy)
	if versions, err := proxy.localVersions("npm", "request"); err != nil || versions != nil {
		t.Errorf("localVersions with hosted disabled = %+v, %v, want nil", versions, err)
	}
}

func TestPushLocalArtifactChecksManifest(t *testing.T) {
	tests := []struct {
		name      string
		ecosystem string
		pkg       string
		version   string
		archive   []byte
		wantErr   error
	}{
		{name: "npm other version", ecosystem: "npm", pkg: "request", version: "2.88.3",
			archive: npmTarball(`{"name":"request","version":"2.88.2"}`), wantErr: ErrHostedManifestMismatch},
		{name: "npm other name", ecosystem: "npm", pkg: "request", version: "2.88.3",
			archive: npmTarball(`{"name":"requests","version":"2.88.3"}`), wantErr: ErrHostedManifestMismatch},
		{name: "npm manifest not an object", ecosystem: "npm", pkg: "request", version: "2.88.3",
			archive: npmTarball(`["request"]`), wantErr: ErrHostedInvalidArchive},
		{name: "not an archive", ecosystem: "npm", pkg: "request", version: "2.88.3",
			archive: []byte("plain text"), wantErr: ErrHostedInvalidArchive},
		{name: "npm tarball without package.json", ecosystem: "npm", pkg: "request", version: "2.88.3",
			archive: composerZip("", `{}`), wantErr: ErrHostedInvalidArchive},
		{name: "composer other name", ecosystem: "composer", pkg: "monolog/monolog", version: "3.5.0",
			archive: composerZip("", `{"name":"monolog/other"}`), wantErr: ErrHostedManifestMismatch},
		{name: "composer other version", ecosystem: "composer", pkg: "monolog/monolog", version: "3.5.0",
			archive: composerZip("", `{"name":"monolog/monolog","version":"3.4.0"}`), wantErr: ErrHostedManifestMismatch},
		{name: "composer zip without composer.json", ecosystem: "composer", pkg: "monolog/monolog", version: "3.5.0",
			archive: npmTarball(`{}`), wantErr: ErrHostedInvalidArchive},
		{name: "composer manifest at the root", ecosystem: "composer", pkg: "monolog/monolog", version: "3.5.0",
			archive: composerZip("", `{"name":"monolog/monolog","require":{"php":">=8.1"}}`)},
		{name: "composer manifest in a top directory", ecosystem: "composer", pkg: "monolog/monolog", version: "3.5.0",
			archive: composerZip("Seldaek-monolog-abc123", `{"name":"Monolog/Monolog","version":"3.5.0"}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy, _, store, _ := setupTestProxy(t)
			target, err := HostedTargetFor(tt.ecosystem, tt.pkg, tt.version, "")
			if err != nil {
				t.Fatalf("HostedTargetFor: %v", err)
			}
			_, err = proxy.PushLocalArtifact(context.Background(), tt.ecosystem, target, bytes.NewReader(tt.archive), "", "")
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("PushLocalArtifact: %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("PushLocalArtifact error = %v, want %v", err, tt.wantErr)
			}
			if listed, _ := proxy.LocalArtifacts(tt.ecosystem, tt.pkg); len(listed) != 0 {
				t.Errorf("rejected archive was recorded: %+v", listed)
			}
			if len(store.files) != 0 {
				t.Errorf("rejected archive left %d objects in storage", len(store.files))
			}
		})
	}
}

// removeStored deletes the stored object of an artifact behind the cache's back.
func removeStored(t *testing.T, proxy *Proxy, versionPURL, filename string) {
	t.Helper()
	record, err := proxy.DB.GetArtifact(versionPURL, filename)
	if err != nil || record == nil {
		t.Fatalf("GetArtifact: %+v, %v", record, err)
	}
	if err := proxy.Storage.Delete(context.Background(), record.StoragePath.String); err != nil {
		t.Fatalf("deleting stored object: %v", err)
	}
}

func TestPushedArtifactIsNeverReplacedFromUpstream(t *testing.T) {
	const versionPURL, filename = "pkg:npm/request@2.88.3", "request-2.88.3.tgz"
	tests := []struct {
		name    string
		damage  func(t *testing.T, proxy *Proxy)
		wantErr error
	}{
		{name: "stored object missing", wantErr: ErrHostedArtifactUnavailable,
			damage: func(t *testing.T, proxy *Proxy) { removeStored(t, proxy, versionPURL, filename) }},
		{name: "record cleared after an integrity failure", wantErr: ErrHostedArtifactMissing,
			damage: func(t *testing.T, proxy *Proxy) {
				record, err := proxy.DB.GetArtifact(versionPURL, filename)
				if err != nil || record == nil {
					t.Fatalf("GetArtifact: %+v, %v", record, err)
				}
				if err := proxy.DB.DiscardArtifact(versionPURL, filename, record.StoragePath.String); err != nil {
					t.Fatalf("DiscardArtifact: %v", err)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy, db, _, fetcher := setupTestProxy(t)
			fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader("upstream tarball")), ContentType: "application/gzip"}
			pushRequest(t, proxy)
			tt.damage(t, proxy)

			_, err := proxy.GetOrFetchArtifactFromURL(context.Background(), "npm", "request", "2.88.3", filename,
				"https://registry.npmjs.org/request/-/request-2.88.3.tgz")
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("GetOrFetchArtifactFromURL error = %v, want %v", err, tt.wantErr)
			}
			if fetcher.fetchCalled {
				t.Error("a miss on a pushed artifact fetched the upstream copy")
			}
			local, err := db.GetLocalArtifacts("pkg:npm/request")
			if err != nil || len(local) != 1 || !local[0].Manifest.Valid {
				t.Errorf("pushed records after the miss = %+v, %v, want the pushed one with its manifest", local, err)
			}
		})
	}
}

// zipWith builds a zip holding files in the given order.
func zipWith(files ...[2]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.Create(f[0])
		must(err)
		_, err = io.WriteString(w, f[1])
		must(err)
	}
	must(zw.Close())
	return buf.Bytes()
}

func TestComposerManifestLookup(t *testing.T) {
	const monolog = `{"name":"monolog/monolog"}`
	const fixture = `{"name":"acme/test-fixture"}`
	tests := []struct {
		name    string
		archive []byte
		wantErr error
	}{
		{name: "root manifest after a nested one",
			archive: zipWith([2]string{"tests/composer.json", fixture}, [2]string{"composer.json", monolog})},
		{name: "top directory manifest next to a deeper one",
			archive: zipWith([2]string{"monolog-abc/tests/composer.json", fixture}, [2]string{"monolog-abc/composer.json", monolog})},
		{name: "two top directories with manifests",
			archive: zipWith([2]string{"tests/composer.json", fixture}, [2]string{"monolog-abc/composer.json", monolog}),
			wantErr: ErrHostedInvalidArchive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := HostedTargetFor("composer", "monolog/monolog", "3.5.0", "")
			if err != nil {
				t.Fatalf("HostedTargetFor: %v", err)
			}
			_, err = readHostedManifest("composer", bytes.NewReader(tt.archive), target)
			if tt.wantErr == nil && err != nil {
				t.Errorf("readHostedManifest error = %v, want the root or single top directory manifest", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("readHostedManifest error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestConcurrentPushesOfTheSameFileHaveOneWinner(t *testing.T) {
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HostedEnabled = true
	target, err := HostedTargetFor("generic", "tools", "1.2.0", "tool.tar.gz")
	if err != nil {
		t.Fatalf("HostedTargetFor: %v", err)
	}

	const pushes = 8
	start := make(chan struct{})
	results := make([]error, pushes)
	pushed := make([]HostedArtifact, pushes)
	var wg sync.WaitGroup
	for i := range pushes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			body := strings.NewReader(fmt.Sprintf("build %d", i))
			pushed[i], results[i] = proxy.PushLocalArtifact(context.Background(), "generic", target, body, "", "")
		}()
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, err := range results {
		switch {
		case err == nil && winner >= 0:
			t.Fatalf("pushes %d and %d both succeeded", winner, i)
		case err == nil:
			winner = i
		case !errors.Is(err, ErrHostedExists):
			t.Fatalf("push %d error = %v, want ErrHostedExists", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no push succeeded")
	}

	result, err := proxy.GetLocalArtifact(context.Background(), "generic", "tools", "1.2.0", "tool.tar.gz")
	if err != nil || result == nil {
		t.Fatalf("GetLocalArtifact = %v, %v", result, err)
	}
	served, _ := io.ReadAll(result.Reader)
	_ = result.Reader.Close()
	if string(served) != fmt.Sprintf("build %d", winner) {
		t.Errorf("served %q, want the winning push %q", served, fmt.Sprintf("build %d", winner))
	}
}

// A fetch that started before a push and records after it must not replace
// the pushed artifact or its version's integrity.
func TestCacheFillAfterPushKeepsThePush(t *testing.T) {
	proxy, _, store, _ := setupTestProxy(t)
	proxy.HostedEnabled = true
	pushRequest(t, proxy)

	const fetchedPath = "npm/request/2.88.3/fetched/request-2.88.3.tgz"
	if _, _, err := store.Store(context.Background(), fetchedPath, strings.NewReader("upstream bytes")); err != nil {
		t.Fatalf("storing fetched copy: %v", err)
	}
	fetched := testArtifact("upstream bytes", "pkg:npm/request@2.88.3", "request-2.88.3.tgz", "application/gzip")
	err := proxy.updateCacheDB("npm", "request", "pkg:npm/request", "https://registry.npmjs.org/request/-/request-2.88.3.tgz",
		fetchedPath, fetched, "sha512-upstream")
	if !errors.Is(err, database.ErrLocalArtifactExists) {
		t.Fatalf("updateCacheDB over a push error = %v, want ErrLocalArtifactExists", err)
	}

	result, err := proxy.GetLocalArtifact(context.Background(), "npm", "request", "2.88.3", "request-2.88.3.tgz")
	if err != nil || result == nil {
		t.Fatalf("GetLocalArtifact = %v, %v, want the pushed copy", result, err)
	}
	served, err := io.ReadAll(result.Reader)
	_ = result.Reader.Close()
	if err != nil || string(served) != pushedTarball {
		t.Errorf("served %d bytes, %v, want the pushed tarball, integrity intact", len(served), err)
	}
}

func TestGenericPushDoesNotReadTheStoredFile(t *testing.T) {
	proxy, _, store, _ := setupTestProxy(t)
	proxy.HostedEnabled = true
	target, err := HostedTargetFor("generic", "tools", "1.2.0", "tool.tar.gz")
	if err != nil {
		t.Fatalf("HostedTargetFor: %v", err)
	}
	store.openErr = errors.New("storage opened")
	if _, err := proxy.PushLocalArtifact(context.Background(), "generic", target, strings.NewReader("bytes"), "", ""); err != nil {
		t.Fatalf("PushLocalArtifact: %v", err)
	}
}
