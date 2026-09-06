# PRD: Remove phantom migration — plain scheme resolves to pure

## Problem

The v0.2→v0.3 transition machinery (transitional plain→retry default, deprecation gate, legacy escape-hatch plan) was designed for deployers of a released product. **reproxy has no releases in the wild and no users beyond the developer.** v0.2.0 was tagged but never deployed anywhere the transition window could protect. The entire migration layer is complexity bought for an audience that does not exist:

- `event=deprecation` dual-trigger gate (retry keys OR consumed network retry)
- `hasRetryKeys` inert scan
- `newDeprecationLogger` + `onNetworkRetry` callback threading through `attemptLoop`
- `ExplicitMode` field (its ONLY consumer is the deprecation gate)
- The planned `REPROXY_LEGACY_PLAIN_RETRY` env var and the v0.3 flip task itself

This is the same class of error as the general-purpose positioning constraint, but on the time axis: "general-purpose product" was read as "has a user base mid-migration."

## Requirement

Delete the phantom transition. The plain scheme segment resolves to **pure mode** as the terminal state, effective now.

- `parseSchemeSegment`: plain form → `ModePure`, remove the `TODO(v0.3)` marker and transitional comments.
- Delete `hasRetryKeys` (query.go), `newDeprecationLogger`, the `onNetworkRetry` parameter and its call site in `attemptLoop` (proxy.go), and the `deprecation` log event.
- Remove `ExplicitMode` from `PathTarget`: its only consumer was the deprecation gate. With plain ≡ `+pure`, nothing distinguishes plain from explicit anymore (retry is distinguished by Mode itself).
- Mode×channel matrix simplifies: the plain-scheme rows become pure-mode rows (plain + header → header channel with pure mode, NOT a conflict — plain no longer selects retry mode).
- Tests: plain rows flip from retry assertions to pure assertions; deprecation tests (`TestProxyDeprecationLogGate`, `TestProxyPlainSchemeDeprecationDedupeBothTriggers`, `TestHasRetryKeys`, the `TestProxyRequestLogCarriesMode` plain row's mode value, conflict-matrix plain rows, e2e conflict plain row) update or delete accordingly. `TestProxyPlainSchemeExplicitRetryKeysUnknownStill400` becomes: plain scheme never splits the query, so `?retry.wat=1` is target data now reaching the upstream verbatim.
- README: delete the Migration section; the plain-form bullet becomes "pure mode (same as `+pure`)"; remove deprecation-log cross-references; the v0.1.0 network-retry-difference callout stays but loses the migration framing.
- Spec: retry-proxy-contract.md (mode grammar: plain → pure terminal; matrix rows; deprecation gate section removed), logging-guidelines.md (remove `deprecation` event + its rules).

## Constraints

- Go stdlib only; fail-closed semantics unchanged for everything except the plain-scheme default.
- v0.1.0 test-suite compatibility is NO LONGER an AC (that compatibility was itself part of the phantom): plain-scheme rows are expected to change behavior. `+retry` rows must stay behavior-identical (that channel is stable-forever and real).
- Mutation discipline: after the flip, re-run a mini mutation scan on the changed semantics (plain-resolves-pure, no-deprecation-machinery-left, plain+header-is-not-conflict) — 3 targets minimum, isolated copy.
- Gates: build/vet/gofmt/test ×3; CI both OS + Linux race.

## Acceptance Criteria

- [ ] `/https/host?anything` behaves exactly like `/https+pure/host?anything` (query verbatim incl. retry.*-shaped data, body streams, single attempt, no X-Retry-* headers).
- [ ] `/https/host` + `X-Reproxy-Retry-Policy` header = header channel with pure mode (retries per policy), NOT a 400 conflict.
- [ ] `+retry` behavior unchanged (all `+retry`-path semantics tests green).
- [ ] No deprecation machinery remains: `grep -rE "deprecation|hasRetryKeys|onNetworkRetry|ExplicitMode|LEGACY_PLAIN"` over *.go returns zero hits.
- [ ] `ExplicitMode` removed from `PathTarget`.
- [ ] README has no Migration section, no v0.3 references, no REPROXY_LEGACY_PLAIN_RETRY mention.
- [ ] Specs updated (contract + logging).
- [ ] Mini mutation scan: 3/3 killed, report in task dir.
- [ ] Gates green ×3; CI green.
- [ ] Version: tag v0.2.1 (v0.2.0's tag message describes transitional semantics that no longer describe master; the new tag supersedes it. v0.2.0 stays in history — rewriting tags is worse than a superseding tag.)

## Rollout

Single task, single commit (plus archive). Tag v0.2.1. No transition, no escape hatch — there is no audience for either.
