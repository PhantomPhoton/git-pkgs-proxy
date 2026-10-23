package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	"github.com/vmihailenco/msgpack/v5"
)

func zstdMsgpack(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := msgpack.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	out, err := compressCondaShard(raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func decodeZstdMsgpack(t *testing.T, body []byte, v any) {
	t.Helper()
	raw, err := decompressCondaShard(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := msgpack.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

type shardFixture struct {
	upstream  *httptest.Server
	shardHash string
	requests  []string
}

// newShardUpstream serves a sharded index for conda-forge/noarch with one
// shard for testpkg, holding an old and a recent build.
func newShardUpstream(t *testing.T, info map[string]any) *shardFixture {
	t.Helper()
	now := time.Now()
	shard := zstdMsgpack(t, map[string]any{
		"packages": map[string]any{
			"testpkg-1.0.0-0.tar.bz2": map[string]any{
				"name": "testpkg", "version": "1.0.0", "timestamp": now.Add(-7 * 24 * time.Hour).UnixMilli(),
				"sha256": bytes.Repeat([]byte{1}, 32),
			},
		},
		"packages.conda": map[string]any{
			"testpkg-1.0.0-0.conda": map[string]any{
				"name": "testpkg", "version": "1.0.0", "timestamp": now.Add(-7 * 24 * time.Hour).UnixMilli(),
			},
			"testpkg-2.0.0-0.conda": map[string]any{
				"name": "testpkg", "version": "2.0.0", "timestamp": now.Add(-time.Hour).UnixMilli(),
			},
		},
		"removed": []string{"testpkg-0.1.0-0.tar.bz2"},
	})
	sum := sha256.Sum256(shard)
	f := &shardFixture{shardHash: hex.EncodeToString(sum[:])}

	layout := ""
	if s, _ := info["shards_base_url"].(string); strings.Contains(s, "shards/") {
		layout = "shards/"
	}
	var index []byte
	f.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.Path)
		switch r.URL.Path {
		case "/conda-forge/noarch/" + condaShardIndexFile:
			_, _ = w.Write(index)
		case "/conda-forge/noarch/" + layout + f.shardHash + condaShardSuffix:
			_, _ = w.Write(shard)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.upstream.Close)

	// {upstream} in an info value stands for the upstream's URL, {host} for
	// its host.
	placeholders := strings.NewReplacer("{upstream}", f.upstream.URL, "{host}", strings.TrimPrefix(f.upstream.URL, "http://"))
	for k, v := range info {
		if s, ok := v.(string); ok {
			info[k] = placeholders.Replace(s)
		}
	}
	index = zstdMsgpack(t, map[string]any{
		"version": 1,
		"info":    info,
		"shards":  map[string]any{"testpkg": sum[:]},
	})
	return f
}

func shardTestHandler(f *shardFixture, cooldownDefault string) http.Handler {
	proxy := testProxy()
	if cooldownDefault != "" {
		proxy.Cooldown = &cooldown.Config{Default: cooldownDefault}
	}
	return NewCondaHandlerWithUpstream(proxy, "http://proxy.local", f.upstream.URL).Routes()
}

func serveShardRequest(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

type shardRecords struct {
	Packages      map[string]map[string]any `msgpack:"packages"`
	PackagesConda map[string]map[string]any `msgpack:"packages.conda"`
	Removed       []string                  `msgpack:"removed"`
}

func (s shardRecords) filenames() []string {
	var names []string
	for k := range s.Packages {
		names = append(names, k)
	}
	for k := range s.PackagesConda {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

type shardIndex struct {
	Version int               `msgpack:"version"`
	Info    map[string]any    `msgpack:"info"`
	Shards  map[string][]byte `msgpack:"shards"`
}

func TestCondaShardsWithoutCooldown(t *testing.T) {
	for _, shardsBaseURL := range []string{"", "./shards/"} {
		t.Run(shardsBaseURL, func(t *testing.T) {
			f := newShardUpstream(t, map[string]any{"base_url": "", "shards_base_url": shardsBaseURL, "subdir": "noarch"})
			h := shardTestHandler(f, "")

			w := serveShardRequest(h, "/conda-forge/noarch/"+condaShardIndexFile)
			if w.Code != http.StatusOK {
				t.Fatalf("index status = %d", w.Code)
			}
			var index shardIndex
			decodeZstdMsgpack(t, w.Body.Bytes(), &index)
			if got := hex.EncodeToString(index.Shards["testpkg"]); got != f.shardHash {
				t.Fatalf("index hash = %s, want upstream %s", got, f.shardHash)
			}
			if index.Info["shards_base_url"] != shardsBaseURL {
				t.Fatalf("shards_base_url = %v, want %q", index.Info["shards_base_url"], shardsBaseURL)
			}

			dir := strings.TrimPrefix(shardsBaseURL, "./")
			w = serveShardRequest(h, "/conda-forge/noarch/"+dir+f.shardHash+condaShardSuffix)
			if w.Code != http.StatusOK {
				t.Fatalf("shard status = %d", w.Code)
			}
			var shard shardRecords
			decodeZstdMsgpack(t, w.Body.Bytes(), &shard)
			if len(shard.filenames()) != 3 {
				t.Fatalf("records = %v, want all 3", shard.filenames())
			}
		})
	}
}

func TestCondaShardsWithCooldown(t *testing.T) {
	f := newShardUpstream(t, map[string]any{"base_url": "", "shards_base_url": "./shards/", "subdir": "noarch", "created_at": "2026-01-01T00:00:00Z"})
	h := shardTestHandler(f, "3d")

	w := serveShardRequest(h, "/conda-forge/noarch/"+condaShardIndexFile)
	if w.Code != http.StatusOK {
		t.Fatalf("index status = %d", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("Cache-Control"), "public, max-age=") {
		t.Errorf("Cache-Control = %q", w.Header().Get("Cache-Control"))
	}
	var index shardIndex
	decodeZstdMsgpack(t, w.Body.Bytes(), &index)
	masked := hex.EncodeToString(index.Shards["testpkg"])
	if masked == f.shardHash || len(masked) != 64 {
		t.Fatalf("index hash = %s, want a masked hash", masked)
	}
	if index.Info["created_at"] != "2026-01-01T00:00:00Z" || index.Info["subdir"] != "noarch" {
		t.Errorf("info lost fields: %v", index.Info)
	}
	shardsBaseURL, _ := index.Info["shards_base_url"].(string)
	if !strings.HasPrefix(shardsBaseURL, "./"+condaCooldownShardDir+"/") || !strings.HasSuffix(shardsBaseURL, "/shards/") {
		t.Fatalf("shards_base_url = %q", shardsBaseURL)
	}

	w = serveShardRequest(h, "/conda-forge/noarch/"+strings.TrimPrefix(shardsBaseURL, "./")+masked+condaShardSuffix)
	if w.Code != http.StatusOK {
		t.Fatalf("shard status = %d: %s", w.Code, w.Body)
	}
	var shard shardRecords
	decodeZstdMsgpack(t, w.Body.Bytes(), &shard)
	want := []string{"testpkg-1.0.0-0.conda", "testpkg-1.0.0-0.tar.bz2"}
	if got := shard.filenames(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("records = %v, want %v", got, want)
	}
	if len(shard.Removed) != 1 {
		t.Errorf("removed = %v, want it kept", shard.Removed)
	}
	if _, ok := shard.Packages["testpkg-1.0.0-0.tar.bz2"]["sha256"].([]byte); !ok {
		t.Errorf("sha256 is no longer binary: %T", shard.Packages["testpkg-1.0.0-0.tar.bz2"]["sha256"])
	}

	// The unfiltered shard stays out of reach while cooldown is on.
	w = serveShardRequest(h, "/conda-forge/noarch/shards/"+f.shardHash+condaShardSuffix)
	if w.Code != http.StatusNotFound {
		t.Fatalf("plain shard status = %d, want 404", w.Code)
	}
}

func TestCondaCooldownShardRejectsBadPaths(t *testing.T) {
	f := newShardUpstream(t, map[string]any{"base_url": "", "shards_base_url": ""})
	h := shardTestHandler(f, "3d")
	hash := strings.Repeat("a", 64) + condaShardSuffix
	for _, p := range []string{
		"/conda-forge/noarch/cooldown-shards/abc/" + hash,
		"/conda-forge/noarch/cooldown-shards/1/other/" + hash,
		"/conda-forge/noarch/cooldown-shards/1/" + strings.Repeat("A", 64) + condaShardSuffix,
		"/conda-forge/noarch/cooldown-shards/1/x.msgpack.zst",
	} {
		if w := serveShardRequest(h, p); w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", p, w.Code)
		}
	}
	if len(f.requests) != 0 {
		t.Errorf("upstream was asked for %v", f.requests)
	}
}

func TestCondaShardIndexUnsupportedLayout(t *testing.T) {
	for name, info := range map[string]map[string]any{
		"packages elsewhere":   {"base_url": "https://cdn.example.com/conda-forge/noarch/", "shards_base_url": ""},
		"shards elsewhere":     {"base_url": "", "shards_base_url": "https://cdn.example.com/shards/"},
		"nested shards":        {"base_url": "", "shards_base_url": "./a/b/"},
		"packages upstream":    {"base_url": "{upstream}/conda-forge/noarch/", "shards_base_url": ""},
		"shards upstream":      {"base_url": "", "shards_base_url": "{upstream}/conda-forge/noarch/"},
		"no scheme upstream":   {"base_url": "//{host}/conda-forge/noarch/", "shards_base_url": ""},
		"root-relative":        {"base_url": "/conda-forge/noarch/", "shards_base_url": ""},
		"root-relative shards": {"base_url": "", "shards_base_url": "/conda-forge/noarch/shards/"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newShardUpstream(t, info)
			for _, cd := range []string{"", "3d"} {
				w := serveShardRequest(shardTestHandler(f, cd), "/conda-forge/noarch/"+condaShardIndexFile)
				if w.Code != http.StatusNotFound {
					t.Errorf("cooldown %q: status = %d, want 404 so clients use repodata.json", cd, w.Code)
				}
			}
		})
	}
}

func TestCondaShardIndexUnknownVersion(t *testing.T) {
	index := zstdMsgpack(t, map[string]any{"version": 2, "info": map[string]any{}, "shards": map[string]any{}})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(index)
	}))
	defer upstream.Close()

	proxy := testProxy()
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	h := NewCondaHandlerWithUpstream(proxy, "http://proxy.local", upstream.URL).Routes()
	if w := serveShardRequest(h, "/conda-forge/noarch/"+condaShardIndexFile); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestCondaShardMaskDependsOnBucket(t *testing.T) {
	a := condaShardMask("conda-forge", "noarch", "1")
	if a == condaShardMask("conda-forge", "noarch", "2") || a == condaShardMask("conda-forge", "linux-64", "1") {
		t.Fatal("mask must change with bucket and subdir")
	}
}
