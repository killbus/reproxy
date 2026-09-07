# Design — v0.4.0 leading policy segment

## Parse flow (before → after)

```
v0.3 ParsePath(rest after "/"):
  schemeSeg := first segment
  scheme, params := parseSchemeSegment(schemeSeg)   // "+" cut inside

v0.4 ParsePath(rest after "/"):
  rest starts with "+"?
    yes → policySeg := rest up to next "/"          // control segment
          params := parsePolicyPairList(policySeg[1:], segmentCarrier)
          rest := after that "/"
    no  → params := nil
  schemeSeg := first segment of remaining rest      // PURE target
  scheme := http|https only (no "+" logic)
  authority, rawPath := rest
```

The leading-segment dispatch is a single byte check at position 1 — the same
one-mechanism class as the v0.3 `+` cut, one cut, no lookahead. The
guardrail moves to the dispatch site.

## Where each piece changes

| Site | v0.3 | v0.4 |
|---|---|---|
| `target.go` usageHint (:42) | `/SCHEME[+POLICY]/AUTHORITY` | `[/+POLICY]/SCHEME/AUTHORITY` |
| `target.go` ParsePath (:75-121) | scheme segment carries both | leading `/+` segment parsed first; scheme segment pure |
| `target.go` parseSchemeSegment (:133-170) | `+` cut, policy via pair grammar | `+` cut deleted; strict http/https table only |
| `header.go` carrierSegment (:46) | "path scheme segment" | "leading policy segment" |
| `proxy.go` conflict 400 (:192-193) | names scheme segment | names leading segment; remedy shape `/+P/https/host` |
| README grammar + carriers | weld form | leading form + rationale (3 lines) + stability note |
| retry-proxy-contract.md | full | full rewrite of grammar/matrix/errors/tests |

## Test surface

Same test families, new spelling for policy-bearing rows:

- `target_test.go` grammar table: `+POLICY` rows become `/+POLICY/https/...`
  rows; v0.3 weld rows become unsupported-scheme death rows; new ladder rows
  (empty `/+`, trailing `/+P` with no scheme, `%2B`-leading, `/+a/+/https`).
- `proxy_test.go` / `e2e_test.go`: policy URLs re-spelled; conflict matrix
  unchanged semantics, new wording assertions.
- `header_test.go` bijection/equivalence: segment spellings re-spelled.
- Mutation scan: m4–m7 equivalents (bare word, conflict, `%2B`, plain→retry)
  plus new ladder rows.

## Version & pins

- Tag **v0.4.0**; supersede semantics as always (v0.3.0 tag never rewritten).
- compose + README image pins: v0.3.0 → v0.4.0 (first image-shipping tag).
- Release notes: grammar change headline; v0.3 policy URLs die as unsupported
  scheme; pure form unchanged.

## Rollout / rollback

- Single commit carries grammar + tests + docs + spec + pins. Tag after CI.
- Rollback = revert the commit; the v0.3 tag still exists. No data, no state.
