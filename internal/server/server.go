// Package server wires the HTTP surface: /healthz, /v1/models, and the
// /v1/chat/completions relay with chain-based fallback and SSE passthrough.
package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/creamy-ghost/guvna/internal/adaptors"
	"github.com/creamy-ghost/guvna/internal/auth"
	"github.com/creamy-ghost/guvna/internal/chains"
	"github.com/creamy-ghost/guvna/internal/config"
	"github.com/creamy-ghost/guvna/internal/health"
	"github.com/creamy-ghost/guvna/internal/keypool"
	"github.com/creamy-ghost/guvna/internal/logring"
	"github.com/creamy-ghost/guvna/internal/router"
	"github.com/creamy-ghost/guvna/internal/telemetry"
)

// KeepAliveInterval is how long the relay waits without upstream data before
// writing an SSE comment line to hold the connection open.
const KeepAliveInterval = 15 * time.Second

// retries is how many times a step is retried (beyond the first attempt) on
// transient failures (429/5xx/network) before the chain moves to the next step.
const retries = 2

type Server struct {
	cfg       *config.Config
	rtr       *router.Router
	auth      *auth.Authenticator
	tm        *telemetry.Telemetry
	hlth      *health.Tracker
	logs      *logring.Ring
	store     *chains.Store
	pools     map[string]*keypool.Pool
	started   time.Time
	env       func(string) string
	retryWait func(attempt int) time.Duration
}

func New(cfg *config.Config, rtr *router.Router, a *auth.Authenticator, tm *telemetry.Telemetry, hlth *health.Tracker, logs *logring.Ring, store *chains.Store) *Server {
	pools := make(map[string]*keypool.Pool, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if pool, err := keypool.New(p.Name, p.AllKeyEnvs(), p.Rotation, p.Quarantine); err == nil {
			pools[p.Name] = pool
		}
	}
	return &Server{cfg: cfg, rtr: rtr, auth: a, tm: tm, hlth: hlth, logs: logs, store: store, pools: pools, started: time.Now(), env: os.Getenv, retryWait: defaultRetryWait}
}

// defaultRetryWait backs off 250ms then 1s for the two retry attempts.
func defaultRetryWait(attempt int) time.Duration {
	if attempt <= 1 {
		return 250 * time.Millisecond
	}
	return time.Second
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("GET /v1/models", s.auth.Middleware(http.HandlerFunc(s.handleModels)))
	mux.Handle("POST /v1/chat/completions", s.auth.Middleware(http.HandlerFunc(s.handleChat)))
	mux.Handle("GET /v1/chains", s.auth.Middleware(http.HandlerFunc(s.handleChainList)))
	mux.Handle("POST /v1/chains", s.auth.Middleware(http.HandlerFunc(s.handleChainCreate)))
	mux.Handle("DELETE /v1/chains/{name}", s.auth.Middleware(http.HandlerFunc(s.handleChainDelete)))
	mux.Handle("GET /admin/status", s.auth.AdminMiddleware(http.HandlerFunc(s.handleAdminStatus)))
	mux.Handle("GET /admin/logs", s.auth.AdminMiddleware(http.HandlerFunc(s.handleAdminLogs)))
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

// handleAdminStatus reports chain health and usage aggregates to the admin
// (or a remote CLI with the admin key).
func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	stats, err := s.tm.Stats()
	if err != nil {
		http.Error(w, `{"error":{"message":"stats unavailable"}}`, http.StatusInternalServerError)
		return
	}
	steps := s.hlth.Snapshot()
	stepsDown := 0
	for _, st := range steps {
		if st.Down {
			stepsDown++
		}
	}
	keys := make([]keypool.KeyStatus, 0, len(s.pools))
	for _, name := range s.poolNames() {
		keys = append(keys, s.pools[name].Keys()...)
	}
	resp := struct {
		Uptime    string              `json:"uptime"`
		Chains    []string            `json:"chains"`
		Steps     []health.StepStatus `json:"steps"`
		StepsDown int                 `json:"steps_down"`
		Keys      []keypool.KeyStatus `json:"keys"`
		Usage     telemetry.Stats     `json:"usage"`
	}{Uptime: time.Since(s.started).Round(time.Second).String()}
	resp.Chains = s.rtr.ChainNames()
	resp.Steps = steps
	resp.StepsDown = stepsDown
	resp.Keys = keys
	resp.Usage = stats
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	n := 100
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 512 {
			n = parsed
		}
	}
	resp := struct {
		Lines []string `json:"lines"`
	}{Lines: s.logs.Tail(n)}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	names := s.rtr.ChainNames()
	resp := struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}{Object: "list"}
	for _, n := range names {
		resp.Data = append(resp.Data, struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		}{ID: n, Object: "model", Created: time.Now().Unix(), OwnedBy: "guvna"})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleChainList returns the current chains (config + runtime) with sources.
func (s *Server) handleChainList(w http.ResponseWriter, r *http.Request) {
	resp := struct {
		Chains []chainInfo `json:"chains"`
	}{}
	for _, c := range s.rtr.ListChains() {
		steps := make([]chainStep, 0, len(c.Steps))
		for _, st := range c.Steps {
			steps = append(steps, chainStep{Provider: st.Provider, Model: st.Model, Params: st.Params})
		}
		resp.Chains = append(resp.Chains, chainInfo{Name: c.Name, Source: c.Source, Steps: steps})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

type chainStep struct {
	Provider string         `json:"provider"`
	Model    string         `json:"model"`
	Params   map[string]any `json:"params,omitempty"`
}

type chainInfo struct {
	Name   string      `json:"name"`
	Source string      `json:"source"`
	Steps  []chainStep `json:"steps"`
}

type chainCreateRequest struct {
	Name  string      `json:"name"`
	Steps []chainStep `json:"steps"`
}

// handleChainCreate adds a runtime chain and persists it. Any valid key
// (admin or client) may create chains — apps self-provision their routes.
func (s *Server) handleChainCreate(w http.ResponseWriter, r *http.Request) {
	var req chainCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":{"message":"invalid json"}}`, http.StatusBadRequest)
		return
	}
	steps := make([]router.Step, 0, len(req.Steps))
	for _, st := range req.Steps {
		steps = append(steps, router.Step{Provider: st.Provider, Model: st.Model, Params: st.Params})
	}
	if err := s.rtr.AddChain(req.Name, steps); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "already exists") {
			status = http.StatusConflict
		}
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), status)
		return
	}
	if s.store != nil {
		if err := s.store.Save(s.rtr); err != nil {
			log.Printf("chains: persist failed: %v", err)
			http.Error(w, `{"error":{"message":"chain added but not persisted"}}`, http.StatusInternalServerError)
			return
		}
	}
	log.Printf("chains: created %q (%d steps)", req.Name, len(steps))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(chainInfo{Name: req.Name, Source: "runtime", Steps: req.Steps})
}

// handleChainDelete removes a runtime chain. Config-defined chains are
// refused — they live in config.yaml.
func (s *Server) handleChainDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.rtr.RemoveChain(name); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		http.Error(w, fmt.Sprintf(`{"error":{"message":%q}}`, err.Error()), status)
		return
	}
	if s.store != nil {
		if err := s.store.Save(s.rtr); err != nil {
			log.Printf("chains: persist failed: %v", err)
			http.Error(w, `{"error":{"message":"chain removed but not persisted"}}`, http.StatusInternalServerError)
			return
		}
	}
	log.Printf("chains: deleted %q", name)
	w.WriteHeader(http.StatusNoContent)
}

type chatRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":{"message":"read body"}}`, http.StatusBadRequest)
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `{"error":{"message":"invalid json"}}`, http.StatusBadRequest)
		return
	}
	chain, steps, err := s.rtr.Resolve(req.Model)
	if err != nil {
		if errors.Is(err, router.ErrNotFound) {
			http.Error(w, fmt.Sprintf(`{"error":{"message":"unknown chain or model %q — create a chain via POST /v1/chains or use a provider-prefixed model"}}`, req.Model), http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":{"message":"resolve failed"}}`, http.StatusInternalServerError)
		return
	}
	log.Printf("chat: chain=%s model=%q steps=%d stream=%v", chain, req.Model, len(steps), req.Stream)

	var lastErr error
	var lastStatus int
	var lastBody []byte
	var lastCT string
	for _, step := range steps {
		if s.hlth.IsDown(step.Provider, step.Model) {
			log.Printf("chat: skipping %s/%s (cool-off)", step.Provider, step.Model)
			continue
		}
		resp, keyEnv, err := s.tryStep(r, step, body)
		if err != nil {
			log.Printf("chat: step %s/%s failed: %v", step.Provider, step.Model, err)
			lastErr = err
			s.hlth.Mark(step.Provider, step.Model, 0, err)
			s.record(step, chain, req, 0, err, keyEnv)
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			s.hlth.MarkSuccess(step.Provider, step.Model)
			if p := s.pools[step.Provider]; p != nil {
				p.MarkSuccess(keyEnv)
			}
			s.relaySuccess(w, r, step, chain, req, resp, keyEnv)
			return
		}
		lastStatus = resp.StatusCode
		lastBody, _ = io.ReadAll(resp.Body)
		lastCT = resp.Header.Get("Content-Type")
		resp.Body.Close()
		if retryable(resp.StatusCode, nil) {
			s.hlth.Mark(step.Provider, step.Model, resp.StatusCode, nil)
		}
		log.Printf("chat: step %s/%s failed: status %d", step.Provider, step.Model, resp.StatusCode)
		s.record(step, chain, req, resp.StatusCode, errors.New(http.StatusText(resp.StatusCode)), keyEnv)
	}
	// All steps failed: propagate the last upstream response as-is.
	log.Printf("chat: all steps failed for chain %q: %v", chain, lastErr)
	if lastStatus == 0 {
		http.Error(w, `{"error":{"message":"no providers available"}}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", lastCT)
	w.WriteHeader(lastStatus)
	w.Write(lastBody)
}

// tryStep attempts one chain step, retrying transient failures (429, 5xx,
// network errors) up to retries times with backoff. Each attempt picks a key
// from the provider's pool; quarantinable failures rotate to the next key.
// Streaming is safe: a retry only happens when no 2xx response has been
// received yet — once the upstream starts streaming we are committed to that
// stream.
//
// Returns (resp, keyEnv, nil) when an upstream response was obtained (2xx or
// not — body left open for the caller to relay), or (nil, "", err) when every
// attempt failed at the network level.
func (s *Server) tryStep(r *http.Request, step router.Step, body []byte) (*http.Response, string, error) {
	p, ok := s.provider(step.Provider)
	if !ok {
		return nil, "", fmt.Errorf("provider %q not in config", step.Provider)
	}
	pool := s.pools[step.Provider]
	if pool == nil {
		pool, _ = keypool.New(p.Name, p.AllKeyEnvs(), p.Rotation, p.Quarantine)
		s.pools[step.Provider] = pool
	}
	payload := mergeParams(withModel(body, step.Model), step.Params)
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 && s.retryWait != nil {
			time.Sleep(s.retryWait(attempt))
		}
		keyEnv := pool.Pick()
		key := s.env(keyEnv)
		if key == "" {
			err := fmt.Errorf("provider %q: key env %q not set", p.Name, keyEnv)
			log.Printf("chat: %s/%s attempt %d: %v", step.Provider, step.Model, attempt+1, err)
			lastErr = err
			continue
		}
		a, err := adaptors.New(p, key)
		if err != nil {
			log.Printf("chat: %s/%s attempt %d: %v", step.Provider, step.Model, attempt+1, err)
			lastErr = err
			continue
		}
		resp, err := a.Chat(r.Context(), payload)
		if err != nil {
			log.Printf("chat: %s/%s attempt %d: %v", step.Provider, step.Model, attempt+1, err)
			pool.Mark(keyEnv, keypool.ClassFor(0, err))
			lastErr = err
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, keyEnv, nil
		}
		log.Printf("chat: %s/%s attempt %d: status %d", step.Provider, step.Model, attempt+1, resp.StatusCode)
		if !retryable(resp.StatusCode, nil) && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
			return resp, keyEnv, nil // other 4xx: config/request error, never retried
		}
		pool.Mark(keyEnv, keypool.ClassFor(resp.StatusCode, nil))
		if attempt == retries {
			return resp, keyEnv, nil // transient failures exhausted: last response
		}
		resp.Body.Close()
	}
	return nil, "", lastErr
}

// retryable reports whether a failure is transient: network errors, rate
// limits, and 5xx. Client errors (4xx) are config or request problems and are
// never retried or counted against provider health.
func retryable(status int, err error) bool {
	if err != nil {
		return true
	}
	return status == http.StatusTooManyRequests || status >= 500
}

// withModel rewrites the "model" field of a chat completion body to the given
// model name, passing everything else through untouched.
func withModel(body []byte, model string) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if m["model"] != nil {
		enc, err := json.Marshal(model)
		if err == nil {
			m["model"] = enc
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// mergeParams applies a step's params as fill-missing defaults: a param is set
// on the body only when the client didn't supply that field. Explicit client
// values always win. The body is returned unchanged on any parse failure.
func mergeParams(body []byte, params map[string]any) []byte {
	if len(params) == 0 {
		return body
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	for k, v := range params {
		if _, present := m[k]; present {
			continue
		}
		enc, err := json.Marshal(v)
		if err != nil {
			continue
		}
		m[k] = enc
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func (s *Server) provider(name string) (config.Provider, bool) {
	for _, p := range s.cfg.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return config.Provider{}, false
}

// poolNames returns provider names that have a key pool, sorted.
func (s *Server) poolNames() []string {
	names := make([]string, 0, len(s.pools))
	for n := range s.pools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (s *Server) relaySuccess(w http.ResponseWriter, r *http.Request, step router.Step, chain string, req chatRequest, resp *http.Response, keyEnv string) {
	defer resp.Body.Close()
	if !req.Stream {
		// Non-streaming: relay body, best-effort token count.
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			if r.Context().Err() != nil {
				// Client disconnected — not an upstream failure.
				return
			}
			s.poolRecord(keyEnv, step, chain, req, 502, 0, 0, keypool.ClassTransient)
			s.record(step, chain, req, 502, err, keyEnv)
			http.Error(w, `{"error":{"message":"upstream read failed"}}`, http.StatusBadGateway)
			return
		}
		in, out := telemetry.ParseUsage(body)
		s.poolRecord(keyEnv, step, chain, req, resp.StatusCode, in, out, "")
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.Header().Set("X-Guvna-Step", step.Provider+"/"+step.Model)
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return
	}
	s.relayStream(w, r, step, chain, req, resp, keyEnv)
}

func (s *Server) relayStream(w http.ResponseWriter, r *http.Request, step router.Step, chain string, req chatRequest, resp *http.Response, keyEnv string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":{"message":"streaming unsupported"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Guvna-Step", step.Provider+"/"+step.Model)
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	br := bufio.NewReader(resp.Body)
	lastWrite := time.Now()
	var in, out int64
	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		if time.Since(lastWrite) > KeepAliveInterval {
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
			lastWrite = time.Now()
			continue
		}
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return
			}
			flusher.Flush()
			lastWrite = time.Now()
			// Streaming usage arrives in the final data: chunk; capture it
			// best-effort for telemetry.
			if payload := dataPayload(line); payload != nil {
				if i, o := telemetry.ParseUsage(payload); i != 0 || o != 0 {
					in, out = i, o
				}
			}
		}
		if err != nil {
			if r.Context().Err() != nil {
				return // client disconnected mid-stream
			}
			if errors.Is(err, io.EOF) {
				s.hlth.MarkSuccess(step.Provider, step.Model)
				if p := s.pools[step.Provider]; p != nil {
					p.MarkSuccess(keyEnv)
				}
				s.poolRecord(keyEnv, step, chain, req, resp.StatusCode, in, out, "")
				return
			}
			// Mid-stream failure: propagate as an error chunk, then close.
			// Mark the provider so later requests skip it during cool-off.
			log.Printf("chat: stream error from %q: %v", step.Provider, err)
			io.WriteString(w, `data: {"error":{"message":"upstream stream error"}}`+"\n\n")
			flusher.Flush()
			s.hlth.Mark(step.Provider, step.Model, 0, err)
			s.poolRecord(keyEnv, step, chain, req, 502, in, out, keypool.ClassTransient)
			return
		}
	}
}

// poolRecord records usage against the serving key and marks its pool state:
// failures for class != "", success otherwise.
func (s *Server) poolRecord(keyEnv string, step router.Step, chain string, req chatRequest, status int, in, out int64, class keypool.Class) {
	p := s.pools[step.Provider]
	if p == nil || keyEnv == "" {
		return
	}
	if class != "" {
		p.Mark(keyEnv, class)
	} else {
		p.MarkSuccess(keyEnv)
	}
	p.Record(keyEnv, in, out)
	s.recordTokens(step, chain, req, status, in, out, keyEnv)
}

// dataPayload returns the JSON payload of an SSE data: line, or nil.
func dataPayload(line []byte) []byte {
	s := strings.TrimSpace(string(line))
	if !strings.HasPrefix(s, "data:") {
		return nil
	}
	return []byte(strings.TrimSpace(s[len("data:"):]))
}

func (s *Server) record(step router.Step, chain string, req chatRequest, status int, err error, key string) {
	e := telemetry.Event{
		Provider: step.Provider,
		Chain:    chain,
		Model:    step.Model,
		Stream:   req.Stream,
		Status:   status,
		Key:      key,
		Ts:       time.Now(),
	}
	if err != nil {
		e.Err = err.Error()
	}
	s.tm.Record(e)
}

func (s *Server) recordTokens(step router.Step, chain string, req chatRequest, status int, in, out int64, key string) {
	e := telemetry.Event{
		Provider:  step.Provider,
		Chain:     chain,
		Model:     step.Model,
		Stream:    req.Stream,
		Status:    status,
		TokensIn:  in,
		TokensOut: out,
		Key:       key,
		Ts:        time.Now(),
	}
	s.tm.Record(e)
}
