package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/creamy-ghost/guvna/internal/auth"
	"github.com/creamy-ghost/guvna/internal/chains"
	"github.com/creamy-ghost/guvna/internal/config"
	"github.com/creamy-ghost/guvna/internal/health"
	"github.com/creamy-ghost/guvna/internal/keypool"
	"github.com/creamy-ghost/guvna/internal/logring"
	"github.com/creamy-ghost/guvna/internal/router"
	"github.com/creamy-ghost/guvna/internal/telemetry"
)

type testEnv map[string]string

func (e testEnv) get(k string) string { return e[k] }

func testServer(t *testing.T, providers []config.Provider, ch []config.Chain, env map[string]string) (*Server, *httptest.Server) {
	t.Helper()
	return testServerDefault(t, providers, ch, env, "main")
}

func testServerDefault(t *testing.T, providers []config.Provider, ch []config.Chain, env map[string]string, defaultChain string) (*Server, *httptest.Server) {
	t.Helper()
	cfg := &config.Config{Port: 20128, Providers: providers, Chains: ch, DefaultChain: defaultChain}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	rtr := router.New(cfg)
	a := auth.New("admin-secret", []string{"client-1"})
	tm, err := telemetry.Open(t.TempDir()+"/t.db", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tm.Close() })
	store := chains.New(t.TempDir())
	s := New(cfg, rtr, a, tm, health.New(), logring.New(64), store)
	s.env = testEnv(env).get
	s.retryWait = func(int) time.Duration { return 0 }
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func completionUpstream(t *testing.T, status int, body string, stream bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var got struct {
			Model string `json:"model"`
		}
		json.Unmarshal(raw, &got)
		if got.Model == "" {
			http.Error(w, `{"error":"missing model"}`, http.StatusBadRequest)
			return
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(status)
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
			w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
}

func doChat(t *testing.T, ts *httptest.Server, model string, stream bool) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": model, "stream": stream})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer client-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestChatFallsBackToSecondStep(t *testing.T) {
	bad := completionUpstream(t, http.StatusTooManyRequests, `{"error":"rate limited"}`, false)
	defer bad.Close()
	good := completionUpstream(t, http.StatusOK, `{"choices":[{}],"usage":{"prompt_tokens":5,"completion_tokens":9}}`, false)
	defer good.Close()

	providers := []config.Provider{
		{Name: "a", Type: "openai", BaseURL: bad.URL, KeyEnv: "KEY_A"},
		{Name: "b", Type: "openai", BaseURL: good.URL, KeyEnv: "KEY_B"},
	}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{
		{Provider: "a", Model: "m-a"},
		{Provider: "b", Model: "m-b"},
	}}}
	_, ts := testServer(t, providers, chains, map[string]string{"KEY_A": "k", "KEY_B": "k"})

	resp := doChat(t, ts, "main", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), "usage") {
		t.Errorf("expected upstream body, got %s", got)
	}
}

func TestChatAllStepsFailPropagatesLast(t *testing.T) {
	bad1 := completionUpstream(t, http.StatusTooManyRequests, `{"error":"429"}`, false)
	defer bad1.Close()
	bad2 := completionUpstream(t, http.StatusBadGateway, `{"error":"502"}`, false)
	defer bad2.Close()

	providers := []config.Provider{
		{Name: "a", Type: "openai", BaseURL: bad1.URL, KeyEnv: "KEY_A"},
		{Name: "b", Type: "openai", BaseURL: bad2.URL, KeyEnv: "KEY_B"},
	}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{
		{Provider: "a", Model: "m-a"},
		{Provider: "b", Model: "m-b"},
	}}}
	_, ts := testServer(t, providers, chains, map[string]string{"KEY_A": "k", "KEY_B": "k"})

	resp := doChat(t, ts, "main", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), "502") {
		t.Errorf("expected last upstream body, got %s", got)
	}
}

func TestChatRewritesModelToStepModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var got struct {
			Model string `json:"model"`
		}
		json.Unmarshal(raw, &got)
		if got.Model != "real-model" {
			http.Error(w, `{"error":"expected real-model, got `+got.Model+`"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{}]}`))
	}))
	defer up.Close()

	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: up.URL, KeyEnv: "KEY_A"}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{{Provider: "a", Model: "real-model"}}}}
	_, ts := testServer(t, providers, chains, map[string]string{"KEY_A": "k"})

	resp := doChat(t, ts, "main", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, got)
	}
}

func TestChatSkipsProviderWithoutKey(t *testing.T) {
	good := completionUpstream(t, http.StatusOK, `{"choices":[{}]}`, false)
	defer good.Close()

	providers := []config.Provider{
		{Name: "a", Type: "openai", BaseURL: "http://127.0.0.1:1", KeyEnv: "KEY_A"},
		{Name: "b", Type: "openai", BaseURL: good.URL, KeyEnv: "KEY_B"},
	}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{
		{Provider: "a", Model: "m-a"},
		{Provider: "b", Model: "m-b"},
	}}}
	_, ts := testServer(t, providers, chains, map[string]string{"KEY_B": "k"})

	resp := doChat(t, ts, "main", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestChatStreamingPassthrough(t *testing.T) {
	up := completionUpstream(t, http.StatusOK, "", true)
	defer up.Close()
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: up.URL, KeyEnv: "KEY_A"}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{{Provider: "a", Model: "m-a"}}}}
	_, ts := testServer(t, providers, chains, map[string]string{"KEY_A": "k"})

	resp := doChat(t, ts, "main", true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), "[DONE]") {
		t.Errorf("stream body missing [DONE]: %q", got)
	}
	if !strings.Contains(string(got), "hi") {
		t.Errorf("stream body missing delta: %q", got)
	}
}

func TestModelsListsChainNames(t *testing.T) {
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: "http://x", KeyEnv: "K"}}
	chains := []config.Chain{
		{Name: "fast", Steps: []config.Step{{Provider: "a", Model: "m-a"}}},
		{Name: "smart", Steps: []config.Step{{Provider: "a", Model: "m-b"}}},
	}
	_, ts := testServerDefault(t, providers, chains, map[string]string{"K": "k"}, "fast")

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer client-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range out.Data {
		got[d.ID] = true
	}
	if !got["fast"] || !got["smart"] || len(out.Data) != 2 {
		t.Errorf("models = %v", got)
	}
}

func TestHealthUnauthenticated(t *testing.T) {
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: "http://x", KeyEnv: "K"}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{{Provider: "a", Model: "m"}}}}
	_, ts := testServer(t, providers, chains, map[string]string{"K": "k"})

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestChatRequiresAuth(t *testing.T) {
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: "http://x", KeyEnv: "K"}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{{Provider: "a", Model: "m"}}}}
	_, ts := testServer(t, providers, chains, map[string]string{"K": "k"})

	resp := doChatNoAuth(t, ts)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func doChatNoAuth(t *testing.T, ts *httptest.Server) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": "main"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(string(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestTelemetryRecordsAttempts(t *testing.T) {
	bad := completionUpstream(t, http.StatusTooManyRequests, `{"error":"429"}`, false)
	defer bad.Close()
	good := completionUpstream(t, http.StatusOK, `{"choices":[{}],"usage":{"prompt_tokens":5,"completion_tokens":9}}`, false)
	defer good.Close()

	providers := []config.Provider{
		{Name: "a", Type: "openai", BaseURL: bad.URL, KeyEnv: "KEY_A"},
		{Name: "b", Type: "openai", BaseURL: good.URL, KeyEnv: "KEY_B"},
	}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{
		{Provider: "a", Model: "m-a"},
		{Provider: "b", Model: "m-b"},
	}}}
	s, ts := testServer(t, providers, chains, map[string]string{"KEY_A": "k", "KEY_B": "k"})

	resp := doChat(t, ts, "main", false)
	resp.Body.Close()

	if err := s.tm.Flush(); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.tm.DB().QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Errorf("telemetry rows = %d, want 2 (one per step attempt)", rows)
	}
	var tokensOut int64
	if err := s.tm.DB().QueryRow(`SELECT tokens_out FROM requests WHERE provider='b'`).Scan(&tokensOut); err != nil {
		t.Fatal(err)
	}
	if tokensOut != 9 {
		t.Errorf("tokens_out = %d, want 9", tokensOut)
	}
}

func TestChatSkipsDownStep(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("down provider must not be called")
		http.Error(w, `{"error":"should not reach"`, http.StatusInternalServerError)
	}))
	defer down.Close()
	good := completionUpstream(t, http.StatusOK, `{"choices":[{}]}`, false)
	defer good.Close()

	providers := []config.Provider{
		{Name: "a", Type: "openai", BaseURL: down.URL, KeyEnv: "KEY_A"},
		{Name: "b", Type: "openai", BaseURL: good.URL, KeyEnv: "KEY_B"},
	}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{
		{Provider: "a", Model: "m-a"},
		{Provider: "b", Model: "m-b"},
	}}}
	s, ts := testServer(t, providers, chains, map[string]string{"KEY_A": "k", "KEY_B": "k"})
	for i := 0; i < health.FailureThreshold; i++ {
		s.hlth.Mark("a", "m-a", 0, errors.New("boom"))
	}
	if !s.hlth.IsDown("a", "m-a") {
		t.Fatal("step should be down after threshold failures")
	}

	resp := doChat(t, ts, "main", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via healthy step", resp.StatusCode)
	}
}

func TestChatRetriesTransientFailure(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	}))
	defer up.Close()

	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: up.URL, KeyEnv: "KEY_A"}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{{Provider: "a", Model: "m-a"}}}}
	_, ts := testServer(t, providers, chains, map[string]string{"KEY_A": "k"})

	resp := doChat(t, ts, "main", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retries", resp.StatusCode)
	}
	if hits != 3 {
		t.Errorf("upstream hits = %d, want 3 (2 retries)", hits)
	}
}

func TestChatClientErrorDoesNotMarkDown(t *testing.T) {
	bad := completionUpstream(t, http.StatusBadRequest, `{"error":"bad model"}`, false)
	defer bad.Close()
	good := completionUpstream(t, http.StatusOK, `{"choices":[{}]}`, false)
	defer good.Close()

	providers := []config.Provider{
		{Name: "a", Type: "openai", BaseURL: bad.URL, KeyEnv: "KEY_A"},
		{Name: "b", Type: "openai", BaseURL: good.URL, KeyEnv: "KEY_B"},
	}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{
		{Provider: "a", Model: "m-a"},
		{Provider: "b", Model: "m-b"},
	}}}
	s, ts := testServer(t, providers, chains, map[string]string{"KEY_A": "k", "KEY_B": "k"})

	for i := 0; i < health.FailureThreshold; i++ {
		resp := doChat(t, ts, "main", false)
		resp.Body.Close()
	}
	if s.hlth.IsDown("a", "m-a") {
		t.Error("4xx client errors must not mark a provider down")
	}
}

func TestChatStreamingCapturesUsage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":22}}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()

	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: up.URL, KeyEnv: "KEY_A"}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{{Provider: "a", Model: "m-a"}}}}
	s, ts := testServer(t, providers, chains, map[string]string{"KEY_A": "k"})

	resp := doChat(t, ts, "main", true)
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), "[DONE]") {
		t.Fatalf("stream body missing [DONE]: %q", got)
	}

	if err := s.tm.Flush(); err != nil {
		t.Fatal(err)
	}
	var in, out int64
	if err := s.tm.DB().QueryRow(`SELECT tokens_in, tokens_out FROM requests`).Scan(&in, &out); err != nil {
		t.Fatal(err)
	}
	if in != 11 || out != 22 {
		t.Errorf("stream tokens = %d/%d, want 11/22", in, out)
	}
}

func TestAdminStatusRequiresAdminKey(t *testing.T) {
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: "http://x", KeyEnv: "K"}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{{Provider: "a", Model: "m"}}}}
	s, ts := testServer(t, providers, chains, map[string]string{"K": "k"})
	s.tm.Record(telemetry.Event{Provider: "a", Chain: "main", Model: "m", Status: 200, TokensIn: 3, TokensOut: 5})
	if err := s.tm.Flush(); err != nil {
		t.Fatal(err)
	}

	get := func(token string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/status", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	if resp := get("client-1"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("client key status = %d, want 401", resp.StatusCode)
		resp.Body.Close()
	}
	resp := get("admin-secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Chains    []string `json:"chains"`
		StepsDown int      `json:"steps_down"`
		Usage     struct {
			Requests  int   `json:"requests"`
			TokensIn  int64 `json:"tokens_in"`
			TokensOut int64 `json:"tokens_out"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Chains) != 1 || out.Chains[0] != "main" {
		t.Errorf("chains = %v", out.Chains)
	}
	if out.Usage.Requests != 1 || out.Usage.TokensIn != 3 || out.Usage.TokensOut != 5 {
		t.Errorf("usage = %+v, want 1 req 3/5 tokens", out.Usage)
	}
}

func TestAdminLogsReturnsRecentLines(t *testing.T) {
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: "http://x", KeyEnv: "K"}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{{Provider: "a", Model: "m"}}}}
	_, ts := testServer(t, providers, chains, map[string]string{"K": "k"})

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/logs?n=5", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Lines []string `json:"lines"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Lines == nil {
		t.Error("logs lines must be a list (possibly empty)")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logs status = %d, want 200", resp.StatusCode)
	}
}

func postChain(t *testing.T, ts *httptest.Server, token string, name string, steps []chainStep) *http.Response {
	t.Helper()
	body, _ := json.Marshal(chainCreateRequest{Name: name, Steps: steps})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chains", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestChainsCreateWithClientKey(t *testing.T) {
	up := completionUpstream(t, http.StatusOK, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`, false)
	defer up.Close()
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: up.URL, KeyEnv: "K"}}
	_, ts := testServer(t, providers, nil, map[string]string{"K": "k"})

	resp := postChain(t, ts, "client-1", "mychain", []chainStep{{Provider: "a", Model: "m"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	// Chat through the new chain.
	chat := doChat(t, ts, "mychain", false)
	defer chat.Body.Close()
	if chat.StatusCode != http.StatusOK {
		t.Fatalf("chat via new chain = %d, want 200", chat.StatusCode)
	}
}

func TestChainsCreateDuplicateConflict(t *testing.T) {
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: "http://x", KeyEnv: "K"}}
	_, ts := testServer(t, providers, nil, map[string]string{"K": "k"})

	if resp := postChain(t, ts, "client-1", "dup", []chainStep{{Provider: "a", Model: "m"}}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create = %d, want 201", resp.StatusCode)
	}
	resp := postChain(t, ts, "client-1", "dup", []chainStep{{Provider: "a", Model: "m"}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409", resp.StatusCode)
	}
}

func TestChainsCreateRejectsBadNameAndUnknownProvider(t *testing.T) {
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: "http://x", KeyEnv: "K"}}
	_, ts := testServer(t, providers, nil, map[string]string{"K": "k"})

	if resp := postChain(t, ts, "client-1", "Bad_Name", []chainStep{{Provider: "a", Model: "m"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad name = %d, want 400", resp.StatusCode)
	}
	if resp := postChain(t, ts, "client-1", "okname", []chainStep{{Provider: "nope", Model: "m"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown provider = %d, want 400", resp.StatusCode)
	}
}

func TestChatUnknownModelNotFound(t *testing.T) {
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: "http://x", KeyEnv: "K"}}
	_, ts := testServer(t, providers, nil, map[string]string{"K": "k"})

	resp := doChat(t, ts, "does-not-exist", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown model chat = %d, want 404", resp.StatusCode)
	}
}

func TestChainsDeletePersistsAndUnroutes(t *testing.T) {
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: "http://x", KeyEnv: "K"}}
	_, ts := testServer(t, providers, nil, map[string]string{"K": "k"})

	if resp := postChain(t, ts, "client-1", "temp", []chainStep{{Provider: "a", Model: "m"}}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/chains/temp", nil)
	req.Header.Set("Authorization", "Bearer client-1")
	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	del.Body.Close()
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", del.StatusCode)
	}
	chat := doChat(t, ts, "temp", false)
	defer chat.Body.Close()
	if chat.StatusCode != http.StatusNotFound {
		t.Fatalf("chat after delete = %d, want 404", chat.StatusCode)
	}
}

func TestChainsDeleteConfigChainRefused(t *testing.T) {
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: "http://x", KeyEnv: "K"}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{{Provider: "a", Model: "m"}}}}
	_, ts := testServer(t, providers, chains, map[string]string{"K": "k"})

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/chains/main", nil)
	req.Header.Set("Authorization", "Bearer client-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("config chain delete = %d, want 400", resp.StatusCode)
	}
}

// TestChatFailsOverAcrossKeys: a dead key (401) must rotate to the next key
// within the retry budget, and the telemetry row records the serving key.
func TestChatFailsOverAcrossKeys(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid key"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`))
	}))
	defer up.Close()

	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: up.URL, KeyEnvs: []string{"KEY_A1", "KEY_A2"}}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{{Provider: "a", Model: "m-a"}}}}
	s, ts := testServer(t, providers, chains, map[string]string{"KEY_A1": "bad", "KEY_A2": "good"})

	resp := doChat(t, ts, "main", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat = %d, want 200 via key failover", resp.StatusCode)
	}
	if hits != 2 {
		t.Errorf("upstream hits = %d, want 2 (bad key then good key)", hits)
	}

	if err := s.tm.Flush(); err != nil {
		t.Fatal(err)
	}
	var key string
	var tokensOut int64
	if err := s.tm.DB().QueryRow(`SELECT key, tokens_out FROM requests`).Scan(&key, &tokensOut); err != nil {
		t.Fatal(err)
	}
	if key != "KEY_A2" {
		t.Errorf("serving key = %q, want KEY_A2", key)
	}
	if tokensOut != 3 {
		t.Errorf("tokens_out = %d, want 3", tokensOut)
	}
	// The dead key must be quarantined with reason auth.
	if ks := s.pools["a"].Keys(); ks[0].State != "quarantined" || ks[0].Reason != "auth" {
		t.Errorf("key0 state = %q/%q, want quarantined/auth", ks[0].State, ks[0].Reason)
	}
}

func TestAdminStatusShowsKeys(t *testing.T) {
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: "http://x", KeyEnvs: []string{"KEY_A1", "KEY_A2"}}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{{Provider: "a", Model: "m"}}}}
	s, ts := testServer(t, providers, chains, map[string]string{"KEY_A1": "k", "KEY_A2": "k"})
	s.pools["a"].Mark("KEY_A1", keypool.ClassAuth)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/status", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Keys []struct {
			Provider string `json:"provider"`
			Env      string `json:"env"`
			State    string `json:"state"`
			Reason   string `json:"reason"`
			Failures int    `json:"failures"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Keys) != 2 {
		t.Fatalf("keys = %d entries, want 2", len(out.Keys))
	}
	if out.Keys[0].Provider != "a" || out.Keys[0].Env != "KEY_A1" ||
		out.Keys[0].State != "quarantined" || out.Keys[0].Reason != "auth" || out.Keys[0].Failures != 1 {
		t.Errorf("key0 = %+v, want quarantined KEY_A1 auth 1 failure", out.Keys[0])
	}
	if out.Keys[1].State != "healthy" {
		t.Errorf("key1 state = %q, want healthy", out.Keys[1].State)
	}
}

// captureUpstream serves 200 and records the raw request body it received.
func captureUpstream(t *testing.T, bodies chan []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies <- raw
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"choices":[{}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
}

func bodyField(t *testing.T, body []byte, field string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return m[field]
}

func TestStepParamsMergedFillMissing(t *testing.T) {
	bodies := make(chan []byte, 1)
	up := captureUpstream(t, bodies)
	defer up.Close()
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: up.URL, KeyEnv: "K"}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{
		{Provider: "a", Model: "m-a", Params: map[string]any{"reasoning_effort": "high", "max_tokens": 8192}},
	}}}
	_, ts := testServer(t, providers, chains, map[string]string{"K": "k"})

	// Client sends neither field — step params must fill both.
	resp := doChat(t, ts, "main", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Guvna-Step"); got != "a/m-a" {
		t.Errorf("X-Guvna-Step = %q, want a/m-a", got)
	}
	body := <-bodies
	if bodyField(t, body, "reasoning_effort") != "high" {
		t.Errorf("reasoning_effort not filled: %s", body)
	}
	if bodyField(t, body, "max_tokens") != float64(8192) {
		t.Errorf("max_tokens not filled: %s", body)
	}
}

func TestStepParamsClientWins(t *testing.T) {
	bodies := make(chan []byte, 1)
	up := captureUpstream(t, bodies)
	defer up.Close()
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: up.URL, KeyEnv: "K"}}
	chains := []config.Chain{{Name: "main", Steps: []config.Step{
		{Provider: "a", Model: "m-a", Params: map[string]any{"max_tokens": 8192}},
	}}}
	_, ts := testServer(t, providers, chains, map[string]string{"K": "k"})

	body, _ := json.Marshal(map[string]any{"model": "main", "stream": false, "max_tokens": 2048})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer client-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := <-bodies
	if bodyField(t, got, "max_tokens") != float64(2048) {
		t.Errorf("client max_tokens overridden by step param: %s", got)
	}
}

func TestStepParamsStreamAndModelNeverMerged(t *testing.T) {
	up := completionUpstream(t, http.StatusOK, `{"choices":[{}]}`, false)
	defer up.Close()
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: up.URL, KeyEnv: "K"}}
	// Reserved keys are rejected at chain creation, so a config chain with
	// them must fail config validation — the gateway never relays them.
	chains := []config.Chain{{Name: "main", Steps: []config.Step{
		{Provider: "a", Model: "m-a", Params: map[string]any{"stream": true}},
	}}}
	cfg := &config.Config{Port: 20128, Providers: providers, Chains: chains, DefaultChain: "main"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("config with reserved param key accepted")
	}
}

func TestChainsCreateParamsRejectedReserved(t *testing.T) {
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: "http://x", KeyEnv: "K"}}
	_, ts := testServer(t, providers, nil, map[string]string{"K": "k"})

	resp := postChain(t, ts, "client-1", "bad", []chainStep{{Provider: "a", Model: "m", Params: map[string]any{"messages": []any{}}}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create with reserved param = %d, want 400", resp.StatusCode)
	}
}

func TestChainsCreateEchoesParams(t *testing.T) {
	providers := []config.Provider{{Name: "a", Type: "openai", BaseURL: "http://x", KeyEnv: "K"}}
	_, ts := testServer(t, providers, nil, map[string]string{"K": "k"})

	params := map[string]any{"reasoning_effort": "high", "max_tokens": 8192}
	resp := postChain(t, ts, "client-1", "tuned", []chainStep{{Provider: "a", Model: "m", Params: params}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/chains", nil)
	req.Header.Set("Authorization", "Bearer client-1")
	lresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer lresp.Body.Close()
	var out struct {
		Chains []struct {
			Name  string `json:"name"`
			Steps []struct {
				Provider string         `json:"provider"`
				Model    string         `json:"model"`
				Params   map[string]any `json:"params"`
			} `json:"steps"`
		} `json:"chains"`
	}
	if err := json.NewDecoder(lresp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range out.Chains {
		if c.Name != "tuned" {
			continue
		}
		found = true
		if len(c.Steps) != 1 || c.Steps[0].Params["reasoning_effort"] != "high" {
			t.Fatalf("params not echoed: %+v", c.Steps)
		}
	}
	if !found {
		t.Fatal("chain tuned not listed")
	}
}
