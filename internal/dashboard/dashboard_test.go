package dashboard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeGateway serves canned gateway responses and records requests.
type fakeGateway struct {
	t        *testing.T
	status   string
	chains   string
	logs     string
	lastReq  *http.Request
	lastBody string
	postCode int
	postResp string
	delCode  int
	delResp  string
}

func (f *fakeGateway) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/status", func(w http.ResponseWriter, r *http.Request) {
		f.lastReq = r
		if f.status == "" {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(f.status))
	})
	mux.HandleFunc("/admin/logs", func(w http.ResponseWriter, r *http.Request) {
		f.lastReq = r
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(f.logs))
	})
	mux.HandleFunc("/v1/chains", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			f.lastReq = r
			b, _ := io.ReadAll(r.Body)
			f.lastBody = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			code := f.postCode
			if code == 0 {
				code = http.StatusCreated
			}
			w.WriteHeader(code)
			w.Write([]byte(f.postResp))
			return
		}
		w.Write([]byte(f.chains))
	})
	mux.HandleFunc("/v1/chains/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.lastReq = r
		if f.delCode != 0 {
			w.WriteHeader(f.delCode)
			w.Write([]byte(f.delResp))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func newTestDashboard(t *testing.T, gw *fakeGateway) (*Dashboard, *httptest.Server, *httptest.Server) {
	t.Helper()
	gw.t = t
	gws := httptest.NewServer(gw.handler())
	d, err := New(gws.URL, "test-admin-key")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ds := httptest.NewServer(d.Handler())
	t.Cleanup(gws.Close)
	t.Cleanup(ds.Close)
	return d, gws, ds
}

const testStatus = `{"uptime":"1h2m3s","chains":["myfree"],"steps":[
	{"provider":"groq","model":"llama","failures":0,"down":false,"down_for_ns":0},
	{"provider":"bazaarlink","model":"qwen","failures":4,"down":true,"down_for_ns":45000000000}],
	"steps_down":1,
	"keys":[{"provider":"groq","index":0,"env":"GROQ_1_KEY","state":"healthy","reason":"","failures":0,"requests":10,"tokens_in":100,"tokens_out":50},
	{"provider":"bazaarlink","index":1,"env":"BAZAARLINK_2_KEY","state":"quarantined","reason":"auth","failures":2,"requests":5,"tokens_in":10,"tokens_out":0}],
	"usage":{"requests":12,"streams":3,"errors":1,"tokens_in":200,"tokens_out":90,
	"by_provider":[{"provider":"groq","requests":12,"errors":1,"tokens_in":200,"tokens_out":90}]}}`

const testChains = `{"chains":[
	{"name":"myfree","source":"runtime","steps":[{"provider":"groq","model":"llama"}]},
	{"name":"cfg","source":"config","steps":[{"provider":"mistral","model":"codestral-latest"}]}]}`

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestHealthz(t *testing.T) {
	d, err := New("http://127.0.0.1:1", "k")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := httptest.NewServer(d.Handler())
	defer s.Close()
	code, body := get(t, s.URL+"/healthz")
	if code != 200 || !strings.Contains(body, `"ok"`) {
		t.Fatalf("healthz = %d %q", code, body)
	}
}

func TestOverviewRenders(t *testing.T) {
	gw := &fakeGateway{status: testStatus}
	_, _, ds := newTestDashboard(t, gw)
	code, body := get(t, ds.URL+"/partial/overview")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{"1h2m3s", "myfree", "groq", "bazaarlink", "12", "200 / 90"} {
		if !strings.Contains(body, want) {
			t.Errorf("overview missing %q", want)
		}
	}
}

func TestOverviewGatewayDown(t *testing.T) {
	gw := &fakeGateway{} // empty status = 500
	_, _, ds := newTestDashboard(t, gw)
	code, body := get(t, ds.URL+"/partial/overview")
	if code != 200 {
		t.Fatalf("partial must degrade to 200 + error box, got %d", code)
	}
	if !strings.Contains(body, "gateway") {
		t.Errorf("expected error box, got %q", body)
	}
}

func TestKeysRenders(t *testing.T) {
	gw := &fakeGateway{status: testStatus}
	_, _, ds := newTestDashboard(t, gw)
	_, body := get(t, ds.URL+"/partial/keys")
	for _, want := range []string{"GROQ_1_KEY", "healthy", "quarantined", "down"} {
		if !strings.Contains(body, want) {
			t.Errorf("keys missing %q", want)
		}
	}
}

func TestLogsClampN(t *testing.T) {
	gw := &fakeGateway{logs: `{"lines":["a","b"]}`}
	_, _, ds := newTestDashboard(t, gw)
	for path, wantN := range map[string]string{
		"/partial/logs":        "n=100",
		"/partial/logs?n=256":  "n=256",
		"/partial/logs?n=9999": "n=512",
		"/partial/logs?n=abc":  "n=100",
		"/partial/logs?n=-5":   "n=100",
		"/partial/logs?n=512":  "n=512",
	} {
		_, body := get(t, ds.URL+path)
		if !strings.Contains(body, "n="+strings.TrimPrefix(wantN, "n=")) {
			t.Errorf("%s: poller missing %q", path, wantN)
		}
		if !strings.Contains(body, "a") {
			t.Errorf("%s: missing log lines", path)
		}
	}
	if q := gw.lastReq.URL.Query().Get("n"); q != "512" {
		// last request in map order is random; just verify clamping happened
		// at least once by re-requesting the overflow case.
		_, _ = get(t, ds.URL+"/partial/logs?n=9999")
		if q := gw.lastReq.URL.Query().Get("n"); q != "512" {
			t.Errorf("gateway got n=%q, want 512", q)
		}
	}
}

func TestChainsView(t *testing.T) {
	gw := &fakeGateway{chains: testChains}
	_, _, ds := newTestDashboard(t, gw)
	_, body := get(t, ds.URL+"/partial/chains")
	for _, want := range []string{"myfree", "cfg", "groq:llama", "locked"} {
		if !strings.Contains(body, want) {
			t.Errorf("chains missing %q", want)
		}
	}
	// runtime chain gets a delete button, config chain doesn't
	if !strings.Contains(body, `hx-delete="/chains/myfree"`) {
		t.Errorf("missing delete button for runtime chain")
	}
	if strings.Contains(body, `hx-delete="/chains/cfg"`) {
		t.Errorf("config chain must not be deletable")
	}
}

func TestChainCreateSuccess(t *testing.T) {
	gw := &fakeGateway{chains: testChains, postCode: 201, postResp: `{"name":"n","source":"runtime","steps":[]}`}
	_, _, ds := newTestDashboard(t, gw)
	form := url.Values{"name": {"newchain"}, "steps": {"groq:llama\nmistral:codestral-latest"}}
	resp, err := http.PostForm(ds.URL+"/chains", form)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if strings.Contains(body, "value=\"newchain\"") {
		t.Errorf("form should clear on success, got %q", body)
	}
	if !strings.Contains(gw.lastBody, `"name":"newchain"`) {
		t.Errorf("gateway got %q", gw.lastBody)
	}
	if !strings.Contains(gw.lastBody, `"provider":"mistral","model":"codestral-latest"`) {
		t.Errorf("steps not forwarded as JSON: %q", gw.lastBody)
	}
}

func TestChainCreateValidation(t *testing.T) {
	gw := &fakeGateway{chains: testChains}
	_, _, ds := newTestDashboard(t, gw)
	form := url.Values{"name": {"bad"}, "steps": {"not-a-step"}}
	resp, err := http.PostForm(ds.URL+"/chains", form)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if !strings.Contains(body, "provider:model") {
		t.Errorf("expected validation error, got %q", body)
	}
	if !strings.Contains(body, "value=\"bad\"") {
		t.Errorf("form input should be preserved on error")
	}
	if gw.lastBody != "" {
		t.Errorf("gateway must not be called on validation error, got %q", gw.lastBody)
	}
}

func TestChainCreateGatewayConflict(t *testing.T) {
	gw := &fakeGateway{
		chains:   testChains,
		postCode: 409,
		postResp: `{"error":{"message":"chain \"myfree\" already exists"}}`,
	}
	_, _, ds := newTestDashboard(t, gw)
	form := url.Values{"name": {"myfree"}, "steps": {"groq:llama"}}
	resp, err := http.PostForm(ds.URL+"/chains", form)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if body := string(b); !strings.Contains(body, "already exists") {
		t.Errorf("gateway error should surface, got %q", body)
	}
}

func TestChainDelete(t *testing.T) {
	gw := &fakeGateway{chains: testChains}
	_, _, ds := newTestDashboard(t, gw)
	req, _ := http.NewRequest(http.MethodDelete, ds.URL+"/chains/myfree", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if body := string(b); !strings.Contains(body, "myfree") {
		t.Errorf("expected re-rendered list, got %q", body)
	}
	if gw.lastReq.URL.Path != "/v1/chains/myfree" {
		t.Errorf("gateway path = %q", gw.lastReq.URL.Path)
	}
}

func TestChainDeleteGatewayError(t *testing.T) {
	gw := &fakeGateway{chains: testChains, delCode: 404, delResp: `{"error":{"message":"chain not found"}}`}
	_, _, ds := newTestDashboard(t, gw)
	req, _ := http.NewRequest(http.MethodDelete, ds.URL+"/chains/ghost", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if body := string(b); !strings.Contains(body, "not found") {
		t.Errorf("expected error banner, got %q", body)
	}
}

func TestParseSteps(t *testing.T) {
	steps, err := parseSteps("groq:llama\n  mistral:codestral-latest\n\n")
	if err != nil {
		t.Fatalf("parseSteps: %v", err)
	}
	if len(steps) != 2 || steps[0].Provider != "groq" || steps[1].Model != "codestral-latest" {
		t.Fatalf("steps = %+v", steps)
	}
	for _, bad := range []string{"", "\n  \n", "nostep", ":model", "provider:"} {
		if _, err := parseSteps(bad); err == nil {
			t.Errorf("parseSteps(%q) should fail", bad)
		}
	}
}

func TestStaticVendored(t *testing.T) {
	d, err := New("http://127.0.0.1:1", "k")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := httptest.NewServer(d.Handler())
	defer s.Close()
	for _, p := range []string{"/static/htmx.min.js", "/static/pico.min.css", "/static/custom.css"} {
		code, body := get(t, s.URL+p)
		if code != 200 || len(body) < 100 {
			t.Errorf("%s = %d (%d bytes)", p, code, len(body))
		}
	}
}

func TestAdminKeyForwarded(t *testing.T) {
	gw := &fakeGateway{status: testStatus}
	_, _, ds := newTestDashboard(t, gw)
	_, _ = get(t, ds.URL+"/partial/overview")
	if h := gw.lastReq.Header.Get("Authorization"); h != "Bearer test-admin-key" {
		t.Errorf("Authorization = %q", h)
	}
}

func TestPageRenders(t *testing.T) {
	d, err := New("http://127.0.0.1:1", "k")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := httptest.NewServer(d.Handler())
	defer s.Close()
	_, body := get(t, s.URL+"/")
	for _, want := range []string{"/static/htmx.min.js", "/static/pico.min.css", `hx-get="/partial/chains"`, `hx-get="/partial/logs`} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}
