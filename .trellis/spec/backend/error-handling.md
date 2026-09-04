# reproxy Concurrency & Streaming Semantics

> Commit point, race cleanup, and streaming invariants for the proxy loop.
> Source: task 09-04 check round (F-1) and audit pinned semantics.

---

## Commit Point (the core invariant)

**The commit point is the first response byte written to the client** (`WriteHeader`).

- Before commit: any upstream/network failure may trigger a retry.
- After commit: no retry is possible. If the upstream dies mid-body, the proxy **does not** retry and does not fabricate an error page over the partial bytes — it aborts the connection with `panic(http.ErrAbortHandler)` (Go stdlib convention: tells the server to close the TCP connection rather than synthesize a 200-termination over partial content).
- Response headers from the winning upstream are written exactly once; a retried path never leaks a previous attempt's headers.

### Pattern

```go
w.Header().Set("X-Retry-Count", strconv.Itoa(attemptNo-1))
w.WriteHeader(resp.StatusCode)          // commit point
io.Copy(w, resp.Body)                  // after this, failure is fatal
if copyErr != nil { panic(http.ErrAbortHandler) }
```

### Why

Retrying after bytes have reached the client would duplicate side effects and corrupt partial downloads; serving a clean error over a half-delivered 200 is a protocol violation. The only correct move post-commit is to cut the connection.

## Raced Response Cleanup (F-1)

The TTFB wait runs a goroutine + `select` (NOT `context.WithTimeout` on the request — that would kill long SSE streams after headers arrive). When a `select` branch wins while the success channel still holds a response, that response **must be drained and closed** or the connection leaks.

### Wrong
```go
case <-timer.C:
    cancel()
    return nil, nil, errors.New("per-try timeout") // response in done channel: leaked conn
```

### Correct
```go
case <-timer.C:
    cancel()
    res := <-done
    p.drainAndClose(res.resp)
    return nil, nil, fmt.Errorf("per-try timeout: %v", ...)
```

Apply the same drain in the client-disconnect branch (`r.Context().Done()`).

### Tests

`TestProxyRacedResponseBodyClosed` (deterministic regression: forces the timer branch to win with a response in flight, asserts the body is closed).

## Body Streaming (degraded mode)

Request bodies above the capture cap (10 MiB) switch to one-shot streaming: the proxy splices the already-read probe bytes back in front of the unread body and forwards **once** (no retry — replay is impossible).

```go
r.Body = struct{ io.Reader; io.Closer }{
    Reader: io.MultiReader(bytes.NewReader(captured.Prefix), r.Body),
    Closer: r.Body,
}
```

- Byte fidelity is asserted at the unit level (`TestProxyDegradedBodyForwarded`) and e2e (`TestE2EPOSTOversizedDegraded`) — assert the upstream-received body equals the sent bytes, not just status codes.
- Response streaming: once committed, the upstream body is copied to the client as it arrives (SSE works without buffering — asserted by `TestE2ESSEStreaming` chunk timing).

## SSRF Resolve-Then-Pin (audit item 9)

DNS answers are resolved and validated **once** per request; the dial is pinned to the validated IP:

- Fail closed: ANY forbidden record (private, loopback, link-local `169.254/16` incl. cloud metadata, etc.) rejects the whole request — no partial trust.
- IP literals skip DNS but still pass the forbidden check.
- L4 re-assert at dial time (`DialContext` re-checks the pinned IP family and forbidden status) — closes DNS-rebinding TOCTOU.
- 4-in-6 mapped addresses are unmapped before v4 checks; derived prefixes (NAT64, 6to4) checked too.

## TTFB via goroutine + timer

Per-try timeouts guard **time-to-first-byte only**. `context.WithTimeout` around the whole round trip would cancel legitimate long streams (SSE) after headers arrive; the goroutine+timer+select shape keeps the stream alive while still bounding the wait.
