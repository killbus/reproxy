# 协议设计分析（审计员独立推演）

> 本文件是 TEAM B 审计员对需求方协议草案的独立分析，不依赖生态调研结论。
> 涉及外部事实（RFC 章节号、Envoy 语义等）已标注 `[待验证]`；这些标记为调研前的推演状态，其验证结果分布在 `openresty-and-prior-art.md`（RFC/Envoy/nginx/gRPC/AWS/OWASP）与 `go-ecosystem.md` / `rust-ecosystem.md`（各库 retry 语义），最终采信结论见 `audit-report.md` Part 1。
> 覆盖审计清单项 1–9。

## 1. 动态 upstream URL 的表达方式

### 1.1 候选方案对比

| 方案 | 示例 | 优点 | 缺点 |
|---|---|---|---|
| A. 路径内嵌（需求草案） | `/https/example.com:4000/v1/x?y=1` | 任何 HTTP 客户端可用；浏览器 EventSource 也可用（无法发自定义 header） | 需严格定义解析算法；编码陷阱多 |
| B. Header 携带 | `X-Upstream: https://host/path` + 请求体为原样转发 | URL 无歧义 | EventSource / 部分浏览器 API 无法设置 header；每请求都要拆两处 |
| C. forward-proxy absolute-form | `GET https://host/path HTTP/1.1` 直发 proxy | RFC 9110 原生形式 `[待验证章节号]`；URL 零歧义 | **致命伤：https 上游会走 CONNECT 隧道**，隧道对 proxy 不透明，无法 retry；仅 http 上游可行 |
| D. 子域名编码 | `https://host.proxy.example/` | 对客户端最透明 | 需 wildcard DNS + 逐主机 TLS 证书，复杂度高 |
| E. 端口映射 | 每上游一端口 | 无 | 不可动态扩展 |

**结论：方案 A（路径内嵌）为正确选择**，与 cors-anywhere 的先例形式一致 `[待验证：cors-anywhere 当前访问控制状态]`。
浏览器 EventSource 无法携带自定义 header，这使方案 B 在 SSE 场景不可用——而 SSE 正是本项目的核心场景之一。

### 1.2 路径表达的精确解析规范（草案需补全）

需求草案 `/https/example.com:4000/v1/x` 未定义边界行为。审计建议规范：

```
PROXY-TARGET := "/" SCHEME "/" AUTHORITY [ "/" RAW-PATH ]
SCHEME      := "http" | "https"
AUTHORITY   := host [":" port]        ; 不含 userinfo（含 "@" 直接拒绝）
```

必须逐项定义（草案全部缺失）：

1. **默认端口**：`/http/host/...` → 80，`/https/host/...` → 443；显式端口优先。
2. **authority 禁止 userinfo**：`/https/user:pass@host/` 一律 400 拒绝。`@` 是解析器歧义与 SSRF 混淆的经典来源。
3. **RAW-PATH 保留原始编码**：proxy 收到的路径是 percent-encoded 形式，**必须原样透传给 upstream，不得 decode 再 re-encode**。
   - 任何 normalize 都可能改变 upstream URL 语义：`%2F` decode 后变成路径分隔符、`%20` 与 `+` 在 query 中语义不同、大小写规范化破坏签名。
   - 这直接影响 S3 presigned URL 等 HMAC 签名场景：canonical query 的字节序必须保留（见 §4）。
4. **空路径**：`/https/example.com` 与 `/https/example.com/` 均映射 `https://example.com/`。建议规范为：authority 后无 `/` 视为 `/`。
5. **host 合法性**：仅允许 DNS 名称 / IPv4 / [IPv6]；IPv6 需方括号包裹。端口范围 1–65535，拒绝前导 `0`（八进制歧义，见 §9）。
6. **未知 scheme**：`/ftp/...` → 400，白名单 `http`/`https`。
7. **缺失嵌套目标**：`/` 或 `/https` 或 `/https/` → 400（说明用法）。

### 1.3 Query 拆分规范（对应审计项 4 的 namespace）

请求 query 混装 upstream 参数与 proxy 控制参数，拆分规则必须确定：

- **命名空间**：以 `retry.` 开头的 key（含 `retry[*].` / `retry[status].` 变体）为 proxy 保留，剥离后不再透传 upstream；其余原样透传。
- **字节保留**：透传部分必须保留 **原始顺序与原始编码**。实现上应基于原始 query string 做 key 前缀匹配切割，而不是 parse → re-serialize（parse/re-serialize 会丢 key 顺序、改编码、丢无值 key 的 `?a&b` 形式——这些对签名 URL 都是破坏）。
- **未知 retry.* key**：fail closed → 400。理由：silent ignore 会让策略调试变成猜谜（`retry.atempt=3` 拼错 → 无 retry 且无报错，用户以为有保护）。
- **未来扩展预留**：`retry.` 单一前缀即可，不需要更多保留字。若未来增加非 retry 的控制参数，建议引入二级前缀（如 `proxy.`），而非扩大 `retry.` 语义。

## 2. `retry[*]` / `retry[status]` 的 scope、inheritance、merge、优先级

### 2.1 字段级 override 的语义确认

草案的 merge 语义（`[*]` 为默认 scope，`[status]` 覆盖个别字段）本身是合理的，与 CSS specificity / 配置分层惯例一致。需要补全的规则：

1. **优先级链应为三层而非两层**（草案缺失第一层）：

   ```
   server 默认策略（配置文件/env，运维兜底）
        ↓ query `retry[*]`
        ↓ query `retry[NNN]`
        → effective policy
   ```

   理由：生产部署必须允许运维设定安全上限（max attempts、max budget、destination allowlist）。若只有 query 层，任一客户端可以 `retry[*].attempts=999&retry[*].initial=1ms` 把 proxy 变成放大器。server 层应支持 **cap 语义**（clamp 而非 override），审计建议 MVP 就定义：`server.max_attempts`、`server.max_budget` 为硬顶。

2. **scope selector 语法（MVP 收窄）**：
   - 仅支持精确状态码：`retry[429]`。不支持 `retry[5xx]` / `retry[500-599]` 区间 scope。
   - 理由：区间 scope 引入重叠歧义（`retry[500-599]` 与 `retry[480-599]` 同时存在时 502 归谁？），"最具体优先" 在区间重叠时无定义。精确码 + 前述 §3 的条件列表已覆盖需求。扩展路径记录在案：未来若加 `5xx` 类 scope，规则为 精确码 > 类 scope > `[*]`。

3. **`retry.status`（条件）与 `retry[status]`（策略）的关系**：
   - `retry.status` 是**门**：只有列出的状态码触发 retry。
   - `retry[NNN].*` 只**塑形**已过门的重试，不隐式开新的门。
   - 因此 `retry[429].attempts=4` 但 429 ∉ retry.status 时：该 scope 为死配置。审计建议：返回 400（配置了永不生效的策略几乎必然是用户错误）。折中方案：warn-log + 忽略。**取 400**，与 §1.3 的 fail-closed 原则一致。

4. **重复 key**：`retry[*].attempts=3&retry[*].attempts=5` → last wins（标准 query 语义），文档写明即可。

5. **非法值**：`retry[*].attempts=abc`、`retry[999].attempts=1`（999 非法状态码）→ 400，附错误信息指明哪个 key。

### 2.2 草案未覆盖的维度：网络错误条件

`retry.status` 只定义了 status 维度的 retry 条件。**连接被拒、TLS 握手失败、连接重置、响应头超时**是另一类条件，草案完全未提。这是重大缺口：实际中 LLM API 最常见的可重试故障恰恰是 connect error 与 reset。

审计建议 MVP 增加：

```
retry.network=1            ; 或 retry.on=network
```

语义：网络级失败（connect refused / TLS fail / reset / EOF before headers）是否触发 retry。默认值建议 **1（开启）**——理由见 §6：pre-delivery 的网络失败对所有 method 都是安全重放。是否允许 status-scoped 覆盖网络策略（`retry[429].network=0` 无意义，网络失败没有 status）→ 不允许，`retry.network` 只在 `[*]` 层。

## 3. status list / status range 表达

草案 `retry.status=400,429,500-599`。审计意见：

1. **建议增加类简写 `Nxx`**：`retry.status=429,5xx` 是最高频写法。Envoy 的 `retry_on: 5xx` `[待验证]`、常见 CORS/网关配置均用此惯例。支持 `5xx` 与 `5XX`（normalize 为小写）。
2. **语法规则需钉死**（草案缺失）：
   - 分隔：仅 `,`；`500 - 599` 中含空白 → 400（不做宽容 trim，宽松解析是歧义之源，错误信息指明即可）。
   - 区间：闭区间 `500-599`；反转区间（`599-500`）→ 400；单端开区间（`500-`）MVP 不支持 → 400。
   - 合法码：100–599（1xx 不会作为最终响应出现于 upstream 转发场景，但语法上允许；实现只需 3 位数字校验 + `1\d\d`-`5\d\d` 范围）。
   - 重复与重叠：`500-599,502` 静默 union 去重，不报错。
   - 大小写：`5xx`/`5XX` 均接受。
3. **表达力核对**：Envoy `retriable_status_codes` 只支持精确码列表、不支持区间 `[待验证]`；Prometheus/Alertmanager 支持区间。草案的区间语法无先例障碍，保留。

## 4. `attempts` 语义：总次数 vs 额外次数

**这是草案最危险的不定义点之一**——业界先例严重分裂 `[待验证各库具体语义]`：

| 语义流派 | 含义 | 先例（待验证） |
|---|---|---|
| 总尝试次数（含首次） | attempts=1 → 不重试 | gRPC A6 `max_attempts`、AWS SDK `max_attempts` |
| 额外重试次数 | attempts=1 → 共发 2 次 | Envoy `num_retries`、curl `--retry`、hashicorp go-retryablehttp `RetryMax` |

审计建议：

1. **选"总尝试次数"语义**，即 `attempts=N` = 该请求最多被发给 upstream N 次，attempts=1 = 不重试，attempts=0 非法 → 400。
   - 理由：字段名 `attempts` 与 "总次数" 的自然语言直觉一致；"attempts=1 却重试了一次" 是反直觉陷阱。
   - gRPC 与 AWS SDK（当前事实标准）均取此语义。
2. **文档必须配一个对照表**（attempts=1/2/3 时实际发包数），并在响应 header 回显生效策略（见 §8.3）。
3. **草案缺失的总预算控制（必须补）**：仅有 per-attempt backoff 上限时，`attempts=10&initial=1s&max=8s` 的最坏总时长 ≈ Σ(1,2,4,8,8,…) 可远超客户端耐心。必须增加：

   ```
   retry.budget=30s        ; 整个 retry 生命周期的总时长上限（含所有 attempt + backoff 等待）
   ```

   预算耗尽 → 停止重试，返回最后一次响应（或 504 若无任何响应）。Envoy 有对应 `retry_back_off` + per-try timeout 组合 `[待验证]`；此参数与 server 层 `server.max_budget` cap 配套。

## 5. backoff / jitter / Retry-After 标准化

### 5.1 草案缺口

`backoff=exponential, initial=1s, max=8s` 未定义：jitter、Retry-After、其他策略参数。

### 5.2 审计建议的参数面

```
retry[*].backoff  = constant | linear | exponential     ; 默认 exponential
retry[*].initial  = <duration>                          ; 首次重试前等待
retry[*].max      = <duration>                          ; 单次等待上限
retry[*].jitter   = none | full | equal                 ; 默认 full
retry[*].retry_after = honor | ignore                   ; 默认 honor
```

1. **duration 语法**：强制带单位（`1s`、`100ms`、`1.5s`）；裸数字 → 400。避免 ms/s 歧义（Go `time.ParseDuration` 拒绝裸数，是好先例）。
2. **exponential 公式需钉死**（否则不同实现漂移）：`wait(n) = min(initial × 2^(n-1), max)`，n 从 1 起（首次重试前 wait=initial）。
3. **jitter 公式**（AWS Architecture Blog 经典三分法 `[待验证]`）：
   - `full`: `wait = rand(0, min(initial × 2^(n-1), max))` —— 默认推荐，防惊群最优。
   - `equal`: `temp = min(initial × 2^(n-1), max); wait = temp/2 + rand(0, temp/2)` —— 兼顾延迟与分散。
   - `none`: 无 jitter（测试友好，文档警示不要在生产用）。
   - **默认 full**。decorrelated jitter 列为 future，不进 MVP（参数面过大）。
4. **linear 策略**：`wait(n) = min(initial × n, max)`。constant：`wait = initial`。
5. **Retry-After 语义**（RFC 9110 定义，仅响应方可设置 `[待验证章节号]`）：
   - `honor`（默认）：若触发重试的响应携带 `Retry-After`，取 `max(计算backoff, Retry-After)` 还是**直接替换** backoff？**审计建议：直接替换（cap 到 `max` 与剩余 budget 的较小值）**——服务器明确说了等多久，尊重它比叠加我们的猜测更正确。
   - 两种格式 `delay-seconds` 与 `HTTP-date` 均需解析 `[待验证]`；HTTP-date 在过去 → 视为 0。
   - cap：`Retry-After: 3600` 若超出剩余 budget → 预算耗尽逻辑接管，返回该次响应。**绝不能因 honor 一个巨大的 Retry-After 而无限挂起。**

## 6. non-idempotent request 的 replay 语义

### 6.1 RFC 层面的定性

RFC 9110 §9.2.2 `[待验证]`：GET/HEAD/PUT/DELETE/OPTIONS/TRACE idempotent；POST 不是，"sender 不应未经确认自动重发"。

### 6.2 本设计的正当性论证

关键事实：**retry policy 由客户端自己在 query 中指定**。客户端配置 `retry.status` 于一个 POST 上，即是"确认"本身——这在本设计下不是 proxy 违规替客户端决策，而是客户端显式授权。因此：

1. **无需 `retry.non_idempotent=1` 之类的额外开关**——客户端传 retry 参数即构成对 POST 重放的显式 opt-in。增加开关徒增参数面。
2. 但必须区分两类失败（这是文档与实现的核心正确性要求）：

   | 失败类别 | 请求是否已送达 upstream | 重放安全性 |
   |---|---|---|
   | pre-delivery（connect refused / TLS fail / DNS fail） | 否 | **对所有 method 安全** |
   | post-delivery（5xx 响应 / 读响应时 reset / EOF） | 是，且可能已执行 | 非 idempotent method 有副作用风险（重复扣费、重复生成） |

3. **文档必须写清**：POST 收到 500 后重放，upstream 可能已执行过一次（LLM 场景 = 可能重复计费/重复生成）。客户端选择 `retry.status=500` 即接受此风险。proxy 的职责是不隐瞒：建议在最终响应 header 中回显 `X-Retry-Count: N`（见 §8.3）。
4. `retry.network`（§2.2）与 `retry.status` 分开控制，恰好给了客户端"只重放 pre-delivery 失败"（绝对安全）的选择空间：`retry.network=1` 且不设 `retry.status`。

## 7. 不读取 body 如何可靠 replay —— 审计结论：严格意义上不可能，需求需重新表述

### 7.1 为什么"完全不读取"不可行

重试 = 同一请求字节需要存在 N 份可用副本。TCP 收到的字节流是 consumed 的；客户端只发一次；不存在任何协议机制（100-continue、HTTP/2、trailers）能让同一份 body 被发送两次而不在某一层存有副本。**结论：body 必须在某处被缓存（内存或磁盘）。**

### 7.2 需求的正确表述

"不自行读取和重建 request body"应重新表述为：

> **复用底层 proxy 框架已有的 request body buffering/spooling 基础设施，而不是自己写 read-then-rebuild 逻辑。**

各栈的既有基础设施（复用点，详见生态调研文件）：
- **nginx**：`client_body_buffer_size` + 超限自动落盘 temp file——这正是 nginx `proxy_next_upstream` 能重试的机制基础 `[待验证：proxy_request_buffering off 与 retry 的互斥关系]`。
- **Pingora**：框架层 request body cache（`fail_to_connect` 重试路径）`[待验证]`。
- **Go stdlib**：`httputil.ReverseProxy` 无任何 body 缓存基础设施；`http.Request.GetBody` 是 client-side 概念，server 侧 `req.Body` 只能读一次。**必须自己 tee**（`io.TeeReader` 进 bounded buffer / temp file）。
- **reqwest(-middleware)**：body 需满足 `Clone`（本质 = 调用方预先持有完整副本）。

### 7.3 实现模式（若走自建/Go 路线必须处理的设计点）

```
转发请求体时:  req.Body ──tee──> bounded memory buffer ──溢出──> temp file spool
                                          │
重试时:  spool 内容作为新请求的 body（Content-Length 已知，chunked 请求需重新分块）
```

必须钉死的边界：

1. **上限**：`max_body_buffer`（默认建议 10 MiB，配置化）。AI API 场景 body 均为小 JSON，10 MiB 绰绰有余。
2. **超限行为（三选一，需定夺）**：
   - (a) 拒绝请求（413）——诚实但严苛；
   - (b) 降级为 no-retry 透传 + 响应头标记——不破坏可用性，但"客户端以为有 retry 实际没有"违反契约；
   - (c) 全量落盘——无上限风险（磁盘 DoS）。
   - **审计建议 (b)**：流式透传 + `X-Retry-Dropped: body-too-large` header + warn log。理由：超限 body（文件上传）几乎从不需要 proxy 级 retry，且该行为可观测、不静默。极端安全性场景可配 `--strict-body-limit` 切换到 (a)。
3. **chunked 请求重放**：spool 的是解码后的字节；重放时按新连接重新分块、Content-Length 用实际字节数。原请求的 trailer 若存在则丢失（罕见，文档记录为已知限制）。
4. **流式请求体中途失败**：客户端还在发送 body 时 upstream 死了——此时 body 副本不完整 + 客户端未必发完。MVP 简化：请求体阶段（headers 已转、body 未收完）的 upstream 失败**不重试**，向客户端传播错误。完整方案（等 body 收齐再判定）列为 future。注意：这与"客户端发完 body 后才开始首个 upstream attempt"的 nginx 式缓冲模型是两种不同取舍——nginx 式（先收齐再转发）实现简单、可重试窗口完整；流式直转（tee 模式）首字节延迟低但请求阶段失败难重试。**MVP 建议 nginx 式：收齐（含 spool）再转发。** 代价：client→proxy 的 body 上传不受 streaming 加速——对 AI API 的几 KB JSON 无影响。

## 8. SSE / streaming 的 retry 边界

### 8.1 核心定义：commit point

草案"响应开始输出后结束 retry 生命周期"需要精确化。审计给出的规范：

> **Commit point = proxy 向客户端连接写出该响应的第一个字节（含 status line / headers）。**
> 一旦任何响应字节（哪怕只是 headers）已写给客户端，当前 retry 生命周期终止；此后 upstream 失败只能向客户端传播错误，绝不重试、绝不改写已发出的 status。

推演出的完整状态机（每次 attempt）：

```
(1) dial upstream            ── 失败 → retryable(network)？
(2) 发送 request headers+body ── 失败 → pre-delivery 失败，retryable(network)？
(3) 收到 response status+headers
      ├─ status ∈ retry.status → 丢弃该响应，backoff，retry
      ├─ status ∉ retry.status → 将 headers 写给客户端【COMMIT】→ 流式转发 body
      └─ 等待 headers 超时（per-attempt TTFB deadline）→ retryable(network)？
(4) body 流式转发中（已 COMMIT）
      └─ upstream 中途失败 → 终止客户端连接（截断的 chunked 流），不重试
```

关键推论（草案未写、必须写明的语义）：

1. **"200 之后 body 前死亡"不可重试**：headers 已 COMMIT（写给客户端），upstream 随即断开 → 客户端收到截断流。这是诚实语义：不能在客户端已看到 200 后改发 502。可观测性要求：日志记录 `stream_truncated`。
2. **可选的 "hold headers until first body byte" 扩展**（MVP 不做，记录在案）：推迟 headers 转发直到首个 body 字节，可让 "200-then-die-before-body" 也可重试。代价：合法但静默的流（SSE 长时间无事件）会让客户端 fetch 一直 pending。默认 forward-on-final 是标准 proxy 行为。
3. **HEAD / 204 / 304**：无 body，headers 即全部——COMMIT 后正常结束。

### 8.2 超时体系（草案完全缺失，必须补）

| 参数 | 作用域 | 语义 |
|---|---|---|
| `retry.per_try_timeout` | 单次 attempt | 从 attempt 开始到**收到 response headers** 的 deadline（TTFB 约束）。收到 headers 后不再适用。 |
| `retry.idle_timeout` | 流式阶段 | COMMIT 后 body 两个 chunk 之间的最大间隔（SSE 长流的看门狗）。默认大（如 60s）或可关。 |
| `retry.budget` | 整个 retry 生命周期 | 总时长硬顶（§4.3）。 |

注意 per_try_timeout 的 TTFB 限定是 SSE 正确性的关键：若 per-try 超时覆盖整个流，会误杀长 SSE。这是已知网关坑，必须从参数定义上规避。已验证（`openresty-and-prior-art.md` §B1）：Envoy `per_try_timeout` 文档明确 "This timeout **only applies before any part of the response is sent to the downstream**"，即同样将 per-try 超时限定在响应开始下发之前——本参数设计与该先例对齐。

### 8.3 可观测性（建议 MVP 即含，轻量）

- 响应 header 回显生效策略与结果：`X-Retry-Count: 3`、`X-Retry-Limit: 4`（超过 `max_body_buffer` 降级时 `X-Retry-Dropped`）。
- log 每次重试：attempt N、status、backoff、剩余 budget。

## 9. SSRF / DNS rebinding / 内网访问

**这是本设计的第一安全边界，不是可选项。** 路径内嵌动态 upstream = 默认全开 forward proxy。cors-anywhere 的历史（开放实例被大规模滥用导致被迫关闭公开访问）`[待验证当前状态]` 是直接前车之鉴。

### 9.1 威胁清单

1. **内网/元数据寻址**：127/8、10/8、172.16/12、192.168/16、169.254/16（含云元数据 169.254.169.254）、::1、fc00::/7、fe80::/10。
2. **DNS rebinding（TOCTOU）**：校验时解析到公网 IP，连接时解析到内网 IP。
3. **IP 编码混淆**：十进制 `http://2130706433/`、八进制 `http://0177.0.0.1/`、十六进制、IPv4-mapped IPv6 `::ffff:10.0.0.1`、`0.0.0.0`。
4. **redirect 跟随 SSRF**：upstream 返回 302 → 内部地址，若 proxy 跟随重定向则绕过一切校验。
5. **userinfo 混淆**：`http://expected-host@evil-host/`。
6. **滥用为开放代理**：实例被扫描后当免费跳板（cors-anywhere 案例）。

### 9.2 防护设计（defense in depth，按层）

```
L1 请求解析层   : §1.2 的严格解析（拒绝 userinfo、拒绝八进制端口、仅 http/https、IPv6 方括号）
L2 目标层      : destination allowlist（hostname/wildcard，server 配置）
                 —— 默认空 = 全拒绝？MVP 建议：默认空 + 显式配置必须，文档大字标注
                 —— 逃生舱：--dangerous-allow-all（自担风险，文档警示滥用后果）
L3 解析层      : proxy 自行 DNS 解析（不委托给 HTTP client 的默认解析）
                 - 所有 A/AAAA 记录逐一校验：任一命中内网段 → 拒绝
                 - 解析结果 PIN 住：直接 dial 已校验的 IP（SNI/Host 仍用原 hostname）
                 —— 这是 rebinding 的标准反制：校验与连接使用同一 IP
L4 拨号层      : dial 时二次断言目标 IP 不在禁段（防解析层 bug）
L5 重定向层    : 永不跟随 3xx（透传给客户端，客户端自己决定）
                 —— 透明 relay 的正确行为本就不该解释 redirect；若未来支持跟随，每跳重跑 L2-L4
L6 资源层      : 全链路 timeout（§8.2）+ 响应大小上限可选
L7 部署层      : 文档写明：公开暴露必须加认证；预期部署形态是 localhost / 内网工具
```

### 9.3 与各技术栈的对接点

- **Go**：`http.Transport.DialContext`/`Control` 回调拿到实际 dial 地址做 L4；自实现 L3 需绕过 Transport 的默认解析（自定义 `DialContext` 内自行 `net.Resolver` + 校验 + dial）。
- **Pingora**：`HttpPeer` / 自定义 `Peer` 可否 dial 预解析 IP + 保留 SNI `[待验证]`。
- **nginx/OpenResty**：`resolver` + `balancer_by_lua` 可拿到 resolved 地址做校验 `[待验证具体 API]`。

## 10. 审计发现的草案缺口汇总（按严重度）

| # | 缺口 | 严重度 | 对应章节 |
|---|---|---|---|
| 1 | attempts 语义未定义（总次数 vs 额外次数） | 高 | §4 |
| 2 | 网络错误重试条件完全缺失 | 高 | §2.2 |
| 3 | 总预算（budget）/ 超时体系缺失 | 高 | §4.3、§8.2 |
| 4 | body replay "不读取"的前提不可实现，需重新表述 | 高 | §7 |
| 5 | commit point 未精确定义（"开始输出"含糊） | 高 | §8.1 |
| 6 | SSRF 防护设计完全缺失 | 高 | §9 |
| 7 | query 拆分的字节保留（签名 URL 兼容）未定义 | 中 | §1.3 |
| 8 | jitter 与 Retry-After 处理未定义 | 中 | §5 |
| 9 | 路径解析边界（默认端口、userinfo、编码保留、空路径）未定义 | 中 | §1.2 |
| 10 | server 层默认策略/cap 三层优先级缺失 | 中 | §2.1 |
| 11 | 状态码列表边界规则（空白、反转区间、类简写）未定义 | 低 | §3 |
| 12 | 超限 body 的行为未定义 | 低 | §7.3 |

—— 以上 12 项缺口中 6 项为高严重度。**总体判断：草案的方向（路径内嵌 upstream、query 级 retry policy、字段级 scope merge、status 条件与策略分离）全部成立，但距离可直接实现的规范还差一层精确化。本文件 §1.2–§9.2 即为补全建议，可直接作为实现规范的骨架。**
