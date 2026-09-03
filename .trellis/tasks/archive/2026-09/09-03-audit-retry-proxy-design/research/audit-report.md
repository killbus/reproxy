# 审计报告：轻量 HTTP Retry Reverse Proxy（TEAM B）

> 任务：`.trellis/tasks/09-03-audit-retry-proxy-design`
> 日期：2026-09-03
> 证据基础：四份调研文件（三份生态调研的引用均于 2026-09-03 访问验证；`protocol-design-analysis.md` 为审计员独立推演，其外部事实已逐项与调研文件交叉核对）
> - `protocol-design-analysis.md` —— 审计员对需求草案的独立协议分析（项 1–9）
> - `go-ecosystem.md` —— Go 生态（源码级核实）
> - `rust-ecosystem.md` —— Rust 生态（源码级核实）
> - `openresty-and-prior-art.md` —— OpenResty/nginx + Envoy/RFC/gRPC/AWS/OWASP 先例（源码级核实）

---

## 摘要（Executive Summary）

**草案方向全部成立，但存在 6 项高严重度规范缺口；生态侧无可复用的现成项目，推荐 Go 纯 stdlib + cenkalti/backoff v7 自建（约 500–1000 行核心代码），以 Traefik `retry.go`（Apache-2.0）为实现参考模板。**

三个核心判定：

1. **协议**：路径内嵌 dynamic upstream、query 驱动 per-request retry policy、字段级 scope merge、status 条件与策略分离——四个方向判定正确。但 `attempts` 语义未定义、网络错误条件缺失、body replay 前提不可实现等 6 项高严重度缺口必须先补全（本报告 Part 1 给出补全规范）。
2. **生态**：该需求组合（路径动态 upstream + query 驱动 per-request policy + 状态码重试 + SSE + POST replay）在 Go / Rust / OpenResty 三大生态均为空白。Caddy 与 Traefik 的状态码重试都是 2026 年内最近几个月才落地的新能力（PR 分别为 2026-04 / 2026-02，均于 2026-05 随版本发布）且均为静态配置；Pingora 状态码重试是未合并的 feature request（#873）。
3. **路径**：复用现有项目——无候选；基于现有项目扩展——Caddy/Traefik/Pingora/one-api 全部劣于自建；**独立实现（复用框架级基础设施）是唯一合理路径**。技术栈推荐 Go（理由见 Part 3）。

---

# Part 1：协议设计审计（审计项 1–9）

每项给出：**判定**（成立 / 需修正 / 需补全）+ 结论 + 证据指针。

## 项 1：动态 upstream URL 表达方式 — 判定：方向成立，规范需补全

**结论**：路径内嵌（`/https/example.com:4000/v1/x?y=1`）是正确选择。排除法：

- Header 方案（`X-Upstream`）：**SSE 场景不可用**——浏览器 EventSource 无法携带自定义 header，而 SSE 是核心场景。
- Forward-proxy absolute-form：https 上游必须走 CONNECT 隧道，隧道对 proxy 不透明，**无法实现 retry**——致命。
- 子域名编码：需 wildcard DNS + 逐主机证书，复杂度不成比例。

cors-anywhere 验证了路径内嵌模式的可用性，但其公共部署被滥用到 Heroku 强制关停（issue #301）——路径式动态 upstream **必须默认带访问控制**（详见项 9）。

**需补全的解析规范**（草案完全未定义，实现前必须钉死）：

```
PROXY-TARGET := "/" SCHEME "/" AUTHORITY [ "/" RAW-PATH ]
```

| 边界 | 规范 |
|---|---|
| 默认端口 | `/http/h/` → 80；`/https/h/` → 443；显式端口优先 |
| userinfo | authority 含 `@` 一律 400（`user:pass@host` 是 SSRF 混淆经典来源） |
| 路径编码 | RAW-PATH **原样字节透传，禁止 decode/re-encode**（S3 签名 URL 等场景 canonical 形式必须保留） |
| 空路径 | `/https/h` ≡ `/https/h/` ≡ `https://h/` |
| host 形态 | 仅 DNS 名 / IPv4 / [IPv6]（方括号强制）；端口 1–65535、拒绝前导 0（八进制歧义） |
| scheme | 白名单 `http`/`https`；其他 400 |

详见 `protocol-design-analysis.md` §1.2。

## 项 2：`retry[*]` / `retry[status]` scope 与优先级 — 判定：模型成立，需加一层 + 收窄语法

**结论**：字段级 inheritance/override（`[*]` 默认 → `[status]` 覆盖个别字段）成立，与 CSS specificity / 配置分层惯例一致。四点修正：

1. **优先级链必须是三层**（草案只有 query 两层）：`server 配置默认（含硬性 cap）→ retry[*] → retry[NNN]`。没有 server 层，任何客户端可用 `retry[*].attempts=999` 把 proxy 变成放大器。server 层语义为 **clamp**（收窄不放宽）。
2. **scope selector 收窄为精确状态码**：仅 `retry[429]`，不支持 `retry[5xx]` / 区间 scope——区间重叠时"最具体优先"无定义。扩展路径：未来加 `5xx` 类 scope 时规则为 精确码 > 类 > `[*]`。
3. **条件（门）与策略（形）分离确认**：`retry.status` 是门，`retry[NNN].*` 只塑形已过门的重试。`retry[429].attempts=4` 但 429 ∉ retry.status → 死配置，**返回 400**（fail closed，与未知 key 同策略）。
4. **重复 key** last-wins；**非法值**（`attempts=abc`、`retry[999]`）400 并指明 key。

## 项 3：status list / range 表达 — 判定：成立，加类简写 + 钉死边界

**结论**：`retry.status=400,429,500-599` 保留，并建议增加 `Nxx` 类简写（`retry.status=429,5xx` 是最高频写法；Envoy `retry_on: 5xx`、New-API 的 status code ranges 均此惯例）。

边界规则：仅 `,` 分隔（含空白 → 400）；闭区间、反转区间 → 400；合法码 100–599；重叠与重复静默 union；`5xx`/`5XX` 均 accept。先例：New-API 已验证 `"100-199,500-599"` 字符串形式在生产中可行；Envoy `retriable_status_codes` 只支持精确码列表（不支持区间）——我们的区间语法无先例障碍，且表达力更强。

## 项 4：`attempts` 语义 — 判定：**必须定义为总尝试次数**（含首次）

**结论**：`attempts=N` = 该请求最多发给 upstream N 次；`attempts=1` = 不重试；`attempts=0` 非法 → 400。

业界先例**两两相反**，这是草案最危险的未定义点（源码级核实：`openresty-and-prior-art.md` §B1–B4 对照表覆盖 gRPC/AWS/nginx/Envoy；`go-ecosystem.md` §2/§7.2 覆盖 cenkalti v4/v7、failsafe-go、avast/retry-go）：

| 语义 | 先例 |
|---|---|
| **总尝试次数（含首次）** | gRPC A6 `maxAttempts`、AWS `max_attempts`、nginx `proxy_next_upstream_tries`、cenkalti/backoff v7 `WithMaxTries`、failsafe-go `WithMaxAttempts` |
| 额外重试次数 | Envoy `num_retries`、curl `--retry`、cenkalti v4 `WithMaxRetries`、avast/retry-go |

选择"总次数"的理由：字段名 `attempts` 的自然语言直觉一致；gRPC/AWS/nginx 三个最重先例一致；`attempts=1` 无重试、无 off-by-one 争议。**文档必须配 attempts=1/2/3 实际发包数对照表。**

**必须同时补上总预算**（草案缺失）：`retry.budget=30s`——整个 retry 生命周期（所有 attempt + backoff 等待）的硬顶。只有单次 cap 时 `attempts=10&initial=1s&max=8s` 最坏可等待远超客户端耐心。预算耗尽 → 返回最后一次响应（或 504）。

## 项 5：backoff / jitter / Retry-After — 判定：需补全参数面与公式

**结论**（全部公式已对照 Envoy/gRPC/AWS 源码验证）：

```
retry[*].backoff  = constant | linear | exponential   # 默认 exponential
retry[*].initial  = <duration>                        # 默认 1s
retry[*].max      = <duration>                        # 单次等待上限，默认 8s
retry[*].jitter   = none | full | equal               # 默认 full
retry[*].retry_after = honor | ignore                # 默认 honor
```

- **duration 强制带单位**（`1s`/`100ms`/`1.5s`；裸数字 → 400）。
- **exponential 公式钉死**：`wait(n) = min(initial × 2^(n-1), max)`，n 从 1 起。
- **jitter 默认 full**（AWS Architecture Blog 三分法；Envoy 源码即 full jitter `rand[0, 2^N·B)`、AWS `rand(0,1)×min(cap, base×2^n)`）：`wait = rand(0, min(initial×2^(n-1), max))`。equal 保留选项，decorrelated 列为 future。
- **Retry-After 默认 honor**：若触发重试的响应带 `Retry-After`，**直接替换**计算 backoff（服务器说了等多久，尊重它），但 cap 到 `max` 与剩余 budget 的较小值——绝不能因 honor 一个 `Retry-After: 3600` 无限挂起。两种格式（`delay-seconds` / `HTTP-date`）都解析；HTTP-date 在过去 → 0。
- 语义边界（RFC 9110 §10.2.3 原文核实）：Retry-After 规范上是 server → user agent 的指示；proxy 消费它安排自己的重试是 Envoy `rate_limited_retry_back_off` 已采用的合理延伸，但需知晓这是延伸而非规范要求。

**勘误（生态调研重要发现）**：网传 Envoy 有 `honor_retry_after_header` 字段——**不存在**（v1.9–v1.36 proto 全查 0 命中），Envoy 的真实机制是 `rate_limited_retry_back_off.reset_headers`。设计文档不得引用该讹传。

## 项 6：non-idempotent replay 语义 — 判定：成立，但必须做成显式 opt-in + 默认分层

**结论**：RFC 9110 §9.2.2 **明文 "A proxy MUST NOT automatically retry non-idempotent requests"**。本设计的正当性在于：**retry policy 由客户端在 query 中显式指定——传 retry 参数即构成对 POST 重放的知情 opt-in**（镜像 nginx `non_idempotent` 旗标的形态）。因此：

1. 不需要额外的 `retry.non_idempotent=1` 开关——客户端为 POST 配 `retry.status` 即是 opt-in，开关徒增参数面。
2. 但必须区分两类失败并在文档写明：
   - **pre-delivery**（connect refused / TLS fail / DNS fail）：请求未送达 upstream，**对所有 method 安全重试**；
   - **post-delivery**（5xx 响应 / 读响应时 reset）：upstream 可能已执行，非幂等方法有副作用风险（LLM 场景 = 可能重复计费/重复生成）。
3. `retry.network`（项 2 补充）与 `retry.status` 分开控制，恰好给了客户端"只重放绝对安全的 pre-delivery 失败"的选择：只设 `retry.network=1` 不设 `retry.status`。
4. 诚实性义务：最终响应回显 `X-Retry-Count: N`——proxy 的职责是不隐瞒重试已发生。

## 项 7：body replay — 判定：**"不读取 body"严格意义上不可能，需求需重新表述**

**结论**：重试 = 同一请求字节需要 N 份可用副本；TCP 字节流是 consumed 的；客户端只发一次。**不存在任何协议机制让同一份 body 发两次而不在某层存有副本。** 需求应重述为：

> 复用底层 proxy 框架已有的 body buffering/spooling 基础设施，而非自己写 read-then-rebuild。

各栈既有基础设施（复用点）：

| 栈 | 既有基础设施 | 限制 |
|---|---|---|
| nginx | `client_body_buffer_size` + 超限自动落盘（`proxy_request_buffering on` 默认）——nginx 能跨重试重放 body 的机制基础 | `buffering off` 时"已开始发 body 则不能换上游" |
| Traefik v3.7+ | `mirror.NewReusableRequest(req, 2MB cap)`：`io.ReadFull(max+1)` 探测超限 + 内存缓冲重放 + **超限自动降级禁用状态码重试** | 2MB 硬编码默认（可配） |
| Envoy | retry 启用即缓冲请求体（router.cc 源码：`buffering = (retry_enabled \|\| redirect) && !overflowed`），超限放弃重试继续流式转发（无在途请求则 507） | — |
| Pingora | 内建 retry buffer（`enable_retry_buffering()` 默认启用） | **64 KiB 硬上限**（`BODY_BUF_LIMIT` 源码核实），超限截断后禁用重试 |
| Go stdlib | **无任何 body 缓存基础设施**——`GetBody` 是 client-side 概念（源码注释："For server requests, it is unused"），server 侧 `req.Body` 只能读一次 | 必须 tee 或先收齐 |

**实现模式建议**（Go 路线，Traefik 模板）：`io.ReadFull(max+1)` 探测 + cap 内内存缓冲 + 重试时 `bytes.Reader` 重放。超限行为三选一，**建议降级**（流式透传 + `X-Retry-Dropped: body-too-large` header + warn log）：超限 body（文件上传）几乎从不需要 proxy 级 retry，且行为可观测、不静默；`--strict-body-limit` 可切换为拒绝（413）。默认 cap 建议 10 MiB。

**收发时序**：MVP 建议 nginx 式"先收齐（含 spool）再转发"——实现简单、可重试窗口完整；代价是 client→proxy 上传不流式加速，对 AI API 的几 KB JSON 无影响。（tee 直转模式首字节延迟更低但请求阶段失败难重试，列为 future。）

## 项 8：SSE / streaming retry 边界 — 判定：需精确化 commit point + 补超时体系

**结论**：草案"响应开始输出后结束 retry 生命周期"精确化为：

> **Commit point = proxy 向客户端写出该响应的第一个字节（含 status line / headers）。** 此后当前 retry 生命周期终止；upstream 失败只能向客户端传播，绝不重试、绝不改写已发出的 status。

每次 attempt 的完整状态机：

```
(1) dial upstream            失败 → retryable(network)?
(2) 发送 headers+body        失败 → pre-delivery 失败，retryable(network)?
(3) 收到 response headers
      ├─ status ∈ retry.status → 丢弃响应，backoff，retry
      ├─ status ∉ retry.status → headers 写给客户端【COMMIT】→ 流式转发 body
      └─ 等待 headers 超时 → retryable(network)?
(4) body 转发中（已 COMMIT）  upstream 失败 → 终止客户端连接（截断流），不重试
```

关键语义（必须写进文档）：**"200 之后 body 前死亡"不可重试**——headers 已 COMMIT，upstream 断开则客户端收到截断流（诚实语义：不能在客户端已见 200 后改发 502）。日志记 `stream_truncated`。可选扩展（MVP 不做）："hold headers until first body byte" 可让该 case 可重试，代价是静默流让 fetch 长时间 pending。

**该边界在所有成熟实现中同构**（调研交叉验证）：nginx 文档明文"nothing has been sent to a client yet"；Envoy `downstream_response_started_` 门控一切重试（源码）；Pingora 框架保证；Go stdlib `panic(http.ErrAbortHandler)`；Traefik `written || hijacked` 即终止。设计正确性有充分先例背书。

**超时体系（草案完全缺失，必须补）**：

| 参数 | 语义 |
|---|---|
| `retry.per_try_timeout` | attempt 开始到**收到 response headers** 的 deadline（TTFB 约束）。收到 headers 后不适用 |
| `retry.idle_timeout` | COMMIT 后 body 两个 chunk 之间的最大间隔（SSE 长流看门狗），默认大或可关 |
| `retry.budget` | 整个 retry 生命周期总时长硬顶 |

**注意**：per_try_timeout 的 TTFB 限定是 SSE 正确性的关键——若覆盖整个流会误杀长 SSE。Envoy `per_try_timeout` 是同构先例而非反例：文档明确 "only applies before any part of the response is sent to the downstream"（响应开始下发即失效），本设计与之对齐。

## 项 9：SSRF / DNS rebinding — 判定：**第一安全边界，MVP 必须内建**

**结论**：路径内嵌动态 upstream = 默认全开 forward proxy。cors-anywhere 前车之鉴（公共部署被滥用到 Heroku 强制关停，2021 起须浏览器挑战）。七层防护（defense in depth，对照 OWASP SSRF Cheat Sheet + DNS rebinding 对策）：

```
L1 请求解析   : 项 1 的严格解析（拒 userinfo/八进制端口/scheme 白名单/IPv6 方括号）
L2 目标层     : destination allowlist（hostname/wildcard，server 配置）
                默认空 = 全拒绝 + --dangerous-allow-all 逃生舱（文档警示）
L3 解析层     : proxy 自行 DNS 解析 → 所有 A/AAAA 逐一校验非内网段 → PIN 住已校验 IP
                （禁段：127/8、10/8、172.16/12、192.168/16、169.254/16 含云元数据、
                  0.0.0.0/8、::1、fc00::/7、fe80::/10、224.0.0.0/4、ff00::/8 —— v4+v6 都查）
L4 拨号层     : dial 时二次断言目标 IP 不在禁段（防 L3 bug）；连接后校验 TLS 主机名
L5 重定向层   : 永不跟随 3xx（透传给客户端）——跟随即绕过 L2-L4
L6 资源层     : 全链路 timeout（项 8 超时体系）
L7 部署层     : 默认访问控制（共享 secret header 或 allowlist 必居其一）；文档写明预期部署形态为 localhost/内网工具
```

**L3 是 DNS rebinding 的标准反制**：校验与连接绑定同一解析结果（resolve-then-pin），消灭 check-use TOCTOU 窗口；绝不"校验一次、拨号时重新解析"。

Go 落地点：自定义 `http.Transport.DialContext`（拿到实际 dial 地址做 L4 + 自行 resolve 做到 L3）。Pingora 落地点：`HttpPeer::new` 接受预解析 `SocketAddr`，DNS 完全是用户责任（docs.rs 核实）——钉扎可行。

---

# Part 2：生态复用性审计（审计项 10）

## Go 生态

| 候选 | 判定 | 关键事实（源码级核实） |
|---|---|---|
| stdlib `httputil.ReverseProxy` | **复用（流式底座）** | 无内置 retry（文档/源码零命中）；SSE 自动 flush（`text/event-stream` 或 `ContentLength==-1` 时忽略 FlushInterval 立即 flush）；`GetBody` server 侧 unused；Transport 不自动重试 POST（"Don't retry non-idempotent requests"） |
| cenkalti/backoff v7.0.0 | **复用（退避引擎）** | `WithMaxTries` = 总尝试次数语义（与我们项 4 选择一致）；`RetryAfter(d, cause)` 原语天然支持 Retry-After；`RandomizationFactor` 即 jitter |
| go-retryablehttp v0.7.8 | 参考不引入 | 任意 `io.Reader` body 会 `io.ReadAll` 全量入内存**且无上限**——直接当 Transport 有 OOM 风险 |
| Caddy v2.11.3+ | 否决 | `lb_retry_match`（CEL 表达式按 `{rp.status_code}` 重试）2026-04 才合并；retry 是 upstream-selection 级、固定 try_interval、无指数退避、无 Retry-After；**全部 retry 参数静态配置**，query 驱动 per-request policy 无法表达 |
| Traefik v3.7.0+ | 否决（运行时）采纳（参考） | 状态码/timeout/non-idempotent 重试 2026-05 才加入；2MB cap body 重放 + httptrace "零字节写出"边界 + 超限降级——**最佳代码参考（Apache-2.0）**；但配置静态、不可作为库嵌入 |
| gost / one-api / New-API | 否决 | gost 无 retry（源码验证）；one-api/New-API 是渠道网关（重试=换渠道、全局配置、body 无 cap 全量读入、无退避） |

## Rust 生态

| 候选 | 判定 | 关键事实（源码级核实） |
|---|---|---|
| Pingora 0.8.1 | 可行但劣于自建组合 | `fail_to_connect` 仅连接失败触发；请求体 retry buffer 内建但 **64 KiB 硬上限**（超限截断禁用重试）；**状态码驱动重试原生不支持**（issue #873 OPEN，PR #872 `upstream_response_decision` 未合并）；h2 upstream InvalidH2 会无条件重试已发出的非幂等 POST（issue #979，违反 RFC 9110）；SSE 已验证（维护者实测） |
| axum + hyper + reqwest 组合 | 可行（备选路线） | 全控制权；`bytes_stream()` + `Body::from_stream` SSE 直通；预缓冲 Bytes 克隆 O(1)；自写命令式重试循环中状态码 + Retry-After 是自然表达 |
| reqwest-retry 0.9.1 | 不引入 | Retry-After 永远做不了（issue #146：trait 看不到响应头）；流式 body 直接报错 |
| sozu / pingap / tensorzero 等 | 否决 | sozu 只有连接级 + backend 熔断（响应级重试是 TODO）+ AGPL + master/worker 过重；其余全是静态配置模型；AI 网关们是 provider 路由模型，无路径动态 upstream |

## OpenResty / nginx

**否决**，两个层面：

1. **纯 nginx 原生不可行（源码级证据）**：变量 `proxy_pass` 直连主机名时，`naddrs==1` 时 `peers->single` 导致 `pc->tries=0`——**单地址动态目标一次失败即终结，无法重试**（`ngx_http_upstream_round_robin.c:612` 核实）。且 `proxy_next_upstream` 状态码仅支持固定 7 个（500/502/503/504/403/404/429）、无 backoff/jitter/Retry-After（重试立即发起）。
2. **OpenResty 需大量 Lua 且形态不符**：`balancer_by_lua` + `set_more_tries`（受 `proxy_next_upstream_tries` 硬上限约束）+ 每次重试重设 `set_current_peer` 可解动态地址，但状态码触发集先天残缺、退避/Retry-After 仍全要 Lua 自研（balancer 上下文禁 yield，`ngx.sleep` 可用性 UNVERIFIED）；或 `content_by_lua` + lua-resty-http 全自研——等于用 Lua 重写目标产物。**且 OpenResty 非单二进制（目录树形态）、Windows 支持二等公民（UNVERIFIED，社区共识）**——直接违反目标形态。

**复用价值**：`proxy_request_buffering on`（先收齐 body 再转发 + 超限落盘）确认了"缓冲式代理"是重放式代理的标准形态；nginx 文档"nothing has been sent to a client yet"是最早的 commit point 先例。

## 跨生态综合判定

**该需求组合（路径动态 upstream + query 驱动 per-request retry policy + 状态码重试 + SSE + POST replay）在三大生态均为空白**——Go 调研原话："没有维护良好的现成项目同时做'路径派生 upstream + 状态码重试 + SSE'"；Rust 调研："不存在满足全部需求的现成项目"；OpenResty：两条子路线都偏离目标形态。

值得注意的时序事实：**Caddy（2026-04 PR #7569）与 Traefik（2026-02 PR #12667）的状态码重试都是近几个月才落地的新能力**——通用反代的状态码重试本来就是边缘需求，query 驱动 per-request 化更是无人做过。这不是我们没找到，是确实没有。

---

# Part 3：技术栈与 MVP 范围（审计项 11）

## 技术栈推荐：Go（纯 stdlib + cenkalti/backoff v7）

Go 与 Rust 路线 B（axum+reqwest 自写循环）都可行且代码量相近（Go 500–1000 行 vs Rust 400–700 行）。**推荐 Go** 的决定性理由：

1. **流式底座免费**：`httputil.ReverseProxy` 开箱提供 hop-by-hop 头处理、SSE 自动 flush（`text/event-stream` 检测）、streaming copy、`panic(http.ErrAbortHandler)` 边界——Rust 路线 B 这些都要自己拼（reqwest bytes_stream → axum Body::from_stream 透传 OK，但 hop-by-hop 剥离等细节自管）。
2. **有 Apache-2.0 的同构参考实现**：Traefik `retry.go` 的 body-cap 降级 + httptrace 零字节检测 + written 边界，语义与我们需求几乎一一对应，可直接对照实现；Rust 侧没有等价参考（Pingora 语义不同构）。
3. **退避引擎现成**：cenkalti/backoff v7 的 `WithMaxTries`（总尝试次数）、`RetryAfter` 原语、`RandomizationFactor` 恰好匹配我们项 4/5 选定的语义；Rust 侧需手写退避（~50 行，非决定性但无加分）。
4. **SSRF 钉扎有成熟挂载点**：`http.Transport.DialContext` 拿到实际 dial 地址做二次断言 + 自行 resolve——Go 社区熟模式。
5. Pingora 的核心缺口（状态码重试 #873）恰好是我们最中心的需求；其超额供给（百万 QPS 级连接复用）对轻量工具是负资产（复杂度税）。

Rust 路线 B 作为**记录在案的备选**：若团队 Rust 强于 Go、或未来要演进为通用网关，再切换（届时若 Pingora PR #872 已合并，Pingora 路线可重评）。

## 最小 MVP 范围

**包含**（全部为可独立验收的功能项）：

| # | 功能 | 验收标准 |
|---|---|---|
| 1 | 路径解析（严格 grammar） | §1.2 全部边界行为有单测；非法输入 400 带指明原因 |
| 2 | Query namespace 拆分 | retry.* 剥离不透传；透传部分字节序+编码不变（签名 URL 测试用例）；未知 retry key 400 |
| 3 | Retry policy 解析 | 三层优先级（server cap → [*] → [NNN]）；status list/range/Nxx；attempts=总次数；全部非法值 400 |
| 4 | Body 捕获与重放 | cap 默认 10 MiB；超限降级（透传 + `X-Retry-Dropped` + warn log）+ `--strict-body-limit` 模式 |
| 5 | Retry 循环 | 状态码 + 网络错误双通道；per-try TTFB 超时；总 budget；exponential + full jitter 默认；Retry-After honor + cap |
| 6 | Commit point 边界 | headers 写出后绝不重试（状态机 §8 单测）；SSE 透传实测（含长流） |
| 7 | SSRF 七层防护 | 禁段全清单单测（v4+v6）；resolve-then-pin；不跟随重定向；默认拒绝 + allowlist 配置 |
| 8 | 可观测性 | `X-Retry-Count` / `X-Retry-Limit` 响应头；每次重试日志（attempt/status/backoff/剩余 budget） |
| 9 | Server 配置面 | allowlist、max_attempts cap、max_budget cap、body cap、监听地址、访问控制（默认要求其一） |

**明确排除**（记入文档的 future）：temp-file spooling（超限直接降级）；请求体流式直转（tee 模式）；hold-headers-until-first-byte 扩展；`5xx` 类 scope；decorrelated jitter；多 upstream LB/failover；熔断/retry budget（单实例单用户场景不需要防风暴，server cap 已够）；HTTP/3、gRPC。

## 主要风险

| 风险 | 缓解 |
|---|---|
| 公开暴露被滥用（cors-anywhere 前车之鉴） | 默认访问控制必居其一（L7）；文档大字警示 |
| POST 重放副作用（重复计费） | RFC 合规的显式 opt-in 语义；X-Retry-Count 透明回显；文档风险声明 |
| 巨型 Retry-After 挂起 | honor 一律 cap 到 max 与剩余 budget 较小值 |
| 签名 URL 被 query 拆分破坏 | 字节级透传（不 parse/re-serialize）+ 专项测试用例 |
| SSE 被超时误杀 | per_try_timeout 严格限定 TTFB；idle_timeout 独立且默认宽松 |

---

# Part 4：路径决策（审计项 12）

## 最终推荐：**独立实现（build），复用框架级基础设施**

三选项裁决：

| 选项 | 判定 | 理由 |
|---|---|---|
| 复用现有项目 | ❌ 无候选 | 三大生态均空白（Part 2 综合判定）；最接近的 retry-proxy（webtor-io）2022 年废弃；one-api 类是渠道网关非透明 relay |
| 基于现有项目扩展 | ❌ 全部劣于自建 | Caddy：静态配置模型之外的需求要写 plugin + 维护 fork，且 2026-04 才有状态码重试、无退避/Retry-After——扩展它等于在别人的配置模型里重写一遍，代码量 ≥ 自建且引入巨型依赖。Traefik：不可嵌入。Pingora：最中心需求是 OPEN issue，vendor PR #872 要背上 fork 维护税，且 64 KiB body cap、#979 h2 缺陷。one-api：业务耦合 OpenAI 语义，剥离成本 > 自建 |
| **独立实现** | ✅ | Go stdlib ReverseProxy（流式）+ cenkalti/backoff v7（退避）+ Traefik retry.go（Apache-2.0 参考模板）；自建核心 ~500–1000 行：URL 解析（含 SSRF）、policy 解析、retry 循环、body 捕获重放、commit point 守卫 |

**本质判断**：我们的需求（per-request query 驱动策略 + 路径动态 upstream）天然落在所有现成产品的配置模型之外——它们全部假设 retry policy 是部署期静态配置。自建不是"找不到所以自己写"的妥协，而是该需求形态下的正确架构决策；且自建面被 stdlib/backoff 库/Traefik 参考三重复用压到了最小。

---

## 附录：草案缺口清单（12 项，按严重度）

| # | 缺口 | 严重度 | 本报告对应 |
|---|---|---|---|
| 1 | attempts 语义未定义 | 高 | 项 4 |
| 2 | 网络错误重试条件缺失 | 高 | 项 2/6 |
| 3 | 总预算/超时体系缺失 | 高 | 项 4/8 |
| 4 | body replay "不读取"前提不可实现 | 高 | 项 7 |
| 5 | commit point 未精确定义 | 高 | 项 8 |
| 6 | SSRF 防护完全缺失 | 高 | 项 9 |
| 7 | query 拆分字节保留未定义 | 中 | 项 1 |
| 8 | jitter 与 Retry-After 处理未定义 | 中 | 项 5 |
| 9 | 路径解析边界未定义 | 中 | 项 1 |
| 10 | server 层默认/cap 三层优先级缺失 | 中 | 项 2 |
| 11 | 状态码列表边界规则未定义 | 低 | 项 3 |
| 12 | 超限 body 行为未定义 | 低 | 项 7 |

**总评**：草案的四个方向性决策（路径内嵌、query 级 policy、字段级 scope merge、条件/策略分离）全部成立；12 项缺口中 6 项高严重度均为"未定义"而非"设计错误"——修正成本低，且本报告 Part 1 已给出全部补全规范。**协议设计可直接进入实现阶段，实现路径为 Go 自建（复用 stdlib + backoff v7 + Traefik 参考模板）。**
