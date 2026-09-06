# reproxy Protocol Contract

> Executable contract for the retry control-plane channels, mode grammar, and proxy behavior.
> Source: task 09-04-implement-reproxy-mvp (23/23 mutations), task 09-04-query-ownership-modes (29/29 mutations; design.md §1–§9, decisions D1–D15), and task 09-06-no-phantom-migration (3/3 mini-scan; plain scheme → pure terminal, transition layer deleted).

---

## 1. Scope / Trigger

Any change to request parsing, mode/channel resolution, retry policy resolution, response headers, or budget handling in reproxy touches this contract. The proxy is **general-purpose**: defaults and ranges must never be justified by an example use case (e.g. LLM APIs).

## 2. Signatures

```go
// query.go — namespace split before any policy work (retry mode only)
func SplitQuery(rawQuery string) (passthrough string, retryParams url.Values, err *RequestError)

// target.go — path grammar returns the query-ownership mode
func ParsePath(escapedPath string) (PathTarget, *RequestError) // PathTarget carries Mode (plain form resolves to pure)

// header.go — header channel: reserved-namespace guard + policy parse
func ParseRetryPolicyHeader(h http.Header) (url.Values, *RequestError) // nil params = header absent
func validateReproxyNamespace(h http.Header) *RequestError              // runs in EVERY mode
func isReproxyHeader(name string) bool                                 // strip predicate in buildOutboundHeaders

// policy.go — three-tier resolution, fail-closed
func Parse(params url.Values, cfg *ServerConfig) (Policy, *RequestError)

// policy.go — D13 single-attempt literal for headerless pure mode
func SingleAttemptPolicy(cfg *ServerConfig) Policy

// backoff.go — per-retry wait computation
func ComputeWait(sp ScopePolicy, attemptNo int, retryAfterHeader string, now time.Time, budgetRemaining time.Duration) time.Duration
```

## 3. Contracts

### Request: mode grammar

```
SCHEME-SEGMENT := SCHEME [ "+" MODE ]    // matched on ORIGINAL ESCAPED bytes
SCHEME         := "http" | "https"       // case-insensitive, lowercased
MODE           := "retry" | "pure"       // case-insensitive, lowercased with the scheme
```

- Plain scheme → **pure mode (terminal state)**: query never split, body streams, single attempt. (D15's v0.3 flip criteria are moot — the flip already happened; there was no deployed audience to migrate.)
- `%2B` is not `+`: `/https%2Bpure/host` is a 400 as the literal segment `https%2Bpure` (original-bytes-first; `ParsePath` receives `r.URL.EscapedPath()` — never "simplify" to `r.URL.Path`).
- Invalid forms → 400 naming the raw quoted segment; `usageHint` names all three shapes.

### Request: query namespace split (retry mode only)

- Keys with prefix `retry.` or `retry[` are stripped from the forwarded query; everything else is forwarded **byte-identical** (`RawQuery` never re-encoded — signed-URL safety).
- Unknown `retry.*` keys are a 400, not ignored.
- **Pure mode never calls SplitQuery**: `RawQuery` forwards verbatim, `retry.*` spellings are target data (the collision case: upstream `?retry.count=5` reaches the upstream untouched).

### Request: header channel (`X-Reproxy-Retry-Policy`)

```
X-Reproxy-Retry-Policy: status=5xx; network=1; budget=30s; [*].attempts=3; [429].attempts=5
```

- Pairs split on `;`, whitespace-trimmed; `key=value` where keys are the query grammar minus the `retry.` prefix (scope keys map to `retry[*].x`, never `retry.[*].x`).
- The parser is a **pure transform to `retry.`-prefixed url.Values fed to the existing `Parse()`** — identical validation matrix and 400 bodies, zero new field rules.
- One occurrence only (multiple → 400). Values never contain `;` or `=`; values are never URL-decoded.
- **Degenerate input → 400 in every mode** (empty value, `;;`, whitespace-only, missing `=`, empty key): absent is the ONLY no-policy spelling.
- `X-Reproxy-*` is reserved on every request in every mode: unknown member → 400; all members stripped before forwarding upstream.

### Mode × channel matrix (the behavioral spec)

| Path mode | Header? | Behavior |
|---|---|---|
| `+pure` | no | Pure proxy: query verbatim, body streams (no capture), single attempt (literal, D13) |
| `+pure` | yes | Policy from header; **Capture runs** (replay required — cap/413/degraded revive) |
| plain | no | Pure mode (same as `+pure`, terminal) |
| plain | yes | Header channel with pure mode — the intended combo, NOT a conflict |
| `+retry` | no | v0.1.0 semantics exactly |
| `+retry` | yes | **400 conflict** — names both channels + remedy + actor source |

Invariant: query channel and header channel are mutually exclusive **whenever query-splitting is active** — fail-closed 400, never silent precedence.

### Policy: three-tier field-level resolution

```
built-in defaults -> retry[*].FIELD -> retry[NNN].FIELD
```

- Gates live only at `[*]` level: `retry.status` (comma list: 3-digit codes 100–599, closed ranges `500-599`, class `4xx`/`5XX` case-insensitive), `retry.network` (`0`/`1` only — valueless is a 400, fail closed), `retry.budget` (duration with unit, > 0).
- Scope fields: `attempts` (TOTAL incl. first; 1 = no retry), `backoff` (`constant|linear|exponential`), `initial`, `max`, `jitter` (`none|full|equal`), `retry_after` (`honor|ignore`).
- `attempts=1` is legal (means no retry). Dead config — `retry[NNN]` scope whose code is not in `retry.status` — is a 400.
- Server clamps (`MaxAttempts`, `MaxBudget`) narrow and never widen.

### Response headers / observability

`X-Retry-Count`, `X-Retry-Limit`, `X-Retry-Exhausted` emitted **only when a retry lifecycle exists**: retry mode always; pure mode only with a header policy. Headerless pure mode emits none of them (single attempt, nothing to observe) — including on the 504 network-failure path (`exhausted()` gates on the same lifecycle flag). `X-Retry-Dropped` when an oversized body is degraded to streaming pass-through (retry mode, or `+pure`+header where capture revived). Per-retry log lines carry the computed wait (`event=wait backoff=...`).

### Body capture invariant (D13/D14)

- `Capture()` exists solely to replay across attempts. `pure ∧ no header ⇒ no Capture` — the body streams to the upstream with **inbound framing preserved** (`req.ContentLength = r.ContentLength`, never unconditionally `-1`/chunked).
- The headerless-pure policy is `SingleAttemptPolicy(cfg)` — a literal, never `Parse(url.Values{})`: empty input returns the v0.1.0 defaults (3 attempts, network=1) and would smuggle a retry lifecycle into pure mode. The Budget field stays live (feeds per-try TTFB, clamped by `MaxBudget`).

### Budget semantics

`retry.budget` is a hard cap over attempts + waits, checked at the loop top (first attempt exempt). Exhaustion commits the held upstream response (delivered as-is, `X-Retry-Exhausted: true`); it never fabricates a response.

## 4. Validation & Error Matrix

| Condition | Result |
|---|---|
| Unknown `retry.*` key | 400, names the key, lists recognized keys |
| Unknown scheme-segment form (`https+retrt`, `+`, `+pure+retry`, `https%2Bpure`) | 400 naming the raw quoted segment, hint names all three shapes |
| `+retry` mode + `X-Reproxy-Retry-Policy` present | 400 conflict — names both channels, remedy (+pure or remove header), actor source (middleware possibility) |
| Unknown `X-Reproxy-Foo` header | 400 (reserved namespace, every mode) |
| Multiple `X-Reproxy-Retry-Policy` occurrences | 400 |
| Degenerate header (empty / `;;` / whitespace-only / missing `=` / empty key) | 400 in every mode — absent is the only no-policy spelling |
| `attempts < 1` / non-integer | 400 |
| `max < initial` (in a resolved scope) | 400 — checked AFTER scope assembly, never per-field (order-independent) |
| `retry[NNN]` code not in `retry.status` | 400 (dead config) |
| Gate key inside a scope (`retry[429].status`) | 400 — gates are global-only |
| Reversed/open range, empty list item, out-of-range code | 400 |
| `retry.network` / `retry.budget` invalid or valueless | 400 (fail closed) |
| All 400 bodies | JSON `{error, hint}` |

## 5. Good/Base/Bad Cases

- Good: `?retry.status=5xx&retry[*].attempts=4&retry[429].attempts=2` — 429 uses attempts=2, everything else in the gate uses 4.
- Good: `/https+pure/api.example.com/x?retry.count=5` — collision case: `retry.count` is target data, reaches the upstream verbatim, single attempt.
- Good: `/https+pure/host` + `X-Reproxy-Retry-Policy: status=5xx; [*].attempts=3` — header policy drives retries, query untouched.
- Good: `/https/host` + `X-Reproxy-Retry-Policy` — plain form is pure mode: the intended header-channel combo, retries per policy.
- Base: no retry params in retry mode — pure reverse proxy, zero behavioral overhead.
- Bad: `?retry.status=429&retry[500].attempts=2` — 400 dead config (500 not in gate).
- Bad: `/https+retry/host?retry.status=5xx` + policy header — 400 both-channels conflict.

## 6. Tests Required

- Byte preservation: `TestSplitQueryPassthroughBytePreservation`, `TestE2EQueryBytePreservation` (RawQuery byte-identical round trip).
- Mode grammar: `target_test.go` grammar table (3 valid forms × schemes × case variants; invalid forms incl. escaped-`%2B` rows).
- Mode/channel matrix: `TestProxyModeChannelConflictMatrix` (every §3 row).
- Pure mode: `TestProxyPureModeQueryVerbatimAndSingleCall`, `TestProxyPureModeBodyStreamsNoCapture` (incl. ContentLength framing assertion), `TestProxyPureModeSingleAttemptLiteralNotParseDefaults`.
- Plain form: `TestProxyPlainSchemeQueryNeverSplit` — plain never splits the query; `retry.*`-shaped keys reach the upstream verbatim.
- Header channel: `header_test.go` grammar + degenerate tables, `TestHeaderTransformEquivalence` (query vs header → deep-equal Policy).
- Order independence: `TestParseFieldOrderIndependence` (50 iterations — map iteration order must not change the verdict).
- Dead config: `TestParseDeadConfig` + `TestProxyDeadConfig400`.
- Mutation lock: every row of the validation matrix has a test that fails when the behavior is broken (see mutation-scan.md in the task dir — 29/29 captured).

## 7. Wrong vs Correct

### Wrong
```go
// Validating max >= initial inside the per-field switch:
if sp.Max < sp.Initial { return bad(...) }
// map iteration order decides which field lands last -> legal configs randomly 400
```

```go
// Headerless pure mode reusing Parse for "defaults":
policy = Parse(url.Values{}, cfg)
// Parse(∅) returns 3 attempts + network=1 — a retry lifecycle smuggled into pure mode
```

### Correct
```go
// Assemble the whole scope first, validate cross-fields once after:
if err := validateScopeCrossFields(policy.Default, "retry[*]"); err != nil { return Policy{}, err }
```

```go
// D13: the single attempt is a literal (Attempts=1, NetworkGate off, StatusGate nil);
// only Budget stays live — it feeds per-try TTFB, not retries:
policy = SingleAttemptPolicy(cfg)
```
