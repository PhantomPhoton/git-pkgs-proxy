package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
)

type goCooldownUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	hits   map[string]int
}

func (u *goCooldownUpstream) count(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[path]
}

// newGoCooldownUpstream serves example.com/mod with an old v1.0.0, a
// recent v1.1.0 and a recent v1.2.0-rc.1, and @latest pointing at v1.1.0.
func newGoCooldownUpstream(t *testing.T, latest string) *goCooldownUpstream {
	t.Helper()
	now := time.Now()
	times := map[string]time.Time{
		"v1.0.0":      now.Add(-10 * 24 * time.Hour),
		"v1.1.0":      now.Add(-time.Hour),
		"v1.2.0-rc.1": now.Add(-2 * time.Hour),
	}
	u := &goCooldownUpstream{hits: map[string]int{}}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits[r.URL.Path]++
		u.mu.Unlock()
		rest, ok := strings.CutPrefix(r.URL.Path, "/example.com/mod/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeInfo := func(v string) {
			_ = json.NewEncoder(w).Encode(goInfo{Version: v, Time: times[v]})
		}
		switch {
		case rest == "@v/list":
			_, _ = w.Write([]byte("v1.0.0\nv1.1.0\nv1.2.0-rc.1\n"))
		case rest == "@latest":
			writeInfo(latest)
		case rest == "@v/main.info":
			writeInfo("v1.1.0")
		case strings.HasSuffix(rest, ".info"):
			v := strings.TrimSuffix(strings.TrimPrefix(rest, "@v/"), ".info")
			if _, ok := times[v]; !ok {
				http.NotFound(w, r)
				return
			}
			writeInfo(v)
		case strings.HasSuffix(rest, ".mod"):
			_, _ = w.Write([]byte("module example.com/mod\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.server.Close)
	return u
}

func goCooldownHandler(t *testing.T, u *goCooldownUpstream) (http.Handler, *mockFetcher, *Proxy) {
	t.Helper()
	proxy, _, _, fetcher := setupTestProxy(t)
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	return NewGoHandlerWithUpstream(proxy, "http://localhost:8080", u.server.URL).Routes(), fetcher, proxy
}

func goGet(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func TestGoCooldownFiltersList(t *testing.T) {
	u := newGoCooldownUpstream(t, "v1.1.0")
	h, _, _ := goCooldownHandler(t, u)

	w := goGet(h, "/example.com/mod/@v/list")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Body.String(); got != "v1.0.0\n" {
		t.Fatalf("list = %q, want only v1.0.0", got)
	}

	// Publish times are stored, so a second list needs no .info requests.
	_ = goGet(h, "/example.com/mod/@v/list")
	if n := u.count("/example.com/mod/@v/v1.0.0.info"); n != 1 {
		t.Errorf("v1.0.0.info fetched %d times, want 1", n)
	}
}

func TestGoCooldownRefusesVersions(t *testing.T) {
	u := newGoCooldownUpstream(t, "v1.1.0")
	h, fetcher, _ := goCooldownHandler(t, u)

	for _, p := range []string{
		"/example.com/mod/@v/v1.1.0.info",
		"/example.com/mod/@v/v1.1.0.mod",
		"/example.com/mod/@v/v1.1.0.zip",
		"/example.com/mod/@v/main.info",
	} {
		w := goGet(h, p)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), errGoCooldown) {
			t.Errorf("%s: status = %d body %q, want 403 cooldown", p, w.Code, w.Body.String())
		}
	}
	if fetcher.fetchCalled {
		t.Error("zip in cooldown was fetched")
	}

	for _, p := range []string{"/example.com/mod/@v/v1.0.0.info", "/example.com/mod/@v/v1.0.0.mod"} {
		if w := goGet(h, p); w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", p, w.Code)
		}
	}
}

func TestGoCooldownLatest(t *testing.T) {
	u := newGoCooldownUpstream(t, "v1.1.0")
	h, _, _ := goCooldownHandler(t, u)

	w := goGet(h, "/example.com/mod/@latest")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var info goInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Version != "v1.0.0" {
		t.Fatalf("@latest = %s, want v1.0.0", info.Version)
	}
}

func TestGoCooldownLatestAllowed(t *testing.T) {
	u := newGoCooldownUpstream(t, "v1.0.0")
	h, _, _ := goCooldownHandler(t, u)

	w := goGet(h, "/example.com/mod/@latest")
	var info goInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil || info.Version != "v1.0.0" {
		t.Fatalf("@latest = %d %s, want v1.0.0", w.Code, w.Body)
	}
	if n := u.count("/example.com/mod/@v/list"); n != 0 {
		t.Errorf("list fetched %d times for an allowed @latest", n)
	}
}

func TestGoCooldownDisabledPassesThrough(t *testing.T) {
	u := newGoCooldownUpstream(t, "v1.1.0")
	h, _, proxy := goCooldownHandler(t, u)
	proxy.Cooldown = nil

	if w := goGet(h, "/example.com/mod/@v/list"); w.Body.String() != "v1.0.0\nv1.1.0\nv1.2.0-rc.1\n" {
		t.Fatalf("list = %q", w.Body.String())
	}
	if w := goGet(h, "/example.com/mod/@v/v1.1.0.info"); w.Code != http.StatusOK {
		t.Fatalf("info status = %d", w.Code)
	}
}

func TestNewestGoVersion(t *testing.T) {
	tests := []struct {
		versions []string
		want     string
	}{
		{[]string{"v1.0.0", "v1.10.0", "v1.9.0", "v2.0.0-rc.1"}, "v1.10.0"},
		{[]string{"v1.0.0-alpha", "v1.0.0-beta"}, "v1.0.0-beta"},
		{[]string{"v2.0.0+incompatible", "v1.5.0"}, "v2.0.0+incompatible"},
		{nil, ""},
	}
	for _, tt := range tests {
		if got := newestGoVersion(tt.versions); got != tt.want {
			t.Errorf("newestGoVersion(%v) = %q, want %q", tt.versions, got, tt.want)
		}
	}
}
