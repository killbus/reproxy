# Implement report — 09-07-docker-image-ci

## Progress (append-only)

### 1. `.gitattributes` — CRLF guard

Windows `core.autocrlf` would check out `Dockerfile` with CRLF; a CRLF
Dockerfile can inject `\r` into build output (the `ARG VERSION` default
value, heredocs, RUN lines). Added `text eol=lf` for `Dockerfile`,
`*.yml`/`*.yaml`, `.dockerignore` alongside the existing `*.go` rule
(same rationale: the CI gofmt gate). LF blobs in the index; checkout-only
concern.

### 2. `cmd/reproxy --version` + tests

- `cmd/reproxy/main.go`: package-level `var version = "dev"`, stamped by
  `-ldflags "-X main.version=<tag>"`. Guard `versionRequested(args)` checks
  `args[0] == "--version"` **before** flag parsing (so it wins even in
  combinations the FlagSet would reject); prints the version and exits 0.
  Leading-position-only: `--listen :8080 --version` falls through to flag
  parsing (unknown-flag 2, verified live) — a version banner is a leading
  convention (c.f. git/docker/go tooling).
- `cmd/reproxy/main_test.go`: table test over the guard (leading, leading+
  more, no-args, other-flag-first, unknown flag, `-version` single dash,
  positional) + default-stamp test.
- Verified locally: `go run ./cmd/reproxy --version` → `dev`;
  `go build -ldflags "-X main.version=v9.9.9-test"` → `--version` prints
  the injected value. Empty-allowlist startup gate unaffected.
- Gates: build/vet/gofmt/test all green.

### 2a. `printVersion` seam added

`fmt.Fprintln(os.Stdout, version)` moved into `printVersion(w io.Writer)`;
tests pin the output shape (`v1.2.3\n`, `dev\n`) — the CI smoke asserts
this exact line. ldflags injection re-verified after the refactor.

### 3. DEVIATION FROM PRD SMOKE SKETCH — loopback upstream is impossible (product fact)

PRD §Deliverables-4 says the round-trip smoke should "start the container
with `--allowlist 127.0.0.1` ... pointing at a trivial local test
upstream". **This cannot work and I verified it live:**

```
$ reproxy --allowlist 127.0.0.1 & curl http://127.0.0.1:18080/http/127.0.0.1:18080/
403 {"error":"upstream \"127.0.0.1:18080\" resolves to a forbidden address
127.0.0.1 (private, loopback, link-local, or otherwise reserved)"}
```

SSRF L3 (ssrf.go `forbiddenIPv4Prefixes` — `127.0.0.0/8`, RFC 1918,
link-local, etc.) is **unconditional**: `--allowlist` gates L2 only and
`--dangerous-allow-all` explicitly "still applies the forbidden-IP list"
(README Security section). There is no flag that permits a loopback or
private upstream — by design, since the product's core guarantee is "no
SSRF". A local upstream round-trip is therefore structurally impossible,
not a configuration matter.

(Also notable: the dev host's DNS resolves example.com to 198.18.66.101 —
a fake-IP range that is *itself* in the forbidden table (198.18.0.0/15) —
so public-host round-trips are not reproducible locally either; only
GitHub runners have clean DNS.)

**Redesigned smoke (kept one notch simpler than the PRD's ambition,
achievable, and honestly stronger on the product's own axes):**

1. Version banner == expected version string (unchanged).
2. Round-trip through the proxy to a **real public https upstream**
   (`https://example.com/` — stable, anycast, IANA-held, expected to be
   up; assert status 200 and the "Example Domain" body marker) with the
   container run using `--allowlist example.com`.

   This proves: binary alive, server up, allowlist gate working (an
   un-allowlisted host 403s), public-IP DNS resolution, TLS handshake
   with certificate validation — the last one exercises the
   distroless/static CA-roots rationale in the same request (a scratch
   image would fail exactly here). It needs no helper processes, no
   `--network host`, and no fake upstream.

   Failure tolerance: the version assertion is the pinned failure mode;
   the round-trip depends on example.com's availability. A failure there
   is a real signal, not a flake shield — but it is the one external
   dependency, documented here as the trade for the impossible loopback
   design.

Decision: documented deviation per the workflow's "product facts beat
PRD sketches" principle. The PRD's intent (prove binary alive, server
up, gate working, forwarding works) is fully preserved.

A third smoke assertion was added beyond the PRD's two: the allowlist
gate itself (non-allowlisted host -> 403 naming the allowlist). Verified
deterministic locally — it does not touch DNS or the network (L2 fires
before L3), so it carries zero external-dependency risk:

```
$ reproxy --allowlist api.example.com & curl .../https/example.com/
403 {"error":"upstream host \"example.com\" is not in the allowlist",...}
```

### 4. Workflow mechanics — locally simulated

- The `meta` step's bash was executed locally with both
  `GITHUB_REF_TYPE` values: tag path produces `version=v0.4.0` +
  two-line tags heredoc; dispatch path produces
  `dev-run<N>-<shortsha>` + `dispatch-test` tag. Multi-line
  `GITHUB_OUTPUT` heredoc verified.
- `head -n1` extraction of the first tag from the multi-line output
  verified for both paths.
- All embedded `run:` blocks pass `bash -n` (extracted via PyYAML then
  parsed).
- YAML structure validated with PyYAML (steps, uses, permissions).
- `EXPECTED_VERSION` is passed via step `env`, not `${{ }}`
  interpolation inside the shell line (a tag name with shell
  metacharacters must never become code).

### 5. Actions versions — re-verified live (2026-09-07, gh api)

| Action | Latest release | Published | Matches PRD |
|---|---|---|---|
| actions/checkout | v7.0.1 | 2026-07-20 | yes |
| docker/login-action | v4.6.0 | 2026-07-29 | yes |
| docker/setup-buildx-action | v4.3.0 | 2026-08-19 | yes |
| docker/build-push-action | v7.3.0 | 2026-07-01 | yes |

Input names verified against each action's `action.yml` at that release:
login (`registry/username/password`), setup-buildx (defaults used),
build-push (`context/file/load/push/tags/build-args/annotations` all
present in v7.3.0's input table).

### 5a. Distroless base tag — verified against the registry, not the PRD sketch

The PRD writes `gcr.io/distroless/static` + `USER nonroot`. The
distroless README (checked 2026-09-07) marks bare `distroless/static`
a deprecated alias — the maintained family is
`gcr.io/distroless/static-debian13` with tags
`latest, nonroot, debug, debug-nonroot` ("Any other tags are considered
deprecated and are no longer updated").

Registry facts pulled from gcr.io directly:
- `distroless/static:latest` and `distroless/static-debian13:latest`
  share the SAME index digest (f2ea2709...); same for `:nonroot`
  (1c2c046b...). The alias still resolves — but the PRD's core reason
  for distroless-over-scratch is CA-bundle freshness, and a frozen
  alias is exactly how that goes stale silently.
- Decision: `FROM gcr.io/distroless/static-debian13:nonroot` — the
  maintained spelling, and the `nonroot` tag itself guarantees the
  non-root runtime (no separate `USER` needed for correctness; kept
  `USER nonroot` as explicit documentation). Verified resolvable on
  gcr.io (OCI index, amd64 manifest present).

### 6. Workflow-level bugs found by self-review (before any dispatch)

1. **Event keying**: the meta step keyed on `GITHUB_REF_TYPE == tag`,
   but a `workflow_dispatch` run ON a tag ref would then have taken the
   release path (publishing a version tag + :latest from a manual test
   run!). Rekeyed on `GITHUB_EVENT_NAME == push` — this workflow's push
   trigger only fires for v* tags, so push-event == release. Both
   paths simulated locally (dispatch on v0.3.0 ref now correctly
   yields `dev-run<N>-<sha>` + `dispatch-test`).
2. **set -e vs readiness probe**: GitHub's default shell runs `bash -e`;
   the readiness loop's failed curl probes are shielded by the
   `if`-condition context, so the loop can retry. Documented inline.
3. **`docker run --detach` + trap**: the round-trip smoke detaches the
   proxy container and registers an EXIT trap to remove it — a failed
   step cannot leak a container holding port 18080 (which the compose
   step afterwards also binds).

### 7. Compose build-variant smoke added (PRD acceptance criterion)

The PRD requires "`docker compose up` (build: variant) starts a serving
proxy — validated on the GitHub runner within the dispatched workflow".
Added a third smoke step: sed-rewrite of the committed compose file into
the local-build variant (rewrite validated locally: result parses as
YAML with exactly build/command/ports/restart), `docker compose up -d
--build`, readiness wait, then a deterministic gated request (host
outside the allowlist -> 403) — no external dependency. This also covers
PRD deliverable 3's README-documented local-build variant end-to-end.

### 8. NOTE for main session / release time — compose pinned tag

The PRD's own compose example pins `ghcr.io/killbus/reproxy:v0.3.0`,
but its Rollout section says existing tags (v0.1.0–v0.3.0) are never
re-pushed and "the first image ships with the NEXT tag". Followed the
example verbatim (v0.3.0) as instructed; the compose file therefore
becomes pullable only after... it is bumped to the first image-bearing
tag. Bump `docker-compose.yml` (and the README tag-semantics example,
which cites v0.3.0) as part of the next release's commit. The compose
smoke step's sed pattern matches `v[0-9.]+` generically, so it survives
that bump.

## Registry confirmation (user question mid-task)

No Docker Hub publish exists or was ever planned: the workflow logs in to
and pushes `ghcr.io/killbus/reproxy` only (GITHUB_TOKEN, packages:write).
Grep over all artifacts for docker.io / dockerhub / docker hub: zero
matches. Nothing was committed from this task (git log unchanged,
artifacts live in the working tree), so there is nothing to fixup or
rebase — the main session commits the whole task as the single commit
the PRD prescribes.

## Summary for the main session

Artifacts (all new unless noted):
- `Dockerfile` — golang:1.25 builder (CGO_ENABLED=0, -trimpath,
  -ldflags -X main.version=$VERSION, default dev) ->
  gcr.io/distroless/static-debian13:nonroot, USER nonroot, EXPOSE 8080,
  ENTRYPOINT ["/reproxy"]. One build path for humans and CI.
- `.dockerignore` — source-only context (VCS/tooling/docs/tests/
  binaries excluded). Filter simulated against the real tree: 15 files
  sent, 4167 dropped, no test/md/trellis leakage.
- `docker-compose.yml` — exactly image/command/ports/restart; PRD's
  four exclusions documented as comments.
- `.github/workflows/docker-publish.yml` — v* tag push +
  workflow_dispatch; build (load) -> 3 smokes -> push. Dispatch pushes
  only the ephemeral `dispatch-test` tag. Actions pinned at verified
  latest releases (see §5).
- `cmd/reproxy/main.go` + `main_test.go` — `--version` via
  versionRequested/printVersion seam, `var version = "dev"`, tests for
  guard table + default stamp + output shape.
- `README.md` — Docker section (run one-liner, compose example, tag
  semantics incl. immutability, local-build variant, distroless-not-
  scratch rationale).
- `.gitattributes` (modified) — eol=lf for Dockerfile/*.yml/.dockerignore.

Decisions taken (details in the sections above):
- build-push-action (twice: load, then push) rather than raw docker
  build/push: it is the one place annotations + build-args map to OCI
  labels correctly, and the smoke steps need `load: true` — raw docker
  would need a separate `docker buildx build --load` with hand-written
  annotation flags. Two steps, one action, fewer moving parts than
  `docker buildx build` invoked twice with long flag lists.
- dispatch version string: `dev-run<N>-<shortsha>` (never publishable
  as a release tag; carries provenance for debugging).
- dispatch pushes `dispatch-test` (ephemeral, overwritten every
  dispatch) instead of skipping push entirely: exercising the actual
  push path (login scope, package creation, annotations on push) is
  part of what the test channel exists to prove.
- Smoke redesign: public https upstream instead of the PRD's impossible
  local upstream (§3) + deterministic allowlist-gate check + compose
  build-variant check (§7).
- Distroless base: `static-debian13:nonroot` over the deprecated
  `static:latest` alias (§5a).

For the main session to validate on the runner (no local Docker):
- `workflow_dispatch` the workflow on main: all three smokes must pass;
  the run pushes ghcr.io/killbus/reproxy:dispatch-test.
- Inspect the pushed image's OCI annotations (revision = SHA,
  version = dev-run<N>-<sha>, source = repo URL).
- After the next tag: version tag + latest with tag-matching --version
  output; compose file still pins v0.3.0 (bump at release time, §8).

Gates: go build/vet/gofmt/test all green (twice — after --version and
after all changes). ci.yml untouched (git diff clean).

### 9. Mutation scan (see `mutation-scan.md` in this dir)

8 mutations against the `--version` semantics, isolated copy, all
captured — 0 survivors. Two survivors were found and closed during the
scan (banner-to-stderr at main's call site; exit code 1 instead of 0),
each by extending `TestMainVersionBannerOnStdout` (test-binary
reentrancy: child rewrites os.Args, calls main, parent asserts the
child's stdout and clean exit — the exact channel CI's smoke reads).
Restore byte-equality verified after every mutation; isolated copy
removed. The workflow/Dockerfile side has no runnable local tests (no
Docker); its capture channel is the runner smoke, owned by the main
session.

### 10. `.gitignore` bug found and fixed (pre-existing, load-bearing for this task)

The bare `reproxy` rule (meant to ignore the built binary at the repo
root) also matched the `cmd/reproxy` package DIRECTORY —
`git check-ignore` confirmed it would have silently excluded the new
`cmd/reproxy/main_test.go` from `git add .`. Anchored to `/reproxy`
(verified: root binary still ignored, the test file now visible as
untracked-and-addable).






