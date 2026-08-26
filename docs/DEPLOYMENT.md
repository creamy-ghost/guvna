# Deployment

Guvna is a single static binary with config-as-code. Two supported ways to run it.

## Option A — Docker Compose

```sh
git clone https://github.com/creamy-ghost/guvna && cd guvna
cp .env.example .env              # add ADMIN_KEY + API_KEYS + provider keys
docker compose -f deploy/docker-compose.yml up -d
```

The compose file builds the distroless image locally (~13MB, no shell, runs as nonroot) and mounts a named volume at `/data` for SQLite telemetry + `chains.yaml`.

Or use the published multi-arch image (amd64/arm64):

```sh
docker run -d --name guvna \
  -p 20128:20128 \
  --env-file .env \
  -e GOMEMLIMIT=256MiB \
  -v guvna-data:/data \
  -v "$PWD/config.yaml:/config.yaml:ro" \
  ghcr.io/creamy-ghost/guvna:latest
```

## Option B — Plain binary / systemd user unit

```sh
go install github.com/creamy-ghost/guvna/cmd/guvna@latest
cp .env.example .env && set -a && source .env && set +a   # or use systemd EnvironmentFile=
guvna -config config.yaml                             # data dir defaults to ~/.guvna
```

A minimal unit runs it as your user on port 20128, keys via env, never in git.

## TLS exposure

Keep the gateway localhost-only unless you need remote clients. When exposing:

- Terminate TLS in front (caddy example below). Auth is always on (admin key + client API keys), but keys should still travel over TLS.

```
gateway.example.com {
  reverse_proxy 127.0.0.1:20128
}
```

- If another service already binds `*:443`, put the gateway vhost on a different port (e.g. `:9443`) — two listeners on 443 cause flaky TLS handshakes.

## Keys & rotation

Provider keys live in env via `key_envs` lists in config.yaml; multiple keys per provider form a rotation pool:

- `rotation:` per provider — `round_robin` (default) / `least_used` / `sequential`
- Class-based quarantine — 401/403 → `auth` (24h), 429 → `rate_limit` (60s), 5xx/network → `transient` (60s); cool-off doubles per consecutive failure; success resets
- On a quarantinable response the same request retries on the next healthy key (within the step retry budget); all keys down → chain falls to the next step
- Per-key state is visible in `/admin/status` → `keys`; telemetry rows carry the serving `key`
- Quarantine is in-memory — restart clears it

To rotate a key: edit `.env`, then `docker compose up -d --force-recreate` (or restart the process).

## Chains workflow

The gateway starts with **zero chains**. Clients create them per use case (any valid API key); chains persist in `/data/chains.yaml` and survive restarts. Config-defined chains also load (merged, conflicts skipped with a warning).

```sh
# API (client key)
curl -X POST http://127.0.0.1:20128/v1/chains \
  -H "Authorization: Bearer $CLIENT_KEY" -H "Content-Type: application/json" \
  -d '{"name":"myfree","steps":[{"provider":"bazaarlink","model":"qwen/qwen3.7-flash:free"},{"provider":"groq","model":"llama-3.3-70b-versatile"}]}'
curl -X DELETE http://127.0.0.1:20128/v1/chains/myfree -H "Authorization: Bearer $CLIENT_KEY"

# CLI (same surface, works remotely over HTTPS)
guvna-cli chains list
guvna-cli chains add myfree --step bazaarlink:qwen/qwen3.7-flash:free --step groq:llama-3.3-70b-versatile
guvna-cli chains rm myfree
```

Chat with a chain name as the model, or bypass chains entirely with a provider-prefixed model (`groq/llama-3.3-70b-versatile`). Unknown model → 404 with a pointer to the creation API.

## Observability

```sh
export GUVNA_URL=http://127.0.0.1:20128 GUVNA_ADMIN_KEY=...
guvna-cli status          # chains, step health, usage table (--json for raw)
guvna-cli logs -n 100     # tail the in-memory ring
curl -s -H "Authorization: Bearer $ADMIN_KEY" http://127.0.0.1:20128/admin/status | jq
```

Admin endpoints (`/admin/status`, `/admin/logs`) are admin-key-only.

## Gotchas worth knowing

- Docker `--env-file` does **not** strip quotes around values (shell `source` does) — keep values unquoted
- Nonroot (UID 65532) cannot `mkdir /data` at container root — the image creates + chowns it at build time
- Client disconnects mid-stream are dropped silently (never recorded as upstream failures)

## Security defaults

- Auth always on (admin key + API keys) — never exposed without it
- Remote CLI authenticates against the same gateway HTTPS endpoint
- TLS via your reverse proxy when exposed; localhost-only otherwise
