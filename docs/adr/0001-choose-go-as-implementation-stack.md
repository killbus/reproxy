# ADR-0001：实现技术栈选择 Go

- **状态**：已接受（Accepted）
- **日期**：2026-09-03
- **决策人**：killbus（基于 TEAM B 审计报告 `.trellis/tasks/archive/2026-09/09-03-audit-retry-proxy-design/research/audit-report.md`）
- **关联**：审计结论为"独立实现（build），复用框架级基础设施"；本 ADR 决定用哪个语言生态落地

## 背景与语境

审计已确认（源码级核实，2026-09-03）：

1. 需求组合（路径动态 upstream + query 驱动 per-request retry policy + 状态码重试 + SSE + POST replay）在 Go / Rust / OpenResty 三大生态均为空白，无现成项目可复用。
2. 两条可行自建路线进入最终对比：
   - **Go 路线**：纯 stdlib（`net/http` + `httputil.ReverseProxy`）+ cenkalti/backoff v7
   - **Rust 路线 B**：axum + hyper(-util) + reqwest 组合，自写命令式重试循环
3. OpenResty 已在审计中否决（源码级证据：单地址动态目标 `pc->tries=0` 无法重试；非单二进制；Windows 二等公民）。

## 决策

**选择 Go。** 原则：用成熟生态，不闭门造车——代理机制层全部站在 Go stdlib 十五年加固的设施上，自建代码只写产品逻辑（策略解析 / retry 循环 / SSRF 防护）。

### 决策计算（加权矩阵，权重来自目标形态：streaming-first、single binary、安全边界优先）

| # | 维度 | 权重 | Go | Rust B | 依据 |
|---|---|---|---|---|---|
| 1 | 流式/代理底座复用 | 20% | 9 | 6 | `httputil.ReverseProxy` 免费提供 hop-by-hop 处理、SSE 自动 flush（`text/event-stream` 检测）、`ErrAbortHandler` 边界；Rust 无 ReverseProxy 等价物，代理语义需自行拼装 |
| 2 | 重试语义参考实现 | 20% | 9 | 4 | Traefik v3.7 `retry.go`（Apache-2.0）与 MVP 语义逐项同构（body cap + 超限降级、httptrace 零字节检测、`written||hijacked` commit point）；Rust 侧无等价参考 |
| 3 | 退避/Retry-After 库契合 | 10% | 8 | 5 | cenkalti/backoff v7：`WithMaxTries`（总次数语义）+ `RetryAfter` 原语 + `RandomizationFactor`，三需求点全命中；Rust 需手写退避 |
| 4 | SSRF 钉扎挂载点 | 10% | 8 | 7 | `Transport.DialContext` 拿实际 dial IP 做 L4 断言，L3+L4 同点闭环；reqwest `resolve()` 亦可 |
| 5 | 依赖/供应链面 | 10% | 9 | 5 | Go：1 个外部依赖（cenkalti，纯 Go）；Rust：数百传递 crate——对安全边界工具是真实架构差异 |
| 6 | 目标规模性能 | 5% | 7 | 9 | 瓶颈是 upstream LLM API（秒级 TTFB），代理层 CPU 占比 <5%，此维度不构成决策因素 |
| 7 | API 稳定性视野 | 10% | 8 | 6 | `net/http` 十余年冻结级稳定；Rust hyper 0.14→1.0 近期破坏性重写先例 |
| 8 | 构建/跨平台 | 5% | 9 | 7 | Go 交叉编译零配置；Rust musl 需工具链 |

**加权得分：Go 7.7 vs Rust B 5.1。**

决定性逻辑：本产品的核心价值 = 重试策略的正确性（实现风险大头）+ 代理机制的可靠性（代码量大头）。Go 把后者免费送你（stdlib），还把前者的最佳参考实现（Traefik retry.go，同语义、Apache-2.0 可抄）递到手上；Rust 给的性能优势恰好是本场景不需要的。Rust 名义代码量更少（400–700 行 vs 500–1000 行），但那是露出水面的部分——水面下的代理语义层在 Go 里免费。

## 后果

**正向**：
- 实现风险因 Traefik 参考模板直接减半；外部依赖仅 1 个（cenkalti/backoff v7）
- 交叉编译/部署简单，符合 single binary、stateless 目标形态

**负向 / 接受的代价**：
- 放弃 Rust 的内存安全保证与峰值性能（本场景非瓶颈）
- goroutine 每 TLS 连接 ~10KB 级内存（轻量场景无影响，见重估条件）

**重估（revisit）条件**——任一成立时重新评估：
1. 部署形态变为边缘高密度（10 万+并发流/实例、内存受限）→ Rust/Pingora 路线重评
2. 产品演进为多 upstream 网关/LB，且 Pingora PR #872（`upstream_response_decision` 钩子）已合并 → Pingora 状态码重试缺口消失
3. 团队构成变化使 Rust 维护成本显著低于 Go

## 实现基线（由本决策确定）

- 语言：Go（module `reproxy`），外部依赖仅 `github.com/cenkalti/backoff/v7`
- 参考实现：Traefik `pkg/middlewares/retry/retry.go`（Apache-2.0，对照实现不逐行照抄）
- MVP 范围与验收标准：见审计报告 Part 3（9 项功能）
- 协议规范：见审计报告 Part 1（项 1–9 补全规范）
