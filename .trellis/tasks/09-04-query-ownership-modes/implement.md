# Implementation plan: query-ownership-modes

Execution order assumes design.md §0 amendment is approved. Each batch ends
with green gates before the next dispatch. Sub-agents (trellis-implement) get
this file + prd.md + design.md as context.

## Batch 1 — scheme-segment grammar (target.go)

- [ ] `PathTarget.Mode` field (`"retry"` | `"pure"`), mode stripped from Scheme
- [ ] ParsePath: accept `SCHEME+MODE` forms; lowercase whole segment before split; strict table (anything not in {plain, +retry, +pure} × {http,https} → 400 with usage hint naming all three shapes)
- [ ] Plain form resolves to `retry` (transitional, per design §1) with TODO marker for the v0.3 flip
- [ ] `usageHint` extended with the three accepted forms
- [ ] target_test.go: full grammar table (valid ×3 forms ×2 schemes × case variants; invalid: `+`, `+retrt`, `!retry`, `rx`, `+pure+retry`, trailing `+`, **`https%2Bpure` escaped-mode segment (must 400, original-bytes-first)**; IPv6, default ports, raw-path preservation per mode)
- [ ] Existing target_test.go cases: verify all still pass (plain stays valid; Mode assertion added where the suite pins behavior)

Validation: `go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1`

## Batch 2 — header channel (header.go, new)

- [ ] `ParseRetryPolicyHeader(h http.Header) (url.Values, *RequestError)`: single occurrence check; pair split on `;`; whitespace trim; key grammar = query channel minus `retry.` prefix; transform to `retry.`-prefixed url.Values; **degenerate input → 400 (empty value, `;;` empty pair, whitespace-only — D-normalization ladder; "absent" is the only no-policy spelling)**
- [ ] Unknown `X-Reproxy-*` namespace guard: any other `X-Reproxy-Foo` on the request → 400 (checked wherever headers are read)
- [ ] `buildOutboundHeaders`: strip all `X-Reproxy-*` before forwarding
- [ ] header_test.go: pair grammar table, multiple-occurrence 400, unknown-namespace 400, **degenerate-input table (empty value, `;;`, whitespace-only, absent-header-must-not-400)**, transform equivalence (same input via query vs header → same Policy deep-equal), Parse's own 400 matrix exercised through the header path
- [ ] No proxy.go wiring yet (parse + strip only)

Validation: gates as Batch 1.

## Batch 3 — pipeline branches (proxy.go)

- [ ] Mode resolution per design §4: `+pure` → pure; `+retry` → retry; plain → retry + `event=deprecation` when **any `retry.*`/`retry[` key present OR the request consumed a default network retry** (gate per design §5; key scan via light `hasRetryKeys(rawQuery) bool` helper, network-retry consumption flagged by the attempt loop — do not run full SplitQuery twice)
- [ ] Pure branch: skip SplitQuery (RawQuery forwarded verbatim), skip Capture (body streams), policy = header policy or **single-attempt LITERAL — never Parse(url.Values{}), which returns 3-attempts+network=1 defaults (D13)**; emit X-Retry-* response headers only when a header policy exists
- [ ] Retry branch: header present → 400 conflict (names both channels + remedy); else existing path
- [ ] Pure + header policy: Capture DOES run (replay required by requested policy), degraded machinery applies
- [ ] `event=request` gains `mode=` key; `logLine` deprecation event
- [ ] proxy_test.go additions per design §6 table: conflict matrix (every §3 row), pure-mode byte tests, single-call assertions, deprecation log on/off (**incl. fires on network-retry-consumed without retry keys**), `+pure`+header capture
- [ ] e2e_test.go: `retry.count` collision case (pure mode reaches upstream verbatim), header-stripped upstream assertion, `+pure`+header e2e with retries observable
- [ ] Mutation-target additions from review: pure-mode-uses-Parse-empty-defaults, empty-header-treated-as-absent, scheme-%2B-decoded-to-mode, deprecation-log-fires-without-retry-keys-but-network-retry
- [ ] Verify: existing suite unmodified and green (R3 AC)

Validation: gates + suite ×3 consecutive.

## Batch 4 — docs + mutation scan + check

- [ ] README: three-channel model, ownership principle, mode table (design §3), migration section (v0.2 transitional → v0.3 terminal, network-retry default note), `retry%2E` footnote, X-Reproxy-* reserved namespace
- [ ] README wording pins (design §7): "query-pure" prose naming, `+` = mode modifier (not transport syntax), conflict-400 remedy names the actor source (middleware/gateway possibility), `+pure`+header capture-revival note in the mode-table body
- [ ] README grammar section: updated scheme-segment EBNF
- [ ] Mutation scan (isolated copy, task-dir report `mutation-scan.md` per 09-04 convention): ~12 targets from design §6; compile failures don't count; 0 survivors required
- [ ] trellis-check dispatch; fix findings
- [ ] Spec update: retry-proxy-contract.md gains mode grammar + header channel; quality-guidelines mutation list extended

Validation: gates + CI green (push, both OS; Linux race).

## Rollback points

Each batch is a standalone commit; revert at batch granularity. Batch 3 is the
only behavior-touching batch — the transitional plain→retry mapping keeps
existing URLs working, so a post-Batch-3 revert restores v0.1.0 behavior exactly.
