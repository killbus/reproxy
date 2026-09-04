# Mutation Scan — reproxy MVP test suite

> 目的：防自证闭环——证明测试套件**能挂**（每次故意破坏一条钉死语义，套件必须变红），而不是只证明它能绿。
> 方法：在隔离副本（/tmp/ms，快照自 commit 00a57e5，即 Batch 4 提交后的主树；快照时点 trellis-check 的修复尚未合入，见方法学勘误）上做 23 个最小变异，每个变异后跑全量套件，断言失败；全部还原后确认恢复绿。主树未受影响（trellis-check 并行运行中）。

## 判定规则

- **被捕获（红）**：编译通过且套件出现 FAIL → 该钉死行为已被测试锁死 ✓
- **存活（绿）**：套件仍然 ok → 测试盲区，需要补断言 ✗

## 变异清单与结果

| # | 目标行为（钉死语义来源） | 变异 | 结果 | 捕获测试 |
|---|---|---|---|---|
| M1 | attempts ≥ 1 校验（审计项 4） | `n < 1` → `n < 0` | ✅ 红 | TestParseInvalidScopeFields 等 |
| M2 | 死配置 400（审计项 2） | 禁用 RetryableStatus 检查 | ✅ 红 | TestParseDeadConfig / TestProxyDeadConfig400 |
| M3 | 反转区间 400（审计项 3） | `l > h` 检查禁用 | ✅ 红 | parseStatusGate 反转区间用例 |
| M4 | attempts 服务端 clamp 只收窄（审计项 2） | clamp 条件禁用 | ✅ 红 | TestParseClamp 系列 |
| M5 | Nxx 大小写不敏感（审计项 3） | ToLower 移除 | ✅ 红 | 5XX 大小写用例 |
| M6 | 指数公式 2^(n-1)（审计项 5） | shift 偏移 +1 | ✅ 红 | TestComputeWaitExponentialSequence |
| M7 | honor 的 Retry-After 不被 jitter（审计项 5） | 恢复 Batch-2 bug（无条件 jitter） | ✅ 红 | TestComputeWaitRetryAfterNotJittered |
| M8 | 不可解析 Retry-After 回退计算值（审计项 5） | 未知值返回 true | ✅ 红 | TestComputeWaitRetryAfter（unparseable 用例） |
| M9 | 等待不超剩余 budget（审计项 4） | budget cap 禁用 | ✅ 红 | TestComputeWaitBudgetCap / TestProxyBudgetExhaustion |
| M10 | 预算硬停·循环顶（审计项 4） | attemptNo>1 检查禁用 | ✅ 红 | TestE2EBudgetExhaustion |
| M11 | 预算硬停·状态分支交付持有响应（审计项 4） | 分支禁用 | ✅ 红 | e2e 场景 12（budget 截断） |
| M12 | 降级透传字节保真（审计项 7） | 恢复 Batch-3 bug（丢 Prefix） | ✅ 红 | TestProxyDegradedBodyForwarded / TestE2EPOSTOversizedDegraded |
| M13 | Retry-After 驱动等待（审计项 5） | header 不读取 | ✅ 红 | TestProxyRetryAfter 驱动等待用例 |
| M14 | strict 模式 413（审计项 7） | 413 分支禁用 | ✅ 红 | TestProxyStrictBodyLimit413 |
| M15 | hop-by-hop 剥离（RFC 9110 §7.6.1） | Keep-Alive 不剥离 | ✅ 红 | hop-by-hop 断言用例 |
| M16 | X-Forwarded-* 追加（代理链义务） | 设置移除 | ✅ 红 | XFF 断言用例 |
| M17 | query 字节保真（审计项 1） | segment 被解码 | ✅ 红 | TestE2EQueryBytePreservation / TestProxyQueryBytePreservation |
| M18 | commit 后中断连接（Go stdlib 约定） | panic 改 return | ✅ 红 | commit-point 死亡用例 |
| M19 | 169.254/16 云元数据禁段（审计项 9） | 从禁段表删除 | ✅ 红 | isForbiddenIP 全清单用例 |
| M20 | 解析 fail-closed（审计项 9） | 混合解析放行（continue） | ✅ 红 | TestSSRF 混合解析用例 |
| M21 | 拨号 L4 二次断言（审计项 9） | 断言禁用 | ✅ 红 | TestSSRF DialContext re-assert 用例 |
| M22 | 通配符单标签锚定（审计项 9/L2） | 匹配子子域名 | ✅ 红 | TestAllows `a.b.example.com` 用例 |
| M23 | L7 启动拒绝空 allowlist（审计项 9） | 启动门禁用 | ✅ 红 | TestValidateRefusesEmptyAllowlist |

## 扫描方法学说明

- M7/M12 首轮结果是 `[build failed]`（变异文本造成语法错误）——**编译失败不算捕获**，已用合法语法变异重跑并确认测试红。
- 每个变异只动一处（最小变异），跑全量套件，还原后继续下一个。
- 结束时副本与快照逐字节比对：全等（copy==pristine==main tree）。

## 附带发现：时序敏感测试（真实缺陷，非扫描干扰）

无 CPU 争用时 8 轮中 4 轮复现失败——这不是扫描引入的噪声，是测试自身缺陷：

- `TestProxyRetryAfterPastDate` / `TestProxyRetryAfterIgnored` / `TestProxyPerStatusScopeShaping`
- 共同模式：断言 `elapsed ≤ 100-150ms`，等待实际只有 1-2ms；实测 handler 管道（全 mock）耗时在 **4ms~68ms 间波动**（Windows 调度/计时器抖动），上界余量不足
- **对变异扫描结论的影响：无**——这 3 个测试只断言上界，永远不会捕获"让事情变快"的变异；23 个变异各自的捕获测试断言的都是确定性行为（精确值/字节相等/状态码/≥900ms 下界）
- **修复**：上界放宽到 500ms（判别主力是下界断言，放宽不损失判别力）；或引入注入时钟
- **状态**：待主树 trellis-check 完成后修复（避免与其 3 连跑冲突）

## 方法学勘误

- 结束校验中标记为 "main==copy" 的 md5 对比实际比较了副本自身（shell 作用域笔误）；有效结论仅为 copy==pristine（逐文件 diff 全等）。快照来自 `cp`（提交后主树），副本内 23 个变异全部还原并 diff 校验。主树在此期间由 trellis-check 独占使用。

## 结论

**23/23 变异全部被捕获，0 存活**——测试套件对审计钉死语义的每一处都有回归锁。防自证闭环要求落地：套件不仅全绿，且被证明**能挂**。
