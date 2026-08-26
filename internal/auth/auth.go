package auth

import (
	"net/http"
	"strings"
)

// Authenticator validates Bearer tokens against an admin key and a set of client keys.
type Authenticator struct {
	adminKey string
	client   map[string]bool
}

// New builds an Authenticator. adminKey comes from the ADMIN_KEY env; clientKeys
// is the set of valid client API keys. All are compared with constant-time equality.
func New(adminKey string, clientKeys []string) *Authenticator {
	client := make(map[string]bool, len(clientKeys))
	for _, k := range clientKeys {
		if k != "" {
			client[k] = true
		}
	}
	return &Authenticator{adminKey: adminKey, client: client}
}

func (a *Authenticator) Valid(token string) bool {
	if a.adminKey != "" && constantTimeEqual(a.adminKey, token) {
		return true
	}
	return a.client[token]
}

// Middleware wraps a handler, requiring a valid Bearer token.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r.Header.Get("Authorization"))
		if !ok || !a.Valid(token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="guvna"`)
			http.Error(w, `{"error":{"message":"invalid api key","type":"invalid_request_error"}}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// AdminMiddleware is like Middleware but only the admin key passes. Used for
// the admin surface (/admin/*); client keys cannot read it.
func (a *Authenticator) AdminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r.Header.Get("Authorization"))
		if !ok || !a.isAdmin(token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="guvna"`)
			http.Error(w, `{"error":{"message":"admin key required"}}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Authenticator) isAdmin(token string) bool {
	return a.adminKey != "" && constantTimeEqual(a.adminKey, token)
}

func bearer(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(prefix):]), true
}

// constantTimeEqual avoids leaking key lengths via early exit on length mismatch,
// and timing information via early exit on byte mismatch.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
