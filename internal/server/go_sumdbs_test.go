package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/go-chi/chi/v5"
)

func TestGoSumDBsReachGoHandler(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lookup/golang.org/x/text@v0.14.0" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "lookup record")
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.BaseURL = "https://proxy.example"
	cfg.Upstream.GoSumDBs = map[string]string{"sum.example": upstream.URL}
	p := &handler.Proxy{HTTPClient: upstream.Client(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := &Server{cfg: cfg}
	r := chi.NewRouter()
	s.mountProtocolHandlers(r, p)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/go/sumdb/sum.example/lookup/golang.org/x/text@v0.14.0", nil))
	if w.Code != http.StatusOK || w.Body.String() != "lookup record" {
		t.Errorf("status = %d, body = %q; want the lookup from upstream.go_sumdbs", w.Code, w.Body.String())
	}
}
