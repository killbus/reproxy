# Check Report — v0.3.0 grammar rewrite

Check agent verification of the implement agent's work against the PRD's 12
acceptance criteria and the specs. Main session had already verified gates +
residue greps; this pass is the deeper AC-by-AC and semantics hunt, plus an
independent re-run of the m4–m7 mutation claims.

## Verdict per acceptance criterion

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | `/https/host?anything` single attempt, query byte-identical, no X-Retry-* | PASS | `TestProxyPlainQueryVerbatimAndSingleCall` (proxy_test.go:1385) — retry.*-shaped keys byte-identical, 1 call, all 4 X-Retry-* absent; `TestProxyPlainSchemeQueryNeverSplit` (:1758); `TestE2EPlainRetryDotParamsReachUpstream` (e2e_test.go:606); `TestProxyPlainNetworkFailureNotRetried` — 504 path emits no X-Retry-*; `TestProxyPlainBodyStreamsNoCapture` — ContentLength framing preserved. Code: proxy.go:223 unconditional `upstreamQuery := r.URL.RawQuery`; commitResponse/exhausted gate on `retryLifecycle`. |
| 2 | `/https+status=5xx;*.attempts=3/host?retry.count=5` retries, query verbatim, X-Retry-* emitted | PASS | `TestProxySegmentPolicyQueryIsTargetData` (proxy_test.go:1172) pins the exact headline sentence (200 after retry, RawQuery == `retry.count=5`, X-Retry-Count: 2); `TestE2ESegmentPolicyPassthroughQueryUntouched` (e2e_test.go:635); `TestProxyQueryBytePreservation` (segment-policy path, %-encoding classes byte-identical). |
| 3 | Segment and header policies produce deep-equal Policy; bijection round-trip locked | PASS | `TestParsePathSegmentHeaderGrammarEquivalence` (target_test.go:198) — identical url.Values; `TestSegmentBracketBijection` (target_test.go:227) — mapper-level for all 6 scope fields × {`*`,100,429,599} + 3 gates, plus 2 full-parse `reflect.DeepEqual` round trips; `TestHeaderTransformEquivalence` (header_test.go:301) — deep-equal Policy. All three exist and assert what implement-report claims. |
| 4 | `+retry`/`+pure`/`+foo` → identical generic unknown-field 400, no legacy branch | PASS | target_test.go:59–62 — all four rows (incl. `+RETRY`) expect `unknown policy field "X"` + same hint. Mechanism: header.go:158–171 — key validated BEFORE the `=`-presence check, so any bare word dies in `queryKeyForSegmentKey` (header.go:228–252) as the generic 400. One code path, no word list, no historical branch. Greps (run independently): `SplitQuery\|ModeRetry\|ModePure` → zero; `legacy`/`v0.2`/word `old` in *.go → zero. The only `+retry`/`+pure` strings in *.go are the no-legacy test rows themselves (target_test.go:59–60). `v0.1.0` mentions (policy.go:259, proxy.go:215, header_test.go:409–430, proxy_test.go:1813) document live Parse(∅) behavior — correct to retain. |
| 5 | Segment × header → 400 conflict | PASS | proxy.go:189–195 — names both carriers, remedy ("remove the header, or drop the policy from the path (e.g. use /https/host)"), middleware actor hint. `TestProxyPolicyChannelConflictMatrix` (4 rows, body asserts all four phrases); `TestE2EPolicyChannelConflictOverTCP`. m5 re-verified (see below). |
| 6 | Segment policy revives capture (cap/413/degraded) — test-pinned | PASS | `TestProxyDegradedBodyPassthrough`, `TestProxyStrictBodyLimit413`, `TestProxyDegradedBodyForwarded`, `TestE2EPOSTOversizedDegraded` all now drive the machinery via segment policies; capture gated on `retryLifecycle` (proxy.go:231), true iff a policy is present (either carrier). |
| 7 | Residue grep `SplitQuery\|ModeRetry\|ModePure` → zero | PASS | Run independently → zero matches; query.go/query_test.go deleted (git status: D). Remaining `mode` word-hits in *.go are degraded/strict/honor modes — unrelated concepts. |
| 8 | README rewritten; withdrawal note; channel-peers updated | PASS (after 1 fix) | Two forms + header presented; "The retry policy" chapter with two carriers, dotted↔bracketed note, mutual exclusion, plain-path semantics; "Stability note" (README.md:362–370) documents the withdrawn promise in prose with "the runtime has no compatibility mode"; old three-channels-peers table + mode grammar deleted. Only `+retry`/`+pure` strings are inside the stability note (history documentation, PRD-permitted). Fix applied: documented whitespace-around-`=` (see findings F-1). |
| 9 | Specs rewritten | NOT DONE — planned | Explicitly out of check scope; gap list below for the main session. |
| 10 | Mutation mini-scan m4–m7 killed | PASS (independently re-verified) | See "Independent mutation re-verification" — all four re-run in my own isolated copy, all killed, capturing tests confirmed. mutation-scan.md claims are accurate. |
| 11 | Gates green ×3; CI green | PASS locally; CI pending push | build/vet/gofmt/test green, re-run 3× after my fixes. Race cannot run on this host (no C compiler — expected, per quality-guidelines); CI Linux job covers it; no structural concurrency change vs v0.2 (carrier switch replaced mode switch, same threading). |
| 12 | Tag v0.3.0 | Main session's job | No git operations performed by the check agent. |

## Independent mutation re-verification (m4–m7)

Isolated copy at `%TEMP%/reproxy-check-mut` (all .go files diff-verified
byte-identical to the working tree before starting; pristine main tree never
touched; copy deleted after).

- **m4** — `parseSchemeSegment` parses the policy body then discards it
  (`_ = params`, returns nil): suite FAIL. Capturers confirmed:
  TestProxyRetriesStatusSequence, TestE2ERetryToSuccess,
  TestProxySegmentPolicyQueryIsTargetData, TestParsePath policy rows,
  TestProxyPolicyChannelConflictMatrix (conflict row turns 200),
  TestE2EPolicyChannelConflictOverTCP. Killed.
- **m5** — segment×header conflict branch replaced with a fallthrough: suite
  FAIL; TestProxyPolicyChannelConflictMatrix and
  TestE2EPolicyChannelConflictOverTCP both red. Killed.
- **m6** — `queryKeyForSegmentKey` maps `*.FIELD` → `retry.FIELD`: suite FAIL;
  bijection/equivalence tests red plus the retry-shaping cascade. Killed.
- **m7** — query split reintroduced on policy paths (strip `retry.`/`retry[`
  keys when a policy is present): suite FAIL;
  TestProxyPlainSchemeHeaderPolicyRetries red with exactly the expected
  message ("upstream query = data=%2Fpath, want retry.count=4&..."). Killed.

4/4 re-verified killed. After each mutation the file was restored from the
main tree; final state diff-verified byte-identical and the isolated copy
deleted.

## Semantics hunting results (dispatch focus items)

1. **EscapedPath / original bytes** — correct. `strings.Cut` on the first
   original-byte `+`; only the scheme part is lowercased. `%2B` and `%2b`
   both die as `unsupported scheme "https%2B..."` (target_test.go:84–85);
   policy body never lowercased (:37 `+STATUS=429` unknown field; :53 values
   never lowercased); a `+` later in the raw path is not policy (:99). A
   second `+` inside the policy body becomes key/value bytes and dies in
   Parse (e.g. `status=5xx+foo` → invalid value 400) — fail-closed holds.
2. **Fail-closed ladder, both carriers** — complete. Segment: bare `+`, empty
   pair, `;;`, leading/trailing `;`, whitespace-only, missing `=`,
   empty key, `=`-in-value (target_test.go:65–75). Header: 13 degenerate rows
   (header_test.go:112–201). Shared `parsePolicyPairList` — one ladder.
3. **Conflict error body** — names both channels, remedy, middleware hint;
   asserted by both matrix tests (see AC5).
4. **Lifecycle coupling** — in sync everywhere. `retryLifecycle` is set iff
   a policy is present; gates capture, X-Retry-* in commitResponse (578) and
   exhausted (622), X-Retry-Dropped under degraded∧lifecycle (581). The
   policy-less 504 path emits no X-Retry-* (TestProxyPlainNetworkFailure-
   NotRetried). SingleAttemptPolicy literal preserved (proxy.go:217, never
   Parse(url.Values{})); discriminators pinned by
   TestProxyPlainSingleAttemptLiteralNotParseDefaults and
   TestSingleAttemptPolicyLiteral.
5. **Log line** — `mode=` deleted (grep `mode=` → zero); `policy=` carries
   segment|header|none (logRequest, proxy.go:705–709);
   TestProxyRequestLogCarriesPolicySource asserts all three values.
6. **SSRF/SSE/plain-path tests kept plain form** — yes: all
   TestProxySSRF*/TestE2ESSRF*/SSE tests use `/http/...` plain paths
   (unchanged from v0.2, where they were already plain), plus new
   TestProxyPlainSSRFStillEnforced / TestE2EPlainSSRFStillEnforced.
7. **TestProxyPerStatusScopeShaping** — diff-verified: only the target URL
   and two comment words changed; body and assertions untouched. 20 isolated
   runs green.
8. **Test-policy mapping (every old behavioral expectation → new-grammar
   test)** — verified by inventory diff. Every v0.2 test maps 1:1 (renamed
   pure→plain / mode→carrier names) plus net-new coverage:
   TestProxySegmentPolicyQueryIsTargetData, TestParsePathSegmentHeaderGrammar-
   Equivalence, TestSegmentBracketBijection, and the extended TestParsePath
   policy rows. The one semantically-obsolete test (old
   TestE2EPureModeRetryURLStillWorks, which pinned query-splitting) was
   correctly replaced by TestE2ESegmentPolicyPassthroughQueryUntouched (the
   old pinned behavior — `retry.*` stripped — is now structurally impossible
   and its inverse is pinned instead).

## Defects found and fixed (self-fix, mechanical only)

- **F-1 (low, test blind spot + doc accuracy): inner-whitespace trimming
  unpinned.** The shared `parsePolicyPairList` trims the key (header.go:172)
  and the value (header.go:195) around `=`, so `status = 429` parses. This is
  a silent grammar widening vs v0.2 (the old header parser trimmed neither —
  `status= 429` produced `retry.status = " 429"` → Parse 400), and no test
  pinned it for either carrier: removing either `strings.TrimSpace` would
  survive the suite. Probed empirically (both carriers accept it), then
  fixed by adding test rows:
  - header_test.go `TestHeaderPairGrammar` — row "whitespace around the
    equals sign" (`status = 429; network = 0`).
  - target_test.go `TestParsePath` — row "policy whitespace around equals"
    (`/https+status = 429;network=0/example.com/x`).
  - README.md "The retry policy" — now documents "optional whitespace around
    pairs and around the `=`" (previously said only "around pairs").
  Gates re-run green ×3 after the fix.

## Issues not fixed (reported for main-session judgment)

- **N-1 (nit, pre-existing): `e2eEnv.doReq` vestigial if/else**
  (e2e_test.go:119–124) — both branches of `if hdr != nil` build the request
  identically. Present in v0.2 (verified via `git show HEAD`), not introduced
  by this task. Cosmetic; fine for a future chore commit.
- **N-2 (nit, doc comment): `validateReproxyNamespace` doc** (header.go:254–258)
  still instructs "callers that take their policy from the scheme segment
  must run it separately" — no such caller exists now (ParseRetryPolicyHeader
  runs on every path, proxy.go:183). The statement is not false, just
  vestigial; left for the spec/comment pass to avoid touching a shared
  helper's docs mid-check.

## Verification results

- `go build ./...` — PASS
- `go vet ./...` — PASS
- `gofmt -l .` — empty
- `go test -count=1 ./...` — ok (run 3× after fixes, incl. once with -count=2)
- `go test -run TestProxyPerStatusScopeShaping -count=20` — ok (flake check)
- `-race` — cannot run on this host (no C compiler; CI Linux job covers it)

## Spec-rewrite input for the main session (AC9)

**retry-proxy-contract.md** (rewrite, do not amend):
- §2 Signatures: delete `SplitQuery`; `ParsePath(escapedPath) (PathTarget,
  url.Values, *RequestError)`; add `parsePolicyPairList(value string, c
  policyCarrier)` + `queryKeyForSegmentKey` / `queryKeyForHeaderKey`;
  `ParseRetryPolicyHeader` unchanged; reword `SingleAttemptPolicy`'s comment
  from "headerless pure mode" to "the no-policy path".
- §3 Grammar: `SCHEME-SEGMENT := SCHEME [ "+" POLICY ]`; scheme
  case-insensitive (lowercased), policy body on original escaped bytes,
  never lowercased. Delete the MODE line, the "query namespace split"
  section, and the mode × channel matrix; new matrix is 4 cells — plain
  (no policy: single attempt, streams, no X-Retry-*), `+POLICY` (segment
  carrier), plain + header (header carrier), `+POLICY` + header (400
  conflict). New "segment policy" subsection: dotted scope spelling
  (`*.FIELD`, `NNN.FIELD`), key shape validated eagerly, the bare-word rule
  (key validated before `=`-presence so any bare word is the generic
  unknown-field 400 — no legacy branch), second-system guardrail note.
- §4 Validation matrix: replace scheme-segment rows (unknown scheme-segment
  form, bare `+`), add segment degenerate ladder (empty pair, missing `=`,
  empty key, `=`-in-value, whitespace-only) and generic unknown-policy-field
  row; update the conflict row (segment × header, remedy "drop the policy
  from the path"); Parse-level rows unchanged (now reached via both
  carriers).
- §5 cases: re-spell to `/https+status=5xx;*.attempts=4;429.attempts=2/...`;
  the `retry.count` collision case now holds on EVERY path (plain and policy
  paths alike).
- §6 Tests Required: delete TestSplitQueryPassthroughBytePreservation (byte
  preservation is now structural — pin via TestProxyQueryBytePreservation /
  TestE2EQueryBytePreservation / TestProxySegmentPolicyQueryIsTargetData);
  mode grammar table → policy grammar table; TestProxyModeChannelConflictMatrix
  → TestProxyPolicyChannelConflictMatrix; pure-mode test names → plain /
  no-policy names; header equivalence tests now segment↔header; add
  TestSegmentBracketBijection, TestParsePathSegmentHeaderGrammarEquivalence,
  TestProxySegmentPolicyQueryIsTargetData, TestProxyRequestLogCarriesPolicySource.
- §7: keep the Parse(∅) wrong-example; reword headerless-pure → policy-less.
- Header/source attribution line: add task 09-07.

**logging-guidelines.md**:
- Event catalog `request` keys: `mode` → `policy` (segment | header | none).
- Rules bullet "mode on request is retry or pure (plain logs pure)" →
  "policy on request names the carrier the policy arrived on (segment |
  header) or none".

**quality-guidelines.md**:
- "Don't: re-encode forwarded query strings" — the do-example calls
  `SplitQuery`, which no longer exists; rewrite the do-side to "forward
  `r.URL.RawQuery` verbatim (there is no split step)" and keep the
  `u.Query().Encode()` don't-side as the anti-pattern.

**index.md** (minor): the Retry Proxy Contract row says "Query namespace" —
reword to "policy carriers".

## Summary

12/12 ACs verified (11 pass; AC9 specs correctly deferred to the main
session with the gap list above). One low-severity defect found and fixed
(F-1: unpinned inner-whitespace grammar widening — 2 test rows + 1 README
line). Two nits reported (N-1 vestigial if/else, pre-existing; N-2 vestigial
doc comment). Mutation claims m4–m7 independently re-verified 4/4 killed in
an isolated copy. Gates green ×3 after fixes; known flake stable at 20 runs.
Ready for the main session's spec rewrite and v0.3.0 tag.
