# Architecture

## High-level

```
any OpenAI-compatible client
        │  /v1/*  (port 20128, auth: Bearer admin/API key)
        ▼
┌──────────────────────────────────────────┐
│  Guvna daemon (single static binary) │
│  ┌────────────┐ ┌───────────────┐        │
│  │  Router    │ │  Compression  │        │
│  │  adaptors  │ │  pipeline*    │        │
│  └────────────┘ └───────────────┘        │
│  ┌────────────┐ ┌───────────────┐        │
│  │  Chains    │ │  Telemetry    │        │
│  │  (runtime) │ │  (SQLite)     │        │
│  └────────────┘ └───────────────┘        │
└───────────────┬──────────────────────────┘
                │ outbound HTTPS
        ┌───────┴────────┐
        │ 50+ providers  │
        └────────────────┘

Separate, optional:  read-only web UI binary* (never in core)
CLI:                guvna-cli status / logs / chains (local + remote over gateway)

* planned — see ROADMAP phases 6–7; everything unmarked is shipped.
```

## Components

### 1. Router (built)
- `/v1/chat/completions` (+ `/v1/models`) OpenAI-compatible, streaming SSE passthrough, 15s keep-alives
- **Chain routing is the core primitive**: chains = ordered (provider, model) steps. First 2xx step serves; non-2xx walks to the next step; all steps failed → last upstream response propagated as-is. No silent mid-stream restarts. Chains are retried (2 retries, 250ms→1s backoff) and failure-marked (3 consecutive failures → cool-off 60s doubling to 10min, auto-recovery).
- **No default chains**: every chain is created per use case by the client via `POST /v1/chains` (any valid API key — apps self-provision; persisted to `<data-dir>/chains.yaml`, atomic rewrite). `DELETE /v1/chains/{name}` removes runtime chains. `/v1/models` lists chains (empty until the client creates them). Unknown model → 404 with a pointer to the creation API — no silent fallback.
- **Provider-prefixed passthrough**: any model named `<prefix>/<model>` routes directly to that provider, bypassing chains. Prefixes are per-provider in config (`prefixes:` list, defaults to the provider name). If the suffix is in the provider's known models, only the suffix is sent upstream (e.g. `groq/llama-3.3-70b-versatile` → groq, model `llama-3.3-70b-versatile`); otherwise the full name is sent (namespaced catalogs, e.g. `openai/gpt-5.6-luna` → orcarouter, model `openai/gpt-5.6-luna`).
- **Loose validation, no catalogs**: chain creation validates provider exists and model is non-empty — NOT model existence. The upstream provider IS the catalog; a stale local catalog would reject valid new models (upstream 404s at request time and the chain falls over instead).
- **Key pools + rotation**: providers take `key_envs: [K1, K2, ...]` (a single `key_env` still works = 1-key pool). Per-provider rotation strategy (`rotation:`): `round_robin` (default) / `least_used` / `sequential`, picking only healthy keys. Class-based quarantine (`quarantine:` per-provider overrides; defaults 24h auth / 60s rate_limit / 60s transient): 401/403 → `auth`, 429 → `rate_limit`, 5xx/network → `transient`; durations double per consecutive failure (capped). Success resets a key. Failover: on a quarantinable response the same request is retried on the next healthy key within the step retry budget (401/403 consume retries too — dead keys burn budget fast). All keys down → step treated as down → chain walks on. Streaming rotation is per-request; a mid-stream key death propagates an error chunk (no mid-stream swap). Key state (state/reason/failures/cool-off remaining/requests/tokens per key) is exposed in `/admin/status` `keys`; telemetry rows carry the serving `key`. Quarantine is in-memory (resets on restart, like step health).
- **Model rewrite**: relay rewrites the request's `model` field per step (clients send logical names; upstream needs the real model). Everything else passes through untouched.
- Provider adaptors: small per-provider interface (`Chat(ctx, body) (*http.Response, error)`), pattern from new-api's `relay_adaptor.go`. Two types built: `openai` (base + `/v1/chat/completions`) and `gemini` (base + `/v1beta/openai` + chat path — Gemini's official OpenAI-compat endpoint, passthrough no translation). Outbound requests carry `User-Agent: guvna/0.1` (bazaarlink throttles UA-less requests).
- Auth: Bearer admin key (`ADMIN_KEY`) or client key (`API_KEYS`), always on; `/healthz` unauthenticated
- Telemetry: in-memory event buffer, async batch flush to SQLite every 30s (pure-Go driver, no CGO); hot path never touches disk; crash loses <30s
- Single process, one port. No dashboard in the process.

### 2. Compression pipeline (planned — ROADMAP Phase 6, not built)
Decision precedence when built (adapted from OmniRoute): per-request header (`x-guvna-compression`) → named profile → adaptive → config default → off. Cache-aware compression is always on (never touch already-cached prefixes).

v1 engines:
1. **Caveman-style** — JSON rule packs + language packs + injected system prompt. Rule pack format and packs are MIT-licensed from JuliusBrussee/caveman and OmniRoute's `open-sse` compression config.
2. **RTK-style tool-output filters** — reimplement filters for ~10 dominant commands (git, grep, ls, build logs) in Go. Reference: rtk-ai/rtk (Rust CLI, no library API — reimplement, don't bind).
3. **Session dedup** — content-addressed pruning of repeated/redundant conversation turns.

v2 (not in v1): LLMLingua-2 (ONNX MobileBERT, heavy — separate process or sidecar), headroom-style live-zone (only compress the uncached tail).

### 3. Catalog sync (cancelled)

~~Subscribe-sync script pulling OmniRoute's MIT catalog data.~~ **Cancelled 2026-08-16** — catalogs go stale and a stale catalog rejects valid new models. The upstream provider IS the catalog: chain creation validates loosely (provider exists, model non-empty) and unknown models fail at request time, where the chain falls over. Full rationale in DECISIONS.md.

### 4. Config & data
- **Config-as-code**: YAML in git (`config.yaml`): providers, keys via env, profiles, compression settings, ports, routing weights
- **SQLite**: usage/telemetry only (request counts, tokens, errors). Never for config.
- Data dir: `~/.guvna/` (config override, db, logs)

### 5. Security
- Auth always on: one admin key (env `ADMIN_KEY`), client keys (env `API_KEYS`); the gateway refuses to start without them
- Remote CLI authenticates with the admin key over the gateway's HTTPS endpoint — no new port, no separate token scheme (revocable scoped tokens: future work, ROADMAP Phase 4)
- No multi-user, no registration, no open dashboard

### 6. Web UI (planned — ROADMAP Phase 7, not built)
- Read-only status: provider health, usage, token savings
- Separate binary; can be omitted from deployment entirely

## Sizing

- Core gateway: ~3-6k lines of Go (scoped reimplementation of OmniRoute's routing subset)
- RSS: ~15-40MB idle
- Distroless image ~10-20MB
- Rationale in RESEARCH.md (why scratch, not port or fork)

## Non-negotiable defaults

- `REQUIRE_API_KEY` equivalent always on when exposed
- Config in git, not DB
- UI never in core process
- Port 20128
