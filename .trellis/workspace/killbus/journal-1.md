# Journal - killbus (Part 1)

> AI development session journal
> Started: 2026-09-03

---



## Session 1: TEAM B audit: retry reverse proxy design & tech path
<!-- trellis-session: v=2 fp=da83b40f99b9a9bf -->

**Date**: 2026-09-03
**Task**: TEAM B audit: retry reverse proxy design & tech path
**Branch**: `main`

### Summary

Audited the lightweight HTTP retry reverse proxy design (12 items: protocol design items 1-9, ecosystem reuse item 10, tech stack & MVP item 11, path decision item 12). Deliverables: audit-report.md + protocol-design-analysis.md + go/rust/openresty ecosystem research (source-verified, cited 2026-09-03). Verdict: draft direction sound with 6 high-severity spec gaps (attempts semantics, network-error condition, budget/timeout system, body replay premise, commit point, SSRF defense); no reusable project exists across Go/Rust/OpenResty; recommended path = build in Go (stdlib ReverseProxy + cenkalti/backoff v7 + Traefik retry.go as Apache-2.0 reference template, ~500-1000 LOC core), 9-item MVP scope with acceptance criteria.

### Git Commits

| Hash | Message |
|------|---------|
| `f4f3775` | docs(audit): TEAM B audit of retry reverse proxy design and tech path |

### Status

[OK] **Completed**


## Session 2: Implement reproxy MVP: check round, mutation scan, spec capture
<!-- trellis-session: v=2 fp=e3cab00bc3afd2df -->

**Date**: 2026-09-04
**Task**: Implement reproxy MVP: check round, mutation scan, spec capture
**Branch**: `main`

### Summary

Implemented the full reproxy MVP in Go stdlib across 4 batches (target parsing, query namespace split, policy resolution, body capture, backoff, SSRF resolve-then-pin, retry loop with budget/commit-point semantics, main entrypoint, e2e tests, README; 309 test cases). Ran a 23-mutation scan on an isolated copy proving every pinned semantic is test-locked (0 survivors). trellis-check found and fixed F-1 raced-response body leak, F-8 fail-closed network gate, F-10 timing flake, F-11 backoff logging; verdict READY-FOR-FINISH. Captured the protocol contract, concurrency semantics, and test-hardening rules into .trellis/spec/backend/. Archived task 09-04; proxy_mod.html left untracked pending owner decision.

### Git Commits

| Hash | Message |
|------|---------|
| `0a7b76a` | feat(reproxy): config, target path parsing, query namespace split (Steps 1-3) |
| `425e9d6` | feat(retry): policy parsing, body capture, backoff engine (Steps 4-6) |
| `be24540` | feat(proxy): SSRF resolve-then-pin and core retry loop (Steps 7-8) |
| `00a57e5` | feat(main): binary entrypoint, end-to-end tests, README (Steps 9-10) |
| `4068f78` | fix(check): race-condition body drain, fail-closed network gate, test hardening |
| `02f6ee1` | docs(spec): capture reproxy protocol contract, concurrency and test standards (Phase 3.3) |

### Status

[OK] **Completed**


## Session 3: Project close-out: bootstrap spec completion, CI, v0.1.0, scaffold tracking
<!-- trellis-session: v=2 fp=8a76cc243a08c84a -->

**Date**: 2026-09-04
**Task**: Project close-out: bootstrap spec completion, CI, v0.1.0, scaffold tracking
**Branch**: `main`

### Summary

Closed out the reproxy project after MVP: deleted stray proxy_mod.html (F-7, user-approved); completed 00-bootstrap-guidelines (backend spec files filled from real code — directory structure, logging event catalog, database N/A; frontend marked N/A for the Go CLI project) and archived it; added GitHub Actions CI (build/vet/gofmt/test on Linux+Windows, race detector on Linux where the dev host cannot run it — no C compiler); tagged v0.1.0 at the CI commit; tracked all agent-platform scaffolding (.trellis/scripts, .claude, .codex, .cursor, .opencode, .agents — 202 files) with settings.local.json and runtime state excluded via root .gitignore. No git remote configured yet — CI stays dormant until the repo is pushed somewhere.

### Git Commits

| Hash | Message |
|------|---------|
| `9b39e94` | chore(task): archive 00-bootstrap-guidelines |
| `64d9839` | docs(spec): complete backend spec fill-in; frontend marked N/A (bootstrap task) |
| `7a2132f` | ci: GitHub Actions — build, vet, gofmt, test on Linux+Windows, race on Linux |
| `22531f9` | chore: track Trellis and agent-platform scaffolding |

### Status

[OK] **Completed**

## 2026-09-06 — 09-04-query-ownership-modes done (v0.2.0)

Query ownership landed: three-channel model (+pure/+retry scheme modes, X-Reproxy-Retry-Policy header, transitional plain→retry with deprecation gate). Design went through a 5-expert adversarial chatroom (3 rounds) before implementation; converged refinements D13–D15 folded into design.md (single-attempt literal, capture invariant, degenerate-header ladder, v0.3 flip criteria, README wording pins). 4 implement batches + full-scope check; mutation scan 29/29 killed after fixing F-1 (pure-mode framing lock — applied the m18 mutation for real to verify red, then restored). CI green on both OS + Linux race. Tagged v0.2.0. v0.3 flip (plain→pure + legacy env var) is a separate future task per design §7.

## 2026-09-06 — 09-06-no-phantom-migration (v0.2.1)

User challenge: "哪来现有用户，这是一个新项目，都没有 release，先入为主了？" — killed the v0.2→v0.3 transition machinery as a phantom-audience design. Plain scheme → pure terminal state; deprecation gate / hasRetryKeys / onNetworkRetry / ExplicitMode / event=deprecation / planned REPROXY_LEGACY_PLAIN_RETRY all deleted. Net −193 lines.

Key decisions: (1) plain-row test policy — retry-semantics tests switched to +retry, plain-form tests flipped to pure expectations (both channels keep coverage); (2) tag v0.2.1 supersedes v0.2.0 rather than rewriting (tag messages describing dead semantics are superseded, never force-pushed); (3) check agent caught implement-report deviation-4 as a false claim (TestProxyRedirectNotFollowed not actually switched) — dispatching independent verification catches report drift.

## 2026-09-07 — 09-07-v0-3-grammar-rewrite (v0.3.0)

User's own reading killed the mode axis: "老实说，这个 +pure/+retry，我不知道有什么意义，我觉得 https+策略即第一性又自然干净。" Five-round chatroom (Fielding/Hickey/Brooks/Christensen/Cunningham) converged: R3 exposed the retry∧query-upstream-owned cell (the collision disease: upstream retry.status silently swallowed — fail-closed blocks unknown keys, not legal same-shape keys); R4 built the segment-policy solution; R5 killed +retry/modes entirely. The "stable forever" promise to zero users was withdrawn — a promise to nobody is mispriced, not sacred.

New grammar: `/https/host?query` single attempt; `/https+POLICY/host?query` retry per policy (query untouched — unconditionally upstream-owned); header channel retained. One pair grammar, two carriers (segment dotted scopes, header brackets — total bijection). No legacy detection: `+retry`/`+pure`/`+foo` die identically as the generic unknown-policy-field 400. Mutation m4–m7 4/4 killed. Tag v0.3.0 — not a fixup into v0.2.1 (phantom deletion and debt settlement are understandings from different times; separate tags keep the ledger honest).

Process notes: (1) implement agent stalled once (600s watchdog, pure exploration phase, zero loss — re-dispatched) and the process exited once mid-unit-4 (resumed from transcript; incremental persistence to implement-report.md made the resume cheap — units 1–3 already on disk); (2) check agent found F-1: the shared pair grammar silently widened to trim whitespace around `=` (v0.2 header parser trimmed neither) with no test pinning it — fixed with test rows on both carriers plus README grammar line; mutation claims re-verified from scratch by the check agent in its own isolated copy; (3) workflow deviation self-caught: round-5 rewrite escalated before the Trellis task existed — converged back with "先收敛到 trellis" (task 09-07 created, PRD frozen, agent's contract re-pointed to it mid-flight).

Mini mutation scan 3/3 killed (m1 plain→ModeRetry, m2 plain+header conflict restored, m3 SplitQuery-on-pure-path). Gates ×3 green; CI both OS + Linux race green (run 34038337558). Commit 3f73e30 + archive acf73a2.
