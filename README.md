# Guvna

Low-resource AI gateway in Go. One OpenAI-compatible endpoint that routes to free-tier LLM providers through named, self-healing chains — built for one user, runs in ~15–40MB of RAM.

[![CI](https://github.com/creamy-ghost/guvna/actions/workflows/ci.yml/badge.svg)](https://github.com/creamy-ghost/guvna/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

## Why

Most LLM gateways are multi-tenant platforms: databases, dashboards, hundreds of MB of RAM. Guvna is the opposite bet — **config-as-code, no database, one static binary** (~16MB, distroless image ~13MB):

- **Chain-based routing** — define ordered provider/model fallback steps per use case (`mychain` → bazaarlink qwen → groq llama → mistral codestral). Clients call chain names; real provider model names never leak into your apps.
- **Key pools** — per-provider rotation (round-robin / least-used / sequential) with class-based quarantine (auth / rate-limit / transient) and automatic failover. One dead key never burns the pool.
- **Provider-prefixed passthrough** — call any catalog model directly as `<provider>/<model>` when you don't need a chain.
- **Self-healing** — unhealthy providers cool off (60s → 10min, auto-recovery); chains fail over across steps; no silent mid-stream restarts.
- **Remote CLI** — `guvna-cli status|logs|chains` against the gateway's HTTPS endpoint, from anywhere.
- **Observability** — async SQLite telemetry (requests, tokens, serving key), `/admin/status` + `/admin/logs`.
- **Free-tier native** — ships with configs for Bazaarlink, Groq, Mistral, Gemini, OrcaRouter free tiers.

## Quickstart

```sh
git clone https://github.com/creamy-ghost/guvna && cd guvna
cp .env.example .env        # fill in ADMIN_KEY, API_KEYS, and at least one provider key
docker compose -f deploy/docker-compose.yml up -d
```

Or without Docker:

```sh
go install github.com/creamy-ghost/guvna/cmd/guvna@latest
set -a; source .env; set +a
guvna -config config.yaml          # listens on :20128
```

Tagged releases also publish an image to `ghcr.io/creamy-ghost/guvna`.

### First request

Chains are created by clients over the API (there are no default chains):

```sh
curl -X POST http://127.0.0.1:20128/v1/chains \
  -H "Authorization: Bearer $CLIENT_KEY" -H "Content-Type: application/json" \
  -d '{"name":"myfree","steps":[
        {"provider":"bazaarlink","model":"qwen/qwen3.7-flash:free"},
        {"provider":"groq","model":"llama-3.3-70b-versatile"}]}'

# chat — any OpenAI SDK works, just point base_url at Guvna
curl http://127.0.0.1:20128/v1/chat/completions \
  -H "Authorization: Bearer $CLIENT_KEY" -H "Content-Type: application/json" \
  -d '{"model":"myfree","messages":[{"role":"user","content":"hello"}]}'
```

If `bazaarlink` is down or rate-limited, the same request transparently falls through to `groq`. Or skip chains entirely: `"model": "groq/llama-3.3-70b-versatile"`.

Manage chains remotely:

```sh
export GUVNA_URL=https://your-gateway.example.com GUVNA_ADMIN_KEY=...
guvna-cli chains list
guvna-cli status
```

## Configuration

Everything is YAML in git (`config.yaml`): providers, models, rotation strategy, chains. Keys live only in env (`.env.providers`, gitignored). Per-step request params can be set inside chain steps (`params`, client values always win). See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full data model and [docs/FREE_MODELS.md](docs/FREE_MODELS.md) for per-provider free-tier limits.

## Status

Use-ready and running in production (single user) since 2026-08-16. Compression engines and further phases are tracked in [docs/ROADMAP.md](docs/ROADMAP.md).

## Documentation

| File | What it contains |
|---|---|
| [docs/VISION.md](docs/VISION.md) | Vision, v1 goals, non-goals |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Design: components, data model, security, chains, key pools |
| [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) | Deploy anywhere: Compose, binary/systemd, TLS, keys, chains |
| [docs/RESEARCH.md](docs/RESEARCH.md) | Why: comparisons (new-api, litellm, OmniRoute), compression reality check |
| [docs/ROADMAP.md](docs/ROADMAP.md) | Build order, phase-by-phase definition of done |
| [docs/DECISIONS.md](docs/DECISIONS.md) | Decision log with dates and rationale |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: [SECURITY.md](SECURITY.md).

## License

MIT. All reference material used (OmniRoute catalog data, new-api, rtk, caveman) is MIT.
