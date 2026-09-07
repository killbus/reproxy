# docker-smoke extraction check report

Check agent verification, 2026-09-07. Evidence source: the files themselves
(working tree + staged index + `git show c4cde20:...`), never the implement
report's claims. Host has no Docker — runtime gates AC-6/AC-7 are pending the
main session's dispatch run (see end).

## Section A — pure-relocation fidelity vs c4cde20 (PRD AC-2)

Method: `git show c4cde20:.github/workflows/docker-publish.yml` (confirmed
byte-identical to HEAD's copy: `git diff HEAD..c4cde20 --stat` on the file is
empty) diffed line-by-line against scripts/docker-smoke.sh.

### Check 1 (version banner)
Executable lines identical: `docker run --rm "${image}" --version` (first_tag →
image = arg plumbing, PRD-allowed), expected/actual echo pair, exact-equality
`if [[ "${actual}" != "${expected}" ]]`, `exit 1`. Nothing dropped, nothing
added to the assertion. `::error::` → `check 1/3 (version banner) FAILED:`
(allowed echo-prefix change). **PASS.**

### Check 2 (round-trip + allowlist gate)
Identical: `docker run --rm --detach --name reproxy-smoke -p 18080:8080
"${image}" --allowlist example.com`; readiness loop `seq 1 30` x
`curl -s -o /dev/null --max-time 2 http://127.0.0.1:18080/` + `sleep 1`;
`docker logs reproxy-smoke || true` on both failure branches; proxied request
`--max-time 30` to `http://127.0.0.1:18080/https/example.com/` asserting 200;
`grep -q "Example Domain" /tmp/body.html`; gated request `--max-time 5` to
`http://127.0.0.1:18080/https/other.example.org/` asserting 403;
`grep -q "not in the allowlist" /tmp/gated.json`. Both comment blocks (SSRF-L3
public-stand-in, readiness if-shield) travel verbatim. **PASS.**

### Check 3 (compose)
Identical: same `sed -E` rewrite of docker-compose.yml → docker-compose.local.yml,
same `grep -q "build: \."` guard + `cat` on failure, same
`docker compose -f docker-compose.local.yml up -d --build`, readiness loop on
8080, gated request `--max-time 5` to `http://127.0.0.1:8080/https/example.com/`
asserting 403, `logs || true` on failure branches. Dry-ran the sed + guard
against the current docker-compose.yml on this host: rewrite succeeds (v0.3.0
image line → `build: .`), guard passes. **PASS.**

### Verdicts on the three declared deviations

1. **Explicit `docker rm -f reproxy-smoke >/dev/null` at end of check 2**
   (script line 148). REAL and NECESSARY: the workflow removed the container
   at the step boundary (per-step EXIT trap fired when step 2's bash exited);
   a single-script trap fires only at script end, so without the explicit rm
   the round-trip container would still be running during check 3 — unlike
   CI. Does not change any assertion, port, URL, container name, or timeout.
   The unshielded (no `|| true`) form is deliberate and documented in-script
   (lines 144-148): a failed rm on a container that must exist is a failure
   worth stopping for; the EXIT trap retries the removal regardless. Worst
   case it converts an already-failing run into a failing run. ACCEPTABLE.
2. **`cd` anchored on BASH_SOURCE** (lines 44-45). REAL and NECESSARY: a
   workflow step gets cwd = workspace for free; a repo script invoked from
   any cwd must establish it (the compose check reads docker-compose.yml and
   writes the local variant next to it). In CI the computed dir IS the
   workspace — behavior identical. ACCEPTABLE.
3. **Single shared EXIT trap** (lines 52-61). REAL and NECESSARY: three
   step-boundary traps cannot exist inside one script, and reassigning
   `trap` mid-script would clobber the earlier cleanup. Covers all old duties
   (container rm, compose down -v, local.yml rm) and adds /tmp body cleanup —
   which the PRD deliverable itself demands ("Self-contained trap cleanup for
   every container/file it starts"). ACCEPTABLE, with one residual gap —
   see Finding 2.

**Section A verdict: PASS.** No assertion, port, URL, container name, or
timeout differs from the c4cde20 blocks.

## Section B — PRD acceptance criteria 1-5

1. **AC-1 PASS.** `git ls-files -s scripts/docker-smoke.sh` → `100755` (also
   after this check's edit + re-stage). Exactly two positional args enforced at
   lines 30-33 BEFORE any docker call. Live-tested 0/1/3 args: usage to stderr,
   exit 2, no docker invocation.
2. **AC-2 PASS** — covered by Section A.
3. **AC-3 PASS.** `.gitattributes` line 29 `*.sh text eol=lf`, with the comment
   "Shell scripts check out as LF like the Dockerfile/*.go rules above" tying
   it to the existing rules. `git check-attr` confirms `text: set, eol: lf`
   for the script. Rule affects exactly one tracked file
   (`git ls-files "*.sh"` → only scripts/docker-smoke.sh).
4. **AC-4 PASS.** "Example Domain" / "not in the allowlist" / "did not become
   ready" appear in the workflow NOWHERE (grep of .github/workflows/ empty);
   they appear only in scripts/docker-smoke.sh plus docs/tests. Smoke step is
   the script call with env-var indirection (`SMOKE_IMAGE`/`EXPECTED_VERSION`
   env → `first_tag="$(echo ... | head -n1)"` → two quoted args; no `${{ }}`
   inside `run:`). YAML parses (PyYAML safe_load, encoding='utf-8'); step
   list: Checkout, Buildx, Login, meta, Build, Smoke, Push.
5. **AC-5 PASS.** README "Building locally" contains
   `docker build -t reproxy:local . && scripts/docker-smoke.sh reproxy:local dev`
   in a sh block with framing sentence and dev note. Cross-checked the `dev`
   claim: Dockerfile `ARG VERSION=dev`.

**AC-6 / AC-7 NOT verifiable locally** — dev host has no Docker. Pending the
main session's workflow_dispatch run of docker-publish going green and the
`gh api` manifest check on ghcr.io/killbus/reproxy:dispatch-test returning 200.

## Section C — kill-list compliance

- No Makefile (`ls Makefile makefile GNUmakefile` → not found).
- No shellcheck anywhere in .github/ or scripts/.
- No split into three scripts: scripts/ contains only docker-smoke.sh.
- No port knobs: 18080/8080 hardcoded; no SMOKE_PORT or env indirection on
  ports anywhere in the script.
- No new CI steps beyond the one script call (step list above; net -127 lines
  of YAML, consistent with the PRD's "~120 lines out").

**Section C verdict: PASS.**

## Section D — script hygiene + workflow-region integrity

- `bash -n scripts/docker-smoke.sh`: PASS (re-run after this check's edit).
- Zero CR bytes: Python byte count = 0 CR / 202 LF, ends with newline;
  `git ls-files --eol` → `i/lf w/lf`. NOTE: the initial `grep -c $'\r'`
   probe reported "202" — a Git Bash text-mode artifact on this host,
   disproven by the byte-level count. Recorded so nobody re-trips on it.
- Workflow meta/buildx/login/build/push steps byte-identical to c4cde20: the
  full `git diff c4cde20 -- .github/workflows/docker-publish.yml` is ONE hunk
  (@@ -92,146 +92,19 @@) confined to the smoke region; everything outside is
  untouched context.
- implement-report.md exists and covers all planned work — but its unit
  numbering skips "Unit 5" (goes 1,2,3,4,+addendum,6,7; the workflow-rewrite
  material lives under Unit 7). Cosmetic gap in the task artifact, not code.

## Section E — quality gates

No Go code changed in this task; gates re-run to prove no accidental
breakage: `go build ./...` PASS, `go vet ./...` PASS, `gofmt -l .` empty PASS,
`go test ./...` ok (reproxy 5.005s, cmd/reproxy 4.501s).

## Findings

1. **FIXED (minor).** scripts/docker-smoke.sh:172-173 — the relocated compose
   comment still said the round-trip container "is gone via the EXIT trap",
   contradicting the script's own single-trap design and its lines 140-148
   comment (the explicit rm at end of check 2, not the trap, removes it before
   check 3). Rewrote to "was removed at the end of check 2". The R6-bug content
   (port 8080 not 18080) is preserved. Re-staged; index mode still 100755;
   bash -n PASS; 0 CR bytes.
2. **LOW (not fixed — any fix would itself violate pure relocation).**
   docker-compose.local.yml leaks if the compose check fails in the
   sed → grep-guard window: `compose_file` is armed only AFTER the guard, so
   the EXIT trap neither removes the file nor runs `down -v` on that path.
   This matches the old workflow exactly (its trap was installed only after
   `up`, so a guard-failure left the file on the ephemeral runner too), and
   the PRD's allowed-differences list does not cover moving the arming point
   or adding an `rm` inside the relocated failure branch. It also makes
   implement-report Unit 7's "strictly more cleanup coverage" claim imprecise:
   coverage starts at the guard, not at sed. Recorded for the main session;
   on a dev host the leaked file shows as untracked in git status (not
   gitignored). No assertion is affected.
3. **INFO.** The script adds progress/OK echo lines ("check N/3: …", "check
   1/3: version banner OK", the check-2/check-3 summary lines) beyond the old
   code's echo set. Output-only; names the check (the PRD's allowed
   echo-prefix category); changes no assertion. All ORIGINAL OK-echoes are
   retained verbatim ("round-trip OK: 200 + upstream body marker verified",
   "compose OK: service up, gated 403 verified").

## Overall verdict

**READY-FOR-FINISH** for everything locally verifiable. AC-1..AC-5 and all
kill-list/hygiene checks PASS; the one code touch-up (Finding 1) is done and
staged. AC-6/AC-7 remain the main session's runtime gates (workflow_dispatch
green run + GHCR dispatch-test manifest 200) — the PRD names that run as the
completion gate, and it must happen before the v0.4.0 tag push (R8-3
ordering).
