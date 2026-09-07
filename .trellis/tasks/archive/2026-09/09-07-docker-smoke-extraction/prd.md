# Extract Docker smoke tests to scripts/docker-smoke.sh

## Origin

Chatroom round 7, R7-4..R7-8 (2026-09-07). The three smoke checks currently
live as ~150 lines of inline bash inside `.github/workflows/docker-publish.yml`
`run:` blocks. R6's own converged principle — "humans and CI use the same build
path" (one Dockerfile) — was only executed halfway: the build is shared, the
verification is not. The smoke checks pin **product contracts** (version banner
== injected value, proxy actually round-trips, allowlist gate in force, compose
file runnable), and product contracts belong in the repo, not inside a CI
template where they are reviewed as config instead of code.

R7's strongest counter-attack (phantom audience — "local replayability serves
nobody, the dev host has no Docker") was resolved on the record: the defense
of extraction is **"the contract gets an address in the repo"**, NOT local
replayability. Local replayability is recorded as an unowned benefit and is
explicitly not part of the decision basis.

## Deliverables

1. **`scripts/docker-smoke.sh <image> <expected-version>`** — one file, two
   positional args, no knobs. Contains all three checks, byte-level relocated
   from docker-publish.yml (pure relocation, no redesign — R7-8):
   - Check 1: version banner — `docker run --rm <image> --version` output
     equals `<expected-version>` exactly.
   - Check 2: public round-trip + allowlist gate — detach container on port
     18080 with `--allowlist example.com`, readiness loop (30×2s), proxied
     request to example.com asserts 200 + "Example Domain" body marker,
     then a non-allowlisted host asserts 403 + "not in the allowlist" body.
   - Check 3: compose variant — sed-rewrite `image:` → `build: .` into
     `docker-compose.local.yml` **written next to the checkout** (compose
     resolves relative paths against the first `-f` file's directory), up
     with build, readiness loop on 8080, non-allowlisted host asserts 403.
   - Self-contained trap cleanup for every container/file it starts.
   - The R6-bug comments travel with the code (port 8080 not 18080 for
     compose; file written next to checkout not /tmp; SSRF L3 forbids
     loopback upstreams so example.com is the stable public stand-in).
   - `set -euo pipefail`; fail messages echo the failed check's name.

2. **`.gitattributes`** gains `*.sh text eol=lf` (with a comment line tying it
   to the existing `Dockerfile`/`*.go` lines). This is a precondition, not a
   nicety: Windows checkout autocrlf would otherwise make the script CRLF and
   Git Bash would choke on it.

3. **`.github/workflows/docker-publish.yml`** — the three smoke steps collapse
   into one step calling `scripts/docker-smoke.sh "$first_tag"
   "$version"` (env-var indirection, never template interpolation into the
   shell line). Meta (event-keyed), buildx, login, build, push steps stay
   untouched. Net: ~120 lines of YAML out.

4. **`README.md`** Docker section gains one local-verification line:
   `docker build -t reproxy:local . && scripts/docker-smoke.sh reproxy:local dev`
   (a local build without VERSION build-arg reports `dev`).

## Constraints

- **Pure relocation** (R7-8): the check logic moves as-is. Readiness loop,
  trap cleanup, example.com marker assertion, 403 gate assertion, the sed
  rewrite — all byte-level moves. No redesign, no new assertions, no dropped
  assertions. Differences allowed ONLY for: shebang line, `set -euo pipefail`,
  the two args replacing env vars, and echo prefixes naming the check.
- **No new mechanisms** (R7 kill list): no Makefile, no shellcheck CI step,
  no split into three scripts (shared readiness/cleanup scaffolding stays
  shared), no port knobs (18080/8080 stay hardcoded).
- **stdlib-only rule untouched**: that is a product constraint on Go code;
  a POSIX shell script is infra, same category as the workflow YAML.
- **Ordering**: this task lands BEFORE the v0.4.0 tag is pushed (R8-3) — the
  first image-shipping tag must pass through the extracted smoke path.

## Acceptance Criteria

1. `scripts/docker-smoke.sh` exists, is executable-bit-set in git, takes
   exactly two positional args, and rejects wrong arg counts with usage.
2. The three checks' logic in the script is verifiably the same as the YAML
   blocks it replaced (reviewer can diff the moved logic against
   docker-publish.yml @ c4cde20).
3. `.gitattributes` contains the `*.sh text eol=lf` line.
4. docker-publish.yml contains no inline smoke logic; its smoke step is the
   script call; the workflow still parses as valid YAML.
5. README contains the local-verification line.
6. **Verification channel** (the dev host has no Docker): a workflow_dispatch
   run of docker-publish goes fully green — version banner, round-trip + gate,
   compose checks all passing through the extracted script. This is the
   completion gate.
7. `gh api` anonymous manifest check on `ghcr.io/killbus/reproxy:dispatch-test`
   returns 200 after the run (the push path still works end-to-end).
