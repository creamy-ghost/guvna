# Dashboard

Optional web UI for the gateway: status, chains CRUD, key pools, logs.
No playground (dropped — the CLI and API cover ad-hoc chat).

## Run it

Same repo, separate binary + image. The gateway deploy is untouched;
the dashboard only starts when asked (zero RAM when off).

```sh
# local (gateway on :20128)
GUVNA_URL=http://127.0.0.1:20128 GUVNA_ADMIN_KEY=... \
  go run ./cmd/guvna-dashboard            # listens on 127.0.0.1:20129

# compose (VPS)
docker compose -f deploy/docker-compose.yml --profile dashboard up -d --build
```

Open `http://127.0.0.1:20129`. Tabs lazy-load on click; status/keys poll
every 5s and logs every 3s, only while their tab is open. Switching tabs
kills the previous poller (the trigger lives inside the loaded partial).

## How it works

- Pure proxy: `internal/dashboard` talks HTTP to the gateway only
  (`GET /admin/status`, `GET /admin/logs`, `GET/POST/DELETE /v1/chains`) —
  the same surface as `guvna-cli`. No gateway changes, no shared imports
  with `internal/server`.
- The admin key stays server-side (env). Browsers see rendered HTML only.
- Frontend is vendored, no CDN, no build step: `web/static/htmx.min.js`
  (htmx 2.0.10, pinned stable), `web/static/pico.min.css` (Pico 2.1.1),
  Go `html/template` partials embedded via `go:embed`.
- `/admin/status` is cached 2s so concurrent tab polls coalesce.
- Chain deletes are limited to `source: runtime` chains in the UI;
  config-defined chains show as locked (they live in `config.yaml`).

## Exposure

Never expose the dashboard directly. It binds localhost; put a reverse
proxy with access control in front (same pattern as the gateway vhost):

```
dashboard.example.com:9444 {
    basic_auth {
        admin $2a$...
    }
    reverse_proxy 127.0.0.1:20129
}
```

Anyone who reaches the dashboard can mutate chains (it acts with the
admin key), so the proxy auth is the access control. No built-in login
by design — single user, network boundary instead of a token scheme.

## Budget

Idle RSS ~8–12MB, distroless image ~15MB, `mem_limit: 128m`,
`GOMEMLIMIT=64MiB`. Static assets ~134KB total.
