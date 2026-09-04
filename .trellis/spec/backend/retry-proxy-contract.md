# reproxy Protocol Contract

> Executable contract for the retry query namespace and proxy behavior.
> Source: task 09-04-implement-reproxy-mvp (audit-report.md pinned semantics, mutation-locked by 23/23 captured mutations).

---

## 1. Scope / Trigger

Any change to request parsing, retry policy resolution, response headers, or budget handling in reproxy touches this contract. The proxy is **general-purpose**: defaults and ranges must never be justified by an example use case (e.g. LLM APIs).

## 2. Signatures

```go
// query.go — namespace split before any policy work
func SplitQuery(rawQuery string) (passthrough string, retryParams url.Values, err *RequestError)

// policy.go — three-tier resolution, fail-closed
func Parse(params url.Values, cfg *ServerConfig) (Policy, *RequestError)

// backoff.go — per-retry wait computation
func ComputeWait(sp ScopePolicy, attemptNo int, retryAfterHeader string, now time.Time, budgetRemaining time.Duration) time.Duration
```

## 3. Contracts

### Request: query namespace split

- Keys with prefix `retry.` are stripped from the forwarded query; everything else is forwarded **byte-identical** (`RawQuery` never re-encoded — signed-URL safety).
- Unknown `retry.*` keys are a 400, not ignored.

### Policy: three-tier field-level resolution

```
built-in defaults -> retry[*].FIELD -> retry[NNN].FIELD
```

- Gates live only at `[*]` level: `retry.status` (comma list: 3-digit codes 100–599, closed ranges `500-599`, class `4xx`/`5XX` case-insensitive), `retry.network` (`0`/`1` only — valueless is a 400, fail closed), `retry.budget` (duration with unit, > 0).
- Scope fields: `attempts` (TOTAL incl. first; 1 = no retry), `backoff` (`constant|linear|exponential`), `initial`, `max`, `jitter` (`none|full|equal`), `retry_after` (`honor|ignore`).
- `attempts=1` is legal (means no retry). Dead config — `retry[NNN]` scope whose code is not in `retry.status` — is a 400.
- Server clamps (`MaxAttempts`, `MaxBudget`) narrow and never widen.

### Response headers / observability

`X-Retry-Count`, `X-Retry-Limit`, `X-Retry-Exhausted` on every retried-path response; `X-Retry-Dropped` when an oversized body is degraded to streaming pass-through. Per-retry log lines carry the computed wait (`event=wait backoff=...`).

### Budget semantics

`retry.budget` is a hard cap over attempts + waits, checked at the loop top (first attempt exempt). Exhaustion commits the held upstream response (delivered as-is, `X-Retry-Exhausted: true`); it never fabricates a response.

## 4. Validation & Error Matrix

| Condition | Result |
|---|---|
| Unknown `retry.*` key | 400, names the key, lists recognized keys |
| `attempts < 1` / non-integer | 400 |
| `max < initial` (in a resolved scope) | 400 — checked AFTER scope assembly, never per-field (order-independent) |
| `retry[NNN]` code not in `retry.status` | 400 (dead config) |
| Gate key inside a scope (`retry[429].status`) | 400 — gates are global-only |
| Reversed/open range, empty list item, out-of-range code | 400 |
| `retry.network` / `retry.budget` invalid or valueless | 400 (fail closed) |
| All 400 bodies | JSON `{error, hint}` |

## 5. Good/Base/Bad Cases

- Good: `?retry.status=5xx&retry[*].attempts=4&retry[429].attempts=2` — 429 uses attempts=2, everything else in the gate uses 4.
- Base: no retry params — pure reverse proxy, zero behavioral overhead.
- Bad: `?retry.status=429&retry[500].attempts=2` — 400 dead config (500 not in gate).

## 6. Tests Required

- Byte preservation: `TestSplitQueryPassthroughBytePreservation`, `TestE2EQueryBytePreservation` (RawQuery byte-identical round trip).
- Order independence: `TestParseFieldOrderIndependence` (50 iterations — map iteration order must not change the verdict).
- Dead config: `TestParseDeadConfig` + `TestProxyDeadConfig400`.
- Mutation lock: every row of the validation matrix has a test that fails when the behavior is broken (see mutation-scan.md in the task dir).

## 7. Wrong vs Correct

### Wrong
```go
// Validating max >= initial inside the per-field switch:
if sp.Max < sp.Initial { return bad(...) }
// map iteration order decides which field lands last -> legal configs randomly 400
```

### Correct
```go
// Assemble the whole scope first, validate cross-fields once after:
if err := validateScopeCrossFields(policy.Default, "retry[*]"); err != nil { return Policy{}, err }
```
