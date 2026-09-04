// Package dashboard is the optional Guvna web UI: a separate binary that
// proxies a live gateway over HTTP and renders server-side HTML partials
// (Go html/template + HTMX, no client state, no build step).
//
// It never imports internal/server — the gateway is spoken to over HTTP
// only, so core can't regress. The admin key lives server-side in env;
// browsers only ever see rendered HTML.
package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creamy-ghost/guvna/web"
)

// statusCacheTTL coalesces concurrent tab polls into one gateway request.
const statusCacheTTL = 2 * time.Second

// maxBody caps proxied gateway responses (status payloads are small).
const maxBody = 1 << 20

// Dashboard proxies a Guvna gateway and renders the UI.
type Dashboard struct {
	gateway  string
	adminKey string
	client   *http.Client
	tmpl     *template.Template
	auth     *sessionAuth

	mu         sync.Mutex
	statusBody []byte
	statusAt   time.Time
}

// New builds a Dashboard targeting gateway (base URL, no trailing slash)
// with adminKey as the Bearer token for /admin/* and /v1/chains, and
// dashUser/dashPass gating the UI itself via the login page.
func New(gateway, adminKey, dashUser, dashPass string) (*Dashboard, error) {
	tmpl, err := template.ParseFS(web.FS,
		"templates/layout.html",
		"templates/login.html",
		"templates/partials/overview.html",
		"templates/partials/chains.html",
		"templates/partials/keys.html",
		"templates/partials/logs.html",
	)
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Dashboard{
		gateway:  strings.TrimRight(gateway, "/"),
		adminKey: adminKey,
		client:   &http.Client{Timeout: 10 * time.Second},
		tmpl:     tmpl,
		auth:     newSessionAuth(dashUser, dashPass),
	}, nil
}

// Handler wires the UI surface: page, partials, mutations, vendored static.
func (d *Dashboard) Handler() http.Handler {
	static, err := staticFS()
	if err != nil {
		panic(fmt.Sprintf("dashboard static: %v", err))
	}
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /login", d.handleLoginView)
	mux.HandleFunc("POST /login", d.handleLogin)
	mux.HandleFunc("POST /logout", d.handleLogout)
	mux.HandleFunc("GET /", d.auth.requireAuth(d.handlePage))
	mux.HandleFunc("GET /partial/overview", d.auth.requireAuth(d.handleOverview))
	mux.HandleFunc("GET /partial/keys", d.auth.requireAuth(d.handleKeys))
	mux.HandleFunc("GET /partial/logs", d.auth.requireAuth(d.handleLogs))
	mux.HandleFunc("GET /partial/chains", d.auth.requireAuth(d.handleChainsView))
	mux.HandleFunc("POST /chains", d.auth.requireAuth(d.handleChainCreate))
	mux.HandleFunc("DELETE /chains/{name}", d.auth.requireAuth(d.handleChainDelete))
	return mux
}

func (d *Dashboard) handlePage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	d.tmpl.ExecuteTemplate(w, "layout", map[string]string{"Gateway": d.gateway})
}

// loginView renders the login page; Error is empty on first view.
type loginView struct {
	Error string
	User  string
}

func (d *Dashboard) handleLoginView(w http.ResponseWriter, r *http.Request) {
	if d.auth.authed(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	d.tmpl.ExecuteTemplate(w, "login", loginView{})
}

func (d *Dashboard) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		d.tmpl.ExecuteTemplate(w, "login", loginView{Error: "bad form submission"})
		return
	}
	user := r.FormValue("user")
	if !d.auth.allowAttempt(clientIP(r), time.Now()) {
		http.Error(w, "too many attempts, try again in a minute", http.StatusTooManyRequests)
		return
	}
	if !d.auth.checkPassword(user, r.FormValue("password")) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		d.tmpl.ExecuteTemplate(w, "login", loginView{Error: "wrong username or password", User: user})
		return
	}
	d.auth.resetAttempts(clientIP(r))
	setCookie(w, r, d.auth.issue(user, time.Now()), int(sessionTTL.Seconds()))
	http.Redirect(w, r, "/", http.StatusFound)
}

func (d *Dashboard) handleLogout(w http.ResponseWriter, r *http.Request) {
	setCookie(w, r, "", -1)
	http.Redirect(w, r, "/login", http.StatusFound)
}

// gatewayDo performs an authenticated request against the gateway and
// returns status, capped body, and transport error separately so callers
// can distinguish "gateway down" from "gateway said no".
func (d *Dashboard) gatewayDo(method, path string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, d.gateway+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.adminKey)
	req.Header.Set("User-Agent", "guvna-dashboard/0.1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, b, nil
}

// gatewayError extracts a human message from a gateway error body:
// {"error":{"message":"..."}} — falls back to the HTTP status.
func gatewayError(status int, body []byte) string {
	var eg struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &eg) == nil && eg.Error.Message != "" {
		return fmt.Sprintf("gateway: %s (HTTP %d)", eg.Error.Message, status)
	}
	return fmt.Sprintf("gateway: HTTP %d", status)
}

// status fetches /admin/status with a short cache so concurrent tab polls
// coalesce into one upstream request.
func (d *Dashboard) status() (statusResp, error) {
	var s statusResp
	d.mu.Lock()
	if time.Since(d.statusAt) < statusCacheTTL && d.statusBody != nil {
		cached := d.statusBody
		d.mu.Unlock()
		if err := json.Unmarshal(cached, &s); err != nil {
			return s, err
		}
		return s, nil
	}
	d.mu.Unlock()

	code, body, err := d.gatewayDo(http.MethodGet, "/admin/status", nil)
	if err != nil {
		return s, fmt.Errorf("gateway unreachable: %w", err)
	}
	if code < 200 || code > 299 {
		return s, fmt.Errorf("%s", gatewayError(code, body))
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return s, fmt.Errorf("bad status payload: %w", err)
	}
	d.mu.Lock()
	d.statusBody = body
	d.statusAt = time.Now()
	d.mu.Unlock()
	return s, nil
}

// chains fetches the chain list. Uncached — mutations must read fresh.
func (d *Dashboard) chains() (chainList, error) {
	var cl chainList
	code, body, err := d.gatewayDo(http.MethodGet, "/v1/chains", nil)
	if err != nil {
		return cl, fmt.Errorf("gateway unreachable: %w", err)
	}
	if code < 200 || code > 299 {
		return cl, fmt.Errorf("%s", gatewayError(code, body))
	}
	if err := json.Unmarshal(body, &cl); err != nil {
		return cl, fmt.Errorf("bad chains payload: %w", err)
	}
	return cl, nil
}

func (d *Dashboard) handleOverview(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s, err := d.status()
	if err != nil {
		d.tmpl.ExecuteTemplate(w, "overview", overviewView{Error: err.Error()})
		return
	}
	v := overviewView{
		Uptime:    s.Uptime,
		Chains:    append([]string(nil), s.Chains...),
		StepsDown: s.StepsDown,
		Usage:     s.Usage,
	}
	sort.Strings(v.Chains)
	for _, st := range s.Steps {
		if st.Down {
			v.DownSteps = append(v.DownSteps, stepView{
				Provider: st.Provider,
				Model:    st.Model,
				Failures: st.Failures,
				DownFor:  fmtDuration(st.DownForNs),
			})
		}
	}
	sort.Slice(v.DownSteps, func(i, j int) bool {
		if v.DownSteps[i].Provider != v.DownSteps[j].Provider {
			return v.DownSteps[i].Provider < v.DownSteps[j].Provider
		}
		return v.DownSteps[i].Model < v.DownSteps[j].Model
	})
	d.tmpl.ExecuteTemplate(w, "overview", v)
}

func (d *Dashboard) handleKeys(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s, err := d.status()
	if err != nil {
		d.tmpl.ExecuteTemplate(w, "keys", keysView{Error: err.Error()})
		return
	}
	v := keysView{Keys: append([]keyStatus(nil), s.Keys...)}
	for _, st := range s.Steps {
		v.Steps = append(v.Steps, stepView{
			Provider: st.Provider,
			Model:    st.Model,
			Failures: st.Failures,
			Down:     st.Down,
			DownFor:  fmtDuration(st.DownForNs),
		})
	}
	sort.Slice(v.Keys, func(i, j int) bool {
		if v.Keys[i].Provider != v.Keys[j].Provider {
			return v.Keys[i].Provider < v.Keys[j].Provider
		}
		return v.Keys[i].Index < v.Keys[j].Index
	})
	sort.Slice(v.Steps, func(i, j int) bool {
		if v.Steps[i].Provider != v.Steps[j].Provider {
			return v.Steps[i].Provider < v.Steps[j].Provider
		}
		return v.Steps[i].Model < v.Steps[j].Model
	})
	d.tmpl.ExecuteTemplate(w, "keys", v)
}

func (d *Dashboard) handleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	n := 100
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 512 {
			n = parsed
		}
	}
	code, body, err := d.gatewayDo(http.MethodGet, "/admin/logs?n="+strconv.Itoa(n), nil)
	if err != nil {
		d.tmpl.ExecuteTemplate(w, "logs", logsView{N: n, Error: fmt.Sprintf("gateway unreachable: %v", err)})
		return
	}
	if code < 200 || code > 299 {
		d.tmpl.ExecuteTemplate(w, "logs", logsView{N: n, Error: gatewayError(code, body)})
		return
	}
	var out struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		d.tmpl.ExecuteTemplate(w, "logs", logsView{N: n, Error: "bad logs payload"})
		return
	}
	d.tmpl.ExecuteTemplate(w, "logs", logsView{N: n, Lines: out.Lines})
}

// chainsPage renders the chains tab: form state preserved across errors,
// cleared on success.
func (d *Dashboard) chainsPage(errMsg, formName, formSteps string) chainsView {
	v := chainsView{Error: errMsg, FormName: formName, FormSteps: formSteps}
	if cl, err := d.chains(); err != nil {
		if v.Error != "" {
			v.Error += "; " + err.Error()
		} else {
			v.Error = err.Error()
		}
	} else {
		v.Chains = cl.Chains
	}
	sort.Slice(v.Chains, func(i, j int) bool { return v.Chains[i].Name < v.Chains[j].Name })
	return v
}

func (d *Dashboard) handleChainsView(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	d.tmpl.ExecuteTemplate(w, "chains", d.chainsPage("", "", ""))
}

// handleChainCreate parses a name + newline-separated provider:model steps
// from the HTMX form and forwards them to POST /v1/chains. The full tab is
// re-rendered either way: inputs preserved on error, cleared on success.
func (d *Dashboard) handleChainCreate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := r.ParseForm(); err != nil {
		d.tmpl.ExecuteTemplate(w, "chains", d.chainsPage("bad form submission", "", ""))
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	raw := r.FormValue("steps")
	steps, err := parseSteps(raw)
	if err != nil {
		d.tmpl.ExecuteTemplate(w, "chains", d.chainsPage(err.Error(), name, raw))
		return
	}
	if name == "" {
		d.tmpl.ExecuteTemplate(w, "chains", d.chainsPage("chain name is required", name, raw))
		return
	}
	payload, _ := json.Marshal(map[string]any{"name": name, "steps": steps})
	code, body, gerr := d.gatewayDo(http.MethodPost, "/v1/chains", payload)
	if gerr != nil {
		d.tmpl.ExecuteTemplate(w, "chains", d.chainsPage(fmt.Sprintf("gateway unreachable: %v", gerr), name, raw))
		return
	}
	if code < 200 || code > 299 {
		d.tmpl.ExecuteTemplate(w, "chains", d.chainsPage(gatewayError(code, body), name, raw))
		return
	}
	d.tmpl.ExecuteTemplate(w, "chains", d.chainsPage("", "", ""))
}

// handleChainDelete forwards to DELETE /v1/chains/{name} and re-renders.
func (d *Dashboard) handleChainDelete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	name := r.PathValue("name")
	if name == "" {
		d.tmpl.ExecuteTemplate(w, "chains", d.chainsPage("missing chain name", "", ""))
		return
	}
	code, body, err := d.gatewayDo(http.MethodDelete, "/v1/chains/"+name, nil)
	if err != nil {
		d.tmpl.ExecuteTemplate(w, "chains", d.chainsPage(fmt.Sprintf("gateway unreachable: %v", err), "", ""))
		return
	}
	if code != http.StatusNoContent && (code < 200 || code > 299) {
		d.tmpl.ExecuteTemplate(w, "chains", d.chainsPage(gatewayError(code, body), "", ""))
		return
	}
	d.tmpl.ExecuteTemplate(w, "chains", d.chainsPage("", "", ""))
}

// parseSteps parses newline-separated provider:model lines into chain steps.
func parseSteps(raw string) ([]chainStep, error) {
	var steps []chainStep
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		p, m, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(p) == "" || strings.TrimSpace(m) == "" {
			return nil, fmt.Errorf("bad step %q — want provider:model", line)
		}
		steps = append(steps, chainStep{Provider: strings.TrimSpace(p), Model: strings.TrimSpace(m)})
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("at least one provider:model step is required")
	}
	return steps, nil
}

// fmtDuration renders nanoseconds as a human duration (gateway encodes
// time.Duration as an integer).
func fmtDuration(ns int64) string {
	if ns <= 0 {
		return "—"
	}
	return (time.Duration(ns)).Round(time.Second).String()
}
