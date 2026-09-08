# PRD — Multi-arch Docker images: linux/amd64 + linux/arm64 via native runners

## Origin — the R6 kill-list defect, corrected

R6 put multi-arch on the kill list with a two-part justification, both parts
defective on audit (2026-09-08, owner challenge "竟然没有发布 arm 镜像？"):

1. **Self-referential signal.** "First arm64 pull request is the signal" —
   arm users cannot pull an image that was never published; the signal was
   designed to never arrive. Not "wait for demand" but "delete the
   precondition for demand".
2. **Stale cost premise.** Free GitHub-hosted arm64 runners for public repos
   (`ubuntu-24.04-arm`, Graviton, 4 vCPU/16GB) have existed since 2025-01;
   reproxy is a public repo. And CGO_ENABLED=0 cross-compilation needs no
   QEMU — the "multi-arch = slow emulation" premise was 2023-era. The
   classification error: arm was bucketed with cosign/SBOM (audience: people
   who would verify signatures — genuinely nonexistent) instead of with
   build-matrix breadth (audience: every M-series Mac, Graviton instance,
   Raspberry Pi — real hardware that exists today).

**Owner challenge is itself the demand signal.** Scope: linux/amd64 +
linux/arm64. Nothing else.

## Deliverables

1. **Dockerfile** — cross-compile with zero behavior change for humans:

   ```dockerfile
   FROM --platform=$BUILDPLATFORM golang:1.25 AS builder
   ARG TARGETARCH
   ARG VERSION=dev
   RUN CGO_ENABLED=0 GOARCH=${TARGETARCH} go build ...
   ```

   A plain `docker build` on any machine still works and produces a native
   image (`TARGETARCH` defaults to the build platform) — the one-build-path
   principle survives; multi-arch is a CI-only concern (two native jobs, no
   QEMU anywhere).

2. **docker-publish.yml** — two-NATIVE-job matrix:

   - job `build` (strategy: matrix: platform: [linux/amd64 on ubuntu-latest,
     linux/arm64 on ubuntu-24.04-arm]): checkout, buildx, login, build
     `load: true` with `platforms: ${{ matrix.platform }}`, run
     **`scripts/docker-smoke.sh` unchanged** (the extraction payoff — same
     script, now on both arches; on the arm runner it exercises the arm64
     binary end-to-end through the CA handshake), then `push: true` by
     **digest only** (single-platform manifest pushed to the registry under
     `ghcr.io/killbus/reproxy@sha256:...`, NOT under any pullable tag).
   - job `merge` (needs: [build], runs-on: ubuntu-latest): docker buildx
     imagetools create — merges the two digests into ONE manifest list,
     tags it `${IMAGE}:${version}` + `${IMAGE}:latest` (tag path) or
     `:dispatch-test` (dispatch path). **Nothing pullable by tag exists
     until both arches' smoke passed** — the smoke-before-push guarantee
     extends to the merged artifact.
   - Event-keyed version/tags logic moves to the merge job unchanged
     (push → v-tag + latest; dispatch → dispatch-test).
   - ci.yml untouched.

3. **README** — Docker section: arch table (amd64 + arm64, since v0.4.1);
   one sentence on native-builders matrix (no emulation); note v0.4.0 and
   earlier are amd64-only. `docker run`/compose examples unchanged
   (multi-arch manifests make `ghcr.io/killbus/reproxy:v0.4.1` work
   everywhere transparently).

4. **Compose/pins** — compose + README pins bump v0.4.0 → v0.4.1 at release
   time (the standing bump rule; this task ships the tag so it carries the
   bump).

## Constraints

- **No QEMU** anywhere (native runners only — the stale-premise fix).
- **No new knobs**: platforms are a fixed 2-entry matrix, not an input; no
  SMOKE_PORT-style env, no per-arch smoke variants.
- **scripts/docker-smoke.sh is NOT modified** — pure reuse (any needed
  change is a defect in this design, not in the script).
- **Tag discipline**: v0.4.0 is immutable history — it stays amd64-only
  forever. First multi-arch tag is v0.4.1 (patch bump: packaging breadth,
  zero product-code change).
- **Kill list stays dead** for everything else: no cosign/SBOM/scanning/
  caches/multi-arch-beyond-arm64 — this task is breadth, not verification
  infrastructure.
- Actions pins at latest (existing rule); verify docker/build-push-action
  and setup-buildx-action support the digest-push + imagetools flow at
  their pinned versions.

## Acceptance Criteria

1. Dockerfile: `FROM --platform=$BUILDPLATFORM`, `ARG TARGETARCH` +
   `GOARCH=${TARGETARCH}`, plain `docker build` still works (documented);
   distroless runtime stage unchanged.
2. Workflow: two native matrix jobs; each runs the unchanged smoke script
   against its own arch image; digests pushed untagged; merge job creates
   the tagged manifest list only after both smokes pass.
3. **Dispatch verification** (the runtime gate, dev host has no Docker):
   one workflow_dispatch run fully green — both arch jobs smoke (version
   banner exact match, round-trip 200 + marker, gate 403, compose variant
   green on BOTH amd64 and arm64 runners), merge job tags dispatch-test.
4. Anonymous manifest check: `ghcr.io/killbus/reproxy:dispatch-test`
   returns a **manifest LIST** (mediaType `...manifest.list.v2+json` or
   OCI index) with exactly 2 platforms (amd64 + arm64), both children
   resolvable (blob checks 200).
5. README arch table + notes present; compose/README pins at v0.4.1 at
   release time.
6. Tag v0.4.1 pushed after CI green; the v0.4.1 release publishes the
   first multi-arch image (banner exact-match on both arches in the publish
   logs); Release notes state amd64-only history for ≤v0.4.0.
7. Local gates: YAML parses; bash -n on the smoke script (unchanged file —
   proof by absence of diff); go test untouched-green.

## Kill list (this task)

- No QEMU/emulation, no buildx `platforms:` multi-arch emulation fallback
  "for simplicity".
- No new smoke variants, no per-arch script parameters.
- No cache-from/to "while we're in there" (still zero deps — nothing to
  cache).
- No v0.4.0 re-push / tag rewrite of any kind.
- No riscv64/s390x/v7 "while we're in there" (arm64 is the challenged
  scope; each further arch waits for its own signal).
