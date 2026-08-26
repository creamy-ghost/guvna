# Vision & Goals

## Vision

A low-resource, single-user AI gateway that makes free-tier quotas go 2-3x further via token compression, and is cloud-first by design — the deliberate inversion of OmniRoute.

Free-tier providers (Bazaarlink, Groq, Mistral, Gemini, OrcaRouter) bill in tokens, not dollars. Compression that saves 30-90% of tokens means 2-3x more work per free token. That is the whole point of a free-catalog x compression gateway.

## Design pillars

1. **Single-user** — one admin key, global usage stats, minimal security surface. Multi-user is out of scope — per-upstream-account quotas make shared use self-defeating.
2. **Cloud-first** — headless daemon, one static binary, one port, config-as-code YAML in git (never DB-driven config), SQLite only for usage/telemetry, distroless image, one-command deploy.
3. **Security default-on** — auth always enabled, scoped tokens for remote CLI, no open-by-default dashboard.
4. **Upstream is the catalog** — no local model catalogs, no strict model-existence checks: chain creation validates loosely and unknown models fail at request time, where the chain falls over. A stale local catalog would reject valid new models; the provider can't. (Supersedes the original catalog-sync pillar — see DECISIONS.md.)

## Goals (v1)

- `/v1` OpenAI-compatible API (chat completions, streaming SSE) on port 20128 — drop-in for any OpenAI-compatible client
- Provider layer with adaptors pattern, fallback + retries
- Loose validation, no local catalogs (see pillar 4)
- 3 compression engines:
  1. Caveman-style output mode (JSON rule packs + system-prompt injection, ~zero runtime cost)
  2. RTK-style tool-output filters for dominant commands (git/grep/ls/build logs)
  3. Session dedup / repetition pruning (content-addressed)
- Config-as-code YAML; SQLite for usage/telemetry only
- Single admin key + API keys (single-user)
- Minimal read-only web UI — separate optional binary, never in core
- Local CLI + remote CLI with scoped tokens
- systemd unit on Arch + container on a VPS
- Tests + release docs

## Non-goals (v1)

- Multi-user accounts, per-user quotas, billing
- MCP / A2A / Electron app
- LLMLingua-2 (ONNX MobileBERT) and headroom-style cache-aware live-zone compression
- WebSocket realtime, context relay, combos

## Future (v2, not scoped)

- LLMLingua-2 + headroom-style cache-aware engines (only compress the uncached tail so prompt cache stays intact)
- Auth bolted on (per-user keys) without touching router core
- Combo / context-relay routing strategies
