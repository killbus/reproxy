# Implement Report — v0.3.0 grammar rewrite

Task: segment policy, delete mode axis. PRD: `prd.md` in this dir.

## Plan (units of work, appended as each completes)

1. Shared policy grammar: refactor `header.go` pair-list parse into a shared pure transform; add the segment key mapper (dotted ↔ bracketed bijection).
2. `target.go`: delete `Mode`/`ModeRetry`/`ModePure`; `ParsePath` returns `(PathTarget, url.Values, *RequestError)` — the segment policy is parsed and validated eagerly.
3. Delete `query.go` + `query_test.go`; move `gateKeys`/`scopeFields`/`isDigits` into `policy.go`.
4. `proxy.go`: unconditional query pass-through; segment × header conflict 400; lifecycle = policy present (either carrier); `policy=segment|header|none` on the request log line (replaces `mode=`).
5. Test migration: `target_test.go`, `header_test.go`, `proxy_test.go`, `e2e_test.go`.
6. README rewrite.
7. Mutation mini-scan m4–m7 in an isolated copy outside the repo.

## Key design decisions

- **ParsePath signature**: `ParsePath(escapedPath string) (PathTarget, url.Values, *RequestError)`.
  The second return is the parsed segment policy (`nil` = no `+POLICY` suffix). Rationale:
  `PathTarget` stays a pure destination record (comparable with `==`, so the grammar table
  tests keep direct equality), and control-plane data does not ride on the destination
  struct. The policy body is validated inside `ParsePath` (left-to-right: scheme → policy →
  authority), so every path-level 400 fires before any body/SSRF work.
- **Shared pair grammar**: `parsePolicyPairList(value string, c policyCarrier)` in `header.go`
  (refactored out of `parseRetryPolicyHeaderValue`). One grammar (`;`-separated `key=value`,
  whitespace-trimmed, values never contain `;`/`=`), two carriers, each supplying a
  `queryKeyFor` mapper. The header mapper is total (unknown keys deferred to `Parse`, as
  today — header behavior and error bodies unchanged); the segment mapper validates key
  shape eagerly (gate | dotted scope).
- **Key validation order** (the no-legacy rule): inside the shared pair loop the KEY is
  validated before the `=`-presence check. A bare word (`/https+retry/host`,
  `/https+pure/host`, `/https+foo/host`) therefore dies as the *generic*
  `unknown policy field "retry|pure|foo"` 400 — one code path, no mode-word list, no
  historical branch. A *recognized* key missing its `=` (e.g. `/https+status/host`)
  still hits the fail-closed ladder's missing-`=` 400.
- **Dotted ↔ bracketed mapping** (total bijection): segment `*.FIELD` → `retry[*].FIELD`,
  `NNN.FIELD` → `retry[NNN].FIELD`, gates map as-is (`status` → `retry.status`); the header
  keeps `[*].FIELD` / `[NNN].FIELD` (brackets are legal in header values, illegal in path
  segments). Both mappers land on the same `retry.*` keys, so both carriers feed the same
  `Parse()` validation matrix — locked by an equivalence test plus a bijection test.
- **Case handling**: the scheme part of the segment is lowercased (case-insensitive);
  the policy body after `+` is matched on original escaped bytes and never lowercased —
  keys are case-sensitive exactly as in the header channel (`/HTTPS+status=5xx/` works,
  `/HTTPS+STATUS=5XX/` is an unknown policy field).
- **Lifecycle**: policy present (either carrier) ⇒ retry lifecycle ⇒ X-Retry-* ⇒ body
  capture. No policy ⇒ `SingleAttemptPolicy` literal (kept, unchanged semantics).
- **Log line**: `mode=` deleted; `request` events now carry `policy=segment|header|none`
  (the channel-mix observable survives, now naming the policy source).

## Progress log

- [x] Plan recorded; starting parse refactor.
- [x] `header.go` rewritten: `parsePolicyPairList(value, carrier)` is the shared pure
  transform; carriers differ only in `queryKeyFor`. Header mapper total (behavior +
  error bodies preserved); segment mapper validates key shape eagerly (gates via
  `gateKeys`, dotted scopes `*`/3-digit). Key validated BEFORE the `=` check so bare
  words die as the generic unknown-field 400. `scopeFields` hint updated to dotted form
  in segment errors. NOTE: `gateKeys` currently lives in `query.go` (to be deleted); it
  will move to `policy.go` in the next step.
- [x] `target.go` rewritten: `Mode` field and `ModeRetry`/`ModePure` constants deleted;
  `ParsePath(escapedPath) (PathTarget, url.Values, *RequestError)`; `parseSchemeSegment`
  cuts on the FIRST original-byte `+`, lowercases only the scheme part, parses the policy
  body via the shared `parsePolicyPairList` with `segmentCarrier`. Second-system
  guardrail doc comment at the parse site. `usageHint` rewritten for the two shapes.
- [x] `query.go` + `query_test.go` deleted; `gateKeys`/`scopeFields`/`isDigits` moved
  into `policy.go`. Policy docs updated (SplitQuery references removed).
- [x] `proxy.go` rewired (unit 4 complete): `ParseRetryPolicyHeader` runs on EVERY path
  (the X-Reproxy-* namespace guard fires regardless of carrier); carrier switch
  (`both` → conflict 400 naming both channels + remedy + middleware actor hint;
  `segment` → Parse; `header` → Parse; neither → `SingleAttemptPolicy` literal);
  `upstreamQuery := r.URL.RawQuery` unconditional; `policySource` (segment|header|none)
  threaded through `attemptLoop`/`exhausted` into `logRequest` — the request log line is
  now `policy=` (mode concept deleted). `policyHeaderPresent` helper deleted (the parse
  itself is the presence check now). Remaining mode-word comments rewritten.
  Build green (`go build ./...`); tests now fail on the old grammar as expected —
  test migration is next.
- [x] Test migration complete (unit 5): all four suites on the new grammar.
  - `target_test.go`: `TestParsePath` table extended with a `wantPolicy` column —
    policy-grammar rows (gates, dotted scopes, case rules, values never lowercased),
    bare-word rows (`+retry`/`+pure`/`+foo`/`+RETRY` → generic unknown-field 400),
    fail-closed ladder rows (missing `=`, empty body, `;;`, leading/trailing `;`,
    empty key, `=`-in-value, non-digit scope), escaped-plus edges, plus
    `TestParsePathSegmentHeaderGrammarEquivalence` and `TestSegmentBracketBijection`
    (mapper-level for all scope fields/gates + full-parse Policy deep-equal round trips).
    Fixes applied this session: whitespace-only policy needs a literal-space body
    (`%20%20` is original bytes, dies as unknown field); `+attempt=3` dies as the
    segment 400 `unknown policy field` (eager validation — Parse's "unknown retry
    parameter" wording is header-carrier-only); `/https+42bogus` dies on the policy
    before the authority check; bijection round-trip scope must be gated (dead config).
  - `proxy_test.go`: every `retry.*`/`retry[*].*` query migrated into segment
    policies (`/http+status=500;*.attempts=2/...`); no-policy cases → plain form;
    pure-mode tests renamed and re-aimed at their new-grammar carriers
    (`TestProxyPlainQueryVerbatimAndSingleCall`, `TestProxyPlainNoRetryOnStatusGate`,
    `TestProxyPlainNetworkFailureNotRetried`, `TestProxyPlainBodyStreamsNoCapture`,
    `TestProxyPlainSingleAttemptLiteralNotParseDefaults`, `TestProxyPlainSSRFStillEnforced`,
    header-channel tests renamed to `TestProxyPlainHeaderPolicy*`,
    `TestProxyPolicyChannelConflictMatrix` now a 4-row segment×header matrix with
    the new conflict body assertions, `TestProxyRequestLogCarriesPolicySource`
    asserts `policy=segment|header|none`).
  - `e2e_test.go`: same URL migration; `TestProxyPerStatusScopeShaping`-style timing
    test untouched beyond its URL; pure-mode e2e tests renamed to plain/carrier names
    (`TestE2EPlainRetryDotParamsReachUpstream`, `TestE2ESegmentPolicyPassthroughQueryUntouched`,
    `TestE2EPlainHeaderPolicy*`, `TestE2EPolicyChannelConflictOverTCP`,
    `TestE2EPlainSSRFStillEnforced`); `TestE2EHappyPathGET` carries a segment policy
    (its `X-Retry-Count: 1` assertion requires a lifecycle); unknown-key/dead-config
    body assertions updated to the segment error spellings.
  - `header_test.go`: equivalence tests now compare a `ParsePath` segment policy
    against the header carrier (deep-equal Policies, identical url.Values).
  - Residue greps: `SplitQuery|ModeRetry|ModePure` → zero; `legacy` → zero
    (comments reworded to "bare-word rule"); `v0.2` → zero. Pre-existing `v0.1.0`
    mentions (Parse(∅) default documentation, the D13 rationale) retained — they
    document live behavior, not the retired grammar.
  Gates green: `go build ./...`, `go vet ./...`, `gofmt -l .` (empty),
  `go test -count=1 ./...`.
- [x] README rewritten (unit 6): "How it works" now presents the two forms
  (plain + `+POLICY`) with the query unconditionally target-owned; the mode
  chapter, mode table, mode-suffix grammar, and retry/pure semantics sections
  are replaced by "The retry policy" (shared grammar, two carriers, dotted ↔
  bracketed spelling note, mutual exclusion, plain-path semantics incl. no
  capture / no X-Retry-* / SSRF identical) and the header-channel section;
  the retry-parameter table is re-spelled in segment form (`status`,
  `*.attempts`, `NNN.<field>`) with a bracketed-spelling note for the header;
  the response-headers section says lifecycle = policy present and documents
  `policy=segment|header|none` on the log line; HTTP conformance, security,
  flags, and error tables updated (capture applies "whenever a policy is
  present"); a "Stability note" records the withdrawn stable-forever promise
  in prose (history documentation, not runtime); zero mode words, no
  legacy/old/v0.2 runtime text.
- [x] Mutation mini-scan m4–m7 complete (unit 7): isolated copy at
  `%TEMP%/reproxy-mut` (Go files diff-verified byte-identical to the working
  tree first; pristine twin kept for revert + diff-verify; main tree never
  touched; both copies deleted after). Results — m4 (segment policy silently
  dropped after parse) killed by 25+ tests; m5 (conflict check deleted) killed
  by `TestProxyPolicyChannelConflictMatrix` + `TestE2EPolicyChannelConflictOverTCP`;
  m6 (`*.FIELD` → `retry.FIELD` instead of `retry[*].FIELD`) killed by the
  bijection/equivalence locks plus the full retry-shaping cascade; m7 (query
  split reintroduced on policy paths) killed by the three policy-present
  query-verbatim tests. One compile failure (m4 first attempt) re-run with
  valid syntax per the quality-guidelines rule. 4/4 killed, 0 survived.
  Full matrix and method notes: `mutation-scan.md` in this dir.
- [x] Post-mutation sweep: scrubbed retired-spelling words from code comments
  (target.go / header.go bare-word comments now name no specific words);
  re-ran residue greps — `SplitQuery|ModeRetry|ModePure` zero, `legacy` zero,
  `v0.2` zero (the only `+retry`/`+pure` strings left in *.go are the
  PRD-mandated no-legacy test rows themselves). Added
  `TestProxySegmentPolicyQueryIsTargetData` pinning the PRD's headline AC
  sentence (`/https+status=5xx;*.attempts=3/host?retry.count=5` — retry per
  policy, `retry.count=5` upstream verbatim, X-Retry-* emitted), which was
  previously covered only via the header carrier.

## Final state

All gates green on the main tree: `go build ./...`, `go vet ./...`,
`gofmt -l .` (empty output), `go test -count=1 ./...` (ok, ~5s).
Files changed: `header.go`, `target.go`, `policy.go`, `proxy.go`,
`README.md`, `header_test.go`, `target_test.go`, `proxy_test.go`,
`e2e_test.go`, `policy_test.go`; deleted: `query.go`, `query_test.go`.
Task-dir artifacts: this report, `mutation-scan.md`. No commits, tags, or
`.trellis/spec/**` edits (main session's job).
