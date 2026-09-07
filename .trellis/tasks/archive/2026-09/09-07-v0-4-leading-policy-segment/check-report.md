# Check report — v0.4.0 leading policy segment

Adversarial full-scope check against the PRD before commit/tag. Incremental
record: each section appended as it completes.

## H. Suite state (baseline)

- `go vet ./...` — clean (exit 0).
- `gofmt -l .` — empty.
- `go test ./... -count=1` — ok (reproxy 5.001s, cmd/reproxy 3.882s).
- Case count: 405 `=== RUN` entries (matches implement-report's 396 -> 405).

Safety deviation (noted for the main session): the implementation is
UNSTAGED, so `git checkout -- <file>` would revert to v0.3 and destroy the
v0.4 work. All breaking-test reverts below restore from a byte-identical
backup copy and are verified with `cmp` + `git diff` equality against the
pre-mutation state. No git commit was made; the index was never touched.

## B. Pure-form byte-stability — PASS

Method: backed up the 11 changed files (byte copies), `git stash push` the 8
Go files (code + tests) to restore the committed v0.3 tree, ran the full
suite, popped the stash, and `cmp`-verified all 8 files byte-identical to the
backup afterward. README/spec/compose changes were left in place (they do
not affect compilation or test behavior).

- v0.3 tree (HEAD): `go test ./... -count=1` green — 174 top-level `--- PASS`
  entries, 0 FAIL.
- v0.4 tree (restored): 177 top-level `--- PASS`, 0 FAIL.
- Set difference (sorted test names): ZERO tests present in v0.3 and absent
  in v0.4 (no regression). Only additions:
  - TestE2EV03DeathShape400
  - TestE2EPlusInTargetPathForwardedVerbatim
  - TestProxyPlusLeadingPathSegmentIsTargetData
- git diff review of target_test.go / proxy_test.go / e2e_test.go confirms
  plain-path rows (`/https/host/...`, ports, IPv4/IPv6, DNS, userinfo) are
  byte-identical; only policy-bearing rows were re-spelled. The two
  `/https/host/+x`-family rows were ADDED under "Valid simple targets", not
  modified.
- Note: the v0.3 test table row `/ftp+status=5xx` (unknown scheme with
  policy weld) was deleted. Its mechanism survives through equivalent rows:
  `/https+status=5xx/h` and `/https+retry/h` (the v0.3 death shapes) — same
  code path (parseSchemeSegment strict table), so no coverage hole.

Section A error-ladder test-row mapping (static verification, all rows found):
- `/+statusx=5xx/https/h` -> target_test.go "unknown policy field in segment".
- `/+/https/h`, `/+` -> target_test.go "empty policy after plus" /
  "empty policy alone"; proxy_test.go TestProxyBadTarget400 `/+/https/...`.
- `/+status=5xx` (no scheme) -> target_test.go "policy without scheme
  follows" + "policy with no scheme after slash"; TestProxyBadTarget400
  `/+status=5xx`.
- `/+a/+/https/h` -> target_test.go "second plus segment is a scheme".
- `/%2Bstatus=5xx/https/h` -> target_test.go "escaped plus leading is a
  scheme"; TestProxyBadTarget400 `%2B` row.
- `/https/host/+x` -> target_test.go "plus-leading path segment is target
  data" + "plus-leading deep path segments"; proxy_test.go
  TestProxyPlusLeadingPathSegmentIsTargetData; e2e_test.go
  TestE2EPlusInTargetPathForwardedVerbatim.
- `/https+status=5xx/h` -> target_test.go "v0.3 death shape" rows (2);
  TestProxyBadTarget400 (2 rows); TestE2EV03DeathShape400 (over TCP, asserts
  `unsupported scheme "https+status=5xx"` in the JSON body).
- conflict row -> TestProxyPolicyChannelConflictMatrix +
  TestE2EPolicyChannelConflictOverTCP (wording: "leading policy segment").

## A. R3 ladder — live mini-mutation verification (the two subtle rows) — PASS

Both mutations made in the working tree, tested, restored from the
byte-identical backup (/tmp/target.go.orig), and cmp-verified (sha256
39a46ff... matched after each restore; full `go test .` green after each
restore).

Mini-mutation 1 (row: `/https/host/+x` forwarded verbatim — `+` is control
ONLY at position 1): replaced the position-1 `strings.HasPrefix(rest, "+")`
dispatch with a greedy `strings.Index(rest, "+")` dispatch (any `+` before
the scheme becomes control; equivalent in spirit to the implement agent's
m7 but my own cut). Compiles clean (go vet 0).
- Captures: 10 total failures (4 top-level + 6 subtest rows):
  - TestParsePath: v0.3_death_shape_unsupported_scheme,
    v0.3_death_shape_bare_word, encoded_bytes_preserved,
    plus_in_raw_path_is_not_policy, plus-leading_path_segment_is_target_data,
    plus-leading_deep_path_segments_are_target_data
  - TestE2EPlusInTargetPathForwardedVerbatim,
    TestE2EV03DeathShape400, TestProxyPlusLeadingPathSegmentIsTargetData
- Verdict: KILLED. Both `/https/host/+x` rows fire plus the TCP-level
  verbatim-forward test and (as expected) the v0.3 death-shape rows.

Mini-mutation 2 (row: `/+a/+/https/h` -> `unsupported scheme "+"` — the
second `+`-segment dies as a scheme): corrupted the strict table so the bare
control word "+" is accepted as a scheme (lower == "+" -> "http"), breaking
the ladder's parse-as-scheme step.
- Captures: 2 failures — TestParsePath/second_plus_segment_is_a_scheme
  (top-level TestParsePath FAIL + the specific subtest row).
- Verdict: KILLED. The exact R3 row is test-locked at the ParsePath layer.
  (No proxy/e2e row pins this specific ladder outcome — acceptable: the
  ParsePath row is the contract location, and TestProxyBadTarget400 rows
  cover the same code path end-to-end for other spellings.)

## D. Declared deviation: policy validation AFTER parseSchemeSegment

(a) Every R3 row under this ordering (traced against target.go:80-175):
- `/+statusx=5xx/https/h`: control segment cut, policy deferred; scheme
  "https" validates; then parsePolicyPairList fires unknown-field 400. HOLDS.
- `/+/https/h`, `/+`: the empty-body branch (TrimSpace == "") fires the
  shared ladder's empty-body 400 BEFORE the scheme is ever parsed. HOLDS
  (see section G for the hole analysis of this construct).
- `/+status=5xx` (no scheme): the empty-body check does not fire (body
  non-empty), the rest=="" check fires "missing upstream scheme". HOLDS.
- `/+a/+/https/h`: first control segment cut (policySeg "a", pendingPolicy),
  second segment "+" goes to parseSchemeSegment -> unsupported scheme "+"
  fires BEFORE the deferred policy parse, so "a" is never named. HOLDS —
  this is exactly the R3 requirement.
- `/%2Bstatus=5xx/https/h`: byte 1 is "%" not "+", no control segment, the
  literal segment dies in the scheme table. HOLDS.
- `/https/host/+x`: no control segment (byte 1 "h"), "+" data. HOLDS.
- `/https+status=5xx/h`: no control segment, weld fails the table. HOLDS.
- Conflict row: proxy.go fires after both carriers parse — unaffected by
  ParsePath internal ordering. HOLDS.
Note: for `/+a/ftp/h`-shaped inputs (policy junk + unknown scheme), the
scheme error now fires BEFORE the policy error. v0.3's behavior for the
analogous `/ftp+status=5xx/...` was also scheme-first, so ladder precedence
is genuinely inherited, not new.

(b) Shared-ladder 400 bodies for POLICY CONTENT errors are unchanged from
v0.3 by construction: parsePolicyPairList and queryKeyForSegmentKey are
byte-identical (git diff of header.go touches only comments and the
carrierSegment constant). The only body-text change in the pair-grammar
layer is the carrier NAME inside existing templates: "path scheme segment"
-> "leading policy segment" (sanctioned by R4). Error text diff for
target.go/proxy.go/header.go vs v0.3 (git diff of Reason/Hint literals):
- target.go usageHint: re-spelled to the new grammar shape (R5 sanctions).
- target.go parseSchemeSegment: dropped ", optionally with a + retry
  policy" from the unsupported-scheme reason (R2 sanctions the hint-text
  change; PRD explicitly names target.go:152).
- target.go NEW reason: "missing upstream scheme in path" (R3 row sanctions
  a missing-scheme 400; the reason wording is new but the PRD names the
  row's outcome "400 missing upstream scheme").
- proxy.go conflict Reason/Hint: "scheme segment" -> "leading policy
  segment", remedy unchanged shape (R4 sanctions; wording names leading
  segment + remedy `/https/host` + middleware actor hint).
No other new error text exists.

(c) Unsanctioned error text: grep of all Reason/Hint literals in
target.go, proxy.go, header.go (full list inspected — 30+ literals) shows
only the categories above as changed. Everything else (ports, IPv4/IPv6,
DNS, userinfo, namespace, multiple-header, empty-pair, unknown-field) is
byte-identical to v0.3.

## E. No legacy detection — PASS

grep of *.go for legacy|deprecated|v0.2|v0.3|mode word:
- Non-test Go error paths: ZERO hits. The only "deprecated" hit is
  ssrf.go:26 — an RFC comment about 6to4 relay anycast (pre-existing,
  unrelated to reproxy grammar).
- v0.3 references in *.go live ONLY in: target.go doc comments (the
  guardrail's sanctioned historical sentence, ParsePath doc, and
  parseSchemeSegment doc — all comments, not error text) and test
  names/comments (death-shape rows). No old spellings in any Reason/Hint.
- No mode-word list, no "retry"/"pure" special-casing in non-test code:
  the only occurrence of the words "retry", "pure" as quoted words is
  header.go:224, a doc comment explaining the bare-word rule.
- grep for `"retry"` / `"pure"` as string literals in non-test code: none
  (the strings that exist are "retry" log-event tags and url.Values key
  prefixes, all pre-existing v0.3 behavior).

## F. Docs & pins — PASS

README.md (verified via git diff + grep):
- Intro paragraph: leading `/+`-prefixed segment wording present.
- Quick-start curl: `/+status=500,502-504;*.attempts=3/https/example.com/api`.
- Grammar line: `[/+<policy>]/<scheme>/<host>[:port]/<path>?<query>` (line ~103).
- Policy-rides-the-leading-segment bullet + v0.3 death shape documented
  (README:113-116) + `%2B` fence re-spelled for the first segment
  (README:125-128) + `+` control-only-at-position-1 (README:129-130).
- Three-line rationale present as a bulleted "Three properties define
  reproxy's URL surface" block (README:131-136): owns exactly one URL
  surface / scheme segment pure target / `+` slot speaks only retry policy.
- Carriers section: both examples re-spelled; "Leading path segment" bullet;
  conflict wording names the leading path segment (README:167-169, 217-221).
- Stability note: v0.3→v0.4 sentence present (README:463-470) — weld died as
  unsupported scheme, plain form byte-stable since v0.1.0.
- Remaining `/https+` spellings in README: lines 114, 116, 464 — ALL inside
  the death-shape / stability-note contexts (documenting what dies), which
  the PRD sanctions. No live usage examples carry the weld spelling.

Pins:
- docker-compose.yml:16 -> ghcr.io/killbus/reproxy:v0.4.0. VERIFIED.
- README compose example (line 45), version-tag list (line 58), docker-run
  --version example (line 68) -> v0.4.0. VERIFIED.
- Dockerfile:4 build-arg example already v0.4.0 (from the docker-smoke
  task). CI (docker-publish.yml) derives the version from the pushed tag —
  no hardcoded version to bump (grep confirms no v0.3.0 residue anywhere).

retry-proxy-contract.md (verified via git diff, full read):
- Source line gains the 09-07-v0-4 task with 10/10 mutations. Matches
  mutation-scan.md.
- §2 signatures: ParsePath unchanged shape, doc says nil = no /+POLICY
  segment — matches code (target.go returns nil policyParams when no
  control segment; the ONLY no-policy spelling).
- §3 grammar block `["/" "+" POLICY] "/" SCHEME ...` — matches ParsePath.
  Ladder-precedence note matches the implemented ordering (scheme before
  policy). Position-1 rule matches the HasPrefix check. Guardrail block is
  the R5 rewrite (v0.3 death sentence included).
- Carrier section and matrix rows re-spelled to `/+POLICY`; conflict row
  names the leading policy segment — matches proxy.go.
- §4 error matrix: the four new R3 rows present (unknown field in segment,
  empty control segment, missing scheme, second +-segment, target-path +
  data); v0.3-weld row updated to name the weld spellings.
- §5 good/base/bad: re-spelled; v0.3 death-shape Bad row and
  `/+a/+/https/host` Bad row present.
- §6 test list: m1–m10, death-shape rows, `/+`-in-target-path rows — all
  exist as actual tests (verified in section A/B).
- §7 wrong/correct: new "restoring a + cut inside the scheme segment" wrong
  example; new leading-dispatch correct example — matches the actual code
  shape (HasPrefix + parseSchemeSegment).
- Spec-vs-code check on one subtle claim: spec says "second + -shaped
  segment dies as unsupported scheme "+"" — matches target.go behavior
  (pendingPolicy deferred until after parseSchemeSegment). No
  spec-vs-code drift found.

## G. Empty-policy-segment construct (target.go:102-111) — SOUND, style note only

The construct:

```go
if strings.TrimSpace(policySeg) == "" {
    if _, rerr := parsePolicyPairList(policySeg, segmentCarrier); rerr != nil {
        return PathTarget{}, nil, rerr
    }
}
```

Claim to verify: "the ladder always errors for an empty body, so the call
is an error-or-impossible form".

Trace of parsePolicyPairList (header.go:141-147): the FIRST statement is
`trimmed := strings.TrimSpace(value); if trimmed == "" { return 400 ... }`.
The guard and the callee's error condition are the SAME predicate
(TrimSpace(v) == ""), evaluated on the SAME string. The call is therefore
a tautology — rerr is non-nil for every input that reaches it.

Is there any input where the construct silently falls through?
- `policySeg` reaching the guard is bounded: rest[1:i] cut at the first "/"
  (or rest[1:] when no "/" follows). It is never nil (string).
- Live probe (temporary test, deleted after): `/+`, `/+/https/h`, `/+ `,
  `/+  /https/h`, `/+<tab>/https/h`, `/+ /`, `/+/` — ALL return the
  "policy in the leading policy segment is present but empty" 400. None
  fall through to pendingPolicy.
- Escaped whitespace (`/+%20%20/https/h`) is NOT trimmed (original bytes):
  it falls to the scheme check, survives, then dies as `unknown policy
  field "%20%20"` — the intended fail-closed outcome (and pinned by the
  existing "escaped whitespace is not whitespace" test row).
- `/+status=5xx` (no scheme): empty-body branch does not fire; the
  rest=="" check returns "missing upstream scheme". Intended R3 row.
- Ordering detail verified: for `/+/` (empty body + no scheme after the
  slash), the empty-body 400 fires BEFORE the missing-scheme check —
  correct precedence (degenerate policy named first).

No hole found. Style observation (not fixed, zero behavioral impact): the
construct could be simplified to an unconditional error return
(`return PathTarget{}, nil, &RequestError{...}`) or the callee's error
could be asserted; the current form calls a function whose error is
provably always non-nil on this path, which reads as a lint-level
"error-or-impossible" idiom and is documented as such at the call site.
The implement agent's comment ("it always errors for an empty body, so the
call is an error-or-impossible form") is accurate.

## C. Mutation scan re-verification (from scratch, isolated copy)

Scan environment: %TEMP%\reproxy-check-mut (robocopy of the repo working
tree, excluding .git and .trellis). Each mutant below was applied to the
COPY, `go vet` had to pass (compile failure is NOT a capture), `go test
. -count=1 -timeout 120s` run, failures counted, then the file restored
from the pristine snapshot and `cmp`-verified byte-identical. After the
last revert, the copy's full suite ran green.

Re-verified mutants (my own cuts, not the implement agent's exact syntax
where avoidable):

- **m1** — leading-`+` dispatch disabled (`if false &&` guard on the
  HasPrefix). vet 0. CAPTURED: 77 failures (top-level TestE2EHappyPathGET,
  TestE2ERetryToSuccess, TestE2EExhaustionDeliversLastResponse, conflict
  matrix, every policy-bearing e2e/proxy row). (Implement agent reported 46;
  my count includes subtest rows — same verdict.)
- **m2** — v0.3 `+` cut restored inside parseSchemeSegment
  (`strings.Cut(segment, "+")` before the strict table). vet 0. CAPTURED:
  6 failures — TestParsePath/{second_plus_segment_is_a_scheme,
  v0.3_death_shape_unsupported_scheme, v0.3_death_shape_bare_word},
  TestProxyBadTarget400, TestE2EV03DeathShape400. Matches the implement
  agent's 3 top-level (mine adds subtest rows).
- **m5** — `%2B` decoded before the dispatch
  (`strings.ReplaceAll(rest, "%2B", "+")`). vet 0. CAPTURED: 5 failures —
  TestParsePath/{escaped_plus_leading_is_a_scheme, escaped_plus_is_not_a_
  policy}, TestProxyBadTarget400, TestE2EPlusInTargetPathForwardedVerbatim.
- **m10** — query re-encoded (`r.URL.Query().Encode()` instead of RawQuery
  pass-through, in proxy.go). vet 0. CAPTURED: 7 failures —
  TestProxyQueryBytePreservation, TestE2EQueryBytePreservation,
  TestProxyPlainQueryVerbatimAndSingleCall, TestE2EPlainRetryDotParams
  ReachUpstream, TestE2EPlainHeaderPolicy,
  TestProxyPlainSchemeQueryNeverSplit, TestProxyPlainSchemeHeaderPolicy
  Retries. Matches the implement agent's 7 exactly.

Revert + diff-verify: every mutant file restored from the pristine
snapshot and `cmp`-verified byte-identical; after the last revert the
copy's full suite ran green (5.111s) and all copy files were cmp-verified
byte-identical to the working tree. Compile failure did not occur for any
of my four cuts (each vet 0).

Verdict: 4/4 re-verified mutants KILLED. The implement agent's 10/10 claim
is consistent with everything I could reproduce independently; I did not
re-run m3/m4/m6-m9 (bounded scope per dispatch; m4 conflict and m6 D13
behavior are separately pinned by the passing conflict-matrix and
plain-path tests I ran in section B, and m3's bare-word row was
live-verified in section A static mapping + the equivalence tests).

## Items fixed during this check

None — no defects found that required code changes. The implementation
matches the PRD on every checked axis. Two non-blocking observations were
recorded instead of fixed (section G style note; section A note that the
`/+a/+/https/h` row is pinned at the ParsePath layer only, which is where
the contract locates it).

## Final state

- `go vet ./...` clean; `gofmt -l .` empty; `go test ./... -count=1` green
  (reproxy 5.372s, cmd/reproxy 4.058s), 405 cases.
- All 11 changed files cmp-verified byte-identical to the check-start
  snapshot — this check mutated nothing permanently (probe test deleted,
  scan copy is in %TEMP% and disposable).
- No git commit was made; the index was never touched; no stash remains
  (popped and verified).

## Verdict: READY-FOR-TAG

Not verified locally (main session's scope): tag v0.4.0 creation, push,
CI runs (both OS + Linux race), GitHub Release. Everything verifiable
before the commit is green.

Non-blocking observations (recorded, not fixed):
1. target.go:102-111 — the empty-segment construct calls a function whose
   error is provably always non-nil on that path (tautology); the inline
   comment documents it correctly. Zero behavioral impact.
2. The `/+a/+/https/h` ladder row is pinned at the ParsePath layer only
   (no proxy/e2e row for that exact spelling); the same code path is
   covered end-to-end by other TestProxyBadTarget400 rows. Acceptable.
