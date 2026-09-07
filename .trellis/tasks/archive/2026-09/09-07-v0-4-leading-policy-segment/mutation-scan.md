# Mutation scan — v0.4.0 leading policy segment

Method (per quality-guidelines.md): an isolated copy of the repo (robocopy
via git ls-files to a temp dir, never the main tree) carries each mutant;
`go vet` must pass (a compile failure is NOT a capture — mutants were
re-run with valid syntax when the first cut failed to compile); the full
`go test .` suite must show at least one FAIL; then the file is restored
from the pristine snapshot and `diff`-verified byte-identical.

Scan environment: /tmp/reproxy-mutation-scan (disposable), copied from the
working tree at unit-3-complete state (405 tests, all green). All 10
mutants killed; 0 survivors.

| ID | Behavior pinned | Mutation (one mechanism, valid syntax) | Capturing tests (failures observed) |
|---|---|---|---|
| m1 | Leading-`+` dispatch exists: `/+POLICY/...` parses as a control segment | `if false && strings.HasPrefix(rest, "+")` in ParsePath — dispatch disabled, every `/+POLICY` URL falls to scheme parsing | 46 failures: TestParsePath, TestProxyRetriesStatusSequence, TestHeaderTransformEquivalence, TestE2ERetryToSuccess, conflict matrix, every policy-bearing e2e row |
| m2 | No `+` logic in parseSchemeSegment (v0.3 death shape dies as unsupported scheme) | restore a v0.3 `strings.Cut(segment, "+")` cut inside parseSchemeSegment before the table | 3 failures: TestParsePath, TestProxyBadTarget400, TestE2EV03DeathShape400 |
| m3 | Bare word in the leading segment is the generic unknown-field 400 (eager key validation) | segment mapper made total: `if gateKeys[key] || true` in queryKeyForSegmentKey | 36 failures: TestParsePath bare-word rows, TestProxyUnknownRetryKey400, TestE2EUnknownRetryKey400, equivalence tests |
| m4 | Segment + header carriers are mutually exclusive (conflict 400) | `case false && segmentParams != nil && headerParams != nil:` in proxy.go — conflict gate disabled | 2 failures: TestProxyPolicyChannelConflictMatrix, TestE2EPolicyChannelConflictOverTCP |
| m5 | `%2B` is not `+` (original-bytes fence on the dispatch byte) | `strings.ReplaceAll(rest, "%2B", "+")` before the dispatch in ParsePath | 3 failures: TestParsePath (`%2B` rows), TestProxyBadTarget400, TestE2EPlusInTargetPathForwardedVerbatim |
| m6 | Policy-less path is the D13 single-attempt literal, never Parse(∅) defaults | `Parse(url.Values{}, p.Config)` instead of `SingleAttemptPolicy(p.Config)` in the no-policy branch | 1 failure: TestProxyPlainNetworkFailureNotRetried (network gate armed by the v0.1.0 defaults -> 3 attempts instead of 1) |
| m7 | `+` is control ONLY at segment position 1 (target-path `+` is data) | dispatch on `strings.Index(rest, "+")` (greedy, any `+` before the scheme) instead of the position-1 HasPrefix | 4 failures: TestParsePath (`/https/host/+x` rows), TestProxyPlusLeadingPathSegmentIsTargetData, TestE2EPlusInTargetPathForwardedVerbatim, TestE2EV03DeathShape400 |
| m8 | `/+status=5xx` with no scheme after it is the missing-scheme 400 | `if false && rest == ""` around the missing-upstream-scheme return | 2 failures: TestParsePath (missing-scheme rows), TestProxyBadTarget400 |
| m9 | `/+` and `/+/https/h` are the degenerate empty-control-segment 400 (absent is the only no-policy spelling) | `if false && strings.TrimSpace(policySeg) == ""` — empty body falls through to pendingPolicy (which is "" -> no policy) | 2 failures: TestParsePath (`/+`, `/+/https/h` rows), TestProxyBadTarget400 |
| m10 | Query is unconditionally upstream-owned (byte-identical RawQuery) | `q := r.URL.Query(); upstreamQuery := q.Encode()` — re-encode instead of pass-through | 7 failures: TestProxyQueryBytePreservation, TestE2EQueryBytePreservation, TestProxyPlainQueryVerbatimAndSingleCall, and 4 more query-verbatim rows |

Notes:

- m1's first cut disabled the dispatch with a syntax-valid `false &&`
  guard; the m6 first cut failed to compile (`undefined: url`) and was
  re-run with the import added before counting as a capture — compile
  failure is not a capture.
- After every mutant the touched file was restored from its pristine
  snapshot and `diff`-verified byte-identical; after the last revert the
  full suite in the scan copy ran green (405 cases).

Result: 10/10 killed, 0 survivors.
