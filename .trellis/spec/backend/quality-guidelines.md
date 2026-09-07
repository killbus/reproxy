# reproxy Quality Guidelines

> Testing standards and forbidden patterns for this codebase.
> Source: task 09-04 — check round findings (F-3/F-4/F-6/F-10) and the mutation-scan method.

---

## Testing Requirements

### Gates

`go build ./...`, `go vet ./...`, `gofmt -l .` (empty output), `go test ./...` — all must pass before any commit.

### Test hardening rules (lessons from the check round)

1. **Assert outcomes, not calls.** A test that asserts an upstream was called N times but never checks what body it received is vacuous (Batch 3's degraded-mode body drop passed exactly such a test). Byte-level equality assertions for anything forwarded.
2. **No dead test code.** Unused helpers, unreferenced variables, and assertions on values never read (F-3/F-4/F-6) all inflate the illusion of coverage. Delete them.
3. **Upper-bound timing on Windows needs ~500ms headroom.** Handler-pipeline overhead (even fully mocked) varies 4–68ms on this host; waits of 1–2ms cannot be asserted with a 100ms upper bound. Prefer a **lower-bound assertion as the discriminator** (e.g. elapsed >= the expected wait) and keep the upper bound generous (500ms). Mutation-caught semantics should never depend on an upper bound — "faster" mutations are invisible to them.
4. **Order independence must be tested with iteration.** One-shot table tests pass even when behavior depends on map iteration order; loop 50+ shuffled iterations (`TestParseFieldOrderIndependence`).
5. **Race-sensitive paths get deterministic regression tests** even when `-race` can't run on the host (no C compiler): force the racing branch to win with a controlled response in flight (`TestProxyRacedResponseBodyClosed`).

### Anti-self-certification (自证闭环) — mutation scanning

A green suite proves nothing until it is shown to be able to go red. Before finishing a task that pins semantics:

- For each pinned behavior, make a minimal mutation (one line) that breaks it, run the full suite in an **isolated copy** (never the main tree while another agent holds it), and assert FAIL.
- **A compile failure is not a capture** — re-run with a valid-syntax mutation.
- Restore and diff-verify byte equality after each mutation.
- Record the matrix (behavior, mutation, capturing test) in the task dir (`mutation-scan.md`).
- 0 survivors required; any survivor is a test blind spot that must be filled before finishing.

## Forbidden Patterns

### Don't: re-encode forwarded query strings

```go
// Don't — regenerating RawQuery re-encodes and breaks signed URLs
q := u.Query(); ...; u.RawQuery = q.Encode()
```

```go
// Do — forward the raw query string byte-identical, always (v0.3.0: the
// query is unconditionally upstream-owned; there is no split)
upstreamQuery := r.URL.RawQuery
```

**Why**: `url.Values.Encode()` normalizes encoding and ordering; signed URLs (HMAC over the query) then fail upstream.

### Don't: validate cross-field constraints per field

```go
// Don't — depends on which field the map yields last
case "max": if sp.Max < sp.Initial { return bad }
```

```go
// Do — validate once after the scope is fully assembled
validateScopeCrossFields(sp, scopeName)
```

**Why**: Go map iteration order is random; per-field cross-checks make legal configs randomly 400.

### Don't: context.WithTimeout around a streaming round trip

**Why**: it cancels the body read too — long-lived streams (SSE) die after headers arrive. Use a goroutine + timer + select for TTFB only, and drain-and-close the raced response in the losing branches.

## Observability Convention

Every retry-path response carries `X-Retry-Count` / `X-Retry-Limit` / `X-Retry-Exhausted`; degraded bodies add `X-Retry-Dropped`. Every wait logs its computed duration. New failure modes must extend this set, not invent parallel mechanisms.
