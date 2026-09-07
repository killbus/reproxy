# Check report — 09-07-docker-image-ci

Fresh check run (previous check agent died on infrastructure errors before
writing a report; nothing resumed from it). Findings appended incrementally
as the check progressed.

## Verified so far (host-runnable claims re-run, all confirmed)

- Gates: `go build ./...`, `go vet ./...`, `gofmt -l .` (empty), `go test
  -count=1 ./...` — all green (reproxy + reproxy/cmd/reproxy).
- ldflags injection re-verified live: `go build -ldflags "-X
  main.version=v9.9.9-check"` → `--version` prints `v9.9.9-check`; unstamped
  build prints `dev`. Leading-position-only guard verified live: `--version
  --listen :9090` prints banner; `--listen :9090 --version` and `-version`
  fall through to flag parsing (exit 2, unknown flag).
- ci.yml untouched (`git diff .github/workflows/ci.yml` empty).
- Both YAML files parse (PyYAML); all four `run:` blocks pass `bash -n`.
- Meta-step bash simulated for both event paths (push → `version=v0.4.0`,
  two-line tags `v0.4.0`+`latest`; dispatch → `dev-run42-dc96e3a`,
  `dispatch-test` only). Heredoc `$GITHUB_OUTPUT` syntax confirmed correct,
  including the YAML-indentation trick on the `${IMAGE}:latest"`
  continuation line.
- Actions pins re-verified live via `gh api repos/<repo>/releases/latest`:
  checkout v7.0.1, login-action v4.6.0, setup-buildx-action v4.3.0,
  build-push-action v7.3.0 — all four match the latest release exactly.
- Distroless base re-verified live against gcr.io: `distroless/static-debian13`
  has `nonroot` and `latest` tags; `nonroot` is an OCI index with an amd64
  manifest (13 layers, config present).
- `.gitignore` anchoring verified live: `git check-ignore cmd/reproxy/main_test.go`
  → not ignored (exit 1); root binary `reproxy` → matched by `/reproxy`
  (line 9).
- `.dockerignore` filter simulated against the real tree: 13 files kept
  (source + go.mod + Dockerfile + compose), 4169 dropped, zero leakage of
  tests/docs/.trellis/.github.
- compose file: exactly image/command/ports/restart, values match PRD.
- Kill list grep in non-comment lines: platforms/cosign/sbom/trivy/cache-from/
  cache-to appear ONLY in the comment block (lines 16-18). No DockerHub
  references anywhere.
- `github.repositoryUrl` confirmed to exist as a github-context property
  (official docs) — but its VALUE is `git://github.com/owner/repo.git`;
  see defect D2 below.

## Findings in progress

Checking compose build-context resolution for the /tmp-rewritten compose
file and the build-push-action inputs.
