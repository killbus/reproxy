# Implement report — multi-arch Docker images (linux/amd64 + linux/arm64)

Task: .trellis/tasks/09-08-multi-arch-images (PRD: R6 kill-list defect fix)
Rule honored: appended per completed unit, not written at the end.

---

## Unit 1 — Dockerfile cross-compile

**File**: `Dockerfile`

**Change**:
- `FROM golang:1.25` → `FROM --platform=$BUILDPLATFORM golang:1.25` (builder
  stage always runs on the build machine's platform — no emulation for a
  foreign target).
- Added `ARG TARGETARCH` in the builder stage (BuildKit's automatic platform
  args are global-scope; a stage must redeclare one to use it — verified in
  Dockerfile reference, "Automatic platform ARGs in the global scope").
- `RUN CGO_ENABLED=0 go build` → `RUN CGO_ENABLED=0 GOARCH=${TARGETARCH} go build`.
- Header comment extended: documents the multi-arch cross-compile trick and
  that a plain `docker build` is still native-by-default (TARGETARCH defaults
  to the build machine's arch when no `--platform` is given).
- Runtime stage (distroless static-debian13:nonroot) unchanged — it is already
  a multi-arch base.

**Human path invariants**:
- `docker build -t reproxy:local .` unchanged: no --platform → TARGETARCH =
  build machine's arch → native binary. One build path preserved (PRD
  Deliverable 1).
- `--build-arg VERSION=...` unchanged.

**Local verification done**: none possible without Docker on this host
(windows dev box, no engine). Verified by construction against the Dockerfile
reference (automatic platform args; --platform=$BUILDPLATFORM documented at
dockerfile.md line ~651: "allow you to force a stage to native build platform
(--platform=$BUILDPLATFORM)"). The dispatch run is the runtime gate.

---

## Unit 2 — Workflow: two native matrix build jobs + digest-only push

**File**: `.github/workflows/docker-publish.yml` (job `build`)

**Shape**:
- `strategy.matrix.include`: exactly two fixed entries —
  `{platform: linux/amd64, runner: ubuntu-latest, arch: amd64}` and
  `{platform: linux/arm64, runner: ubuntu-24.04-arm, arch: arm64}`.
  Not a workflow input (no knob); adding an arch is a workflow edit.
  `arch` is bookkeeping for the per-leg job-output key.
- Steps per leg: Checkout → setup-buildx → login (GITHUB_TOKEN, unchanged) →
  `Compute version` (SAME event-keyed version logic; tags list NOT computed
  here — the merge job owns tags) → `Build image (loaded locally for smoke
  tests)` with `load: true`, `platforms: ${{ matrix.platform }}`,
  `tags: reproxy:smoke-${{ matrix.arch }}` → smoke step
  (unchanged script, env-var indirection preserved) → `Push image by digest
  only` → `Export digest to per-arch job output`.
- Digest-only push uses build-push-action v7.3.0's `outputs:` input
  (NOT push+tags):
  `outputs: type=image,name=ghcr.io/killbus/reproxy,push-by-digest=true,name-canonical=true,push=true`
  with NO `tags:` input — the pushed artifact exists only as
  `ghcr.io/killbus/reproxy@sha256:<digest>`, not pullable by name.
- Job outputs: `digest-amd64` / `digest-arm64` — DISJOINT keys per leg.
  Rationale (verified in GitHub workflow-syntax docs, "Using Job Outputs in
  a Matrix Job"): matrix job outputs are combined across legs, and
  identically-named outputs race ("the last matrix job that runs will
  override the output value" — order not guaranteed). The docs' own example
  keys outputs per leg value (`output_${version}`); I key per leg arch.
  Each leg's export step (`id: export`) emits exactly one key:
  `echo "digest-${ARCH}=${DIGEST}" >> "$GITHUB_OUTPUT"` via env indirection.

**Invariants preserved**:
- Event-keyed version: `push` → version = `${GITHUB_REF_NAME}`; dispatch →
  `dev-run<N>-<short SHA>` (can never collide with a release tag). A dispatch
  ON a tag ref still takes the dispatch path.
- Smoke-before-anything-pullable: the digest push step runs AFTER the smoke
  step in each leg; and digests are untagged until the merge job (Unit 3)
  runs, which requires BOTH legs green.
- The build step for the loaded image uses a local tag
  (`reproxy:smoke-amd64`), not a registry tag — nothing in the registry
  exists until the digest push.

## Unit 3 — Workflow: merge job (manifest list + tags)

**File**: `.github/workflows/docker-publish.yml` (job `merge`)

- `needs: build`, `runs-on: ubuntu-latest`.
- Steps: Checkout (the dispatch version string needs `git rev-parse`) →
  setup-buildx (installs the buildx plugin; `imagetools create
  --annotation` requires a current buildx) → login → `Compute version and
  image tags` (the SAME event-keyed logic as before, moved here verbatim:
  push → `:<tag>` + `:latest`, dispatch → `:dispatch-test` only) →
  `Create manifest list and push`.

**The imagetools command** (chosen shape: a plain `run:` step calling the
docker CLI — the simplest honest form; the alternative
build-push-action-with-`context: false` wrapper adds an action dependency to
express a single CLI call):

```sh
docker buildx imagetools create \
  -t <tag1> [-t <tag2>] \
  --annotation "index:org.opencontainers.image.revision=<sha>" \
  --annotation "index:org.opencontainers.image.version=<version>" \
  --annotation "index:org.opencontainers.image.source=<repoUrl>" \
  ghcr.io/killbus/reproxy@sha256:<amd64-digest> \
  ghcr.io/killbus/reproxy@sha256:<arm64-digest>
```

**Why this is safe with index-typed sources (the nesting question)**:
each build-push-action push carries a default provenance attestation
(v7.3.0 source, `src/context.ts` `getAttestArgs`: public repo + BuildKit
>= 0.11 + not a docker-exporter → `--attest type=provenance,mode=max`), so
each per-arch digest is an OCI *index* (image manifest + attestation
manifest), exactly like the existing v0.4.0 in GHCR (verified by anonymous
manifest fetch). `imagetools create` handles this correctly by design: in
buildx `util/imagetools/create.go` `combine()`, sources whose media type is
Docker manifest list / OCI index are UNPACKED and their child manifests are
re-added to the new index (no index-of-indexes nesting). The merge output
is one flat manifest list: amd64 image + amd64 attestation + arm64 image +
arm64 attestation.

**Annotation decision**: kept, not dropped, and extended. The three
`org.opencontainers.image.*` annotations stay on each arch's build (they
land on the child image manifests — verified against v0.4.0's child
manifest, which carries exactly revision/source/version). The merge step
ADDS the same three at the image-index level via
`--annotation "index:..."` (the only annotation prefix imagetools create
supports for the index; manifest-descriptor is the other supported prefix).
`org.opencontainers.image.revision/source/version` on the index make
`docker buildx imagetools inspect` (and registry UIs) show image metadata
without resolving children. Supported since buildx 0.12 (annotations
support); the runners' setup-buildx-action v4.3.0 installs a current buildx.

**Why needs.build gating is sufficient for smoke-before-pullable**:
`needs: build` means every matrix leg succeeded; a failed smoke fails its
leg's job, which fails the merge job (default fail-fast semantics for
`needs` is all-legs-required). Tags are created only in the merge step —
so no pullable tag exists before both smokes passed. (Digests pushed in
step "Push image by digest only" are untagged registry content, by design
not pullable by name; the PRD explicitly accepts digest-only pushes before
merge.)

**Digest output format**: build-push-action's `steps.push.outputs.digest`
is already `sha256:`-prefixed (main.ts `resolveDigest(metadata)` reads
`containerimage.digest`), so the merge command composes
`"${IMAGE}@${DIGEST}"` — NOT `@sha256:${DIGEST}` (which would double the
prefix).

---

## Unit 4 — README arch table

**File**: `README.md` (Docker section)

**Added**:
- `### Architectures` subsection right after the distroless rationale: a
  tag-range table — `v0.4.1` and later: amd64 yes / arm64 yes; `v0.4.0` and
  earlier: amd64 only. Plus three notes: same `docker run` works everywhere
  via manifest-list resolution (no platform flag), earlier tags never gain
  arm64 (immutability), CI builds each arch on a native runner (no
  emulation).
- One paragraph in "Building locally": a plain `docker build` is always
  native (the Dockerfile pins the build stage to $BUILDPLATFORM and picks
  the arch via GOARCH), and the `buildx build --platform` example for
  cross-compiling without QEMU.

**Fact base** (anonymous GHCR API, checked 2026-09-08): the repository's
only tags ever are `v0.4.0`, `latest`, `dispatch-test` — no images exist
before v0.4.0, so the "≤v0.4.0 is amd64-only" row is exactly true, not an
approximation. v0.4.0's manifest is an OCI index with a single amd64
image child (+ provenance attestation).

**Left for the main session (per task brief)**: pin bumps v0.4.0 → v0.4.1
in README examples + docker-compose.yml at tag time. The arch table already
references v0.4.1 textually.

---

## Unit 5 — Self-review: external verification, PRD AC audit, local gates

### Upstream facts verified before/during writing (citations)

1. **build-push-action v7.3.0** (README + `action.yml` + `src/main.ts` +
   `src/context.ts`, fetched from the v7.3.0 tag on raw.githubusercontent.com):
   - `outputs` is a listed input ("List of output destinations"), passed
     through as `--output <value>` args.
   - `digest` is a listed step output; `resolveDigest(metadata)` returns
     `containerimage.digest` — already `sha256:`-prefixed.
   - `annotations` input → `--annotation` args (buildx >= 0.12).
   - Default provenance: public repo + BuildKit >= 0.11 + non-docker
     exporter → `--attest type=provenance,mode=max` automatically (context.ts
     `getAttestArgs`). So a push-by-digest artifact is an OCI index
     (image + attestation) — the same shape as the already-published v0.4.0.
   - `outputs` parsing: action pins `@docker/actions-toolkit 0.92.0`;
     its `util.ts getList()` with `ignoreComma: true` (the opts used for the
     `outputs` input) keeps my comma-separated single-line value as ONE
     `--output` arg (multi-field CSV records are `join(',')`-ed back).

2. **Docker docs — Image and registry exporters**
   (docs.docker.com/build/exporters/image-registry/): `type=image`
   attribute keys `name`, `push`, `push-by-digest` ("Push image without
   name"), `name-canonical` — the exact form used:
   `type=image,name=...,push-by-digest=true,name-canonical=true,push=true`.

3. **buildx imagetools create** (docs.docker.com/reference/cli/docker/buildx/
   imagetools/create/ + buildx source `util/imagetools/create.go` at master):
   "Create a new manifest list based on source manifests. The source
   manifests can be manifest lists or single platform distribution
   manifests" — and in `combine()`, index-typed sources are UNPACKED and
   their child manifests re-added to the new index. Multiple digests → ONE
   flat manifest list; no nested index-of-indexes. `--annotation
   index:<key>=<val>` is the documented way to annotate the index (the
   only supported prefixes are `index:` and `manifest-descriptor:`).

4. **GitHub workflow syntax — "Using Job Outputs in a Matrix Job"**
   (docs.github.com actions workflow-syntax): matrix job outputs "will be
   combined from all jobs inside the matrix"; the warning states
   identically-named outputs race ("the last matrix job that runs will
   override the output value"). The docs' own example keys outputs per leg
   (`output_${version}`) and shows the combined result holding every leg's
   value — an absent output in one leg does not clobber another leg's
   value. My per-arch keys follow exactly that documented shape.

5. **Dockerfile reference — "Automatic platform ARGs in the global scope"**
   (docs.docker.com/reference/dockerfile/): `TARGETARCH` is auto-set from
   the requested platform; must be redeclared inside a stage; the docs'
   own cross-compile example uses `FROM --platform=$BUILDPLATFORM` +
   `ARG TARGETARCH` + `GOARCH` (the same pattern implemented).

6. **GHCR anonymous registry API** (checked 2026-09-08): the repository's
   complete tag set is `["dispatch-test", "v0.4.0", "latest"]` — no image
   exists before v0.4.0, and v0.4.0's manifest is an OCI index with exactly
   one amd64 image child + one `unknown/unknown` provenance attestation
   child; the three `org.opencontainers.image.*` annotations live on the
   amd64 child manifest (not the index).

7. **docker/github-builder production workflow** (`.github/workflows/
   build.yml` @ main, fetched 2026-09-08) — the pattern Docker now ships
   and recommends — corroborates the two riskiest mechanics with production
   usage:
   - Per-leg matrix job outputs: `result_0` … `result_19` with the comment
     "needs predefined outputs as we can't use dynamic ones atm"
     (actions/runner PR 2477) — each matrix leg writes only its own
     indexed key; the finalize job reads ALL of them from
     `needs.build.outputs`. Same shape as my `digest-amd64` /
     `digest-arm64`.
   - Digest-only push output, byte-for-byte the same attribute set I use:
     `type=image,"name=...",oci-artifact=true,push-by-digest=true,
     name-canonical=true,push=...`.
   - Its finalize job runs `imagetools create` with every leg's digest as
     source plus `index:`-prefixed annotations — the same merge shape
     (they call it through a JS helper; the underlying CLI contract is
     the one cited in item 3).
   - Note: github-builder defaults arm platforms to `ubuntu-24.04-arm`
     — the runner label this task uses.

   Fetch method note: WebFetch is not in this agent's tool catalog; all
   upstream docs/source were retrieved with `curl` against the exact
   version tags (v7.3.0 / v4.3.0 / v0.92.0 / master) and parsed locally.

### Provenance/annotation decision (explicit)

- Default provenance attestation is KEPT (not disabled with
  `provenance: false`). Reasons: (a) v0.4.0 already ships with it — keeping
  the default is zero behavior change for the artifact shape; (b) the PRD's
  scope is packaging breadth, not attestation policy; (c) disabling would
  be a second, unstated change riding along. Consequence for AC 4: the
  merged `dispatch-test` manifest list will have 4 child entries — 2
  platform-bearing image manifests (amd64 + arm64) and 2 `unknown/unknown`
  attestation manifests. "Exactly 2 platforms" holds (platform count, not
  raw manifest count); `docker pull` on either arch resolves the right
  image. The verifier should count `.manifests[] | select(.platform.os ==
  "linux")` = 2.
- Annotations: the three `org.opencontainers.image.{revision,version,
  source}` stay on each arch build (child manifest placement, same as
  every release to date — verified on v0.4.0) AND are added at the index
  level by the merge step via `--annotation "index:..."`. The docker docs
  guide for imagetools annotations documents exactly this prefix.

### PRD Acceptance Criteria audit (what was / was not checkable locally)

| AC | Status locally |
|---|---|
| 1. Dockerfile `FROM --platform=$BUILDPLATFORM` + `ARG TARGETARCH` + `GOARCH`, plain build still works (documented), runtime stage unchanged | Implemented + documented (README "Building locally"); runtime proof needs Docker — dispatch run |
| 2. Two native matrix jobs; unchanged smoke script per arch; digests pushed untagged; merge tags only after both smokes pass | Implemented; GH Actions semantics proof — dispatch run |
| 3. Dispatch verification (both arches smoke green, merge tags dispatch-test) | NOT checkable locally (no Docker, no runners) — main session |
| 4. Anonymous manifest check (list, 2 platforms, children resolvable) | NOT checkable until dispatch — main session (note the attestation shape above) |
| 5. README arch table + notes; pins at v0.4.1 at release | Table + notes done; pin bumps are main-session at tag time |
| 6. Tag v0.4.1 after CI green; release notes | Main session |
| 7. Local gates: YAML parses, bash -n smoke (unchanged), go test green | ALL GREEN (below) |

### Local gates (all run 2026-09-08, final pass)

- `go build ./...` — OK
- `go vet ./...` — OK
- `gofmt -l .` — empty
- `go test ./...` — `ok reproxy`, `ok reproxy/cmd/reproxy`
- `bash -n scripts/docker-smoke.sh` — OK
- `python yaml.safe_load` on docker-publish.yml and ci.yml — parses; no
  duplicate keys; no tabs; no CRLF (LF-only per .gitattributes rules)
- `git diff scripts/docker-smoke.sh` — **0 bytes (the deliverable proof of
  zero change)**
- Every `run:` block in the workflow extracted and `bash -n`-checked —
  all OK
- Merge tag-loop + full imagetools command simulated with real values
  (v0.4.0's actual digest) — composition is byte-exact to the intended
  command line
- `git status` — only Dockerfile, .github/workflows/docker-publish.yml,
  README.md modified; nothing staged, nothing committed

### Mutation scan — not applicable, and why

The quality-guidelines mutation-scan discipline applies to pinned Go
semantics with capturing tests. This task changes zero Go product code and
zero tests (PRD: "packaging breadth, zero product-code change"), so there
is no suite whose red/green discriminance needs proving. The runtime gate
for the changed surfaces is the dispatch run (AC 3/4), which the main
session triggers; the local gates above are the full local proof
obligation and are green.

### Known deviations from the design sketch (with justification)

1. **Third matrix field `arch`** (sketch said `{platform, runner}` pairs):
   bookkeeping only — it keys the per-leg job output
   (`digest-${arch}`). Without disjoint keys the two legs would race on one
   `digest` output (GitHub docs warning, cited above). Not a knob: fixed
   data, not an input.
2. **Smoke image tag is local** (`reproxy:smoke-<arch>`), not the registry
   tag: build jobs no longer own tags (the merge job does), so the smoke
   step needs a local tag for the loaded image. The smoke script contract
   is untouched — it takes any image reference.
3. **Merge job has setup-buildx**: not in the sketch, needed because
   `docker buildx imagetools create --annotation` requires the buildx
   plugin (not preinstalled at a guaranteed version on the runner).
4. **Simplified smoke-step tag extraction**: the old
   `first_tag=$(echo ... | head -n1)` existed because `tags` was a
   multi-line list; the smoke image is now a single local tag, so the
   extraction would be dead code. Env-indirection discipline preserved.

### Remaining for the main session

1. Trigger one workflow_dispatch run; require all-green (both arch legs
   smoke + merge).
2. Anonymous manifest check on `ghcr.io/killbus/reproxy:dispatch-test`
   (count platform-bearing children = 2; amd64 + arm64 both resolvable).
3. v0.4.1 tag + compose/README pin bumps at release time.
4. Release notes: amd64-only history for <= v0.4.0; first multi-arch tag
   v0.4.1; banner exact-match on both arches in the publish logs.
