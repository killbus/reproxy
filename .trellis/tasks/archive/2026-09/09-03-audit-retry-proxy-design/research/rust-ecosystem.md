# Rust 生态调研：轻量 HTTP 重试反向代理（reproxy）

> 任务：审计「动态 upstream + 每请求重试策略 + SSE 流式优先」的轻量 retry 反向代理设计。
> 调研日期：2026-09-03。所有事实均来自下列引用来源的实际访问（WebFetch / GitHub API），未经验证的内容已标注 UNVERIFIED。

---

## 1. Pingora（Cloudflare）— 深度调研（最重要）

### 1.1 版本与成熟度

- Pingora 最新版本 **0.8.1**（2026-06-04 发布），docs.rs 显示 `pingora` / `pingora-proxy` 0.8.1（[docs.rs ProxyHttp](https://docs.rs/pingora/latest/pingora/proxy/trait.ProxyHttp.html)，[releases](https://github.com/cloudflare/pingora/releases)，访问 2026-09-03）。
- 仓库活跃：主仓 27k+ stars，2026-08 仍有提交（GitHub API，访问 2026-09-03）。
- 近期版本要点：0.8.0（2026-03-02，mTLS 客户端证书校验、CONNECT 代理默认禁用、UpgradedBody 显式化）；0.7.0（2026-01-30，ConnectionFilter、虚拟 L4 流）；0.6.0（2025-08-15，h2 安全升级）；0.5.0（2025-05-09，**"Add ability to configure max retries for upstream proxy failures"**）（[releases](https://github.com/cloudflare/pingora/releases)，访问 2026-09-03）。
- 注意：Pingora 是**框架/库**，不是开箱即用的代理（官方自述 "A library for building fast, reliable and evolvable network services"）。

### 1.2 `ProxyHttp` trait 与重试相关回调（已核实到源码级）

版本 0.8.1，来源：[docs.rs ProxyHttp](https://docs.rs/pingora/latest/pingora/proxy/trait.ProxyHttp.html) + 直接读取 `pingora-proxy/src/proxy_trait.rs`、`docs/user_guide/phase.md`、`failover.md`（GitHub API raw，访问 2026-09-03）：

- **`fail_to_connect(&self, session, peer, ctx, e: Box<Error>) -> Box<Error>`**（同步、非 async）：仅在**建立到 upstream 的连接出错**时调用。用户在此将错误标记为可重试：`e.set_retry(true)`；若标记，**`upstream_peer()` 会被再次调用**，从而可以选择同一 upstream 重试或 failover 到另一个 upstream。若不可重试，请求直接结束。文档明确："When `fail_to_connect()` is called, pingora-proxy guarantees that nothing was sent upstream."（[failover.md](https://github.com/cloudflare/pingora/blob/master/docs/user_guide/failover.md)，访问 2026-09-03）
- **`error_while_proxy(&self, peer, session, e, ctx, client_reused: bool) -> Box<Error>`**：连接建立（或复用）**之后**的代理阶段错误时调用。**默认实现**已核实源码：
  ```rust
  e.retry.decide_reuse(client_reused && !session.as_ref().retry_buffer_truncated());
  ```
  即默认策略是 `RetryType::ReusedOnly`——仅当连接是池中复用（请求可证明未发出）且 retry buffer 未被截断时才重试（`pingora-proxy/src/proxy_trait.rs`，访问 2026-09-03）。用户可覆盖此方法自行决定（包括对 POST 也重试）。
- **`RetryType`**（`pingora-error` 源码核实）：`enum RetryType { Decided(bool), ReusedOnly }`，`decide_reuse(reused)` 把 `ReusedOnly` 收敛为 `Decided(reused)`（`pingora-error/src/lib.rs`，访问 2026-09-03）。
- **重试上限**：`ServerConf.max_retries`，文档注释 "This setting is a fail-safe and defaults to 16"，外层循环 `while retries < self.max_retries`（`pingora-core/src/server/configuration/mod.rs`、`pingora-proxy/src/lib.rs`，访问 2026-09-03）。
- **响应已开始输出后**：官方 failover 文档："once the response header is already sent downstream, there is nothing the proxy can do other than logging an error and then giving up on the request."（[failover.md](https://github.com/cloudflare/pingora/blob/master/docs/user_guide/failover.md)，访问 2026-09-03）——与我们「输出开始即终止重试生命周期」的语义一致，由框架保证。

### 1.3 重试时的请求体（body）处理——Pingora 有内置 retry buffer

已核实到源码（这是关键结论）：

- `HttpSession`（下游）内建 **`retry_buffer: Option<FixedBuffer>`**，注释 "An internal buffer that holds a copy of the request body up to a certain size"（`pingora-core/src/protocols/http/v1/server.rs`，访问 2026-09-03）。
- **`Session::enable_retry_buffering()`** 在每次代理开始时由 `proxy_h1.rs` / `proxy_h2.rs` **无条件自动调用**（源码 `session.as_mut().enable_retry_buffering();`，`pingora-proxy/src/proxy_h1.rs` 与 `proxy_h2.rs`，访问 2026-09-03）——即 Pingora 代理默认始终启用请求体重试缓冲。
- **缓冲上限 64 KiB**：`const BODY_BUF_LIMIT: usize = 1024 * 64;`（`pingora-core/src/protocols/http/v1/common.rs` 与 `v2/server.rs`，访问 2026-09-03）。超过即截断：`get_retry_buffer()` 在 `is_truncated()` 时返回 `None`，`retry_buffer_truncated()` 可查询；截断后默认 `error_while_proxy` 策略会禁用重试。
- 重发路径（已核实源码）：`proxy_h1.rs` 中 `let buffer = session.as_ref().get_retry_buffer(); if buffer.is_some() || session.as_mut().is_body_empty() { ... send_body_to_pipe(...) }`——重试时若有完整 buffer 就整体重发请求体；`proxy_h2.rs` 同样模式（`if let Some(buffer) = session.as_mut().get_retry_buffer()`）。
- **社区对大 body 的补丁**：lindenbaum 维护了一个用 spool 文件（临时文件）作为 retry buffer 的分支，但因性能顾虑未上游（[issue #349 评论](https://github.com/cloudflare/pingora/issues/349)，访问 2026-09-03）。即：**>64 KiB 的请求体无法在当前 Pingora 上自动重试**（需要自己 fork/扩展，或自行在外层 buffer）。
- 相关 API（[docs.rs Session](https://docs.rs/pingora-proxy/latest/pingora_proxy/struct.Session.html)，访问 2026-09-03）：`enable_retry_buffering(&mut self)`、`get_retry_buffer(&self) -> Option<Bytes>`、`retry_buffer_truncated(&self) -> bool`、`read_request_body()`、`read_body_or_idle(bool)`。
- 注意陷阱（issue 核实）：`read_request_body()` **每调用只读一个 chunk 而非整个 body**（文档未写清），且直接消费下游流导致后续管道拿不到 body；正确做法是 `enable_retry_buffering()` + 循环读完 + `get_retry_buffer()`（[issue #349](https://github.com/cloudflare/pingora/issues/349)、[issue #575](https://github.com/cloudflare/pingora/issues/575)，访问 2026-09-03）。

### 1.4 能否按 HTTP 状态码重试（Envoy retriable-status-codes 式）？

**不能（原生不支持，截至 0.8.1 / 2026-09）。** 已核实：

- Pingora 重试触发点只有连接建立失败（`fail_to_connect`）和连接建立后的代理错误（`error_while_proxy`，默认 ReusedOnly）——**没有「读到 upstream 响应状态码后决定重试」的钩子**。
- Feature request [issue #873 "Allow status-code-driven upstream retries via a ProxyHttp hook"](https://github.com/cloudflare/pingora/issues/873)（2026-04-26 提出，OPEN）：明确指出 "Pingora retries upstream calls only on connect-time errors… status-code-driven retries — the common 502/503/504 case… cannot be expressed cleanly"，并对比 nginx `proxy_next_upstream`、Envoy `retry_policy.retry_on`、HAProxy `retry-on`。提案增加 `upstream_response_decision` 钩子（header 到达后、字节未下发前可返回 retryable error）。
- 对应 [PR #872](https://github.com/cloudflare/pingora/pull/872)（OPEN，未合并，2026-09-03 核实，1 条评论）。
- 该 issue 给出的 workaround：用户自己在 `upstream_response_filter` 里缓冲完整响应（状态码 + body），不满足条件就自行重新发起（递归式重试）——代价是只有小响应可缓冲，且要在用户层实现整个重试循环。
- **结论：在 Pingora 上做 status-code 驱动的重试，必须自己实现「缓冲响应头/体 + 中止 + 重发」逻辑，框架不给回调级别的支持。**

另有一个相关缺陷（风险提示）：[issue #979](https://github.com/cloudflare/pingora/issues/979)（OPEN，2026-08-26）：对 HTTP/2 upstream 的 `InvalidH2` 错误，pingora-proxy **无条件重试**（包括非幂等 POST，且请求体已完整发出后），违反 RFC 9110 §9.2.2 与自身文档；h1 路径是 `ReusedOnly` 安全的。若我们的 upstream 走 h2 且做显式非幂等重试，需要意识到这个库级行为差异。

### 1.5 动态 upstream（每请求）

- `upstream_peer(&self, session: &mut Session, ctx) -> Result<Box<HttpPeer>>` 是**两个必须实现的方法之一**，每个请求调用（重试时再次调用）。文档："Define where the proxy should send this request to."（[docs.rs ProxyHttp](https://docs.rs/pingora/latest/pingora/proxy/trait.ProxyHttp.html)，访问 2026-09-03）。从请求路径解析出目标 host:port 并构造 `HttpPeer` 返回即实现「路径表达动态 upstream」——社区 issue #575 中用户已在 `upstream_peer` 里按 header/路径选目标，属常规用法（[issue #575](https://cloudflare/pingora/issues/575)）。
- 修改转发路径/Host：`upstream_request_filter()` 中可改写 `RequestHeader` 的 uri（set_uri）、删改 header——issue #349 中用户展示了从 `/view` 前缀改写 upstream path 的完整例子（[issue #349](https://github.com/cloudflare/pingora/issues/349)，访问 2026-09-03）。

### 1.6 Peer/拨号控制（SSRF 钉扎能力）

- `HttpPeer::new<A: ToSocketAddrs>(address, tls, sni)` **接受已解析的 `SocketAddr`**，构造即拨号该地址；也可传 `("host", port)` 由其解析。另有 `new_uds`、`new_proxy`（CONNECT 代理）、`new_mtls`（[docs.rs HttpPeer](https://docs.rs/pingora/latest/pingora/upstreams/peer/struct.HttpPeer.html)，访问 2026-09-03）。
- **自定义 `Peer` trait 可行**：`pub trait Peer: Display + Clone`，必需方法 `address()/tls()/sni()/reuse_hash()`，其余 22 个 provided 方法（超时、bind_to、TCP 调优、verify_cert/verify_hostname、ALPN、CA、socket tweak hook 等）。`address()` 返回的就是 pingora 自己的 `SocketAddr`（inet/unix），**框架不做 DNS**——把域名解析成 IP 完全是用户的责任（[docs.rs Peer](https://docs.rs/pingora/latest/pingora/upstreams/peer/trait.Peer.html)，访问 2026-09-03）。→ 自己先 DNS 解析再传 IP，即可实现 SSRF 钉扎（预解析 + 连接时校验 SNI/Host 匹配）。
- TLS 校验可关/可自定 CA：`PeerOptions { verify_cert, verify_hostname, alternative_cn, ca, alpn, connection_timeout, read_timeout, ... }`（[peer.md](https://github.com/cloudflare/pingora/blob/master/docs/user_guide/peer.md)，访问 2026-09-03）。
- 默认 hop-by-hop 头处理：Pingora 会剥离开 hop-by-hop 字段；chunked 补齐；可用 `HttpUpstreamRequestPolicy::preserve()` 恢复透传（RFC 不合规，自担风险）（[peer.md](https://github.com/cloudflare/pingora/blob/master/docs/user_guide/peer.md)，访问 2026-09-03）。

### 1.7 SSE / 流式响应

- 响应流式：Pingora 代理为 duplex 模式（上游读响应与下游写并发），`upstream_response_body_filter` / `response_body_filter` 每个 body chunk 调用（[phase.md](https://github.com/cloudflare/pingora/blob/master/docs/user_guide/phase.md)，访问 2026-09-03）。
- SSE 实证：维护者在 [issue #841](https://github.com/cloudflare/pingora/issues/841)（2026-09-03 核实）中给出无法复现 macOS SSE 不 flush 的详细报告——curl -N 实测每秒事件实时到达，且单测覆盖 chunked body write + flush、1460B 写缓冲、超过缓冲的大事件、`response_duplex_vec` 批量 header+body 路径，"All flushed correctly to a real TCP client"。报告者原始问题未复现（Pingora 0.8.0/macOS，维护者在 master@d9e6d7a 无法复现）。issue #147（closed）：SSE body 可在 `upstream_response_body_filter` 逐 chunk 拿到（[issue #147](https://github.com/cloudflare/pingora/issues/147)）。
- **结论：SSE 透传在 Pingora 上是验证过的常规用法；无响应体缓冲（除非启用 pingora-cache）。**

### 1.8 Pingora 官方 retry 示例

`pingora-proxy/examples/backoff_retry.rs`（已读全文，GitHub raw，访问 2026-09-03）：在 `fail_to_connect` 里 `ctx.retries += 1; e.set_retry(true)`，在 `upstream_peer` 里按 `ctx.retries` 计算 `10^retries` ms 的指数退避并 `tokio::time::sleep`——即**退避+抖动要在用户层实现（在 upstream_peer 里 sleep）**，框架只提供重试回环。

### 1.9 在 Pingora 上实现我们全部需求的缺口清单

| 需求 | Pingora 支持度 | 需自研部分 |
|---|---|---|
| 动态 upstream（路径解析） | 完全支持（upstream_peer + upstream_request_filter 改写 uri） | 路径解析 + Host/SNI 设置 ~50 行 |
| 每请求重试策略（query 参数） | 无策略 DSL，但 CTX per-request，fail_to_connect/upstream_peer 可读 session | 解析 `retry.*` 参数 + 策略结构 ~100 行 |
| 状态码触发重试 | **无原生支持**（#873 OPEN，PR #872 未合并） | 需在 `upstream_response_filter` 后自缓冲响应 + 自中止 + 自重发；或 vendor PR #872 的钩子 |
| 网络错误重试 | `fail_to_connect` + `error_while_proxy`（可覆盖，对非幂等也可放行） | 策略判断逻辑 |
| 非幂等 POST 重试 | retry buffer 自动缓存 body 并重发（≤64 KiB） | >64 KiB body 需 fork 或外层自缓冲；注意 #979 的 h2 InvalidH2 无条件重试缺陷 |
| 指数退避 + jitter + Retry-After | 框架无内置（官方示例即在 upstream_peer 里 sleep） | 退避计算 ~50 行；Retry-After 需在自缓冲响应时读取 |
| SSE 流式 | 已验证（duplex + body filter 逐 chunk + flush 正常） | 无 |
| 输出开始后停止重试 | 框架保证（响应头下发后 fail） | 无 |
| SSRF 钉扎（预解析 IP） | Peer 接受 SocketAddr；DNS 是用户责任 | 自己 resolve + 传 IP ~20 行 |

---

## 2. tower / tower-http / axum / hyper

### 2.1 tower::retry::Policy 语义

来源：[docs.rs tower::retry::Policy](https://docs.rs/tower/latest/tower/retry/trait.Policy.html)（tower 0.5.3，访问 2026-09-03）：

```rust
pub trait Policy<Req, Res, E> {
    type Future: Future<Output = ()>;
    fn retry(&mut self, req: &mut Req, result: &mut Result<Res, E>) -> Option<Self::Future>;
    fn clone_request(&mut self, req: &Req) -> Option<Req>;
}
```

- **body 无 Clone 强制约束**——但 `clone_request` 必须返回 `Option<Req>`：不可克隆（流式 body）→ 返回 `None` → **该请求完全跳过重试路径**（"the retry function will not be called if the None is returned"）。要做流式 body 重试必须先缓冲（Bytes 共享指针或 Arc 包装）。
- `retry` 拿到 `&mut Result<Res, E>`——**对「按响应状态码重试」是理想位置**：可以检查 `Res`（http::Response）的 status 决定是否重试，还能 mutate 请求/结果。这是 tower 组合方案里最重要的一点。
- `Policy::retry` 的返回 Future 即等待/退避（可 sleep 任意时长）——**退避和 Retry-After 都可以在 Policy 里实现**。
- tower 0.5.3（2026-06-20 docs.rs 日期）还有 `retry::budget`、`retry::backoff`（generic backoff utilities）（[tower retry 模块](https://docs.rs/tower/latest/tower/retry/index.html)，访问 2026-09-03）。

### 2.2 tower-http 有 retry layer 吗？

**没有。** 已核实 tower-http 源码模块列表（0.7.1，2026-08-31 发布；GitHub API `tower-http/src/` 目录列表，访问 2026-09-03）：有 follow_redirect、limit、compression/decompression、trace、timeout 等，**无 retry 模块**。issue 搜索亦无 retry layer 计划（`gh search issues` tower-rs/tower-http "retry" 无相关结果）。重试需直接用 `tower::retry::Retry` + 自写 Policy。

### 2.3 axum 流式 / SSE

- `Body::from_stream<S>(stream: S) -> Body` where `S: TryStream + Send + 'static, S::Ok: Into<Bytes>, S::Error: Into<Box<dyn Error + Send + Sync>>`（[docs.rs axum Body](https://docs.rs/axum/latest/axum/body/struct.Body.html)，axum 0.8.9 / axum-core 0.5.6，访问 2026-09-03）——reqwest `bytes_stream()` 的输出可直接喂进去（`Bytes` 满足 `Into<Bytes>`）。
- axum 自带 `axum::response::sse`（`Sse`、`Event`、`KeepAlive`、`KeepAliveStream`），从 `Stream<Item = Result<Event, E>>` 构造 SSE 响应（[docs.rs axum sse](https://docs.rs/axum/latest/axum/response/sse/index.html)，访问 2026-09-03）。对我们的场景，**SSE 透传用 `Body::from_stream` 更合适**（不需要重新构造 Event，直接字节级透传，Content-Type 原样转发）；axum SSE 模块适合自己生成 SSE。
- hyper 层面：hyper 1.x 的 body 是 Stream 语义，写出即逐 chunk flush（无应用层聚合缓冲），透传 SSE 是常规用法（UNVERIFIED 的部分是「per-chunk flush」的确切 TCP 行为——hyper 1.x 会尽快写出，不额外缓冲）。

### 2.4 hyper-util client

- hyper-util 0.1.20（2026-06-22 docs.rs 日期），`client::legacy::{Client, Builder, connect}` 需要 feature `client-legacy`（[docs.rs hyper-util](https://docs.rs/hyper-util/latest/hyper_util/client/legacy/index.html)，访问 2026-09-03）。
- **无内置 retry**——文档无任何 retry/reconnect 项；重试交给上层（tower / reqwest-middleware）。legacy client 有连接池（pool 模块在 connect 下）。
- hyper/hyper-util 本身对「发送预缓冲 body」没有特殊 API——`http_body_util::Full<Bytes>` 即预缓冲 body，天然可重放。

---

## 3. reqwest 栈

### 3.1 reqwest 本体

- 最新 **reqwest 0.13.4**（2026-07-28 docs.rs 日期，[docs.rs reqwest Response](https://docs.rs/reqwest/latest/reqwest/struct.Response.html)，访问 2026-09-03）。
- **无内置 retry**。Response 提供 `error_for_status`/`error_for_status_ref`；`bytes_stream()` 在 `stream` feature 下 "Convert the response into a `Stream` of `Bytes` from the body."——**SSE 透传标准做法**。
- 依赖里出现 tower 0.5.2 / tower-http 0.6.8，但未暴露 retry API。

### 3.2 reqwest-middleware + reqwest-retry

版本与维护（GitHub API + docs.rs，访问 2026-09-03）：

- `reqwest-middleware` 0.5.2（2026-05-19 发布）、`reqwest-retry` 0.9.1（2026-02-05 发布）；仓库 TrueLayer/reqwest-middleware，未归档，2026-05-19 最后 push，46 open issues（[repo](https://github.com/TrueLayer/reqwest-middleware)、[releases](https://github.com/TrueLayer/reqwest-middleware/releases)，访问 2026-09-03）。docs.rs reqwest-middleware 0.5.2（2026-07-18），依赖 reqwest ^0.13.1。
- **body replay**（源码级核实，[middleware.rs 源码](https://docs.rs/reqwest-retry/latest/src/reqwest_retry/middleware.rs.html)）：`execute_with_retry` 在**每次尝试前 `req.try_clone()`**；不可克隆（流式 body）直接报错 `"Request object is not cloneable. Are you passing a streaming body?"`（[issue #44](https://github.com/TrueLayer/reqwest-middleware/issues/44)）。文档说明：Bytes body 是共享指针所以克隆 O(1)——**即预缓冲 body（`reqwest::Body::from(Vec<u8>)`）可廉价重试**。
- **重试分类**（源码核实，[retryable_strategy.rs 源码](https://docs.rs/reqwest-retry/latest/src/reqwest_retry/retryable_strategy.rs.html)）：`DefaultRetryableStrategy` → 5XX、408、429 为 `Transient`；其余 4XX 与大部分 reqwest 错误为 `Fatal`；2XX 返回 `None`（不重试）。**状态码集合是硬编码的默认值，但 `RetryableStrategy` 是公开 trait**（`fn handle(&self, res: &Result<Response, Error>) -> Option<Retryable>`）——可自定义按任意状态码重试（含我们要的 `400,429,500-599` per-request 语义，可在 middleware 外层按请求参数构造策略）。
- **退避**：`RetryPolicy` trait（来自 retry-policies crate）`should_retry(start_time, n_past_retries) -> RetryDecision`，`Retry { execute_after }` 由中间件 sleep；`ExponentialBackoff` builder（retry_limit/initial_backoff/max_backoff/backoff_multiplier）。jitter 默认含（UNVERIFIED：jitter 细节来自搜索结果，未在源码页直接确认——ExponentialBackoff builder 有 jitter 相关项，搜索结果称默认应用）。
- **Retry-After 不支持**：[issue #146](https://github.com/TrueLayer/reqwest-middleware/issues/146)（OPEN）：维护者确认 RetryPolicy "doesn't get to see the response itself"，无法实现 server 建议 deadline；社区替代品：[melotic/reqwest-retry-after](https://github.com/melotic/reqwest-retry-after)（2 stars，2026-07-27 有 push）与 [clechasseur/reessaie](https://github.com/clechasseur/reessaie)（1 star，2026-08-29 有 push）——都是极小项目，不建议依赖。
- **流式响应**：reqwest `bytes_stream()` → axum `Body::from_stream` 透传，SSE 无问题。

### 3.3 小结

reqwest-retry 覆盖：网络错误 + 固定状态码集（408/429/5XX）、指数退避、预缓冲 body 重放。**不覆盖**：per-request 状态码配置（需自定义 Strategy 且要绕过「strategy 只看全局 client 配置」的结构——每请求参数得自己在外层做）、Retry-After（不支持，trait 看不到响应头）、流式 body（直接报错，需先缓冲）。

---

## 4. 现有 Rust 动态/重试反向代理项目

### 4.1 sozu（Clever Cloud）

- 仓库 [sozu-proxy/sozu](https://github.com/sozu-proxy/sozu)，AGPL-3.0+（proxy）/ LGPL-3（命令库）。活跃：2.2.1（2026-08-28）、2.2.0（2026-07-16）、2.1.1（2026-07-10），2026-08-30 有 push，3.7k stars（GitHub API，访问 2026-09-03）。
- 定位：**静态配置的边缘反向代理/LB**（热重配置、零停机自升级、TLS termination、HTTP/1.1+H2、TCP/UDP、PROXY protocol、Kawa 零拷贝解析）（[README](https://github.com/sozu-proxy/sozu/blob/main/README.md)，访问 2026-09-03）。
- retry 相关（源码核实）：`lib/src/retry.rs` 定义 `RetryPolicy` trait（ExponentialBackoffPolicy：max_tries/current_tries/fail/succeed/can_try/is_down）——但这是**backend 级熔断/健康检查退避**，用于 `Backend::can_open()`（`backends.rs`：`retry_policy.can_try()` 决定后端是否可拨）。HTTP 层的「请求重试」：`kawa_h1/mod.rs` 有 `connection_attempts: u8` 与 `check_circuit_breaker()`（`CONN_RETRIES` 上限，超限 Answer503 "Max connection attempt reached"）——即**连接建立失败时在多个 backend 间重试，upstream 返回错误状态码不重试**。代码注释里甚至有 "some backend errors are actually retryable / TODO: maybe retry or return a different default answer"——响应级重试是未完成项（`lib/src/protocol/kawa_h1/mod.rs`，访问 2026-09-03）。`Retry-After` 只是它自己 429 限流响应的响应头，不是重试输入。
- **架构不匹配**：单线程 mio 事件循环 + master/worker 进程模型 + protobuf IPC 命令面——为一个轻量无状态单二进制工具引入这套架构过重；AGPL 许可证也需注意。
- **结论：不适合复用。动态 upstream 不是它的模型（cluster/backend 静态注册，运行时命令改配置）；请求级重试缺失。**

### 4.2 基于 Pingora 的现成代理

（GitHub API 搜索 "pingora"，按 stars 排序，访问 2026-09-03）

- [vicanso/pingap](https://github.com/vicanso/pingap)（1.4k stars，2026-09-01 有 push）：nginx 式反向代理，YAML/TOML 配置驱动。**有 per-location retry 预算**：`upstream_peer` 应用 "the location's retry budget (`max_retries`, `max_retry_window`)"（[pingap-proxy/README](https://github.com/vicanso/pingap/blob/main/pingap-proxy/README.md)，访问 2026-09-03）——但仍是**连接级重试预算**，非状态码驱动，也非 per-request query 参数驱动。
- [sadoyan/aralez](https://github.com/sadoyan/aralez)（773 stars）、[DDULDDUCK/pingora-proxy-manager](https://github.com/DDULDDUCK/pingora-proxy-manager)（398 stars）：均为配置驱动反代，无 per-request 动态 upstream / 状态码重试。
- [aula-id/mini-gateway-rs](https://github.com/aula-id/mini-gateway-rs)（102 stars，2026-03-09 最后 push）：基于 pingora 的动态可配置路由网关（GUI 控制面 + 插件脚本）——控制面方向，不是路径表达动态上游；近半年不活跃。
- [caibirdme/penguin](https://github.com/caibirdme/penguin)（22 stars，2025-10-30 push）：Pingora 上的 API 网关（Kong/APISIX 类比），YAML 路由 + 插件；源码搜索无 retry 实现（`gh search code` repo:caibirdme/penguin retry 无命中）。
- **结论：没有任何现成 Pingora 项目提供「路径动态 upstream + query 参数重试策略 + 状态码重试」。它们全部是静态配置模型。**

### 4.3 Rust AI 网关类项目

- [tensorzero/tensorzero](https://github.com/tensorzero/tensorzero)：11.7k stars，但**已归档**（archived=true，2026-06-11 最后 push）（GitHub API，访问 2026-09-03）。
- [LiteLLM-Labs/litellm-rust](https://github.com/LiteLLM-Labs/litellm-rust)（207 stars，2026-06-25 push）：axum 0.8 + reqwest 0.12 + sqlx 的 LiteLLM 兼容网关；面向 coding agents（Claude Code/Codex），SSE 透传（reqwest stream feature + futures-util）。自述 "proof of concept repo"。源码搜索 retry 仅命中 docs/debugging.md（`gh search code`）——无系统性重试实现。**架构参考价值高（axum+reqwest 组合做 LLM 网关的最小样板），但不可直接复用。**
- [Noveum/ai-gateway](https://github.com/Noveum/ai-gateway)（96 stars，2026-08-24 push）：OpenAI 兼容多 provider 网关；README 未提 retry/fallback；provider 按 `x-provider` header 选，**非路径动态**；架构 axum 0.7 + hyper 0.14 + reqwest 0.12（Cargo.toml 核实）。非我们的模型。
- 其他小项目（majiayu000/litellm-rs 110 stars、nyroway/nyro 189 stars、topjohnwu/agy-gyro 15 stars——后者 "Local retry proxy to stabilize the Antigravity CLI… against transient Google Gemini API errors" 与我们场景最接近但规模极小且刚起步）。
- **结论：AI 网关们都是「provider 路由 + 协议翻译」模型，没有「任意路径动态 upstream + per-request 重试策略」的。**

---

## 5. 各项目「复用契合度」评估表

| 项目 | 动态 upstream(路径) | per-request 重试策略 | 状态码重试 | 非幂等重试+body 重放 | 退避/Retry-After | SSE 流式 | 维护状态 | 复用结论 |
|---|---|---|---|---|---|---|---|---|
| **Pingora 0.8.1** | upstream_peer 完美支持 | CTX per-request，需自研解析 | **无**（#873 OPEN） | retry buffer ≤64 KiB 自动重放；h2 InvalidH2 有无条件重试缺陷(#979) | 无内置，官方示例在 upstream_peer 里 sleep | 已验证（#841 维护者实测+单测） | 活跃（CF 生产） | **框架级最优底盘，但状态码重试与 >64KiB body 重试要自研** |
| **axum 0.8.9 + hyper + tower** | handler 里自由解析路径 | 自写 Policy（retry 拿 &mut Result 可看状态码） | Policy 里完全自定义 | 需自缓冲 body（Bytes clone O(1)） | Policy Future 里 sleep，完全自定义（含读 Retry-After，因能拿到响应） | Body::from_stream + bytes_stream 直通 | 全部活跃 | **组合自由度最高，但连接池/HTTP 客户端要自己组** |
| **reqwest 0.13.4 + reqwest-middleware/retry 0.9** | 自由（每请求构造 URL） | Strategy trait 可换但按 client 维度；per-request 需外层自定义 | 默认 408/429/5XX；自定义 Strategy 可任意 | 预缓冲 Bytes 克隆 O(1)；流式 body 直接报错 | ExponentialBackoff；**Retry-After 不支持**(#146) | bytes_stream 透传 OK | 活跃但小团队 | **客户端级重试现成，但 Retry-After 缺失 + per-request 策略别扭** |
| **sozu 2.2.1** | 否（cluster 静态注册） | 否 | 否（连接级 + backend 熔断） | — | backend 级 ExponentialBackoff | 支持 | 活跃 | **不匹配：架构重 + AGPL** |
| **pingap / penguin / mini-gateway-rs 等** | 否（配置驱动） | 部分（pingap 有 location 级 retry budget） | 否 | — | 有限 | 支持 | 活跃/一般 | **可读源码学模式，不可直接复用** |

---

## 6. Rust 路线结论

**判定：需求集合可完整实现，(a) Pingora 与 (b) axum+hyper+reqwest 组合都可行；不存在满足全部需求的现成项目。最小自研量比较：**

### 路线 A：Pingora 框架
- 拿到的东西：生产级连接池/duplex 流式/优雅重启/多 runtime、SSE 已验证、请求体 retry buffer 自动重放（≤64 KiB）、输出开始后天然不可重试。
- 必须自研：
  1. **状态码驱动重试**（最大缺口）：在 `upstream_response_filter` 阶段自缓冲响应（含 Retry-After 解析），不满足策略则丢弃、回环重试（自己维护重试计数/backoff）。或者 vendor [PR #872](https://github.com/cloudflare/pingora/pull/872) 的 `upstream_response_decision` 钩子（33 行 trait 方法 + 15 行接线，未合并）。
  2. >64 KiB 请求体的重试缓冲（fork 加 spool，参考 lindenbaum 分支）或直接限定「超大 body 不重试」。
  3. 退避+jitter+Retry-After 计算（官方示例模式：upstream_peer 里 sleep）。
  4. 路径解析 + DNS 预解析 + SSRF 钉扎。
- 风险：#979（h2 upstream InvalidH2 无条件重试非幂等请求）——若 upstream 是 h2，需要规避/打补丁；#872 未合并意味着升级 pingora 版本时自研钩子要 rebase。
- 预估自研量：约 600–1000 行 Rust（含策略解析与状态码重试循环）。

### 路线 B：axum + hyper(-util) + reqwest(-middleware)
- 拿到的东西：全控制权。`tower::retry::Policy::retry` 拿到 `&mut Result<Res, E>` 可**直接读响应状态码与 Retry-After 头**——状态码重试在这个模型里是一等公民；`Body::from_stream` + `bytes_stream` SSE 直通；reqwest `try_clone` 对预缓冲 Bytes 是 O(1)。
- 必须自研：
  1. 整个重试编排（用 tower Policy 或直接在 handler 里写循环——后者更简单直接：handler 收请求 → 读 query 参数 → buffer body → loop { 发请求（预解析 IP + SNI 钉扎）→ 检查状态码/错误 → 退避 → 重试 } → 命中即 `Body::from_stream(resp.bytes_stream())` 返回）。
  2. 连接池调优、超时矩阵、HTTP/2 支持（reqwest 自带）。
  3. 优雅关闭/信号处理（axum/tokio 标配）。
- 风险：自己拼装的客户端栈在高并发下的连接复用细节（reqwest 内部是 hyper-util pool，成熟）；reqwest-retry 的 Retry-After 缺失对路线 B 无影响（自己写循环不看它）。
- 预估自研量：约 400–700 行 Rust（handler + 策略 + 重试循环 + 透传）。**代码量更少**，因为重试循环是命令式的、无框架回调模型的间接层。

### 推荐
**对 reproxy 的需求集（单二进制、无状态、动态 upstream、per-request 状态码+网络错误重试、非幂等重试、SSE），路线 B（axum+reqwest 自写重试循环）是更小的自研面**；Pingora 的核心价值（高并发连接复用、百万 QPS 级性能）对「轻量 retry 代理」是超额供给，而它的核心缺口（状态码重试）恰好是我们最中心的需求。若未来要演进成通用网关/多 upstream LB，再考虑迁移 Pingora（届时 PR #872 若合并，缺口消失）。

---

## 引用清单（均于 2026-09-03 访问）

- Pingora: [docs.rs ProxyHttp](https://docs.rs/pingora/latest/pingora/proxy/trait.ProxyHttp.html) · [docs.rs Session](https://docs.rs/pingora-proxy/latest/pingora_proxy/struct.Session.html) · [docs.rs HttpPeer](https://docs.rs/pingora/latest/pingora/upstreams/peer/struct.HttpPeer.html) · [docs.rs Peer trait](https://docs.rs/pingora/latest/pingora/upstreams/peer/trait.Peer.html) · [failover.md](https://github.com/cloudflare/pingora/blob/master/docs/user_guide/failover.md) · [phase.md](https://github.com/cloudflare/pingora/blob/master/docs/user_guide/phase.md) · [peer.md](https://github.com/cloudflare/pingora/blob/master/docs/user_guide/peer.md) · [releases](https://github.com/cloudflare/pingora/releases) · [issue #873](https://github.com/cloudflare/pingora/issues/873) · [PR #872](https://github.com/cloudflare/pingora/pull/872) · [issue #979](https://github.com/cloudflare/pingora/issues/979) · [issue #349](https://github.com/cloudflare/pingora/issues/349) · [issue #575](https://github.com/cloudflare/pingora/issues/575) · [issue #841](https://github.com/cloudflare/pingora/issues/841) · [issue #147](https://github.com/cloudflare/pingora/issues/147) · [backoff_retry 示例](https://github.com/cloudflare/pingora/blob/master/pingora-proxy/examples/backoff_retry.rs)
- tower/axum/hyper: [tower Policy](https://docs.rs/tower/latest/tower/retry/trait.Policy.html) · [tower retry 模块](https://docs.rs/tower/latest/tower/retry/index.html) · [tower-http src 目录](https://github.com/tower-rs/tower-http/tree/main/tower-http/src) · [axum Body](https://docs.rs/axum/latest/axum/body/struct.Body.html) · [axum sse](https://docs.rs/axum/latest/axum/response/sse/index.html) · [hyper-util legacy client](https://docs.rs/hyper-util/latest/hyper_util/client/legacy/index.html)
- reqwest: [docs.rs reqwest Response](https://docs.rs/reqwest/latest/reqwest/struct.Response.html) · [reqwest-middleware repo](https://github.com/TrueLayer/reqwest-middleware) · [middleware.rs 源码](https://docs.rs/reqwest-retry/latest/src/reqwest_retry/middleware.rs.html) · [retryable_strategy.rs 源码](https://docs.rs/reqwest-retry/latest/src/reqwest_retry/retryable_strategy.rs.html) · [issue #146 Retry-After](https://github.com/TrueLayer/reqwest-middleware/issues/146) · [issue #44 流式 body](https://github.com/TrueLayer/reqwest-middleware/issues/44) · [melotic/reqwest-retry-after](https://github.com/melotic/reqwest-retry-after) · [clechasseur/reessaie](https://github.com/clechasseur/reessaie)
- 项目: [sozu repo](https://github.com/sozu-proxy/sozu) · [sozu README](https://github.com/sozu-proxy/sozu/blob/main/README.md) · [pingap](https://github.com/vicanso/pingap) · [pingap-proxy README](https://github.com/vicanso/pingap/blob/main/pingap-proxy/README.md) · [penguin](https://github.com/caibirdme/penguin) · [mini-gateway-rs](https://github.com/aula-id/mini-gateway-rs) · [tensorzero（已归档）](https://github.com/tensorzero/tensorzero) · [litellm-rust](https://github.com/LiteLLM-Labs/litellm-rust) · [Noveum/ai-gateway](https://github.com/Noveum/ai-gateway)
