# Logging Guidelines

> How reproxy logs, documented from the actual implementation (`proxy.go`).

---

## Shape

One line per event via the injectable logger:

```go
// Proxy.Log is *log.Logger; nil falls back to log.Default() (tests inject a
// bytes.Buffer to assert log output).
func (p *Proxy) logLine(event, msg string) {
    logger := p.Log
    if logger == nil { logger = log.Default() }
    logger.Printf("event=%s %s", event, msg)
}
```

Every line starts with `event=<kind>` followed by space-separated `key=value` pairs — greppable, zero dependencies, no timestamps (the std logger already prefixes them).

## Event catalog (fixed — extend, don't invent parallel kinds)

| Event | When | Keys |
|---|---|---|
| `request` | one per request (summary) | method, target, status, attempts, duration, **policy** |
| `retry` | each retry decision | target, attempt, status, reason, remaining_budget, attempts=cur/limit |
| `wait` | before each backoff sleep | target, backoff |
| `warn` | degraded body (cap exceeded) | message |
| `stream_truncated` | upstream dies after commit point | error detail |

Rules:

- Durations are rounded to milliseconds (`d.Round(time.Millisecond)`) — sub-ms noise adds nothing.
- `reason` is quoted (`%q`) — it's free text, the rest are machine-parsed tokens.
- `policy` on `request` is `segment`, `header`, or `none` — the carrier the retry policy arrived on (`none` = policy-less single-attempt pass-through; the mode concept was deleted in v0.3.0).
- **Errors returned to clients are also logged server-side exactly once** — the JSON error body names the reason; the log line is the server-side record of the same event. No double logging.
- Never log request bodies, target paths beyond host:port, or client query strings (they may carry credentials in signed URLs).
