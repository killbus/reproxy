# reproxy

reproxy is a lightweight, stateless, general-purpose HTTP retry reverse proxy.
The upstream destination and (optionally) the retry policy are both expressed
in the request path, so any client that can build a URL can ask for retries on
a per-request basis — no sidecar config files, no per-route server
configuration. It ships as a single Go binary built on the standard library
only (zero external dependencies), streams request and response bodies
(including `text/event-stream`), and enforces server-side destination
restrictions so it cannot be used as an open relay.

## Quick start

```sh
go build ./cmd/reproxy        # produces ./reproxy
./reproxy --allowlist example.com
```

```sh
curl "http://localhost:8080/https+status=500,502-504;*.attempts=3/example.com/api"
```

A binary built this way reports `--version` as `dev`; release builds are
stamped with the git tag via `-ldflags`.

The server refuses to start with an empty allowlist unless `--dangerous-allow-all`
is passed (see [Security](#security)).

## Docker

A prebuilt image is published at `ghcr.io/killbus/reproxy` (distroless-based,
non-root, single static binary). One-liner:

```sh
docker run --rm -p 8080:8080 ghcr.io/killbus/reproxy:latest \
  --allowlist example.com
```

Or as a compose file (`docker compose up -d`):

```yaml
services:
  reproxy:
    image: ghcr.io/killbus/reproxy:v0.3.0   # pinned version
    command: ["--allowlist", "api.example.com"]
    ports: ["8080:8080"]
    restart: unless-stopped
```

The image is `gcr.io/distroless/static-debian13:nonroot` (not `scratch`)
because reproxy makes outbound HTTPS connections with certificate
validation — a bare scratch image has no CA roots and the first `https`
upstream would fail.

### Tag semantics

- **Version tags** (`v0.1.0`, `v0.3.0`, …) are **immutable**: each is pushed
  once at release time and never re-pushed. What a version tag pointed to
  yesterday is what it points to forever. Pin production deployments to a
  version tag.
- **`latest`** is the newest release only — it is a convenience for
  trying the tool, not a deployment target.
- The binary reports its version via `--version` (the tag it was built
  from; `dev` for local builds):

```sh
docker run --rm ghcr.io/killbus/reproxy:v0.3.0 --version
```

### Building locally

```sh
docker build -t reproxy:local .
docker run --rm -p 8080:8080 reproxy:local --allowlist example.com
```

The local image reports `--version` as `dev`; inject a version with
`--build-arg VERSION=vX.Y.Z`. The compose local-build variant replaces
`image:` with `build: .`:

```yaml
services:
  reproxy:
    build: .
    command: ["--allowlist", "api.example.com"]
    ports: ["8080:8080"]
    restart: unless-stopped
```

## How it works

The request path carries the upstream target and, after a `+`, the retry
policy. The query belongs to the target and is always forwarded untouched:

```
/<scheme>[+<policy>]/<host>[:port]/<path>?<query>
```

- `<scheme>` is `http` or `https`; anything else is a 400.
- `<policy>` is a retry policy (see below). Its absence — a plain
  `/https/host/...` path — means single attempt, no retry. Absent is the only
  no-policy spelling: a bare `+` or an unparseable policy body is a 400.
- Default ports: `http` → 80, `https` → 443. An explicit port wins. Ports must
  be 1–65535 in plain decimal; leading zeros are rejected.
- `<host>` may be a DNS name, an IPv4 literal, or a bracketed IPv6 literal
  (`/https/[2001:db8::1]/x`). Bracketless IPv6 and userinfo
  (`user:pass@host`) are rejected with 400.
- An empty path normalizes to `/`: `/https/h` and `/https/h/` both mean
  `https://h/`.
- The path is forwarded **as raw bytes** — never decoded or re-encoded — so
  canonical encodings required by signed URLs survive intact. This includes
  the scheme segment: `%2B` is not `+`, so `/https%2Bstatus=5xx/host/...` is
  not a policy-carrying path — it fails scheme validation as the literal
  segment `https%2Bstatus=5xx`.
- The scheme part is case-insensitive (`/HTTPS/...` works); the policy body
  after `+` is matched on original bytes and is case-sensitive.

## The retry policy

A policy is a semicolon-separated list of `key=value` pairs, with optional
whitespace around pairs and around the `=`. Two carriers accept the same
grammar:

```
/https+status=5xx;*.attempts=3;429.attempts=5/host/path?query   (path segment)
X-Reproxy-Retry-Policy: status=5xx; [*].attempts=3; [429].attempts=5   (header)
```

- **Path segment** — the policy rides the scheme segment after a `+`. Scope
  keys are dotted (`*.attempts`, `429.attempts`) because brackets are illegal
  in a path segment; gates are bare words (`status`, `network`, `budget`).
- **Header** — `X-Reproxy-Retry-Policy` carries the same policy with bracketed
  scope spellings (`[*].attempts`, `[429].attempts`), which are legal in a
  header value. See [the header channel](#the-header-channel-x-reproxy-retry-policy).

The two carriers map onto the same field set and feed the same validation, so
every check — unknown keys, bad values, dead configurations — produces the
same 400 bodies either way. A key that is neither a gate nor a dotted scope
(`*.FIELD` with FIELD known, `NNN.FIELD` with NNN a 3-digit code) is rejected
as an unknown policy field; there is no other `+` vocabulary and no
identifier slot.

The two carriers are **mutually exclusive**: a policy in the path segment plus
the header is a 400 naming both channels and the remedy. The plain path plus
the header is the intended header-channel combination, not a conflict.

### Plain path semantics

`/https/host/...` with no policy anywhere is a pure reverse proxy:

- **The query is never inspected.** `RawQuery` is forwarded byte-identical —
  including `retry.`-prefixed keys that are the target's own data. The
  headline case: an upstream API with a `retry.count` parameter receives it
  verbatim through `/https/...?retry.count=7`.
- **No retries at all** — one attempt, no status gate, no network retry.
  (Per-try TTFB bounds still apply: they bound a hang, not a retry.)
- **The request body is never captured.** Capture exists solely to replay a
  body across attempts; one attempt means no replay. There is no 10 MiB cap,
  no 413, no degraded mode, and no `X-Retry-Dropped` — the body simply
  streams to the upstream, which is also faster (no double-buffering).
- **No `X-Retry-*` response headers** — there is no retry lifecycle to report.
- SSRF protections (allowlist, resolve-then-pin, forbidden-IP checks, no
  redirect following) apply identically with or without a policy.

Adding a policy (either carrier) re-enables body capture, because the
requested policy may need a replayable body. The full capture machinery comes
back with it: the 10 MiB cap, 413 in strict mode, the degraded pass-through,
and `X-Retry-Dropped` on the response. Asking for retries is not free.

### The header channel (`X-Reproxy-Retry-Policy`)

For programmatic clients that can set headers, the policy can travel
out-of-band, leaving the path plain:

```
X-Reproxy-Retry-Policy: status=5xx; network=1; budget=30s; [*].attempts=3; [429].attempts=5
```

- The value uses the shared pair grammar with bracketed scope spellings.
  Keys are case-sensitive; values are taken literally (never URL-decoded —
  header values are plain text). No legitimate value needs `;` or `=`, so
  the pair split stays unambiguous.
- An **absent header is the only "no policy" spelling**. A present-but-empty
  value, whitespace-only content, or an empty pair (`;;`) is a 400 on every
  request: degenerate input must never read as "default policy".
- Exactly one occurrence is allowed; multiple `X-Reproxy-Retry-Policy`
  headers are a 400.
- `X-Reproxy-*` is a reserved request-header namespace: any other
  `X-Reproxy-Foo` is a 400 (fail closed), on every request regardless of
  carrier. All `X-Reproxy-*` headers are stripped before the request is
  forwarded upstream.
- Using both carriers at once — a policy in the path segment plus the
  header — is a 400 naming the conflict and the remedy ("remove the header,
  or drop the policy from the path"). If you did not set this header, a
  middleware or gateway between you and reproxy may have added it.

## Retry fields

| Field | Scope | Meaning | Default | Validation |
|---|---|---|---|---|
| `status` | global | Which upstream statuses trigger a retry (the gate) | empty (no status retries) | exact codes, closed ranges, or class shorthand — see below |
| `network` | global | Retry network failures (dial/TLS/write/TTFB timeout) | `1` | `0` or `1` |
| `budget` | global | Hard cap on the total retry lifecycle (all attempts + waits) | `30s` | duration with unit, > 0 |
| `*.attempts` | default scope | Total attempts including the first | `3` | integer ≥ 1 |
| `*.backoff` | default scope | Wait curve: `constant` \| `linear` \| `exponential` | `exponential` | one of the three |
| `*.initial` | default scope | Base wait | `1s` | duration with unit, > 0 |
| `*.max` | default scope | Cap on a single computed wait | `8s` | duration with unit, ≥ `*.initial` |
| `*.jitter` | default scope | `none` \| `full` \| `equal` | `full` | one of the three |
| `*.retry_after` | default scope | `honor` \| `ignore` upstream `Retry-After` | `honor` | one of the two |
| `NNN.<field>` | per-status | Override any field above **for retries triggered by status NNN** | — | `NNN` is an exact 3-digit code, 100–599 |

In the header carrier the scope spellings are bracketed: `[*].attempts`,
`[429].attempts`.

`status` accepts a comma-separated list of exact codes (`429,500`), closed
ranges (`500-599`), and class shorthand (`5xx`, `5XX`). Overlaps and
duplicates union silently. Reversed ranges, whitespace, empty items, and
codes outside 100–599 are 400s.

Durations require a unit (`1s`, `100ms`, `1.5s`); a bare number is a 400.

### Resolution order

Fields resolve through a three-tier chain with field-level override:

```
built-in default  ->  *.FIELD  ->  NNN.FIELD
```

Each tier overrides only the fields it names. `429.initial=100ms` changes the
wait for 429-triggered retries only; every other field keeps the `*` (or
built-in) value.

Gates and budget live only at the global level — `429.status` or
`500.budget` are 400s.

### Server clamps

Server flags narrow, never widen: `--max-attempts` caps `attempts` (in every
scope) and `--max-budget` caps `budget`. A request asking for more is
silently clamped down to the server cap.

### Dead configurations

Gates and shaping are separate: `429.attempts=4` with 429 **not** in the
`status` gate can never take effect, so it is a 400 rather than a silently
ignored parameter. Every invalid value produces a 400 naming the offending
key.

## `attempts` semantics

`attempts` counts **total attempts including the first request** — the same
convention as gRPC `maxAttempts`, AWS `max_attempts`, and nginx
`proxy_next_upstream_tries`.

| `attempts` | Upstream requests | Retries after the first |
|---|---|---|
| `1` | 1 | 0 (retry disabled) |
| `2` | 2 | 1 |
| `3` (default) | 3 | 2 |
| `N` | N | N−1 |

When attempts run out, the client receives the **last actual upstream
response** (the real status code and body), flagged with `X-Retry-Exhausted: 1`
— the proxy never invents a status the upstream did not send.

## Backoff

Waits are computed per retry number `n` (1-based: the wait before the *second*
attempt is `n=1`):

| Curve | Formula |
|---|---|
| `exponential` (default) | `wait(n) = min(initial × 2^(n−1), max)` |
| `linear` | `wait(n) = min(initial × n, max)` |
| `constant` | `wait = initial` |

Jitter then applies to the computed wait:

| Jitter | Result |
|---|---|
| `full` (default) | `rand(0, computed)` |
| `equal` | `computed/2 + rand(0, computed/2)` |
| `none` | `computed` |

### `Retry-After`

When the triggering response carries a `Retry-After` header and the scope is
`retry_after=honor` (the default), the parsed delay **replaces** the computed
backoff entirely. Honored delays are never jittered — the server said how long
to wait — but they are capped to `max` and to the remaining budget, so a
`Retry-After: 3600` cannot hang the request. Both formats are parsed:
delay-seconds and HTTP-date (a date in the past means zero delay).

### Budget

`budget` is a hard cap over the whole lifecycle: every attempt plus every
wait. When the remaining budget cannot cover the next wait, the retry loop
stops and the client receives the last upstream response (flagged exhausted),
or a 504 if every attempt failed on the network. Each attempt's wait for
response headers is additionally bounded by a 30s time-to-first-byte cap
(subjacent to the remaining budget).

## Response headers

| Header | Meaning |
|---|---|
| `X-Retry-Count` | Total attempts made, including the first |
| `X-Retry-Limit` | The attempt limit that was in force |
| `X-Retry-Exhausted` | `1` when attempts or budget ran out on retryable failures |
| `X-Retry-Dropped` | `body-too-large` when the body exceeded the capture cap and retry was skipped (see below) |

These are emitted only when a retry lifecycle exists — that is, when a
policy was present (either carrier). The policy-less path emits none of
them: a single attempt with no gates is not a retry lifecycle.

Every retry is also logged with the attempt number, triggering status, wait,
and remaining budget. The per-request summary line (`event=request`) carries
the policy's carrier (`policy=segment` / `policy=header` / `policy=none`) so
operators can watch their channel mix.

## HTTP conformance notes

- **Hop-by-hop headers** (`Connection`, `Keep-Alive`, `Transfer-Encoding`,
  `TE`, `Upgrade`, `Proxy-Authenticate`, `Proxy-Authorization`, plus anything
  named in a `Connection` header) are stripped in both directions, per
  RFC 9110 §7.6.1. `Via`, `X-Forwarded-For`, `X-Forwarded-Proto`, and
  `X-Forwarded-Host` are appended, preserving any existing chain entries.
- **Non-idempotent replay is opt-in.** RFC 9110 §9.2.2 says a proxy MUST NOT
  automatically retry non-idempotent requests. reproxy only retries a POST when
  the client explicitly passed a retry policy — passing a policy on a
  non-idempotent request constitutes informed consent to replay. The proxy
  discloses every replay via `X-Retry-Count`; be aware a retried POST may have
  side effects at the upstream (duplicate processing, duplicate charges).
- **The commit point is the first response byte.** The retry lifecycle ends
  when the proxy writes the first byte (status line/headers) to the client.
  After that, an upstream failure mid-body is never retried and never
  rewritten: the client sees a truncated stream, which is logged as
  `stream_truncated`. A response can only be retried before any of it has
  been delivered.
- **Streaming is preserved end to end.** Response bodies — including SSE — are
  forwarded chunk-by-chunk with flushing as chunks arrive, never buffered
  until the stream ends. The TTFB cap covers only the wait for response
  headers, so long streams are never killed by the retry budget; there is no
  idle timeout on response bodies.
- **Redirects are relayed, never followed.** A 3xx from the upstream reaches
  the client unchanged.
- **Request bodies are captured for replay** up to `--max-body` (default
  10 MiB) whenever a policy is present (replay is possible exactly then). A
  body over the cap is not retried: in the default mode it is streamed
  through once with `X-Retry-Dropped: body-too-large` (and a warn log); with
  `--strict-body-limit` the request is rejected with 413. The policy-less
  path never captures the body (nothing to replay), so no cap applies there
  at all.

## Security

reproxy forwards to a client-chosen upstream, which is an open-relay risk by
nature. Defenses are layered:

1. **Destination allowlist (L2).** The server refuses to start until
   `--allowlist` is set or `--dangerous-allow-all` is passed. Entries are exact
   hostnames or single-label wildcards (`*.example.com` matches
   `a.example.com` but not `example.com` or `a.b.example.com`). A request for
   a host outside the list is 403 **before any DNS work**.
2. **Resolve-then-pin (L3).** The proxy resolves the hostname itself and
   validates every returned A/AAAA record against a forbidden list —
   loopback, RFC 1918 private, CGNAT, link-local (including cloud metadata
   `169.254.169.254`), multicast, reserved, TEST-NET, and their IPv6
   equivalents (`::1`, `fc00::/7`, `fe80::/10`, `ff00::/8`, documentation),
   including 4-in-6 mapped forms. If **any** record is forbidden the whole
   request is 403 (fail closed — a hostname resolving to both a public and a
   private address is treated as hostile). The validated IPs are pinned and
   the dialer never re-resolves, closing the DNS-rebinding TOCTOU window.
3. **Dial re-assertion (L4).** The dialer re-checks the forbidden list on the
   exact address it is about to dial, as defense in depth against an L3 bug.
4. **No redirect following (L5).** 3xx responses are relayed; the proxy never
   re-requests a `Location` itself, so a redirect cannot bypass layers 2–4.
5. **Time bounds (L6).** Per-attempt TTFB caps and the retry budget bound
   every lifecycle.
6. **Startup gate (L7).** An empty allowlist is a startup error unless the
   operator explicitly passes `--dangerous-allow-all`.

TLS to https upstreams uses the hostname from the path for SNI and
certificate validation.

> **WARNING: `--dangerous-allow-all` disables the destination allowlist.** The
> proxy will then forward to any *public* host (the forbidden-IP list still
> applies). This is intended for local development and trusted internal
> networks only — a publicly reachable instance becomes an open relay.
> Deploy reproxy as an internal, trusted hop, never as a public endpoint.

## Server flags

| Flag | Default | Meaning |
|---|---|---|
| `--listen` | `:8080` | Listen address (`host:port`) |
| `--allowlist` | empty | Comma-separated destination allowlist (repeatable): exact hostnames and `*.suffix` wildcards |
| `--max-attempts` | `10` | Server cap on total attempts per request (clamps `attempts` down) |
| `--max-budget` | `30s` | Server cap on the retry lifecycle (clamps `budget` down) |
| `--max-body` | `10485760` (10 MiB) | Request body capture cap in bytes; larger bodies are not retried (applies whenever a policy is present; the policy-less path never captures) |
| `--strict-body-limit` | `false` | Reject oversized request bodies with 413 instead of degrading to pass-through (whenever a policy is present) |
| `--dangerous-allow-all` | `false` | Disable the destination allowlist (open relay exposure) |

The server waits up to 10s for request headers (slowloris guard) and shuts
down gracefully on SIGINT/SIGTERM, draining in-flight requests for up to 10s.

## Error responses

Client-visible errors use a JSON body:

```json
{"error": "unknown policy field \"wat\"", "hint": "policy keys are status, network, budget, *.FIELD, or NNN.FIELD ..."}
```

| Status | When |
|---|---|
| `400` | Malformed target path, unknown/invalid policy field, dead configuration (e.g. a `429.*` scope without 429 in the `status` gate), both policy carriers used at once, unknown or degenerate `X-Reproxy-*` header |
| `403` | Host not in the allowlist; target resolves to (or is) a forbidden address |
| `413` | Request body over the cap in `--strict-body-limit` mode (when a policy is present) |
| `502` | Upstream hostname could not be resolved |
| `504` | All attempts failed on the network, or the budget ran out with no upstream response to deliver |

Status-coded failures from the upstream itself are always relayed as-is — a
500 from the upstream is the client's answer, not a proxy error.

## Stability note

An earlier grammar put a mode word (`+retry` / `+pure`) in the scheme
segment's `+` slot and promised it stable forever; that promise was made
before any user depended on it and is withdrawn in favor of the simpler
zero-mode grammar documented here. The `+` slot now speaks only retry
policy. This is a documentation-level history note: the runtime has no
compatibility mode and no special-casing for the earlier spellings — a word
there that is not a policy key is a plain unknown-field 400.

## Limitations / future work

Excluded from the current scope (recorded for future work):

- temp-file spooling (oversized bodies degrade to pass-through instead)
- request-body streaming pass-through (tee mode) alongside capture
- hold-headers-until-first-byte
- `5xx` class scopes for `NNN.*` (per-status scopes are exact codes only; class shorthand works in the `status` gate)
- decorrelated jitter
- multi-upstream load balancing / failover
- circuit breaking / retry budgets
- HTTP/3
- gRPC
- management/metrics endpoints
