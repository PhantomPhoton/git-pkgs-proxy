package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/git-pkgs/proxy/internal/storage"
	"github.com/git-pkgs/registries/fetch"
	"github.com/go-chi/chi/v5"
)

const (
	testHostedToken     = "push-token"
	testHostedMaxUpload = 64
	pushedBody          = "patched tarball"
)

type hostedTestServer struct {
	router           http.Handler
	fetches          *int
	upstreamRequests *int
}

// countingFetcher fails every fetch, so a test fails loudly if the proxy goes
// upstream for something it should serve from the pushed copy.
type countingFetcher struct{ calls *int }

func (f countingFetcher) Fetch(ctx context.Context, url string) (*fetch.Artifact, error) {
	return f.FetchWithHeaders(ctx, url, nil)
}

func (f countingFetcher) FetchWithHeaders(context.Context, string, http.Header) (*fetch.Artifact, error) {
	*f.calls++
	return nil, fetch.ErrNotFound
}

func (f countingFetcher) Head(context.Context, string) (int64, string, error) {
	*f.calls++
	return 0, "", fetch.ErrNotFound
}

func newHostedTestServer(t *testing.T) hostedTestServer {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Create(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("database.Create: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := storage.OpenBucket(context.Background(), "file://"+filepath.Join(dir, "artifacts"))
	if err != nil {
		t.Fatalf("storage.OpenBucket: %v", err)
	}

	calls := 0
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := handler.NewProxy(db, store, countingFetcher{calls: &calls}, fetch.NewResolver(), logger)

	upstreamRequests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamRequests++
		http.NotFound(w, nil)
	}))
	t.Cleanup(upstream.Close)

	r := chi.NewRouter()
	registerHostedRoutes(r, NewHostedAPIHandler(proxy, testHostedMaxUpload, logger), testHostedToken)
	r.Mount("/npm", http.StripPrefix("/npm", handler.NewNPMHandler(proxy, "http://proxy.test", upstream.URL).Routes()))
	composer := handler.NewComposerHandlerWithUpstreams(proxy, "http://proxy.test", upstream.URL, upstream.URL)
	r.Mount("/composer", http.StripPrefix("/composer", composer.Routes()))
	return hostedTestServer{router: r, fetches: &calls, upstreamRequests: &upstreamRequests}
}

func (s hostedTestServer) do(t *testing.T, method, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec
}

func TestHostedPushIsServedByTheEcosystemHandler(t *testing.T) {
	s := newHostedTestServer(t)

	rec := s.do(t, http.MethodPut, "/api/hosted/npm/request/2.88.3", testHostedToken, pushedBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, body %s", rec.Code, rec.Body)
	}
	var pushed HostedPushResponse
	if err := json.NewDecoder(rec.Body).Decode(&pushed); err != nil {
		t.Fatalf("decoding PUT response: %v", err)
	}
	if pushed.Ecosystem != "npm" || pushed.Name != "request" || pushed.Version != "2.88.3" ||
		pushed.Filename != "request-2.88.3.tgz" || pushed.Size != int64(len(pushedBody)) {
		t.Errorf("PUT response = %+v", pushed)
	}

	rec = s.do(t, http.MethodGet, "/npm/request/-/request-2.88.3.tgz", "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != pushedBody {
		t.Fatalf("download status = %d, body %q, want 200 and the pushed tarball", rec.Code, rec.Body)
	}
	if *s.fetches != 0 {
		t.Errorf("download contacted the upstream %d times", *s.fetches)
	}
}

func TestHostedPushIsServedByTheComposerHandler(t *testing.T) {
	s := newHostedTestServer(t)

	rec := s.do(t, http.MethodPut, "/api/hosted/composer/monolog/monolog/3.5.0", testHostedToken, pushedBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, body %s", rec.Code, rec.Body)
	}

	rec = s.do(t, http.MethodGet, "/composer/files/monolog/monolog/3.5.0/monolog-3.5.0.zip", "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != pushedBody {
		t.Fatalf("download status = %d, body %q, want 200 and the pushed archive", rec.Code, rec.Body)
	}
	if *s.upstreamRequests != 0 || *s.fetches != 0 {
		t.Errorf("download contacted the upstream (%d metadata requests, %d fetches)", *s.upstreamRequests, *s.fetches)
	}
}

func TestComposerDownloadOfUnpushedFileStillNeedsUpstreamMetadata(t *testing.T) {
	s := newHostedTestServer(t)
	if rec := s.do(t, http.MethodPut, "/api/hosted/composer/monolog/monolog/3.5.0", testHostedToken, pushedBody); rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, body %s", rec.Code, rec.Body)
	}

	rec := s.do(t, http.MethodGet, "/composer/files/monolog/monolog/3.5.0/other.zip", "", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("download of another filename status = %d, want 404 from the upstream lookup", rec.Code)
	}
	if *s.upstreamRequests == 0 {
		t.Error("a filename that was not pushed was served without the upstream metadata lookup")
	}
}

func TestHostedPushScopedNpmPackage(t *testing.T) {
	s := newHostedTestServer(t)

	rec := s.do(t, http.MethodPut, "/api/hosted/npm/@types/node/20.1.0", testHostedToken, pushedBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, body %s", rec.Code, rec.Body)
	}
	rec = s.do(t, http.MethodGet, "/npm/@types/node/-/node-20.1.0.tgz", "", "")
	if rec.Code != http.StatusOK || rec.Body.String() != pushedBody {
		t.Fatalf("download status = %d, body %q, want 200 and the pushed tarball", rec.Code, rec.Body)
	}
}

func TestHostedListAndDelete(t *testing.T) {
	s := newHostedTestServer(t)
	if rec := s.do(t, http.MethodPut, "/api/hosted/npm/request/2.88.3", testHostedToken, pushedBody); rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, body %s", rec.Code, rec.Body)
	}

	rec := s.do(t, http.MethodGet, "/api/hosted/npm/request", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body %s", rec.Code, rec.Body)
	}
	var listed HostedListResponse
	if err := json.NewDecoder(rec.Body).Decode(&listed); err != nil {
		t.Fatalf("decoding GET response: %v", err)
	}
	if listed.Ecosystem != "npm" || listed.Name != "request" || len(listed.Artifacts) != 1 || listed.Artifacts[0].Version != "2.88.3" {
		t.Errorf("GET response = %+v", listed)
	}

	if rec := s.do(t, http.MethodDelete, "/api/hosted/npm/request/2.88.3", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("DELETE without token status = %d, want 401", rec.Code)
	}
	if rec := s.do(t, http.MethodDelete, "/api/hosted/npm/request/2.88.3", testHostedToken, ""); rec.Code != http.StatusNoContent {
		t.Errorf("DELETE status = %d, body %s", rec.Code, rec.Body)
	}
	if rec := s.do(t, http.MethodDelete, "/api/hosted/npm/request/2.88.3", testHostedToken, ""); rec.Code != http.StatusNotFound {
		t.Errorf("second DELETE status = %d, want 404", rec.Code)
	}
}

func TestHostedPushRejections(t *testing.T) {
	tests := []struct {
		name       string
		target     string
		token      string
		body       string
		wantStatus int
	}{
		{name: "no token", target: "/api/hosted/npm/request/2.88.3", body: pushedBody, wantStatus: http.StatusUnauthorized},
		{name: "wrong token", target: "/api/hosted/npm/request/2.88.3", token: "guess", body: pushedBody, wantStatus: http.StatusUnauthorized},
		{name: "unsupported ecosystem", target: "/api/hosted/cargo/serde/1.0.200", token: testHostedToken, body: pushedBody, wantStatus: http.StatusBadRequest},
		{name: "composer name without vendor", target: "/api/hosted/composer/monolog/3.5.0", token: testHostedToken, body: pushedBody, wantStatus: http.StatusBadRequest},
		{name: "missing version", target: "/api/hosted/npm/request", token: testHostedToken, body: pushedBody, wantStatus: http.StatusBadRequest},
		{name: "empty body", target: "/api/hosted/npm/request/2.88.3", token: testHostedToken, wantStatus: http.StatusBadRequest},
		{name: "too large", target: "/api/hosted/npm/request/2.88.3", token: testHostedToken, body: strings.Repeat("x", testHostedMaxUpload+1), wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newHostedTestServer(t)
			rec := s.do(t, http.MethodPut, tt.target, tt.token, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("PUT status = %d, want %d, body %s", rec.Code, tt.wantStatus, rec.Body)
			}
			listed := s.do(t, http.MethodGet, "/api/hosted/npm/request", "", "")
			if strings.Contains(listed.Body.String(), "2.88.3") {
				t.Error("rejected push was stored")
			}
		})
	}
}

func TestHostedListUnsupportedEcosystem(t *testing.T) {
	s := newHostedTestServer(t)
	if rec := s.do(t, http.MethodGet, "/api/hosted/maven/junit", "", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("GET status = %d, want 400", rec.Code)
	}
}

func startHostedServer(t *testing.T, hosted config.HostedConfig) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving proxy address: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	tempDir := t.TempDir()
	cfg := config.Default()
	cfg.Listen = listener.Addr().String()
	cfg.BaseURL = "http://" + cfg.Listen
	cfg.Database.Path = filepath.Join(tempDir, "proxy.db")
	cfg.Storage.URL = "file://" + filepath.Join(tempDir, "artifacts")
	cfg.Hosted = hosted
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validating config: %v", err)
	}

	proxyServer, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), BuildInfo{Version: "test", Commit: "test"})
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}
	startErr := make(chan error, 1)
	go func() { startErr <- proxyServer.Start(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proxyServer.Shutdown(ctx)
		<-startErr
	})
	return cfg.BaseURL
}

func putArchive(t *testing.T, url, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(pushedBody))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestHostedRoutesFollowConfig(t *testing.T) {
	disabled := startHostedServer(t, config.HostedConfig{})
	if status := putArchive(t, disabled+"/api/hosted/npm/request/2.88.3", testHostedToken); status == http.StatusCreated {
		t.Error("push accepted with hosted.enabled false")
	}

	enabled := startHostedServer(t, config.HostedConfig{Enabled: true, Token: testHostedToken})
	if status := putArchive(t, enabled+"/api/hosted/npm/request/2.88.3", ""); status != http.StatusUnauthorized {
		t.Errorf("push without token status = %d, want 401", status)
	}
	if status := putArchive(t, enabled+"/api/hosted/npm/request/2.88.3", testHostedToken); status != http.StatusCreated {
		t.Errorf("push with token status = %d, want 201", status)
	}
}
