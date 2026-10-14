package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

const bearerScheme = "bearer "

// requireBearerToken rejects requests that do not carry
// "Authorization: Bearer <token>". Both tokens are hashed before comparing so
// the check takes the same time whatever the length of the presented token.
func requireBearerToken(token string) func(http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented, ok := bearerToken(r)
			got := sha256.Sum256([]byte(presented))
			if !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="proxy"`)
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "a valid bearer token is required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if len(header) <= len(bearerScheme) || !strings.EqualFold(header[:len(bearerScheme)], bearerScheme) {
		return "", false
	}
	return header[len(bearerScheme):], true
}
