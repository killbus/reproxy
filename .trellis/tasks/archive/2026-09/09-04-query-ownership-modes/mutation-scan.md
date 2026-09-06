# Mutation Scan — query-ownership-modes (Batches 1–3 semantics)

> 目的：防自证闭环——证明测试套件**能挂**（每次故意破坏一条钉死语义，套件必须变红），而不是只证明它能绿。
> 方法：隔离副本（`D:\tmp-reproxy-ms`，已验证与工作树逐字节一致后删除；主树从未被触碰）上做 29 个最小变异，每个变异后跑全量套件（`go test ./... -count=1`），断言失败；全部还原并 diff 校验后确认恢复绿。编译失败不算捕获（按 quality-guidelines 规则，用合法语法变异重跑）。

## 判定规则

- **被捕获（红）**：编译通过且套件出现 FAIL → 该钉死行为已被测试锁死 ✓
- **存活（绿）**：套件仍然 ok → 测试盲区，报告为 finding（本批次不补测试——主会话决定是否阻塞）✗
- **编译失败**：不算捕获，换合法语法重跑

## 变异清单与结果

覆盖 design §6 的 ~17 个目标 + Batch 3 报告 §5 的扩展清单 + 扫描中发现值得加测的相邻不变量（m13/m25/m26/m27）。

| # | 目标行为（钉死语义来源） | 变异 | 结果 | 捕获测试 |
|---|---|---|---|---|
| m01 | pure 模式不拆 query（R2，design §6） | pure 分支 `upstreamQuery = SplitQuery(...)` 透传部分 | ✅ 红 | TestProxyPureModeQueryVerbatimAndSingleCall, TestE2EPureModeRetryDotParamsReachUpstream, TestProxyPureModeHeaderPolicyRetries, TestE2EPureModeHeaderPolicy |
| m02 | pure 模式不捕获 body（D14） | `if retryLifecycle` → `if true` 无条件捕获 | ✅ 红 | TestProxyPureModeBodyStreamsNoCapture, TestE2EPureModeLargeBodyNoCap |
| m03 | 无 header 的 pure 用单次 literal，非 Parse(∅) 默认（D13） | `SingleAttemptPolicy` → `Parse(url.Values{})` | ✅ 红 | TestProxyPureModeNetworkFailureNotRetried（管道半边）；TestProxyPureModeSingleAttemptLiteralNotParseDefaults / TestSingleAttemptPolicyLiteral 锁 literal 本身 |
| m04 | retry 模式 + header → 400 冲突（design §3） | 冲突门 `if false && ...` | ✅ 红 | TestProxyModeChannelConflictMatrix rows 4/6, TestE2EModeChannelConflictOverTCP, TestProxyRetryModeDegenerateHeader400 |
| m05 | 未知 X-Reproxy-* → 400（R4，保留命名空间） | `validateReproxyNamespace` 直接 `return nil` | ✅ 红 | TestHeaderUnknownNamespaceRejected, TestValidateReproxyNamespaceDirect, TestProxyRetryModeUnknownReproxyHeader400, TestProxyPureModeUnknownReproxyHeader400 |
| m06 | X-Reproxy-* 剥离不透传（R4） | `buildOutboundHeaders` 去掉 `isReproxyHeader` 检查 | ✅ 红 | TestBuildOutboundHeadersStripsReproxyNamespace(+NonCanonical), TestE2EHeadersStrippedUpstream |
| m07 | 空 header 不等于 absent（design §2 归一化阶梯） | 空值返回 `(nil, nil)` | ✅ 红 | TestHeaderDegenerateInputRejected, TestHeaderParseEmptyVsAbsent |
| m08 | deprecation 日志在 retry-keys 时触发（design §5） | retry-keys 触发分支禁用 | ✅ 红 | TestProxyDeprecationLogGate case 1 |
| m09 | 无 retry key 但消费了网络重试也触发（design §5 隐式依赖） | `onNetworkRetry` 回调永不调用 | ✅ 红 | TestProxyDeprecationLogGate case 3 |
| m10 | 每请求最多一行 deprecation（per-request 去重） | 去重布尔反转（两个触发都打） | ✅ 红 | TestProxyPlainSchemeDeprecationDedupeBothTriggers |
| m11 | plain 段解析为 retry（过渡期，R5）——parser 半边 | `parseSchemeSegment` plain → ModePure（v0.3 提前翻转） | ✅ 红 | TestParsePath 全部 plain 行（ModeRetry, false）；管道半边见 m26 |
| m12 | header 变换不丢字段（design §2） | `network` 键的 pair 静默丢弃 | ✅ 红 | TestHeaderTransformEquivalence, TestHeaderPairGrammar, TestHeaderParseErrorMatrix |
| m13 | header 键大小写敏感（query 语法对齐） | pair 整体 `ToLower` | ✅ 红 | TestHeaderLegitimateValuesRepresentable（`5XX` 被折成 `5xx` 后与设计的大小写语义分离） |
| m14 | scheme 段大小写归一（design §1） | `strings.ToLower(segment)` 移除 | ✅ 红 | TestParsePath 大小写行（/HTTPS+RETRY、/Https+Pure、/HTTP+Retry、/HTTPS、/HTTP） |
| m15 | 冲突 400 的 remedy 命名 actor 来源（design §7 措辞钉） | "middleware or gateway" 换成中性文本 | ✅ 红 | TestProxyModeChannelConflictMatrix, TestE2EModeChannelConflictOverTCP（都断言 "middleware or gateway" 子串） |
| m16 | pure（无 header）不发 X-Retry-*（R2） | `commitResponse` 的 `if retryLifecycle` → `if true` | ✅ 红 | TestProxyPureModeQueryVerbatimAndSingleCall, TestE2EPureModeRetryDotParamsReachUpstream 的 absent 断言；TestProxyPureModeNetworkFailureNotRetried 锁 exhausted() 半边 |
| m17 | header 单次出现（design §2） | 多次出现 last-wins（直接 return 最后一个的 parse） | ✅ 红 | TestHeaderMultipleOccurrencesRejected |
| m18 | pure 流转保持入站 framing（Batch 3 偏差 2） | `req.ContentLength = r.ContentLength` → `-1`（强制 chunked） | ❌ **存活** | 无（见 Findings） |
| m19 | +pure+header 恢复捕获（D14 复活） | `retryLifecycle = true` 移除（复活消失） | ✅ 红 | TestProxyPureModeHeaderPolicyCapturesBody, TestProxyPureModeHeaderPolicyBodyReplayed, TestProxyPureModeHeaderPolicyRetries, TestE2EPureModeHeaderPolicy, TestE2EPureModeHeaderPolicyBodyReplayE2E |
| m20 | `%2B` 不解码为 mode 语法（design §1 escaped-path 规则） | 解码 `%2B`/`%2b` 为 `+` 后再切分 | ✅ 红 | TestParsePath 两行 escaped-plus（大小写 hex） |
| m21 | `event=request` 带 `mode=`（R3/R6） | mode 键从日志行移除 | ✅ 红 | TestProxyRequestLogCarriesMode（三个 mode 行） |
| m22 | deprecation 门只为 plain 段武装（design §4/§5） | 武装条件 `!target.ExplicitMode` → `target.Mode != ModeRetry`（恒假，永不武装） | ✅ 红 | TestProxyDeprecationLogGate cases 1+3, TestProxyPlainSchemeDeprecationDedupeBothTriggers |
| m23 | `hasRetryKeys` 惰性键扫描正确（query.go） | 命中时返回 false | ✅ 红 | TestHasRetryKeys（14 行表）, TestProxyDeprecationLogGate case 1 |
| m24 | scope 键映射为 `retry[*].x` 而非 `retry.[*].x`（design §2） | `queryKeyForHeaderKey` 一律 `retry.` 前缀 | ✅ 红 | TestHeaderScopeKeysNeverDoubleDot, TestHeaderTransformEquivalence, TestHeaderPairGrammar |
| m25 | 重复键 last-wins（header 通道经 Parse） | `vals[len(vals)-1]` → `vals[0]` | ✅ 红 | TestParseDuplicateKeysLastWins |
| m26 | plain 段管道解析为 retry——pipeline 半边（m11 的管道面） | `case ModePure, ModeRetry:`（纯分支吞掉 retry 分支） | ✅ 红 | 全部 v0.1.0 测试（retry 请求进了 pure 分支全红）+ TestProxyRequestLogCarriesMode plain 行 |
| m27 | header 单次出现——边界变体 | 允许恰好两次（`case 2` 也 parse） | ✅ 红 | TestHeaderMultipleOccurrencesRejected（两行 Add） |
| m18b | pure 流转 framing——第二变体（同 m18） | 同 m18（复核确认） | ❌ **存活** | 无（同 m18 finding） |

**29 个变异：27 被捕获，2 存活（同一盲区的两个变体），0 编译失败计入捕获。**

## 方法学备注

- 每个变异只动一处（最小变异），编译通过才计结果；m07/m17 的首版变异造成语法/类型错误，按规则换成合法语法重跑（记录在案）。
- **三个"存活"假象被甄别为变异自身问题并修正**（不改变结论）：
  1. m01 首版用 `if err == nil` 守卫的 SplitQuery 调用——`retry.*` 键在 SplitQuery 里本来就是 400，错误分支落回原样字节，变异在构造上就没有破坏语义。换成 `upstreamQuery, _, _ = SplitQuery(...)`（真用透传部分）后 4 个测试红。
  2. m20 首版 `ReplaceAll(lower, "%2B", "+")` 是**静默 no-op**：`ToLower` 已把 `B` 折成 `b`，`%2B` 永不匹配。改成两个 hex case 都解码后 TestParsePath 红。教训：变异必须先验证它真的改变了行为（用 dbg 测试确认）再跑套件。
  3. m18/m18b 两个变体独立复核（含对四个最相关测试的定向运行），确认是真存活而非缓存/时序假象。
- 每次变异后从 `pristine/` 备份还原并 diff 校验；结束时副本与工作树逐字节一致，最终全量套件绿，然后整个隔离副本删除。主树全程未被触碰（Batch 4 只改 README.md，无 .go 变更）。

## Findings（存活变异 = 测试盲区，主会话决定是否阻塞）

### F-1: pure 模式流转的出站 framing 无测试锁定（m18/m18b）

**变异**（proxy.go `roundTrip`，pure 流转分支）：

```go
// 原文（Batch 3 偏差 2：保持入站 framing）
req.Body = r.Body
req.ContentLength = r.ContentLength

// 变异后（强制 chunked）
req.Body = r.Body
req.ContentLength = -1
```

**为什么存活**：`TestProxyBodyContentLengthPreserved` 只走 retry 模式（捕获路径 `req.ContentLength = captured.Len`），不经过 pure 流转分支；`TestProxyPureModeBodyStreamsNoCapture` / `TestE2EPureModeLargeBodyNoCap` 断言的是 body 字节和"无 413"，mock transport 的 `lastReq.ContentLength` 在这两条测试里没有被读取，真实 TCP 的 e2e 也只断言字节不断言 framing。变异把每个带固定长度 body 的 pure 请求从 `Content-Length: N` 改成 `Transfer-Encoding: chunked`，套件全绿。

**影响面**：与 v0.1.0 的线上差异是可观测的（上游可见的请求 framing 头不同）；语义上 body 字节不变，Go http server 两种 framing 都接受。属于"行为保真"缺口而非"功能"缺口。

**建议修复**（一行断言，不在本批次做——本批次禁改 .go 行为）：

```go
// 在 TestProxyPureModeBodyStreamsNoCapture 中补：
if mt.lastReq.ContentLength != int64(len(body)) {
    t.Errorf("ContentLength = %d, want %d (inbound framing preserved)", mt.lastReq.ContentLength, len(body))
}
```

## 结论

**27/29 变异被捕获；2 个存活均为同一盲区（F-1：pure 模式出站 framing）。**

**F-1 已由主会话裁决为阻塞项并修复（PRD 验收标准：每条钉死语义必须有红-得-上的锁）：** `TestProxyPureModeBodyStreamsNoCapture` 补一行 `mt.lastReq.ContentLength` 断言（mutation-scan.md 建议原文），随后诚实复验——临时应用 m18 变异（`req.ContentLength = -1`）定向运行该测试确认变红，还原后全量套件 ×3 绿。变异捕获计数修正为 **29/29，0 幸存**。

除已修复的 F-1 外，R1–R6 每条钉死语义（三通道模型、纯模式语义、D13/D14、冲突矩阵、deprecation 门、header 语法与命名空间、`%2B` 原字节规则、mode 日志）都有红-得-上的回归锁。
