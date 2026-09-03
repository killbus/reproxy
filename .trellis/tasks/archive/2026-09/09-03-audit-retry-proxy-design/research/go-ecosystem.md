# Go 生态调研：轻量 HTTP Retry Reverse Proxy 的复用性审计

> 任务：09-03-audit-retry-proxy-design（TEAM B）
> 调研日期：2026-09-03（所有引用均在当日访问验证）
> 结论速览：**推荐 (a) 纯 stdlib 组合 + 少量 retry 库复用（backoff/jitter 策略层），自建 retry 循环与 body replay。Caddy/Traefik/现有项目均无法满足"per-request query 驱动 retry policy + 路径派生动态 upstream"这一核心组合。**

---

## 1. net/http/httputil.ReverseProxy（stdlib）

### 1.1 内置 retry：不存在（已验证）

- [pkg.go.dev/net/http/httputil](https://pkg.go.dev/net/http/httputil)（go1.27.1，访问于 2026-09-03）：包文档中完全不出现 "retry" 一词。后端连接失败走 `ErrorHandler`（默认 log + 502 Bad Gateway）；后端返回任何状态码（含 5xx）都会流经 `ModifyResponse`，不会被自动重试。
- `ErrorHandler` 文档原文："ErrorHandler is an optional function that handles errors reaching the backend or errors from ModifyResponse. If nil, the default is to log the provided error and return a 502 Status Bad Gateway response."
- `ModifyResponse` 文档原文："It is called if the backend returns a response at all, with any HTTP status code."——即 500 响应会原样透传给客户端，不会触发重试。

**结论：retry 必须由我们自己在 ReverseProxy 外层（handler 层）或内层（自定义 Transport/RoundTripper）实现。**

### 1.2 Streaming / SSE：FlushInterval 语义（已验证）

[pkg.go.dev/net/http/httputil](https://pkg.go.dev/net/http/httputil) 与 Go 源码 [`src/net/http/httputil/reverseproxy.go`](https://raw.githubusercontent.com/golang/go/master/src/net/http/httputil/reverseproxy.go)（访问于 2026-09-03）：

- `FlushInterval == 0`：不做周期性 flush。
- `FlushInterval < 0`：每次 write 到 client 后立即 flush（适合 SSE 之外的流式场景）。
- **特殊自动检测**（源码 `flushInterval()` 函数）：响应 `Content-Type: text/event-stream`（按 [eventsource 规范](https://www.w3.org/TR/eventstream/#text-event-stream)）或 `ContentLength == -1` 时，忽略配置值、**立即 flush**。源码注释："For Server-Sent Events responses, flush immediately."
- 响应已开始输出后的错误处理：源码在 `copyResponse` 出错时（`WriteHeader` 已调用）执行 `panic(http.ErrAbortHandler)`，注释引用 Issue 23643："Since we're streaming the response, if we run into an error all we can do is abort the request."

**结论：SSE 场景下 stdlib ReverseProxy 的 flush 行为符合 streaming-first 要求；且"响应已开始输出 = 不可再 retry"这一边界在 stdlib 中由 panic-abort 语义隐式成立（我们需在自己的 retry 层显式判断）。**

### 1.3 加 retry 的两个挂载点

**(1) 包 http.ResponseWriter 检测 "response started"**：在 `WriteHeader(code)` 第一次调用前，可以安全地放弃当前响应、重新发起上游请求。标准做法是实现自定义 ResponseWriter 装饰器（Traefik 就是这么做的，见 §4）。

**(2) 自定义 http.RoundTripper**：`ReverseProxy.Transport` 字段可注入自定义 RoundTripper，在 `RoundTrip` 内部实现 retry 循环。**注意**：go-retryablehttp 的 `RoundTripper` 包装器正是此模式（见 §2.1），但它会在 RoundTrip 内读空并关闭响应体（`drainBody`，上限 `respReadLimit = 4096` 字节，源码 [`client.go`](https://raw.githubusercontent.com/hashicorp/go-retryablehttp/main/client.go) 验证），对于最终成功响应的 body 传递没有破坏，但 retry 决策只能看到 status code + headers（对本案已足够）。

### 1.4 GetBody 归属：client-side 专用（已验证）

Go 源码 [`src/net/http/request.go`](https://raw.githubusercontent.com/golang/go/master/src/net/http/request.go)（访问于 2026-09-03）`Request.GetBody` 字段注释原文：

```go
// GetBody defines an optional func to return a new copy of
// Body. It is used for client requests when a redirect requires
// reading the body more than once. Use of GetBody still
// requires setting Body.
//
// For server requests, it is unused.
```

**结论：服务端收到的 `req.Body` 是单向流，`GetBody` 由 `http.NewRequestWithContext` 在 client 侧自动设置（body 为 `*bytes.Buffer`/`*bytes.Reader`/`*strings.Reader` 时），代理场景下 server request 不会有 GetBody——body replay 完全是我们的责任。**

### 1.5 服务端 body replay 的标准做法（已验证）

- server 端 `req.Body` 为单遍 `io.ReadCloser`，不可重复读（pkg.go.dev/net/http `Request.Body` 文档 + `ErrBodyReadAfterClose` 存在性验证）。
- 标准工作区（跨项目一致的方案）：
  1. **内存缓冲 + 上限**：`io.ReadAll(io.LimitReader(r.Body, max+1))` 检测超限；或使用 `http.MaxBytesReader(w, r.Body, n)`（源码验证：超限时返回 `*MaxBytesError` 并提示 ResponseWriter 关闭连接）。
  2. **重放**：`r.Body = io.NopCloser(bytes.NewReader(body))`，每次重试用新的 `bytes.Reader`。
  3. **超限降级**：body 超过上限时禁用重试（Traefik 的做法，见 §4.2），或落到 temp-file spooling（自定义代码，stdlib 无现成实现）。

### 1.6 Transport 层自动 retry 的边界（已验证，供安全参考）

Go 源码 [`src/net/http/transport.go`](https://raw.githubusercontent.com/golang/go/master/src/net/http/transport.go) `shouldRetryRequest` / `isReplayable`（访问于 2026-09-03）：

- `http.Transport` 仅在**复用连接**上、收到响应前失败时才自动换连接重试。
- 重放条件：方法为 GET/HEAD/OPTIONS/TRACE，或带 `Idempotency-Key`/`X-Idempotency-Key` header；且 body 为 nil/NoBody 或 GetBody != nil。**POST 即使设置了 GetBody 也不会被 Transport 自动重试**（注释原文："Don't retry non-idempotent requests"）。

**结论：显式支持 non-idempotent POST retry 必须在应用层（我们的 proxy 层）完成，不能依赖 Transport。**

---

## 2. Client-side retry 库（作为构建块）

### 2.1 hashicorp/go-retryablehttp（最新 v0.7.8）

版本：[GitHub tags](https://github.com/hashicorp/go-retryablehttp)（访问于 2026-09-03，最新 tag v0.7.8）。MPL-2.0。

**Body replay 机制**（源码 [`client.go` getBodyReaderAndContentLength](https://raw.githubusercontent.com/hashicorp/go-retryablehttp/main/client.go) 已验证）：
- `[]byte` / `*bytes.Buffer` / `*bytes.Reader`：零拷贝复用（每次 `bytes.NewReader(buf)`）。
- `io.ReadSeeker`：用 `Seek(0,0)` 回卷，但官方注释警告与 net/http 存在偶发 data race，"should be avoided if possible"。
- **任意 `io.Reader`：`io.ReadAll` 全量读入内存，无大小上限（源码中无任何 cap 检查）。**
- 代理场景（`FromRequest(r *http.Request)` / [`roundtripper.go` RoundTripper](https://raw.githubusercontent.com/hashicorp/go-retryablehttp/main/roundtripper.go)）：server 收到的 body 是普通 io.Reader，必然走全量内存读取路径，且无上限——**直接作为 proxy 的 Transport 会引入 OOM 风险，必须在外层先行 cap**。

**Retry policy**（源码已验证）：
- `type CheckRetry func(ctx context.Context, resp *http.Response, err error) (bool, error)` —— 签名同时给出 status-code + 网络错误的判断点，**完全可自定义**。
- `DefaultRetryPolicy` 实际行为（源码，与 README 的 "500-range except 501" 描述不一致，README 漏了 429）：连接类错误重试；**429 重试**（源码注释："429 Too Many Requests is recoverable."）；`status == 0 || (>= 500 && != 501)` 重试。
- `DefaultBackoff`：指数退避（2^attempt × min），且 **429/503 时解析 `Retry-After` header**（源码验证）。
- `RateLimitLinearJitterBackoff`（v0.7.8 新增）：429/503 且有 Retry-After 时按 header 等待，否则 linear jitter。
- `Backoff func(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration` —— backoff 函数能看到响应，方便实现 per-status backoff。

**适配评估**：接口形状（CheckRetry + Backoff + 请求前 body rewind）与我们的需求几乎一一对应，但 (1) body 无上限内存读取需在外层 cap；(2) 其 `Client.Do` 循环在重试间 `drainBody`（上限 4096 字节）以复用连接——作为参考实现价值高，直接整体复用价值中等。

### 2.2 cenkalti/backoff（最新 v7.0.0，2026-06-30）

版本时间线（[Go module proxy](https://proxy.golang.org/github.com/cenkalti/backoff/v7/@latest) + GitHub tags，访问于 2026-09-03）：v4.3.0（2024-01）→ v5.0.3（2025-07-23）→ v6.0.1（2026-06-16）→ **v7.0.0（2026-06-30）**。Traefik 目前使用 v4（其源码 import 验证）。

v7 关键特性（[pkg.go.dev/backoff/v7](https://pkg.go.dev/github.com/cenkalti/backoff/v7) 验证）：
- 泛型 API：`Retry[T any](ctx context.Context, operation Operation[T], opts ...RetryOption) (T, error)`。
- `WithMaxTries(n)`："limits the total number of attempts, not retries" —— **总尝试次数语义，与我们的 attempts 语义讨论直接相关**。
- `ExponentialBackOff`：`InitialInterval`（默认 500ms）、`RandomizationFactor`（默认 0.5，即 ±50% jitter）、`Multiplier`（默认 1.5）、`MaxInterval`（默认 60s）。v7 中 `MaxElapsedTime` 改为 `Retry` 的 option。
- **`RetryAfter(d, cause)`**：返回 `RetryAfterError` 使 Retry 等待指定时长并**重置退避序列**——为 server 指定延迟（Retry-After）提供了内建支持。
- v4（Traefik 在用）：`Retry(Operation, BackOff)`、`WithMaxRetries`（重试次数语义）、`RetryNotifyWithData` 等。

**适配评估**：非常适合驱动 backoff/jitter 层（业界事实标准，Traefik 同款）。注意 v4→v7 语义差异（MaxRetries vs MaxTries）与"非并发安全、每次 Retry 需独立 BackOff 实例"。

### 2.3 avast/retry-go（最新 v5.0.0，2025-11-24；v4.7.0 2025-10-14）

[pkg.go.dev/avast/retry-go/v4](https://pkg.go.dev/github.com/avast/retry-go/v4)（访问于 2026-09-03）验证：

- `retry.Do(fn, opts...)` / `retry.DoWithData[T]`；选项：`Attempts(n)`（0=无限）、`RetryIf(func(err error) bool)`、`Unrecoverable(err)`、`AttemptsForError(n, err)`、`OnRetry`、`Context`、`MaxDelay`、`MaxJitter`。
- DelayType：`BackOffDelay`（默认，指数）、`FixedDelay`、`RandomDelay`、`CombineDelay(...)`（组合，如 BackOff + jitter）、`FullJitterBackoffDelay`（v4.7.0，`sleep = random_between(0, min(cap, base*2^attempt))`，AWS 风格 full jitter）。
- **jitter 类型齐全**（full jitter / random / combine）。

**适配评估**：通用函数重试器，不含 HTTP 语义（status code / Retry-After 需自己在 fn 内翻译为 error），适合做 backoff 引擎之一；与 cenkalti/backoff 二选一即可。

### 2.4 failsafe-go（最新 v0.9.7，2026-08-16）

[failsafe-go/failsafe-go](https://github.com/failsafe-go/failsafe-go)（GitHub，访问于 2026-09-03，v0.9.7 发布于 2026-08-16，仍为 0.x）：

- 策略组合（policy composition）：Retry、Fallback、CircuitBreaker、RateLimiter、Bulkhead、Timeout、Hedge、Cache 等，可链式组合（这是它区别于上述库的核心卖点）。
- RetryPolicy（[failsafe-go.dev/retry](https://failsafe-go.dev/retry) 验证）：`HandleErrors(...)`、`HandleResult(nil)`、`HandleIf(func(response *http.Response, err error) bool)`（**官方示例即按 http.Response 判断**）、`WithBackoff(initial, max)`、`WithJitterFactor(.1)`、`WithJitter(d)`、`WithDelayFunc`（可依据执行结果计算延迟——Retry-After 可在此实现）、`WithMaxAttempts(n)`（**总尝试次数**，默认 3）、`WithMaxRetries(n)`、`WithMaxDuration`、`AbortOnResult/AbortOnErrors/AbortIf`。
- 注意：`failsafe-go/limits` 不存在，正确名称就是 **failsafe-go/failsafe-go**；文档域名为 failsafe-go.dev。

**适配评估**：HandleIf + WithDelayFunc 对 status-code 触发与 Retry-After 都有直接表达力，策略可组合；缺点是 0.x 版本（API 稳定性风险）+ 引入较重的依赖树。

### 2.5 库选择小结

| 库 | status-code 触发 | Retry-After | body replay | jitter | 适配 proxy retry 循环 |
|---|---|---|---|---|---|
| go-retryablehttp v0.7.8 | CheckRetry 自定义（默认含 429/5xx） | DefaultBackoff 解析 | ReaderFunc（Reader 全量内存、无 cap） | linear jitter 可选 | **中**：可作参考/Transport，但需外层 body cap |
| cenkalti/backoff v7 | 无（纯退避库） | RetryAfter error 原语 | 无（不涉及） | RandomizationFactor | **高**（backoff 引擎层） |
| avast/retry-go v5/v4.7 | RetryIf 回调 | 无内建 | 无 | full/random/combine | **中高** |
| failsafe-go v0.9.7 | HandleIf(官方 HTTP 示例) | WithDelayFunc | 无 | jitterFactor + jitter | **中高**（0.x 风险） |

---

## 3. Caddy reverse_proxy

（源码 [`modules/caddyhttp/reverseproxy/reverseproxy.go`](https://raw.githubusercontent.com/caddyserver/caddy/master/modules/caddyhttp/reverseproxy/reverseproxy.go) 等，均访问于 2026-09-03；最新稳定版 v2.11.4，2026-06-03 发布，[GitHub Releases](https://github.com/caddyserver/caddy/releases) 验证）

### 3.1 retry 行为：lb_retries 与 retry_match（已验证 + 版本考据）

- **`lb_retries`（JSON `retries`）**：由 PR [#4756](https://github.com/caddyserver/caddy)（commit 2022-07-13 "reverseproxy: Implement retry count, alternative to try_duration"）引入，随 **Caddy v2.6.0（2022-09-20 发布）** 发布（[releases/tags/v2.6.0](https://github.com/caddyserver/caddy/releases/tag/v2.6.0) 验证）。文档原文："How many times to retry selecting available backends for each request if the next available host is down."
- **语义关键点**（源码 `tryAgain` + struct 注释验证）：这是 **upstream-selection 层级的 retry**（按 LB policy 换下一个 upstream），不是同一 upstream 的重试。
  - **Dial 错误（连接都没建立）总是可重试**，不区分方法。
  - **Transport 错误**：默认**只重试 GET**（源码注释："by default, don't retry requests if they aren't GET"），除非配置 `retry_match`。
  - **响应状态码触发重试**：`retry_match` 的 **CEL expression 形式**（占位符 `{rp.status_code}`、`{rp.status_text}`、`{rp.header.*}`、`{rp.is_transport_error}`）由 PR [#7569](https://github.com/caddyserver/caddy/pull/7569)（合并于 2026-04-21）加入，**随 v2.11.3（2026-05-12 发布）可用**（v2.11.3 tag 源码含 `lb_retry_match` 解析与 `retryableResponseError` 已验证）。早于 v2.11.3 的版本 retry_match 只能匹配请求侧条件，不能按响应状态码重试。
  - 重试耗尽时若为响应触发的重试，返回真实上游状态码而非 502（`retryableResponseError` 保底，源码验证）。
- **body replay**：`request_buffers` 选项（字节数上限）将请求体全量缓冲，retry loop 每轮用 `io.NopCloser(bytes.NewReader(bufferedReqBody.Bytes()))` 重放（PR #7360 修复，commit "capture the buffered body once, then reset clonedReq.Body before each retry"，2025 年中）。未设置时非 GET 请求基本不会被 transport-error 重试。
- **流式边界**：`finalizeResponse` 写出头后 `copyResponse` 出错 → `panic(http.ErrAbortHandler)`（引用 issue [#5951](https://github.com/caddyserver/caddy/issues/5951)），与 stdlib 相同；`context.Canceled` 写 499 且不重试。

### 3.2 动态 upstream（已验证）

- 内置 dynamic upstream 源（[`upstreams.go`](https://raw.githubusercontent.com/caddyserver/caddy/master/modules/caddyhttp/reverseproxy/upstreams.go)）：`dynamic srv`（SRV 记录）、`dynamic a`（A/AAAA）、`dynamic multi`（聚合多源）。**每次 proxy-loop 迭代都会重新取 upstream 列表**（注释原文："Dynamic upstreams are retrieved at every iteration of the proxy loop for each request"）。
- **没有**内置的"路径/占位符派生目标 upstream"模块。变通能力：`Upstream.Dial` 地址支持 placeholder（源码 `fillDialInfo`：`repl.ReplaceAll(u.Dial, "")`，注释 "the dial address may vary per-request if placeholders are used"）——即可以把请求路径/查询参数塞进 dial 地址实现 per-request 拨号，但 scheme/路由仍受 transport 与 upstreams 结构约束，且 active health check 对 dynamic upstreams 不生效（注释原文："Active health checks do not work on dynamic upstreams"）。
- Caddyfile `handle_path` + placeholder dial 理论上可拼出路径派生 upstream，但这属于 hack 而非设计用途，SSRF 防护需自行负责。

### 3.3 per-request retry policy（query 驱动）可否表达

**不能。** `retries`/`try_duration`/`retry_match` 均为静态配置项；CEL 表达式可引用请求占位符（如 `{query.retry}`）判断**是否**重试，但**重试次数上限、per-status attempts 覆盖、backoff 形状均不可按请求变化**，且 try_interval 是固定值、没有指数退避、没有 Retry-After 支持。要满足需求必须写 Caddy plugin（自定义 module 编译进自定义构建）。

**结论**：Caddy 适合"静态配置的集群故障转移"；我们的"每请求策略"需求在其配置模型之外。作为 Go 库嵌入（import caddyserver/caddy/v2）在技术上可行，但依赖树和配置面远超我们所需。

---

## 4. Traefik

（文档 [reference/routing-configuration/http/middlewares/retry](https://doc.traefik.io/traefik/reference/routing-configuration/http/middlewares/retry/) + 源码 [`pkg/middlewares/retry/retry.go`](https://raw.githubusercontent.com/traefik/traefik/master/pkg/middlewares/retry/retry.go)、[`pkg/server/service/loadbalancer/mirror/mirror.go`](https://raw.githubusercontent.com/traefik/traefik/master/pkg/server/service/loadbalancer/mirror/mirror.go)，访问于 2026-09-03；最新 v3.7.12 / v2.11.56，2026-08-26 发布）

### 4.1 retry 中间件语义（已验证）

- 文档："retries requests a given number of times to a backend server if that server does not reply"（attempts = 重试次数；`attempts == 1` 时直接跳过中间件，源码验证）。
- 默认**只在网络层失败时重试**："As soon as the server answers, the middleware stops retrying, regardless of the response status."
- 配置项：
  - `status`（如 `"400"`、`"500-599"`）：按状态码范围重试。
  - `disableRetryOnNetworkError`（默认 false）：禁用网络错误重试（此时必须配 status）。
  - `retryNonIdempotentMethod`（默认 false）：允许 POST/LOCK/PATCH 重试。
  - `initialInterval`：指数退避首间隔，**最大间隔 = 2 × initialInterval**（源码验证：multiplier = 2^(1/(attempts-1))）；不设则立即重试（无退避）。
  - `timeout`：重试总时限。
  - `maxRequestBodyBytes`（默认 2MB，源码 `RetryDefaultMaxRequestBodyBytes = 2 * 1024 * 1024` 验证）：可重放 body 的大小上限。
- **版本考据**：状态码重试 + timeout + non-idempotent 由 PR [#12667](https://github.com/traefik/traefik/pull/12667)（commit 2026-02-20 "Enable retries based on HTTP response status codes, timeout, and non-idempotent methods"）加入，**随 v3.7.0（2026-05-05 发布）**（[v3.7.0 release notes](https://github.com/traefik/traefik/releases/tag/v3.7.0) 明确列出该条）。更早版本仅有网络错误重试。

### 4.2 body replay 与流式边界（源码级验证，对我们最有参考价值）

- 状态码重试启用时，用 `mirror.NewReusableRequest(req, maxRequestBodyBytes)`：读 `maxBodySize+1` 字节探测超限（`io.ReadFull` + `ErrUnexpectedEOF` 判断）；**超限则禁用状态码重试**（`statusCodes = nil`），已读字节用 `peekedBody` 包装回填；`-1` = 无限制 `io.ReadAll`。每次重试用 `req.Clone(ctx)` + `io.NopCloser(bytes.NewReader(body))`。
- 网络错误重试的精准边界（2026 年新增的 `WrapHandler` + httptrace）：只有**请求已到达 proxy 层（proxyReached）但尚未向后端写出任何字节（!wroteRequest，由 httptrace `WroteHeaders`/`WroteRequest` 钩子追踪）**时才重试——"no bytes were sent to the backend yet"。中间链路（如 auth 中间件返回 401）的响应不会被重试。
- 流式边界：operation 在 `retryResponseWriter.written || hijacked` 时立即终止（"End the operation as soon as the client received something"）；`WriteHeader` 里若状态码命中重试范围则吞掉本次写入（`shouldNotWrite`），不向客户端输出任何字节。
- backoff：cenkalti/backoff v4 的 ExponentialBackOff（`WithContext` 支持取消）。

**这一整套 "buffer-with-cap + peekedBody 降级 + httptrace 写出检测 + written 边界" 是我们实现的最佳参考模板**（许可证 Apache-2.0，可对照实现）。

### 4.3 per-request 动态 upstream

**不支持。** Traefik 的 router/service 是配置驱动的；路径派生后端需要社区 plugin（如 plugins.traefik.io 的 "Dynamic Backend" 类插件，Yaegi 解释执行、有性能开销）或 Programmatic/HTTP provider（本质是配置更新而非每请求路由）。且 Traefik 是应用/路由器形态，**不可作为库嵌入**（与 Caddy 不同）。

**结论**：Traefik retry 中间件的功能面（状态码 + 网络错误 + 非幂等 + body cap + 指数退避）与我们的单请求需求高度重合，但其配置是静态全局的；作为运行时产品无法承载"query 驱动 per-request policy + 路径派生 upstream"。作为源码参考价值极高。

---

## 5. 现有 Go 项目接近度评估

（均访问于 2026-09-03）

### 5.1 gost / go-gost（v3.3.0，2026-08-30 发布，活跃）

- [go-gost/gost](https://github.com/go-gost/gost)（7.4k stars）：forward proxy / relay / port forwarding / 反向隧道，协议覆盖极广（HTTP/2/3、SOCKS5、Shadowsocks、QUIC……）。
- **无 retry**：`go-gost/x` 仓库（1052 个文件的 git tree）中无任何 retry 相关文件；`handler/http/service.go` 与 `chain/chain.go` 源码 grep "retry" 零命中（已验证）。
- 无路径派生的 per-request HTTP upstream retry。
- **复用价值**：定位完全不同（tunnel/relay），且其 handler 架构围绕多协议转发而非 HTTP 语义级 retry。**不适合。**

### 5.2 one-api（v0.6.10，2025-02-02 发布，维护放缓）与 New-API（v1.0.0-rc.30，2026-08-31 发布，非常活跃）

- [songquanpeng/one-api](https://github.com/songquanpeng/one-api)、[QuantumNous/new-api](https://github.com/QuantumNous/new-api)（one-api 的活跃 fork）：OpenAI 兼容 AI 网关，多渠道（channel）负载均衡 + 失败换渠道重试。
- one-api 源码（[`controller/relay.go`](https://raw.githubusercontent.com/songquanpeng/one-api/main/controller/relay.go) 已验证）：`retryTimes := config.RetryTimes`（**全局配置，非 per-request**）；`shouldRetry()`：429 → true，5xx → true，400 → false，2xx → false，其余 → true；body 用 `common.GetRequestBody`（`io.ReadAll` 全量读入 gin context，**无大小上限**）；重试 = **换另一个渠道**（`CacheGetRandomSatisfiedChannel`），不是同一 upstream 重试；**无 backoff（立即重试）、不解析 Retry-After**。
- New-API 源码（[`controller/relay.go`](https://raw.githubusercontent.com/QuantumNous/new-api/main/controller/relay.go) + [`setting/operation_setting/status_code_ranges.go`](https://raw.githubusercontent.com/QuantumNous/new-api/main/setting/operation_setting/status_code_ranges.go) 已验证）：在 one-api 基础上把重试状态码范围做成**运行时可配置的 status code ranges**（默认 1xx、3xx、401-407、409-499、500-503、505-523、525-599；always-skip：504/524）——这是"status list/range 表达 + 默认 scope"的现成参考；仍有 body 全量缓冲（413 处理）、渠道切换而非同 upstream 重试、全局配置。
- **共同缺失 vs 我们的需求**：非路径派生任意 upstream（upstream 是配置好的渠道列表）；非 per-request policy（query 驱动）；无 backoff/jitter/Retry-After；非通用透明 relay（业务耦合 OpenAI 语义）。
- **复用价值**：协议设计参考（status ranges 字符串格式 `"500-599,429"`、shouldRetry 白/黑名单结构、渠道切换去重 lastFailedChannelId），**不宜作为基座**。

### 5.3 其他

- [webtor-io/retry-proxy](https://github.com/webtor-io/retry-proxy)："Reverse proxy with retry capabilities"，但 0 star、最后 push 2022-01-09（GitHub API 验证）——**已废弃**，不可依赖。
- cors-anywhere 类 Go relay：GitHub API 搜索 "retry reverse proxy path dynamic upstream language:Go" 仅命中个人学习项目（≤1 star，如 sohamm3/resilient-reverse-proxy、joeycumines/ai-concurrency-shaper 等，2026 年仍在 push 但无社区采用）。**没有维护良好的现成项目同时做"路径派生 upstream + 状态码重试 + SSE"。**
- 结论：**该组合（path-based dynamic upstream + per-request query-driven retry policy + SSE + non-idempotent POST replay）在 Go 生态中是空白。**

---

## 6. 各项目复用性评估表

| 项目/库 | 动态 upstream（路径派生） | per-request retry policy | 状态码触发 retry | POST body replay | SSE/流式 | Retry-After/backoff | 嵌入性 | 维护状态 |
|---|---|---|---|---|---|---|---|---|
| stdlib ReverseProxy | ✗（需自建 URL 重写） | ✗ | ✗ | ✗（需自建缓冲） | ✓（FlushInterval -1 / 自动检测 SSE） | ✗ | ✓（库） | Go 1.27.x |
| go-retryablehttp | n/a | 部分（CheckRetry 可编程但按 client 静态） | ✓（自定义 CheckRetry；默认 429+5xx） | ✓ ReaderFunc，但 Reader 无 cap 全量内存 | ✓（RoundTripper 层不影响流式透传） | 部分（DefaultBackoff 解析 Retry-After） | ✓（库/Transport） | 活跃（HashiCorp/IBM） |
| cenkalti/backoff v7 | n/a | n/a | n/a | n/a | n/a | ✓（RetryAfter + jitter） | ✓ | 活跃（v7 2026-06-30） |
| avast/retry-go v5 | n/a | n/a | n/a（RetryIf 回调） | n/a | n/a | ✓（full jitter） | ✓ | 活跃 |
| failsafe-go v0.9.7 | n/a | 部分（policy 可运行时构造） | ✓ HandleIf（官方 HTTP 示例） | ✗ | n/a | ✓ WithDelayFunc | ✓ | 活跃但 0.x |
| Caddy reverse_proxy | △（placeholder dial hack；dynamic 仅 DNS SRV/A） | ✗（静态配置；CEL 可读 query 但次数/退避固定） | ✓（v2.11.3+ lb_retry_match expression） | ✓（request_buffers 有 cap） | ✓（同 stdlib flush 语义） | ✗（固定 try_interval，无退避、无 Retry-After） | △（可作库但重） | 活跃（v2.11.4 2026-06-03） |
| Traefik retry 中间件 | ✗（需 plugin） | ✗（静态配置） | ✓（v3.7.0+ status ranges） | ✓（2MB cap + 超限降级 + httptrace 边界） | ✓（written 边界终止重试） | 部分（指数退避，无 Retry-After） | ✗（不可嵌入） | 活跃（v3.7.12） |
| gost v3.3.0 | ✗ | ✗ | ✗（无 retry） | n/a | n/a | n/a | ✓ | 活跃 |
| one-api / New-API | ✗（渠道列表） | ✗（全局 RetryTimes；New-API 状态码范围可配置） | ✓ | ✓（无 cap 全量内存） | 部分（有 SSE relay 但 retry 与流式边界未按本需求设计） | ✗（无退避） | △（需剥离业务） | one-api 放缓 / New-API 活跃 |

---

## 7. Go 路线最终判定

**判定：(a) 纯 stdlib 组合 + 复用 1 个退避库（cenkalti/backoff v7 或 avast/retry-go）为最优路线；不基于 Caddy/Traefik/现有项目扩展。**

### 7.1 四条路线的最小自定义代码量

1. **纯 stdlib 组合（推荐）**：
   - 复用：`net/http` server + `httputil.ReverseProxy`（流式转发、hop-by-hop 头处理、SSE flush）或直接手写 `RoundTrip` 循环；`http.MaxBytesReader`/`io.LimitReader`（body cap）；backoff 库（jitter/RetryAfter 原语）。
   - 自建（约 500–1000 行核心代码）：路径 → upstream URL 解析（含 SSRF 校验）、query → retry policy 解析（scope/inheritance/range/attempts 语义）、retry 循环（状态码 + 网络错误，写头前判定）、body 捕获与重放（内存 cap + 超限降级可选 temp-file spooling）、"已开始输出" 边界（ResponseWriter WriteHeader 守卫，参照 Traefik responseWriter）。
2. **Caddy 扩展**：需写自定义 module（upstream source plugin + retry policy plugin）并维护自定义构建；Caddy 本体 2.11.3 才有状态码重试且无退避/Retry-After，等于在别人的配置模型里重写一遍我们的需求。自定义代码量 ≥ stdlib 路线，且引入巨型依赖。**否决。**
3. **Traefik**：不可嵌入；配置静态；per-request 需求无法表达。其 retry.go 是最好的**参考实现**（Apache-2.0）。**否决（作为运行时），采纳（作为代码参考）。**
4. **现有项目**：one-api/New-API/gost 定位不符（渠道网关/隧道），剥离成本高于自建。**否决。**

### 7.2 对 PRD 的三条直接输入

1. **attempts 语义**：业界分两派——cenkalti/backoff v7 `WithMaxTries`、failsafe-go `WithMaxAttempts` = 总尝试次数；cenkalti v4 `WithMaxRetries`、avast/retry-go `Attempts`、Traefik `attempts` = 重试次数（不含首次）。**建议我们的 `attempts` 采用"总尝试次数"语义**（与 v7/failsafe-go 一致，避免 off-by-one 争议），并在文档中显式写明。
2. **body replay 方案**：采用 Traefik 模式——`io.ReadFull(max+1)` 探测超限 + cap 内内存缓冲重放 + **超限自动禁用状态码重试**（而非拒绝请求），`httptrace` 思路可用于"未向后端写出任何字节"的网络错误重试兜底。
3. **status range 表达**：New-API 已验证 `"100-199,500-599"` 风格的 range 配置在实践中可行（含 always-skip 白名单机制），可直接借鉴其字符串格式。

---

## 附：引用清单（均于 2026-09-03 访问）

- stdlib: https://pkg.go.dev/net/http/httputil 、https://pkg.go.dev/net/http 、https://raw.githubusercontent.com/golang/go/master/src/net/http/httputil/reverseproxy.go 、https://raw.githubusercontent.com/golang/go/master/src/net/http/request.go 、https://raw.githubusercontent.com/golang/go/master/src/net/http/transport.go
- go-retryablehttp: https://github.com/hashicorp/go-retryablehttp 、https://pkg.go.dev/github.com/hashicorp/go-retryablehttp 、https://raw.githubusercontent.com/hashicorp/go-retryablehttp/main/client.go 、https://raw.githubusercontent.com/hashicorp/go-retryablehttp/main/roundtripper.go
- cenkalti/backoff: https://pkg.go.dev/github.com/cenkalti/backoff/v4 、https://pkg.go.dev/github.com/cenkalti/backoff/v7 、https://proxy.golang.org/github.com/cenkalti/backoff/v7/@latest
- avast/retry-go: https://pkg.go.dev/github.com/avast/retry-go/v4
- failsafe-go: https://github.com/failsafe-go/failsafe-go 、https://failsafe-go.dev/retry 、https://failsafe-go.dev/policies
- Caddy: https://github.com/caddyserver/caddy/releases 、https://github.com/caddyserver/caddy/pull/7569 、https://raw.githubusercontent.com/caddyserver/caddy/master/modules/caddyhttp/reverseproxy/reverseproxy.go 、.../upstreams.go 、.../hosts.go 、.../streaming.go
- Traefik: https://doc.traefik.io/traefik/reference/routing-configuration/http/middlewares/retry/ 、https://github.com/traefik/traefik/releases/tag/v3.7.0 、https://raw.githubusercontent.com/traefik/traefik/master/pkg/middlewares/retry/retry.go 、https://raw.githubusercontent.com/traefik/traefik/master/pkg/server/service/loadbalancer/mirror/mirror.go
- 项目: https://github.com/go-gost/gost 、https://github.com/songquanpeng/one-api 、https://github.com/QuantumNous/new-api 、https://github.com/webtor-io/retry-proxy
