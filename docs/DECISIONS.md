# Decision Log

Rules: one entry per decision, date, context, rationale. New sessions should read this first — these were hard-won, don't relitigate without new evidence.

## 2026-08-14 — Project: Guvna

**Decision:** Build a low-resource, single-user AI gateway from scratch in Go. Name: Guvna.

## 2026-08-14 — Single-user scope

**Decision:** v1 is single-user: one admin key, API keys for clients, global usage stats, minimal read-only web UI.

**Context:** User asked "is omnirouter single user?" — verified: OmniRoute is single-admin (INITIAL_PASSWORD + JWT_SECRET + API keys + scoped CLI tokens), NOT multi-tenant (0 "multi-user" results in docs).

**Rationale:** Free quotas (Kiro ~50 credits/mo, Cerebras 1M/day) are per-upstream-account — multiple users sharing one account just drain one quota. The only honest multi-user free model is bring-your-own-keys, which is new-api's model. Multi-user ~2-3x scope. Auth can be bolted on later without touching router core.

## 2026-08-14 — Language: Go

**Decision:** Go over Rust.

**Rationale:** Rust ~10-30MB RSS but ~50% slower development. Go ~15-40MB RSS, still far below the low-resource bar (the prior Node-based gateway ran 96-243MB). Gateway is network-bound; 1 core is fine.

## 2026-08-14 — From scratch, not fork

**Decision:** Scratch implementation, not a fork of new-api or port of OmniRoute/litellm.

**Rationale:**
- Full OmniRoute port impossible: 8,512 TS files, 320 contributors
- new-api fork: inherits ~80-150MB floor (React dashboard), permanent merge tax, billing-first data model that fights free-catalog + compression-first vision
- litellm: no free catalog, no built-in compression, Rust migration makes it a moving architecture target, and it's closing the low-resource wedge itself
- Scratch: ~3-6k lines, matches the vision, MIT references (new-api adaptors, OmniRoute catalog + compression design, rtk, caveman) are legally usable

## 2026-08-14 — Catalog: subscribe-sync, not own audit

**Decision:** Subscribe-sync script pulls OmniRoute's MIT catalog data (freeModelCatalog.ts, providers.ts) on schedule, diff + commit to git.

**Rationale:** Don't run the weekly free-tier re-audit treadmill. Own the sync layer; data persists in git history even if OmniRoute dies.

## 2026-08-14 — Compression framing: quota stretching

**Decision:** Position compression as quota stretching, not bill savings.

**Rationale:** Independent replay (codepointer, 2026-07-14, 614M tokens / $926) shows only 3.7% real bill savings for rtk/headroom/caveman — claims measure only the compressible payload; real bills are 42% cache-writes + 29% output. But free tiers bill in tokens: 30-90% compression = 2-3x more work per free token. The inversion is the product.

## 2026-08-14 — Port 20128

**Decision:** Listen on port 20128, `/v1` OpenAI-compatible.

**Rationale:** An existing local client config already pointed at `http://127.0.0.1:20128/v1` — drop-in, only the api_key changes.

## 2026-08-14 — No timings in docs

**Decision:** Roadmap has no deadlines/estimates.

**Rationale:** Personal project, explicit user requirement.

## 2026-08-16 — Chains are the core routing primitive

**Decision:** Routing is chain-based from day 1. Config defines named chains = ordered list of (provider, model) steps. Clients send logical names (chain name, or any model inside a chain) — never real provider model names. Unknown names resolve to `default_chain`. First successful step serves; failures walk the chain.

**Context:** Design requirement: each client gets its own cross-provider fallback chain, real model names never leak into clients, and the design stays modular. The relay stays dumb; the chain engine is a modular package.

**Rationale:** Quota stretching across providers needs cross-provider fallback anyway; chains subsume the earlier "no fallback yet" decision. One primitive covers both routing and failover. `/v1/models` exposes chain names.

## 2026-08-16 — Streaming + telemetry pulled into Phase 1

**Decision:** SSE streaming passthrough and async batched SQLite telemetry ship with the Phase 1 core, not Phase 2.

**Rationale:** The primary local client streams by default — Phase 1 is useless without streaming. Telemetry is a 30s-interval flusher (in-memory buffer, hot path never touches disk, crash loses <30s), built before providers multiply so the schema grows with real usage.

## 2026-08-16 — Gemini via its OpenAI-compatible endpoint

**Decision:** Gemini adaptor uses `https://generativelanguage.googleapis.com/v1beta/openai` (official OpenAI-compat mapping), pure passthrough, no format translation.

**Rationale:** Both adaptors share one code path (`openai` and `gemini` are config types pointing at different base URLs). Native translation can be added behind the Adaptor interface later if needed.

## 2026-08-16 — User-Agent on outbound requests

**Decision:** All outbound requests carry `User-Agent: guvna/0.1`.

**Rationale:** Live test: bazaarlink throttled UA-less requests to ~20s vs instant. Found during Phase 1 smoke test.

## 2026-08-16 — Client disconnect is not an upstream failure

**Decision:** When the client connection dies mid-relay (context canceled), the request is dropped silently — no telemetry record, no 502.

**Rationale:** Curl/pipe truncation in testing exposed this; real clients reading the full body never hit it. Recording 502 misattributed client behavior to upstream. Distinguish via `r.Context().Err()`.

## 2026-08-16 — Failure marking: cool-off, not binary on/off

**Decision:** Per-(provider, model) tracker: 3 consecutive failures → 60s cool-off, doubling per failure beyond threshold, capped at 10 min; auto-recovers when cool-off expires. Steps in cool-off are skipped in chains. Retries (2×: 250ms, 1s) only for network errors, 429, 5xx; 4xx never retried or marked.

**Rationale:** Daily use on the VPS means a rate-limited or down provider must not stall or kill chains, and a dead provider must recover without admin action. Cool-off gives self-healing without a health-probe fleet. 4xx are client errors — the step did its job, the caller is wrong.

## 2026-08-16 — VPS-first: gateway lives on the VPS

**Decision:** Primary deployment is the remote VPS behind caddy; Arch local install is deferred.

**Rationale:** Daily use is from the laptop anywhere (cloud-first pillar); the VPS has the TLS + always-on infrastructure. Distroless container, ~13MB image, `--memory=400m` cap, `GOMEMLIMIT=256MiB`.

## 2026-08-16 — Distroless static:nonroot

**Decision:** Final image is `gcr.io/distroless/static-debian12:nonroot` (multi-stage, CGO_ENABLED=0). Healthcheck is a `-healthcheck` flag on the binary itself.

**Rationale:** ~2MB base, zero CVEs, no shell — matches the low-resource/low-attack-surface philosophy. Distroless has no shell/wget, so the binary probes its own /healthz. `scratch` rejected: needs manual CA certs for outbound TLS.

## 2026-08-16 — Remote CLI over the gateway, no new port

**Decision:** Observability = `GET /admin/status` + `GET /admin/logs` in core (admin-key-only) + a `guvna-cli` binary (status/logs, `--json`), hitting the same caddy vhost over TLS.

**Rationale:** Nothing new exposed — the admin key already authenticates; TLS terminates at caddy. `status`/`logs`/usage from the laptop was the top capability for daily use.

## 2026-08-16 — caddy vhost on :9443, not :443

**Decision:** `gateway.example.com:9443` → `127.0.0.1:20128`. Caddy auto-TLS on the custom port.

**Rationale:** Another TLS service already binds `*:443` — two listeners on 443 produced flaky TLS handshakes (requests intermittently hitting the wrong socket). Same reason the dead new-api vhost used :9443.

## 2026-08-16 — host network mode in compose

**Decision:** `network_mode: host` on the VPS; no bridge network.

**Rationale:** The host's docker cannot create bridge networks (iptables setup fails — no iptables/legacy tooling present). Existing containers (caddy et al.) already run host-network. App binds 127.0.0.1:20128 directly; caddy reaches it the same way.

## 2026-08-16 — No catalogs, no strict model checks (upstream is the source of truth)

**Decision:** No local model catalog; chain/model validation is loose (provider must exist in config, model non-empty). `POST /v1/chains` accepts any model string. Cancelled the Phase 5 subscribe-sync as the enforcement mechanism.

**Context:** Mid-build reversal — decision to drop catalog creation and strict catalog checks entirely rather than defer them.

**Rationale:** Catalogs go stale (orcarouter ships new free models weekly). A stale catalog REJECTS valid new models — worse than a typo failing at request time (upstream 404s, chain falls over to the next step). For a single-user gateway the upstream provider IS the catalog. Revisit only if chain creation needs model discovery.

## 2026-08-16 — No default chains

**Decision:** Removed the built-in `free`/`fast`/`smart`/`coder` chains and the `default_chain` concept. `/v1/models` starts empty; unknown model → 404 with a pointer to `POST /v1/chains`. Every chain is created per use case by the client (any valid API key, persisted to `<data-dir>/chains.yaml`).

**Context:** Requirement: no default chains — every chain must be created per use case by the client.

**Rationale:** Chains are a routing primitive, not a product decision baked into config. Client-driven creation keeps the gateway generic — each app provisions exactly what it needs. Provider-prefixed passthrough (`<prefix>/<model>`) covers direct single-model calls without any chain at all.

## 2026-08-16 — Chain creation surface: API + CLI, not config

**Decision:** Chains are runtime state (chains.yaml in the data dir, atomic rewrite), managed via `POST/DELETE /v1/chains` and `guvna-cli chains` — live, no redeploy. Config-defined chains still load (merged, conflicts skipped with a warning).

**Rationale:** The gateway is cloud-first and client-driven; config-in-git is for infra, chains are per-use-case and change with the workload. Apps self-provision with their own API key.

## 2026-08-17 — Provider key pools with rotation + quarantine

**Decision:** Providers configure `key_envs: [K1, K2, ...]` forming a rotation pool (single `key_env` still valid = 1-key pool; both set = config error). Per-provider `rotation:` strategy: `round_robin` (default) / `least_used` / `sequential`. Class-based quarantine: 401/403 → `auth` (default 24h), 429 → `rate_limit` (60s), 5xx/network → `transient` (60s); per-provider `quarantine:` overrides; durations double per consecutive failure, capped. Success resets. Failover retries the same request on the next healthy key inside the step retry budget (401/403 consume budget too). All keys down → step down → chain walks to the next step. Streaming rotation is per-request only; a mid-stream key death propagates an error chunk — never a mid-stream swap.

**Context:** Requested: multiple rotation strategies so usage doesn't burn one key out, plus set-aside of seemingly-dead keys backed by health checking and observability.

**Rationale:** Free-tier keys die (quota exhaustion, revocations — all 3 staged groq keys returned 403). A dead key must cost one attempt, not a burned request: quarantine it class-appropriately (auth death = long; rate limit = short, recoverable) and let the pool forget it until it heals. Per-key observability (`/admin/status` keys section + `key` column in telemetry rows) makes the mechanism auditable; deliberately out of scope: quarantine persistence across restarts (in-memory like step health), CLI key commands (a UI comes later).

## 2026-08-17 — Verified: key failover + quarantine live

**Decision:** Live-verified on the VPS: with `BAZAARLINK_2_KEY` deliberately invalid, round-robin picked it second, 403 → quarantined (`auth`, failures=1), same request served by `BAZAARLINK_1`; `/admin/status` showed the quarantined key with reason; telemetry rows carry the serving key env. Local verification additionally covered: dead groq pool (all 3 keys 403 → all quarantined → last upstream 403 propagated), telemetry key attribution, and `/admin/status` per-key counters.

## 2026-08-17 — Gemma models verified through the OpenAI-compat path; CORS rejected

**Decision:** Added `gemma-4-31b-it` + `gemma-4-26b-a4b-it` to the gemini provider's `models:` list in config.yaml so `gemini/<model>` passthrough strips the prefix correctly. Live-verified through the `/v1beta/openai` path: both models complete (non-streaming + streaming, `data: [DONE]`), and function calling works — a `tools` payload returned a proper `tool_calls` response with JSON arguments. The gateway needs no native-Gemini translation adaptor for multi-app passthrough usage.

**Rejected:** CORS middleware in the gateway. Browser-only concern; no browser client exists yet, and current integrating apps are server-side (no CORS). When a browser client lands it should go through a thin server-side proxy that holds the client key and sets CORS headers — browser boundary belongs in the app layer, not the relay. Revisit only with a committed browser-direct design plus per-key quotas.

**Context:** Priority was migration-readiness — the gateway should have all building blocks ready for later migrations; question raised whether the gateway should ship CORS middleware.

## 2026-08-17 — VPS gemini/gemma unreachable: IPv4/IPv6 preference, NOT censorship (SUPERSEDED)

**Decision (superseded by the entry below):** Deployed the gemma-capable image to the VPS; local verification passed. On the VPS the direct gemini provider timed out — the host resolver returns only real Google AAAA records via getent, and `__VG_IPV4_*` A records via getaddrinfo — and I misread `curl -4`'s 403 as a filtering middlebox block. The keypool quarantine (transient, 60s doubling) did isolate the keys and chains fell over, which worked as designed. **Conclusion was wrong: it was never censorship.**

## 2026-08-17 — Prefer IPv4 dialing in adaptors (fixes gemini/gemma on the VPS)

**Decision:** The gateway's outbound transport now dials IPv4 when A records exist (clone of `http.DefaultTransport` with a `DialContext` that resolves `ip4` first and falls back to the default dialer for IP literals and v6-only hosts). Proven by a probe binary on the VPS: `net.DefaultResolver` ip4 → `__VG_IPV4_*` records, `tcp4` dial OK; default transport (Happy Eyeballs, prefers AAAA) → TLS EOF; forced `tcp4` → HTTP 200 in 137ms. The VPS's DNS virtual gateway serves working A records and dead AAAA ones — Go preferred the dead v6 path, which the keypool quarantine correctly recorded as transient network errors.

**Context:** Correctly diagnosed mid-debugging as the known IPv4-vs-IPv6 problem seen before on this host ("we definitely can access Google on the VPS") — and the fix ships in code (works for every deployment, not a VPS-specific workaround).

**Verified live:** `gemini/gemma-4-31b-it` chat completion 200 through `https://gateway.example.com:9443` with real content and usage; GEMINI_1_KEY healthy in `/admin/status`.

## 2026-08-17 — Per-step params on chains (fill-missing, client wins)

**Decision:** Chain steps gain an optional `params` object (any JSON values, no model-param whitelist — the gateway stays neutral about model catalogs). When a step serves, its params are shallow-merged onto the client request body **fill-missing**: a param applies only if the client didn't set that field; explicit client values always win. Keys `model`, `stream`, `messages` are rejected. Params are set per step, not per chain — mixed-tier chains (fast model + reasoning model) can tune each upstream call independently without risking 400s from providers that reject unknown fields (e.g. a chain-wide `reasoning_effort` breaks gemini).

**Context:** Proposed during downstream-app integration work: per-step request params on chains with step-wins semantics. Rejected step-wins: the chain author IS the requester in this gateway (any valid key self-provisions chains), so step-wins means the client silently overrides its own explicit values (`max_tokens: 2048` → 8192 with no signal) and widens the chain-poisoning blast radius on shared chain names. Fill-missing solves the motivating case identically (client sends plain body, step fills `reasoning_effort: high` + `max_tokens`) while keeping per-request tuning intact.

**Also:** responses now carry `x-guvna-step: <provider>/<model>` so clients can see which step served and whether params applied (works for SSE — header precedes the stream). chains.yaml gains a `version` field; no backward-compat constraint — nothing is migrated yet, chains are regenerable runtime state. Config-defined chains get params too (config.yaml parity). CLI stays JSON-only for params chains (repeatable `--param` flags can't express arrays/objects or bind unambiguously to a step).
