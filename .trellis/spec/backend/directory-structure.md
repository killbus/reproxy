# Directory Structure

> How files are organized in this Go project (documented from the actual layout).

---

## Layout

```
reproxy/
├── *.go                  # package reproxy — flat, one concern per file
│   ├── target.go         # PathTarget + scheme-segment policy parsing (R1)
│   ├── header.go         # shared pair grammar, two policy carriers (R2)
│   ├── policy.go         # three-tier policy resolution (R3)
│   ├── body.go           # CapturedBody probe/replay (R4)
│   ├── backoff.go        # ComputeWait, jitter, Retry-After parsing (R5)
│   ├── proxy.go          # the retry loop, commit point, logging
│   ├── ssrf.go           # forbidden tables, PinnedResolver (R7)
│   ├── config.go         # ServerConfig, flags, validation (R9)
│   └── errors.go        # RequestError (shared error contract)
├── *_test.go             # colocated unit tests, one per source file
├── e2e_test.go           # real-TCP end-to-end scenarios (no mocks above network layer)
├── cmd/reproxy/main.go   # the only binary entrypoint
├── scripts/              # repo-owned verification/infra scripts (not Go, not product code)
│   └── docker-smoke.sh   # the three image smoke checks (version banner, round-trip + gate, compose)
└── docs/adr/             # architecture decision records
```

## Conventions

- **Flat package, not `internal/` layers.** One Go package (`reproxy`) plus a thin `cmd/reproxy` entrypoint. The project is a single focused tool; layering would be ceremony. `internal/` is only warranted once a second consumer exists.
- **One concern per file, named for the PRD requirement it implements.** `policy.go` is R3; `ssrf.go` is R7. A new requirement gets a new file, not an annex to an existing one.
- **Tests colocated**: `policy.go` ↔ `policy_test.go`. Cross-file integration scenarios go in `e2e_test.go`.
- **No `util.go` / `helpers.go`** — a file that needs such a name has no concern; name it after what it does (see `backoff.go`).
- **`scripts/` is verification/infra, not product code.** Shell scripts there (e.g. `docker-smoke.sh`) pin product contracts that CI and local builds both run — the repo, not a CI template, owns them (the "same build path" principle applied to verification). The stdlib-only rule is a Go-product constraint; shell is the same category as workflow YAML. New scripts: LF-only (`.gitattributes`), executable bit set in the index, no knobs — hardcoded ports/paths with the reasoning in comments.
- Stdlib only (ADR-0001). Adding an external dependency requires an ADR-level justification, not a casual import.
