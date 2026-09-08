# reproxy Protocol Contract

> Executable contract for the retry policy carriers, request grammar, and proxy behavior.
> Source: task 09-04-implement-reproxy-mvp (23/23 mutations), task 09-04-query-ownership-modes (29/29 mutations), task 09-06-no-phantom-migration, task 09-07-v0-3-grammar-rewrite (mode axis deleted), and task 09-07-v0-4-leading-policy-segment (10/10 mutations; policy moved from the scheme segment to the leading `/+POLICY` segment, scheme returned to pure target).

---

## 1. Scope / Trigger

Any change to request parsing, policy-carrier resolution, retry policy resolution, response headers, or budget handling in reproxy touches this contract. The proxy is **general-purpose**: defaults and ranges must never be justified by an example use case (e.g. LLM APIs).

## 2. Signatures

```go
// target.go — path grammar parses the destination AND the leading-segment
// policy (eagerly validated: every path-level 400 fires before any body/SSRF work)
func ParsePath(escapedPath string) (PathTarget, url.Values, *RequestError)
// PathTarget stays a pure destination record (== comparable); the second
// return is the parsed leading-segment policy (nil = no /+POLICY segment —
// the only no-policy spelling from the path).

// header.go — the shared pair grammar and its two carriers
func ParseRetryPolicyHeader(h http.Header) (url.Values, *RequestError) // nil = header absent
func parsePolicyPairList(value string, c policyCarrier) (url.Values, *RequestError) // shared pure transform
func validateReproxyNamespace(h http.Header) *RequestError // runs on EVERY request
func isReproxyHeader(name string) bool     // strip predicate in buildOutboundHeaders

// policy.go — three-tier resolution, fail-closed
func Parse(params url.Values, cfg *ServerConfig) (Policy, *RequestError)

// policy.go — D13 single-attempt literal for the policy-less path
func SingleAttemptPolicy(cfg *ServerConfig) Policy

// backoff.go — per-retry wait computation
func ComputeWait(sp ScopePolicy, attemptNo int, retryAfterHeader string, now time.Time, budgetRemaining time.Duration) time.Duration
```

## 3. Contracts

### Request: path grammar

```
PROXY-TARGET   := [ "/" "+" POLICY ] "/" SCHEME "/" AUTHORITY [ "/" RAW-PATH ]
SCHEME         := "http" | "https"   // case-insensitive, lowercased — PURE TARGET
POLICY         := pair (";" pair)*   // the shared pair grammar, ORIGINAL bytes, never lowercased
```

- Plain form (`/https/host`): no policy from the path. Single attempt **unless the header channel supplies a policy**.
- Leading `/+POLICY` form: the control segment is dispatched by a single byte check at position 1 (original escaped bytes — `%2B` is not `+`), its body parsed with the shared pair grammar, then the scheme segment follows as a pure target.
- Ladder precedence (inherited): scheme errors name the scheme BEFORE policy errors name the field — a second `+`-shaped segment (`/+a/+/https/h`) dies as `unsupported scheme "+"`, and the v0.3 weld (`/ftp+status=5xx/host`) dies as an unsupported scheme, not a policy 400.
- `%2B` is not `+`: `/%2Bstatus=5xx/https/host` is a 400 as the literal segment `%2Bstatus=5xx` (original-bytes-first; `ParsePath` receives `r.URL.EscapedPath()` — never "simplify" to `r.URL.Path`).
- The scheme part is case-insensitive (lowercased); the policy body is case-sensitive, matched on original escaped bytes.
- `+` is control ONLY at segment position 1. In the target path it is data: `/https/host/+x` forwards `/+x` to the upstream verbatim.
- Invalid forms → 400 naming the raw quoted segment; `usageHint` names both shapes (`[/+POLICY]/SCHEME/AUTHORITY[/PATH]`).
- **SECOND-SYSTEM GUARDRAIL** (doc comment at the parse site): the leading `+` segment speaks ONLY retry policy; the scheme segment is pure target. v0.3 welded policy onto the scheme token; that grammar died in v0.4 as an unsupported scheme. No transport selectors, no feature flags, no additional namespaces may live in the `+` slot. A future extension belongs in a new header or surface, not this slot.

### Request: the query is unconditionally upstream-owned

- `RawQuery` forwards **byte-identical on every path** — plain, segment-policy, header-policy. There is no split, no strip, no reserved `retry.*` namespace (deleted in v0.3.0).
- `retry.*`-shaped keys are target data: `?retry.count=5` reaches the upstream verbatim even on a policy path. Signed-URL byte preservation is structurally true, not merely test-guaranteed.

### Request: the two policy carriers (one grammar)

```
segment: /+status=5xx;attempts=3;429.attempts=5/https/host/path?query
header:  X-Reproxy-Retry-Policy: status=5xx; attempts=3; [429].attempts=5
```

- Both carriers feed the **shared pair grammar** (`parsePolicyPairList`): `key=value` pairs, `;`-separated, whitespace-trimmed (around pairs **and around the `=`**), values never contain `;` or `=`, never URL-decoded. The transform is pure into `retry.`-prefixed `url.Values` consumed by the same `Parse()` — identical validation matrix and 400 bodies, zero new field rules.
- **Status-code scope spelling is the only carrier difference**: the segment writes `NNN.FIELD` (brackets are gen-delims, illegal in path segments); the header writes `[NNN].FIELD`. The dotted↔bracketed mapping is a total bijection on those scopes, round-trip locked by tests. The global scope has no spelling of its own in either carrier: a bare field IS global (`attempts=3`); the `*.` / `[*].` spellings are dead (400, v0.5.0).
- Header mapper is total (unknown keys defer to `Parse`, preserving header error bodies); the segment mapper validates key shape **eagerly** — the bare-word rule below.
- **Bare-word rule (no legacy detection)**: the KEY is validated BEFORE the `=`-presence check, so `/+retry/https/host`, `/+pure/https/host`, and `/+foo/https/host` all die identically as the generic `unknown policy field` 400. One code path, no mode-word list, no historical branch, no old/legacy/v0.2 in error text. A *recognized* key missing its `=` still hits the fail-closed ladder.
- Header: one occurrence only (multiple → 400). `X-Reproxy-*` is reserved on every request: unknown member → 400; all members stripped before forwarding upstream.

### Carrier matrix (the behavioral spec)

| Path | Header | Behavior |
|---|---|---|
| plain | no | Pure pass-through: query verbatim, body streams (no capture), single attempt (literal, D13), no X-Retry-* |
| `/+POLICY` | no | Retry per segment policy; query verbatim; capture runs |
| plain | yes | Retry per header policy; query verbatim; capture runs |
| `/+POLICY` | yes | **400 conflict** — names both channels (leading policy segment), remedy, middleware actor hint |

Invariant: the two carriers are mutually exclusive — fail-closed 400, never silent precedence.

### Policy: three-tier field-level resolution

```
built-in defaults -> retry[*].FIELD -> retry[NNN].FIELD
```

- Gates live only at `[*]` level: `retry.status` (comma list: 3-digit codes 100–599, closed ranges `500-599`, class `4xx`/`5XX` case-insensitive), `retry.network` (`0`/`1` only — valueless is a 400, fail closed), `retry.budget` (duration with unit, > 0).
- Scope fields: `attempts` (TOTAL incl. first; 1 = no retry), `backoff` (`constant|linear|exponential`), `initial`, `max`, `jitter` (`none|full|equal`), `retry_after` (`honor|ignore`).
- `attempts=1` is legal (means no retry). Dead config — `retry[NNN]` scope whose code is not in `retry.status` — is a 400.
- Server clamps (`MaxAttempts`, `MaxBudget`) narrow and never widen.

### Response headers / observability

`X-Retry-Count`, `X-Retry-Limit`, `X-Retry-Exhausted` emitted **only when a retry lifecycle exists**: a policy is present (either carrier). The policy-less path emits none — including on the 504 network-failure path (`exhausted()` gates on the same lifecycle flag). `X-Retry-Dropped` when an oversized body is degraded to streaming pass-through (only where capture ran). Per-retry log lines carry the computed wait (`event=wait backoff=...`). The `request` log line carries `policy=segment|header|none` (the mode concept was deleted in v0.3.0).

### Body capture invariant (D13/D14)

- `Capture()` exists solely to replay across attempts. `no policy ⇒ no Capture` — the body streams to the upstream with **inbound framing preserved** (`req.ContentLength = r.ContentLength`, never unconditionally `-1`/chunked).
- The policy-less policy is `SingleAttemptPolicy(cfg)` — a literal, never `Parse(url.Values{})`: empty input returns the v0.1.0 defaults (3 attempts, network=1) and would smuggle a retry lifecycle into the policy-less path. The Budget field stays live (feeds per-try TTFB, clamped by `MaxBudget`).

### Budget semantics

`retry.budget` is a hard cap over attempts + waits, checked at the loop top (first attempt exempt). Exhaustion commits the held upstream response (delivered as-is, `X-Retry-Exhausted: true`); it never fabricates a response.

## 4. Validation & Error Matrix

| Condition | Result |
|---|---|
| Unknown policy field (bare word in segment: `retry`, `pure`, `foo`; malformed dotted scope) | 400, generic `unknown policy field` naming the key — identical for old mode words and any junk |
| Unknown scheme-segment form (v0.3 weld `https+status=5xx`, `https+retry`; `%2B` forms) | 400 naming the raw quoted segment, hint names both shapes |
| `/+statusx=5xx/https/h` (unknown field in the control segment) | 400, generic `unknown policy field` naming the key (shared ladder) |
| `/+/https/h`, `/+` (empty control segment) | 400 naming the policy grammar — degenerate; absent is the only no-policy spelling |
| `/+status=5xx` (no scheme follows the control segment) | 400 missing upstream scheme |
| `/+a/+/https/h` (second `+`-shaped segment) | 400 `unsupported scheme "+"` — the ladder parses it as a scheme |
| `/https/host/+x` (`+` in the target path) | Not an error — forwarded byte-for-byte (`+` is control only at segment position 1) |
| Segment policy + `X-Reproxy-Retry-Policy` both present | 400 conflict — names both channels, remedy (drop one), middleware actor hint |
| Unknown `X-Reproxy-Foo` header | 400 (reserved namespace, every request) |
| Multiple `X-Reproxy-Retry-Policy` occurrences | 400 |
| Degenerate policy (empty / `;;` / whitespace-only / missing `=` / empty key) | 400 on BOTH carriers — absent is the only no-policy spelling |
| Whitespace inside a pair around `=` (`status = 429`) | Accepted (trimmed, pinned by test rows) |
| `attempts < 1` / non-integer | 400 |
| `max < initial` (in a resolved scope) | 400 — checked AFTER scope assembly, never per-field (order-independent) |
| `retry[NNN]` code not in `retry.status` | 400 (dead config) |
| Gate key inside a scope (`[429].status`) | 400 — gates are global-only |
| Reversed/open range, empty list item, out-of-range code | 400 |
| `retry.network` / `retry.budget` invalid or valueless | 400 (fail closed) |
| All 400 bodies | JSON `{error, hint}` |

## 5. Good/Base/Bad Cases

- Good: `/+status=5xx;attempts=4;429.attempts=2/https/host/x` — 429 uses attempts=2, everything else in the gate uses 4.
- Good: `/+status=5xx/https/host/x?retry.count=5` — headline property: `retry.count` is target data, reaches the upstream verbatim, policy drives retries.
- Good: `/https/host` + `X-Reproxy-Retry-Policy: status=5xx; attempts=3` — header policy drives retries, query untouched.
- Good: `/https/host/+x` — the `+` is target data, forwarded verbatim.
- Base: `/https/host/x?y=1` — pure reverse proxy, single attempt, zero behavioral overhead.
- Bad: `/+status=429;500.attempts=2/https/host` — 400 dead config (500 not in gate).
- Bad: `/+status=5xx/https/host` + policy header — 400 both-carriers conflict.
- Bad: `/+retry/https/host` — the generic unknown-field 400, same as `+foo` (no legacy branch exists).
- Bad: `/https+status=5xx/host` — the v0.3 death shape: 400 `unsupported scheme "https+status=5xx"` quoting the raw segment.
- Bad: `/+a/+/https/host` — 400 `unsupported scheme "+"` (one control segment only).

## 6. Tests Required

- Byte preservation: `TestProxyQueryBytePreservation`, `TestE2EQueryBytePreservation`, `TestProxySegmentPolicyQueryIsTargetData` (query verbatim INCLUDING on policy paths — the v0.3.0 headline).
- Path grammar: `target_test.go` grammar table (plain / `/+POLICY` forms × schemes × case variants; `%2B` rows; whitespace-around-`=` rows; v0.3 death-shape rows; `/+`-in-target-path rows).
- Carrier matrix: `TestProxyPolicyChannelConflictMatrix` (every §3 row), `TestE2EPolicyChannelConflictOverTCP`.
- One-grammar lock: `TestParsePathSegmentHeaderGrammarEquivalence` (segment vs header → deep-equal Policy), `TestSegmentBracketBijection` (dotted↔bracketed round trip), `TestHeaderTransformEquivalence`.
- Policy-less path: `TestProxyPlainQueryVerbatimAndSingleCall`, `TestProxyPlainBodyStreamsNoCapture` (incl. ContentLength framing assertion).
- Log line: `TestProxyRequestLogCarriesPolicySource` asserting `policy=segment|header|none`.
- Order independence: `TestParseFieldOrderIndependence` (50 iterations — map iteration order must not change the verdict).
- Dead config: `TestParseDeadConfig` + `TestProxyDeadConfig400`.
- Mutation lock: every row of the validation matrix has a test that fails when the behavior is broken (see mutation-scan.md in the task dir — m1–m10 killed, including the new ladder rows).

## 7. Wrong vs Correct

### Wrong
```go
// Validating max >= initial inside the per-field switch:
if sp.Max < sp.Initial { return bad(...) }
// map iteration order decides which field lands last -> legal configs randomly 400
```

```go
// The policy-less path reusing Parse for "defaults":
policy = Parse(url.Values{}, cfg)
// Parse(∅) returns 3 attempts + network=1 — a retry lifecycle smuggled into
// the policy-less path
```

```go
// Special-casing old mode words (v0.4.0 has NO legacy detection):
if policyField == "retry" || policyField == "pure" {
    return bad("legacy mode word removed in v0.3.0 ...")
}
// The grammar is forward-looking only; +retry dies as the SAME generic
// unknown-policy-field 400 as +foo — no word list, no history in errors
```

```go
// Restoring a "+" cut inside the scheme segment (the v0.3 weld):
schemePart, policyPart, _ := strings.Cut(segment, "+")
// v0.4's scheme segment is a pure target; the weld died as an unsupported
// scheme. A "+" in the segment is an error, never a policy carrier.
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

```go
// One grammar, two carriers — the pair loop validates the key BEFORE the
// "=" check, so every bare word (retry, pure, foo) takes the same path:
key, val, found := strings.Cut(pair, "=")
if !found {
    if _, kerr := c.queryKeyFor(strings.TrimSpace(pair)); kerr != nil {
        return nil, kerr // generic unknown-policy-field 400
    }
    ...
}
```

```go
// Leading control segment: one byte check at position 1, original bytes:
if strings.HasPrefix(rest, "+") {
    policySeg, rest = /* body up to next "/" */
    params, rerr := parsePolicyPairList(policySeg, segmentCarrier)
    ...
}
// Then the scheme segment — pure target, strict {http, https} table:
scheme, rerr := parseSchemeSegment(schemeSeg)
```
