# PRD: v0.3.0 grammar rewrite — segment policy, delete mode axis

## Problem

The v0.2 grammar carries two artifacts of incomplete understanding, both paid for in real costs:

1. **The mode axis (`+retry`/`+pure`)** encodes query ownership as a declared prefix. But `+retry`'s declared ownership ("reproxy claims the `retry.*` namespace in the query") puts control data on a surface the upstream has claims on. The collision disease (an upstream's own `retry.status` silently swallowed and stripped, zero warning) is structural, not incidental. The scheme segment — the only URL surface reproxy fully owns and never forwards upstream — is where control data belongs.
2. **The mode words are a vocabulary where a rule suffices.** "Policy present → retry; absent → single attempt" is one derivation. `plain`/`+pure` were already two spellings of one fact; `+retry` was a spelling for "policy in query" which is the wrong location.

A five-round design review (chatroom: Fielding/Hickey/Brooks/Christensen/Cunningham roles; rounds 1–4 verdicts superseded where contradicted) converged with the owner's own reading ("https+策略 即第一性又自然干净"): the zero-mode grammar is the first-principles form. The v0.2 "stable forever" promise for `+retry` was made to zero users and is withdrawn — a promise to nobody is mispriced, not sacred.

## The new grammar

```
/https/host/path?query                            → pass-through, single attempt
/https+POLICY/host/path?query                     → retry per POLICY; query untouched
/https/host/path?query + X-Reproxy-Retry-Policy   → retry per header policy
```

POLICY (appended to the scheme with `+`, `;`-separated pairs):

```
/https+status=5xx;*.attempts=3;429.attempts=5/example.com/v1/x?retry.count=5
```

- **Query is unconditionally upstream-owned.** Byte-identical forwarding always, including `retry.*`-shaped keys. `SplitQuery` and the entire `retry.*` query namespace are deleted. Signed-URL byte preservation goes from test-guaranteed to structurally true.
- **Policy grammar = the existing header grammar** (`key=value` `;`-separated, whitespace-trimmed) feeding the same `Parse()` via a shared pure transform. One grammar, two carriers (segment, header).
- **Dotted scopes in the segment**: `*.attempts`, `429.attempts` (brackets are gen-delims, illegal in path segments). Header keeps its bracket spelling. The dotted↔bracketed mapping is a total bijection, round-trip-locked by tests.
- **Original bytes**: `%2B` is not `+` (existing precedent extends); policy body matched on original escaped bytes; scheme case-insensitive (lowercased), policy body never lowercased.
- **Fail-closed ladder extends**: bare `+`, empty pair, `;;`, whitespace-only, missing `=`, empty key → 400. Absent is the only no-policy spelling.
- **Conflict**: segment policy + `X-Reproxy-Retry-Policy` header → 400 (names both channels, remedy, middleware actor hint).
- **Lifecycle**: policy present (either carrier) ⇒ retry lifecycle exists ⇒ X-Retry-* headers emitted ⇒ body capture revives (10 MiB cap, 413 strict, degraded passthrough, X-Retry-Dropped) — identical to current +pure+header semantics.
- **No legacy detection.** `/https+retry/host` dies as "unknown policy field: retry" — the same generic 400 as `/https+foo/host`. No mode-word blacklist, no historical special-casing, no error text mentioning old/legacy/v0.2. The grammar is forward-looking only.
- **Second-system guardrail** (doc comment at the parse site): the `+` slot speaks only retry policy. Nothing else may ever live there.

## Constraints

- Go stdlib only; fail-closed semantics preserved everywhere.
- The header channel is RETAINED (distinct employer: sending-end middleware/SDK, machine-set policy). Not up for re-litigation in this task.
- Test policy: every behavioral expectation of the old grammar maps to an equivalent new-grammar test (retry semantics via segment policy; plain-path single-attempt/SSRF/SSE tests keep plain form). `TestProxyPerStatusScopeShaping` (known timing flake) must not be modified beyond its path/param migration to the new grammar.
- Mutation discipline: mini-scan on the changed semantics in an isolated copy outside the repo, targets m4–m7 (see Tests), revert + diff-verify each.

## Acceptance Criteria

- [ ] `/https/host?anything` = single attempt, query byte-identical, no X-Retry-* headers (pure pass-through).
- [ ] `/https+status=5xx;*.attempts=3/host?retry.count=5` retries per policy; `retry.count=5` reaches the upstream verbatim; X-Retry-* emitted.
- [ ] Segment policy and header policy produce deep-equal `Policy` for a representative policy (one-grammar lock; bijection dotted↔bracketed round-trip locked).
- [ ] `/https+retry/host` and `/https+pure/host` and `/https+foo/host` → identical generic unknown-field 400 (no legacy branch — grep for legacy/old/v0.2 mentions in *.go returns zero).
- [ ] Segment × header both present → 400 conflict.
- [ ] Segment policy revives capture (cap/413/degraded) — test-pinned.
- [ ] Old-grammar residue: `grep -rnE "SplitQuery|ModeRetry|ModePure" --include="*.go" .` → zero.
- [ ] README: grammar rewritten; "stable forever" withdrawal note in prose (history documentation, not runtime); channel-peers paragraph updated to two carriers + plain.
- [ ] Specs rewritten: retry-proxy-contract.md (grammar, matrix: 4 cells — plain / +policy / header / conflict; validation matrix; signatures — SplitQuery removed, segment parse signature added), logging-guidelines.md (mode= line: mode concept deleted; replace with policy source or drop per implement report).
- [ ] Mutation mini-scan m4–m7 killed, report appended in task dir.
- [ ] Gates green ×3 (build/vet/gofmt/test); CI green (both OS + Linux race).
- [ ] New tag **v0.3.0** (not a fixup into v0.2.1 — phantom deletion and query-channel debt settlement are two understandings from different times; separate tags keep the ledger honest). v0.2.1 stays in history.

## Rollout

Single task, single commit + tag v0.3.0. Zero backward compatibility, no deprecation window — there are no counterparties. Implementation is dispatched; check agent verifies; main session tags.
