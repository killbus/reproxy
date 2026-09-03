# TEAM B Audit: Lightweight HTTP Retry Reverse Proxy — Design & Tech Path

## Background

TEAM B（本任务）负责对一个“轻量、通用、可配置的 HTTP reverse proxy”的定位与协议设计做技术审计，
并确定后续实现路径。项目核心诉求：

- 透明 HTTP relay：`Client → Retry Proxy → HTTP Upstream`
- 动态 upstream（通过请求路径表达）
- 按请求定义 retry policy（通过 query 参数表达）
- 按 HTTP status 配置独立 retry 行为（scope + inheritance/override）
- 指数退避及其他 backoff 策略
- 显式支持 non-idempotent request retry
- 完整支持 HTTP streaming / SSE；响应开始输出后终止当前 retry 生命周期
- 尽可能复用底层 HTTP proxy 的 request replay 能力

目标技术形态：single binary、stateless、low overhead、streaming-first、dynamic upstream、request-level retry policy。

仓库现状：当前 repo 仅是 Trellis 脚手架，**无任何实现代码**。因此本次审计 = 需求中协议设计的技术审计
+ 外部开源生态（Go / Rust / OpenResty）的复用性评估 + 实现路径决策。

## Scope

### In scope（12 项审计重点，来自 TEAM B 审计清单）

1. 动态 upstream URL 的表达方式及协议设计
2. `retry[*]` / `retry[status]` 的 scope、inheritance、merge、优先级
3. status list / status range 的表达方式
4. `attempts` 语义：总请求次数 vs 额外 retry 次数
5. exponential backoff、jitter、Retry-After 的标准化方式
6. POST 等 non-idempotent request 的 replay 语义
7. 不自行读取/重建 request body 情况下的可靠 replay 机制
8. SSE / streaming 已开始输出后的 retry 边界
9. 动态 upstream 的 SSRF / DNS rebinding / 内网访问安全约束
10. Go / Rust / OpenResty 现有开源实现的复用价值
11. 最适合的技术栈与最小 MVP 范围
12. 最终建议：复用现有项目 / 基于现有项目扩展 / 独立实现

### Out of scope

- 任何代码实现（审计阶段不写实现）
- 负载均衡、熔断、限流、认证鉴权等 proxy 周边能力（可提及但不入 MVP）
- 速率限制治理、多租户、配置持久化/管理面
- 非 HTTP(S) 协议（gRPC/HTTP3 可作为附注，不入审计主结论）

## Deliverables

1. **审计报告**（`research/audit-report.md`）：覆盖 12 项审计重点，每项给出结论、理由、风险与建议
2. **实现路径决策**（含在报告中）：明确推荐路线（reuse / extend / build）及理由
3. **MVP 范围建议**（含在报告中）：最小可验证功能集与验收标准
4. **协议设计修正建议**（含在报告中）：对需求文档中初步语法（路径表达、retry query 语法、status scope）的修正与补全

## Acceptance Criteria

- [ ] 12 项审计重点每项都有明确结论（含“不适用/需修改”的判定），不悬而未决
- [ ] 每项关键结论有可验证的依据（标准文档、源码事实、性能特征或明确标注的权衡判断）
- [ ] 动态 upstream 表达方式有明确推荐方案（含 SSRF 防护边界设计）
- [ ] retry policy 语法（scope/inheritance/status range/attempts 语义/backoff/Retry-After）有完整、无歧义的规范描述
- [ ] non-idempotent replay 与 body replay 机制有明确结论（复用什么基础设施、自建什么）
- [ ] streaming retry 边界有精确定义（什么算“已开始输出”、何时终止 retry）
- [ ] Go/Rust/OpenResty 三条技术路线各有复用性评估（点名具体项目/库及复用点）
- [ ] 最终给出单一明确推荐路径 + MVP 范围 + 主要风险
- [ ] 报告结论可直接作为后续实现任务的 PRD/design 输入

## Constraints

- 审计阶段不引入代码依赖，不创建索引之外的构建文件
- 外部生态调研基于公开信息（官方文档/源码/发布物），标注信息时效
- 报告语言：中文为主，技术术语保留英文原文
- **Agent 分步持久化**：所有被 dispatch 的研究 agent 必须每完成一节立即将该节事实与引用 append 到 research/ 交付文件，禁止最后一次性写出（进程中断导致过调研成果全部丢失）
- **中断即恢复**：研究 agent 或请求遇到 4xx/5xx / 进程中断视为临时故障，指数退避重试或经 SendMessage 恢复 agent，不停止审计推进
