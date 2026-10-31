## 2026-10-31 - Redundant JSON Unmarshaling on Hot Path
**Learning:** Chaining separate functions for JSON manipulation (e.g., `withModel` followed by `mergeParams`) in request handlers causes multiple full unmarshal/marshal cycles on the same payload. This is a severe anti-pattern in a high-throughput proxy as it wastes significant CPU and allocations on the request hot path.
**Action:** When a request body requires multiple transformations, combine them into a single parsing pass.
