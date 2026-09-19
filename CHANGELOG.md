# Changelog

## v1.2.0 — 2026-09-19

从上游 `Sliverkiss/workbuddy2api`（同步点 76bb543 之后 80 个提交）择优选入，
全部改动均以「我们代码里确实存在该缺陷」为前置，逐项用上游原始测试用例在本仓库
复现（RED）后修复（GREEN），不照搬上游架构。

### 修复（实证复现）

- **SSE 非 delta message 分支未 latch**：每帧携带完整 `message` 的上游会让正文被
  逐帧重复追加（N 帧 → N 遍）。实测 `content="abcabcabc"` → 修复后 `"abc"`。
- **SSE delta content 空串误置 latch**：OpenAI 风格 role-only 首帧（`content:""`）
  吞掉 latch，后续真正文被 `!gotAnyContent` 守卫静默丢弃。实测 `content=""` → 修复后
  正常输出。
- **tool_call 缺 index 塌缩**：缺 index 一律归 0，多调用场景下不同 call 被合并进同一
  槽（arguments 串联、name 互相覆盖）。改为「id 优先 / lastIdx 兜底 / 跳号分配」。
  实测 2 个调用塌成 1 个 → 修复后 2 个独立。
- **tool_calls 截断无保护**：原仅认 `finish_reason=="length"`，EOF 截断（无 `[DONE]`）
  的残缺 JSON 参数被原样下发，客户端解析非法 JSON 卡死会话。现覆盖两种截断来源。
- **session GC goroutine 关停竞态**：`StartGC` 的 goroutine 每轮无锁重读 `r.stop`，
  `StopGC` 持锁置 nil → 数据竞争（`-race` 复现）+ 关停失效（goroutine 泄漏）。
  改为启 goroutine 前捕获局部 channel。
- **限流重置时间仅认中文文案**：英文 `reset at <时间>` 形态（global 域）未解析，
  退回固定基数指数退避导致冷却翻倍。补齐英文正则。
- **usage 缺 `total_tokens` 不合成**：严格按 schema 校验的客户端收不到该字段。
- **`backfillReasoningContent` 门控不全**：补 `thinkingEnabled || hasTrace` 门控，
  并对 `reasoning` 字段做镜像归一化（部分租户校验 `len(reasoning)>0`，缺失/null/空串 400）。

### 新增（吸收上游）

- **`prompt_cache_key` 注入**（`cache_key.go`）：按账号隔离的稳定前缀缓存键，
  上游实测命中后费用降约 17×。经 `ChatStreamContextWithConversation` 接线到请求路径。
- **孤儿 tool_call↔tool 配对清理**（`tool_pairing.go`）：`repackToolResultBlocks`
  重排 + `cleanupOrphanToolCalls` 删孤儿，修复上游 code 11148「整条会话报废」。
  所有模型一律执行，独立于 sanitize 开关。
- **工具参数截断检测**（`truncation.go`）：`isTruncatedArguments` / `dropTruncatedToolCalls`。

### 验证

- `gofmt -l .` 空；`go build ./...` / `go vet ./...` / `go test -count=1 ./...` /
  `CGO_ENABLED=1 go test -race -count=1 ./...` 全通过。
- 7 项缺陷在修复前均以探针实证复现（RED），修复后全部转绿。
- 移植上游回归用例：`sse_upstream_test.go`、`sse_latch_test.go`、`backfill_reasoning_test.go`、
  `tool_pairing_test.go`、`truncation_test.go`、`cache_key_test.go`、`gc_stop_test.go`。
- Windows amd64 实机烟测：启动 → `/v1/intl/models` 正常 → `prompt_cache_key` 注入生效
  （`wb2a-<uid8>-<hex>`，同账号同会话稳定、跨账号不碰撞）→ 孤儿 tool_call 被清理。

### 未采纳（有意保留差异）

- 上游移除 `server.max_body_mb`（breaking）：我们仍保留该预拦截，需单独评估后再定。
- 上游 `models.dev` 四级查找链 / `global_models.go`：我们用自建 `intl_catalog.go`，
  且 `deepseek-v4.1-flash` 输出上限（384000）与上游知识表一致，无替换必要。
- 上游 `/v1/stats`、`auths/` 热加载、账号临时停用：属独立功能，另立版本。

## v1.1.1 — 2026-09-18

元数据修正补丁，不改任何请求链路与站点隔离行为：

- 国际站 `deepseek-v4.1-flash` 的 `max_output_tokens` 由 50000 修正为 384000（DeepSeek 官方 models.dev 目录值，亦为 28 家 provider 共识档）。
- 澄清两处同名不同部署的常量边界：CN 动态目录 `normalizeContextWindow` 的 50000 兜底保持不变（与腾讯官方 `product.internal.json` 对 `deepseek-v4-flash` 的声明一致），Intl 目录不经过该函数，两侧禁止互相套用。
- `max_output_tokens` 仅用于 `/v1/models` 与面板展示；网关从不在出站请求注入或钳制 `max_tokens`（唯一写入点是 `translateMaxCompletionTokens`，只搬运客户端显式给出的 `max_completion_tokens`，且显式 `max_tokens` 优先），因此该修正只影响读取此字段做自我裁剪的客户端。

### 验证

- `go build ./...` / `go vet ./...` / `go test ./...` / `go test -race ./...` 全通过。

## v1.1.0 — 2026-09-17

双通道增强版，继续保持路径级站点隔离：

- `/cn/v1/*` 只使用国内账号，`/intl/v1/*` 只使用国际账号；旧 `/v1/*` 兼容入口仍固定为 CN。
- 兼容 OpenAI `max_completion_tokens`，流式请求缺省补 `stream_options.include_usage`。
- Auth 请求级一致快照；token refresh 锁外网络、同账号串行和 generation 防陈旧覆盖；全套 race 测试通过。
- WAF 403 业务信封识别、CN/INTL 独立 IP 级 fail-fast、轮转指数退避与抖动。
- 未知错误连败降权，状态可观测、持久化、过期过滤、人工恢复和热配置完整接线。
- 面板新增磁盘配置热重载和受控重启按钮。
- 内置父子监督器：裸 Windows/FreeBSD/Linux 可自行拉起；Docker 与外部监督器也兼容。
- 保存/重载配置事务串行化，唯一临时文件原子替换，未知键保留，启动项按真实差异提示。
- 重启与热重载 API 要求 Bearer 鉴权；空 `api_key` 禁止远程重启。

### 验证

- `go build ./...`
- `go vet ./...`
- `go test ./...`
- `go test -race ./...`
- Windows amd64 实际烟测：启动 → 磁盘热重载 → API key 切换 → 202 重启 → 新 generation 恢复。
- FreeBSD amd64/arm64 与 Windows amd64 交叉构建。

### 兼容与运维

- 外部监督器模式可设置 `WB2A_DISABLE_SUPERVISOR=1`，此时面板重启会优雅退出，依赖 Docker/rc.d/Windows Service 拉起。
- 修改监听地址后，旧页面无法自动跨端口跳转；请访问新 `listen` 地址。
