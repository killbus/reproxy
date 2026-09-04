# reproxy

reproxy is a lightweight, stateless, general-purpose HTTP retry reverse proxy.
The upstream destination is expressed in the request path and the retry policy
is expressed in query parameters, so any client that can build a URL can ask
for retries on a per-request basis — no sidecar config files, no per-route
server configuration. It ships as a single Go binary built on the standard
library only (zero external dependencies), streams request and response bodies
(including `text/event-stream`), and enforces server-side destination
restrictions so it cannot be used as an open relay.

## Quick start

```sh
go build ./cmd/reproxy        # produces ./reproxy
./reproxy --allowlist example.com
```

```sh
curl "http://localhost:8080/http/example.com/api?retry.status=500,502-504&retry[*].attempts=3"
```

The server refuses to start with an empty allowlist unless `--dangerous-allow-all`
is passed (see [Security](#security)).

## How it works

The request path carries the upstream target, the query carries both the retry
policy and the data passed through to the upstream:

```
/<scheme>/<host>[:port]/<path>?<query>
```

- `<scheme>` is `http` or `https`; anything else is a 400.
- Default ports: `http` → 80, `https` → 443. An explicit port wins. Ports must
  be 1–65535 in plain decimal; leading zeros are rejected.
- `<host>` may be a DNS name, an IPv4 literal, or a bracketed IPv6 literal
  (`/https/[2001:db8::1]/x`). Bracketless IPv6 and userinfo
  (`user:pass@host`) are rejected with 400.
- An empty path normalizes to `/`: `/https/h` and `/https/h/` both mean
  `https://h/`.
- The path is forwarded **as raw bytes** — never decoded or re-encoded — so
  canonical encodings required by signed URLs survive intact.

The query is split into two namespaces:

- Everything starting with `retry.` or `retry[` is a retry parameter. It is
  stripped and never reaches the upstream.
- Everything else is passed through with its **original byte order and
  encoding** — no parse/re-serialize round trip. Valueless keys (`?a&b`),
  empty values (`a=`), duplicate keys, `%2F`-style escapes, and `+` all
  round-trip byte-identically.
- An unrecognized retry key (e.g. `retry.wat=1`) is a 400 naming the key.

## Retry parameters

| Parameter | Scope | Meaning | Default | Validation |
|---|---|---|---|---|
| `retry.status` | global | Which upstream statuses trigger a retry (the gate) | empty (no status retries) | exact codes, closed ranges, or class shorthand — see below |
| `retry.network` | global | Retry network failures (dial/TLS/write/TTFB timeout) | `1` | `0` or `1` |
| `retry.budget` | global | Hard cap on the total retry lifecycle (all attempts + waits) | `30s` | duration with unit, > 0 |
| `retry[*].attempts` | default scope | Total attempts including the first | `3` | integer ≥ 1 |
| `retry[*].backoff` | default scope | Wait curve: `constant` \| `linear` \| `exponential` | `exponential` | one of the three |
| `retry[*].initial` | default scope | Base wait | `1s` | duration with unit, > 0 |
| `retry[*].max` | default scope | Cap on a single computed wait | `8s` | duration with unit, ≥ `initial` |
| `retry[*].jitter` | default scope | `none` \| `full` \| `equal` | `full` | one of the three |
| `retry[*].retry_after` | default scope | `honor` \| `ignore` upstream `Retry-After` | `honor` | one of the two |
| `retry[NNN].<field>` | per-status | Override any field above **for retries triggered by status NNN** | — | `NNN` is an exact 3-digit code, 100–599 |

`retry.status` accepts a comma-separated list of exact codes (`429,500`),
closed ranges (`500-599`), and class shorthand (`5xx`, `5XX`). Overlaps and
duplicates union silently. Reversed ranges, whitespace, empty items, and codes
outside 100–599 are 400s.

Durations require a unit (`1s`, `100ms`, `1.5s`); a bare number is a 400.

### Resolution order

Fields resolve through a three-tier chain with field-level override:

```
built-in default  ->  retry[*].FIELD  ->  retry[NNN].FIELD
```

Each tier overrides only the fields it names. `retry[429].initial=100ms`
changes the wait for 429-triggered retries only; every other field keeps the
`retry[*]` (or built-in) value.

Gates and budget live only at the global level — `retry[429].status` or
`retry[500].budget` are 400s.

### Server clamps

Server flags narrow, never widen: `--max-attempts` caps `attempts` (in every
scope) and `--max-budget` caps `retry.budget`. A request asking for more is
silently clamped down to the server cap.

### Dead configurations

Gates and shaping are separate: `retry[429].attempts=4` with 429 **not** in
`retry.status` can never take effect, so it is a 400 rather than a silently
ignored parameter. Every invalid value produces a 400 naming the offending key.

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

`retry.budget` is a hard cap over the whole lifecycle: every attempt plus every
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

Every retry is also logged with the attempt number, triggering status, wait,
and remaining budget.

## HTTP conformance notes

- **Hop-by-hop headers** (`Connection`, `Keep-Alive`, `Transfer-Encoding`,
  `TE`, `Upgrade`, `Proxy-Authenticate`, `Proxy-Authorization`, plus anything
  named in a `Connection` header) are stripped in both directions, per
  RFC 9110 §7.6.1. `Via`, `X-Forwarded-For`, `X-Forwarded-Proto`, and
  `X-Forwarded-Host` are appended, preserving any existing chain entries.
- **Non-idempotent replay is opt-in.** RFC 9110 §9.2.2 says a proxy MUST NOT
  automatically retry non-idempotent requests. reproxy only retries a POST when
  the client explicitly passed retry parameters — passing retry parameters on a
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
  10 MiB). A body over the cap is not retried: in the default mode it is
  streamed through once with `X-Retry-Dropped: body-too-large` (and a warn
  log); with `--strict-body-limit` the request is rejected with 413.

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
| `--max-budget` | `30s` | Server cap on the retry lifecycle (clamps `retry.budget` down) |
| `--max-body` | `10485760` (10 MiB) | Request body capture cap in bytes; larger bodies are not retried |
| `--strict-body-limit` | `false` | Reject oversized request bodies with 413 instead of degrading to pass-through |
| `--dangerous-allow-all` | `false` | Disable the destination allowlist (open relay exposure) |

The server waits up to 10s for request headers (slowloris guard) and shuts
down gracefully on SIGINT/SIGTERM, draining in-flight requests for up to 10s.

## Error responses

Client-visible errors use a JSON body:

```json
{"error": "unknown retry parameter \"retry.wat\"", "hint": "recognized keys: ..."}
```

| Status | When |
|---|---|
| `400` | Malformed target path, unknown/invalid retry parameter, dead configuration (e.g. `retry[429]` without 429 in `retry.status`) |
| `403` | Host not in the allowlist; target resolves to (or is) a forbidden address |
| `413` | Request body over the cap in `--strict-body-limit` mode |
| `502` | Upstream hostname could not be resolved |
| `504` | All attempts failed on the network, or the budget ran out with no upstream response to deliver |

Status-coded failures from the upstream itself are always relayed as-is — a
500 from the upstream is the client's answer, not a proxy error.

## Limitations / future work

Excluded from the current scope (recorded for future work):

- temp-file spooling (oversized bodies degrade to pass-through instead)
- request-body streaming pass-through (tee mode)
- hold-headers-until-first-byte
- `5xx` class scopes for `retry[NNN]` (per-status scopes are exact codes only; class shorthand works in `retry.status`)
- decorrelated jitter
- multi-upstream load balancing / failover
- circuit breaking / retry budgets
- HTTP/3
- gRPC
- management/metrics endpoints
