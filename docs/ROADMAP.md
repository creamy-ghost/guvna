# Roadmap

Build order with definition of done per phase. No time estimates — this is a personal project, build at your own pace. Each phase ends with something working and verifiable.

**Use-ready cut (2026-08-16):** Guvna is live and usable daily. Gateway runs on the VPS behind caddy (`https://gateway.example.com:9443`), chains self-heal (failure marking + cool-off), and usage/health are visible from the laptop via the remote CLI. Everything below the cut is daily-use; everything after is growth.

---

## Phase 1 — Gateway core (DONE 2026-08-16)

- Go module, single static binary, HTTP server on port 20128
- `GET /v1/models` (chain names) + `POST /v1/chat/completions`, non-streaming + SSE streaming passthrough (keep-alives, error propagation mid-stream)
- Chain-based routing core: named chains = (provider, model) steps, model rewrite per step, fallback across steps, `default_chain` catch-all — **superseded (see Phase 4b): no default chains, clients create chains**
- Two adaptor types: `openai` (bazaarlink, groq, mistral) and `gemini` (via OpenAI-compat endpoint)
- Config-as-code YAML (providers, chains, keys from env), `~/.guvna/config.yaml` override
- Auth: admin key (`ADMIN_KEY`) + client keys (`API_KEYS`), always on; `/healthz` unauthenticated
- Async batched SQLite telemetry (~/.guvna/guvna.db, 30s flush)
- **Done:** curl a chat completion (streaming + non-streaming) through chains against real providers — verified live: bazaarlink (free), groq (fast), gemini (smart)

## Phase 2 — Failure marking (DONE 2026-08-16)

- Per-(provider, model) failure tracker: 3 consecutive failures → 60s cool-off, doubling per failure beyond threshold, capped at 10 min; auto-recovers when cool-off expires
- Steps in cool-off are skipped in chains (log + telemetry, no admin action)
- Retries with backoff within a step (2 retries: 250ms, 1s) for network errors, 429 and 5xx; 4xx never retried or marked
- Streaming usage capture from the final SSE chunk (non-streaming already counted)
- **Done:** provider fails mid-session → chain skips it until cool-off expires; stream rows carry real token counts — verified live (groq 37/10)

## Phase 3 — VPS deploy (DONE 2026-08-16)

- Distroless image: multi-stage, CGO_ENABLED=0, `gcr.io/distroless/static-debian12:nonroot`, ~13MB, no shell
- `-healthcheck` flag (binary probes own /healthz — no shell in image), `-data` flag for the volume
- Docker compose: `network_mode: host` (the VPS can't create bridge networks), `mem_limit: 400m`, `GOMEMLIMIT=256MiB`, named volume `/data`, restart unless-stopped
- Caddy vhost `gateway.example.com:9443` → 127.0.0.1:20128 (port 443 was already occupied by another TLS service)
- Keys via `/home/alex/guvna/.env` (chmod 600), never in git
- **Done:** streaming chat + admin status through the caddy subdomain from the laptop, container health checks green

## Phase 4 — Remote CLI (DONE 2026-08-16, partial)

- Admin surface in core: `GET /admin/status` (uptime, chains, step health, usage aggregates) + `GET /admin/logs` (ring buffer tail), admin-key-only
- `guvna-cli` binary: `status` (table or `--json`) + `logs [-n]`, local (`http://127.0.0.1:20128`) or remote (`--url https://gateway...:9443`, `--token`/`GUVNA_ADMIN_KEY`)
- Scoped tokens: the admin key over the gateway's HTTPS endpoint — no new port exposed
- **Not yet:** `connect` subcommand with revocable scoped tokens, `config`, `providers`
- **Done when:** `guvna-cli status` from laptop shows real usage/health of the VPS instance — verified live

## Phase 4b — Custom chains API (DONE 2026-08-16)

- **No default chains** — every chain is created per use case by the client; `/v1/models` starts empty; unknown model → 404 with a pointer to the creation API (no silent fallback)
- `POST /v1/chains` (any valid API key — apps self-provision), `DELETE /v1/chains/{name}`, `GET /v1/chains`; persisted to `<data-dir>/chains.yaml` (atomic rewrite), merged over config at startup
- Provider-prefixed passthrough: `<prefix>/<model>` routes directly to a provider (strip suffix when known, else full name; per-provider `prefixes:` in config)
- Loose validation: provider must exist, model non-empty — no model-existence checks (upstream is the catalog; stale local catalogs would reject valid new models)
- `guvna-cli chains list|add|rm` (local + remote)
- **Done:** create a chain via API from the laptop, chat through it, survive a container restart — verified live on the VPS (chain `myfree` persisted across `docker restart`)

## Phase 4c — Key pools & rotation (DONE 2026-08-17)

- Per-provider `key_envs:` lists (single `key_env` still accepted = 1-key pool); rotation strategies `round_robin` (default) / `least_used` / `sequential`
- Class-based quarantine: 401/403 → auth (24h), 429 → rate_limit (60s), 5xx/network → transient (60s) — per-provider overridable, doubles per consecutive failure, capped; success resets; in-memory only
- Key failover: dead key costs one attempt, retried on the next healthy key within the step retry budget; 401/403 consume retries; all keys down → step down → chain falls over; streaming rotation is per-request only
- Observability: per-key state in `/admin/status` (state, reason, failures, cool-off, requests/tokens), `key` column in telemetry rows (DB migration on existing volumes)
- **Done:** deliberately-broken key in a live pool → requests succeed via failover, bad key quarantined with reason, telemetry rows carry the serving key — verified live on the VPS (bazaarlink pool) and locally (groq pool all-dead → last 403 propagated)

---

## Phase 5 — Catalog sync (parked — superseded by loose validation)

- ~~Subscribe-sync script: pull OmniRoute MIT catalog files, normalize, diff + commit to git~~ — **cancelled 2026-08-16**: catalogs go stale (orcarouter ships new free models weekly); a stale catalog rejects valid new models — worse than a typo failing at request time. The upstream provider IS the catalog for a single-user gateway. Revisit only if chain creation needs model discovery/completion.
- `guvna providers list` / `providers test` CLI
- Free-tier health flags (per-provider hasFree + freeNote from catalog)
- **Done when:** a fresh sync produces a clean git commit and provider list is queryable

## Phase 6 — Compression engines

1. Caveman-style: rule pack loading + injected system prompt + output normalization
2. RTK-style tool-output filters for ~10 dominant commands (git/grep/ls/build logs)
3. Session dedup / repetition pruning (content-addressed)
- Compression decision precedence: header → named profile → adaptive → config default → off
- Cache-aware: never touch cached prefixes
- **Done when:** token savings measurable per engine (before/after counts in telemetry)

## Phase 7 — Web UI (separate binary)

- Read-only: provider health, usage, token savings
- Separate binary, never embedded in core
- **Done when:** runs standalone against a live gateway, read-only enforced

## Phase 8 — Arch local install

- systemd user unit, data dir `~/.guvna/`, env config
- app rewired: config.yaml points at the live gateway (drop-in port 20128)
- **Done when:** app routes through Guvna on Arch and via the VPS

## Phase 9 — Hardening + release

- Bench harness vs litellm / OmniRoute / new-api (req/s, p99, RSS) → BENCHMARKS.md
- Tests: routing, streaming, compression, auth
- Docs: usage, config reference, deploy guide
- Release: versioned builds, checksums, container image
- **Done when:** clean test run + a documented release cut + benchmark numbers published

## Phase 10 — v2 backlog

- LLMLingua-2 (ONNX) + headroom-style cache-aware live-zone engines
- Per-user keys (auth bolt-on, router untouched)
- Combos / context-relay routing