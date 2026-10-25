package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/registries/fetch"
)

const lfsTestContent = "large file content"

// lfsSignature is the signature in the upstream's download URLs, which must
// not reach logs or the database.
const lfsSignature = "s3cr3t-signature"

// lfsUpstream is a Git host with an LFS batch API whose download hrefs point
// at a separate object store, as GitHub's do. The store only serves objects
// to requests carrying the header and signed URL the batch response gave.
type lfsUpstream struct {
	git, store *httptest.Server
	oid        string
	content    atomic.Value // string the store serves for oid
	batches    atomic.Int32
	downloads  atomic.Int32
	// batchStatus, when set, is the status the batch API fails with.
	batchStatus atomic.Int32
	// signature is the signature the batch API hands out and the store
	// accepts; changing it expires the URLs handed out before.
	signature atomic.Value
	// lastBatch is the last batch request the upstream got.
	lastBatch atomic.Value
	// logs is what the proxy logged.
	logs bytes.Buffer
}

func newLFSUpstream(t *testing.T) *lfsUpstream {
	t.Helper()
	sum := sha256.Sum256([]byte(lfsTestContent))
	u := &lfsUpstream{oid: hex.EncodeToString(sum[:])}
	u.content.Store(lfsTestContent)
	u.signature.Store(lfsSignature)
	u.store = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/objects/"+u.oid || r.Header.Get("X-Signed") != "token" ||
			r.URL.Query().Get("Signature") != u.signature.Load().(string) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		u.downloads.Add(1)
		_, _ = io.WriteString(w, u.content.Load().(string))
	}))
	u.git = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// acme/widget has the object, as does its fork; stranger/widget
		// exists but doesn't have it.
		repo, _, _ := strings.Cut(r.URL.Path, ".git/info/lfs/objects/batch")
		hasObject, exists := map[string]bool{"/acme/widget": true, "/fork/widget": true, "/stranger/widget": false}[repo]
		if r.Method != http.MethodPost || !exists {
			http.NotFound(w, r)
			return
		}
		u.batches.Add(1)
		if status := u.batchStatus.Load(); status != 0 {
			http.Error(w, "failing", int(status))
			return
		}
		var req lfsBatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		u.lastBatch.Store(req)
		resp := lfsBatchResponse{Transfer: "basic"}
		for _, object := range req.Objects {
			if object.OID != u.oid || !hasObject {
				resp.Objects = append(resp.Objects, lfsObjectResponse{OID: object.OID, Size: object.Size,
					Error: &lfsObjectError{Code: http.StatusNotFound, Message: "Object does not exist"}})
				continue
			}
			resp.Objects = append(resp.Objects, lfsObjectResponse{OID: object.OID, Size: object.Size,
				Actions: map[string]lfsAction{"download": {
					Href:   u.store.URL + "/objects/" + u.oid + "?Expires=1&Signature=" + u.signature.Load().(string),
					Header: map[string]string{"X-Signed": "token"},
				}}})
		}
		// An object nobody asked for is not passed on.
		resp.Objects = append(resp.Objects, lfsObjectResponse{OID: strings.Repeat("e", 64), Size: 1,
			Actions: map[string]lfsAction{"download": {Href: u.store.URL + "/other"}}})
		w.Header().Set("Content-Type", gitLFSMediaType)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(u.git.Close)
	t.Cleanup(u.store.Close)
	return u
}

func newLFSHandler(t *testing.T, u *lfsUpstream, repos ...string) *GitHandler {
	t.Helper()
	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	proxy.Logger = slog.New(slog.NewTextHandler(&u.logs, nil))
	proxy.HTTPClient = u.git.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(u.store.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	return NewGitHandler(proxy, nil, config.UpstreamConfig{Git: map[string]config.GitUpstream{
		"up": {URL: u.git.URL, Repos: repos},
	}}).WithBaseURL("http://proxy.test/")
}

func lfsRequest(h *GitHandler, method, target, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", gitLFSMediaType)
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, r)
	return w
}

func lfsBatchBody(operation string, objects ...lfsObject) string {
	body, _ := json.Marshal(lfsBatchRequest{Operation: operation, Transfers: []string{"basic"},
		Ref: json.RawMessage(`{"name":"refs/heads/main"}`), Objects: objects})
	return string(body)
}

func TestGitLFSBatchPointsObjectsAtTheProxy(t *testing.T) {
	u := newLFSUpstream(t)
	h := newLFSHandler(t, u)
	missing := strings.Repeat("0", 64)
	size := int64(len(lfsTestContent))

	w := lfsRequest(h, http.MethodPost, "/up/acme/widget.git/info/lfs/objects/batch",
		lfsBatchBody("download", lfsObject{OID: u.oid, Size: size}, lfsObject{OID: missing, Size: 3}))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != gitLFSMediaType {
		t.Fatalf("batch: status %d, type %q, body %s", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	var resp lfsBatchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Objects) != 2 {
		t.Fatalf("objects = %+v, want the two requested", resp.Objects)
	}
	got, notFound := resp.Objects[0], resp.Objects[1]
	want := fmt.Sprintf("http://proxy.test/git/up/acme/widget.git/info/lfs/objects/%s/%d", u.oid, size)
	if download := got.Actions["download"]; download.Href != want || len(download.Header) != 0 || !got.Authenticated {
		t.Errorf("download = %+v, authenticated %v; want %s without the upstream's headers", download, got.Authenticated, want)
	}
	if notFound.Error == nil || notFound.Error.Code != http.StatusNotFound || notFound.Actions != nil {
		t.Errorf("missing object = %+v, want the upstream's 404", notFound)
	}
	if req, _ := u.lastBatch.Load().(lfsBatchRequest); string(req.Ref) != `{"name":"refs/heads/main"}` {
		t.Errorf("upstream got ref %s, want the client's", req.Ref)
	}
}

func TestGitLFSObjectIsCached(t *testing.T) {
	u := newLFSUpstream(t)
	h := newLFSHandler(t, u)
	path := fmt.Sprintf("/up/acme/widget.git/info/lfs/objects/%s/%d", u.oid, len(lfsTestContent))

	w := lfsRequest(h, http.MethodGet, path, "")
	if w.Code != http.StatusOK || w.Body.String() != lfsTestContent {
		t.Fatalf("first GET: status %d, body %q", w.Code, w.Body)
	}
	if u.batches.Load() != 1 || u.downloads.Load() != 1 {
		t.Errorf("batches %d, downloads %d; want one of each", u.batches.Load(), u.downloads.Load())
	}

	u.git.Close()
	u.store.Close()
	w = lfsRequest(h, http.MethodGet, path, "")
	if w.Code != http.StatusOK || w.Body.String() != lfsTestContent {
		t.Errorf("GET with the upstream down: status %d, body %q; want it from the cache", w.Code, w.Body)
	}
}

func TestGitLFSObjectRefusesWrongContent(t *testing.T) {
	u := newLFSUpstream(t)
	u.content.Store("tampered content!!")
	h := newLFSHandler(t, u)
	path := fmt.Sprintf("/up/acme/widget.git/info/lfs/objects/%s/%d", u.oid, len(lfsTestContent))

	for range 2 {
		if w := lfsRequest(h, http.MethodGet, path, ""); w.Code == http.StatusOK && w.Body.String() == "tampered content!!" {
			t.Fatal("served content that does not match its object ID")
		}
	}
	if u.downloads.Load() != 2 {
		t.Errorf("downloads = %d, want 2: the mismatching object must not be cached", u.downloads.Load())
	}
}

func TestGitLFSRefusals(t *testing.T) {
	u := newLFSUpstream(t)
	h := newLFSHandler(t, u, "acme/*")
	object := lfsObject{OID: u.oid, Size: int64(len(lfsTestContent))}
	tests := []struct {
		name, method, target, body string
		status                     int
	}{
		{"upload", http.MethodPost, "/up/acme/widget.git/info/lfs/objects/batch", lfsBatchBody("upload", object), http.StatusForbidden},
		{"sha512 objects", http.MethodPost, "/up/acme/widget.git/info/lfs/objects/batch",
			`{"operation":"download","objects":[],"hash_algo":"sha512"}`, http.StatusConflict},
		{"other transfers", http.MethodPost, "/up/acme/widget.git/info/lfs/objects/batch",
			`{"operation":"download","transfers":["ssh"],"objects":[]}`, http.StatusUnprocessableEntity},
		{"invalid oid", http.MethodPost, "/up/acme/widget.git/info/lfs/objects/batch",
			lfsBatchBody("download", lfsObject{OID: "../x", Size: 1}), http.StatusUnprocessableEntity},
		{"batch GET", http.MethodGet, "/up/acme/widget.git/info/lfs/objects/batch", "", http.StatusMethodNotAllowed},
		{"repository outside repos", http.MethodPost, "/up/other/widget.git/info/lfs/objects/batch", lfsBatchBody("download", object), http.StatusNotFound},
		{"unknown repository", http.MethodPost, "/up/acme/missing.git/info/lfs/objects/batch", lfsBatchBody("download", object), http.StatusNotFound},
		{"object without size", http.MethodGet, "/up/acme/widget.git/info/lfs/objects/" + u.oid, "", http.StatusNotFound},
		{"uppercase oid", http.MethodGet, "/up/acme/widget.git/info/lfs/objects/" + strings.ToUpper(u.oid) + "/1", "", http.StatusNotFound},
		{"object PUT", http.MethodPut, "/up/acme/widget.git/info/lfs/objects/" + u.oid + "/1", "", http.StatusMethodNotAllowed},
		{"locks", http.MethodPost, "/up/acme/widget.git/info/lfs/locks/verify", "{}", http.StatusNotFound},
		{"missing object", http.MethodGet, "/up/acme/widget.git/info/lfs/objects/" + strings.Repeat("0", 64) + "/3", "", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := lfsRequest(h, tt.method, tt.target, tt.body)
			if w.Code != tt.status || w.Header().Get("Content-Type") != gitLFSMediaType {
				t.Errorf("status %d, type %q, body %s; want %d", w.Code, w.Header().Get("Content-Type"), w.Body, tt.status)
			}
		})
	}
	if u.downloads.Load() != 0 {
		t.Errorf("downloads = %d, want none", u.downloads.Load())
	}
}

func TestGitLFSKeepsSignedURLsOutOfLogsAndDatabase(t *testing.T) {
	u := newLFSUpstream(t)
	h := newLFSHandler(t, u)
	path := fmt.Sprintf("/up/acme/widget.git/info/lfs/objects/%s/%d", u.oid, len(lfsTestContent))

	if w := lfsRequest(h, http.MethodGet, path, ""); w.Code != http.StatusOK {
		t.Fatalf("GET: status %d, body %s", w.Code, w.Body)
	}
	_, versionPURL, err := packagePURLStrings(gitEcosystem, "up/acme/widget", u.oid)
	if err != nil {
		t.Fatal(err)
	}
	record, err := h.proxy.DB.GetArtifact(versionPURL, u.oid)
	if err != nil || record == nil {
		t.Fatalf("artifact record: %v, %v", record, err)
	}
	if want := u.store.URL + "/objects/" + u.oid; record.UpstreamURL != want {
		t.Errorf("recorded upstream URL %q, want %q", record.UpstreamURL, want)
	}

	// A failed download is logged without the signature too, although the
	// connection error names the URL.
	other := newLFSUpstream(t)
	other.store.Close()
	h = newLFSHandler(t, other)
	if w := lfsRequest(h, http.MethodGet, path, ""); w.Code == http.StatusOK {
		t.Fatalf("GET from an unreachable store: status %d", w.Code)
	}
	if !strings.Contains(other.logs.String(), other.store.URL+"/objects/"+other.oid) {
		t.Fatalf("logs don't name the failed download:\n%s", other.logs.String())
	}
	for _, logs := range []string{u.logs.String(), other.logs.String()} {
		if strings.Contains(logs, lfsSignature) {
			t.Errorf("logs contain the download URL's signature:\n%s", logs)
		}
	}
}

func TestGitLFSBatchFromCacheWhenUpstreamFails(t *testing.T) {
	u := newLFSUpstream(t)
	h := newLFSHandler(t, u)
	size := int64(len(lfsTestContent))
	missing := strings.Repeat("0", 64)
	batch := lfsBatchBody("download", lfsObject{OID: u.oid, Size: size}, lfsObject{OID: missing, Size: 3})
	const batchPath = "/up/acme/widget.git/info/lfs/objects/batch"

	// Nothing cached yet: a failing upstream fails the batch.
	u.batchStatus.Store(http.StatusServiceUnavailable)
	if w := lfsRequest(h, http.MethodPost, batchPath, batch); w.Code != http.StatusBadGateway {
		t.Fatalf("batch with nothing cached: status %d, body %s; want 502", w.Code, w.Body)
	}
	u.batchStatus.Store(0)
	if w := lfsRequest(h, http.MethodGet, fmt.Sprintf("/up/acme/widget.git/info/lfs/objects/%s/%d", u.oid, size), ""); w.Code != http.StatusOK {
		t.Fatalf("GET: status %d, body %s", w.Code, w.Body)
	}

	// A refusal is the upstream's answer, not an outage.
	u.batchStatus.Store(http.StatusForbidden)
	if w := lfsRequest(h, http.MethodPost, batchPath, batch); w.Code != http.StatusBadGateway {
		t.Errorf("batch the upstream refused: status %d, body %s; want 502", w.Code, w.Body)
	}

	u.git.Close()
	w := lfsRequest(h, http.MethodPost, batchPath, batch)
	if w.Code != http.StatusOK {
		t.Fatalf("batch with the upstream down: status %d, body %s", w.Code, w.Body)
	}
	var resp lfsBatchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Objects) != 2 {
		t.Fatalf("objects = %+v, want the two requested", resp.Objects)
	}
	want := fmt.Sprintf("http://proxy.test/git/up/acme/widget.git/info/lfs/objects/%s/%d", u.oid, size)
	if got := resp.Objects[0]; got.Actions["download"].Href != want || !got.Authenticated || got.Error != nil {
		t.Errorf("cached object = %+v, want a download from %s", got, want)
	}
	if got := resp.Objects[1]; got.Actions != nil || got.Error == nil {
		t.Errorf("uncached object = %+v, want an error", got)
	}
}

func TestGitLFSHeadDoesNotDownload(t *testing.T) {
	u := newLFSUpstream(t)
	h := newLFSHandler(t, u)
	path := fmt.Sprintf("/up/acme/widget.git/info/lfs/objects/%s/%d", u.oid, len(lfsTestContent))

	w := lfsRequest(h, http.MethodHead, path, "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Length") != fmt.Sprint(len(lfsTestContent)) {
		t.Errorf("HEAD: status %d, Content-Length %q", w.Code, w.Header().Get("Content-Length"))
	}
	if u.downloads.Load() != 0 {
		t.Errorf("downloads = %d, want none for a HEAD", u.downloads.Load())
	}
	missing := fmt.Sprintf("/up/acme/widget.git/info/lfs/objects/%s/3", strings.Repeat("0", 64))
	if w := lfsRequest(h, http.MethodHead, missing, ""); w.Code != http.StatusNotFound {
		t.Errorf("HEAD of a missing object: status %d, want 404", w.Code)
	}
}

func TestGitLFSObjectAfterBatchNeedsNoSecondBatch(t *testing.T) {
	u := newLFSUpstream(t)
	h := newLFSHandler(t, u)
	size := int64(len(lfsTestContent))
	path := fmt.Sprintf("/up/acme/widget.git/info/lfs/objects/%s/%d", u.oid, size)
	batch := lfsBatchBody("download", lfsObject{OID: u.oid, Size: size})

	if w := lfsRequest(h, http.MethodPost, "/up/acme/widget.git/info/lfs/objects/batch", batch); w.Code != http.StatusOK {
		t.Fatalf("batch: status %d, body %s", w.Code, w.Body)
	}
	if w := lfsRequest(h, http.MethodGet, path, ""); w.Code != http.StatusOK || w.Body.String() != lfsTestContent {
		t.Fatalf("GET: status %d, body %q", w.Code, w.Body)
	}
	if u.batches.Load() != 1 || u.downloads.Load() != 1 {
		t.Errorf("batches %d, downloads %d; want the client's batch and one download", u.batches.Load(), u.downloads.Load())
	}

	// A URL that stopped working before its time is replaced by a fresh one.
	other := newLFSUpstream(t)
	h = newLFSHandler(t, other)
	if w := lfsRequest(h, http.MethodPost, "/up/acme/widget.git/info/lfs/objects/batch", batch); w.Code != http.StatusOK {
		t.Fatalf("batch: status %d, body %s", w.Code, w.Body)
	}
	other.signature.Store("rotated")
	if w := lfsRequest(h, http.MethodGet, path, ""); w.Code != http.StatusOK || w.Body.String() != lfsTestContent {
		t.Fatalf("GET with a revoked URL: status %d, body %q", w.Code, w.Body)
	}
	if other.batches.Load() != 2 {
		t.Errorf("batches = %d, want a second one for a fresh URL", other.batches.Load())
	}
}

func TestGitLFSKeptDownloadsExpire(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	object := lfsObject{OID: strings.Repeat("a", 64), Size: 3}
	href := "https://objects.example/a?sig=1"
	tests := []struct {
		name   string
		action lfsAction
		at     time.Duration
		kept   bool
	}{
		{"no expiry", lfsAction{Href: href}, 4 * time.Minute, true},
		{"no expiry, later", lfsAction{Href: href}, 5 * time.Minute, false},
		{"expires_in", lfsAction{Href: href, ExpiresIn: 600}, 9 * time.Minute, true},
		{"expires_in, within the margin", lfsAction{Href: href, ExpiresIn: 600}, 10*time.Minute - 10*time.Second, false},
		{"expires_in too short to use", lfsAction{Href: href, ExpiresIn: 10}, 0, false},
		{"expires_at", lfsAction{Href: href, ExpiresAt: now.Add(20 * time.Minute).Format(time.RFC3339)}, 19 * time.Minute, true},
		{"expires_at, past", lfsAction{Href: href, ExpiresAt: now.Add(20 * time.Minute).Format(time.RFC3339)}, 20 * time.Minute, false},
		{"long expiry capped", lfsAction{Href: href, ExpiresIn: 86400}, 2 * time.Hour, false},
		{"relative href", lfsAction{Href: "/a"}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d lfsDownloads
			d.put("repo", object, tt.action, now)
			if _, kept := d.get("repo", object, now.Add(tt.at)); kept != tt.kept {
				t.Errorf("kept = %v, want %v", kept, tt.kept)
			}
		})
	}

	var d lfsDownloads
	d.put("repo", object, lfsAction{Href: href}, now)
	if _, kept := d.get("repo", lfsObject{OID: object.OID, Size: 4}, now); kept {
		t.Error("kept for another size")
	}
	if _, kept := d.get("other", object, now); kept {
		t.Error("kept for another repository")
	}
	for i := range gitLFSKeptDownloads + 10 {
		d.put("repo", lfsObject{OID: fmt.Sprintf("%064x", i), Size: 1}, lfsAction{Href: href}, now)
	}
	if len(d.entries) > gitLFSKeptDownloads {
		t.Errorf("%d entries kept, want at most %d", len(d.entries), gitLFSKeptDownloads)
	}
}

func TestGitLFSObjectOfAnotherSizeIsRefused(t *testing.T) {
	u := newLFSUpstream(t)
	h := newLFSHandler(t, u)
	path := fmt.Sprintf("/up/acme/widget.git/info/lfs/objects/%s/%d", u.oid, len(lfsTestContent)-1)

	if w := lfsRequest(h, http.MethodGet, path, ""); w.Code == http.StatusOK {
		t.Fatalf("GET at the wrong size: status %d, body %q", w.Code, w.Body)
	}
	if u.downloads.Load() != 1 || u.batches.Load() != 1 {
		t.Errorf("downloads %d, batches %d; want one of each, without a retry", u.downloads.Load(), u.batches.Load())
	}
}

func TestExactSizeBody(t *testing.T) {
	for _, tt := range []struct {
		body string
		size int64
		ok   bool
	}{
		{"abcdef", 6, true},
		{"abcdef", 5, false},
		{"abcdef", 7, false},
		{"", 0, true},
		{"a", 0, false},
	} {
		got, err := io.ReadAll(&exactSizeBody{ReadCloser: io.NopCloser(strings.NewReader(tt.body)), left: tt.size})
		if (err == nil) != tt.ok || (err != nil && !errors.Is(err, errDownloadSize)) {
			t.Errorf("%q at size %d: err %v, want ok %v", tt.body, tt.size, err, tt.ok)
		}
		if int64(len(got)) > tt.size+1 {
			t.Errorf("%q at size %d: read %d bytes", tt.body, tt.size, len(got))
		}
	}
}

func TestGitLFSObjectCopiedFromAnotherRepository(t *testing.T) {
	u := newLFSUpstream(t)
	h := newLFSHandler(t, u)
	object := func(repo string) string {
		return fmt.Sprintf("/up/%s.git/info/lfs/objects/%s/%d", repo, u.oid, len(lfsTestContent))
	}

	if w := lfsRequest(h, http.MethodGet, object("acme/widget"), ""); w.Code != http.StatusOK {
		t.Fatalf("GET: status %d, body %s", w.Code, w.Body)
	}
	w := lfsRequest(h, http.MethodGet, object("fork/widget"), "")
	if w.Code != http.StatusOK || w.Body.String() != lfsTestContent {
		t.Fatalf("GET from the fork: status %d, body %q", w.Code, w.Body)
	}
	if u.downloads.Load() != 1 || u.batches.Load() != 2 {
		t.Errorf("downloads %d, batches %d; want the fork's object copied after its own batch", u.downloads.Load(), u.batches.Load())
	}
	cached, err := h.proxy.GetCachedArtifact(t.Context(), gitEcosystem, "up/fork/widget", u.oid, u.oid)
	if err != nil || cached == nil {
		t.Fatalf("fork's object not cached: %v, %v", cached, err)
	}
	_ = cached.Reader.Close()

	// A repository the upstream says lacks the object doesn't get it, though
	// a copy is stored.
	if w := lfsRequest(h, http.MethodGet, object("stranger/widget"), ""); w.Code != http.StatusNotFound {
		t.Errorf("GET from a repository without the object: status %d, body %s; want 404", w.Code, w.Body)
	}
}
