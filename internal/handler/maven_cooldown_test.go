package handler

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"hash"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/git-pkgs/cooldown"
	registryclient "github.com/git-pkgs/registries/client"
	"github.com/git-pkgs/registries/fetch"
)

const mavenTestDir = "/com/example/lib/"

// mavenCooldownUpstream serves com.example:lib. Each version's .pom has the
// Last-Modified in times; pomStatus overrides the status of a version's
// .pom. Requests for .pom files are counted.
type mavenCooldownUpstream struct {
	server      *httptest.Server
	versions    []string
	latest      string
	release     string
	lastUpdated time.Time
	times       map[string]time.Time
	pomStatus   map[string]int

	mu    sync.Mutex
	poms  map[string]int
	heads int
}

func newMavenCooldownUpstream(t *testing.T) *mavenCooldownUpstream {
	t.Helper()
	now := time.Now()
	u := &mavenCooldownUpstream{
		versions:    []string{"1.0", "1.1", "2.0", "2.1", "3.0-SNAPSHOT"},
		latest:      "3.0-SNAPSHOT",
		release:     "2.1",
		lastUpdated: now.Add(-time.Hour),
		times: map[string]time.Time{
			"1.0":          now.Add(-30 * 24 * time.Hour),
			"1.1":          now.Add(-10 * 24 * time.Hour),
			"2.0":          now.Add(-2 * time.Hour),
			"2.1":          now.Add(-time.Hour),
			"3.0-SNAPSHOT": now.Add(-time.Minute),
		},
		pomStatus: map[string]int{},
		poms:      map[string]int{},
	}
	u.server = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.server.Close)
	return u
}

// serve answers for the default upstream and, under /portal, for the
// Gradle Plugin Portal fallback alike.
func (u *mavenCooldownUpstream) serve(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(strings.TrimPrefix(r.URL.Path, "/portal"), mavenTestDir)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch rest {
	case "maven-metadata.xml":
		_, _ = w.Write([]byte(u.metadata()))
		return
	case "maven-metadata.xml.asc":
		_, _ = w.Write([]byte("signature"))
		return
	}
	version, file, _ := strings.Cut(rest, "/")
	published, known := u.times[version]
	if !known {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(file, ".pom") {
		u.mu.Lock()
		u.poms[version]++
		if r.Method == http.MethodHead {
			u.heads++
		}
		u.mu.Unlock()
		w.Header().Set(headerLastModified, published.UTC().Format(http.TimeFormat))
		if status := u.pomStatus[version]; status != 0 {
			// An error page dated long ago must not count as an old version.
			w.Header().Set(headerLastModified, time.Unix(0, 0).UTC().Format(http.TimeFormat))
			if status == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "60")
			}
			w.WriteHeader(status)
			return
		}
	}
	_, _ = w.Write([]byte(file + " of " + version))
}

func (u *mavenCooldownUpstream) metadata() string {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<metadata>\n  <groupId>com.example</groupId>\n  <artifactId>lib</artifactId>\n  <versioning>\n")
	b.WriteString("    <latest>" + u.latest + "</latest>\n    <release>" + u.release + "</release>\n    <versions>\n")
	for _, v := range u.versions {
		b.WriteString("      <version>" + v + "</version>\n")
	}
	b.WriteString("    </versions>\n    <lastUpdated>" + u.lastUpdated.UTC().Format(mavenLastUpdatedLayout) + "</lastUpdated>\n  </versioning>\n</metadata>\n")
	return b.String()
}

// pomRequests returns how many requests each version's .pom got.
func (u *mavenCooldownUpstream) pomRequests() map[string]int {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make(map[string]int, len(u.poms))
	for v, n := range u.poms {
		out[v] = n
	}
	return out
}

func (u *mavenCooldownUpstream) totalPOMRequests() int {
	total := 0
	for _, n := range u.pomRequests() {
		total += n
	}
	return total
}

func newMavenCooldownHandler(t *testing.T, u *mavenCooldownUpstream) (http.Handler, *Proxy) {
	t.Helper()
	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	proxy.HTTPClient = u.server.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(u.server.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	t.Cleanup(func() { _ = fetcher.Close() })
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	return NewMavenHandler(proxy, "http://localhost", u.server.URL, u.server.URL+"/portal").Routes(), proxy
}

func mavenMetadataVersions(t *testing.T, body string) []string {
	t.Helper()
	doc, err := parseMavenMetadata([]byte(body))
	if err != nil {
		t.Fatalf("served metadata does not parse: %v\n%s", err, body)
	}
	var versions []string
	for _, v := range doc.versions {
		versions = append(versions, v.value)
	}
	return versions
}

func TestMavenCooldownFiltersMetadata(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	h, proxy := newMavenCooldownHandler(t, u)

	w := getMaven(h, mavenTestDir+"maven-metadata.xml")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	if got := strings.Join(mavenMetadataVersions(t, body), " "); got != "1.0 1.1 3.0-SNAPSHOT" {
		t.Errorf("versions = %s, want 1.0 1.1 3.0-SNAPSHOT", got)
	}
	if !strings.Contains(body, "<release>1.1</release>") || !strings.Contains(body, "<latest>3.0-SNAPSHOT</latest>") {
		t.Errorf("release or latest not recomputed:\n%s", body)
	}
	if strings.Contains(body, "\n\n") {
		t.Errorf("removed versions left blank lines:\n%s", body)
	}

	// The walk stops at 1.1, the first version old enough, and skips the
	// SNAPSHOT.
	want := map[string]int{"2.1": 1, "2.0": 1, "1.1": 1}
	if got := u.pomRequests(); len(got) != len(want) || got["2.1"] != 1 || got["2.0"] != 1 || got["1.1"] != 1 {
		t.Errorf("pom requests = %v, want %v", got, want)
	}

	// Times are stored, so the next request makes no .pom requests.
	ver, err := proxy.DB.GetVersionByPURL(canonicalVersionPURL("maven", "com.example:lib", "2.1"))
	if err != nil || ver == nil || !ver.PublishedAt.Valid {
		t.Fatalf("time of 2.1 not stored: %v %v", ver, err)
	}
	if w := getMaven(h, mavenTestDir+"maven-metadata.xml"); w.Body.String() != body {
		t.Errorf("second response differs:\n%s", w.Body)
	}
	if n := u.totalPOMRequests(); n != 3 {
		t.Errorf("pom requests after second metadata request = %d, want 3", n)
	}
}

func TestMavenCooldownRecomputesLatest(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	u.versions = []string{"1.0", "1.1", "2.0", "2.1"}
	u.latest = "2.1"
	h, _ := newMavenCooldownHandler(t, u)

	body := getMaven(h, mavenTestDir+"maven-metadata.xml").Body.String()
	if !strings.Contains(body, "<latest>1.1</latest>") || !strings.Contains(body, "<release>1.1</release>") {
		t.Errorf("latest and release should be 1.1:\n%s", body)
	}
}

func TestMavenCooldownDropsLatestWhenNothingIsLeft(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	u.versions = []string{"2.0", "2.1"}
	u.latest = "2.1"
	h, _ := newMavenCooldownHandler(t, u)

	body := getMaven(h, mavenTestDir+"maven-metadata.xml").Body.String()
	if strings.Contains(body, "<latest>") || strings.Contains(body, "<release>") || strings.Contains(body, "<version>") {
		t.Errorf("no version is out of cooldown:\n%s", body)
	}
	if !strings.Contains(body, "<versions>\n    </versions>") {
		t.Errorf("version list not emptied in place:\n%s", body)
	}
}

func TestMavenCooldownChecksumsMatchMetadata(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	h, _ := newMavenCooldownHandler(t, u)

	body := getMaven(h, mavenTestDir+"maven-metadata.xml").Body.Bytes()
	for suffix, newHash := range map[string]func() hash.Hash{
		".md5": md5.New, ".sha1": sha1.New, ".sha256": sha256.New, ".sha512": sha512.New,
	} {
		sum := newHash()
		_, _ = sum.Write(body)
		w := getMaven(h, mavenTestDir+"maven-metadata.xml"+suffix)
		if w.Code != http.StatusOK || w.Body.String() != hex.EncodeToString(sum.Sum(nil)) {
			t.Errorf("%s: status = %d body %q, want checksum of served metadata", suffix, w.Code, w.Body)
		}
	}
	// A signature of the upstream metadata would not match.
	if w := getMaven(h, mavenTestDir+"maven-metadata.xml.asc"); w.Code != http.StatusNotFound {
		t.Errorf(".asc status = %d, want 404", w.Code)
	}

	// Unchanged metadata keeps its signature.
	u.lastUpdated = time.Now().Add(-4 * 24 * time.Hour)
	if w := getMaven(h, mavenTestDir+"maven-metadata.xml.asc"); w.Code != http.StatusOK || w.Body.String() != "signature" {
		t.Errorf("unchanged .asc: status = %d body %q, want upstream's", w.Code, w.Body)
	}
}

func TestMavenCooldownRefusesFiles(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	h, _ := newMavenCooldownHandler(t, u)

	for _, file := range []string{"2.1/lib-2.1.pom", "2.1/lib-2.1.jar", "2.1/lib-2.1.jar.sha1", "2.0/lib-2.0-sources.jar"} {
		w := getMaven(h, mavenTestDir+file)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), errMavenCooldown) {
			t.Errorf("%s: status = %d body %q, want 403 cooldown", file, w.Code, w.Body)
		}
	}
	for _, file := range []string{"1.1/lib-1.1.jar", "3.0-SNAPSHOT/lib-3.0-SNAPSHOT.jar"} {
		if w := getMaven(h, mavenTestDir+file); w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", file, w.Code)
		}
	}
	// One .pom request per version, none for the SNAPSHOT.
	if got := u.pomRequests(); got["2.1"] != 1 || got["2.0"] != 1 || got["1.1"] != 1 || got["3.0-SNAPSHOT"] != 0 {
		t.Errorf("pom requests = %v", got)
	}
}

// TestMavenCooldownPOMDownloadCarriesItsTime checks that a build fetching
// pinned versions, which never reads maven-metadata.xml, costs no requests
// beyond its downloads: the .pom's own Last-Modified gives the version's
// time, and Maven fetches the .pom before the version's other files.
func TestMavenCooldownPOMDownloadCarriesItsTime(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	h, proxy := newMavenCooldownHandler(t, u)

	for _, c := range []struct {
		file string
		want int
	}{
		{"2.1/lib-2.1.pom", http.StatusForbidden},
		{"2.1/lib-2.1.jar", http.StatusForbidden},
		{"1.0/lib-1.0.pom", http.StatusOK},
		{"1.0/lib-1.0.jar", http.StatusOK},
	} {
		if w := getMaven(h, mavenTestDir+c.file); w.Code != c.want {
			t.Errorf("%s: status = %d, want %d", c.file, w.Code, c.want)
		}
	}
	u.mu.Lock()
	heads := u.heads
	u.mu.Unlock()
	if heads != 0 {
		t.Errorf("HEAD requests = %d, want 0", heads)
	}
	if got := u.pomRequests(); got["2.1"] != 1 || got["1.0"] != 1 {
		t.Errorf("pom requests = %v, want one GET each", got)
	}
	ver, _ := proxy.DB.GetVersionByPURL(canonicalVersionPURL("maven", "com.example:lib", "2.1"))
	if ver == nil || !ver.PublishedAt.Valid || !ver.PublishedAt.Time.Equal(u.times["2.1"].Truncate(time.Second)) {
		t.Errorf("stored time of 2.1 = %v, want %v", ver, u.times["2.1"])
	}
}

func TestMavenCooldownOldLastUpdatedSkipsPOMs(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	u.lastUpdated = time.Now().Add(-4 * 24 * time.Hour)
	// The times would filter 2.0 and 2.1 if they were read.
	h, _ := newMavenCooldownHandler(t, u)

	w := getMaven(h, mavenTestDir+"maven-metadata.xml")
	if w.Body.String() != u.metadata() {
		t.Errorf("metadata changed:\n%s", w.Body)
	}
	if n := u.totalPOMRequests(); n != 0 {
		t.Errorf("pom requests = %d, want 0", n)
	}
}

func TestMavenCooldownIgnoresNon2xxPOMResponses(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	u.pomStatus["2.1"] = http.StatusNotFound
	h, proxy := newMavenCooldownHandler(t, u)

	// A missing .pom gives no time: 2.1 stays listed, and the walk goes on
	// to 2.0 rather than stopping at the error page's old date.
	versions := mavenMetadataVersions(t, getMaven(h, mavenTestDir+"maven-metadata.xml").Body.String())
	if got := strings.Join(versions, " "); got != "1.0 1.1 2.1 3.0-SNAPSHOT" {
		t.Errorf("versions = %s, want 1.0 1.1 2.1 3.0-SNAPSHOT", got)
	}
	if ver, _ := proxy.DB.GetVersionByPURL(canonicalVersionPURL("maven", "com.example:lib", "2.1")); ver != nil && ver.PublishedAt.Valid {
		t.Errorf("time of a 404 stored: %v", ver.PublishedAt.Time)
	}

	// Any other error fails closed.
	u.pomStatus["2.1"] = http.StatusTooManyRequests
	for _, p := range []string{"maven-metadata.xml", "maven-metadata.xml.sha1", "2.1/lib-2.1.jar"} {
		if w := getMaven(h, mavenTestDir+p); w.Code != http.StatusBadGateway {
			t.Errorf("%s: status = %d, want 502", p, w.Code)
		}
	}
	if ver, _ := proxy.DB.GetVersionByPURL(canonicalVersionPURL("maven", "com.example:lib", "2.1")); ver != nil && ver.PublishedAt.Valid {
		t.Errorf("time of a 429 stored: %v", ver.PublishedAt.Time)
	}
}

// TestMavenCooldownRateLimitedPOMIsHeld checks that a 429 on a .pom time
// request is answered like any held upstream request, with 503 and the
// upstream's Retry-After, so clients wait instead of failing the build.
func TestMavenCooldownRateLimitedPOMIsHeld(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	u.pomStatus["2.0"] = http.StatusTooManyRequests
	h, proxy := newMavenCooldownHandler(t, u)
	gate := registryclient.NewRateLimitGate(u.server.Client().Transport)
	proxy.HTTPClient = &http.Client{Transport: gate}
	proxy.RateLimits = gate

	// Not the .pom, so its time is read with a HEAD.
	w := getMaven(h, mavenTestDir+"2.0/lib-2.0.jar")
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Errorf("status = %d, Retry-After %q; want 503 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
}

func TestMavenCooldownDisabledPassesThrough(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	h, proxy := newMavenCooldownHandler(t, u)
	proxy.Cooldown = nil

	if w := getMaven(h, mavenTestDir+"maven-metadata.xml"); w.Body.String() != u.metadata() {
		t.Errorf("metadata changed:\n%s", w.Body)
	}
	if w := getMaven(h, mavenTestDir+"2.1/lib-2.1.jar"); w.Code != http.StatusOK {
		t.Errorf("jar status = %d, want 200", w.Code)
	}
	if n := u.totalPOMRequests(); n != 0 {
		t.Errorf("pom requests = %d, want 0", n)
	}
}

func TestMavenCooldownPackageOverride(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	h, proxy := newMavenCooldownHandler(t, u)
	proxy.Cooldown = &cooldown.Config{Default: "3d", Packages: map[string]string{"pkg:maven/com.example/lib": "0"}}

	if w := getMaven(h, mavenTestDir+"maven-metadata.xml"); w.Body.String() != u.metadata() {
		t.Errorf("metadata changed:\n%s", w.Body)
	}
	if w := getMaven(h, mavenTestDir+"2.1/lib-2.1.jar"); w.Code != http.StatusOK {
		t.Errorf("jar status = %d, want 200", w.Code)
	}
	if n := u.totalPOMRequests(); n != 0 {
		t.Errorf("pom requests = %d, want 0", n)
	}
}

func TestMavenCooldownNamedRepositoryStoresItsOwnTimes(t *testing.T) {
	u := newMavenCooldownUpstream(t)
	proxy, _, _, _ := setupTestProxy(t)
	proxy.HTTPClient = u.server.Client()
	proxy.Cooldown = &cooldown.Config{Default: "3d"}
	// The named repository is the same server under /portal, which serves
	// the same files.
	h := NewMavenHandler(proxy, "http://localhost", u.server.URL, u.server.URL+"/portal").
		WithRepositories(map[string]string{"mirror": u.server.URL + "/portal"}).Routes()

	for range 2 {
		versions := mavenMetadataVersions(t, getMaven(h, "/mirror"+mavenTestDir+"maven-metadata.xml").Body.String())
		if got := strings.Join(versions, " "); got != "1.0 1.1 3.0-SNAPSHOT" {
			t.Fatalf("versions = %s, want 1.0 1.1 3.0-SNAPSHOT", got)
		}
	}
	if w := getMaven(h, "/mirror"+mavenTestDir+"2.1/lib-2.1.jar"); w.Code != http.StatusForbidden {
		t.Errorf("jar status = %d, want 403", w.Code)
	}
	if n := u.totalPOMRequests(); n != 3 {
		t.Errorf("pom requests = %d, want 3", n)
	}

	// The default repository doesn't use the named one's times.
	_ = getMaven(h, mavenTestDir+"maven-metadata.xml")
	if n := u.totalPOMRequests(); n != 6 {
		t.Errorf("pom requests after default metadata = %d, want 6", n)
	}
}

func TestMavenMetadataWithoutCompact(t *testing.T) {
	in := `<metadata><versioning><latest>2</latest><release>2</release><versions><version>1</version><version>2</version></versions></versioning></metadata>`
	doc, err := parseMavenMetadata([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	got := string(doc.without([]byte(in), map[string]bool{"2": true}))
	want := `<metadata><versioning><latest>1</latest><release>1</release><versions><version>1</version></versions></versioning></metadata>`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
