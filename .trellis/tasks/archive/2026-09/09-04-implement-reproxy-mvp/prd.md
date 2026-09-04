# Implement reproxy MVP (Go)

## Goal

实现轻量 HTTP retry reverse proxy 的 MVP。规范来源（已审计定稿）：

- **协议规范**：TEAM B 审计报告 Part 1（项 1–9 补全规范）`.trellis/tasks/archive/2026-09/09-03-audit-retry-proxy-design/research/audit-report.md`
- **技术基线**：ADR-0001 `docs/adr/0001-choose-go-as-implementation-stack.md`（Go + stdlib + cenkalti/backoff v7，唯一外部依赖）

## Requirements

### R1 路径解析（严格 grammar）

`PROXY-TARGET := "/" SCHEME "/" AUTHORITY [ "/" RAW-PATH ]`，SCHEME ∈ {http, https}：

- 默认端口 http→80 / https→443，显式端口优先；端口 1–65535，拒绝前导 0
- authority 含 `@` → 400；仅 DNS 名 / IPv4 / [IPv6]（方括号强制）
- RAW-PATH **原样字节透传**（不 decode/re-encode）；空路径 `/https/h` ≡ `/https/h/`
- `/ftp/...` 等未知 scheme → 400；缺失嵌套目标（`/`、`/https`、`/https/`）→ 400（说明用法）
- 错误响应指明具体原因

### R2 Query namespace 拆分

- `retry.*` key（含 `retry[*].` / `retry[NNN].` 变体）为 proxy 保留，剥离后不透传
- 透传部分保留**原始字节序与编码**（基于原始 query string 前缀切割，不 parse/re-serialize）
- 未知 `retry.*` key → 400（fail closed），错误指明 key 名

### R3 Retry policy 解析（三层优先级 + 字段级 override）

```
server cap → retry[*] → retry[NNN]（精确码，仅 3 位数字 100–599）
```

字段面（scope 内）：
- `attempts`（总尝试次数，≥1，默认 3；=1 即不重试）
- `backoff` = constant | linear | exponential（默认 exponential）
- `initial`、`max`（duration，强制单位 `s`/`ms`；默认 1s / 8s）
- `jitter` = none | full | equal（默认 full）
- `retry_after` = honor | ignore（默认 honor）
- 门控字段（仅 `[*]` 层）：`retry.status`（list/range/Nxx）、`retry.network`（默认 1）

门与形分离：`retry[429].attempts=4` 但 429 ∉ retry.status → 400（死配置）。非法值 → 400 指明 key。

### R4 Body 捕获与重放

- Traefik 模式：`io.ReadFull(max+1)` 探测 + cap 内内存缓冲 + 重试时 `bytes.Reader` 重放
- 默认 cap 10 MiB（审计建议值，通用代理定位；server 配置 `--max-body` 可调）
- 超限降级：流式透传 + `X-Retry-Dropped: body-too-large` 响应头 + warn log；`--strict-body-limit` 切换为 413
- chunked 请求：spool 解码后字节，重放按新连接分块
- 重试中响应中途失败 → 终止客户端连接，不重试（commit point 后）

### R5 Retry 循环（状态码 + 网络错误双通道）

attempt 状态机（每次 attempt）：
1. dial upstream 失败 → `retry.network` 门控
2. 发送 headers+body 失败 → `retry.network` 门控（pre-delivery）
3. 收到 response headers：
   - status ∈ retry.status → 丢弃，backoff，重试
   - status ∉ → 写给客户端【COMMIT】→ 流式转发 body
   - per-try TTFB 超时 → network 通道
4. body 转发中失败 → 终止连接（不重试）

Backoff 公式（exponential）：`wait(n) = min(initial × 2^(n-1), max)`；full jitter：`wait = rand(0, 计算值)`；equal：`temp/2 + rand(0, temp/2)`。Retry-After（honor）：直接替换计算 backoff，cap 到 `max` 与剩余 budget 较小值；HTTP-date 过去 → 0。`retry.budget`（默认 30s，server cap 可收窄）耗尽 → 返回最后响应或 504。

### R6 Commit point 边界

- **Commit point = 向客户端写出第一个字节（含 status line / headers）**
- 写出后绝不重试、绝不改写已发出的 status；SSE 长流不被 idle_timeout 误杀（默认 0 = 关闭）

### R7 SSRF 七层防护

- L1 严格解析（R1）+ L2 allowlist（server 配置，默认全拒绝 + `--dangerous-allow-all` 逃生舱）
- L3 resolve-then-pin：proxy 自行 DNS 解析 → **全部 A/AAAA** 逐一校验禁段 → dial 已校验 IP（v4+v6 禁段：127/8、10/8、172.16/12、192.168/16、169.254/16、0.0.0.0/8、::1、fc00::/7、fe80::/10、224.0.0.0/4、ff00::/8）
- L4 dial 时二次断言目标 IP（防 L3 bug）
- L5 永不跟随 3xx（透传）
- L6 全链路超时（R5 budget + per-try TTFB）
- L7 部署层访问控制（MVP：allowlist 非空或 `--dangerous-allow-all` 才启动）

### R8 可观测性

- `X-Retry-Count` / `X-Retry-Limit`（attempts）响应头；降级时 `X-Retry-Dropped`
- 每次重试日志（attempt / status / backoff / 剩余 budget）
- 结构化错误页（400/413/504）指明原因

### R9 Server 配置面

CLI flags（`flag` 包）：`--listen`、`--allowlist host1,host2`（精确+通配 `*.example.com`）、`--max-attempts`、`--max-budget`、`--max-body`、`--strict-body-limit`、`--dangerous-allow-all`。默认值见各项。

## Out of scope（MVP 排除，记入 docs）

temp-file spooling（超限直接降级）、请求体流式直转（tee）、hold-headers-until-first-byte、`5xx` 类 scope、decorrelated jitter、多 upstream LB/failover、熔断/retry budget、HTTP/3、gRPC、管理面/指标端点。

## Acceptance Criteria

- [ ] R1–R9 全部有对应的单元测试；`go vet` / `go test ./...` 全绿
- [ ] 签名 URL 场景：透传 query 的字节序与编码不变（专项测试用例）
- [ ] commit point 语义：headers 写出后 upstream 中途死亡不触发重试（单测模拟）
- [ ] SSE 透传实测：本地 mock upstream 发事件，客户端实时收到（端到端测试）
- [ ] SSRF：禁段全清单单测（v4+v6）；allowlist 默认拒绝；resolve-then-pin（DNS rebinding 场景单测）
- [ ] body 超限降级路径单测（cap=10MiB 边界，含 `--strict-body-limit` 模式）
- [ ] Retry-After honor + cap 单测（秒格式 + HTTP-date + 过去时间 + 超大值）
- [ ] 集成 smoke：`go run .` 起服务 → curl 经代理访问本地 mock upstream，重试行为符合预期
- [ ] README.md：用法、配置、协议（retry 参数语义、attempts=1/2/3 对照表）、SSRF/部署安全警示
