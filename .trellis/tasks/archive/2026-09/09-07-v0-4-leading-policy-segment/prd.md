# PRD — v0.4.0: leading +POLICY control segment, scheme returns to pure target

## Origin

Chatroom round 8 (2026-09-07), a debt-settlement round. The owner rejected the
v0.3 grammar shape ("scheme 本质上是 target 的一部分" — the policy welded onto the
scheme token mixes an adverb into a noun). R8 re-adjudicated policy placement
with the steel-man protocol (F-1..F-3) and converged on the **leading policy
segment**. R7's defense of the weld was struck down with it (straw-man kill of
the sigil-prefixed leading segment; the SQLAlchemy false-cognate diagnosed but
left untreated).

## The grammar change

```
v0.3 (dies):  /https+status=5xx;*.attempts=3/host/path?query
v0.4 (new):   /+status=5xx;*.attempts=3/https/host/path?query
pure form:    /https/host/path?query      (byte-for-byte unchanged)
header:      X-Reproxy-Retry-Policy: ...  (unchanged, second carrier)
```

- **Leading segment** `/+POLICY` is a control token: reproxy's own surface.
  The scheme segment follows and is now a **pure target** — it reports only
  the destination protocol, nothing else.
- Pure form (`/https/host/...`) is unchanged byte-for-byte and behaviorally.
- Reading order becomes adverb-first: "with this policy, fetch this URL".

## Requirements

R1. **Grammar.** `PROXY-TARGET := ["/+" POLICY] "/" SCHEME "/" AUTHORITY
    ["/" RAW-PATH]`. The leading `/+POLICY` segment, when present, is parsed
    with the shared pair grammar (`parsePolicyPairList`, `segmentCarrier`) on
    original bytes — same validation matrix, same 400 bodies, dotted scope
    spellings, eager key validation (bare-word rule inherited unchanged).

R2. **Scheme segment is pure target.** `parseSchemeSegment` loses the `+` cut:
    the segment is exactly `http`/`https` (case-insensitive, lowercased).
    Anything else — including the v0.3 spellings `https+status=5xx`,
    `https+retry`, `https+pure` — dies as `unsupported scheme` quoting the raw
    segment. **No legacy detection** (third application of the v0.2 knife).
    The hint text drops "optionally with a + retry policy" (target.go:152).

R3. **Error ladder.** Inherited rows unchanged (fail-closed first-segment
    check, empty authority, port/host validation, original-bytes `%2B` fence).
    New rows:
    - `/+statusx=5xx/https/h` → unknown policy field 400 (shared ladder)
    - `/+/https/h`, `/+` → 400 naming the policy grammar (empty control
      segment, degenerate — absent is the only no-policy spelling)
    - `/+status=5xx` (no scheme follows) → 400 missing upstream scheme
    - `/+a/+/https/h` → `unsupported scheme "+"` 400 (second `+`-segment is
      parsed as a scheme)
    - `/%2Bstatus=5xx/https/h` → byte 1 is not `+` → parsed as scheme →
      unsupported scheme `"%2Bstatus=5xx"` 400
    - `/https/host/+x` → **target path, forwarded byte-for-byte** — `+` is
      control only at segment position 1
    - `/https+status=5xx/h` → unsupported scheme `"https+status=5xx"` 400
      (v0.3 death shape)
    - `/+P/https/h` + header → conflict 400 (message names the leading
      segment and the new remedy shape)

R4. **Carriers.** Both carriers keep the one-grammar / total bijection
    (leading segment dotted scopes ↔ header bracketed scopes). Conflict rule
    and wording update to name the leading segment; bijection/conflict/
    unknown-`X-Reproxy-*` rules unchanged. `carrierSegment` rename: "path
    scheme segment" → "leading policy segment".

R5. **usageHint / README / guardrail rewrite.** `usageHint` (target.go:42)
    shows the new shape. README: grammar line, path-segment description
    ("rides the leading segment"), carriers table rows, examples — the same
    set of places v0.3.0 touched, plus the three-line rationale (R7-3,
    reworded): *reproxy owns exactly one URL surface — the leading `/+`-
    prefixed segment (when present); the scheme segment is pure target; the
    `+` slot speaks only retry policy.* The stability note (README:427-435)
    gains the v0.4 sentence. **Second-system guardrail** (doc comment
    at the parse site): rewrite — "the leading `+` segment speaks only retry
    policy; the scheme segment is pure target. v0.3 welded policy onto the
    scheme token; that grammar died in v0.4 as an unsupported scheme."

R5a. **Spec rewrite.** `.trellis/spec/backend/retry-proxy-contract.md` §2
    signatures (ParsePath unchanged shape), §3 path grammar block, carrier
    section, carrier matrix, §4 error matrix, §5 good/base/bad cases, §6 test
    list — all to the new grammar; wrong/correct examples reworded.

R6. **Behavioral invariants (all inherited, must survive untouched):**
    query unconditionally upstream-owned (byte-identical, `retry.*` = target
    data); D13 literal single attempt; D14 capture iff policy present;
    conflict 400 (segment vs header); reserved `X-Reproxy-*` namespace;
    mutation discipline.

R7. **Tag v0.4.0.** Standard lifecycle: implement → check → spec update →
    commit → tag → push → CI → Release notes. v0.4.0 is the **first
    image-shipping tag**: compose/README image pins move v0.3.0 → v0.4.0 in
    this task (the standing item lands here).

## Constraints

- Zero users, zero counterparties — no deprecation window (fourth breaking
  change applied on this basis).
- Go stdlib only; flat single package.
- No legacy detection anywhere: no mode-word list, no historical branches, no
  old-version references in error text beyond the guardrail's one historical
  sentence.
- Mutation discipline: the error ladder gets mutation-locked test rows; scan
  on isolated copy; compile failure ≠ capture.
- The pure form is byte-stable: any test where `/https/host` behavior changes
  is a bug, not a migration.

## Acceptance Criteria

1. `parseSchemeSegment` contains no `+` logic; the leading `/+` segment is
   parsed before scheme, through `parsePolicyPairList(segmentCarrier)`.
2. Every R3 ladder row has a test that fails when its behavior is broken.
3. `TestParsePathSegmentHeaderGrammarEquivalence`,
   `TestSegmentBracketBijection`, `TestHeaderTransformEquivalence`,
   `TestProxyPolicyChannelConflictMatrix`,
   `TestE2EPolicyChannelConflictOverTCP` pass against the new grammar.
4. The v0.3 death shape (`/https+status=5xx/host`) has an explicit test row
   (unsupported scheme, quoting the raw segment).
5. Pure-form test rows for behavior: existing plain-path tests unchanged
   except where they exercise policy URLs (switched to the leading-segment
   spelling, same expectations).
6. README + spec rewritten per R5/R5a; compose/README image pins at v0.4.0.
7. Mutation scan (isolated copy, revert + diff-verify): all mutants killed.
8. CI green both OS + Linux race; tag v0.4.0 pushed; Release created.

## Kill list

- No mode axis in any spelling or position (R8 killed `/retry+https/` with
  the same knife).
- No `@` sigil (userinfo resonance, buys nothing `+` lacks).
- No parsing of multiple policy segments (`/+A//+B/https/h` — the second
  `/+` dies as unsupported scheme `+`; one control segment only).
- No new error-message taxonomy: the ladder inherits existing categories,
  new rows reuse them (unsupported scheme / unknown field / degenerate).
- No v0.3 compatibility of any kind.

## Future work (out of scope)

- Cargo-culting any further grammar furniture: `+` slot vocabulary is closed
  (retry policy only), and the R8 form is final until a real user signal
  arrives. Build only on demand.
