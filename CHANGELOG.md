# Changelog

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
