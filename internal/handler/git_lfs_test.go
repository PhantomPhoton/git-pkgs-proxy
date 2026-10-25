package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/registries/fetch"
)

const lfsTestContent = "large file content"

// lfsUpstream is a Git host with an LFS batch API whose download hrefs point
// at a separate object store, as GitHub's do. The store only serves objects
// to requests carrying the header the batch response gave.
type lfsUpstream struct {
	git, store *httptest.Server
	oid        string
	content    atomic.Value // string the store serves for oid
	batches    atomic.Int32
	downloads  atomic.Int32
	// lastBatch is the last batch request the upstream got.
	lastBatch atomic.Value
}

func newLFSUpstream(t *testing.T) *lfsUpstream {
	t.Helper()
	sum := sha256.Sum256([]byte(lfsTestContent))
	u := &lfsUpstream{oid: hex.EncodeToString(sum[:])}
	u.content.Store(lfsTestContent)
	u.store = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/objects/"+u.oid || r.Header.Get("X-Signed") != "token" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		u.downloads.Add(1)
		_, _ = io.WriteString(w, u.content.Load().(string))
	}))
	u.git = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/acme/widget.git/info/lfs/objects/batch" {
			http.NotFound(w, r)
			return
		}
		u.batches.Add(1)
		var req lfsBatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		u.lastBatch.Store(req)
		resp := lfsBatchResponse{Transfer: "basic"}
		for _, object := range req.Objects {
			if object.OID != u.oid {
				resp.Objects = append(resp.Objects, lfsObjectResponse{OID: object.OID, Size: object.Size,
					Error: &lfsObjectError{Code: http.StatusNotFound, Message: "Object does not exist"}})
				continue
			}
			resp.Objects = append(resp.Objects, lfsObjectResponse{OID: object.OID, Size: object.Size,
				Actions: map[string]lfsAction{"download": {
					Href:   u.store.URL + "/objects/" + u.oid,
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
