# PRD: Docker image build, compose file, and GHCR publish workflow

## Problem

reproxy ships a single static binary (Go stdlib only, CGO_ENABLED=0). Deployment environments that have Docker but no Go toolchain — the product's first real hire (a stranger trying it out) — cannot run it. Docker packaging closes that gap: a Dockerfile, a docker-compose.yml, and a GitHub Actions workflow that publishes images to GHCR on release.

Owner's requirement on the workflow: **the actions it uses must be at their latest versions** — the workflow fetches its actions (actions/checkout, docker/login-action, …) from their GitHub repository URLs, and stale `@v2`-era pins copied from old blog posts are a common defect. Practice shape was converged via chatroom round 6 (Fielding/Hickey/Brooks/Christensen/Cunningham, 5/5): one Dockerfile build path for humans and CI, tag-triggered publish with SHA/version stamped into the image, distroless-static runtime, a minimal compose file, a build-time smoke test as the only pinned failure mode, and a kill list of phantom-audience rituals (multi-arch, cosign/SBOM, scanning, cache mounts) deferred until a real pull-count signal exists.

## Deliverables

### 1. Dockerfile (multi-stage, one build path for humans and CI)

```
builder:  golang:<version matching go.mod> — CGO_ENABLED=0, go build ./cmd/reproxy,
          ldflags -X main.version=<arg> (default "dev")
runtime:  gcr.io/distroless/static — COPY --from=builder /out/reproxy /reproxy,
          USER nonroot, ENTRYPOINT ["/reproxy"]
```

- Version injection: the binary MUST report its version (add a `--version` flag if absent — prints the ldflags-stamped version). Default when built outside release: "dev".
- distroless static, NOT scratch: reproxy performs outbound HTTPS with certificate validation (ssrf.go transport); a bare scratch image has no CA roots and dies on the first https upstream. (Round-6 verdict: scratch died on contact with the product fact.)
- Non-root user. EXPOSE 8080.
- Self-contained: `docker build .` works with no pre-built artifacts.

### 2. .dockerignore

Exclude .git, .trellis, *.md, tests — the build context is source only. (The "don't ship dirty workspace" guarantee; Brooks: "everything the URL-fetch requirement promises, two lines of .dockerignore provide".)

### 3. docker-compose.yml (executable README)

Minimal sufficient set — exactly these four keys:

```yaml
services:
  reproxy:
    image: ghcr.io/killbus/reproxy:v0.3.0   # pinned version
    command: ["--allowlist", "api.example.com"]  # the product's only mandatory config
    ports: ["8080:8080"]
    restart: unless-stopped
```

- NO volumes (stateless — volumes lie), NO env vars (second config channel), NO healthcheck (no shell in distroless; unexecutable), NO depends_on/networks.
- README documents the local-build variant (`build: .` in place of `image:`); the compose file itself pins the published image.

### 4. .github/workflows/docker-publish.yml

- **Trigger**: push of tags matching `v*`, plus `workflow_dispatch` (manual testing without a release; first run is validated this way).
- **Build**: uses the SAME Dockerfile (one recipe, no drift between local and CI builds).
- **Version stamping**: `--build-arg VERSION=${GITHUB_REF_NAME}`; OCI annotations `org.opencontainers.image.revision` = commit SHA, `org.opencontainers.image.version` = tag, `org.opencontainers.image.source` = repo URL.
- **Publish**: `ghcr.io/killbus/reproxy:${tag}` + `ghcr.io/killbus/reproxy:latest`, using GITHUB_TOKEN with `permissions: packages: write` (minimal scope, no PAT).
- **Smoke test (the only pinned failure mode — the image version must not lie)**:
  1. `docker run --rm ghcr.io/killbus/reproxy:<tag> --version` → output equals the git tag exactly.
  2. Round-trip: start the container with `--allowlist 127.0.0.1` (or `--dangerous-allow-all`) pointing at a trivial local test upstream started in the job; issue one real request through the proxy; assert the expected response. Proves binary alive, server up, allowlist gate working, forwarding works.
  - Push happens ONLY after smoke passes.
- **No** multi-arch (amd64 only — first arm64 pull request is the signal to add it), no cosign/SBOM (no verifiers exist), no vulnerability scanning (near-empty static image), no cache mounts/gha cache (zero deps: nothing to cache at tag frequency).

### 5. README section

A "Docker" section after the build/install instructions: `docker run` one-liner (with --allowlist), the compose example, the GHCR tag semantics (version tags immutable — pushed once, never re-pushed; latest = newest release), and the local-build variant.

## Constraints

- Go stdlib only applies to the product; workflow YAML may use standard actions (actions/checkout, docker/login-action, docker/setup-buildx-action, docker/build-push-action or raw docker commands — prefer the fewest moving parts consistent with the deliverables).
- **Actions pinned at their latest released versions** (verified at PRD time, 2026-09): `actions/checkout@v7.0.1`, `docker/login-action@v4.6.0`, `docker/setup-buildx-action@v4.3.0`, `docker/build-push-action@v7.3.0`. Pin the minor-qualified tag (e.g. `@v7.0.1`), not a bare major (`@v7`) — the fresh-fetch requirement means the workflow references the current latest release of each action, and the implementer should re-verify against `gh api repos/<repo>/releases/latest` at implementation time rather than trusting this table blindly.
- The existing test workflow (ci.yml) is untouched.
- Tag semantics and registry path (ghcr.io/killbus/reproxy) are the irreversible decision — fixed by this PRD.
- Existing tags (v0.1.0–v0.3.0) are never re-pushed; the first image ships with the NEXT tag.

## Acceptance Criteria

- [ ] `docker build .` produces a distroless-static, non-root image whose binary reports the injected version (`docker run --rm <img> --version`). Validation channel: the dev host has no Docker — the build+smoke run is validated via `workflow_dispatch` on GitHub (the same job the tag trigger runs), not locally.
- [ ] Compose file contains exactly image/command/ports/restart; `docker compose up` (build: variant) starts a serving proxy — validated on the GitHub runner within the dispatched workflow (compose is exercised there, not on the Docker-less dev host).
- [ ] Workflow dispatch on a test basis: builds the image from the checkout of the ref, smoke passes (version assertion + request round-trip), publish succeeds.
- [ ] First real `v*` tag push after merge: image `ghcr.io/killbus/reproxy:<tag>` + `:latest` published with correct OCI annotations; smoke ran before push.
- [ ] No multi-arch / cosign / SBOM / scanning / cache mounts present in the workflow (the phantom list — grep-verifiable absence).
- [ ] Every `uses:` reference in the workflow resolves to the action's latest released version (minor-qualified pin), re-verified at implementation time.
- [ ] README Docker section present with run/compose/tag-semantics/local-build.
- [ ] Existing test workflow untouched; full gates (build/vet/gofmt/test) still green; CI green on both OS.
- [ ] `--version` flag added to cmd/reproxy if absent (default "dev"), tested.

## Rollout

Single task, single commit. The first image-bearing release is the next tag (version number decided at release time — packaging adds an artifact, it does not change the product surface). Tags are never re-pushed; workflow_dispatch is the testing channel before that.
