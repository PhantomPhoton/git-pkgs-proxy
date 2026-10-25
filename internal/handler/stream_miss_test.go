package handler

import (
	"bytes"
	"context"
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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/metrics"
	"github.com/git-pkgs/proxy/internal/scanner"
	"github.com/git-pkgs/registries/fetch"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// lockedBuffer is a log destination safe for the goroutine that stores a
// streamed artifact to write to while a test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// streamMissSetup is a Git LFS upstream whose object store sends the first
// half of an object, then waits for release before sending the rest, and a
// proxy serving it to clients the way the server marks their requests.
type streamMissSetup struct {
	proxy     *Proxy
	store     *mockStorage
	client    *httptest.Server
	git       *httptest.Server
	objects   *httptest.Server
	release   chan struct{}
	content   string // what the object store sends
	oid       string // the object ID clients ask for
	path      string
	downloads atomic.Int32
	logs      lockedBuffer
}

// streamMissContent spans many copy buffers: a stream checked against its
// digest holds back its latest chunk until the next one arrives.
var streamMissContent = strings.Repeat("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ\n", 400<<10/63)

// streamMissPrefix is how much a client must get while the upstream still
// holds back the second half.
const streamMissPrefix = 64 << 10

func newStreamMiss(t *testing.T, served string) *streamMissSetup {
	t.Helper()
	return newStreamMissMarked(t, served, true)
}

// newStreamMissMarked is newStreamMiss, with requests marked to detach
// their upstream fetch as the server does, or not, as when the server has
// no detached fetch to spare.
func newStreamMissMarked(t *testing.T, served string, detached bool) *streamMissSetup {
	t.Helper()
	sum := sha256.Sum256([]byte(streamMissContent))
	s := &streamMissSetup{content: served, oid: hex.EncodeToString(sum[:]), release: make(chan struct{})}
	s.objects = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.downloads.Add(1)
		half := len(s.content) / 2
		_, _ = io.WriteString(w, s.content[:half])
		w.(http.Flusher).Flush()
		select {
		case <-s.release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, s.content[half:])
	}))
	s.git = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req lfsBatchRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp := lfsBatchResponse{Transfer: "basic"}
		for _, o := range req.Objects {
			resp.Objects = append(resp.Objects, lfsObjectResponse{OID: o.OID, Size: o.Size,
				Actions: map[string]lfsAction{"download": {Href: s.objects.URL + "/objects/" + o.OID}}})
		}
		w.Header().Set("Content-Type", gitLFSMediaType)
		_ = json.NewEncoder(w).Encode(resp)
	}))

	proxy, _, store, _ := setupTestProxy(t)
	store.seekable = true
	proxy.Logger = slog.New(slog.NewTextHandler(&s.logs, nil))
	proxy.HTTPClient = s.git.Client()
	fetcher := fetch.NewFetcher(fetch.WithHTTPClient(s.objects.Client()), fetch.WithMaxRetries(0))
	proxy.Fetcher = fetcher
	s.proxy, s.store = proxy, store
	h := NewGitHandler(proxy, nil, config.UpstreamConfig{Git: map[string]config.GitUpstream{
		"up": {URL: s.git.URL},
	}}).WithBaseURL("http://proxy.test/")
	routes := h.Routes()
	// As the server marks client requests.
	s.client = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := StreamCacheMiss(r.Context())
		if detached {
			ctx = DetachUpstreamFetch(ctx)
		}
		routes.ServeHTTP(w, r.WithContext(ctx))
	}))
	s.path = fmt.Sprintf("/up/acme/widget.git/info/lfs/objects/%s/%d", s.oid, len(streamMissContent))
	t.Cleanup(func() {
		s.releaseUpstream()
		s.client.Close()
		s.git.Close()
		s.objects.Close()
		_ = fetcher.Close()
	})
	return s
}

func (s *streamMissSetup) releaseUpstream() {
	select {
	case <-s.release:
	default:
		close(s.release)
	}
}

// get requests the object. The response headers must arrive in time even
// while the upstream holds back the second half.
func (s *streamMissSetup) get(t *testing.T) *http.Response {
	t.Helper()
	type result struct {
		resp *http.Response
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := s.client.Client().Get(s.client.URL + s.path)
		got <- result{resp, err}
	}()
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.resp
	case <-time.After(5 * time.Second):
		// Once the upstream is released the response arrives; close it.
		t.Cleanup(func() {
			s.releaseUpstream()
			if r := <-got; r.resp != nil {
				_ = r.resp.Body.Close()
			}
		})
		t.Fatal("no response while the upstream was still sending")
		return nil
	}
}

// cached waits for the stored copy, which is written after the client's
// stream ends.
func (s *streamMissSetup) cached(t *testing.T) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := s.proxy.GetCachedArtifact(t.Context(), gitEcosystem, "up/acme/widget", s.oid, s.oid)
		if err == nil && res != nil {
			_ = res.Reader.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readFirst reads the first streamMissPrefix bytes of body, failing if they
// don't arrive in time.
func readFirst(t *testing.T, body io.Reader) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, streamMissPrefix)
		_, _ = io.ReadFull(body, buf)
		got <- string(buf)
	}()
	select {
	case s := <-got:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no bytes reached the client while the upstream was still sending")
		return ""
	}
}

func TestCacheMissStreamsBeforeUpstreamFinishes(t *testing.T) {
	s := newStreamMiss(t, streamMissContent)
	resp := s.get(t)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.ContentLength != -1 {
		t.Fatalf("status %d, Content-Length %d; want 200, chunked", resp.StatusCode, resp.ContentLength)
	}
	if first := readFirst(t, resp.Body); first != streamMissContent[:streamMissPrefix] {
		t.Fatal("first bytes differ from the artifact's")
	}

	s.releaseUpstream()
	rest, err := io.ReadAll(resp.Body)
	if err != nil || string(rest) != streamMissContent[streamMissPrefix:] {
		t.Fatalf("rest: %d bytes, %v", len(rest), err)
	}
	if !s.cached(t) {
		t.Fatal("streamed artifact was not cached")
	}

	// Served from the cache now, with its size.
	s.objects.Close()
	again := s.get(t)
	body, _ := io.ReadAll(again.Body)
	_ = again.Body.Close()
	if again.StatusCode != http.StatusOK || string(body) != streamMissContent || again.ContentLength != int64(len(streamMissContent)) {
		t.Errorf("cached GET: status %d, length %d, %d bytes", again.StatusCode, again.ContentLength, len(body))
	}
	if s.downloads.Load() != 1 {
		t.Errorf("downloads = %d, want 1", s.downloads.Load())
	}
}

func TestCacheMissStreamAbortedOnMismatch(t *testing.T) {
	tampered := "X" + streamMissContent[1:]
	s := newStreamMiss(t, tampered)
	s.releaseUpstream()
	resp := s.get(t)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err == nil {
		t.Fatalf("read %d bytes without error; want the response aborted", len(body))
	}
	if string(body) == tampered {
		t.Error("client got every byte of an artifact that doesn't match its object ID")
	}
	// Nothing to wait for: storeArtifact has refused it once the stream ends.
	time.Sleep(100 * time.Millisecond)
	if res, _ := s.proxy.GetCachedArtifact(t.Context(), gitEcosystem, "up/acme/widget", s.oid, s.oid); res != nil {
		_ = res.Reader.Close()
		t.Error("mismatching artifact was cached")
	}
}

func TestCacheMissStreamFinishesWhenStorageFails(t *testing.T) {
	s := newStreamMiss(t, streamMissContent)
	s.store.mu.Lock()
	s.store.storeErr = errors.New("no space left on device")
	s.store.mu.Unlock()
	before := testutil.ToFloat64(metrics.ArtifactsServedUncached.WithLabelValues(gitEcosystem))
	s.releaseUpstream()

	resp := s.get(t)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != streamMissContent {
		t.Fatalf("status %d, %d bytes, %v; want the whole artifact", resp.StatusCode, len(body), err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for testutil.ToFloat64(metrics.ArtifactsServedUncached.WithLabelValues(gitEcosystem)) == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := testutil.ToFloat64(metrics.ArtifactsServedUncached.WithLabelValues(gitEcosystem)) - before; got != 1 {
		t.Errorf("served-uncached count went up by %v, want 1", got)
	}
	if !strings.Contains(s.logs.String(), "level=ERROR") || !strings.Contains(s.logs.String(), "not cached") {
		t.Errorf("storage failure not logged at error level:\n%s", s.logs.String())
	}
}

func TestCacheMissStoredAfterClientLeaves(t *testing.T) {
	s := newStreamMiss(t, streamMissContent)
	resp := s.get(t)
	readFirst(t, resp.Body)
	_ = resp.Body.Close()
	s.client.CloseClientConnections()

	s.releaseUpstream()
	if !s.cached(t) {
		t.Fatal("artifact not cached after the client that started the fetch left")
	}
}

func TestCacheMissWaiterGetsStoredArtifact(t *testing.T) {
	s := newStreamMiss(t, streamMissContent)
	first := s.get(t)
	defer func() { _ = first.Body.Close() }()
	readFirst(t, first.Body)

	second := make(chan string, 1)
	go func() {
		resp, err := s.client.Client().Get(s.client.URL + s.path)
		if err != nil {
			second <- err.Error()
			return
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		second <- fmt.Sprintf("%d %d %s", resp.StatusCode, resp.ContentLength, body)
	}()
	select {
	case got := <-second:
		t.Fatalf("second request answered before the artifact was stored: %s", got)
	case <-time.After(200 * time.Millisecond):
	}

	s.releaseUpstream()
	if _, err := io.ReadAll(first.Body); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("200 %d %s", len(streamMissContent), streamMissContent)
	if got := <-second; got != want {
		t.Errorf("second request = %.60q..., want the stored artifact with its length", got)
	}
	if s.downloads.Load() != 1 {
		t.Errorf("downloads = %d, want 1", s.downloads.Load())
	}
}

func newStreamMissScanGroup(t *testing.T, mode string) *scanner.Group {
	t.Helper()
	g, err := scanner.NewGroup(config.ScanningConfig{
		Enabled:    true,
		Timeout:    "15s",
		SigningKey: "test-signing-key",
		Scanners: []config.ScannerConfig{
			{Name: "test-scanner", URL: newTestScanServer(t, true, "").URL, Mode: mode},
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// A block-mode scanner must see the whole artifact before any of it is
// served, so a miss isn't streamed.
func TestCacheMissNotStreamedWithBlockingScanner(t *testing.T) {
	s := newStreamMiss(t, streamMissContent)
	s.proxy.ScanSigningKey = []byte("test-signing-key")
	s.proxy.Scanners = newStreamMissScanGroup(t, "block")

	got := make(chan *http.Response, 1)
	go func() {
		resp, err := s.client.Client().Get(s.client.URL + s.path)
		if err != nil {
			t.Error(err)
		}
		got <- resp
	}()
	select {
	case resp := <-got:
		_ = resp.Body.Close()
		t.Fatal("response started before the upstream finished, with a block-mode scanner")
	case <-time.After(300 * time.Millisecond):
	}
	s.releaseUpstream()
	resp := <-got
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != streamMissContent || resp.ContentLength != int64(len(streamMissContent)) {
		t.Errorf("status %d, length %d, %d bytes; want the scanned, stored artifact", resp.StatusCode, resp.ContentLength, len(body))
	}
}

// A monitor-mode scanner doesn't gate the artifact, so the miss is streamed
// and the scan runs after the client's stream ends.
func TestCacheMissStreamedWithMonitoringScanner(t *testing.T) {
	s := newStreamMiss(t, streamMissContent)
	s.proxy.ScanSigningKey = []byte("test-signing-key")
	s.proxy.Scanners = newStreamMissScanGroup(t, "monitor")

	resp := s.get(t)
	defer func() { _ = resp.Body.Close() }()
	readFirst(t, resp.Body)
	s.releaseUpstream()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	if !s.cached(t) {
		t.Error("artifact not cached")
	}
}

// slowStorage stores like mockStorage, but takes a while to finish once it
// has read the last byte, as an upload to S3 does.
type slowStorage struct {
	*mockStorage
	delay time.Duration
}

func (s *slowStorage) Store(ctx context.Context, path string, r io.Reader) (int64, string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, "", err
	}
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return 0, "", ctx.Err()
	}
	return s.mockStorage.Store(ctx, path, bytes.NewReader(data))
}

// Without a detached fetch, storing still finishes after the client's
// request has ended with the last byte.
func TestCacheMissStoredAfterResponseWithoutDetach(t *testing.T) {
	s := newStreamMissMarked(t, streamMissContent, false)
	s.proxy.Storage = &slowStorage{mockStorage: s.store, delay: 300 * time.Millisecond}
	s.releaseUpstream()
	resp := s.get(t)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(body) != streamMissContent {
		t.Fatalf("%d bytes, %v", len(body), err)
	}
	if !s.cached(t) {
		t.Fatal("artifact not cached once the response ended")
	}
}
