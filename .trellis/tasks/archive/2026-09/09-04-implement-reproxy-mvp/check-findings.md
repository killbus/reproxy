# Check Findings: reproxy MVP final full-scope check

> Check agent: Trellis workflow step 2.2, final iteration
> Date: 2026-09-04
> Scope: PRD acceptance checklist, pinned-semantics conformance (audit Part 1), cross-cutting code quality, README accuracy, test quality, stability
> HEAD under review: 00a57e5 (Steps 1-10 complete)

## Executive summary

| Area | Status |
|---|---|
| Quality gates (build/vet/test/gofmt) | PASS (3 pre-fix + 3 post-fix suite runs green; race detector unavailable on host — no gcc for CGO, manual concurrency review instead) |
| PRD acceptance checklist | PASS (all ACs covered by tests; see checklist below) |
| Pinned-semantics conformance | PASS (all pinned items conform; confirmed independently by coordinator's 23/23 mutation scan) |
| Cross-cutting code quality | 1 major defect found and FIXED (F-1 resource leak); several minor/nit fixed |
| README accuracy | PASS (flags/params/semantics all match code; general-purpose positioning clean) |
| Test quality | PASS (defects fixed: dead helper, vacuous assertion, timing flakes F-10) |
| Stability (3x suite runs) | PASS (6/6 total runs, incl. 3 after all fixes) |

## Quality gate results

| Run | Command | Result | Duration | Notes |
|---|---|---|---|---|
| 1 | `go build ./... && go vet ./... && gofmt -l .` | PASS | ~5s | clean |
| 1 | `go test ./... -count=1` | PASS | 4.754s | 130 top-level tests, 309 total RUN entries |
| 2 | `go test ./... -count=1` | PASS | 4.629s | |
| 3 | `go test ./... -count=1` | PASS | 4.539s | |
| extra | `go test ./... -count=1 -race` | N/A | — | host has no C compiler (`gcc` not found with CGO_ENABLED=1); race check done by manual concurrency review instead |
| post-fix 1 | `go build ./... && go vet ./... && gofmt -l .` | PASS | ~5s | after F-1/F-3/F-4/F-6/F-8/F-10/F-11 fixes |
| post-fix 2 | `go test ./... -count=1` | PASS | 5.100s | |
| post-fix 3 | `go test ./... -count=1` | PASS | 4.827s | |
| post-fix 4 | `go test ./... -count=1` | PASS | 4.907s | |

## Findings

### F-1 (major, fixed-by-me): response body leak when RoundTrip races the TTFB timer / client disconnect

- **File**: `proxy.go:453-463` (`roundTrip`)
- **Description**: When the TTFB timer (or client-disconnect) branch of the `select` wins while the `done` channel already holds a successful `(*http.Response, nil)` result, the code discards `res.resp` after `<-done` without closing `resp.Body`. With the real stdlib transport, an abandoned-but-returned response keeps the pooled connection from being reused and leaks a connection (and its read-loop goroutine) each time the race hits.
- **Evidence**: deterministic repro added as a probe test — a transport that returns a response ~20ms after the 10ms budget-based TTFB cap: `LEAK: response body from the raced RoundTrip was never closed` (probe asserted `Body.Close()` never fired). Also applies to the `<-r.Context().Done()` branch (client disconnect mid-roundtrip).
- **Fix**: in both race branches, after `<-done`, if `res.err == nil && res.resp != nil`, drain-and-close the response body before returning the error. Applied in `proxy.go`; verified with the probe test flipping to closed=true.
- **Status**: fixed-by-me; full gates re-run green.

### F-2 (minor, accepted-risk): `Retry-After` cap uses only `sp.Max` + remaining budget, not remaining-attempt feasibility

`backoff.go:98-106` — honored Retry-After is capped to `sp.Max` and remaining budget, exactly as the audit pins ("cap 到 max 与剩余 budget 较小值"). No defect. (Listed to document the conformance check.)

### F-3 (minor, fixed-by-me): dead test helper `doRecovered` in `proxy_test.go`

- **File**: `proxy_test.go:724-742`
- `doRecovered` duplicates `do` (which already recovers `http.ErrAbortHandler`) and is referenced by no test. Dead code left over from an earlier iteration.
- **Fix**: removed.
- **Status**: fixed-by-me.

### F-4 (minor, fixed-by-me): vacuous assertion in `TestProxyClientDisconnectDuringWait`

- **File**: `proxy_test.go:1052-1056`
- `if w.Code != 200 { /* comment only, no error */ }` asserts nothing. The meaningful assertions (`mt.calls == 1`, empty body) follow; the empty if-block is misleading dead test code.
- **Fix**: replaced with an explicit `w.Body.Len() == 0` style comment cleanup (kept behavioral assertions, removed the empty branch).
- **Status**: fixed-by-me.

### F-5 (minor, needs-decision): outbound Host carries the scheme-default port (`up.example.com:80`)

- **File**: `proxy.go:414` (`req.Host = target.HostPort()`), pinned by `TestE2EHostHeaderIsUpstreams`
- The outbound `Host` is `host:port` with the default port materialized (e.g. `:80` for http). Semantically valid, but some strict upstreams compare `Host` against the origin-form without default port. `nginx` sends `$proxy_host` without a default port.
- Behavior is intentional (e2e test documents it), consistent, and harmless for the vast majority of servers. Recording as accepted-risk rather than changing behavior at final-check stage.
- **Status**: accepted-risk (behavior change would alter a pinned e2e test; recommend revisit only if a real upstream rejects it).

### F-6 (nit, fixed-by-me): `var bodies []string; _ = bodies` dead declaration in `TestProxyBodyReplayedAcrossRetries`

- **File**: `proxy_test.go:480-482`
- Leftover scaffolding. Removed.
- **Status**: fixed-by-me.

### F-7 (nit, recommendation only): `proxy_mod.html` is stray research scaffolding

- Repo-root `proxy_mod.html` (3,739 lines) is a saved copy of the nginx `ngx_http_proxy_module` documentation page — a leftover artifact from the prior audit task (09-03). It is untracked (`??` in git status), referenced by nothing in the repo (`grep proxy_mod` across code/docs/spec: zero hits).
- Per instructions I did NOT delete it; recommend the main session remove it or move it under the archived task's research folder before finishing.
- **Status**: needs-decision (recommend removal).

### Verified non-issues (probed, no defect)

- Bare `retry` key and `retryfoo`/`myretry.x` pass through untouched (namespace boundary correct); `retry[foo` without `]` is a 400.
- `retry.status=1xx` parses (class shorthand accepts 1-5); `0xx`/`9xx` are 400s. Retrying on 1xx is unreachable in practice (transport never surfaces 1xx as final).
- `retry.network` valueless (`retry.network`) decodes to `""` which the parser treats as `1` (default true) — fail-open on a valueless boolean, but the namespace is client-opt-in; matches the documented "0 or 1" validation? No: `""` is accepted as true. README says validation is "`0` or `1`". Minor doc/code divergence — see F-8.

### F-8 (nit, fixed-by-me): README says `retry.network` accepts only `0` or `1`, but an empty value is accepted as `1`

- **File**: `policy.go:442-445` vs `README.md` line 61 ("`0` or `1`")
- A valueless `?retry.network` sets the gate to true (default) rather than 400. This is consistent with "valueless retry key" handling in `query_test.go` ("retry key empty value" -> `""`) but diverges from the README's stated validation. Fail direction is safe (default-on gate).
- **Fix**: aligned the code with the stricter documented contract: `""` no longer accepted as `1` — only `0` and `1` are valid. Wait — this changes behavior; the safer fix is the README. Decision: fix the README to document reality ("`0` or `1`; a valueless key is treated as `1`"). Actually the cleanest is to make `""` a 400 since a valueless `retry.network` is a client error and fail-closed is the spec's stated posture for invalid values. Fixed in code (400), matching the README as written; added a test case.
- **Status**: fixed-by-me (`policy.go` + test).

### F-9 (nit, fixed-by-me): README wording for valueless retry keys

The handler now fails closed on valueless `retry.network` / `retry.status` / `retry.budget` (previously only `network` silently defaulted to `1` while `status`/`budget` already 400'd). The query_test table's "valueless retry key" cases only assert namespace stripping (still correct — the 400 comes later from policy parsing). The README's "`0` or `1`" validation column is now exactly true.

### F-10 (major — test defect, fixed-by-me): timing-sensitive upper bounds flake on Windows

- **Files**: `proxy_test.go` — `TestProxyRetryAfterIgnored` (~593), `TestProxyRetryAfterPastDate` (~961), `TestProxyPerStatusScopeShaping` (~1101), plus `TestProxyBudgetExhaustion` (~252) for the same pattern
- **Source**: coordinator's mutation scan (`.trellis/tasks/09-04-implement-reproxy-mvp/mutation-scan.md`, 附带发现 section) — 4/8 at-rest runs failed; the handler pipeline (fully mocked) was observed varying 4-68ms while the asserts allowed only 100-150ms over 1-2ms waits.
- **Analysis (my own, agreeing)**: these tests' discriminating power is that a multi-second wait did NOT happen (ignored Retry-After, past-dated Retry-After, per-status scoped 1ms wait vs 300ms default). A 100ms upper bound measures pipeline speed, not the pinned semantics. Windows timer/scheduler jitter alone spans tens of ms. Relaxing to 500ms loses zero discrimination (a real failure mode waits seconds) and removes the flake. Same pattern proactively relaxed in `TestProxyBudgetExhaustion` (300ms -> 500ms over a 100ms budget; the 30s initial would blow through any bound if the cap broke).
- **Fix applied**: all four upper bounds relaxed to 500ms with a comment explaining the intent.
- **Verification**: gates green; 3 consecutive full-suite runs all pass (5.1s / 4.8s / 4.9s).
- **Status**: fixed-by-me.

### F-11 (minor, fixed-by-me): R8 per-retry log omitted the backoff value

- **File**: `proxy.go` (`attemptLoop`, both the network-error and status-retry paths)
- PRD R8 pins the per-retry log content as "attempt / status / backoff / remaining budget". The `event=retry` line carried attempt/status/reason/remaining budget/attempts-vs-limit but not the actual waited backoff. Verified live: `event=retry target=... attempt=1 status=500 reason="retryable status" remaining_budget=5s attempts=1/3`.
- **Fix**: added an `event=wait target=... backoff=...` line emitted immediately before each backoff sleep (both retry channels), so every retry's wait is observable. No behavior change; log-only.
- **Status**: fixed-by-me; gates re-run green.

## PRD AC checklist

PRD acceptance criteria, walked item by item (test name + file). All pass on the final tree.

| AC | Verifying tests (file) | Status |
|---|---|---|
| R1–R9 all have unit tests; `go vet` / `go test ./...` green | 130 top-level test functions across 9 `_test.go` files (309 total test entries incl. subtests); gates green on 3 pre-fix runs and 3 post-fix runs | PASS |
| — R1 path parsing (strict grammar) | `TestParsePath` (target_test.go, 40+ table cases: default ports, leading-zero ports, userinfo, IPv6 brackets, host shapes, encoded bytes); `TestPathTargetNormalizeAndRendering`; handler-level `TestProxyBadTarget400`, `TestProxySchemeAndPortNormalization` (proxy_test.go) | PASS |
| — R2 query namespace split | `TestSplitQueryPassthroughBytePreservation`, `TestSplitQueryRetryNamespaceStripped`, `TestSplitQueryRetryParamsCollected`, `TestSplitQueryNotInNamespace`, `TestSplitQueryUnknownRetryKeysRejected`, `TestSplitQueryEmpty` (query_test.go) | PASS |
| — R3 policy parsing (three tiers) | `TestParseDefaults`, `TestParseThreeTierPriority`, `TestParseScopeMergeFullPolicy`, `TestParseServerClamp`, `TestParseStatusGate(+Invalid)`, `TestParseDeadConfig`, `TestParseGateInStatusScope`, `TestParseFieldOrderIndependence` (policy_test.go); handler-level `TestProxyDeadConfig400` | PASS |
| — R4 body capture/replay | `TestCaptureEmptyBody/UnderCap/ExactlyAtCap/OneByteOverCap/LargeOversizedBody/ZeroCap/ReaderReplayIndependence/ReadError` (body_test.go); `TestProxyBodyReplayedAcrossRetries`, `TestProxyDegradedBodyPassthrough/StillSSRFChecked/Forwarded`, `TestProxyStrictBodyLimit413`, `TestProxyBodyContentLengthPreserved` (proxy_test.go); `TestE2EPOSTOversizedDegraded` (e2e_test.go) | PASS |
| — R5 retry loop (dual channel) | `TestProxyRetriesStatusSequence`, `TestProxyNoRetryWithoutStatusGate`, `TestProxyNetworkErrorRetry`, `TestProxyNetworkGateClosed`, `TestProxyExhaustedDeliversLastResponse`, `TestProxyNetworkExhaustion504`, `TestProxyBudgetExhaustion`, `TestProxyTTFBTimeoutRetryable`, `TestProxyRetryAfterHonored/CappedByBudget/Ignored/HTTPDate/PastDate`, `TestRoundTripTimeoutTable`, `TestSleepCtx` (proxy_test.go); `TestComputeWait*`, `TestParseRetryAfter` (backoff_test.go); `TestE2ERetryToSuccess`, `TestE2EExhaustionDeliversLastResponse`, `TestE2EBudgetExhaustion` (e2e_test.go) | PASS |
| — R6 commit point | `TestProxyCommitPointNoRetryAfterHeaders` (mid-body death -> no retry, partial bytes, ErrAbortHandler), `TestProxySSEStreamedNotBuffered`, `TestE2ESSEStreaming` (real TCP, chunk-timing asserted) | PASS |
| — R7 SSRF | `TestIsForbiddenIPv4FullList` (every prefix covered, coverage-enforced), `TestIsForbiddenIPv4Boundaries`, `TestIsForbiddenIPv6FullList`, `TestIsForbiddenIPv6Boundaries`, `TestIsForbiddenIPMappedAndDerived`, `TestResolveAndValidate*` (mixed fail-closed, literal, empty, lookup error, zones), `TestDialContext*` (pins IP, L4 re-assert, inline resolve, family mismatch) (ssrf_test.go); `TestValidateRefusesEmptyAllowlistWithoutEscapeHatch`, `TestAllows*` (config_test.go); handler/e2e: `TestProxySSRFAllowlistGate/PrivateResolution/IPLiteralPrivate`, `TestE2ESSRFPrivateIPLiteral/PrivateResolution/AllowlistGateBeforeDNS`, `TestProxyRedirectNotFollowed` (L5) | PASS |
| — R8 observability | `X-Retry-Count/Limit/Exhausted/Dropped` asserted throughout proxy/e2e tests (e.g. `TestProxyRetriesStatusSequence`, `TestProxyExhaustedDeliversLastResponse`, `TestProxyDegradedBodyPassthrough`, `TestE2EExhaustionDeliversLastResponse`); per-retry log verified live during check (now includes `event=wait backoff=`, see F-11); JSON error bodies asserted (`TestE2EUnknownRetryKey400`, `TestE2EDeadConfig400`) | PASS |
| — R9 server config | `TestNewDefaultConfig`, `TestParseFlagsDefaults/Overrides/RepeatableAllowlist`, `TestValidateRejectsBadValues` (config_test.go); `cmd/reproxy/main.go` (startup gate log, graceful shutdown) | PASS |
| Signed-URL query byte preservation (special test) | `TestSplitQueryPassthroughBytePreservation` (14 byte-level cases: `%2F`, `%20`, `+`, valueless, duplicate, raw `%zz`), `TestProxyQueryBytePreservation`, `TestE2EQueryBytePreservation` (real TCP round trip, RawQuery byte-identical) | PASS |
| Commit-point semantics (headers written then upstream dies -> no retry, unit-mocked) | `TestProxyCommitPointNoRetryAfterHeaders` — asserts 200 already committed, partial body delivered, transport called exactly once, ErrAbortHandler raised | PASS |
| SSE live streaming (e2e against mock upstream) | `TestE2ESSEStreaming` — 3 events 100ms apart, first-arrival promptness AND >=150ms spread asserted on read timestamps; `TestProxySSEStreamedNotBuffered` (write-side timing) | PASS |
| SSRF forbidden list (v4+v6 full list, allowlist default-deny, resolve-then-pin rebinding) | `TestIsForbiddenIPv4FullList` + `TestIsForbiddenIPv6FullList` walk every prefix in the tables (with a coverage guard asserting no prefix lacks cases); `TestResolveAndValidateMixedResults` ([public, private] -> reject = the rebinding/TOCTOU posture); `TestDialContextDialsPinnedIP` (dials the validated IP, never the hostname) | PASS |
| Body-cap degradation (cap=10MiB boundary, incl. strict mode) | `TestCaptureExactlyAtCap` (cap == cap NOT oversized), `TestCaptureOneByteOverCap` (+1 -> oversized), `TestProxyStrictBodyLimit413`, `TestProxyDegradedBodyPassthrough/Forwarded` (byte-exact splice-back), `TestE2EPOSTOversizedDegraded` (full 20-byte body over 16-byte cap) | PASS |
| Retry-After honor + cap (seconds + HTTP-date + past + huge) | `TestParseRetryAfter` (11 cases incl. RFC850/ANSIC/OWS), `TestComputeWaitRetryAfter` (8 cases: capped-at-max, passthrough, past-date->0, unparseable fallback, huge->max), `TestComputeWaitRetryAfterNotJittered`, `TestProxyRetryAfterHonored/CappedByBudget/HTTPDate/PastDate/Ignored` | PASS |
| Integration smoke (`go run .` -> curl through proxy) | `TestE2E*` (15 tests, real listener + real upstream + real client over TCP, no mocks above the network layer); README Quick Start documents the literal curl procedure; `go build ./...` green | PASS (see note 1) |
| README: usage, config, protocol (retry params, attempts table), SSRF warnings | README reviewed against code line by line (see README accuracy section) | PASS |

**Note 1**: The PRD's smoke item is satisfied by the e2e suite (real listener via `httptest.NewServer(proxy)` + real `http.Client` over TCP + the production `PinnedResolver`/dial path) plus a verified build. There is no literal curl-script test; the identical code path (ServeHTTP over real TCP, real dial) is exercised, so I judge this satisfied rather than MISSING.

## Pinned-semantics conformance (audit Part 1) — final status

Every item from the dispatch's pinned list was checked; the mutation scan (23/23 caught, 0 survivors) independently confirms the suite locks these behaviors:

- attempts = TOTAL including first (attempts=1 -> no retry): `TestParseDefaults`, `TestProxyRetriesStatusSequence` (X-Retry-Count: 3), README table. CONFORMS.
- Three-tier resolution, field-level override, server clamps narrow-only: `TestParseThreeTierPriority`, `TestParseServerClamp` (clamp leaves below-cap values untouched); probed that the server cap wins over a `retry[500].attempts` raise. CONFORMS.
- Dead-config 400: `TestParseDeadConfig`, `TestProxyDeadConfig400`, `TestE2EDeadConfig400`. CONFORMS.
- Status gate syntax (exact/range/Nxx; reversed range 400; `5XX` case): `TestParseStatusGate(+Invalid)`, `TestProxyStatusGateClassShorthand`. CONFORMS.
- Backoff formulas + jitter three-state: `TestComputeWaitExponentialSequence` (1,2,4,8,8,8), `TestComputeWaitLinear`, `TestComputeWaitConstant`, `TestComputeWaitJitterFullBounds/EqualBounds/Statistics`, `TestComputeWaitOverflowSaturation` (no int64 wrap). CONFORMS.
- Retry-After honor: replaces computed backoff, capped at max + remaining budget, NOT jittered: `TestComputeWaitRetryAfterNotJittered` (50 iterations, exact 5s under full jitter), `TestComputeWaitRetryAfter` cap cases, `TestProxyRetryAfterCappedByBudget`. CONFORMS.
- Budget: hard cap over attempts+waits; exhaustion -> last response or 504: `TestComputeWaitBudgetCap`, `TestProxyBudgetExhaustion` (network branch -> 504), `TestE2EBudgetExhaustion` (status branch -> last response delivered, exhausted flag). CONFORMS.
- Commit point: retry ends at first byte; post-commit failure -> panic(ErrAbortHandler): `TestProxyCommitPointNoRetryAfterHeaders` (panic path exercised via recover), copyWithFlush + Flush per chunk. CONFORMS.
- Body capture: ReadFull(cap+1) probe; degraded = streaming pass-through with prefix spliced back + X-Retry-Dropped + warn log; strict -> 413: `TestCaptureExactlyAtCap/OneByteOverCap`, `TestProxyDegradedBodyForwarded` (byte-exact), `TestE2EPOSTOversizedDegraded`, `TestProxyStrictBodyLimit413`. CONFORMS.
- SSRF: allowlist before DNS (`TestE2EAllowlistGateBeforeDNS` asserts 0 lookups), resolve-then-pin all-records fail-closed (`TestResolveAndValidateMixedResults`), forbidden list complete (v4 15 prefixes + v6 5 + mapped/NAT64/6to4; coverage guard), dial re-assert (`TestDialContextReassertsForbidden`), never follows 3xx (`TestProxyRedirectNotFollowed`). CONFORMS.
- Query byte preservation: `TestSplitQueryPassthroughBytePreservation` + `TestE2EQueryBytePreservation` (RawQuery byte-identical over real TCP). CONFORMS.
- Hop-by-hop stripping both directions + Connection tokens: `TestProxyHopByHopStrippedOutbound` (incl. `Connection: X-Broken-Token` token-listed header), `TestProxyHopByHopStrippedInbound`; Via/XFF append: `TestProxyViaAndXFFChainAppend` (chain preserved + appended), X-Forwarded-Proto/Host asserted. CONFORMS.
- Error responses: JSON `{"error","hint"}` naming the offending key: `TestE2EUnknownRetryKey400`, `TestE2EDeadConfig400`, all invalid-value tables assert the reason names the key. CONFORMS.

## README accuracy + positioning

- **General-purpose positioning**: README line 3 says "lightweight, stateless, general-purpose HTTP retry reverse proxy"; no scenario-narrowing anywhere. No LLM-specific language in README or code comments; defaults reference the audit's general-purpose values (10 MiB cap, 30s budget, 10 attempts). PASS.
- **Flags table vs config.go ParseFlags**: all 7 flags match (`--listen :8080`, `--allowlist`, `--max-attempts 10`, `--max-budget 30s`, `--max-body 10485760`, `--strict-body-limit false`, `--dangerous-allow-all false`); repeatable `--allowlist` documented and true (`fs.Func`). Slowloris ReadHeaderTimeout 10s + graceful drain 10s documented, matches `cmd/reproxy/main.go`. PASS.
- **Parameter table vs policy.go**: all 6 scope fields + 3 gates with correct defaults (3/exponential/1s/8s/full/honor; status empty, network 1, budget 30s) and validation descriptions. PASS.
- **X-Retry-Count semantics**: "Total attempts made, including the first" matches code (`attempts` = attemptNo of the last executed attempt). PASS.
- **TTFB 30s default**: `roundTripTimeout` const `defaultPerTry = 30s`, bounded by remaining budget; README matches. PASS.
- **SSRF layer descriptions (L2–L7)**: match code (allowlist-before-DNS, resolve-then-pin fail-closed, dial re-assert, no redirect following, budget+TTFB bounds, startup gate). PASS.
- **Exclusions list**: all 10 items from the audit's out-of-scope list present, correctly described (incl. the "class shorthand works in retry.status" clarification). PASS.
- **attempts=1/2/3 table**: matches (1->0 retries, 2->1, 3 default->2, N->N-1). PASS.
- **Error-status table (400/403/413/502/504)**: matches code paths (parse/allowlist/forbidden-IP 403, strict 413, DNS failure 502, network exhaustion/budget 504). PASS.

## Final verdict

**READY-FOR-FINISH**

- All 9 PRD requirement areas verified against code and tests; every acceptance criterion has a passing, discriminating test (see AC checklist).
- Quality gates: build / vet / gofmt clean; suite green on 3 consecutive pre-fix runs (4.75/4.63/4.54s) and 3 consecutive post-fix runs (5.10/4.83/4.91s).
- Mutation scan (coordinator, 23/23 caught, 0 survivors): every pinned semantic is locked — the suite demonstrably fails when any of them is broken.
- Defects found and fixed by this check: F-1 (response body leak on raced TTFB-timeout/client-disconnect — major, new regression test `TestProxyRacedResponseBodyClosed`), F-3/F-4/F-6 (test dead code / vacuous assertion), F-8 (`retry.network` empty value now 400 per documented contract), F-10 (timing flake, relaxed bounds per mutation-scan finding), F-11 (R8 log now includes the backoff value).
- Non-blocking recommendations for the main session:
  - **F-5** (accepted-risk): outbound `Host` carries the scheme-default port (`up.example.com:80`). Pinned by an e2e test; semantically valid; revisit only if a real upstream rejects it.
  - **F-7** (needs-decision): remove or relocate `proxy_mod.html` (untracked nginx-docs research artifact in repo root) before finishing — I did not delete it per instructions.
  - Race detector could not run on this host (no C compiler); concurrency was reviewed manually instead. The TTFB-goroutine paths (F-1 area) are covered by a deterministic regression test.


