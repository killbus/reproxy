# Mutation mini-scan — no phantom migration (3/3 killed)

Task: 09-06-no-phantom-migration. Method per `.trellis/spec/backend/quality-guidelines.md` (anti-self-certification): isolated copy in a temp dir OUTSIDE the repo (`/tmp/reproxy-mutation-scan`, seeded from `git archive HEAD` + working-tree sources copied in, baseline verified green), one mutation applied, full suite run, FAIL asserted, mutation reverted, diff-verified byte-identical to the source tree, next. The main tree was never touched.

Baseline: `go build ./... && go test . -count=1` → ok, before m1 and re-verified after all reverts (final rerun ok).

## m1 — plain branch returns ModeRetry (the old behavior)

- File: `target.go`, `parseSchemeSegment` plain branch: `return scheme, ModePure, nil` → `return scheme, ModeRetry, nil`.
- Result: **KILLED** (5 tests red):
  - `TestParsePath` (all plain-form rows assert `ModePure`)
  - `TestProxyModeChannelConflictMatrix` ("plain, no header" 200/1-call; "plain, header policy" 200/1-call)
  - `TestProxyRequestLogCarriesMode` (plain row wants `mode=pure`)
  - `TestProxyPlainSchemeQueryNeverSplit` (query would split → 400 on `retry.wat`, and X-Retry lifecycle appears)
  - `TestProxyPlainSchemeHeaderPolicyRetries` (plain+header would 400 as a conflict)
- Reverted; `diff target.go` vs source: byte-identical.

## m2 — plain + policy header → conflict 400 (the old conflict restored)

- File: `proxy.go`, `ModePure` branch: inserted a `policyHeaderPresent(r.Header)` conflict-400 gate ahead of `ParseRetryPolicyHeader` (the old v0.2 behavior where the plain form selected retry mode and the header clashed).
- Result: **KILLED** (8 tests red), the plain+header capture among them:
  - `TestProxyPlainSchemeHeaderPolicyRetries` (plain + header must be 200 with 2 calls, X-Retry-Count: 2 — written this task to pin this row)
  - `TestProxyModeChannelConflictMatrix` ("plain, header policy" row = 200)
  - `TestProxyPureModeHeaderPolicyRetries`, `TestProxyPureModeHeaderPolicyCapturesBody`, `TestProxyPureModeHeaderPolicyBodyReplayed` (+pure + header combo, same code path)
  - `TestE2EPureModeHeaderPolicy`, `TestE2EHeadersStrippedUpstream`, `TestE2EPureModeHeaderPolicyBodyReplayE2E` (e2e side of the same path)
- Reverted; `diff proxy.go` vs source: byte-identical.

## m3 — plain query split happens (SplitQuery called on the pure/plain path)

- File: `proxy.go`, `ModePure` branch: replaced the verbatim `upstreamQuery = r.URL.RawQuery` with a `SplitQuery(r.URL.RawQuery)` call (plain and `+pure` share the pure-mode code path, so this is the query-split-on-plain-path mutation).
- Result: **KILLED** (8 tests red), the designated capture among them:
  - `TestProxyPlainSchemeQueryNeverSplit` (plain form's `retry.wat=1` would be claimed/400 instead of reaching the upstream verbatim)
  - `TestProxyPlainSchemeHeaderPolicyRetries` (query no longer verbatim)
  - `TestProxyPureModeQueryVerbatimAndSingleCall`, `TestProxyPureModeHeaderPolicyRetries` (+pure side of the same path)
  - `TestE2EPureModeRetryDotParamsReachUpstream`, `TestE2EPureModeHeaderPolicy`, `TestE2EPureModeHeaderPolicyBodyReplayE2E`, `TestE2EHeadersStrippedUpstream`
- Reverted; `diff proxy.go` vs source: byte-identical.

## Verdict

3/3 killed, 0 survivors. No test blind spot on the three changed semantics (plain-resolves-pure, plain+header-is-not-conflict, plain-query-never-split).
