# Check report — multi-arch Docker images (linux/amd64 + linux/arm64)

Task: .trellis/tasks/09-08-multi-arch-images
Check agent: adversarial static verification (Windows host, no Docker — runtime
gate is the workflow_dispatch run the main session triggers).
Method note: every "verified" claim below was re-derived from primary sources
(actions/runner source, docker/actions-toolkit source at the pinned version,
buildx v0.37.0 source, anonymous registry API calls) or executed locally —
the implement report's citations were treated as claims, not evidence.

---

## A. Dockerfile correctness

**A.1 `FROM --platform=$BUILDPLATFORM golang:1.25 AS builder` + `ARG TARGETARCH` + `GOARCH=${TARGETARCH}` — traced on both machines:**

- Plain `docker build` on amd64: no `--platform` requested → BuildKit's
  automatic platform args take the build machine's values → `TARGETARCH=amd64`
  → `GOARCH=amd64` → native binary. Builder stage pulls linux/amd64 golang
  (BUILDPLATFORM = amd64). Identical artifact to the old Dockerfile.
- Plain `docker build` on arm64 (M-series Mac, Graviton): `TARGETARCH=arm64`
  → `GOARCH=arm64` → native arm64 binary; builder stage pulls linux/arm64
  golang. The OLD Dockerfile behaved the same on this path (FROM without
  --platform defaults to target platform). No behavior change for humans.
- `ARG TARGETARCH` re-declared inside the stage: required — automatic platform
  args are global-scope and must be redeclared per stage to be referenced
  (Dockerfile reference, "Automatic platform ARGs in the global scope").
  Correct.
- `--platform=$BUILDPLATFORM` only changes behavior when building for a
  FOREIGN platform (CI single-arch leg or `buildx --platform`): builder stage
  stays native, GOARCH selects the target. Verified this is the docs' own
  cross-compile example pattern.

**A.2 Runtime stage base is multi-arch — verified against the registry:**

Anonymous manifest fetch of `gcr.io/v2/distroless/static-debian13/manifests/nonroot`:
`Content-Type: application/vnd.oci.image.index.v1+json` (HTTP 200), 6 children:
linux/amd64, linux/arm/v7, linux/arm64/v8, linux/ppc64le, linux/riscv64,
linux/s390x. **linux/arm64 (variant v8) present** — the arm64 leg resolves its
runtime base. Note: distroless lives on gcr.io (not Docker Hub); the initial
Docker Hub probe 401'd, which was my error, not a defect.
`golang:1.25` also verified multi-arch (linux/arm64 child present in the
Docker Hub manifest list).

**A.3 One-build-path principle: preserved.** No CI-only syntax in the
Dockerfile; `ARG TARGETARCH` has no default that changes plain builds;
`--build-arg VERSION=...` unchanged; runtime stage untouched (git diff shows
zero changes below the builder stage). Zero product-code change (git diff
touches no .go files).

---

## B. Workflow correctness — full data-flow trace

**B.1 Matrix outputs — each leg exports only its own key. VERIFIED, three
independent sources:**

- Workflow text: the single export step emits exactly one line,
  `echo "digest-${ARCH}=${DIGEST}" >> "$GITHUB_OUTPUT"` — amd64 leg writes
  `digest-amd64`, arm64 leg writes `digest-arm64`. Disjoint.
- GitHub workflow-syntax docs ("Using Job Outputs in a Matrix Job"): the docs'
  own example statically declares ALL output keys on the matrix job
  (`output_1..3`) while each leg emits only its own via
  `echo "output_${version}=..."`, and the combined result holds every leg's
  value (`{"output_1":"1","output_2":"2","output_3":"3"}`). This is exactly
  this workflow's shape — each leg's empty evaluation of the OTHER leg's key
  does not clobber (the docs' displayed combined output proves the merge
  semantics). The last-writer-wins warning applies only to identically-named
  outputs, which this design avoids.
- Production corroboration: docker/github-builder `build.yml` @main uses the
  identical shape — static `result_0..result_19` job outputs, each leg writing
  only `result_${index}` via github-script, finalize job consuming
  `toJSON(needs.build.outputs)` (and JSON.parsing every key — any
  empty-clobber would throw in Docker's own production CI).

**Hyphenated output keys (`digest-amd64`) — lexer-level verification** (this
was my highest-concern static risk; a misparse would fail only at dispatch
time):

- actions/runner expression lexer (`src/Sdk/Expressions/Tokens/
  LexicalAnalyzer.cs` @main): `TestTokenBoundary` does NOT treat `-` as a
  boundary; `ReadKeywordToken` + `ExpressionUtility.IsLegalKeyword` allow `-`
  after the first char; a keyword following a `.` Dereference token becomes a
  `PropertyName` token. So `steps.export.outputs.digest-amd64` and
  `needs.build.outputs.digest-amd64` parse as property access, NOT as
  subtraction.
- `SetOutputFileCommand` (`FileCommandManager.cs` @main): GITHUB_OUTPUT lines
  are split at the first `=` with NO name validation; hyphenated keys store
  fine (L0 tests show arbitrary `MY_OUTPUT` keys). `StepsContext._propertyRegex`
  (`^[a-zA-Z_][a-zA-Z0-9_]*$`) only affects the auto-generated reference form
  (bracket vs dot) — not storage, not the workflow's own expressions.
- Schema: schemastore `normalJob.outputs` is `additionalProperties: {type:
  string}` — no key pattern; the stricter `workflow_call` output_id pattern
  `^[_a-zA-Z][a-zA-Z0-9_-]*$` explicitly allows hyphens. `digest-amd64` /
  `digest-arm64` match it.

**B.2 The two build invocations use identical content inputs. VERIFIED by
side-by-side read of both steps:**

| input | smoke build | push build |
|---|---|---|
| context | `.` | `.` |
| file | `./Dockerfile` | `./Dockerfile` |
| platforms | `${{ matrix.platform }}` | `${{ matrix.platform }}` |
| build-args | `VERSION=${{ steps.meta.outputs.version }}` | identical |
| annotations | revision=github.sha, version=meta.version, source=repositoryUrl | identical (byte-identical block) |

Differences are exporter-only: `load: true` + `tags: reproxy:smoke-<arch>`
(local docker load, tag is NOT part of image content) vs
`outputs: type=image,...` (registry push). The annotations-identical
cache-hit claim is correct in kind: annotations land on the image manifest
(verified on v0.4.0's child manifest — it carries exactly these three), so
identical annotations + identical VERSION build-arg + identical context
yield the same manifest digest; the second build is at minimum a layer-cache
hit and at worst a rebuild of the same bytes. Provenance differs by design
(docker exporter skips it, image exporter gets mode=max) but attestations are
separate manifests — the image manifest digest is unaffected. The smoked
image and the pushed image are the same manifest.

**B.3 The `outputs:` string — comma-safety verified from the pinned
toolkit source.** build-push-action v7.3.0 pins `@docker/actions-toolkit
0.92.0`; `src/context.ts` reads `outputs: Util.getInputList('outputs',
{ignoreComma: true, quote: false})`. Toolkit 0.92.0 `Util.getList` (src/util.ts,
read in full): the CSV parse splits the comma-containing value into 5 fields,
`record.length != 1`, `ignoreComma` is set → `res.push(record.join(','))` —
reassembled to the exact original string, passed as ONE `--output` arg
(getBuildArgs: `args.push('--output', output)`). No `tags:` input on the push
step (verified in YAML). `outputs` is a declared action input (action.yml
line 66); `digest` is a declared step output, and `resolveDigest` returns
`metadata['containerimage.digest']` — already `sha256:`-prefixed (main.ts +
build.ts resolveDigest, both read). The merge composes `"${IMAGE}@${DIGEST}"`
without re-prefixing — correct.

**B.4 Merge job.** `needs: build`, ubuntu-latest. Event-keyed logic
byte-identical to the old workflow's `Compute version and image tags` step
(verified by extracting both `run:` strings via yaml.safe_load and diffing —
the multi-line `tags` assignment including the column-0 continuation line is
byte-identical; push → `:<version>` + `:latest`, dispatch → `:dispatch-test`
only). The `tags` heredoc output contains NO leading whitespace on the
continuation line (verified in the parsed string). The imagetools command
composes `${IMAGE}@${DIGEST_AMD64}` + `${IMAGE}@${DIGEST_ARM64}`. tag_args
array-building simulated with real values under `bash -e`:
- push path → `-t ...:v0.4.1 -t ...:latest` (2 tags), rc 0
- dispatch path → `-t ...:dispatch-test` (1 tag), rc 0
- blank-line variant → empty lines skipped, rc 0
- empty-TAGS hypothetical → zero-length array, no spurious exit

**B.5 `bash -e` hazards.** The while-read loop uses the if-form (no `&&`
short-circuit to leave a non-zero status); the while CONDITION is exempt
from -e; `done <<< "$TAGS"` here-strings always succeed. The meta steps use
if/else/fi — same pattern the old workflow ran green in production (it
published v0.4.0 and dispatch-test). The export step is a bare `echo >>`
(always succeeds). Every `run:` block extracted and `bash -n`-checked: all
OK.

**B.6 Event-keying on a dispatch of a tag ref.** `GITHUB_EVENT_NAME` is
`workflow_dispatch` on any dispatch run regardless of ref → else-branch →
version=`dev-run<N>-<sha>`, tags=`:dispatch-test` only. Identical logic in
both build (version only) and merge (version + tags) jobs. Version
consistency between jobs: same GITHUB_RUN_NUMBER, same commit (both
checkouts) → identical version strings → banner check and annotations
coherent.

**Provenance default confirmed from source** (context.ts `getAttestArgs`):
public repo + BuildKit >= 0.11 + non-docker-exporter → `--attest
type=provenance,mode=max`. So each pushed per-arch digest is an OCI index
(image + attestation) — the same shape as v0.4.0 in GHCR (verified
anonymously: OCI index, 1 amd64 image child + 1 unknown/unknown child).
`imagetools create` combine() at buildx v0.37.0 (util/imagetools/create.go,
read in full): index-typed sources are UNPACKED (`for _, d := range
mfst.Manifests { addDesc(d, src) }`) — one flat index, no index-of-indexes.
The merged list will carry 4 children: 2 platform images + 2 attestation
manifests. `index:`-prefixed annotations are the documented form and are
allowed on OCI indexes in combine(). The runner images (ubuntu-24.04 AND
ubuntu-24.04-arm AND ubuntu-26.04) preinstall Docker-Buildx 0.36.1 (>= 0.12
required for --annotation) — and setup-buildx-action v4.3.0 skips download
when buildx is available, so the merge job runs a >= 0.12 buildx either way.

---

## C. Smoke-before-pullable invariant — PROVEN from the YAML alone

Trace of every registry-mutating action in the file:

1. Build leg step order is fixed: Checkout → setup-buildx → login → Compute
   version → Build (load, LOCAL tag `reproxy:smoke-<arch>` — not a registry
   ref) → **Smoke** → Push-by-digest → Export. The push step CANNOT run
   before its own leg's smoke passed (sequential steps; a failed smoke fails
   the job).
2. The digest push has no `tags:` input; `push-by-digest=true` pushes
   `ghcr.io/killbus/reproxy@sha256:...` only. Untagged registry content is
   not pullable by name.
3. Tags exist ONLY in the merge job's single `imagetools create` step.
   `needs: build` requires ALL matrix legs to succeed (default needs
   semantics). A failed smoke fails its leg → merge is skipped → no tag.

Failure-path analysis:

- One leg fails/canceled (fail-fast default): the other leg may have pushed
  its untagged digest. Harmless — not pullable by name, GHCR garbage-collects
  untagged manifests. Acceptable; the PRD explicitly accepts digest-only
  pushes before merge.
- Both legs green, merge fails mid-command: both smokes already passed, so
  the invariant (nothing pullable BEFORE both smokes pass) is not violated;
  partial tag application (multi -t) leaves a correct manifest under some
  tags; re-running the merge re-applies all tags (digests are
  content-addressed and stable).
- Re-run semantics: re-running failed jobs re-executes only failed legs +
  merge; digests reproduce from identical inputs; no stale-tag path exists
  because tags are only ever written by the merge step.

Verdict: the invariant holds, and is STRICTER than the old single-job
workflow (a tag now additionally waits for BOTH arches).

---

## D. PRD AC audit

| AC | Status |
|---|---|
| 1. Dockerfile shape + plain-build documented + runtime stage unchanged | **Locally verified** (A.1–A.3; runtime proof at dispatch) |
| 2. Two native matrix jobs; unchanged smoke per arch; untagged digests; merge tags only after both smokes | **Locally verified statically** (B.1–B.5, C); runtime confirmation at dispatch |
| 3. Dispatch run fully green (both arch legs smoke, merge tags dispatch-test) | **Runtime gate — main session** |
| 4. Anonymous manifest check (list, exactly 2 platforms, children resolvable) | **Runtime gate — main session.** Note: count platform-bearing children = 2 (4 raw manifests incl. 2 attestations — provenance default verified from source; the implement report's verifier note is correct) |
| 5. README arch table + notes; pins v0.4.1 at release | **Locally verified**: table + 3 notes + cross-compile paragraph present; compose pin still v0.4.0 (correct — bump at tag time per the standing rule) |
| 6. Tag v0.4.1 after CI green; release notes | **Runtime gate — main session** |
| 7. YAML parses; bash -n smoke (unchanged); go test green | **Locally verified — all green** (below) |

---

## E. Kill-list compliance

Greps over Dockerfile + docker-publish.yml + README.md:

- No QEMU/setup-qemu: only prose mentions ("no QEMU anywhere"). CLEAN.
- No `platforms` as workflow input: `workflow_dispatch:` has no `inputs:`
  block at all; the matrix is a fixed 2-entry include. CLEAN.
- No caches: no cache-from/cache-to/mount=type=cache (only the kill-list
  comment). CLEAN.
- No armv7/riscv64/s390x/ppc64le: zero matches. CLEAN.
- No new actions beyond the pinned set: uses are exactly checkout@v7.0.1,
  setup-buildx-action@v4.3.0, login-action@v4.6.0, build-push-action@v7.3.0 —
  the same four the old workflow already used. CLEAN.
- ci.yml untouched: `git status` shows only Dockerfile, docker-publish.yml,
  README.md modified. CLEAN.
- scripts/docker-smoke.sh unchanged: `git diff scripts/docker-smoke.sh` =
  0 bytes. CLEAN.

---

## F. README accuracy

Fact base verified anonymously against GHCR (method: ghcr.io token dance,
tags/list + manifest fetch, 2026-09-08):

- Complete tag set: `["dispatch-test", "v0.4.0", "latest"]` — no images
  before v0.4.0. The "≤v0.4.0 amd64-only" row is exactly true.
- v0.4.0 manifest: OCI index with exactly one linux/amd64 image child + one
  unknown/unknown attestation child; the three `org.opencontainers.image.*`
  annotations live on the child image manifest. README's claims match
  registry reality.
- Native-builders sentence ("CI builds each architecture on its own native
  runner (no emulation)"): matches the workflow (amd64 → ubuntu-latest,
  arm64 → ubuntu-24.04-arm; Dockerfile cross-compiles via GOARCH with
  $BUILDPLATFORM builder). Docker's own multi-platform guide corroborates
  the runner mapping ("default runner mapping sends Linux Arm platforms to
  ubuntu-24.04-arm").
- Cross-compile paragraph: accurate — CGO_ENABLED=0 + $BUILDPLATFORM builder
  means `buildx build --platform linux/arm64` needs no QEMU and no cross C
  toolchain (go.mod has zero dependencies; the binary is pure Go).
- Hardware examples (M-series Macs, Graviton, Raspberry Pi 4+): correct
  arm64 hardware classes.

Observation (not fixed, with reason): the top-line "A prebuilt multi-arch
image is published" is transiently ahead of registry reality between this
commit and the v0.4.1 tag (latest/v0.4.0 stay amd64-only until then). This
is inherent to shipping docs with the feature and matches the PRD's own
deliverable wording (arch table keyed on v0.4.1); AC6 sequences the tag
immediately after CI green. Qualifying the sentence would go stale the
moment v0.4.1 ships.

---

## G. Local gates (run this check, final pass)

- `go build ./...` — OK
- `go vet ./...` — OK
- `gofmt -l .` — empty
- `go test ./...` — `ok reproxy`, `ok reproxy/cmd/reproxy`
- `bash -n scripts/docker-smoke.sh` — OK (file unchanged, 0-byte diff)
- Every `run:` block in docker-publish.yml extracted and `bash -n`-checked —
  all OK
- Strict YAML parse (custom duplicate-key-rejecting loader) of
  docker-publish.yml and ci.yml — both OK, LF-only, no duplicate keys, no
  tabs
- Full merge command simulated under `bash -e` with real values on both
  event paths — byte-exact intended command lines, rc 0
- jsonschema validation against schemastore github-workflow.json: the only
  findings are the YAML-1.1 `on:` → `True` artifact (bare `on:` parses as
  boolean in PyYAML) — identical in the old committed workflow, not a
  defect; job-output keys `digest-amd64`/`digest-arm64` are schema-allowed
- Mutation scan: N/A — zero Go product code and zero test changes (PRD:
  "packaging breadth, zero product-code change"); the runtime gate for the
  changed surfaces is the dispatch run. Reasoning audited and agreed.
- Impact radius: L1–L5 — no product code touched; blast radius is Docker
  packaging + publish workflow + docs only. ci.yml (the Go gate) untouched.

---

## Issues found and fixed

None. No defect requiring an edit was found; nothing was modified by the
check agent (`git status` still shows exactly the three implement-agent
files, uncommitted, unstaged).

## Issues not fixed (observations)

1. README top-line "multi-arch image is published" is transiently ahead of
   the registry until v0.4.1 is tagged (see F). Informational; PRD-consistent.
2. Merge job's setup-buildx-action is defensive rather than strictly
   required (runner images preinstall buildx 0.36.1). Keeping it matches
   github-builder's finalize job and pins the >= 0.12 guarantee. No action.

## Verdict

**READY-FOR-DISPATCH.** All statically checkable acceptance criteria pass;
the workflow's riskiest mechanics (matrix output combining, hyphenated
output keys, comma-containing `outputs:` input, index flattening, tag loop
under `bash -e`, smoke-before-tag ordering) are verified from primary
sources. Remaining gates are runtime-only and belong to the main session:
dispatch run green (AC3), anonymous manifest list check with exactly 2
platform-bearing children (AC4), then the v0.4.1 tag + compose/README pin
bumps + release notes (AC5 second half, AC6).
