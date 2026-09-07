# Implement report — v0.4.0 leading policy segment

Incremental record: each unit is appended as it completes (resume point on
interruption). Baseline before any change: `go vet`, `gofmt -l`, `go build`,
`go test ./...` all green; 396 test cases.

## Unit 1 — Grammar core (target.go, header.go)

Status: COMPLETE (code compiles, vet/gofmt clean; tests red until unit 3 —
expected, every policy-bearing row still speaks the v0.3 spelling).

Changes:

- `target.go` `ParsePath`: new grammar
  `PROXY-TARGET := ["/+" POLICY] "/" SCHEME "/" AUTHORITY ["/" RAW-PATH]`.
  - Leading control segment dispatched by a single byte check at position 1
    (`strings.HasPrefix(rest, "+")`), original escaped bytes — one cut, no
    lookahead. Body parsed via `parsePolicyPairList(policySeg, segmentCarrier)`.
  - Empty body (`/+`, `/+/https/h`) → shared ladder's "present but empty" 400
    (degenerate; absent is the only no-policy spelling).
  - `/+status=5xx` with nothing after → NEW 400 "missing upstream scheme in
    path" (R3 row).
  - Policy-body validation runs AFTER `parseSchemeSegment`, preserving the
    inherited ladder precedence (scheme errors before policy errors). This
    makes `/+a/+/https/h` die as `unsupported scheme "+"` (the second
    `+`-shaped segment is parsed as a scheme), exactly per R3.
- `target.go` `parseSchemeSegment`: `+` cut deleted entirely. It is now a
  strict `{http, https}` table (case-insensitive, lowercased), returning
  `(scheme, rerr)`. `https+status=5xx`, `https+retry`, `https+pure`,
  `https%2Bstatus=5xx` all die as `unsupported scheme %q: only http and
  https are supported` quoting the raw segment — no legacy detection (third
  application of the v0.2 knife). The trailing ", optionally with a +
  retry policy" phrase is dropped from the error text.
- `target.go` `usageHint` (line 42): new shape
  `[/+POLICY]/SCHEME/AUTHORITY[/PATH]` with a `/+status=5xx;*.attempts=3/https/...`
  example.
- `target.go` guardrail doc comment rewritten per R5: "the leading `+`
  segment speaks ONLY retry policy; the scheme segment is pure target. v0.3
  welded policy onto the scheme token; that grammar died in v0.4 as an
  unsupported scheme." ParsePath/parseSchemeSegment doc comments describe
  the new grammar.
- `header.go`: `carrierSegment` = "leading policy segment" (was "path scheme
  segment"); file header and `queryKeyForSegmentKey` /
  `validateReproxyNamespace` doc comments re-worded. `proxy.go` /
  `policy.go` pipeline comments re-worded ("leading-segment policy").

Ladder probe (temporary test, deleted after): every R3 row verified against
the implementation — all match (see unit 3 for the pinned rows).

## Unit 2 — Conflict wording (proxy.go)

Status: COMPLETE.

- Conflict 400 (proxy.go ~:192): now names "the path's leading policy
  segment"; hint says "the leading-policy-segment channel and the header
  channel are mutually exclusive: remove the header, or drop the policy from
  the path (e.g. use /https/host)" — the new remedy shape per R4.
- proxy.go ServeHTTP doc comment: policy carriers named as
  "(/+POLICY/https/host) or the X-Reproxy-Retry-Policy header".

## Unit 3 — Test surface

Status: COMPLETE. `go vet ./...`, `gofmt -l .` clean; `go test ./...` green.
Test cases: 396 -> 405 (+9).

Changes:

- `target_test.go` grammar table: every policy-bearing row re-spelled to
  `/+POLICY/https|http/...` (leading segment); pure-form rows unchanged with
  byte-identical expectations. v0.3 weld rows replaced by unsupported-scheme
  death rows (`/https+status=5xx/h`, `/https+retry/h`) — not migrations.
  New R3 rows added:
  - `/+statusx=5xx/https/h` -> unknown policy field (shared ladder)
  - `/+/https/h` and `/+` -> degenerate empty control segment
  - `/+status=5xx` (no scheme follows) -> missing upstream scheme; also
    `/+status=5xx/` (slash then nothing) -> missing upstream scheme
  - `/+a/+/https/h` -> `unsupported scheme "+"` (second `+`-segment parsed
    as a scheme)
  - `/%2Bstatus=5xx/https/h` -> `unsupported scheme "%2Bstatus=5xx"`
    (byte 1 is not `+`)
  - `/https/host/+x` and `/https/h/+a/+b` -> target path, forwarded
    verbatim (`+` is control only at segment position 1)
  - `policy scheme lowercase after policy` (`/+status=429/Https/...` works)
- `proxy_test.go`: 34 policy URLs re-spelled; `TestProxyPolicyChannelConflictMatrix`
  wording assertion now expects "leading policy segment";
  `TestProxyBadTarget400` extended with handler-level R3 rows (v0.3 death
  shapes, `%2B` fences, missing-scheme, empty control segment); NEW
  `TestProxyPlusLeadingPathSegmentIsTargetData` pins `/+x` in the target
  path reaching the upstream byte-for-byte with a single attempt.
- `e2e_test.go`: 13 policy URLs re-spelled; conflict-over-TCP wording
  assertion updated; NEW `TestE2EV03DeathShape400` (v0.3 weld spelling over
  real TCP -> unsupported-scheme 400 quoting the raw segment) and
  `TestE2EPlusInTargetPathForwardedVerbatim` (target-path `+` forwarded
  byte-for-byte over real TCP).
- `header_test.go`: `TestHeaderTransformEquivalence` segment spelling
  re-spelled to the leading-segment form.
- Conflict-matrix and equivalence/bijection tests pass against the new
  grammar (PRD acceptance item 3).

One implementation nuance discovered while making `/+a/+/https/h` work per
R3: policy-body validation must run AFTER parseSchemeSegment (scheme errors
name the scheme before policy errors name the field — the inherited ladder
precedence, pinned by the v0.3 `/ftp+status=5xx` row which becomes
`/ftp+status=5xx` unsupported-scheme). This is documented at the parse site
in target.go.

## Unit 4 — Mutation scan

Status: COMPLETE. 10/10 mutants killed, 0 survivors. Full matrix recorded in
mutation-scan.md (same directory). Isolated copy at a temp dir; every
mutant compiled (the one first-cut compile failure, m6, was re-run with
valid syntax — compile failure is not a capture); revert + diff-verify
byte-identical after each; full copy suite green after the last revert.

Mutants: m1 leading-+ dispatch disabled (46 captures); m2 v0.3 `+` cut
restored in parseSchemeSegment (3); m3 segment key mapper made total /
bare word deferred (36); m4 conflict gate disabled (2); m5 `%2B` decoded
before dispatch (3); m6 policy-less path uses Parse(∅) defaults (1);
m7 greedy `+` dispatch before scheme (4); m8 missing-scheme row disabled
(2); m9 empty control segment falls through (2); m10 query re-encoded (7).

## Unit 5 — Docs & spec

Status: COMPLETE.

README.md (same set of places v0.3.0 touched, plus the rationale):

- Intro paragraph: "the upstream destination is expressed in the request
  path and the retry policy (optionally) rides a leading /+-prefixed
  segment in front of it".
- Quick-start curl: `/+status=500,502-504;*.attempts=3/https/example.com/api`.
- "How it works": grammar line `[/+<policy>]/<scheme>/<host>[:port]/<path>?<query>`;
  policy-rides-the-leading-segment wording; v0.3 death shape documented
  (`/https+status=5xx/host` fails scheme validation); `%2B` fence re-spelled
  for the first segment; `+` is control only at segment position 1; the
  three-line rationale (reproxy owns exactly one URL surface — the leading
  /+-prefixed segment (when present); the scheme segment is pure target;
  the + slot speaks only retry policy).
- "The retry policy": both carrier examples re-spelled; carriers bullet
  ("Leading path segment"); conflict wording updated.
- Header channel section: conflict bullet names the leading path segment.
- Stability note: gained the v0.3→v0.4 sentence (weld died as unsupported
  scheme; plain form byte-stable since v0.1.0).
- Tag semantics: version-tag example list bumped to v0.4.0 (immutable-tag
  wording itself unchanged).

retry-proxy-contract.md (full rewrite per R5a): source line gains the
09-07-v0-4 task (10/10 mutations); §2 signatures (ParsePath doc: nil = no
/+POLICY segment); §3 path grammar block `[/"+" POLICY] "/" SCHEME ...` with
pure-target SCHEME, ladder-precedence note, position-1 rule, guardrail
rewritten (v0.3 death sentence); carrier section and matrix re-spelled to
the leading segment; §4 error matrix gains the four new R3 rows (unknown
field in segment, empty control segment, missing scheme, second
+-segment, target-path + data); §5 good/base/bad re-spelled + v0.3 death
shape rows; §6 test list updated (m1–m10); §7 wrong/correct — new
"restoring the v0.3 + cut" wrong example, new leading-dispatch correct
example.

## Unit 6 — Pins

Status: COMPLETE (pins only — nothing committed, tagged, or pushed; the
main session owns the tag/commit/release lifecycle per the dispatch
instructions).

- `docker-compose.yml`: image pin `ghcr.io/killbus/reproxy:v0.3.0` ->
  `v0.4.0`.
- `README.md`: compose example image pin and the `--version` docker-run
  example both `v0.3.0` -> `v0.4.0`; tag-semantics version-tag example list
  bumped to v0.4.0. (The Dockerfile build-arg example already read v0.4.0
  from the docker-smoke task; CI derives the version from the pushed tag —
  no other pins exist.)

Final verification: `gofmt -l .` empty; `go vet ./...` clean;
`go test ./... -count=1` green (405 cases; 396 -> 405, +9).
