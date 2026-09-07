# Mutation scan — 09-07-docker-image-ci (`--version` semantics)

Method per spec: one-line mutations applied in an isolated copy
(`D:\reproxy-mut`, removed after), full `go test -count=1 ./cmd/...`
run per mutation, byte-equality diff-verified after every restore.

The Docker/workflow artifacts have no runnable tests on this host (no
Docker) — their capture channel is the workflow_dispatch smoke run on
the GitHub runner, owned by the main session. This scan pins the Go-side
semantics the smoke depends on.

## Matrix

| # | Behavior pinned | Mutation | Capturing test | Result |
|---|---|---|---|---|
| 1 | `--version` matches only in leading position | guard accepts `--version` anywhere (`contains`) | `TestVersionRequested/other_flag_first` | FAIL (captured) |
| 2 | Banner shape: version + `\n` | `Fprintln` -> `Fprint` | `TestPrintVersionOutput` | FAIL (captured) |
| 3 | Default stamp is exactly `dev` | `var version = "development"` | `TestVersionDefault` | FAIL (captured) |
| 4 | Banner goes to STDOUT (main's call site) | `runVersion(os.Stdout)` -> `runVersion(os.Stderr)` | `TestMainVersionBannerOnStdout` | FAIL (captured) — **survivor on first run; test added** |
| 5 | The guard actually fires | `versionRequested` returns constant false | `TestMainVersionBannerOnStdout` + `TestVersionRequested` | FAIL (captured) |
| 6 | Exit code 0 after the banner | `os.Exit(0)` -> `os.Exit(1)` | `TestMainVersionBannerOnStdout` | FAIL (captured) — **survivor on first run; assertion added** |
| 7 | The flag spelling is `--version` (double dash) | `args[0] == "-version"` | `TestVersionRequested/leading_--version` | FAIL (captured) |
| 8 | The banner is actually printed | `runVersion` returns without calling `printVersion` | `TestMainVersionBannerOnStdout` (+ `TestPrintVersionOutput` unaffected) | FAIL (captured) |

## Survivors found and closed

- **M4 (stdout)**: initially survived — `TestRunVersionGoesToStdout`
  passes its own buffer, so nothing pinned which stream *main* selects.
  Closed with `TestMainVersionBannerOnStdout`: the test binary
  re-executes itself (`GO_WANT_VERSION_PROCESS` handshake, standard
  Go reentrancy pattern), the child rewrites `os.Args` to
  `["reproxy", "--version"]` and calls `main()`; the parent asserts the
  banner arrived on the child's captured stdout — the exact stream
  CI's `docker run <img> --version` reads.
- **M6 (exit code)**: initially survived — the reentrancy test only
  checked stdout content. Added the clean-exit assertion
  (`cmd.Run()` must return nil): a `--version` that exits 1 breaks
  script usage and the smoke's `docker run --rm` semantics.

Both fixes re-verified: rerunning the identical mutation against the
strengthened suite goes red (FAIL), restore diff-verified byte-equal.

## ldflags injection (outside the mutation matrix, verified live)

`go build -ldflags "-X main.version=v9.9.9-test"` -> `--version` prints
`v9.9.9-test`; unstamped build prints `dev`. Re-verified after every
refactor of the seam (three times across the session).

Final state: 0 survivors. Main-tree gates green after the scan
(build/vet/gofmt/test, both packages).
