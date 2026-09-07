# docker-smoke extraction implement report

Units completed, appended as finished (resume point if the process dies).

## Unit 1 — script skeleton + arg handling + cleanup scaffolding

- File: scripts/docker-smoke.sh (LF, shebang, set -euo pipefail).
- Two positional args, exactly-2 check, usage on stderr, exit 2 on wrong count.
- cd to repo root anchored on BASH_SOURCE — compose check needs docker-compose.yml and the rewritten local.yml next to the real Dockerfile (compose resolves build: . against the first -f file dir).
- Single EXIT trap: rm reproxy-smoke container, conditional compose down -v + rm of the rewritten file, rm of the three /tmp response bodies. (In the workflow these were three per-step traps; one script gets one trap over accumulated state — recorded as a relocation adaptation in Unit 7.)
- bash -n: PASS.


## Unit 2 — check-1 relocation (version banner)

- Moved verbatim from the "Smoke - version banner matches expected version" step: docker run --rm --version, expected/actual echo pair, exact-equality compare, ::error message (reworded to name the check; the GitHub-only ::error syntax stays in the workflow step name only).
- Dropped env plumbing (SMOKE_IMAGE/EXPECTED_VERSION + first_tag head -n1): the two positional args replace them, per the PRD allowed-differences list.
- bash -n: PASS.

## Unit 3 — check-2 relocation (round-trip + allowlist gate)

- Moved verbatim from the "Smoke - request round-trip through the proxy" step: same docker run (name reproxy-smoke, -p 18080:8080, --allowlist example.com), same readiness loop (30 x curl --max-time 2 + sleep 1), same proxied request to http://127.0.0.1:18080/https/example.com/ asserting 200 + "Example Domain" body marker, same gated request to other.example.org asserting 403 + "not in the allowlist" body, same `docker logs reproxy-smoke || true` on failures.
- The per-step EXIT trap is NOT duplicated mid-script: the shared EXIT trap from Unit 1 covers the container (a mid-script trap would also clobber itself at each reassignment). Recorded as a relocation adaptation.
- The SSRF-L3 / example.com public-stand-in comment and the readiness-loop if-condition comment travel with the code.
- `first_tag` substitution: script takes a single image arg, no multi-tag head -n1.
- Error messages: `::error::` prefix dropped (not GitHub syntax outside Actions); check name retained in the FAIL message. The `::error::` prefix in a PRD-named "echo prefixes naming the check" sense is replaced by "check N/3 (...)" prefixes.
- bash -n: PASS.

## Unit 4 — check-3 relocation (compose local-build variant)

- Moved verbatim from the "Smoke - compose file (local-build variant)" step: same sed -E rewrite of docker-compose.yml into docker-compose.local.yml (written next to the checkout, per the compose-relative-path comment), same grep guard on "build: .", same `docker compose -f docker-compose.local.yml up -d --build`, same readiness loop on 8080 (not 18080 — the comment travels), same gated request to http://127.0.0.1:8080/https/example.com/ asserting 403, same compose-logs-on-failure lines.
- `compose_file="docker-compose.local.yml"` is set right after the grep guard, arming the shared EXIT trap for the down -v + rm cleanup (replaces the per-step trap that was installed after `up`).
- `cat docker-compose.local.yml` on rewrite-failure retained (it dumps to stdout for diagnosis, then the trap removes the file).
- bash -n: PASS.

## Unit 4 addendum — round-trip container teardown between checks

The workflow removed the round-trip container at its step boundary (EXIT trap) before the compose step ran; a single-script trap would have left it running during check 3. Added an explicit `docker rm -f reproxy-smoke >/dev/null` at the end of check 2 so the compose check runs under the same conditions as in CI. Not `|| true`-shielded (that shielding belongs to a trap that must not mask the exit code; the EXIT trap retries the removal). Recorded as a relocation adaptation, not pure relocation.

(Correction: the first parse attempt failed with GBK UnicodeDecodeError — Windows Python default encoding, not a YAML problem. Re-ran with encoding='utf-8': PASS. The file itself is unchanged UTF-8.)

## Unit 6 — .gitattributes + README

- .gitattributes: added `*.sh text eol=lf` with a comment tying it to the Dockerfile/*.go rules (Git Bash chokes on CRLF shebangs; autocrlf checkout would make scripts/ unusable). Placed in the existing LF-rules block.
- README.md "Building locally": added the local-verification line `docker build -t reproxy:local . && scripts/docker-smoke.sh reproxy:local dev` with one sentence ("Verify a local build the same way CI does") plus the dev-version note. One line per the PRD, with the plainest honest framing.

## Unit 7 — self-review: relocation-fidelity diff against the replaced YAML blocks

Method: extracted the three smoke steps from `git show HEAD:.github/workflows/docker-publish.yml` (old lines 93-234) and diffed each check's executable lines against the corresponding region of scripts/docker-smoke.sh.

### Check 1 (version banner)
- Executable lines identical (docker run --rm --version, expected/actual echoes, exact-equality if, exit 1). Only differences: (a) the env-plumbing preamble (EXPECTED_VERSION/SMOKE_IMAGE/first_tag head -n1) replaced by the two positional args — PRD-allowed; (b) `::error::` prefix → `check 1/3 (version banner) FAILED:` — the PRD's allowed "echo prefixes naming the check" difference (the `::error::` annotation is GitHub Actions syntax, meaningless in a repo script; the check name is retained in the message).

### Check 2 (round-trip + gate)
- Executable lines identical: same docker run (name reproxy-smoke, -p 18080:8080, --allowlist example.com), same 30x(2s+1s) readiness loop with if-shielded curl probes, same proxied request to 'http://127.0.0.1:18080/https/example.com/' asserting 200 + "Example Domain" body marker, same gated request to other.example.org asserting 403 + "not in the allowlist" body, same `docker logs reproxy-smoke || true` on failure. All comments (SSRF-L3 example.com stand-in, readiness if-shield) traveled.
- Adaptation 1 (not pure relocation): the per-step `trap ... EXIT` is replaced by the shared script-level trap; an explicit `docker rm -f reproxy-smoke >/dev/null` at the end of check 2 reproduces the step-boundary teardown so check 3 runs under the same conditions as in CI (round-trip container gone before compose up). Justification: a trap reassigned mid-script would clobber the script-end cleanup; explicit teardown is the honest one-script equivalent.
- `${first_tag}` → `${image}` (single image arg; the multi-tag head -n1 moves to the workflow caller).

### Check 3 (compose)
- Executable lines identical: same sed -E rewrite into docker-compose.local.yml (next to the checkout — comment travels), same grep "build: \." guard + cat on failure, same `docker compose -f docker-compose.local.yml up -d --build`, same readiness loop on 8080 (the 18080-not-8080 comment travels), same gated request to 'http://127.0.0.1:8080/https/example.com/' asserting 403, same compose-logs-on-failure.
- Only differences: the three `::error::` prefixes → `check 3/3 (compose) FAILED:` (PRD-allowed echo-prefix change), plus `compose_file="docker-compose.local.yml"` arming the shared trap immediately after the grep guard (replaces the per-step trap that was installed after `up` — the shared trap now covers the window between the sed rewrite and `up` too, strictly more cleanup coverage than the workflow had).

### Workflow (docker-publish.yml)
- Full diff vs HEAD confined to hunks old-lines 95-234 (the three smoke steps + their banner comment). Meta/buildx/login/build/push steps byte-identical. The new single step: env SMOKE_IMAGE/EXPECTED_VERSION + run: first_tag="$(echo "${SMOKE_IMAGE}" | head -n1)" ; scripts/docker-smoke.sh "${first_tag}" "${EXPECTED_VERSION}". Env-var indirection; no template interpolation into the shell line. YAML parses (PyYAML safe_load, encoding='utf-8').

### Script mechanics
- `bash -n`: PASS. Zero CR bytes (pure LF; `git check-attr` reports text=set eol=lf for the script).
- Wrong-arg-count paths (0, 1, 3 args) print usage to stderr and exit 2 BEFORE any docker call — verified live on this host.
- Staged: `git add` + `git update-index --chmod=+x` → index mode 100755.

### Verification limitation (stated plainly)
This dev host has NO Docker. Verified here: bash -n, YAML parse, arg-count rejection, LF purity, executable bit in index, and line-level diff fidelity of the relocated logic. NOT verified here: the script actually running against a real Docker daemon. The runtime verification channel is a workflow_dispatch run of docker-publish on GitHub (main session triggers it after this implementation); PRD acceptance criteria 6 and 7 (green dispatch run + ghcr.io/killbus/reproxy:dispatch-test manifest 200) are gated on that run.
