# Changelog

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
