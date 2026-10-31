## 2025-02-18 - Avoid double JSON serialization in hot paths
**Learning:** Chaining multiple functions that independently unmarshal and marshal the same JSON payload in an HTTP proxy hot path introduces significant unnecessary GC pressure and CPU overhead.
**Action:** When applying multiple mutations to a JSON body (e.g., updating a `model` field and merging in default parameters), combine them into a single pass that unmarshals the JSON once, applies all modifications, and marshals it once.
