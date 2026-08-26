# Research

Everything below was verified against primary sources in Aug 2026. Confidence is stated per claim.

## Why scratch, not port or fork

### Full port of OmniRoute: impossible
- 8,512 TypeScript files, 320+ contributors, 47.5K stars, monorepo (Next.js, packages/, electron/, open-sse/)
- A scoped reimplementation of the core routing subset is feasible at ~3-6k lines

### Fork of new-api (QuantumNous/new-api, Go, MIT, one-api successor)
- Pros: Go single binary, adaptors (~100+ providers), fallback + billing built in, ~1-2 weekends to a working gateway
- Cons:
  - Inherits ~80-150MB idle floor (embedded React dashboard) — conflicts with low-resource goal
  - Permanent merge tax against fast-moving upstream (one-api fork graveyard is the evidence)
  - Billing-first data model (users/quotas/groups/tokens) fights the free-catalog + compression-first vision — you'd be unbuilding more than you keep
- **Verdict: fork only wins if multi-user billing + 100+ adaptors at launch are wanted. We don't want either (DECISIONS.md).**

### Fork/port of litellm (BerriAI, Python): rejected
- 140+ providers, enterprise cost governance, but NO free catalog and NO built-in compression
- Rust migration (announced 2026-06-22): Rust core is a beta hot-path accelerator inside the Python host (auth/routing/logging stay Python; full Rust server targeted Dec 1 2026). Benchmarks: 31.7MB peak, 6782 req/s, 0.66ms p99
- Not a standalone free-catalog gateway; forking it fights its architecture direction. It's also marketing itself as "fastest, litest" — closing the low-resource wedge. Confidence: high (launch blog + benchmarks verified).

### Scratch: chosen
- Matches the vision (free catalog + compression + low-resource + cloud-first)
- Go ~15-40MB RSS, single static binary, fast enough for a network-bound gateway
- Rust would be ~10-30MB but ~50% slower to develop — Go chosen (DECISIONS.md)

## Compression reality check (critical inversion)

Independent replay (codepointer.substack.com, 2026-07-14) of rtk/headroom/caveman over a 614M-token corpus / $926 real spend: **combined 3.7% bill savings** (rtk 0.5%, headroom 2.8%, caveman 0.4%).

Why claims are inflated: they measure only the compressible payload. Real bills are ~42% cache-writes + ~29% output tokens; streams compression doesn't touch output; cache reads (what compression removes) are the cheapest token.

**The inversion for Guvna:** we target free tiers whose quotas are token-count-based. Compression = quota stretching (2-3x more work per free token), not bill savings. The numbers above do not invalidate the product — they invalidate the "bill savings" framing. Guvna's framing is quota stretching.

## OmniRoute facts used as reference (all MIT)

- Catalog: `open-sse/config/freeModelCatalog.ts` (530 free models), `src/shared/constants/providers.ts` (hasFree + freeNote per provider)
- Claims: 330+ providers, 90+ free, ~1.54-1.94B free tokens/mo across 42-50 free-tier pools (they debunk their own 10.87B ceiling — the honest numbers are the real ones; free catalog re-audited every 2 weeks)
- 12 compression engines; presets: Lite ~15%, Standard/Caveman ~30%, Aggressive ~50%, Ultra ~75%, RTK 60-90% on tool output, Stacked (RTK→Caveman) 78-95%
- Compression decision precedence: per-request header → combo override → named profile → adaptive → panel default → off; cache-aware always-on
- Auth model: single admin (INITIAL_PASSWORD + JWT_SECRET) + API keys + scoped CLI tokens. NOT multi-tenant — no "multi-user" anywhere in docs (code search: 0 results). Confidence: high.
- Known free tiers: Kiro AI (~50 free Claude credits/mo), OpenCode Free (no auth), Pollinations, Cerebras (1M tokens/day), Groq

## Single-user vs multi-user (the free-tier math)

- Free quotas are per-upstream-account: 10 users sharing one gateway account = quota exhaustion
- The only honest multi-user free model = users bring their own keys = turns into new-api
- Single-user core; auth (per-user keys) can be bolted on later without touching the router
- Confidence: high (verified quota model of Kiro/Cerebras; OmniRoute single-admin pattern)

## References

| Source | Use |
|---|---|
| github.com/diegosouzapw/OmniRoute | catalog data, compression engine design, env var semantics |
| github.com/QuantumNous/new-api | adaptor pattern, relay design, SSE relay (relay/relay_adaptor.go, StreamScannerHandler) |
| github.com/rtk-ai/rtk | tool-output filter behavior (reimplement in Go, no lib API) |
| github.com/JuliusBrussee/caveman | JSON rule packs + language packs (MIT) |
| codepointer.substack.com (2026-07-14) | compression reality check |

All MIT-licensed; data legally liftable with attribution.
