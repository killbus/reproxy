# Design: Query ownership — explicit-borrow modes and header channel

Parent PRD: `prd.md` (this file resolves every "design.md decision" flagged there).

---

## 0. PRD amendment flagged by design review (needs user sign-off)

**R2 body semantics change**: the PRD draft said per-try TTFB and body-cap degradation "still apply" in pure mode. Design review overturns this with a stronger rule:

> **Pure mode never captures the request body.** Capture exists solely to replay the body across attempts; pure mode has one attempt and therefore no replay. Consequences: no 10 MiB cap, no 413, no degraded mode, no `X-Retry-Dropped`, and the body streams to the upstream (faster: no double-buffering). Per-try TTFB still applies (it bounds a hang, not a retry).

This is strictly simpler and strictly faster; the cap/413/degraded machinery is replay-protection and has no purpose without retries. The PRD's R2/R5 text is amended accordingly. Everything else below assumes this amendment.

**Review status**: adversarial panel review (3 rounds, no dissent) ratified this amendment; the panel's converged refinements — deprecation gate widening, empty-header normalization ladder, single-attempt literal, v0.3 flip criteria, README wording pins — are folded into §§1–7 below as design decisions D13–D15, not as suggestions.

## 1. Grammar (decision D1/D11)

The scheme segment gains a symmetric mode suffix:

```
SCHEME-SEGMENT := SCHEME [ "+" MODE ]
SCHEME         := "http" | "https"          (case-insensitive, lowercased)
MODE           := "retry" | "pure"          (case-insensitive, lowercased with the scheme)

PROXY-TARGET   := "/" SCHEME-SEGMENT "/" AUTHORITY [ "/" RAW-PATH ]
```

- `/https/example.com/x` — plain form. Mode is **transitional** (R5): v0.2 = retry mode + deprecation log when retry keys are present; v0.3 = pure mode.
- `/https+retry/example.com/x` — **retry mode**, stable forever. Query splits per v0.1.0.
- `/https+pure/example.com/x` — **pure mode**, stable forever. Query never touched; body streams; single attempt unless the header channel supplies a policy.
- Anything else (`https!retry`, `rx`, `https+`, `https+retrt`, `+retry`) → 400 with the usage hint naming all three accepted shapes.

**Rationale for suffix over leading-segment** (`/r/https/...`): the first segment remains "speaking scheme", so a typo produces `"unsupported scheme https+retrt"` — an error that names the actual malformed input — instead of `"unsupported scheme r"` for a mode typo. One mental model (scheme-first), one path shape, three accepted values. `+` is a legal path sub-delim (RFC 3986), needs no percent-encoding, and reads additively ("this scheme, **plus** retry borrowing").

**Escaped-path rule**: the scheme segment is matched against the **original escaped bytes** of the path, before any unescaping. `%2B` is not `+`: `/https%2Bpure/host/...` is not a mode-qualified scheme — it fails scheme validation as the literal segment `https%2Bpure` (400). No normalization ever re-interprets encoded characters as syntax; same original-bytes-first principle as query passthrough.

`PathTarget` gains:

```go
type PathTarget struct {
    Scheme string // "http" | "https" (mode stripped)
    Host   string
    Port   int
    RawPath string
    Mode   string // "retry" | "pure"; the plain form resolves per R5 transition
}
```

`HostPort()`/`URL()`/`Normalize()` are mode-agnostic — mode affects only query/body handling in the pipeline.

## 2. Header channel (decision D3/D9)

One header, compact pair serialization, same field grammar minus the `retry.` prefix:

```
X-Reproxy-Retry-Policy: status=5xx; network=1; budget=30s; [*].attempts=3; [429].attempts=5
```

- Pairs separated by `;`; optional whitespace around pairs (headers are hand-typed more often than URLs). `key=value` with keys spelled exactly as the query channel's post-`retry.` remainder: gates (`status`, `network`, `budget`) and scope fields (`[*].attempts`, `[429].attempts`).
- The parser **transforms to `url.Values` with `retry.`-prefixed keys and calls the existing `Parse()`** — zero new validation code, identical 400 matrix and error bodies for free. Unknown key → the same "unknown retry parameter" 400 `Parse` already emits.
- One header occurrence only: multiple `X-Reproxy-Retry-Policy` headers → 400 (fail closed). Values may not contain `;` or `=`; none of the field grammars need them.
- **Degenerate input → 400, in every mode** (normalization ladder sealed): a present-but-empty value, an empty pair (`;;`), or whitespace-only content all fail identically. "No policy" has exactly one spelling — the header is **absent**. A present-but-empty header is indistinguishable from a truncated or corrupted policy and must not be read as "single attempt".
- `X-Reproxy-*` is a reserved namespace on every request in every mode: any unknown `X-Reproxy-Foo` → 400 (fail closed, same principle as unknown query keys).
- All `X-Reproxy-*` headers are stripped before forwarding upstream (added to the strip logic alongside hop-by-hop headers in `buildOutboundHeaders`).

## 3. Mode × channel matrix (decision D4)

| Path mode | Header present? | Behavior |
|---|---|---|
| `+pure` | no | Pure proxy: query untouched, body streams, single attempt |
| `+pure` | yes | **Intended collision-free combo**: policy from header, query untouched, body streams |
| `+retry` | no | v0.1.0 semantics exactly (query splits, retry.* claimed) |
| `+retry` | yes | **400** — both channels used; error names the conflict and the remedy ("remove the header or use +pure") |
| plain (v0.2) | no | Retry mode + deprecation log when retry.* keys present |
| plain (v0.2) | yes | 400 — same conflict rule (plain selects retry mode transitionally) |
| plain (v0.3) | no | Pure mode (terminal state) |
| plain (v0.3) | yes | Header channel with pure mode |

The conflict rule is one invariant: **the query channel and the header channel are mutually exclusive whenever query-splitting is active**, independent of how retry mode was selected. Fail-closed 400, never silent precedence.

## 4. Pipeline (decision D6/D12)

```
ServeHTTP:
  (1) target := ParsePath(escapedPath)          // now returns Mode
  (2) mode resolution:
        +pure   → mode = pure
        +retry  → mode = retry
        plain   → mode = retry (v0.2, transitional) ; deprecation log if retry.* keys present
                  mode = pure  (v0.3, terminal)
  (3) channel resolution:
        pure:   policy := Parse(headerToValues(r.Header)) or SINGLE-ATTEMPT LITERAL (D13)
        retry:  if headerPresent → 400 conflict
                upstreamQuery, retryParams := SplitQuery(RawQuery)
                policy := Parse(retryParams, cfg)
  (4) body:
        pure:   NO Capture() — body streams to upstream as-is
        retry:  Capture() as today (replay machinery, cap, 413, degraded)
  (5) SSRF gates → attempt loop → commit            (unchanged, both modes)
```

Steps 1 and 5 are untouched code paths (L1–L7 SSRF applies to pure mode identically — the destination is the same attack surface whether or not retries happen). Step 4's pure branch is the PRD-amendment payoff.

`Capture()` and the whole degraded/strict machinery become retry-mode-only. `X-Retry-Count`/`X-Retry-Limit`/`X-Retry-Exhausted` are emitted only when a retry lifecycle exists (retry mode, or pure mode with a header policy — in which case the header policy IS a retry lifecycle, and the response headers report it).

**Edge**: pure mode + header policy → the body still streams (no capture)? If the header asks for 5 attempts, replay needs the body. So: **pure mode + header policy captures the body too** (replay is required by the requested policy). Pure mode means "no capture *unless a policy I supplied out-of-band demands it*". This keeps `+pure`+header the full-featured collision-free channel. Update: `X-Retry-Dropped` can therefore appear in `+pure`+header when the cap degrades — documented in README.

**D13 — single-attempt literal**: the no-header pure-mode policy is a literal single-attempt `Policy` value. Never `Parse(url.Values{})`: empty input returns the v0.1.0 defaults (3 attempts, network=1), which would smuggle a retry lifecycle into pure mode. The trap is that the reuse type-checks — which is why it is a named decision with a mutation target.

**D14 — capture invariant**: `pure ∧ no header ⇒ no body capture`. The single exception is a header policy (replay required), which re-enables the full replay machinery — cap, 413, degraded mode, `X-Retry-Dropped`. README states the revival in body text, not a footnote: adding `X-Reproxy-Retry-Policy` to a `+pure` route is not free.

## 5. Deprecation logging (decision D7)

`event=deprecation` fires in v0.2 on a plain-scheme request when **any `retry.*`/`retry[` key is present OR the request actually consumed a default network retry** (`retry.network=1` implicit path). Two reasons this gate is wider than "has retry keys":

1. The implicit dependent (Hyrum's law): a request whose only v0.3-observable difference is the silent network retry never spells a retry key. Without logging it, the operator has no signal that this class of traffic exists before its behavior changes.
2. Deprecation logging is the operator's only pre-flip visibility. Every logged line is a candidate migration; every unlogged behavioral change is a post-flap support ticket.

The log line dedupes per `(mode, keyset)` within the operator's view (design: log at request time with the keyset; operators aggregate offline — no in-proxy state kept). Plain + no retry keys + no network retry consumed does not log: its v0.3 behavior is byte-identical, so there is nothing to warn about.

**D15 — v0.3 flip criteria** (this task's output feeds them; the flip itself is out of scope):

- Trigger: version bump (≥1 minor after v0.2.0) **and** a time window (≥1 quarter of v0.2 deployed), both required. Not traffic-based: an OSS proxy cannot observe its deployers' traffic mix, so `deprecation` events trending to zero is an operator-side metric, not a release gate the proxy can measure. The README and release notes carry the checklist: "flip when your `event=deprecation` volume stops shrinking".
- v0.3 ships a legacy escape hatch env var (`REPROXY_LEGACY_PLAIN_RETRY=1`, plain segment → retry mode, no deprecation log) for deployers who read the release notes late; **removed in v0.4**. One env var, one release window, then gone — the escape hatch must not outlive the migration it serves.
- The flip task gets `mode=` on `event=request` from this task (R3/R6) so deployers can measure their own channel mix before and after.

## 6. Test map

| Area | File | Anchor tests |
|---|---|---|
| Scheme-segment grammar | `target_test.go` | full valid/invalid table: 3 valid forms × {http,https}, typos, case, `+` alone, empty mode, IPv6, default ports |
| Header parse/transform | `header_test.go` (new) | pair grammar, whitespace, unknown key → Parse's 400, multiple headers → 400, **empty value / `;;` / whitespace-only → 400 (normalization ladder)**, %2B scheme segment rejected |
| Pure-mode pipeline | `proxy_test.go`, `e2e_test.go` | byte-identical query (incl. `retry.count` target data), single upstream call, no X-Retry-Count, body streams (no capture observable via large-body passthrough), SSRF still enforced |
| Retry-mode pipeline | existing suite | **all v0.1.0 tests green, unmodified** (transitional plain→retry makes this true by construction) |
| Conflict matrix | `proxy_test.go` | every row of §3 |
| Deprecation log | `proxy_test.go` | plain+keys logs, plain-no-keys doesn't, +retry/+pure never log |
| Header stripped upstream | `e2e_test.go` | upstream sees no X-Reproxy-* |
| `+pure`+header captures body | `proxy_test.go` | header policy with 5 attempts + body → replay works, X-Retry-Count present |

Mutation targets (~12): pure-mode-splits-query (must stay pure), pure-mode-captures-body (must not), pure-mode-uses-Parse-empty-defaults (must use the literal), retry-mode-ignores-header-conflict (must 400), unknown-X-Reproxy-passes (must 400), header-not-stripped (must strip), empty-header-treated-as-absent (must 400), deprecation-log-omitted, deprecation-log-fires-without-retry-keys-but-network-retry, plain-resolves-wrong-mode, header-transform-drops-field, mode-suffix-casesensitivity, conflict-error-remedy-text, pure-mode-emits-retry-headers (must not), header-single-occurrence, scheme-%2B-decoded-to-mode.

## 7. Rollout

Single PR/task (this one), v0.2.0 tag at completion. v0.3 flips the plain-form default and drops the deprecation log — a separate small task, not this one's scope. No config flags: the mode is per-request in the URL/header, which is the entire point. (The v0.3 legacy escape-hatch env var in D15 belongs to that future task, not this one.)

**README wording pins** (D13–D15 follow-through; the panel's audience analysis):

- Name pure mode **"query-pure"** in prose (the guarantee is about the query, not a broader promise).
- Read `+` as a **mode modifier** on the scheme, not a transport selector — avoid any phrasing that pattern-matches to `git+ssh://` transport syntax, which would wrongly suggest alternate wire protocols.
- The conflict 400 remedy names the **actor source**, not just the fix: "if you did not set this header, a middleware or gateway between you and reproxy may have" — because the header channel is exactly the kind of thing infrastructure adds silently.
- The `+pure`+header capture-revival note (D14) goes in the mode table's body, not a footnote.

## 8. Rejected alternatives (recorded for the audit trail)

| Alternative | Why rejected |
|---|---|
| `__retry__` dunder prefix | Probabilistic mitigation, not ownership fix; second reserved-word family; audit pinned single-prefix |
| Header-only control plane (drop URL) | Breaks v0.1.0; loses "any client that can build a URL" ergonomics |
| Unknown retry key → silent passthrough | Violates fail-closed pinned semantics (typo debugging becomes guesswork) |
| Query-side mode flag (`retry.mode=pure`) | Self-referential: needs to claim a query key to say "don't claim query keys" |
| Relax `SplitQuery` to never 400 on unknown | Same fail-closed violation |
| `X-Reproxy-Passthrough: 1` boolean escape hatch | Superseded by the symmetric `+pure` mode — the mode covers it and also carries "no retries", which the boolean couldn't express |

## 9. Panel review record (2026-09-04, 3 rounds, adversarial)

Five-expert adversarial panel reviewed the design; verdict **GO** with refinements. Ratified: §0 body-capture amendment (no dissent), `+` suffix grammar, mutual-exclusion 400, plain-scheme transition. Folded in as decisions D13–D15: deprecation-gate widening (retry keys OR network-retry consumption), empty-header normalization ladder (all degenerate forms 400), single-attempt literal (never `Parse(∅)`), v0.3 flip criteria (version+time, operator-side metric, legacy env var), README wording pins (query-pure naming, mode-modifier reading, actor-source remedy, capture-revival in body text). Panel transcript lives in session history; this section is the durable record.
