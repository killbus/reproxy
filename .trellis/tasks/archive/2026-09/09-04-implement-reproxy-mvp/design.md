# Design: reproxy MVP (Go)

> 规范来源：audit-report.md Part 1（协议）+ ADR-0001（技术基线）。本文件定义模块边界、数据流与契约。

## 模块结构

```
reproxy/
├── main.go              # CLI flags、ServerConfig 组装、http.Server 启动
├── target.go            # R1 路径解析：PathTarget{Scheme,Host,Port,RawPath} + 严格 grammar
├── query.go             # R2 query 拆分：SplitQuery(raw) → (upstreamQuery, retryParams, err)
├── policy.go            # R3 policy 解析：ParseRetryPolicy(params) → Policy{Default, ByStatus}
├── body.go              # R4 body 捕获：CaptureBody(r, cap) → (*CapturedBody, oversized, err)
├── proxy.go             # R5+R6 核心：retry 循环 + commit point 守卫 + 流式转发
├── ssrf.go              # R7 resolve-then-pin：校验 + 钉扎拨号（自定义 DialContext）
├── backoff.go           # R5 退避计算：exponential/linear/constant × jitter × Retry-After
├── config.go            # R9 ServerConfig：flags → 校验 → clamp 规则
└── *_test.go            # 每模块对应测试
```

零框架（不用 gin/echo）：`net/http` + `httputil.ReverseProxy` 只作为流式转发内核复用（见 §数据流）。

## 核心数据流

```
client request
  → handler(proxy.go)
      (1) target.ParsePath(r.URL.EscapedPath())        [target.go]
      (2) query.SplitQuery(r.URL.RawQuery)              [query.go]
      (3) policy.Parse(retryParams, serverCfg)         [policy.go]
      (4) body.Capture(r.Body, cap)                     [body.go]
      (5) ssrf.ResolveAndValidate(host) → pinnedIPs     [ssrf.go]
      (6) for attempt := 1..attempts:
            outbound := buildOutboundRequest(target, upstreamQuery, capturedBody, attempt)
            resp := transport.RoundTrip(outbound)        [ssrf-pinned transport]
            ├─ 网络错误/超时 → retryable(network)? → backoff → continue
            ├─ status ∈ retry.status → 丢弃响应 → backoff(含 Retry-After) → continue
            └─ 通过 → COMMIT: 写 headers → io.Copy(w, resp.Body) → return
          耗尽 → 最后响应或 504
```

**为什么手写 RoundTrip 循环而不用 `httputil.ReverseProxy`**：retry 决策需要"读响应头后、写字节前"的判定点（commit point 前的最后一扇门），ReverseProxy 的 ErrorHandler 在响应已开始后介入太晚。改用 `http.Transport.RoundTrip` 直接驱动——hop-by-hop 头剥离（`Connection` 及其列出的字段、`Keep-Alive`、`Transfer-Encoding` 等）由 `proxy.go` 内小函数处理，SSE flush 由 `http.Flusher` 按块触发。这与 Traefik retry.go 的结构同构（它同样在 middleware 层自建循环）。

## 契约定义

**Body cap**：默认 10 MiB（审计报告项 7 建议值；通用代理定位，非场景特化）。运维可 `--max-body` 调整。机制：Traefik 探测式 cap + 超限降级。

### target.PathTarget

```go
type PathTarget struct {
    Scheme  string // "http" | "https"
    Host    string // DNS 名 / IPv4 字面量 / IPv6 字面量（无方括号）
    Port    int    // 显式或 scheme 默认
    RawPath string // 原样透传（含前导 "/"），始终非空（空 → "/"）
}
// ParsePath(escapedPath string) (PathTarget, *RequestError)
```

### query.SplitQuery

```go
// 基于原始 query string 逐 key 切割（保留顺序与编码），不 parse/re-serialize
// 返回值 upstreamQuery 保留原始字节（含 ?a&b 无值 key 形式）
func SplitQuery(rawQuery string) (upstreamQuery string, retryParams url.Values, err *RequestError)
```

### policy.Policy

```go
type ScopePolicy struct {  // 某一 scope 的字段集（* 与 NNN 共用）
    Attempts   int
    Backoff    string       // "constant"|"linear"|"exponential"
    Initial, Max time.Duration
    Jitter     string       // "none"|"full"|"equal"
    RetryAfter string       // "honor"|"ignore"
}
type Policy struct {
    Default ScopePolicy              // [*]（或内置默认）
    ByStatus map[int]ScopePolicy     // 精确码 override（与 Default 合成后使用）
    StatusGate  []int                // retry.status 集合（sorted、去重）
    NetworkGate bool                 // retry.network
    Budget      time.Duration
}
// Parse(params url.Values, cfg ServerConfig) (Policy, *RequestError)
// Effective(status int) ScopePolicy —— 合成 [*] + [status]（字段级 override）
```

### body.CapturedBody

```go
type CapturedBody struct {
    Data []byte      // cap 内全量；oversized 时为空
    Oversized bool
    Len  int64
}
func Capture(src io.Reader, cap int64) (*CapturedBody, *RequestError)
// 默认 cap 10 MiB。io.ReadFull(cap+1) 探测；oversized 且 strict → 413 RequestError；
// oversized 且非 strict → 降级标记（handler 加 X-Retry-Dropped 并跳过重试逻辑直传）
```

### ssrf.Resolver（resolve-then-pin）

```go
type PinnedResolver struct { Transport *http.Transport }
// ResolveAndValidate(host, port) ([]netip.Addr, *RequestError)
//   net.Resolver.LookupIPAddr → 逐一 isForbiddenIP → 全部通过才返回
// DialContext: dial 时二次断言 IP（isForbiddenIP 再查一次）→ 用已解析 IP + tls.ServerName=Host
```

## 关键实现决策

1. **hop-by-hop 剥离**：单测覆盖 RFC 9110 §7.6.1 清单（Connection、Keep-Alive、Proxy-Authenticate 等 + Connection 头列出的任意 token）。Via/X-Forwarded-For 由我们追加（1.0/1.1 代理链格式）。
2. **TTFB 超时**：`context.WithTimeout` 包裹单次 RoundTrip；headers 到达即 cancel 不影响 body 读取（body 用独立 context——`context.WithCancelCause` 挂在请求 context 下，响应开始后切换 idle watchdog）。
3. **Retry-After 解析**：`http.ParseTime`（覆盖三种 HTTP-date 变体）+ 秒整数解析；过去 → 0；缺失 → 用计算 backoff。
4. **per-request transport**：MVP 用单个共享 `*http.Transport`（连接池复用），DialContext 内做 resolve-then-pin。`MaxIdleConnsPerHost` 默认走 Go 默认，不做 per-host 调优。
5. **降级透明**：body 超限非 strict 模式 = 单次直传（attempts=1 语义），行为与普通转发完全一致 + `X-Retry-Dropped` 头。
6. **错误响应统一格式**：`RequestError{Code, Reason, Hint}` → JSON body `{"error": reason, "hint": hint}`，header 带对应 Retry 相关字段。

## 测试策略

- 单测先行：每模块表驱动测试（含全部 400 边界、SSRF 禁段全清单、backoff 公式数值断言、Retry-After 三态）
- 集成：`httptest.NewServer` mock upstream（可注入状态码序列 / 延迟 / chunked SSE），handler 层直接调用
- E2E smoke：真实起进程 + curl 验证（验收 AC 最后一条）
- 签名 URL 字节保真：构造带 `%2F`/`%20`/`+`/无值 key/重复 key 的 query，断言 upstream 收到的 RawQuery 逐字节相等
