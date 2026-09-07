# Implement — v0.4.0 leading policy segment

## Preconditions

- [ ] docker-smoke-extraction task ARCHIVED (R8-3: extraction before tag).
- [ ] Working tree clean except this task's artifacts.

## Units

1. [ ] **Grammar core** — `target.go`: leading `/+` dispatch in ParsePath;
       `parseSchemeSegment` `+`-cut deleted (strict http/https); usageHint;
       guardrail doc comment rewritten (leading segment, v0.3 death sentence).
       `header.go`: `carrierSegment` → "leading policy segment".
2. [ ] **Conflict wording** — `proxy.go` conflict 400 names the leading
       segment; remedy text updated.
3. [ ] **Test surface** — target/proxy/e2e/header test rows re-spelled to
       `/+POLICY/https/...`; new ladder rows (R3); v0.3 death-shape rows;
       pure-form rows unchanged. All green locally (`go test ./...`).
4. [ ] **Mutation scan** — isolated copy; m4–m7 equivalents + new ladder
       mutants; revert + diff-verify byte-identical; record in
       mutation-scan.md.
5. [ ] **Docs & spec** — README (grammar, rationale, carriers, stability
       note, examples); retry-proxy-contract.md full rewrite per R5a.
6. [ ] **Pins & release** — compose + README image pins → v0.4.0; commit;
       tag v0.4.0; push; CI green (both OS + Linux race); GitHub Release
       with grammar-change headline.

## Validation commands

- `go vet ./... && gofmt -l .` (empty)
- `go test ./...` — full suite green
- Mutation scan per unit 4 on an isolated copy (robocopy to temp dir)
- CI: push, then both-OS + Linux-race green

## Review gates

- After unit 3: dispatch a trellis-check agent (full-scope check vs PRD).
- After unit 6: main session verifies CI + Release state directly.

## Rollback points

- Revert the single grammar commit; v0.3.0 tag remains the shipped truth.
- Mutation-scan copy is disposable (temp dir).
