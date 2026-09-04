# Implement Plan: reproxy MVP (Go)

> 前置：`go 1.25.1` 已装。零外部依赖（cenkalti/backoff 暂不引入——退避公式简单且公式必须钉死，自实现 ~40 行更符合"公式无歧义"验收；若后续要 Retry 骨架再评估引入）。

## 执行顺序（依赖驱动）

### Step 1：项目骨架 + 配置层
- [ ] `go mod init reproxy`
- [ ] `config.go`：ServerConfig 结构 + flags + 校验（allowlist 解析、cap 默认值（MaxBody=10MiB）、L7 启动断言：allowlist 非空或 --dangerous-allow-all）
- [ ] `config_test.go`：flag 默认值、allowlist 通配匹配（精确 / `*.example.com` / 不匹配）、启动拒绝
- 验证：`go test ./... && go vet ./...`

### Step 2：路径解析（R1）
- [ ] `target.go`：ParsePath + 全部边界（默认端口、userinfo 拒绝、IPv6 方括号、端口范围/前导 0、scheme 白名单、空路径、缺失目标、host 形态校验）
- [ ] `target_test.go`：表驱动 ≥20 用例（每边界正反例）
- 验证：`go test ./... -run TestParsePath -v`

### Step 3：Query 拆分（R2）
- [ ] `query.go`：SplitQuery——原始字节切割（保留顺序/编码/无值 key），retry.* 识别（`retry[*].x` / `retry[NNN].x` / `retry.status` / `retry.network` / `retry.budget`），未知 key 400
- [ ] `query_test.go`：字节保真专项（`%2F`、`%20`、`+`、`a&b`、重复 key、空值 key）；未知 key 错误信息含 key 名
- 验证：`go test ./... -run TestSplitQuery -v`

### Step 4：Policy 解析（R3）
- [ ] `policy.go`：ScopePolicy / Policy / Parse（三层 clamp：server cap → [*] → [NNN]）/ Effective(status) 合成
- [ ] `policy_test.go`：三层优先级、字段级 override、死配置 400（`retry[429].attempts=4` 但 429 ∉ status）、非法值 400、duration 强制单位、status list/range/Nxx 解析（含重叠 union、反转区间 400、`5XX` 大小写）、attempts=0/1 边界
- 验证：`go test ./... -run TestPolicy -v`

### Step 5：Body 捕获（R4）
- [ ] `body.go`：Capture（io.ReadFull(cap+1) 探测 + bytes.Reader 重放 + oversized 双模式；默认 cap 10 MiB——审计建议值，通用定位）
- [ ] `body_test.go`：cap 边界（恰好 10MiB / 10MiB+1）、strict 413、非 strict 降级标记、chunked 重放
- 验证：`go test ./... -run TestBody -v`

### Step 6：Backoff 引擎（R5 部分）
- [ ] `backoff.go`：ComputeWait(policy, n, retryAfterHeader) —— exponential/linear/constant 公式 × jitter 三态 × Retry-After honor（http.ParseTime + 秒）+ cap（max 与剩余 budget）
- [ ] `backoff_test.go`：公式数值断言（exponential: 1s,2s,4s…cap 8s；full jitter 区间；equal 公式）、Retry-After 三态（秒/HTTP-date/过去→0）、budget cap 生效
- 验证：`go test ./... -run TestBackoff -v`

### Step 7：SSRF 防护（R7）
- [ ] `ssrf.go`：isForbiddenIP（v4+v6 全禁段表）+ PinnedResolver（LookupIPAddr → 全记录校验 → DialContext 二次断言 + tls.ServerName）
- [ ] `ssrf_test.go`：禁段全清单逐项（含 169.254.169.254、::ffff:10.0.0.1 映射地址）、allowlist gate、resolve-then-pin 单测（mock resolver 返回 [公网IP, 内网IP] → 拒绝）
- 验证：`go test ./... -run TestSSRF -v`

### Step 8：核心代理循环（R5+R6，最重）
- [ ] `proxy.go`：Handler——组装 outbound request（URL 重写、Via/X-Forwarded-For、hop-by-hop 剥离）→ attempt 循环（RoundTrip + TTFB context + 状态门/网络门 + backoff + budget）→ COMMIT（WriteHeader 守卫 + io.Copy + Flusher）→ 耗尽路径（最后响应 / 504）
- [ ] `proxy_test.go`：状态序列注入的 mock upstream（500,500,200 → 2 次重试成功）；commit point 后死亡不重试；SSE 流式（chunked 逐块 flush 断言）；network 错误重试；Retry-After 驱动的等待；budget 耗尽 504；X-Retry-* 头断言
- 验证：`go test ./... -run TestProxy -v`

### Step 9：main + E2E
- [ ] `main.go`：flag 组装 + 启动日志
- [ ] `e2e_test.go`：起真实 listener + httptest upstream（可脚本化状态/延迟/SSE）→ 完整 client 请求断言
- [ ] `README.md`：用法、协议文档（attempts=1/2/3 对照表、全部 retry 参数语义、400 场景表）、SSRF/部署警示
- 验证：`go test ./... && go vet ./... && go build`

### Step 10：收尾
- [ ] 全量 `go test ./... -count=1`（禁缓存）+ `go vet`
- [ ] 对照 PRD 验收清单逐项勾验
- [ ] commit（Phase 3.4）

## 回滚点

每 Step 一个 commit（Step 1 合并骨架）；出问题 revert 单步。测试先行：每步先写表驱动用例再实现。

## 验证命令

```bash
go build ./... && go vet ./... && go test ./... -count=1 -race
```
