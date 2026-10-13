package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireBearerToken(t *testing.T) {
	tests := []struct {
		name          string
		authorization string
		wantStatus    int
	}{
		{name: "missing header", authorization: "", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", authorization: "Bearer guess", wantStatus: http.StatusUnauthorized},
		{name: "token prefix only", authorization: "Bearer s3cre", wantStatus: http.StatusUnauthorized},
		{name: "basic scheme", authorization: "Basic czNjcmV0", wantStatus: http.StatusUnauthorized},
		{name: "empty bearer", authorization: "Bearer ", wantStatus: http.StatusUnauthorized},
		{name: "valid token", authorization: "Bearer s3cret", wantStatus: http.StatusNoContent},
		{name: "scheme is case-insensitive", authorization: "bearer s3cret", wantStatus: http.StatusNoContent},
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := requireBearerToken("s3cret")(next)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/packages/npm/request/2.88.3", nil)
			if tt.authorization != "" {
				req.Header.Set("Authorization", tt.authorization)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus != http.StatusUnauthorized {
				return
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="proxy"` {
				t.Errorf("WWW-Authenticate = %q, want Bearer realm=\"proxy\"", got)
			}
			var body ErrorResponse
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decoding error body: %v", err)
			}
			if body.Code != ErrCodeUnauthorized {
				t.Errorf("error code = %q, want %q", body.Code, ErrCodeUnauthorized)
			}
		})
	}
}
