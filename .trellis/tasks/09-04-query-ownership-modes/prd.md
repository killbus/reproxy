# PRD: Query ownership — explicit-borrow modes and header channel

## Problem

reproxy's control plane lives in the request query. `SplitQuery` claims every key starting with `retry.` or `retry[` (query.go `inRetryNamespace`), and unknown keys in that namespace are a hard 400 (fail-closed per audit item). But the query's semantic owner is the **target URL**: the upstream's own API may legitimately use `retry.`-prefixed parameters (`retry.count`, `retry.token` — `retry` is a common English word). Today that upstream is **unreachable through reproxy**, with no clean escape:

- Hard 400: upstream uses `retry.something` reproxy doesn't recognize → every request through reproxy fails.
- Silent hijack (rarer): upstream uses a key that happens to parse (`retry.status` with a status-like value) → reproxy consumes it as policy; the upstream never sees its own parameter; the client gets an unrequested retry policy.

An encoded escape hatch exists (`retry%2Ecount` passes through byte-wise) but is undocumented, and only works for upstreams that decode query keys.

The audit research (protocol-design-analysis.md §1.3) analyzed proxy-side extensibility of the namespace but never the reverse direction: upstream data shaped like proxy protocol.

**Rejected mitigation**: a `__retry__` (dunder) prefix. It lowers collision probability but does not fix ownership — the query would still be claimed territory, and a second reserved-word family doubles namespace complexity (the audit pinned "single prefix, no more reserved words"). General-purpose positioning means we cannot bet on collision probability.

## Requirement statement

Make query ownership **explicit** rather than assumed:

- **Default (pure mode)**: the query belongs to the target. Zero parsing, zero namespace claims, byte-identical pass-through. Retries are OFF in this mode unless requested through the header channel.
- **Borrowed (retry mode, URL channel)**: client explicitly declares the borrow in the path scheme segment (reproxy's own territory — the `/https/` whitelist). The query splits per v0.1.0 semantics; `retry.*`/`retry[` keys are claimed as today.
- **Header channel**: policy carried in `X-Reproxy-*` request headers. Query is never split regardless of mode. For programmatic clients that can set headers.

v0.1.0 compatibility: every existing `?retry.status=...` request must keep working verbatim in retry mode. No v0.1.0 URL may change behavior **silently** — see R5 for the one deliberate, documented exception.

## Requirements

### R1: Path scheme-segment mode variants

Extend the R1 path grammar. The first path segment (the scheme, today `http`/`https`) gains mode-qualified forms. Exact literal syntax is a **design.md decision** (candidates: suffix forms like `https!retry`, or a leading mode segment like `/r/https/...`); the PRD pins only:

- A plain `http`/`https` scheme segment selects **pure mode** (query = target data, verbatim).
- A mode-qualified form of the same scheme selects **retry mode** (query splits per v0.1.0).
- Both forms normalize to the same upstream scheme; mode is orthogonal to destination.
- Invalid/malformed mode forms are 400 with the usage hint naming the expected shapes (fail closed, consistent with R1 strictness).
- SSRF L1–L7 semantics identical across modes: allowlist, resolve-then-pin, forbidden-IP checks, no-redirect-follow all apply unchanged.

### R2: Pure mode semantics

- `SplitQuery` is not called; `RawQuery` passes to the upstream byte-identical (existing passthrough guarantee, now unconditional).
- Pure mode means pure proxy: no retries at all (single attempt — a literal, never the `Parse(∅)` defaults, per design D13); requesting retries without the URL channel requires the header channel (R4). Per-try TTFB timeout still applies (it bounds a hang, not a retry).
- **Body capture is absent in pure mode** (design §0 amendment, ratified by adversarial panel review 2026-09-04): capture exists solely to replay across attempts; single attempt ⇒ no replay ⇒ no 10 MiB cap, no 413, no degraded mode, no `X-Retry-Dropped`. The body streams to the upstream. Exception: a header policy (R4) re-enables capture because the requested policy requires replay (design D14).
- Response headers `X-Retry-Count`/`X-Retry-Limit`/`X-Retry-Exhausted` are not emitted (no retry lifecycle to report).

### R3: Retry mode semantics (URL channel)

- v0.1.0 behavior exactly: `retry.*`/`retry[` claimed, unknown keys 400, byte-preserving passthrough of the remainder, all gates/scope resolution/server clamps unchanged.
- The mode variant appears in request logs (`event=request` gains a `mode=` key) so operators can see channel mix.

### R4: Header channel (`X-Reproxy-*`)

- Headers are the policy carrier; the query is never split even in retry mode (the channels are mutually exclusive — using both is a 400 naming the conflict, fail closed).
- Serialization format is a **design.md decision** (candidates: compact `key=value; key=value` string mirroring the query grammar, or JSON). PRD pins: covers the same field set as the query channel (gates + scope fields), same validation matrix, same 400 bodies.
- Headers are stripped before forwarding upstream (reproxy↔client protocol, like hop-by-hop headers).
- Header policy + pure mode = the intended collision-free combination: query untouched, policy out-of-band.
- `X-Reproxy-*` is reserved as a namespace: unknown `X-Reproxy-Foo` is a 400 (same fail-closed principle).

### R5: v0.1.0 migration behavior (documented transition)

Today `/https/example.com/x?retry.status=5xx` retries; after this task the same URL in **pure mode** (plain scheme segment) would not. To avoid silent behavior change:

- **Transitional default**: the plain scheme segment keeps selecting retry mode for one minor version (v0.2), with a log line `event=deprecation` on every such request urging migration to the mode-qualified form. The deprecation gate covers both explicit dependents (retry keys present) and implicit ones (default network retry consumed) — design §5. Pure mode in v0.2 is reachable via an explicit opt-in (`+pure`) and via the header channel.
- **Terminal state (v0.3)**: plain segment = pure mode. Flip criteria per design D15 (version + time window, operator-side metric); v0.3 ships a legacy env-var escape hatch, removed in v0.4.
- README gains a migration section; the deprecation window and end state are pinned there.
- (If the user prefers an immediate clean cut with no transition window, that is a valid PRD amendment — flag it in design review rather than deciding unilaterally.)

### R6: Observability

- `event=request` logs the channel/mode.
- `event=deprecation` logs transitional retry-mode-via-plain-scheme usage (R5).
- Error bodies for mode/channel conflicts name both the offending element and the remedy (which channel to use instead).

### R7: Documentation

README: three-channel model (pure URL / retry URL / header), the ownership principle ("the query belongs to the target; the proxy borrows it only when told to"), migration section, and removal of the implicit-claim language. The `retry%2E` encoded escape hatch becomes a documented footnote (it still works in retry mode for upstreams that decode keys).

## Constraints

- Go stdlib only (ADR-0001).
- All validation fail-closed; error bodies follow the `{error, hint}` JSON contract.
- General-purpose positioning: collision-freedom must be structural, not probabilistic; no scenario-based justification for defaults or scope.
- Mutation-locking: each pinned semantic in R1–R6 gets a regression test, and the final suite must demonstrably fail when each is broken (anti-self-certification, per the established mutation-scan method).
- v0.1.0 requests that never touch `retry.*` keys must behave byte-identically in pure mode — with one observable difference: default `retry.network=1` retried network failures in v0.1.0, and pure mode does not retry them (no retry lifecycle at all). This is the R5 transition's reason to exist and must be covered by its tests.

## Acceptance Criteria

- [ ] R1: mode-qualified path forms parse/normalize/reject per spec; malformed forms 400 with usage hint; unit tests cover the full table (valid forms, each invalid form, scheme defaults, IPv6 brackets, raw-path preservation).
- [ ] R2: pure mode passes RawQuery byte-identical (property/table tests incl. `%`, `+`, valueless, duplicate, `retry.`-prefixed target data); no retry attempts occur (single upstream call asserted); no X-Retry-* response headers; no body capture (large body streams without 413/cap).
- [ ] R2: upstream `retry.count` parameters in pure mode reach the upstream verbatim (the headline collision case, asserted end-to-end).
- [ ] R3: retry mode passes the v0.1.0 test suite unchanged (existing tests green without modification, except where R5 transition logging is asserted).
- [ ] R4: header channel covers the full field set with the same validation matrix (table-driven tests mirroring policy_test.go); headers stripped upstream; both-channels-used is a 400; unknown X-Reproxy-* is a 400.
- [ ] R4: header channel + pure mode e2e: policy from headers, query untouched, retries observable.
- [ ] R5: transitional default logs `event=deprecation`; migration documented in README; behavior matrix (plain segment × retry keys × header presence) pinned by tests.
- [ ] R6: log lines carry mode/channel; conflict errors name the remedy.
- [ ] R7: README documents the three channels, ownership principle, migration, and the encoded escape hatch.
- [ ] Gates: build/vet/gofmt clean; suite green ×3 consecutive; CI matrix green (Linux race included).
- [ ] Mutation scan: every pinned semantic above is demonstrably locked (isolated-copy method per the task-dir convention; compile failures don't count as captures).
