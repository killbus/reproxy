# Implementation report: 09-04-query-ownership-modes

## Batch 1 — scheme-segment grammar (target.go)

Date: 2026-09-06. Scope: implement.md "Batch 1 — scheme-segment grammar" only.

### 1. What changed per file

**`target.go`**

- `PathTarget` gained two fields:
  - `Mode string` — `"retry"` | `"pure"` (constants `ModeRetry` / `ModePure`), the query-ownership mode selected by the scheme segment.
  - `ExplicitMode bool` — reports whether the segment carried an explicit `+MODE` suffix vs. the plain form. **Added one notch beyond the Batch 1 checklist** (which named only `Mode`): design §5's deprecation gate and the v0.3 flip both key off "plain vs. explicit" (`plain` selects retry mode *transitionally*; `+retry` selects it *forever*). Batch 3's `event=deprecation` logic needs this bit — with only `Mode` it cannot distinguish `/https/h` (v0.2: retry + deprecation log) from `/https+retry/h` (retry, never logs). See §3 (deviation D-2) for the reasoning.
- New `parseSchemeSegment(segment string) (scheme, mode string, explicitMode bool, rerr *RequestError)`: strict table `{plain, +retry, +pure} × {http, https}`, whole segment lowercased before the `strings.Cut(lower, "+")` split (so SCHEME and MODE case-normalize together: `HTTPS+PURE` ≡ `https+pure`). The plain form resolves to `ModeRetry` with a `// TODO(v0.3): flip ... to ModePure per the R5 migration (design D15)` marker.
- `ParsePath` grammar comment updated to `SCHEME-SEGMENT := SCHEME [ "+" MODE ]`, including the escaped-path rule doc (`%2B` is not `+`). Returns the new fields.
- `usageHint` now names all three accepted shapes: `/https/...`, `/https+retry/...`, `/https+pure/...`.
- `HostPort()` / `URL()` / `Normalize()` / `parseAuthority` / all host/port validation: **untouched** (mode-agnostic per design §1).
- **`proxy.go`: zero changes.** ParsePath's signature is unchanged (`(string) (PathTarget, *RequestError)`); the call site at proxy.go:151 already passes `r.URL.EscapedPath()`.

**`target_test.go`**

- All `PathTarget{...}` positional literals extended with `Mode, ExplicitMode` (plain forms assert `ModeRetry, false`, pinning the transitional default).
- New grammar-table section (~23 rows): all valid forms (`+retry`/`+pure` × `http`/`https`), case variants (`HTTPS+RETRY`, `Https+Pure`, `HTTP+Retry`), mode-scheme-only (missing authority), IPv6 + ports per mode, raw-path byte preservation per mode, `+` inside the raw path is data not syntax (`/https/h/a+b+c`), empty-path normalization per mode.
- Invalid table: `+retrt` typo (asserts the full quoted segment `unsupported scheme "https+retrt"`), `+` trailing (`"https+"`), `+retry` with no scheme (`"+retry"`), bare `+` (`"+"`), `!retry` separator, `rx`, `+pure+retry` double suffix, `ftp+retry` (unknown scheme with mode suffix), **`https%2Bpure` and `https%2bpure` (escaped-plus, both hex cases, must 400 as the literal segment — original-bytes-first)**.
- `TestPathTargetNormalizeAndRendering` gained two mode rows asserting `HostPort`/`URL`/`Normalize` behave identically for `+pure`/`+retry` targets (mode is orthogonal to destination rendering).

### 2. Grammar table implemented

```
SCHEME-SEGMENT := SCHEME [ "+" MODE ]
SCHEME := "http" | "https"     (case-insensitive)
MODE   := "retry" | "pure"     (case-insensitive, lowercased with the scheme)
```

Valid (→ `{Scheme, Mode, ExplicitMode}`):

| Input segment | Scheme | Mode | ExplicitMode |
|---|---|---|---|
| `https`, `HTTPS`, `Http` | http(s) | retry (transitional) | false |
| `https+retry`, `HTTP+Retry`, `HTTPS+RETRY` | http(s) | retry | true |
| `https+pure`, `Https+Pure` | http(s) | pure | true |

Invalid (400, reason `unsupported scheme "<raw segment>": only http and https are supported, optionally with a +retry or +pure mode suffix`, hint = usageHint with all three shapes):

| Input segment | Why |
|---|---|
| `https+retrt` | typo'd mode |
| `https+` | empty mode |
| `+retry`, `+` | mode without scheme |
| `https!retry` | wrong separator |
| `rx` | unknown scheme |
| `https+pure+retry` | double suffix (`strings.Cut` splits at first `+`; remainder `pure+retry` ≠ any mode) |
| `ftp+retry` | unknown scheme with mode |
| `https%2Bpure`, `https%2bpure` | `%2B` is not `+` — segment matched on original escaped bytes |

Authority/port/host/IPv6/raw-path validation is unchanged and orthogonal to mode (covered per-mode in the new rows).

### 3. Design deviations

1. **None on grammar, error text, or TODO marker** — implemented exactly per design §1 / implement.md Batch 1.
2. **`ExplicitMode` field added** (beyond the checklist's `Mode` alone). Reason: design §4 step 2 and §5 distinguish plain (`+retry` transitional, deprecation-logged) from explicit `+retry` (stable, never logged); Batch 3 cannot reconstruct this from `Mode` alone after the v0.3 flip makes plain resolve to `pure`. Costs one bool; keeps Batch 3 free of re-parsing.
3. **Error text for a bare/leading `+` segment**: the earlier draft message ("unsupported mode suffix ... on scheme ...") was replaced by the single design-literal message `unsupported scheme "<segment>"` naming the whole raw segment — design §1's rationale explicitly pins `unsupported scheme https+retrt` as the typo shape, and one message shape keeps the "first segment speaks scheme" invariant. No test or spec pinned the old wording.
4. **No EscapedPath switch was needed** — see §5.

### 4. Validation

```
go build ./...   ok
go vet ./...     ok
gofmt -l .       (empty after one gofmt -w on target.go — comment alignment)
go test ./... -count=1   ok  reproxy 4.5s   (run 3x consecutively, all green)
```

Suite: green ×3 consecutive. No existing test required modification other than the mechanical `Mode`/`ExplicitMode` literal extension (R3 AC: v0.1.0 rows stay, plain pinned to transitional retry).

### 5. Notes for Batch 3

- **EscapedPath already holds.** proxy.go:151 passes `r.URL.EscapedPath()` (since v0.1.0), and Go's `EscapedPath()` preserves `%2B` verbatim (verified empirically on go1.25.1: `/https%2Bpure/host/x` → `EscapedPath() == "/https%2Bpure/host/x"` while `Path` decodes to `/https+pure/...`). The original-bytes-first rule therefore requires **no call-site change** — the mutation target `scheme-%2B-decoded-to-mode` is already locked by the two escaped-plus test rows. Do not "simplify" any future call site to `r.URL.Path`; that would silently decode `%2B` into mode syntax.
- **Mode resolution order**: `ParsePath` returns the resolved mode already; Batch 3's ServeHTTP branch is `switch target.Mode` plus the deprecation gate keyed on `!target.ExplicitMode` (plain form). The v0.3 flip is a one-line change inside `parseSchemeSegment` (the TODO marker), not a call-site change.
- **Deprecation gate needs `hasRetryKeys(rawQuery)`**: Batch 3 adds that helper per implement.md; note `SplitQuery` currently 400s on unknown retry keys, so the light key-scan must not reuse it (design/implement.md Batch 3 already says this).
- **`buildOutboundHeaders` / header channel**: untouched by Batch 1 by design (Batch 2).
- **proxy_test.go `do()` helper**: unchanged; mode-qualified paths can be passed straight through it (`httptest.NewRequest` keeps the escaped path via `req.URL.EscapedPath()`).

Mutation targets now locked by Batch 1 tests (for the Batch 4 scan): `plain-resolves-wrong-mode` (every plain row asserts `ModeRetry`), `mode-suffix-casesensitivity` (three case-variant rows), `scheme-%2B-decoded-to-mode` (two escaped rows), `unsupported-scheme-names-segment` (quoted-segment assertions on 8 invalid rows).

---

## Batch 2 — header channel (header.go, new)

Date: 2026-09-06. Scope: implement.md "Batch 2 — header channel" only. No proxy.go pipeline wiring (Batch 3); no README/spec (Batch 4).

### 1. What changed per file

**`header.go` (new)**

- `RetryPolicyHeader = "X-Reproxy-Retry-Policy"` and `reproxyHeaderPrefix = "X-Reproxy-"` constants.
- `ParseRetryPolicyHeader(h http.Header) (url.Values, *RequestError)` — the public parse. Runs the namespace guard first, collects the policy header's values across every spelling (`http.CanonicalHeaderKey` comparison, so hand-built non-canonical maps behave like the wire), enforces single occurrence, delegates one value to the pair parser. Absent header → `(nil, nil)` — the ONLY no-policy spelling.
- `parseRetryPolicyHeaderValue(value string)` — pair grammar: outer whitespace trimmed; empty/whitespace-only → 400; `;`-split with per-pair `TrimSpace`; empty pair → 400; `strings.Cut` on first `=` (missing `=` → 400, empty key → 400, value containing `=` → 400 — a second `=` can only be a paste error since no legitimate value needs it); each key prefixed via `queryKeyForHeaderKey` and `params.Add`ed. Zero key/value validation beyond pair shape — everything else is Parse's job.
- `queryKeyForHeaderKey(key string) string` — the transform: keys beginning `[` (scope keys) get `"retry"` prepended (→ `retry[*].attempts`, NOT `retry.[*].attempts`); everything else gets `"retry."` (→ `retry.status`). This matches SplitQuery's canonical key spellings exactly — the check the coordinator's brief called out ("whether `[*].attempts` becomes `retry.[*].attempts`"): it becomes `retry[*].attempts`, and `TestHeaderScopeKeysNeverDoubleDot` locks it.
- `validateReproxyNamespace(h http.Header) *RequestError` — the reserved-namespace guard, exported-adjacent (package-private) and standalone so Batch 3 can call it on the query-channel path (where the policy header is a conflict rather than a parse target). Any `X-Reproxy-*` member other than the policy header → 400 naming the header and the remedy.
- `isReproxyHeader(name string) bool` — canonicalized prefix check, used by `buildOutboundHeaders` for the strip.

**`header_test.go` (new)** — 12 tests: pair grammar table (10 rows incl. the design §2 example verbatim, whitespace-around-pairs, no-whitespace, comma values); scope-keys-never-double-dot; absent-is-only-no-policy (nil/empty/unrelated/lookalike `X-Reproxyx-Other`); degenerate-input table (12 rows); multiple occurrences (canonical + non-canonical spellings); unknown-namespace (incl. non-namespace lookalike must NOT 400); legitimate-values-representable (asserts every legal value in the full field grammar contains neither `;` nor `=` — the design's unambiguity claim, checked rather than trusted); transform equivalence (same policy via query vs header → deep-equal `Policy` AND deep-equal `url.Values`); Parse's 400 matrix through the header path (15 rows); parse round-trip on resolved fields; empty-vs-absent contract; non-canonical spelling accepted; `buildOutboundHeaders` strip (canonical + non-canonical, X-Keep preserved); `isReproxyHeader` table; `validateReproxyNamespace` direct.

**`proxy.go`** — one mechanical edit: `buildOutboundHeaders` skips `isReproxyHeader(name)` alongside the hop-by-hop check, plus its doc comment. No other line touched (verified via `git diff proxy.go`: exactly the 4-line hunk).

**`query.go`, `target.go`** — zero diff.

### 2. Header grammar + degenerate table

```
VALUE     := PAIR { ";" PAIR }
PAIR      := KEY "=" VAL          (whitespace around a pair is trimmed)
KEY       := query-channel key minus the "retry." prefix:
             "status" | "network" | "budget" | "[*].FIELD" | "[NNN].FIELD"
VAL       := any bytes without ";" or "=" (never URL-decoded)
Transform : KEY → query spelling ("[*].x" → "retry[*].x", "status" → "retry.status"),
           then policy.Parse validates everything.
```

Degenerate/shape table (all 400; absent is the only no-policy spelling):

| Input | Reason substring |
|---|---|
| Header absent | — (nil, nil), never 400 |
| Present but empty `""` | `is present but empty` |
| Whitespace-only `"   "` / `" \t "` | `is present but empty` |
| Empty pair `status=5xx;;network=1` | `empty pair` |
| Trailing `;` / leading `;` / `; ;` | `empty pair` |
| Pair without `=` (`status 5xx`) | `not key=value` |
| Empty key (`=5xx`) | `empty key` |
| Value containing `=` (`status=429=500`) | `contains "="` |
| 2+ occurrences (any spelling) | `multiple X-Reproxy-Retry-Policy headers` |
| Any other `X-Reproxy-*` header | `unknown header ... namespace is reserved` |

Parse's matrix then fires identically through the header path (15-row test): unknown gate/field, gate-inside-scope, attempts=0/abc, bad backoff/duration/budget/network/status-list, empty status item, dead config, max<initial, bad scope digits.

### 3. Design deviations

1. **None on the grammar, normalization ladder, transform, or strip.** Implemented exactly per design §2 / implement.md Batch 2.
2. **Two pair-shape 400s beyond the design's literal list** (empty key; value containing `=`): both are strictly within "degenerate input → 400" (the design's own principle — a present-but-empty value must not be read as a policy; `=5xx` and `status=429=500` are the same class of corruption). The `=`-in-value check additionally enforces the design's "values may not contain `;` or `=`" line as a parse rule rather than a hope; `TestHeaderLegitimateValuesRepresentable` proves no legitimate value is excluded.
3. **Values are never URL-decoded** (unlike SplitQuery, which decodes retry-namespace values). Header values are plain text; `%xx` in a header is data. Documented in the function comment. If a future need arises it is a one-line addition, not a redesign.
4. **The namespace guard is a standalone helper (`validateReproxyNamespace`), not folded into `ParseRetryPolicyHeader` only** — the brief offered the choice; standalone wins because Batch 3 must run it on the query-channel path too (retry mode + header = conflict, and that path never parses the policy header). `ParseRetryPolicyHeader` calls it internally, so pure-mode wiring gets it for free either way.

### 4. Validation

```
go build ./...   ok
go vet ./...     ok
gofmt -l .       (empty)
go test ./... -count=1   ok ×3 consecutive (7.4s / 14.3s / 7.0s)
```

Notes from the runs: two of my own first-draft test bugs were fixed during development (an equivalence input that was dead config by accident; a wrong expected-message row). One pre-existing test (`TestProxyPerStatusScopeShaping`) failed once under full-suite load and passes consistently in isolation and in the final ×3 runs — the documented Windows timing-flake class (quality-guidelines rule 3: 500ms upper bound vs 4–68ms host variance), not a regression from this batch; no change made to it.

### 5. Notes for Batch 3

**Signatures to call:**

```go
// The header channel (pure mode + header, or any mode where the header is parsed):
params, rerr := ParseRetryPolicyHeader(r.Header)   // url.Values | nil, *RequestError | nil
if rerr != nil { rerr.Write(w); return }
// params == nil  ⇔  header absent  ⇔  NO policy was requested.
// params != nil  ⇒  run Parse(params, p.Config) — never Parse on nil-params
//                   without the absent check first (see below).

// The namespace guard, for paths that do NOT parse the policy header
// (retry mode, where the header's presence is a conflict instead):
if rerr := validateReproxyNamespace(r.Header); rerr != nil { rerr.Write(w); return }

// Detecting the header's presence for the conflict check (cheap, no parse):
// len(r.Header.Values(RetryPolicyHeader)) > 0  — canonical spellings come free
// from the net/http parser; if you also want hand-built-map tolerance, mirror
// ParseRetryPolicyHeader's CanonicalHeaderKey collection loop.
```

**Key behaviors Batch 3 must know:**

- **`Parse(url.Values{})` returns the v0.1.0 defaults** (3 attempts, network=1, 30s budget) — it does NOT mean "off". This is why D13's single-attempt literal exists: in pure mode with an absent header, construct the single-attempt `Policy` value directly, never `Parse(params)` when `params` is nil-or-empty. `ParseRetryPolicyHeader` returns `nil` (not `url.Values{}`) for an absent header precisely so the nil check is the discriminator. `TestHeaderParseEmptyVsAbsent` pins all of this.
- **A degenerate present header never reaches Parse** — the 400 fires at the header layer. So the pure-mode wiring is: absent → single-attempt literal; present-and-parseable → `Parse(transformed)`; present-but-degenerate/malformed → the header-layer 400 (already written by `rerr.Write`).
- **Retry-mode + any `X-Reproxy-Retry-Policy` occurrence → the §3 conflict 400** ("both channels used"). Write that error in proxy.go (Batch 3 owns it); the namespace guard still applies first so unknown members 400 in every mode.
- **The strip is already live**: `buildOutboundHeaders` now drops all `X-Reproxy-*` (canonical or not), so no wiring is needed for "upstream never sees the control plane" — the e2e assertion (design §6) can be written immediately against current code.
- **Per-try TTFB in pure mode** (design §4): unaffected by this batch; `roundTripTimeout` is policy-independent.
- **Mutation targets this batch locks** (for the Batch 4 scan): `header-transform-drops-field` (equivalence test), `empty-header-treated-as-absent` (degenerate table — flip the empty-value 400 to a nil return and it goes red), `unknown-X-Reproxy-passes` (namespace table), `header-not-stripped` (both strip tests), `header-single-occurrence` (multiple-occurrence tests), `scope-key-double-dot` (never-double-dot test).


---

## Batch 3 — pipeline branches (proxy.go)

Date: 2026-09-06. Scope: implement.md "Batch 3 — pipeline branches" only. No README/spec/mutation-scan (Batch 4). Existing tests: unmodified (verified via git diff — only new test functions appended; the only edits to shared code are the two import lines log/net-url in proxy_test.go, added because the new tests use them).

### 1. What changed per file

**proxy.go** — the only behavior-touching changes:

- `ServeHTTP` restructured per design §4. After target parsing it resolves mode + channel in one `switch target.Mode`:
  - **+pure**: `upstreamQuery = r.URL.RawQuery` verbatim (SplitQuery never called), policy from `ParseRetryPolicyHeader` -> `Parse` (header channel), or the D13 single-attempt literal when the header is absent (`params == nil`). `retryLifecycle` is true only with a header policy.
  - **+retry and plain (transitional)**: namespace guard (`validateReproxyNamespace`) first, then the conflict gate (`policyHeaderPresent` -> 400 naming both channels, the +pure remedy, and the actor-source note), then the plain-form deprecation arming, then the untouched v0.1.0 SplitQuery -> Parse path. `retryLifecycle` always true.
- Body capture moved under `if retryLifecycle` (D14): retry mode and +pure+header capture as before (cap/413/degraded machinery intact); headerless pure mode captures nothing.
- `attemptLoop`, `commitResponse`, `exhausted` gained a `retryLifecycle bool` parameter gating all X-Retry-* response headers. `attemptLoop` also gained `onNetworkRetry func()`, invoked in the network-retry branch after the gate check passes — the deprecation implicit trigger.
- `newDeprecationLogger(p, target)` returns a once-per-request `func(trigger string)`: fires `event=deprecation` with `target=`, `trigger=` ("retry-keys" | "network-retry"), and a `note=` carrying the migration call (naming all three escape forms). No cross-request state.
- `policyHeaderPresent(h)` — spelling-tolerant presence check (canonical + hand-built maps) for the conflict gate.
- `logRequest` gains `mode=` (from `target.Mode`).
- `roundTrip`/`attemptLoop` body plumbing made nil-capture-safe: `captured == nil && r.Body != nil` forwards the request's own body stream once with the inbound framing preserved (`req.ContentLength = r.ContentLength` — Content-Length stays Content-Length, chunked stays chunked). The old code dereferenced `captured` unconditionally (nil-deref — found by the first pure-mode test run).

**query.go** — `hasRetryKeys(rawQuery) bool`: inert presence scan over &-segments using the existing `inRetryNamespace`. Never validates, never 400s, never decodes (unknown/malformed retry keys still count as present — SplitQuery produces the precise 400 later).

**policy.go** — `SingleAttemptPolicy(cfg *ServerConfig) Policy`: the D13 literal. Attempts=1, NetworkGate=false, StatusGate empty, ByStatus empty. Budget stays live (30s default, narrowed by `cfg.MaxBudget`) because it feeds `roundTripTimeout` — per-try TTFB still applies in pure mode (it bounds a hang, not a retry). Shape fields carry inert defaults (no wait is ever computed with one attempt). Doc comment pins why it must never be `Parse(url.Values{})`.

**proxy_test.go** (+~520 lines, 17 new test functions; log/net-url imports added): `testLogBuffer`/`captureLogs`/`logBuf` helpers; pure-mode query byte-identity incl. retry.* target data; pure single-call + no X-Retry-*; pure no status-gate; pure network failure -> 504 without retry (the v0.1.0 observable difference, R5's reason); pure body streams no capture (strict cap 8, 1000-byte body, still 200); the full §3 conflict matrix; degenerate header 400 in retry mode; unknown X-Reproxy-* 400 in both modes; +pure+header retries (2 calls, X-Retry-Count: 2, query untouched); D13 literal not Parse-defaults; +pure+header capture revival (degraded X-Retry-Dropped + strict 413); +pure+header body replay x3; pure SSRF still 403; `mode=` on event=request for all three forms; deprecation gate (fires on retry-keys, silent without, fires on network-retry-consumed with zero retry keys, never fires on +retry/+pure); dedupe one line with both triggers; plain-form unknown retry key still 400; `TestHasRetryKeys` (14-row table incl. bare "retry" not in namespace); `TestSingleAttemptPolicyLiteral` (incl. explicit discriminator against Parse(empty) and the MaxBudget clamp).

**e2e_test.go** (+~280 lines, 8 new test functions): retry.count/retry.token/retry.status collision case reaches upstream verbatim in pure mode; +retry URL channel still works; +pure+header over real TCP (policy from header, query untouched, retry observable, X-Retry-Count: 2); X-Reproxy-* stripped upstream on the full path (+pure+header consumption, lookalike outside namespace passes); pure large body no-cap even strict (500-byte body over 16-byte cap, byte-identical); +pure+header POST replay e2e; conflict 400 over TCP on both +retry and plain forms; pure SSRF 403.

### 2. Mode x channel matrix as implemented (design §3)

| Path mode | Header present? | Behavior | Test anchor |
|---|---|---|---|
| +pure | no | Query verbatim, body streams, single attempt, no X-Retry-* | TestProxyPureModeQueryVerbatimAndSingleCall, TestE2EPureModeRetryDotParamsReachUpstream |
| +pure | yes | Policy from header, query verbatim, capture revived, X-Retry-* emitted | TestProxyPureModeHeaderPolicyRetries, TestE2EPureModeHeaderPolicy |
| +retry | no | v0.1.0 semantics exactly | TestProxyModeChannelConflictMatrix row 3, TestE2EPureModeRetryURLStillWorks |
| +retry | yes | 400 conflict (both channels; remedy names +pure + actor source) | TestProxyModeChannelConflictMatrix row 4, TestE2EModeChannelConflictOverTCP |
| plain (v0.2) | no | Retry mode + deprecation log when retry keys present OR network retry consumed | TestProxyDeprecationLogGate, TestProxyRequestLogCarriesMode |
| plain (v0.2) | yes | 400 — same conflict rule | TestProxyModeChannelConflictMatrix row 6 |
| plain (v0.3) | — | NOT implemented here (terminal state; the v0.3 flip is a separate task per design §7) | — |

### 3. Deviations with reasons

1. **SingleAttemptPolicy takes `cfg *ServerConfig`** (the brief said "construct the Policy directly"; a signature wasn't pinned). Reason: the policy's Budget field feeds `roundTripTimeout` (per-try TTFB), which still applies in pure mode — a literal with Budget 0 would make roundTripTimeout return 1ms (spent budget -> fail fast) and break every pure-mode request. The literal therefore carries the default 30s narrowed by the server's MaxBudget, mirroring Parse's clamp. TestSingleAttemptPolicyLiteral pins the clamp.
2. **Pure-mode streaming preserves inbound Content-Length framing** (`req.ContentLength = r.ContentLength` rather than unconditional -1). Unconditional chunked framing would be an observable wire difference from v0.1.0 for every bodyless or fixed-length POST in pure mode. Framing preservation is the byte-faithful reading of "the body streams to the upstream as-is".
3. **Deprecation trigger name spellings** (trigger=retry-keys / trigger=network-retry) chosen freely — design §5 pins the gate, not the token names. Logged via the standard logLine event catalog extension (event=deprecation), one line per request, no cross-request state.
4. **exhausted() (the 504 network-exhaustion path) also gates X-Retry-* on retryLifecycle.** Headerless pure mode can reach it (single network failure -> 504) and must not claim a retry lifecycle it never had. v0.1.0 behavior in retry mode is unchanged (lifecycle always true there).
5. **One test-helper edit within a test file**: TestProxyDeprecationLogGate's network-retry case originally set retry scope keys (shaping the wait), which made the retry-keys trigger fire first and shadow network-retry — fixed to a zero-retry-key request (default policy still retries network failures). This is inside a Batch-3-new test, not an existing one.

### 4. Validation x3 output summary

```
go build ./...   ok
go vet ./...     ok
gofmt -l .       (empty)
go test ./... -count=1   ok  x3 consecutive
  run 1: ok reproxy 5.073s
  run 2: ok reproxy 5.028s
  run 3: ok reproxy 5.382s
```

No TestProxyPerStatusScopeShaping failures observed in any run (the documented Windows timing-flake class). All pre-existing tests pass unmodified (R3 AC); git diff confirms only additions to test files plus the two import lines.

### 5. Notes for Batch 4 (mutation targets locked + README musts)

**Mutation targets now locked by Batch 3 tests** (beyond Batch 1/2 lists):
- pure-mode-splits-query — TestProxyPureModeQueryVerbatimAndSingleCall (query must stay verbatim incl. retry.* data)
- pure-mode-captures-body — TestProxyPureModeBodyStreamsNoCapture (1000-byte body over an 8-byte strict cap must NOT 413)
- pure-mode-uses-Parse-empty-defaults — TestProxyPureModeSingleAttemptLiteralNotParseDefaults + TestSingleAttemptPolicyLiteral
- retry-mode-ignores-header-conflict — TestProxyModeChannelConflictMatrix rows 4/6
- plain-resolves-wrong-mode (pipeline half) — TestProxyModeChannelConflictMatrix row 6 + TestProxyRequestLogCarriesMode plain row
- deprecation-log-omitted — TestProxyDeprecationLogGate first case
- deprecation-log-fires-without-retry-keys-but-network-retry — TestProxyDeprecationLogGate third case
- deprecation-dedupe — TestProxyPlainSchemeDeprecationDedupeBothTriggers
- mode-log-omitted — TestProxyRequestLogCarriesMode
- pure-mode-emits-retry-headers — the no-X-Retry-* assertions across four pure-mode tests
- network-retry-callback-never-invoked — TestProxyDeprecationLogGate third case
- pure-streaming-framing-changed — TestProxyPureModeBodyStreamsNoCapture + TestE2EPureModeLargeBodyNoCap (byte-identity)
- capture-revival-missing — TestProxyPureModeHeaderPolicyCapturesBody (degraded + strict)

**README must document (design §7 wording pins)**:
- The three-channel model with the ownership principle; "query-pure" prose naming for pure mode; `+` read as a mode modifier, never transport syntax.
- The mode table with the capture-revival note in the +pure+header row's body text (not a footnote): adding X-Reproxy-Retry-Policy to a +pure route re-enables the 10 MiB cap/413/degraded machinery.
- The conflict 400 remedy naming the actor source ("if you did not set this header, a middleware or gateway between you and reproxy may have").
- Migration: v0.2 transitional plain->retry with event=deprecation (both triggers: retry keys OR silent network-retry consumption — the implicit v0.1.0 retry.network=1 dependent); v0.3 flips plain to pure; retry%2E encoded escape-hatch footnote.
- v0.2 observable difference callout: pure mode does not retry network failures that v0.1.0's default did (covered by TestProxyPureModeNetworkFailureNotRetried).
- event=request now carries mode=; event=deprecation line shape (target=, trigger=, note=).

**Spec update feed (Batch 4)**: retry-proxy-contract.md needs the mode grammar + header channel + the D13/D14 invariants; logging-guidelines.md needs "deprecation" in the event catalog and mode= in the request row.

---

## Batch 4 — README + mutation scan

Date: 2026-09-06. Scope: implement.md "Batch 4" only — docs + adversarial verification. **Zero .go changes** (verified: `git diff` shows only README.md modified in the tree; the scan ran in an isolated copy, deleted afterward).

### 1. README changes

Single file: `README.md`. Section-by-section:

- **Intro / Quick start**: "query parameters or headers"; quick-start example now uses `/https+retry/` (the stable retry form).
- **How it works**: path grammar gains `[+<mode>]`; pin that `+` is a **mode modifier on the scheme, not a transport selector** (never phrased like `git+ssh`); raw-bytes rule extended to the scheme segment (`%2B` is not `+`).
- **New section "Query ownership and modes"** (the centerpiece):
  - Ownership principle verbatim: "The query belongs to the target; the proxy borrows it only when told to."
  - Three-channel table (pure / retry / header) with the query-ownership column.
  - Scheme-segment EBNF copied from design §1 verbatim (SCHEME-SEGMENT := SCHEME ["+" MODE], case rules, PROXY-TARGET).
  - **Pure mode semantics**: verbatim query (headline `retry.count` collision case), no retries + **the v0.2 observable-difference callout** (pure mode does not retry network failures v0.1.0's default `retry.network=1` did — R5's reason to exist), TTFB still applies, no body capture (no cap/413/degraded/X-Retry-Dropped, streams), no X-Retry-*, SSRF identical.
  - **Capture-revival note in the mode-table body, not a footnote** (D14 follow-through): `+pure`+header re-enables the full capture machinery; "Adding the header to a `+pure` route is not free."
  - **Retry mode semantics**: v0.1.0 text moved here intact, plus the `retry%2E` encoded escape-hatch footnote (works in retry mode for decoding upstreams; the structural fix is `+pure`).
  - **Header channel section**: the design §2 example verbatim (`status=5xx; network=1; budget=30s; [*].attempts=3; [429].attempts=5`), same-field-set/same-400s statement, **absent-is-the-only-no-policy-spelling rule** (degenerate input → 400 in every mode), single occurrence, reserved X-Reproxy-* namespace + strip, conflict-400 remedy **naming the actor source** ("if you did not set this header, a middleware or gateway between you and reproxy may have added it").
- **Retry parameters table**: pointer note that these keys have header-channel equivalents.
- **Response headers**: lifecycle-only emission rule; `event=request` carries `mode=`.
- **HTTP conformance notes**: body-capture bullet now scopes the cap to retry mode and `+pure`+header (headerless pure = no cap).
- **Server flags**: `--max-body` / `--strict-body-limit` rows scoped to the capturing modes.
- **Error table**: 400 row gains malformed mode suffix / both channels / unknown or degenerate X-Reproxy-*; 413 row scoped.
- **New section "Migration: plain scheme segment"** (R5/R7): v0.2 transitional plain→retry, **the full `event=deprecation` line shape** (`target=`, `trigger=retry-keys|network-retry`, `note=` with the migration call), the gate's two triggers + one-line dedup + when it is silent, the v0.2 observable difference (default network retry), v0.3 terminal state with D15 flip criteria (version+time, operator-side metric, `REPROXY_LEGACY_PLAIN_RETRY=1` escape hatch removed in v0.4), and a how-to-migrate list.
- **Limitations**: tee-mode row now says "outside pure mode" (pure mode streams today).

Wording pins all applied (design §7): "query-pure" prose naming, mode-modifier reading of `+`, actor-source remedy, capture-revival in body text.

### 2. Mutation scan results

Full table + methodology + findings: `mutation-scan.md` (this task dir). Summary:

- **29 mutations applied one at a time** in an isolated copy (`D:\tmp-reproxy-ms`, byte-identical to the working tree — verified per file before starting; deleted after the scan). Compile failures do not count as captures (two first-draft syntax errors were re-run with valid syntax, recorded).
- **27 killed, 2 survived** — both survivors are the same blind spot (F-1 below), so the effective coverage is 27/28 distinct semantics with one gap.
- Three "survivor" readings were diagnosed as mutation artifacts, not gaps, and re-run with corrected mutations: m01's guard made the split a no-op on 400 paths; m20's replace was a silent no-op after ToLower; both corrected and killed.
- Targets covered (the union of design §6 + Batch 3 report §5 + scan-discovered neighbors): pure-mode-splits-query, pure-mode-captures-body, pure-mode-uses-Parse-empty-defaults, retry-mode-ignores-header-conflict, unknown-X-Reproxy-passes, header-not-stripped, empty-header-treated-as-absent, deprecation-log-omitted, deprecation-log-fires-without-retry-keys-but-network-retry, deprecation-dedupe, deprecation-gate-arming-broken, plain-resolves-wrong-mode (parser half m11 + pipeline half m26), header-transform-drops-field, scope-key-double-dot, mode-suffix-casesensitivity (grammar half m14 + header-key half m13), conflict-error-remedy-text, pure-mode-emits-retry-headers, header-single-occurrence (m17 + boundary variant m27), scheme-%2B-decoded-to-mode, network-retry-callback-never-invoked (m09), pure-streaming-framing-changed (m18/m18b — SURVIVED), capture-revival-missing, mode-log-omitted (m21), hasRetryKeys-false (m23), duplicate-key-last-wins (m25).

### 3. Findings

**F-1 (only finding): pure-mode outbound framing has no test lock.** Mutating `req.ContentLength = r.ContentLength` to `-1` (forcing chunked) in proxy.go's pure-streaming branch leaves the entire suite green. `TestProxyBodyContentLengthPreserved` covers only the retry-mode capture path; the pure-mode body tests assert bytes, not framing. This pins Batch 3 deviation 2 (framing preservation as the byte-faithful reading of "streams as-is"). Not fixed in this batch (no .go changes allowed); suggested one-line assertion recorded in mutation-scan.md:

```go
// add to TestProxyPureModeBodyStreamsNoCapture:
if mt.lastReq.ContentLength != int64(len(body)) {
    t.Errorf("ContentLength = %d, want %d (inbound framing preserved)", mt.lastReq.ContentLength, len(body))
}
```

The main session decides whether F-1 blocks (severity: behavioral-fidelity gap, not a functional break — body bytes are unaffected and both framings are legal HTTP; the wire difference is upstream-visible).

### 4. Validation

```
Real tree (after all work; proving nothing was dirtied):
go build ./...   ok
go vet ./...     ok
gofmt -l .       (empty)
go test ./... -count=1   ok  reproxy 4.6s

Isolated scan copy:
- pre-scan: byte-identical to working tree (per-file diff), suite green x2
- post-scan: all 22 .go files + go.mod + cmd/main.go restored to pristine (diff clean),
  suite green, copy deleted
```

README.md is the only working-tree change (plus the two task-dir report files). No .go file touched; no git commands run.

---

## Check round — Phase 2.2 last-iteration full-scope check

Date: 2026-09-06. Scope: all 4 batches, adversarial, per the check dispatch. Verified against prd.md (11 acceptance criteria), design.md §1–§9 (D1–D15), implement.md (4 batches), mutation-scan.md (29/29), and the four backend specs (retry-proxy-contract, error-handling, quality-guidelines, logging-guidelines). No accepted deviation was reopened; no test was weakened or deleted.

### 1. Acceptance-criterion verdicts

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| R1 | Mode-qualified path forms parse/normalize/reject per spec; malformed 400 with usage hint; full grammar table | **Met** | target.go `parseSchemeSegment` ({plain, +retry, +pure} × {http, https}, case-normalized together, `strings.Cut` on `+`); target_test.go ~35-row mode table incl. typo/`+`-alone/double-suffix/`!`/escaped-plus (`https%2Bpure` both hex cases)/IPv6/default ports/raw-path preservation per mode; `usageHint` names all three shapes; plain pinned to `ModeRetry, false` on every plain row with the TODO(v0.3) marker in the parser |
| R2 | Pure mode: RawQuery byte-identical incl. `%`, `+`, valueless, duplicate, `retry.`-prefixed target data | **Met (after fix #1)** | TestProxyPureModeQueryVerbatimAndSingleCall (query byte-equality vs a recorded raw string), TestE2EPureModeRetryDotParamsReachUpstream (the headline `retry.count`/`retry.token` collision, real TCP, upstream-received bytes asserted — quality-guidelines rule 1: outcome not calls). Fix #1 added the `+` character class, which was the one listed byte class with no pure-mode discriminator (it is the byte `url.Values.Encode()` rewrites, so its absence left the re-encode mutation class without a pure-mode lock) |
| R2 | Single upstream call; no X-Retry-*; no body capture (large body streams, no 413/cap) | **Met** | `mt.calls == 1` asserted across all four pure-mode pipeline tests + e2e call counters; X-Retry-* absence asserted in TestProxyPureModeQueryVerbatimAndSingleCall / TestProxyPureModeNetworkFailureNotRetried / TestE2EPureModeRetryDotParamsReachUpstream; TestProxyPureModeBodyStreamsNoCapture (1000-byte body over an 8-byte strict cap, 200, framing preserved per mutation-scan F-1 fix) + TestE2EPureModeLargeBodyNoCap (500-byte body over 16-byte strict cap, byte-identical, real TCP) |
| R3 | Retry mode passes the v0.1.0 suite unchanged | **Met** | git diff on test files: only additions + two import lines (log, net/url) — verified per file; every v0.1.0 test function is unmodified (mechanical PathTarget literal extension only, which pins the transitional plain→retry mapping itself); full suite green ×3 (batches) + ×2 (this check) |
| R4 | Header channel: same field set, same validation matrix, table-driven | **Met (after fix #2)** | header_test.go: pair grammar (now 10 rows incl. the design §2 example verbatim), degenerate table (13 rows), Parse's own 400 matrix through the header path (15 rows), transform equivalence (deep-equal Policy AND deep-equal url.Values), round-trip. Fix #2 added the `jitter`/`retry_after` success-path rows: 7 of 9 field-set members were covered (status, network, budget, attempts, backoff, initial, max), leaving jitter/retry_after parse-only-via-400-rows. "Same field set" is an R4 literal; now all 9 parse successfully through the channel |
| R4 | Headers stripped upstream; both-channels 400; unknown X-Reproxy-* 400 | **Met** | buildOutboundHeaders strips via `isReproxyHeader` (canonical + non-canonical rows); TestE2EHeadersStrippedUpstream (full path, both modes); TestProxyModeChannelConflictMatrix rows 4/6 + TestE2EModeChannelConflictOverTCP (both +retry and plain forms, remedy text asserted); TestHeaderUnknownNamespaceRejected + TestValidateReproxyNamespaceDirect + mode-level 400 tests |
| R4 | Header + pure e2e: policy from headers, query untouched, retries observable | **Met** | TestE2EPureModeHeaderPolicy (real TCP: 500→200, X-Retry-Count: 2, query `retry.count=3&data=%2Fpath` untouched) + TestE2EPureModeHeaderPolicyBodyReplayE2E (POST body replay ×2) |
| R5 | Transitional default logs `event=deprecation`; README migration; behavior matrix pinned | **Met** | TestProxyDeprecationLogGate (4 cases: retry-keys fires, silent-no-keys doesn't, network-retry-consumed fires with zero retry keys, explicit modes never log), TestProxyPlainSchemeDeprecationDedupeBothTriggers (one line when both triggers race), TestProxyModeChannelConflictMatrix (plain × header × retry-keys rows), README "Migration: plain scheme segment" with the full log line shape, both triggers, D15 flip criteria, and the v0.2 observable-difference callout |
| R6 | Logs carry mode/channel; conflict errors name the remedy | **Met** | logRequest emits `mode=` (TestProxyRequestLogCarriesMode, all three forms); deprecation line carries target/trigger/note; conflict 400's hint names the +pure remedy AND the actor source ("if you did not set this header, a middleware or gateway between you and reproxy may have added it") — asserted by substring in both the handler test and the e2e |
| R7 | README: three channels, ownership principle, migration, encoded escape hatch | **Met** | Verified against code line by line (spot-checks below); ownership principle verbatim; three-channel table with query-ownership column; `retry%2E` footnote in retry-mode semantics; all four design §7 wording pins present (query-pure prose, mode-modifier `+` reading, actor-source remedy, capture-revival in the mode-table body) |
| Gates | build/vet/gofmt clean; suite green ×3; CI matrix | **Met (local)** | Static gates + full suite green ×5 total runs across this check (baseline + ×2 after fix set 1 + ×2 after fix set 2). CI matrix is a push-time gate (Batch 4 check list item left for the commit step); not runnable from this agent |
| Mutation | Every pinned semantic locked | **Met** | mutation-scan.md 29/29 (F-1 fixed and re-verified by the main session; the F-1 ContentLength assertion is present in TestProxyPureModeBodyStreamsNoCapture at proxy_test.go:1454-1458). Fix #3 below adds one more proxy-level lock in the same spirit |

### 2. Design conformance details verified (no findings)

- **§3 matrix vs proxy.go ServeHTTP**: every row maps to an actual branch. `+pure`+header parses the header (`retryLifecycle = true` only with a policy); `+retry`/plain run namespace guard → conflict gate → (plain only) deprecation arming → SplitQuery → Parse. Row "plain (v0.3)" correctly absent (separate task per design §7).
- **D13 (single-attempt literal)**: `SingleAttemptPolicy` is the only policy construction on the headerless pure path (proxy.go:206); no call site anywhere passes `url.Values{}` to Parse on the pure path — grep-verified (`Parse(url.Values{})` appears only in tests and doc comments). TestSingleAttemptPolicyLiteral discriminates the literal from Parse(∅) on both Attempts and NetworkGate, and pins the MaxBudget clamp.
- **D14 (capture invariant)**: `if retryLifecycle` guards the whole Capture/413/degraded block (proxy.go:259-285); `+pure`+header re-enables it (TestProxyPureModeHeaderPolicyCapturesBody, degraded + strict rows). Headerless pure never constructs a CapturedBody — the roundTrip nil-capture streaming branch is the only body path it can take.
- **§5 deprecation gate**: both triggers (retry-keys via `hasRetryKeys` at request time; network-retry via the `onNetworkRetry` callback invoked in attemptLoop only after the gate check passes, i.e. an actual retry consumed); per-request dedupe via the `fired` bool closure (one line when both triggers hit); armed only for `!target.ExplicitMode` (plain). No cross-request state. The callback placement is correct: it fires only when a network retry is actually consumed, not on the failure itself — m09's mutation target.
- **§2 header grammar vs header.go**: pair split, whitespace ladder, degenerate → 400, single-occurrence, namespace guard, `queryKeyForHeaderKey` scope-key transform (`retry[*].x`, never `retry.[*].x`), values never URL-decoded, strip in buildOutboundHeaders.
- **SSRF L1–L7 untouched in pure mode**: verified by reading — allowlist gate (ServeHTTP step 5), ResolveAndValidate (step 5b), WithPinnedIPs dial pinning, and the L1 path grammar all run before/independent of the mode switch; mode affects only steps 2–4 (query/policy/body). TestProxyPureModeSSRFStillEnforced + TestE2EPureModeSSRFStillEnforced pin it.
- **Error contract {error, hint}**: every new 400 path (scheme-segment, header pair grammar, namespace, conflict, degenerate) constructs a RequestError with both fields; Write renders the pinned JSON shape. Conflict error names both channels, the +pure remedy, and the actor source.
- **Logging**: `event=deprecation` extends the fixed event catalog (no parallel mechanism); keys are target/trigger/note with note %q-quoted (free text); `mode=` joins the request row; no query strings or target paths beyond host:port in any new line (the deprecation note contains path *shapes*, not request data).

### 3. README spot-checks (all true of the code)

- Grammar table + EBNF: matches target.go exactly (case rules, three shapes, `%2B` rule).
- Mode table's "Who carries the policy" column: matches the ServeHTTP branches row for row.
- Conflict error text quoted vs actual: README paraphrases ("remove the header, or use a `+pure` path") — actual hint says exactly that plus the actor-source sentence; consistent.
- Migration section's deprecation trigger description: `retry-keys`/`network-retry` tokens and the one-line dedupe match newDeprecationLogger + attemptLoop; the example log line is byte-accurate (verified against a real captured line in the test run output).
- X-Reproxy-* reserved namespace claim: matches validateReproxyNamespace (runs in every mode) + the buildOutboundHeaders strip.
- Header example (`status=5xx; network=1; budget=30s; [*].attempts=3; [429].attempts=5`): parses as documented (TestHeaderPairGrammar row 1, verbatim).
- `--max-body` / `--strict-body-limit` scoping and the X-Retry-* lifecycle-only rule: match the D14/commitResponse/exhausted gating.

### 4. Findings

**F-1 (low, test-coverage gap, fixed): pure-mode query byte-identity had no `+` discriminator.** The R2 AC names the byte classes "%, +, valueless, duplicate" for pure-mode byte-identity tests; `+` was the only class absent (present only in retry-mode SplitQuery tests). Since `url.Values.Encode()` is exactly the mutation that rewrites `+` ↔ `%2B`, its absence left that mutation class without a pure-mode lock. Fixed: added `&g=p+q` to the raw query in TestProxyPureModeQueryVerbatimAndSingleCall (proxy_test.go:1362-1365).

**F-2 (low, test-coverage gap, fixed): header channel success-path coverage was 7/9 of the field set.** R4 pins "covers the same field set as the query channel"; `jitter` and `retry_after` appeared in header tests only via 400 rows, never a successful parse. Fixed: added the "jitter and retry_after scope fields" row to TestHeaderPairGrammar (header_test.go:42-45), asserting both `retry[*].jitter` and `retry[429].retry_after` round-trip.

**F-3 (low, test-coverage gap, fixed): degenerate-header 400 had no pure-mode proxy-level test.** Design §2 pins "degenerate input → 400 in every mode"; the proxy-level degenerate test existed only for retry mode (where the error surfaces indirectly). Pure mode is the channel where the header-layer 400 is the operative error (no conflict gate ahead of it) — a regression making ParseRetryPolicyHeader treat empty as absent would turn the pure path into a silent single-attempt pass-through. Fixed: TestProxyPureModeDegenerateHeader400 (proxy_test.go, after TestProxyRetryModeDegenerateHeader400) — three degenerate forms (empty, whitespace-only, empty pair), each asserting 400 + zero transport calls.

**Dismissed (investigated, not defects):**

- D-1: "Client-visible errors are not logged server-side on the new 400 paths" (logging-guidelines "exactly once" rule). Dismissed: this is v0.1.0 behavior carried forward unchanged (git show HEAD:proxy.go confirms — rerr.Write with no logLine predates this task); the new paths are consistent with the existing convention, the guideline's intent (no double logging) is honored, and changing the error-logging convention is out of scope for this task's check round.
- D-2: "TestProxyPerStatusScopeShaping flake." Isolate-verified 5/5 PASS in isolation and green in all 5 full-suite runs this round; the documented Windows timing class, not a regression.
- D-3: "`+pure`+header e2e lacks a degraded-body row." The D14 revival is covered at the handler level in both degraded and strict modes (TestProxyPureModeHeaderPolicyCapturesBody) and the replay path over real TCP (TestE2EPureModeHeaderPolicyBodyReplayE2E); the specific degraded-over-TCP combination adds no new code path. Dismissed as redundancy.
- D-4: "`mode=` not on deprecation/error 400 paths." By design: 400s exit before logRequest in v0.1.0 too; the request-summary line is the channel-mix signal (design §5/D15), and deprecation lines carry target+trigger already.
- D-5: "hasRetryKeys counts invalid retry keys as present." Intentional and documented (query.go comment): the inert scan arms the gate; SplitQuery produces the precise 400 later — TestProxyPlainSchemeExplicitRetryKeysUnknownStill400 pins the interaction.

All three fixes are test-strengthening only (assertions added, no production code changed, no test weakened). Mutation-lock posture improves monotonically: F-1 closes the pure-mode re-encode blind spot; F-3 closes the pure-path half of the empty-header normalization ladder at the pipeline level.

### 5. Final gates (after all fixes)

```
run 1: go build ./... ok; go vet ./... ok; gofmt -l . (empty); go test ./... -count=1  ok reproxy 4.811s
run 2: go build ./... ok; go vet ./... ok; gofmt -l . (empty); go test ./... -count=1  ok reproxy 4.226s
```

Two consecutive full-gate runs green. TestProxyPerStatusScopeShaping green in every run (and 5/5 isolated). No commits made; working tree delta = the original 10-file change set plus the three test strengthening edits (proxy_test.go +2 rows/1 new test, header_test.go +1 row).
