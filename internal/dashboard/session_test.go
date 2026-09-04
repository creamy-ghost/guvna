package dashboard

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testDashUser = "admin"
	testDashPass = "test-dash-pass"
)

// httptestServer serves a Dashboard and returns its URL.
func httptestServer(t *testing.T, d *Dashboard) string {
	t.Helper()
	s := httptest.NewServer(d.Handler())
	t.Cleanup(s.Close)
	return s.URL
}

// login performs the login form flow and returns a client holding the session.
func login(t *testing.T, dsURL string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	c := &http.Client{Jar: jar}
	resp, err := c.PostForm(dsURL+"/login", url.Values{"user": {testDashUser}, "password": {testDashPass}})
	if err != nil {
		t.Fatalf("login POST: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("login landed on %d, want 200 after redirect to /", resp.StatusCode)
	}
	return c
}

func cget(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestLoginPage(t *testing.T) {
	d, err := New("http://127.0.0.1:1", "k", testDashUser, testDashPass)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := httptestServer(t, d)
	code, body := get(t, s+"/login")
	if code != 200 || !strings.Contains(body, `type="password"`) {
		t.Fatalf("login page = %d, has-password=%v", code, strings.Contains(body, "password"))
	}
	// Already-authed visits bounce to /.
	c := login(t, s)
	resp, err := c.Get(s + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	defer resp.Body.Close()
	// jar client follows the 302; final page is the dashboard.
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "partial/overview") {
		t.Errorf("authed /login should land on dashboard")
	}
}

func TestLoginRejectsBadPassword(t *testing.T) {
	d, err := New("http://127.0.0.1:1", "k", testDashUser, testDashPass)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := httptestServer(t, d)
	resp, err := http.PostForm(s+"/login", url.Values{"user": {"admin"}, "password": {"nope"}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "wrong username or password") {
		t.Errorf("expected error text, got %q", b)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Errorf("failed login must not set a session")
		}
	}
}

func TestGuards(t *testing.T) {
	gw := &fakeGateway{status: testStatus}
	_, _, ds := newTestDashboard(t, gw)

	// Plain navigation → redirect to /login.
	plain := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := plain.Get(ds.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/login" {
		t.Errorf("/ = %d -> %q, want 302 -> /login", resp.StatusCode, resp.Header.Get("Location"))
	}

	// HTMX partial → 401 + HX-Redirect (a 302 would swap login HTML into the tab).
	req, _ := http.NewRequest(http.MethodGet, ds.URL+"/partial/overview", nil)
	req.Header.Set("HX-Request", "true")
	resp2, err := plain.Do(req)
	if err != nil {
		t.Fatalf("partial: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 401 || resp2.Header.Get("HX-Redirect") != "/login" {
		t.Errorf("partial = %d HX-Redirect=%q", resp2.StatusCode, resp2.Header.Get("HX-Redirect"))
	}

	// Authed client passes through.
	c := login(t, ds.URL)
	if code, _ := cget(t, c, ds.URL+"/partial/overview"); code != 200 {
		t.Errorf("authed partial = %d", code)
	}
}

func TestLogout(t *testing.T) {
	d, err := New("http://127.0.0.1:1", "k", testDashUser, testDashPass)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := httptestServer(t, d)
	c := login(t, s)
	resp, err := c.PostForm(s+"/logout", url.Values{})
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	resp.Body.Close()
	// The jar drops the expired cookie, so / bounces to the login page
	// (rendered 200 after the client follows the redirect).
	resp2, err := c.Get(s + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp2.Body.Close()
	b, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(b), `type="password"`) {
		t.Errorf("after logout / should render login, got %q", string(b)[:min(120, len(b))])
	}
}

func TestLoginRateLimit(t *testing.T) {
	d, err := New("http://127.0.0.1:1", "k", testDashUser, testDashPass)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := httptestServer(t, d)
	var last int
	for i := 0; i < maxAttempts+2; i++ {
		resp, err := http.PostForm(s+"/login", url.Values{"user": {"admin"}, "password": {"nope"}})
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("after %d bad attempts last status = %d, want 429", maxAttempts+2, last)
	}
}

func TestSessionCrypto(t *testing.T) {
	a := newSessionAuth("admin", "s3cret")
	now := time.Now()
	tok := a.issue("admin", now)
	if !a.valid(tok, now.Add(time.Hour)) {
		t.Errorf("fresh token invalid")
	}
	if a.valid(tok, now.Add(sessionTTL+time.Hour)) {
		t.Errorf("expired token accepted")
	}
	if a.valid(tok+"x", now) {
		t.Errorf("tampered token accepted")
	}
	if a.valid("!!!not-base64!!!", now) {
		t.Errorf("garbage token accepted")
	}
	other := newSessionAuth("admin", "different")
	if other.valid(tok, now) {
		t.Errorf("token valid under different password")
	}
	if !a.checkPassword("admin", "s3cret") || a.checkPassword("admin", "wrong") || a.checkPassword("root", "s3cret") {
		t.Errorf("password check wrong")
	}
}
