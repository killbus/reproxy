# Implement report — no phantom migration

Task: 09-06-no-phantom-migration. Flip the plain scheme segment to pure mode as the terminal state and delete the entire v0.2→v0.3 transition layer.

## 1. Changes per file

### Source

- **`target.go`**
  - `PathTarget`: deleted the `ExplicitMode` field and its doc comment; `Mode` doc updated ("the plain form (no `+MODE` suffix) resolves to `pure`").
  - `parseSchemeSegment`: plain branch now returns `ModePure`; deleted the `TODO(v0.3)` block and transitional language; return signature dropped the `explicitMode` bool (now `(scheme, mode string, rerr *RequestError)`); doc comment rewritten (plain ≡ `+pure`).
  - `ParsePath`: destructure and `PathTarget` construction updated for the new signature (no `ExplicitMode`).
  - `usageHint`: unchanged — the three accepted shapes are the same three; the hint names forms, not per-form mode semantics.
- **`proxy.go`**
  - Deleted `newDeprecationLogger` (whole function).
  - Deleted the `onNetworkRetry` variable from ServeHTTP's var block and the whole `if !target.ExplicitMode { ... }` deprecation-arming block.
  - `attemptLoop`: dropped the `onNetworkRetry` parameter and the callback invocation + comment in the network-retry path; doc comment updated.
  - `default: // ModeRetry` branch comment now says "+retry scheme segment" (plain arrives as ModePure).
  - `ServeHTTP` doc comment: mode-resolution paragraph rewritten (plain ≡ `+pure`; no transitional language).
  - `logRequest` doc comment: dropped "ahead of the v0.3 plain-form flip".
- **`query.go`**: deleted `hasRetryKeys` and its doc comment. `inRetryNamespace` and everything else untouched.

### Tests

- **`target_test.go`**: removed the `ExplicitMode` bool from every positional `PathTarget` literal (all rows); every plain-form row now asserts `ModePure` (was `ModeRetry, false`); section comment updated. Error rows and `+retry`/`+pure` rows otherwise unchanged.
- **`proxy_test.go`**:
  - `TestProxyModeChannelConflictMatrix`: plain rows renamed and re-asserted — "plain, no header" → 200/1 call; "plain, header policy" → 200/1 call (the old conflict row deleted, with a comment stating plain+header is the intended combo).
  - `TestProxyRequestLogCarriesMode`: plain row's wantMode flipped "retry" → "pure"; dropped the per-row deprecation-absence assertion.
  - DELETED `TestProxyDeprecationLogGate`, `TestProxyPlainSchemeDeprecationDedupeBothTriggers`, `TestHasRetryKeys`.
  - `TestProxyPlainSchemeExplicitRetryKeysUnknownStill400` → rewritten as `TestProxyPlainSchemeQueryNeverSplit`: plain scheme never splits the query — `?retry.wat=1` is target data reaching the upstream verbatim (500-relay path, one call, byte-identical RawQuery).
  - NEW `TestProxyPlainSchemeHeaderPolicyRetries`: plain + `X-Reproxy-Retry-Policy` = header channel with pure mode — 200, two calls, query untouched, `X-Retry-Count: 2` (mutation m2's capture; also AC #2's direct test).
  - Retry-semantics tests switched from plain to `+retry` paths (list in §2).
  - `captureLogs`/`logBuf` helpers kept (used by `TestProxyRequestLogCarriesMode`); helper comment de-deprecation-ized.
  - `TestProxyPureModeNetworkFailureNotRetried` doc: dropped "the R5 transition's reason to exist".
- **`e2e_test.go`**:
  - `TestE2EModeChannelConflictOverTCP`: deleted the plain-form conflict row (now only `+retry`); comment updated.
  - `TestE2EHappyPathGET`: switched to `/http+retry/...` (it asserts `X-Retry-Count: 1`).
  - Retry-semantics tests switched to `+retry` paths (list in §2).
  - `TestE2EHeadersStrippedUpstream`: the plain-form lookalike-header leg kept (pure mode, no conflict) with an updated comment.
  - Mode-independent plain rows kept as-is (SSE streaming, SSRF 403s, Host assertion, allowlist-before-DNS).

### Docs

- **`README.md`**:
  - Plain-form bullet (§Query ownership): now "pure mode, same as `+pure`" — Transitional wording and Migration link removed.
  - Pure-mode semantics: the v0.1.0 network-retry difference stays as a plain statement; deprecation-log cross-reference removed.
  - Header-channel conflict bullet: "(or plain segment)" parenthetical removed.
  - DELETED the entire "Migration: plain scheme segment" section (~35 lines incl. the deprecation log sample and the `REPROXY_LEGACY_PLAIN_RETRY` escape hatch).
  - Mode table unchanged — three channels stay three; the plain form was never listed as a fourth channel.
- **`.trellis/spec/backend/retry-proxy-contract.md`**:
  - §2: `hasRetryKeys` signature removed; `ParsePath` comment now says "PathTarget carries Mode (plain form resolves to pure)".
  - §3 mode grammar: heading de-versioned; plain → **pure mode (terminal state)** with a one-line note that D15's flip criteria are moot.
  - §3 matrix: plain rows are pure-mode rows (plain+no-header pure; plain+header = intended combo, NOT a conflict); the old "plain (v0.2) retry+deprecation" row removed.
  - Deleted the "Deprecation gate (v0.2, plain scheme)" subsection.
  - §4 matrix row: "`+retry`/plain mode + header" → "`+retry` mode + header".
  - §5: added a Good case for plain+header (the intended combo); existing cases were already plain-safe.
  - §6: deprecation test entries replaced by a plain-form entry (`TestProxyPlainSchemeQueryNeverSplit`).
- **`.trellis/spec/backend/logging-guidelines.md`**: deleted the `deprecation` event row and its two rules lines; `mode=` on `request` kept, wording updated to "plain scheme logs `pure`".

## 2. Plain-row test policy (flipped vs switched-to-+retry)

Policy applied: tests whose PURPOSE is retry semantics switched their path to `+retry` (preserves coverage of the stable channel); tests whose purpose is the plain form itself (or mode-independent behavior) kept the plain path with flipped pure expectations.

**Switched to `+retry` (retry-semantics purpose):**

- proxy_test.go: `TestProxyRetriesStatusSequence`, `TestProxyNetworkErrorRetry`, `TestProxyNetworkGateClosed`, `TestProxyExhaustedDeliversLastResponse`, `TestProxyNetworkExhaustion504`, `TestProxyBudgetExhaustion`, `TestProxyQueryBytePreservation`, `TestProxyDegradedBodyPassthrough`, `TestProxyBodyReplayedAcrossRetries`, `TestProxyRetryAfterHonored`, `TestProxyRetryAfterCappedByBudget`, `TestProxyRetryAfterIgnored`, `TestProxyDeadConfig400`, `TestProxyUnknownRetryKey400`, `TestProxyCommitPointNoRetryAfterHeaders`, `TestProxyIntegrationRetryAgainstRealUpstream`, `TestProxyTTFBTimeoutRetryable`, `TestProxyRetryAfterHTTPDate`, `TestProxyRetryAfterPastDate`, `TestProxyPerStatusScopeShaping`, `TestProxyStatusScopeRaisesLimit`, `TestProxyStatusGateClassShorthand`, `TestProxyOnlyRetryParams`, `TestProxyRacedResponseBodyClosed` (retry.budget), `TestProxyBodyContentLengthPreserved` (replay framing), `TestProxyMethodPreserved` (replay docstring), plus `TestProxyDegradedBodyStillSSRFChecked`/`TestProxyStrictBodyLimit413` (capture-cap machinery is retry-mode-only), `TestProxyNoRetryWithoutStatusGate`, `TestProxyHopByHopStrippedOutbound`, `TestProxyHopByHopStrippedInbound`, `TestProxyRedirectNotFollowed` (kept asserting retry-mode observable behavior — switched for assertion consistency where X-Retry headers or retry defaults were involved), `TestProxyHeadRequest`.
  - Note: `TestProxyNoRetryWithoutStatusGate`, `TestProxyHopByHop*`, `TestProxyRedirectNotFollowed`, `TestProxyHeadRequest` were switched to keep the suite's retry-mode default-policy coverage (a plain path would now exercise pure mode and leave the retry-mode default path untested there).
- e2e_test.go: `TestE2ERetryToSuccess`, `TestE2EExhaustionDeliversLastResponse`, `TestE2EPOSTBodyReplay`, `TestE2EPOSTOversizedDegraded`, `TestE2EQueryBytePreservation`, `TestE2EUnknownRetryKey400`, `TestE2EDeadConfig400`, `TestE2EBudgetExhaustion`, `TestE2EHappyPathGET`.

**Kept plain, expectations flipped to pure:**

- `TestParsePath` (target_test.go, all plain rows → `ModePure`), `TestProxyModeChannelConflictMatrix` plain rows, `TestProxyRequestLogCarriesMode` plain row, `TestProxyPlainSchemeQueryNeverSplit` (rewritten), `TestProxyPlainSchemeHeaderPolicyRetries` (new).

**Kept plain, mode-independent purpose (no change needed):**

- proxy_test.go: `TestProxySSRFAllowlistGate`, `TestProxySSRFPrivateResolutionRejected`, `TestProxySSRFIPLiteralPrivate`, `TestProxySSEStreamedNotBuffered`, `TestProxySchemeAndPortNormalization`, `TestProxyViaAndXFFChainAppend`, `TestProxyClientDisconnectDuringWait` (uses `+retry` query — actually switched; listed above), `TestProxyBadTarget400` (error rows).
- e2e_test.go: `TestE2ESSEStreaming`, `TestE2ESSRFPrivateIPLiteral`, `TestE2ESSRFPrivateResolution`, `TestE2EAllowlistGateBeforeDNS`, `TestE2EHostHeaderIsUpstreams`, `TestE2EHeadersStrippedUpstream` lookalike leg.
- header_test.go: the two `httptestRequest("GET", "/http/up.example.com/x")` calls feed `buildOutboundHeaders` directly (no mode resolution) — mode-independent, unchanged.

## 3. Mutation mini-scan

3/3 killed, 0 survivors. Isolated copy in `/tmp/reproxy-mutation-scan` (outside the repo; main tree never modified; every mutation reverted and diff-verified byte-identical). Details in `mutation-scan.md`:

- **m1** plain branch → `ModeRetry`: killed by 5 tests (`TestParsePath`, `TestProxyModeChannelConflictMatrix`, `TestProxyRequestLogCarriesMode`, `TestProxyPlainSchemeQueryNeverSplit`, `TestProxyPlainSchemeHeaderPolicyRetries`).
- **m2** plain/pure + policy header → conflict 400 (old conflict restored): killed by 8 tests, capture `TestProxyPlainSchemeHeaderPolicyRetries` (written this task).
- **m3** `SplitQuery` called on the pure/plain path: killed by 8 tests, capture `TestProxyPlainSchemeQueryNeverSplit`.

## 4. Validation

- Gates ×3 on the real tree, all green: `go build ./... && go vet ./... && gofmt -l .` (empty) && `go test ./... -count=1` → `ok reproxy` (4.0s / 3.9s / 3.8s) each run.
- AC grep: `grep -rE "deprecation|hasRetryKeys|onNetworkRetry|ExplicitMode|LEGACY_PLAIN" --include="*.go" .` → zero hits. README: no `REPROXY_LEGACY_PLAIN_RETRY`, no `## Migration` heading, no v0.3 references.
- Race detector cannot run on this host (`gcc` not found, `CGO_ENABLED=1` fails) — pre-existing documented limitation; the Linux CI job covers `-race`.
- Flake note (per coordinator guidance): one run during an intermediate gate showed a failure whose captured log line (`mode=pure status=500 attempts=1`) is consistent with the known timing-flake class. `TestProxyPerStatusScopeShaping` isolated with `-count=200`: 200/200 pass. That test was NOT modified. All three final gate runs were fully green with no flakes.

## 5. Deviations

1. **`TestProxyRetryAfterCappedByBudget` / `TestProxyBudgetExhaustion` / `TestProxyTTFBTimeoutRetryable`** — these rely on the retry-mode DEFAULT policy (network gate on, 3 attempts) from a zero-retry-param query; the plain path no longer carries a default retry lifecycle, so they were switched to `+retry` (their purpose is default-policy retry behavior, which is now `+retry`-only). Covered by the §2 policy.
2. **m2 mutation scope**: restoring the old plain+header conflict required keying the conflict on the shared pure-mode branch (plain and `+pure` are indistinguishable after `ExplicitMode`'s removal), so the m2 mutation 400s both plain+header and `+pure`+header. The designated capture (`TestProxyPlainSchemeHeaderPolicyRetries`, plain form) goes red, and the mutation additionally breaks the `+pure`+header combo tests — strictly stronger than required.
3. **Version tag v0.2.1**: not performed here — tagging is a git/commit-phase action, and the implement agent is forbidden from `git commit`/`git push`. The tag belongs to the main session's Phase 3.4 commit step.
4. `TestProxyHopByHopStrippedOutbound`/`Inbound`, `TestProxyRedirectNotFollowed`, `TestProxyHeadRequest` were switched to `+retry` although their assertions are mode-independent — done deliberately so the plain path's coverage in those tests does not silently drift (they historically exercised the default retry policy via plain; keeping plain would change what they test without any assertion noticing). This preserves their original coverage intent under the new grammar.
