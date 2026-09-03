# OpenResty/nginx 路线与协议层先行设计调研（A/B 两部分）

任务：`.trellis/tasks/09-03-audit-retry-proxy-design`
调研日期：2026-09-03（所有引用均于该日期访问验证）
调研方式：官方文档 + 上游源码（nginx/envoy GitHub 主分支）+ RFC 全文 + AWS 官方文档/博客 + OWASP/GitHub 一手来源。

---

## Part A: OpenResty / nginx

### A1. nginx proxy retry 机制（`proxy_next_upstream` 系列）

以下均来自 nginx 官方文档（[ngx_http_proxy_module](https://nginx.org/en/docs/http/ngx_http_proxy_module.html)，访问日期 2026-09-03），并经 nginx 主分支源码交叉验证（见下）。

**`proxy_next_upstream` 语法与取值**：

```
proxy_next_upstream error | timeout | denied | invalid_header | http_500 | http_502 |
                     http_503 | http_504 | http_403 | http_404 | http_429 |
                     non_idempotent | off ...;
```
默认值：`error timeout`。

- `error`：与后端建立连接、发送请求或读取响应头时发生错误；
- `timeout`：上述过程中发生超时；
- `denied`：服务器拒绝连接（1.29.3，商业订阅限定）；
- `invalid_header`：后端返回空响应或无效响应；
- `http_500`/`http_502`/`http_503`/`http_504`：返回对应状态码；
- `http_403`、`http_404`：返回 403/404；
- `http_429`：返回 429（**1.11.13 起支持**）；
- `non_idempotent`：**1.9.13 起支持**。文档原文："normally, requests with a non-idempotent method (`POST`, `LOCK`, `PATCH`) are not passed to the next server if a request has been sent to an upstream server (1.9.13); enabling this option explicitly allows retrying such requests" —— 即默认情况下非幂等方法（POST/LOCK/PATCH）一旦已发送给上游就不会转发到下一台服务器，加此参数显式放行；
- `off`：禁用"传递到下一台服务器"。

关键语义（文档原文）：
1. "One should bear in mind that passing a request to the next server is only possible if **nothing has been sent to a client yet**."（只有还没向客户端发送任何字节才可能重试到下一台服务器）——与本项目"响应一旦开始输出给客户端即结束重试生命周期"的要求完全一致。
2. error/timeout/denied/invalid_header **总是**算作失败尝试，即使未在指令中列出；http_500/502/503/504/429 仅在显式列出时才算；**http_403/http_404 永远不算失败尝试**（不影响 max_fails 统计）。
3. 只支持固定关键字取值，**不支持变量**，且状态码取值只有 `http_500`、`http_502`、`http_503`、`http_504`、`http_403`、`http_404`、`http_429` 这七个，**不能写任意状态码**（如 400、5xx 区间、"500-599" 之类）。这是本设计（`retry.status=400,429,500-599`）与 nginx 原生能力最直接的差距。

**`proxy_next_upstream_tries`**（1.7.5 起）：限制"passing a request to the next server"的最大 tries 次数，`0` 为不限。源码验证（[ngx_http_upstream.c](https://github.com/nginx/nginx/blob/master/src/http/ngx_http_upstream.c)）：`u->peer.tries` 是**总尝试次数（含首次）**，每次 `peer.get` 成功选中一台服务器后 `pc->tries--`（见 [ngx_http_upstream_round_robin.c](https://github.com/nginx/nginx/blob/master/src/http/ngx_http_upstream_round_robin.c) 的 `ngx_http_upstream_free_round_robin_peer`），`ngx_http_upstream_next` 中 `u->peer.tries == 0` 时终止。**即 tries=3 意味着总共最多尝试 3 次（1 次原始 + 2 次重试）**。

**`proxy_next_upstream_timeout`**（1.7.5 起）：限制重试可用的总时长（从 `peer.start_time` 起算），`0` 为不限。

**对我们设计的意义**：nginx 的失败判定是全局 location 级静态配置，不能按请求定制；状态码集合硬编码为 7 个；"总时长预算"（next_upstream_timeout）而非"每次尝试超时"（无 per-try timeout）。按 query 参数定制重试策略在原生 nginx 里做不到。

### A2. CRITICAL：动态 upstream（变量 proxy_pass）能否 retry —— 源码级结论

结论：**原生 nginx 用"变量 proxy_pass 直连主机名"时，重试只在域名解析出多个 IP 的情况下有效（跨 IP 轮询），单 IP 时一次失败即终结；要获得"同一目标多次重试"，必须让 peer.tries > 1 或使用 balancer_by_lua。**

文档层面（[ngx_http_proxy_module#proxy_pass](https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_pass)，访问日期 2026-09-03）：
> "Parameter value can contain variables. In this case, if an address is specified as a domain name, the name is searched among the described server groups, and, if not found, is determined using a resolver."
> "If a domain name resolves to several addresses, all of them will be used in a round-robin fashion."

即：变量值先按"已定义的 upstream 组名"匹配（匹配到则完全等价于静态 upstream 组，包含组的所有重试语义）；匹配不到则用 `resolver` 解析域名，解析出多个地址时按 round-robin 依次使用。

源码验证（nginx master，访问日期 2026-09-03）：
1. `ngx_http_upstream.c` 中 `u->resolved != NULL` 分支：变量目标先在 `umcf->upstreams` 里按 host+port 匹配 upstream 组（`goto found`，走组的 `peer.init`，即组内重试语义）；未匹配到则：
   - 已有 `sockaddr`（变量本身就是 IP）→ `ngx_http_upstream_create_round_robin_peer()` 直接建**单 peer** 组后 connect；
   - 域名 → 异步 `ngx_resolve_name` → resolve handler（`ngx_http_upstream_resolve_handler`，约 1240 行起）把 **naddrs 个地址** `create_round_robin_peer` 建组。
2. 关键代码（[ngx_http_upstream_round_robin.c:612](https://github.com/nginx/nginx/blob/master/src/http/ngx_http_upstream_round_robin.c)）：
   ```c
   peers->single = (ur->naddrs == 1);
   peers->number = ur->naddrs;
   peers->tries = ur->naddrs;
   ```
   而 `ngx_http_upstream_free_round_robin_peer` 里：
   ```c
   if (rrp->peers->single) { ... pc->tries = 0; return; }
   ```
   **单地址（多数动态目标场景）时 `pc->tries` 直接清零 → `ngx_http_upstream_next` 里 `u->peer.tries == 0` → 立即 finalize，无重试**。
3. `next_upstream_tries` 对动态 peer 同样生效（`if (u->conf->next_upstream_tries && u->peer.tries > ...) { u->peer.tries = ...; }`），但只能把 tries 往下调，不能把单 peer 的 1 变成 N。

**对我们的设计的意义**：如果选 nginx 原生（不带 Lua）实现"路径中携带动态 upstream + 重试到同一目标"，**不可行**——除非：
- (a) 域名恰好解析到多个 IP（retry 变成"跨 IP"而非"同目标重试"，语义不符）；或
- (b) 用 `balancer_by_lua`（见 A3），由 Lua 在每次重试时重新调用 `set_current_peer`，通过 `set_more_tries` 把 `peer.tries` 抬上去。

注：网上流传"变量 proxy_pass 完全不能 retry"的说法不准确——解析出多 IP 时确实会跨 IP retry；准确表述是"单地址动态目标无法重试"。这一点来自源码验证而非二手资料。

### A3. OpenResty `balancer_by_lua` + `ngx.balancer`

来源：[lua-nginx-module README（balancer_by_lua_block）](https://github.com/openresty/lua-nginx-module#balancer_by_lua_block)、[lua-resty-core ngx.balancer 文档](https://github.com/openresty/lua-resty-core/blob/master/lib/ngx/balancer.md)、以及源码 [ngx_http_lua_balancer.c](https://github.com/openresty/lua-nginx-module/blob/master/src/ngx_http_lua_balancer.c)（均访问于 2026-09-03）。

- `balancer_by_lua_block` 运行于 `upstream {}` 块内，作为该 upstream 的动态 balancer；README 原文："The Lua load balancer can totally ignore the list of servers defined in the `upstream {}` block and select peer from a completely dynamic server list (**even changing per request**) via the ngx.balancer module"。
- README 原文："The Lua code handler registered by this directive **might get called more than once in a single downstream request** when the Nginx upstream mechanism retries the request on conditions specified by directives like the proxy_next_upstream directive." —— 每次重试都会重新进入 balancer 代码，可用 `ngx.balancer.get_last_failure()` 获取上次失败原因（返回 `nil` 表示第一次尝试；`"next"`= 后端返回了坏状态码（连接可复用）；`"failed"`= 通信致命错误（超时、reset，连接必须废弃））。
- `set_current_peer(host, port, host?)`：设置本次尝试的地址；**host 必须是 IP**（文档："Domain names in `host` do not make sense."），域名解析需在更早的阶段（如 `precontent_by_lua`/`access_by_lua`）用 lua-resty-dns 做好并通过 `ngx.ctx` 传入——因为 balancer 上下文**不允许 yield**（cosocket、light thread 被禁用）。第三个可选参数设置 TLS SNI（`proxy_ssl_name` 的替代）。
- `set_more_tries(count)`：**每次调用设置"当前尝试失败后再追加的尝试次数"（不含当前这次）**。源码验证（`ngx_http_lua_ffi_balancer_set_more_tries`）：
  ```c
  max_tries = r->upstream->conf->next_upstream_tries;
  total = bp->total_tries + r->upstream->peer.tries - 1;
  if (max_tries && total + count > max_tries) { count = max_tries - total; *err = "reduced tries due to limit"; }
  bp->more_tries = count;
  ```
  随后在 get peer 成功时 `r->upstream->peer.tries += bp->more_tries;`。要点：
  - **必须在 `balancer_by_lua*` 上下文调用**（其他上下文报 "API disabled in the current context"）；
  - 受 `proxy_next_upstream_tries` **硬上限**约束（next_upstream_tries=0 即不限）；超限会被削减并返回警告 `"reduced tries due to limit"`（第一返回值仍为 true）；
  - 因为每次重试都重新执行 balancer 代码，可以在**每次重试时重新计算目标地址**（例如重试换端口、换解析出的另一个 IP）；
  - **非幂等请求**：重试仍走 nginx 的 `proxy_next_upstream` 判定——要重试 POST，仍需 `proxy_next_upstream ... non_idempotent`（源码 `ngx_http_upstream_next`：`if (u->request_sent && (r->method & (NGX_HTTP_POST|NGX_HTTP_LOCK|NGX_HTTP_PATCH))) { ft_type |= NGX_HTTP_UPSTREAM_FT_NON_IDEMPOTENT; }`）。ngx.balancer 文档本身不讨论非幂等请求重放。
- `recreate_request()`（v0.1.20）：重试前重建上游请求缓冲（例如重试前改 header）——注意内存池只增不减，"Do not call this function too often or memory leaks may be noticeable."
- `enable_keepalive(idle_timeout?, max_requests?)`（v0.1.18，仅 HTTP 子系统；默认 60s/100；不能与 upstream `keepalive` 指令同时使用）。

**对我们设计的意义**：`balancer_by_lua` + `set_more_tries` + 每次重试重设 `set_current_peer` 完美解决了 A2 的"动态单地址不能重试"问题，且把"重试多少次"的决定权交给 Lua（可在每次尝试时读取 query 参数动态决定剩余次数）。但它只解决"往哪连、重试几次"，**不解决"什么状态码触发重试、退避多久"**——触发条件仍是静态的 `proxy_next_upstream`（七个固定状态码 + error/timeout），且 nginx 原生**没有 backoff/jitter**（重试是立即发起的），更没有 honor Retry-After。这部分必须由 balancer Lua 里用 `ngx.sleep()` 自己实现（balancer 上下文不允许 yield，`ngx.sleep` 在 balancer 里是否可用存疑——**UNVERIFIED**，需要在 content/其他阶段或用 Lua 全自研循环时处理；详见 A4 的替代路线）。

### A4. 请求体处理（重放 body 的机制）

- `client_body_buffer_size`（[ngx_http_core_module](https://nginx.org/en/docs/http/ngx_http_core_module.html#client_body_buffer_size)）：默认"两个内存页"——x86/32 位/x86-64 为 8K，其他 64 位平台通常 16K；"In case the request body is larger than the buffer, **the whole body or only its part is written to a temporary file**"（超出部分落盘到 `client_body_temp_path`）。`client_body_in_single_buffer on` 配合 `$request_body` 减少拷贝。
- `proxy_request_buffering on`（默认，[文档](https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_request_buffering)）："the entire request body is read from the client before sending the request to the proxied server"——**这就是 nginx 能在多次 upstream 重试间重放请求体的机制**（nginx 的 upstream 请求体从内存 buffer 或临时文件重新发起，无需应用层缓存）。文档同时写明："When buffering is disabled, the request body is sent to the proxied server immediately as it is received. In this case, **the request cannot be passed to the next server if nginx already started sending the request body**."
- 另注意：chunked 请求体在 `proxy_http_version` 非 1.1/2 时也会被整体缓冲（与 buffering 设置无关）；`proxy_http_version` 默认自 1.29.7 起为 1.1（之前默认 1.0）。
- 结论：**"先整体接收请求体（必要时落盘）再转发"是重放式代理的标准形态**，nginx 用"内存 buffer + 落盘"两级实现。对 SSE 流式**响应**无影响（`proxy_buffering`/响应侧独立）。

### A5. lua-resty-http 与 SSE 流式

来源：[ledgetech/lua-resty-http README](https://github.com/ledgetech/lua-resty-http)（访问日期 2026-09-03）。

- 请求体：`httpc:request(uri, {body = ...})`，body 可为字符串/字符串表/**迭代器函数**（流式发送，需自备 Content-Length 或自行实现 chunked 编码）；`httpc:get_client_body_reader(chunksize)` 返回下游请求体的流式迭代器，"This iterator can also be used as the value for the body field in request params"——即转发代理可以用它把客户端 body 流式透传给上游。
- 响应体：`res.body_reader` 是流式读取迭代器，"The body_reader iterator can be used to stream the response body in chunk sizes of your choosing"；对 chunked 响应 "the iterator will return the chunks as they arrive"（**SSE 适用**——按块到达即转出）；`res:read_body()` 则整体读入字符串。reader 返回的 chunk 尺寸是"最大值"（实际可能更小）。超时用 `set_timeouts(connect, send, read)` 分别设置（read 作用于"iterators returned from receiveuntil"，即流式读取中每次等待的超时）。
- 连接复用：响应体必须读完（`read_body` 或 `body_reader` 直到 nil + `read_trailers`）才能 `set_keepalive` 归还连接池。
- **限制**：流式请求 body 的迭代器是一次性的——"a streamed request body cannot be replayed since the iterator is exhausted after one pass"（README 未直接讨论重试场景；此为机制推论）。要在 Lua 里做"带重试的流式请求体"，必须先把 body 收进内存/文件再重放。
- **是否只能全自研 Lua 重试循环**：是。lua-resty-http 只是 HTTP 客户端库，没有重试策略、backoff、状态码判定；要获得"按状态码 + 按请求（query 参数）定制"的重试，唯一途径是在 `content_by_lua`（或 access 阶段预处理 + 代理）里自己写"发请求 → 检查状态码 → 决定重试（`ngx.sleep` 退避）→ 流式回写响应"的循环。这正是本设计要复用的"请求重放/流式基础设施"在 Lua 世界的等价物，但状态码判定、退避、Retry-After 解析全部要手写。

### A6. OpenResty 路线结论（verdict）

- **能否实现全部需求**：可以，但有两条子路线，均需相当多自定义 Lua：
  1. **nginx 原生 upstream 重试 + balancer_by_lua**：`balancer_by_lua` 解决动态地址/每试重定目标/次数（`set_more_tries`）；请求体重放由 `proxy_request_buffering on` + `client_body_buffer_size`/临时文件免费获得；SSE 由 nginx 核心流式转发（响应侧 `proxy_buffering off`）质量最好。但**状态码触发集只能从 7 个固定值里选**、无 per-request 定制、**无 backoff/jitter/Retry-After**（重试立即发起），要补齐只能折衷（例如在 balancer 里 `ngx.sleep`？——balancer 上下文禁 yield，**UNVERIFIED**）。对 SSE 且重试必须在响应头阶段判定（nginx 正好满足"未向客户端输出才能重试"）。
  2. **content_by_lua + lua-resty-http 全自研**：请求/响应均用 body_reader 流式；重试策略（状态码、次数、退避、Retry-After、非幂等 POST 重放）完全可控；代价是所有 HTTP 细节（chunked、连接复用、超时、TLS/SNI、hop-by-hop 头）自己负责，且流式请求体必须先缓冲再重放（与 nginx 原生行为一致）。
- **部署形态**：OpenResty 是"nginx + LuaJIT"的发行版，Windows 支持二等公民（OpenResty 官方不为 Windows 提供构建；**UNVERIFIED**，但社区共识如此）。不是单二进制，是一个目录树（nginx 二进制 + lua 库 + 配置）；状态可做到无状态。内存占用：nginx worker 常驻约几十 MB 级（随连接数增长；具体数值**UNVERIFIED**）。
- **对我们设计的意义**：如果团队已有 OpenResty 运维经验，路线 1（balancer_by_lua + 原生重试）能最大化复用 nginx 的请求缓冲/重放与流式内核，但重试语义（状态码集合、退避、Retry-After）先天残缺；路线 2 则等于用 Lua 重写我们要设计的东西。两条路都偏离"单二进制、流式优先"的目标形态。

---

## Part B: 协议先行设计与标准

### B1. Envoy retry policy（最成熟的先行设计）

来源：[router filter 配置文档](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/router_filter)（x-envoy-retry-on / x-envoy-max-retries / per-try-timeout 各节）、[route_components.proto](https://github.com/envoyproxy/envoy/blob/main/api/envoy/config/route/v3/route_components.proto)（RetryPolicy 消息，源码验证）、[circuit_breaker.proto](https://www.envoyproxy.io/docs/envoy/latest/api-v3/config/cluster/v3/circuit_breaker.proto)、[retry_state_impl.cc](https://github.com/envoyproxy/envoy/blob/main/source/common/router/retry_state_impl.cc)、[backoff_strategy.cc](https://github.com/envoyproxy/envoy/blob/main/source/common/common/backoff_strategy.cc)、[http_routing.rst](https://github.com/envoyproxy/envoy/blob/main/docs/root/intro/arch_overview/http/http_routing.rst)。均访问于 2026-09-03。

**`retry_on` 取值与含义**（router filter 文档，措辞为原文摘译）：
- `5xx`：上游返回任何 5xx，或完全无响应（disconnect/reset/read timeout）。**包含 connect-failure 与 refused-stream**。注意：整体请求超时（x-envoy-upstream-rq-timeout-ms 触发的 504）不重试。
- `gateway-error`：类似 5xx 但只针对 502/503/504 或完全无响应。
- `reset`：上游完全无响应（disconnect/reset/read timeout）。
- `reset-before-request`：同 reset，但仅当请求尚未发往上游（headers 还没发出）时才重试。
- `connect-failure`：连接失败（connect timeout 等，TCP 层）。包含在 5xx 中。
- `refused-stream`：上游以 REFUSED_STREAM 复位流（"This reset type indicates that a request is safe to retry"）。包含在 5xx 中。
- `retriable-status-codes`：响应码命中 `retriable_status_codes` 列表（或 `x-envoy-retriable-status-codes` 头）时重试。
- `retriable-headers`：响应头命中 `retriable_headers` 匹配器时重试。
- `retriable-4xx`：目前仅 409。
- `envoy-ratelimited`：响应带 `x-envoy-ratelimited` 头（由 rate limit filter 设置）。
- `http3-post-connect-failure`：HTTP/3 已连接后失败。
- 可用逗号组合多个条件。

**`num_retries` 计数语义**：proto 注释原文："Specifies the **allowed number of retries**. This parameter is optional and **defaults to 1**."——即 `num_retries` 是**额外重试次数**，总尝试次数 = num_retries + 1。源码验证（retry_state_impl.cc:75）：`retries_remaining_(route_policy.numRetries())`，每次决定重试时 `retries_remaining_--`，为 0 返回 `NoRetryLimitExceeded`。请求头 `x-envoy-max-retries` 可覆盖（优先级高于 route 配置）。

**`retriable_status_codes`**：`repeated uint32`，仅当 retry_on 含 `retriable-status-codes` 时生效；可被 `x-envoy-retriable-status-codes` 头覆盖。

**`retry_back_off`（base_interval / max_interval）与 jitter 公式**：
- proto：base_interval 必填（>0，<1ms 向上取整）；max_interval 可选，"must be greater than or equal to the `base_interval` if set. **The default is 10 times the `base_interval`**"。
- 文档（router filter）：默认 base interval **25ms**（可用运行时参数 `upstream.base_retry_backoff_ms` 改）；"Envoy uses a **fully jittered exponential back-off** algorithm"；"Given a base interval B and retry number N, the back-off for the retry is in the range **[0, (2^N−1)B)**"；示例：第一次重试 0–24ms、第二次 0–74ms、第三次 0–174ms；上限默认 10×base（250ms）。
- 源码验证（backoff_strategy.cc）：
  ```c
  uint64_t JitteredExponentialBackOffStrategy::nextBackOffMs() {
    const uint64_t backoff = next_interval_;                    // B, 2B, 4B...
    next_interval_ = (next_interval_ < doubling_limit_) ? (next_interval_ * 2u) : max_interval_;
    return (random_.random() % backoff);                       // [0, backoff)
  }
  ```
  即第 N 次重试的退避 = uniform[0, 2^N·B)，且区间本身封顶于 max_interval。**注意文档区间 [0,(2^N−1)B) 与源码 [0,2^N·B) 有细微出入**（文档以"retry number N"从 1 起算时两者一致；以 0 起算时文档少一档）——按源码为准：第 1 次重试为 [0, 2B)。
- `rate_limited_retry_back_off`：服务器指示的退避。reset_headers（如 `Retry-After`（SECONDS 格式）或 `X-RateLimit-Reset`（UNIX_TIMESTAMP 格式））按顺序匹配，取到的值作为 back-off；超过 `max_interval`（默认 300s）则丢弃该值改试下一个 header；无匹配则回落指数退避。加抖动公式："`random(interval, interval * 1.5)`"（源码为 `random % (min_interval>>1) + min_interval`，即 [interval, 1.5·interval)）。
- **`honor_retry_after_header`：该字段在当前 Envoy API 中不存在**。经 GitHub code search + 各版本 tag（v1.9–v1.36）route_components.proto 检索均为 0 命中；Envoy 处理 `Retry-After` 的官方途径就是 `rate_limited_retry_back_off.reset_headers`（配合 `retry_on` 匹配 429 等）。网传"honor_retry_after_header 默认 false"的说法无法在任何 Envoy 官方来源验证，应视为**讹传（UNVERIFIED / 不存在）**。（criblio 的 `response_honor_retry_after_header` 是 Cribl 自家产品字段，非 Envoy。）

**`per_try_timeout`**：非零的"每次尝试（含首次）"上游超时；proto 原文注明若未设置则用全局 route timeout，且因此 5xx 策略下整体超时不重试（预算耗尽）。文档："This timeout **only applies before any part of the response is sent to the downstream**"——即 per-try 超时一旦响应已开始下发就失效（与我们的"响应开始即不可重试"同构）。另有 `per_try_idle_timeout`（v13 字段）：在"整个请求已被 router 收到 + 已获得连接池连接"之后才开始计，且响应流回下游期间继续计时。

**Retry budgets / 限流**：
- 集群熔断器的 `max_retries`：并行重试上限，默认 3。
- `retry_budget`（Thresholds.RetryBudget）：`budget_percent` 默认 **20%**（"limit on concurrent retries as a percentage of the sum of active requests and active pending requests"，例：100 active + 25% = 允许 25 个并发重试）；`min_retry_concurrency` 默认 **3**（并发重试下限）；`budget_interval` 默认 0（只看当前 active+pending）；"the retry budget will **override any configured retry circuit breaker**"（设置了 budget 就取代 max_retries）。溢出时计数器 `upstream_rq_retry_overflow`。
- 架构文档（http_routing.rst）建议：配置 retry budgets（首选）或调低 max_retries 以避免 retry storms。

**请求体缓冲与重试的关系（源码验证，router.cc）**：
```c
const bool retry_enabled = retry_state_ && retry_state_->enabled();
...
bool buffering = (retry_enabled || redirect_enabled) && (!request_buffer_overflowed_);
```
即：**只要启用了 retry（或 internal redirect），Envoy router 默认就会缓冲请求体**（`callbacks_->addDecodedData(data, true)`）；当缓冲超过限制（route 的 `request_body_buffer_limit` / `per_request_buffer_limit_bytes`，且受连接管理器 buffer limit 约束）时**放弃重试而不是失败**："retry or redirect buffer overflow: skipping buffering"，`retry_state_.reset()`，正文继续流式转发。若当时没有任何 upstream 在途请求则直接回 507 Insufficient Storage。同样：`maybeRetryHeaders`/`maybeRetryReset` 均以 `downstream_response_started_` 为前置条件——**响应开始下发后不再重试**。
- **对我们设计的意义**：Envoy 的成熟做法就是"启用重试 ⇒ 缓冲请求体（有限额，超限则降级为不重试）+ 响应未开始才可重试 + 集群级 retry budget 防风暴"。这是本设计应当对齐的三条黄金法则。

### B2. RFC 9110：幂等方法与 Retry-After

来源：[RFC 9110](https://www.rfc-editor.org/rfc/rfc9110.html) 全文（rfc-editor.org txt 版逐字核对，访问日期 2026-09-03）。

**§9.2.1 Safe Methods**：GET、HEAD、OPTIONS、TRACE 定义为 safe（"essentially read-only"）。

**§9.2.2 Idempotent Methods**：
> "A request method is considered 'idempotent' if the intended effect on the server of multiple identical requests with that method is the same as the effect for a single such request. Of the request methods defined by this specification, **PUT, DELETE, and safe request methods** are idempotent."

即幂等方法 = **PUT、DELETE + (GET/HEAD/OPTIONS/TRACE)**。关键句：
> "Idempotent methods are distinguished because the request can be **repeated automatically if a communication failure occurs before the client is able to read the server's response**."
> "A client SHOULD NOT automatically retry a request with a non-idempotent method unless it has some means to know that the request semantics are actually idempotent, regardless of the method, or some means to detect that the original request was never applied."
> "**A proxy MUST NOT automatically retry non-idempotent requests.**"

（另：§2.4 指出"Some requests can be automatically retried by a client in the event of an underlying connection failure, as described in Section 9.2.2"。）

**对我们设计的意义（关键）**：RFC 明文**禁止代理自动重试非幂等请求**。本设计要显式支持 POST 重试，属于"运营方知情选择"（第 9.2.2 节也承认客户端可以凭"知道该资源安全"或"能检测原请求未被执行"来自动重试 POST）。设计上应当：默认只重试幂等方法 + 显式 opt-in（query 参数）才重试 POST，并把这个 opt-in 写进文档；这与 nginx 的 `non_idempotent` 旗标、Envoy 的做法（见 B1，Envoy 默认会对流式请求体做缓冲后重试，不区分方法——其立场是应用层自己决定 retry_on）形成对照。

**§10.2.3 Retry-After**：
> "Servers send the 'Retry-After' header field to indicate how long the user agent ought to wait before making a follow-up request. When sent with a **503 (Service Unavailable)** response, Retry-After indicates how long the service is expected to be unavailable to the client. When sent with any **3xx (Redirection)** response, Retry-After indicates the minimum time that the user agent is asked to wait before issuing the redirected request."
> "The Retry-After field value can be either an **HTTP-date or a number of seconds** to delay after receiving the response."
> ```
> Retry-After = HTTP-date / delay-seconds
> delay-seconds  = 1*DIGIT
> ```
> 示例：`Retry-After: Fri, 31 Dec 1999 23:59:59 GMT` / `Retry-After: 120`。

- 发送者：RFC 措辞是 "Servers send"——从定义上这是**源服务器**指示 user agent 的头。RFC 9110 并未明文授权或禁止中间层（proxy）读取并转用它；作为代理消费上游 429/503 的 Retry-After 来安排自己的重试，是对该语义的合理延伸（Envoy rate_limited_retry_back_off 即此做法），但严格说规范只定义了 server→user_agent 方向。**解析实现必须同时支持 delay-seconds（非负整数秒）与 HTTP-date 两种格式**（HTTP-date 需转换为相对秒）。
- 注意：RFC 9110 §10.2.3 未限定 Retry-After 只能伴随哪些状态码出现（举例用 503 与 3xx；实践中 429 也常见，且 429 定义在 §15.5.4 并提到 Retry-After——见该节）。

### B3. gRPC A6 客户端重试设计

来源：[A6 client retries 设计文档](https://github.com/grpc/proposal/blob/master/A6-client-retries.md)（grpc/proposal master，访问日期 2026-09-03）。

- `maxAttempts` 原文："specifies the **maximum number of RPC attempts, including the original request**"（含首次请求的总次数）。校验规则：必须 >1；**大于 5 的值按 5 处理**（客户端侧上限，可通过 channel 参数调整，防御 service config 经 DNS 下发的攻击面）。
- 指数退避 + 抖动："Jitter of **plus or minus 0.2** is applied to the backoff delay"；"The initial retry attempt will occur after `initialBackoff * random(0.8, 1.2)`. After that, the n-th attempt will occur after `min(initialBackoff*backoffMultiplier**(n-1), maxBackoff) * random(0.8, 1.2)`"——±20% 抖动（均匀乘性抖动）。
- **Retry throttling（token bucket）**：按 server name 维护 `token_count`（0..maxTokens，初始 maxTokens，例 maxTokens=10、tokenRatio=0.1）：
  - 每个失败的 RPC `token_count -= 1`；
  - 每个成功的 RPC `token_count += tokenRatio`；
  - 阈值 = `maxTokens / 2`；`token_count <= 阈值` 时**停止重试**（重试请求被取消、错误直接返回应用；首个外发 RPC 永远会发出）；
  - 只有"可重试/非致命状态码失败或收到 pushback 不重试指示"的 RPC 才计入失败（避免把 INVALID_ARGUMENT 这类请求错误算成服务故障）。
- 服务器 pushback：可在响应 metadata 里指示"n 毫秒后重试"或"不再重试"；客户端"will retry after exactly that delay"，后续重试的退避序列重置回 initialBackoff。
- Call deadline 覆盖所有 attempts（与 Envoy 的整体超时包含重试同思路）。
- **对我们设计的意义**：gRPC 的 maxAttempts 语义（**含首次**）与 Envoy num_retries（**不含首次**）正好相反——两种主流实现各选一边。我们的 query 参数 `attempts=3` 必须明确定义属于哪种语义，并对齐其一（建议：attempts = 总尝试数，gRPC 式，因为"3 次尝试"对用户更直观）；retry 节流器（token bucket）是多租户代理防止 retry storm 的可借鉴模式。

### B4. AWS SDK standard retry mode

来源：[AWS SDK Reference — Retry behavior](https://docs.aws.amazon.com/sdkref/latest/guide/feature-retry-behavior.html)（访问日期 2026-09-03；注意该页为 **2026 新版重试行为**，需 `AWS_NEW_RETRIES_2026=true` 显式 opt-in，未 opt-in 时为旧行为）。

- **计数语义**：`max_attempts` "Total attempts **including the initial request**"，默认 3（= 1 次原始 + 2 次重试；设为 1 即完全禁用重试）。DynamoDB/DynamoDB Streams 客户端默认 4。
- **退避公式（full jitter）**：
  ```
  delay = random(0, 1) × min(20,000 ms, base_delay × 2^retry)
  ```
  - `base_delay`：transient 错误 50ms，throttling 错误 1000ms（**按错误类型分档**）；DynamoDB 25ms/1000ms；
  - 上限 20s（transient 到第 10 次重试触顶；throttling 第 6 次）；
  - `retry` 从 0 起（第一次重试）。
  - 明确称为 "exponential backoff with **full jitter**"。
- **Retry-After**：AWS 服务用 **`x-amz-retry-after`**（毫秒）头做服务器定向重试：出现时使用服务器值，钳制在 `[computed_backoff, computed_backoff + 5000ms]`，有效最大 25s，**不加 jitter**（"because the service is expected to jitter it"）。**不是标准 `Retry-After` 头**。
- **Retry quota（token bucket）**：容量 500 token；每次 transient 重试扣 14，每次 throttling 重试扣 5；重试成功后返还该次消耗，无重试成功返还 1；耗尽即直接返回错误（不重试）。默认 3 次尝试下，持续失败率超过约 22%（transient）/32%（throttling）时开始耗尽配额。**长期低于该失败率时配额无感知**（fail-fast 防风暴，与 gRPC throttle 同思路但按"失败影响"加权）。
- 错误分类：transient（RequestTimeout、InternalError、I/O 失败、无识别错误码的 500/502/503/504）/ throttling（ThrottlingException、TooManyRequestsException、SlowDown 等）/ non-retryable；**错误码优先于 HTTP 状态码**（5xx + throttling 错误码 = throttling 类）。
- **对我们设计的意义**：AWS 的"按错误类型分档 base delay"、"full jitter"、"服务器定向重试钳制区间"、"token bucket 配额"都是可直接抄的成熟细节；其 `max_attempts` 计数语义（含首次）与 gRPC 一致。

### B5. AWS Architecture Blog《Exponential Backoff and Jitter》（2015, Marc Brooker）

来源：[Exponential Backoff and Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/)（访问日期 2026-09-03）。

三种 jitter 公式（原文图表的转录）：
- **Full Jitter**：`sleep = random_between(0, min(cap, base * 2^attempt))`
- **Equal Jitter**：`temp = min(cap, base * 2^attempt); sleep = temp/2 + random_between(0, temp/2)`（"prevents very short sleeps, always keeping some of the slow down from the backoff"）
- **Decorrelated Jitter**：`sleep = min(cap, random_between(base, sleep * 3))`（"we also increase the maximum jitter based on the last random value"）

结论："The solution isn't to remove backoff. It's to add jitter." 无 jitter 的纯指数退避最差（work 最多且完成时间最长）；Equal Jitter 略差于 Full Jitter；Full vs Decorrelated 各有胜负（Full 做的总 work 少，Decorrelated 完成稍快），两者都大幅优于无 jitter。100 个竞争客户端场景下 jitter 把调用次数砍半以上、把重试尖峰摊平成近似恒定速率。2023-05 更新注明：多数 AWS SDK 的 standard/adaptive retry mode 已内置该模式。
- **对我们设计的意义**：默认实现 **full jitter**（`sleep = rand(0, min(cap, base*2^n))`）即对齐 AWS/Envoy 主流做法（Envoy 的公式本质就是 full jitter）。

### B6. SSRF 缓解参考（针对"路径携带动态 upstream"）

来源：[OWASP SSRF Prevention Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Server_Side_Request_Forgery_Prevention_Cheat_Sheet.html)（访问日期 2026-09-03）。

**必须实现的缓解清单（对照 OWASP）**：
1. **优先 allowlist**（Case 1：目标已知且可信）；无法 allowlist（Case 2，目标任意）时退而求其次做 denylist。OWASP 原话："Deny-lists are bypass-prone. Prefer allow-lists."，"When unavoidable, block these minimum ranges:"
   - 云元数据：`169.254.169.254`、`metadata.amazonaws.com`、`metadata.google.internal`
   - 回环：`127.0.0.0/8`、`0.0.0.0/8`、`::1/128`
   - RFC1918 私网：`10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16`
   - 多播：`224.0.0.0/4`、`ff00::/8`
   - 以及 link-local（169.254.0.0/16）与 IPv6 对应范围（文档"Challenges in blocking URLs at application layer"一节要求 v4+v6 同时检查，避免绕过）。
2. **校验解析后的 IP，而不是主机名字符串**：先解析域名，取**所有** A/AAAA 记录逐一检查是否公网地址；"Use the output value of the method/library as the IP address to compare against the allowlist"（防 hex/八进制/dword 编码绕过）；v4 与 v6 都要检查。
3. **DNS rebinding 对策（DNS pinning / resolve-then-connect）**：本代理场景（拨号用户可控的主机名）标准做法是**解析一次 → 校验解析出的 IP → 用该 IP 建立连接（连接时不再重新解析）**，消灭 check 与 use 之间的 TOCTOU 窗口（见 B7）。OWASP 该页的表述是"检索域名背后所有 IP 并逐一公网校验"+（对组织内域名）内部 DNS 优先解析 + 监控 allowlist 域名的解析结果。
4. **禁用重定向跟随**："Disable the support for the following of the redirection in your web client"（否则攻击者可用 302 跳回内网地址绕过校验）。
5. **协议/scheme 白名单**：只允许 `HTTP`/`HTTPS`（SSRF 不限于 HTTP：`file://`、`gopher://`、`data://`、`dict://` 都是向量）；"Do not accept complete URLs from the user"——只接受受控的 host[:port] + path。
6. **网络层兜底**：egress 防火墙 deny-by-default、网络分段（"Only allowed routes will be available for this application"）。
7. 云环境额外防御：迁移到 IMDSv2 并禁用 IMDSv1。

**cors-anywhere（Rob Wu）作为路径式动态 upstream 的先行设计**：
来源：[Rob--W/cors-anywhere README](https://github.com/Rob--W/cors-anywhere) + [issue #301](https://github.com/Rob--W/cors-anywhere/issues/301)（访问日期 2026-09-03）。
- URL 方案：目标 URL 直接内嵌在路径中——"The url to proxy is literally taken from the path"；用法 `/http://google.com/`、`/google.com`（协议可省略默认 http；端口 443 时默认 https：`/google.com:443` → https）。与本设计的 `/https/example.com:4000/v1/...` 同族（差异：我们用 `/scheme/host:port/rest` 三段式且强制 scheme 段）。
- 开放代理滥用史（issue #301 原文）："abuse has become so common that the platform where the demo is hosted (Heroku) has asked me to shut down the server"；2021-01-31 起公共 demo 停止作为 open proxy，2021-02-01 起必须完成浏览器 challenge（访问解锁页面临时解锁）才可用；Heroku AUP 明文"forbids the use of Heroku for operating an open proxy"。
- 当前门控：`originWhitelist`/`originBlacklist`（空 = 允许所有）、`requireHeader`（"If set, the request must include this header or the API will refuse to proxy"，推荐 `['Origin', 'X-Requested-With']` 防被当普通浏览器代理使用）、`checkRateLimit` 回调（按 origin 限流，如 50 req / 3 min）。
- **对我们设计的意义**：路径式动态 upstream 是被验证过的模式，但其公共部署必然沦为被滥用的开放代理——**必须默认带访问控制/限流**（哪怕只是共享 secret header），否则重试能力会把它放大成"带放大器的开放代理"（重试 = 每请求成倍流量）。

### B7. DNS rebinding

来源：[Wikipedia — DNS rebinding](https://en.wikipedia.org/wiki/DNS_rebinding)（访问日期 2026-09-03）。

- 定义与机制："a method of manipulating resolution of domain names"；攻击者控制权威 DNS 并用**极短 TTL** 使答案不进缓存；首次解析返回承载恶意脚本的（公网）IP，脚本再次请求同一主机名时权威 DNS 改答新 IP（可为内网地址），从而绕过同源策略/前置校验。本质是 **check（首次解析/校验）与 use（实际连接）之间的 TOCTOU**。
- 标准对策：
  - **DNS pinning**："the IP address is locked to the value received in the first DNS response"（把首次解析结果钉住，后续不重新解析；需 fail-safe：IP 变化即停止）；
  - 解析链上过滤：递归解析器或网关丢弃应答中的私网/回环地址（与 RFC 5782 的 blocklist 用法有冲突需注意）；
  - Web 服务器拒绝无法识别的 Host 头（服务端自保）。
- **对本代理的落地要求**：对用户提供的 hostname，**先完整解析（含 v4+v6）→ 对每个地址做私网/元数据校验 → 连接时强制使用已校验的那个 IP**（拨号与校验绑定同一结果），绝不能"校验一次、拨号时重新解析"。连接后再校验对端证书主机名（SNI/hostname pinning）防止中间替换。这与 OWASP B6.2/B6.3 合起来构成"解析即校验、校验即拨号"的闭环。

---

## 与本设计（query 参数重试语法）直接相关的对照速查

| 维度 | nginx | Envoy | gRPC A6 | AWS standard |
|---|---|---|---|---|
| 次数语义 | `next_upstream_tries` = 总尝试数（含首次，源码验证） | `num_retries` = 额外重试数（默认 1，总尝试 = N+1，源码验证） | `maxAttempts` = 总尝试数（含原始请求），上限 5 | `max_attempts` = 总尝试数（含首次），默认 3 |
| 状态码触发 | 固定 7 个（500/502/503/504/403/404/429），不可自定义任意码 | `retriable-status-codes` 任意码列表 + `retriable-headers` | retryableStatusCodes（gRPC 状态码） | 按 error code 分类（transient/throttling）+ HTTP 状态 |
| 退避 | **无**（立即重试） | full jitter：`rand[0, 2^N·B)`，B 默认 25ms，cap 默认 10×B | ±20% 乘性抖动：`backoff * random(0.8, 1.2)` | full jitter：`rand(0,1)×min(20s, base×2^retry)`，base 分档 50ms/1000ms |
| Retry-After | 不支持 | `rate_limited_retry_back_off.reset_headers`（`honor_retry_after_header` 字段不存在） | 服务器 pushback（"exactly that delay"） | `x-amz-retry-after`（毫秒，非标准头，钳制 [backoff, backoff+5s]） |
| 非幂等 POST | `non_idempotent` 旗标（1.9.13）放行，但 RFC 9110 禁止代理自动重试非幂等请求（代理须让运营方显式 opt-in） | 缓冲请求体后重试，方法无关（由 retry_on 决定） | 仅按可重试状态码；未提供幂等标记 | 不区分（AWS API 语义内） |
| 防风暴 | max_fails/fail_timeout（被动） | retry budget（20% active，min 3）或 max_retries=3 | token bucket（maxTokens=10，阈值 maxTokens/2） | retry quota（500 token，14/5 per retry） |
| 响应已开始 | 不再重试（文档明文） | 不再重试（`downstream_response_started_` 门控，源码验证） | deadline 门控 | — |
| 请求体重放 | buffering on 时整读（内存+临时文件）后可重放 | retry 启用即缓冲，超限放弃重试（507） | 应用层 | 应用层 |

## 三条对推荐结论影响最大的发现

1. **nginx 变量 proxy_pass 的单地址动态目标无法重试**（源码级验证：`create_round_robin_peer` 对 `naddrs==1` 置 `peers->single`，free_peer 时 `pc->tries=0` 直接终结）——纯 nginx 原生方案被否；OpenResty 必须用 `balancer_by_lua` + `set_more_tries`（其上限受 `proxy_next_upstream_tries` 约束）或全自研 Lua 循环，而后者等于重写本设计的目标产物。
2. **重试语义的计数与触发细节在各先行设计间分歧极大**：Envoy `num_retries`=额外重试（默认 1）、gRPC `maxAttempts`/AWS `max_attempts`=含首次总数、nginx tries=总数；Envoy 抖动为 full jitter `rand[0,2^N·B)`、gRPC 为 ±20% 乘性、AWS 为分档 base + full jitter；"honor Retry-After"在 Envoy 中的实际机制是 `rate_limited_retry_back_off`（传闻中的 `honor_retry_after_header` 字段不存在）。我们的 query 语法必须显式选定语义并写明（建议：attempts=总尝试数 + full jitter + Retry-After 优先且钳制上限）。
3. **RFC 9110 §9.2.2 明文"A proxy MUST NOT automatically retry non-idempotent requests"**——本设计"显式支持 POST 重试"必须做成默认关闭的显式 opt-in（对照 nginx `non_idempotent` 旗标的存在形式），且 SSRF 面（路径式动态 upstream = cors-anywhere 的滥用前车之鉴）要求：解析后校验全部 A/AAAA 是否公网地址、校验与拨号绑定同一解析结果（防 DNS rebinding TOCTOU）、禁止跟随重定向、默认带访问控制——否则"重试代理"会变成带流量放大器的开放代理。
