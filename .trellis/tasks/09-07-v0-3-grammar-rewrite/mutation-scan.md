# Mutation Mini-Scan — v0.3.0 grammar rewrite (m4–m7)

Purpose: anti-self-certification — prove the suite **can fail** on the four
changed-semantics targets (each mutation deliberately breaks one pinned
behavior of the new grammar; the suite must go red), not merely that it is
green on the honest tree.

Method: isolated copy at `%TEMP%/reproxy-mut` (Windows), verified
byte-identical to the working tree's Go files (`diff -r`, excluding
non-Go/non-task directories) before starting; a `pristine/` twin was kept for
revert + diff-verify after each mutation. Each mutation is a minimal
one-place edit, compiled before counting, full suite `go test -count=1
./...` per mutation, assert FAIL. The main tree was never touched.

Judgment rules (from `.trellis/spec/backend/quality-guidelines.md`):

- **Killed (red)**: compiles and the suite FAILs — the pinned behavior is
  locked.
- **Survived (green)**: suite still ok — a test blind spot, reported as a
  finding.
- A compile failure is not a capture — the first m4 attempt used `_, rerr :=
` which failed with `no new variables on left side of :=`; it was replaced
with a valid-syntax variant (`params` parsed then discarded via `_ = params`,
returning nil) and re-run. Recorded per the rules.

## Matrix

| # | Pinned behavior | Mutation | Result | Capturing tests (selection) |
|---|---|---|---|---|
| m4 | Segment policy is parsed and takes effect (PRD AC: `/https+POLICY/host` retries per policy) | `parseSchemeSegment` parses the policy body, discards the result, returns nil params (segment policy silently treated as plain) | Killed (red) — 25+ tests | `TestProxyRetriesStatusSequence`, `TestProxyPerStatusScopeShaping`, `TestE2ERetryToSuccess`, `TestE2ESegmentPolicyPassthroughQueryUntouched`, `TestHeaderTransformEquivalence`, `TestParsePath` policy rows, `TestProxyPolicyChannelConflictMatrix` (conflict row turns 200), `TestE2EPolicyChannelConflictOverTCP` |
| m5 | Segment × header conflict is a 400 (PRD AC: both carriers → conflict) | The `case segmentParams != nil && headerParams != nil:` conflict branch deleted from the carrier switch (both present falls through to segment-only, no 400) | Killed (red) | `TestProxyPolicyChannelConflictMatrix` (segment+header row), `TestE2EPolicyChannelConflictOverTCP` |
| m6 | Dotted ↔ bracketed scope bijection (PRD AC: segment and header produce deep-equal Policy; total bijection round-trip locked) | `queryKeyForSegmentKey` maps `*.FIELD` to `retry.FIELD` instead of `retry[*].FIELD` | Killed (red) | `TestSegmentBracketBijection` (mapper + round-trip rows), `TestParsePathSegmentHeaderGrammarEquivalence`, `TestParsePath` `policy_full_https` row; plus the whole retry-suite red cascade (every `*.FIELD` scope shape is now unknown to Parse: `TestProxyRetriesStatusSequence`, `TestE2ERetryToSuccess`, …) |
| m7 | Query is unconditionally upstream-owned (PRD AC: query byte-identical ALWAYS, including on policy paths) | Policy path reintroduces query splitting: `retry.`/`retry[`-prefixed keys stripped from `upstreamQuery` when either carrier supplied a policy | Killed (red) | `TestProxyPlainHeaderPolicyRetries`, `TestProxyPlainSchemeHeaderPolicyRetries`, `TestE2EPlainHeaderPolicy` (each asserts a `retry.count=N` target-data key reaches the upstream verbatim under a header policy) |

**4 mutations: 4 killed, 0 survived, 0 compile failures counted as captures.**

## Notes

- m4 first attempt (`_, rerr := parsePolicyPairList(...)`) was a compile
  failure — not counted; re-run with valid syntax.
- m6's failure mode is informative: mapping `*.FIELD` → `retry.FIELD`
  (instead of `retry[*].FIELD`) produces a key Parse rejects as `unknown
  retry parameter` on every default-scope policy, so both the bijection lock
  and the entire retry-shaping suite go red — the mapping is load-bearing in
  both directions (equivalence AND behavior).
- m7's capturers are the policy-present query-verbatim tests (header-policy
  paths with `retry.count` target data). The policy-less plain-path verbatim
  tests (`TestProxyPlainQueryVerbatimAndSingleCall`,
  `TestE2EPlainRetryDotParamsReachUpstream`) do NOT catch m7 by construction —
  the mutation only splits on policy paths, and those tests carry no policy.
  That split is exactly the PRD's headline property (query is target data on
  EVERY path, including policy paths), and it is the policy-present tests
  that pin it.
- After each mutation the file was restored from `pristine/` and diff-verified
  byte-identical; after the final mutation the whole isolated tree was
  diff-verified identical to pristine, the suite re-run green
  (`go test -count=1 ./...` → ok), and both isolated copies deleted. The main
  tree was never modified.
