package dashboard

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Session auth for the dashboard: a styled login page issuing a signed
// cookie, instead of the browser's native basic-auth popup (which can't be
// branded and trains users to paste passwords into chrome).
//
// No credentials ever reach the browser. The session key is derived from
// the dashboard password itself, so changing the password invalidates all
// sessions. stdlib only: SHA-256 + subtle compare for the password (same
// pattern as internal/auth), HMAC-SHA256 for the cookie.

const (
	sessionCookie = "guvna_dash"
	sessionTTL    = 30 * 24 * time.Hour

	// attemptWindow caps password guesses per client IP: brute force over
	// the internet is the main threat once the login page is public.
	attemptWindow = time.Minute
	maxAttempts   = 10
)

// sessionAuth guards the UI routes. Zero value is unusable; build with newSessionAuth.
type sessionAuth struct {
	user     string
	passHash [32]byte

	mu       sync.Mutex
	attempts map[string]*attempt
}

type attempt struct {
	count int
	since time.Time
}

func newSessionAuth(user, password string) *sessionAuth {
	return &sessionAuth{user: user, passHash: sha256.Sum256([]byte(password)), attempts: make(map[string]*attempt)}
}

// checkPassword compares in constant time (length folded in, like internal/auth).
func (s *sessionAuth) checkPassword(user, password string) bool {
	if subtle.ConstantTimeCompare([]byte(user), []byte(s.user)) != 1 {
		// Still hash the guess so user enumeration isn't timing-cheap.
		_ = sha256.Sum256([]byte(password))
		return false
	}
	got := sha256.Sum256([]byte(password))
	return subtle.ConstantTimeCompare(got[:], s.passHash[:]) == 1
}

// issue returns a signed session cookie value for user valid for sessionTTL.
func (s *sessionAuth) issue(user string, now time.Time) string {
	exp := now.Add(sessionTTL).Unix()
	payload := user + "|" + strconv.FormatInt(exp, 10)
	mac := hmac.New(sha256.New, s.passHash[:])
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload + "|" + fmt.Sprintf("%x", mac.Sum(nil))))
}

// valid reports whether cookieValue is an unexpired session we issued.
func (s *sessionAuth) valid(cookieValue string, now time.Time) bool {
	raw, err := base64.RawURLEncoding.DecodeString(cookieValue)
	if err != nil {
		return false
	}
	user, expStr, sig, ok := cut2(string(raw))
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || now.Unix() > exp {
		return false
	}
	mac := hmac.New(sha256.New, s.passHash[:])
	mac.Write([]byte(user + "|" + expStr))
	want := fmt.Sprintf("%x", mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(sig), []byte(want)) == 1 && user == s.user
}

func cut2(v string) (a, b, c string, ok bool) {
	i := strings.IndexByte(v, '|')
	if i < 0 {
		return "", "", "", false
	}
	j := strings.LastIndexByte(v, '|')
	if j <= i {
		return "", "", "", false
	}
	return v[:i], v[i+1 : j], v[j+1:], true
}

// authed upstream: cookie present and valid.
func (s *sessionAuth) authed(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return s.valid(c.Value, time.Now())
}

// setCookie writes (or, when value is "", clears) the session cookie.
// Secure follows the request scheme so plain-HTTP local dev keeps working
// while Caddy-terminated production (X-Forwarded-Proto: https) is strict.
func setCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// requireAuth wraps UI handlers. HTMX partial requests get HX-Redirect
// (a 302 inside a swap would render the login page into the tab); plain
// navigation gets a normal redirect to /login.
func (s *sessionAuth) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authed(r) {
			next(w, r)
			return
		}
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/login")
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

// clientIP reduces RemoteAddr to a host for attempt counting.
func clientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// allowAttempt records a failed login and reports whether the client may
// keep trying. Successes call resetAttempts.
func (s *sessionAuth) allowAttempt(ip string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[ip]
	if !ok || now.Sub(a.since) > attemptWindow {
		a = &attempt{since: now}
		s.attempts[ip] = a
	}
	a.count++
	return a.count <= maxAttempts
}

func (s *sessionAuth) resetAttempts(ip string) {
	s.mu.Lock()
	delete(s.attempts, ip)
	s.mu.Unlock()
}
