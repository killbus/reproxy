# Check report — no phantom migration

Task: 09-06-no-phantom-migration. Check agent verification of the plain→pure flip and the v0.2→v0.3 transition-layer deletion (all changes uncommitted in the working tree).

## 1. Gates

Full sequence `go build ./... && go vet ./... && gofmt -l .` (empty) + `go test ./... -count=1`:

| Run | When | Result |
|---|---|---|
| 1 | before check-agent fixes | green (ok reproxy 4.700s) |
| 2 | before check-agent fixes | green (ok reproxy 3.806s) |
| 3 | after check-agent fixes (§4) | green (ok reproxy 4.655s) |
| 4 | after check-agent fixes (final) | green (ok reproxy 4.571s) |

- No `TestProxyPerStatusScopeShaping` failure occurred in any full-suite run. Isolated verification `go test -run TestProxyPerStatusScopeShaping -count=50` → PASS (50/50). The implement agent's single intermediate-run flake did not reproduce; test untouched (this check agent also did not modify it).
- Race detector: not run on this host (no C compiler) — pre-existing documented limitation; the Linux CI job runs `-race`. CI workflow (`.github/workflows/ci.yml`) exists with ubuntu-latest + windows-latest matrix and Linux race; it executes on push/PR, so it runs when the main session commits (the working tree is uncommitted — CI green cannot be observed yet; gates on this host are the available evidence).

## 2. AC verification

| # | AC | Verdict | Evidence |
|---|---|---|---|
| 1 | `/https/host?anything` ≡ `/https+pure/host?anything` (query verbatim incl. retry.*-shaped, body streams, single attempt, no X-Retry-*) | PASS | Code: plain and `+pure` both resolve to `Mode == "pure"` (`target.go` parseSchemeSegment) and share the single `case ModePure` branch in `ServeHTTP` — behavior identity is structural, not coincidental. Tests: `TestProxyPlainSchemeQueryNeverSplit` (500-relay, 1 call, `retry.wat=1&data=%2Fpath` byte-identical, X-Retry-* absent — header-absence assertions added by this check, see §4). Request-body streaming on plain is covered by code-path identity plus `TestProxySSEStreamedNotBuffered` (plain path) and `TestProxyPureModeBodyStreamsNoCapture` (`+pure` twin). |
| 2 | `/https/host` + `X-Reproxy-Retry-Policy` = header channel with pure mode, NOT 400 | PASS | `TestProxyPlainSchemeHeaderPolicyRetries`: 200, 2 calls, query untouched, `X-Retry-Count: 2`. `TestProxyModeChannelConflictMatrix` "plain, header policy" row = 200/1-call (header with 200-upstream mock → single attempt). Conflict 400 remains only on `+retry`. |
| 3 | `+retry` behavior unchanged | PASS | All retry-semantics tests switched to `/http+retry/` paths, assertions unchanged (git diff shows path-prefix-only changes for every §2-switched test — verified per-test, see §5). Full suite green ×4. |
| 4 | No deprecation machinery: AC grep zero hits | PASS | `grep -rnE "deprecation\|hasRetryKeys\|onNetworkRetry\|ExplicitMode\|LEGACY_PLAIN" --include="*.go" .` → zero hits (exit 1). Only remaining "R5" in Go sources is `proxy.go:3` "(R5 + R6)" — the original 09-04 MVP PRD's retry-loop/commit-point requirement IDs, pre-existing, untouched by this task. |
| 5 | `ExplicitMode` removed from `PathTarget` | PASS | `target.go`: struct has Scheme/Host/Port/RawPath/Mode only; `parseSchemeSegment` returns `(scheme, mode string, rerr *RequestError)`; `ParsePath` construction updated. |
| 6 | README: no Migration section, no v0.3 refs, no REPROXY_LEGACY_PLAIN_RETRY | PASS | `grep -niE "deprecation\|LEGACY_PLAIN\|migration\|v0\.3" README.md` → zero hits. Remaining `v0.1.0` mentions are the legitimate historical network-retry-difference callout (PRD: stays, migration framing removed — verified). Plain bullet reads "pure mode, same as `+pure`". |
| 7 | Specs updated (contract + logging) | PASS | `retry-proxy-contract.md`: §2 hasRetryKeys signature gone; mode grammar "plain → pure mode (terminal state)"; matrix plain rows = pure-mode rows (plain+header = intended combo); deprecation-gate subsection deleted; §4 row "`+retry` mode + header"; §5 plain+header Good case; §6 plain-form test entry. `logging-guidelines.md`: `deprecation` event row + its rules deleted; `mode=` rule says plain logs `pure`. |
| 8 | Mutation scan 3/3 killed, report in task dir | PASS (report) | `mutation-scan.md` present: m1 (plain→ModeRetry) 5 kills, m2 (plain/pure+header→conflict 400) 8 kills, m3 (SplitQuery on pure path) 8 kills — all KILLED, isolated copy, revert-diff-verified. All named capture tests exist in the tree and pass (verified by name grep + green suite). Method matches quality-guidelines.md §anti-self-certification. |
| 9 | Gates green ×3; CI green | PASS (host) / PENDING (CI) | Gates green ×4 on this host (§1). CI cannot run until commit — workflow is configured and unchanged; main session's commit phase triggers it. |
| 10 | Tag v0.2.1 | PENDING (main session) | Correctly deferred per implement-report deviation 3: tagging is a git/commit-phase action forbidden to implement/check agents. `git tag -l` shows v0.1.0, v0.2.0 only. Main session must tag v0.2.1 at commit time. |

## 3. Behavioral spot-checks (run by this agent, not trusted from the report)

- Plain + `?retry.wat=1&data=%2Fpath`: `TestProxyPlainSchemeQueryNeverSplit` asserts 500-relay (not 400), 1 transport call, byte-identical RawQuery. Ran verbosely: PASS. The PRD's exact case (formerly `TestProxyPlainSchemeExplicitRetryKeysUnknownStill400` → 400) is inverted and pinned.
- Plain + header: `TestProxyPlainSchemeHeaderPolicyRetries` asserts 200/2 calls/`X-Retry-Count: 2`/query untouched. PASS.
- `TestParsePath` all plain rows → `ModePure` (positional literals now 5 fields). PASS.
- `TestProxyRequestLogCarriesMode` plain row → `mode=pure`. PASS.
- `TestProxyModeChannelConflictMatrix`: 6 rows — plain×no-header 200/1, plain×header 200/1, retry×header 400/0 with body naming both channels/remedy/middleware. PASS.

## 4. Issues found and fixed (self-fix policy)

1. **`proxy_test.go:670` (`TestProxyRedirectNotFollowed`) — report/code mismatch, fixed.** implement-report §2 + deviation 4 list this test as switched to `+retry`, but the code still used the plain path `/http/up.example.com/x`. Per the deviation-4 rationale (mode-independent tests that historically exercised the default retry policy via plain must switch, or retry-mode coverage silently drifts), switched the path to `/http+retry/up.example.com/x`. Assertions untouched (302 relayed verbatim, Location preserved, 1 call). Without this, no test covered redirect-not-followed under retry mode. Test passes.
2. **`TestProxyPlainSchemeQueryNeverSplit` — weakened-vs-twin coverage, fixed.** AC #1 names "no X-Retry-* headers" for the plain form, but the test asserted only status/calls/query — its `+pure` twin (`TestProxyPureModeQueryVerbatimAndSingleCall`) asserts all four X-Retry-* headers absent. Added the same four-header absence loop to the plain test. This also strengthens mutation-m2/m3 capture on the plain path.
3. **`.trellis/spec/backend/retry-proxy-contract.md:81-83` — pre-existing duplicate heading, fixed.** `### Policy: three-tier field-level resolution` appeared twice back-to-back (present in HEAD before this task). Deleted one.

All three fixes re-verified: full gates green (runs 3-4), targeted tests PASS (`TestProxyRedirectNotFollowed`, `TestProxyPlainSchemeQueryNeverSplit`, `TestProxyModeChannelConflictMatrix`, `TestProxyRequestLogCarriesMode`, `TestParsePath`, `TestProxyPlainSchemeHeaderPolicyRetries`).

## 5. Test-policy audit (report §2 claim verification)

Mechanical path-scan of every test listed in implement-report §2 (first `do`/`get`/`doReq`/`NewRequest` path within each function):

- **Switched to `+retry` — confirmed for every listed proxy_test.go test** (TestProxyRetriesStatusSequence, NetworkErrorRetry, NetworkGateClosed, ExhaustedDeliversLastResponse, NetworkExhaustion504, BudgetExhaustion, QueryBytePreservation, DegradedBodyPassthrough, BodyReplayedAcrossRetries, RetryAfterHonored, RetryAfterCappedByBudget, RetryAfterIgnored, DeadConfig400, UnknownRetryKey400, CommitPointNoRetryAfterHeaders, IntegrationRetryAgainstRealUpstream, TTFBTimeoutRetryable (line 911), RetryAfterHTTPDate, RetryAfterPastDate, PerStatusScopeShaping, StatusScopeRaisesLimit, StatusGateClassShorthand, OnlyRetryParams, RacedResponseBodyClosed, BodyContentLengthPreserved, MethodPreserved, DegradedBodyStillSSRFChecked, StrictBodyLimit413, NoRetryWithoutStatusGate, HopByHopStrippedOutbound, HopByHopStrippedInbound, RedirectNotFollowed (after §4 fix), HeadRequest, ClientDisconnectDuringWait, DegradedBodyForwarded). Spot-diffs of the full non-path diff hunks: the only non-path changes are comment updates (docstrings, conflict-matrix row comments) and the three intended test rewrites/deletions — no assertion was deleted or weakened elsewhere.
- **e2e switched list confirmed**: TestE2ERetryToSuccess, ExhaustionDeliversLastResponse, POSTBodyReplay, POSTOversizedDegraded, QueryBytePreservation, UnknownRetryKey400, DeadConfig400, BudgetExhaustion, HappyPathGET all use `+retry` paths.
- **Kept plain, flipped pure**: TestParsePath rows, conflict-matrix plain rows, RequestLogCarriesMode plain row — confirmed.
- **Kept plain, mode-independent**: TestProxySSRFAllowlistGate/PrivateResolutionRejected/IPLiteral (403 before mode matters), TestProxySSEStreamedNotBuffered (streams), TestProxySchemeAndPortNormalization, TestProxyViaAndXFFChainAppend, TestProxyBadTarget400 (parse-error rows), TestProxyRedirectNotFollowed → switched (§4.1, deviation-4 policy), header_test.go buildOutboundHeaders calls (no mode resolution — mode-independent), e2e SSE/SSRF/allowlist/Host rows, TestE2EHeadersStrippedUpstream lookalike leg (plain, pure mode, no conflict — comment updated correctly).
- **No test silently weakened**: the deleted tests (TestProxyDeprecationLogGate, TestProxyPlainSchemeDeprecationDedupeBothTriggers, TestHasRetryKeys) tested deleted machinery — correct deletions, their subjects no longer exist. TestProxyPlainSchemeExplicitRetryKeysUnknownStill400 was rewritten (not deleted) into TestProxyPlainSchemeQueryNeverSplit with the inverted-and-pinned expectation, per PRD. TestProxyPerStatusScopeShaping diff is exactly one path-prefix line — timing assertions untouched (verified by grep of its diff hunk).
- **No dead test code introduced**: `captureLogs`/`logBuf` still used (TestProxyRequestLogCarriesMode); `inRetryNamespace` still used by SplitQuery; no unreferenced helpers remain.

## 6. Mutation-scan residue check

- `git status --porcelain` shows only the eight intended modified files + the task dir. Repo root has no scan artifacts; `grep -rn "reproxy-mutation" *.go` → zero hits.
- Build + full suite green on the working tree (§1) — consistent with "every mutation reverted"; the named capture tests all exist and pass.

## 7. Spec/code/README cross-layer consistency

- Contract §2 signature block matches `target.go` (3-value return, no ExplicitMode) and the deleted `query.go` helper.
- Contract §3 matrix matches `TestProxyModeChannelConflictMatrix` rows exactly.
- README mode table: three channels, plain not a fourth row — deliberate per implement report; the grammar bullet ("plain form: pure mode, same as `+pure`") makes plain≡pure derivable everywhere else. No contradiction found between README, contract, and code on any plain-form behavior.
- `docs/adr/` (single ADR, language choice) — no stale references.

## 8. Verdict

**PASS** (with 2 code fixes + 1 spec fix applied by this check; tag v0.2.1 and CI observation remain main-session commit-phase actions).
