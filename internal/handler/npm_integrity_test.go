package handler

import (
	"context"
	"crypto/sha1" //nolint:gosec // npm's dist.shasum is SHA-1
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/git-pkgs/registries/fetch"
)

const npmIntegrityTarball = "tarball data"

var npmIntegrityCases = []struct {
	name   string
	dist   map[string]string
	wantOK bool
}{
	{"matching sha512", map[string]string{"integrity": sha512SRI(npmIntegrityTarball)}, true},
	{"mismatched sha512", map[string]string{"integrity": sha512SRI("other bytes")}, false},
	{"matching sha256", map[string]string{"integrity": sha256SRI(npmIntegrityTarball)}, true},
	{"strongest algorithm decides", map[string]string{"integrity": sha256SRI("other bytes") + " " + sha512SRI(npmIntegrityTarball)}, true},
	{"any digest of the strongest algorithm", map[string]string{"integrity": sha512SRI("other bytes") + " " + sha512SRI(npmIntegrityTarball)}, true},
	{"unparseable integrity", map[string]string{"integrity": "sha512-nope"}, true},
	{"shasum alone is not checked yet", map[string]string{"shasum": sha1Hex("other bytes")}, true},
	{"no checksum", map[string]string{}, true},
}

// TestNPMDownloadVerifiesDeclaredIntegrity checks a freshly fetched tarball
// against dist.integrity: a mismatch is a 502 and nothing is cached.
func TestNPMDownloadVerifiesDeclaredIntegrity(t *testing.T) {
	for _, tt := range npmIntegrityCases {
		t.Run(tt.name, func(t *testing.T) {
			proxy, srv := npmIntegrityProxy(t, tt.dist)

			resp, body := npmIntegrityGet(t, srv)
			if wantStatus := map[bool]int{true: http.StatusOK, false: http.StatusBadGateway}[tt.wantOK]; resp.StatusCode != wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, wantStatus, body)
			}
			if tt.wantOK && body != npmIntegrityTarball {
				t.Errorf("body = %q, want the tarball", body)
			}
			cached, err := proxy.GetCachedArtifact(context.Background(), "npm", "leftpad", "1.0.0", "leftpad-1.0.0.tgz")
			if err != nil {
				t.Fatalf("reading cache: %v", err)
			}
			if cached != nil {
				_ = cached.Reader.Close()
			}
			if (cached != nil) != tt.wantOK {
				t.Errorf("cached = %v, want %v", cached != nil, tt.wantOK)
			}
		})
	}
}

// TestNPMDownloadRecordsDeclaredIntegrity checks that the integrity a tarball
// was verified against is kept with the cached version, so cache hits are
// verified against it too.
func TestNPMDownloadRecordsDeclaredIntegrity(t *testing.T) {
	sri := sha512SRI(npmIntegrityTarball)
	proxy, srv := npmIntegrityProxy(t, map[string]string{"integrity": sri})
	if resp, body := npmIntegrityGet(t, srv); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; body: %s", resp.StatusCode, body)
	}

	pkgPURL, versionPURL, err := packagePURLStrings("npm", "leftpad", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	record, err := proxy.DB.GetCachedArtifact(pkgPURL, versionPURL, "leftpad-1.0.0.tgz")
	if err != nil || record == nil {
		t.Fatalf("cache record = %v, %v", record, err)
	}
	if record.Integrity.String != sri {
		t.Errorf("recorded integrity = %q, want %q", record.Integrity.String, sri)
	}
}

// TestNPMStreamedDownloadVerifiesDeclaredIntegrity checks dist.integrity
// when tarballs are streamed without being stored: a mismatch aborts the
// response, so the client never receives a complete body.
func TestNPMStreamedDownloadVerifiesDeclaredIntegrity(t *testing.T) {
	for _, tt := range npmIntegrityCases {
		t.Run(tt.name, func(t *testing.T) {
			proxy, srv := npmIntegrityProxy(t, tt.dist)
			proxy.StreamArtifacts = true

			// The response is aborted on a mismatch: before its headers when
			// the body fits in the server's buffer, during the body otherwise.
			body, err := func() ([]byte, error) {
				resp, err := http.Get(srv.URL + "/leftpad/-/leftpad-1.0.0.tgz")
				if err != nil {
					return nil, err
				}
				defer func() { _ = resp.Body.Close() }()
				return io.ReadAll(resp.Body)
			}()

			if tt.wantOK {
				if err != nil || string(body) != npmIntegrityTarball {
					t.Errorf("body = %q, %v; want the complete tarball", body, err)
				}
				return
			}
			if err == nil {
				t.Errorf("received the whole body (%q), want the response aborted", body)
			}
		})
	}
}

func npmIntegrityProxy(t *testing.T, dist map[string]string) (*Proxy, *httptest.Server) {
	t.Helper()
	upstream := npmIntegrityUpstream(t, dist)
	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.HTTPClient = upstream.Client()
	fetcher.artifact = &fetch.Artifact{Body: io.NopCloser(strings.NewReader(npmIntegrityTarball)), Size: int64(len(npmIntegrityTarball))}

	srv := httptest.NewServer(NewNPMHandler(proxy, "http://proxy.test", upstream.URL).Routes())
	t.Cleanup(srv.Close)
	return proxy, srv
}

func npmIntegrityGet(t *testing.T, srv *httptest.Server) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/leftpad/-/leftpad-1.0.0.tgz")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// npmIntegrityUpstream serves a leftpad packument whose 1.0.0 carries dist.
func npmIntegrityUpstream(t *testing.T, dist map[string]string) *httptest.Server {
	t.Helper()
	packument, err := json.Marshal(map[string]any{
		"name":      "leftpad",
		"dist-tags": map[string]string{"latest": "1.0.0"},
		"versions": map[string]any{
			"1.0.0": map[string]any{"dist": dist},
		},
	})
	if err != nil {
		t.Fatalf("encoding packument: %v", err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		_, _ = w.Write(packument)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func sha1Hex(data string) string {
	sum := sha1.Sum([]byte(data)) //nolint:gosec // npm's dist.shasum is SHA-1
	return hex.EncodeToString(sum[:])
}
