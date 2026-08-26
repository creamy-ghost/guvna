# Security Policy

## Reporting

Report vulnerabilities privately via [GitHub Security Advisories](https://github.com/creamy-ghost/guvna/security/advisories/new). Please do not open public issues for security problems. You can expect an initial response within 7 days.

## Scope

Guvna is a **single-user** gateway holding valuable secrets: admin key, client API keys, and upstream provider keys.

In scope:

- Auth bypass on `/v1`, `/admin/*`, or chain management endpoints
- Provider-key leakage through logs, telemetry, error messages, or `/admin/status`
- Path traversal / arbitrary file read via `-config`/`-data` handling
- Injection into the SQLite telemetry store
- Anything letting one client key read another client's chains or usage

Out of scope (by design):

- Multi-tenant isolation — Guvna serves one operator; all client keys are trusted to that operator
- DoS resilience beyond basic timeouts — it is meant to run behind a reverse proxy
- The host environment (reverse proxy TLS, firewall) — see docs/DEPLOYMENT.md hardening notes

## Design guarantees

- Auth cannot be disabled: without valid admin/client keys the gateway refuses every route
- Provider keys exist only in process env + memory; never logged, never persisted to disk by the gateway
- Telemetry stores hashes-free request metadata (model, tokens, serving key reference), not message contents
