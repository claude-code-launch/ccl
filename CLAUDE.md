# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working in this repository.

## 常用开发命令

仓库是 Go 1.26 CLI，源码入口为 `main.go`；`package.json` 只是发布 npm wrapper，不是主构建系统。

- 构建本地验证二进制：`go build -o /tmp/ccl-verify .`
- 构建全部 Go package（CI）：`go build ./...`
- 运行全部测试：`go test ./...`
- 运行 CI 等价的竞态测试：`go test -race ./...`
- 运行单个测试：`go test ./cmd -run TestRunAuthGrokWithoutAliasDerivesName`
- 按包运行回归测试：`go test ./internal/oauthproxy ./internal/claude ./cmd`
- 检查格式：`gofmt -l .`；输出必须为空（也可用 `gofmt -w <files>` 修复）
- 静态检查：`go vet ./...`
- CI 顺序见 `.github/workflows/ci.yml`：gofmt → `go test -race ./...` → vet → build。

需要隔离本地配置时使用临时 `HOME`，避免读写真实的 `~/.ccl`：

```bash
export CCL_TEST_HOME="$(mktemp -d)"
HOME="$CCL_TEST_HOME" /tmp/ccl-verify preview
HOME="$CCL_TEST_HOME" /tmp/ccl-verify doctor
HOME="$CCL_TEST_HOME" /tmp/ccl-verify models --all
```

## 高层架构

数据流是 `main.go` → `cmd`（Cobra 命令）→ provider/config 解析 → `internal/providersession`（决定运行时形态）→ `internal/claude`（生成会话 settings 并拉起 Claude Code）→ 必要时进入 `internal/oauthproxy` 的本机 loopback runtime。

### 命令分派与配置

- `cmd/root.go` 的 `cmd.Execute` 先判断首个参数是否为 ccl 自己注册的命令。未知首个参数会被当作 Claude Code 参数透传；字面量首参 `claude` 会被去掉后再透传。因此测试命令分派时不要随意运行会启动真实 Claude Code 会话的参数。
- `cmd/` 负责命令语义、provider 管理、OAuth/cloud 命令和 Bubble Tea TUI；协议转换与凭据刷新应放在 `internal` 包，不要堆入 Cobra handler。
- `internal/config` 读写 `~/.ccl/config.yaml`，兼容迁移旧 `~/.cc/config.yaml`，读取时迁移旧字段，并以原子写入和 `0600` 权限保存。
- `internal/provider.Provider` 是持久化配置模型：包含 protocol/type、endpoint、模型池、Opus/Sonnet/Haiku/Custom/Subagent 槽位、OAuth provider/credential 绑定和 provider 级环境变量。`internal/providersession.Session` 使用它的副本；会话临时 endpoint、随机 key 和模型目录不能写回配置。
- `cmd/root.go` 中已有 active provider 时配置优先于环境变量；只有没有 active provider 时才回退到 `ANTHROPIC_*` / `OPENAI_*` 环境变量。

### Claude Code 启动链路

`internal/claude/launcher.go` 是启动边界：

1. 调用 `providersession.Prepare`，必要时发现模型并启动本机 runtime。
2. 按 provider 槽位和模型目录生成临时 `settings.json`，把 endpoint、鉴权、模型 alias、context/compact 和 Claude Code runtime 环境变量写入其中。
3. 清理会与 settings 冲突的继承环境变量；对 embedded proxy 强制使用本次会话的 loopback URL/key。
4. 执行外部 `claude --settings <temp-file> ...`，退出后停止 runtime 并删除临时文件。

`internal/modelrouting` 负责从逗号分隔模型池启发式映射 Opus/Sonnet/Haiku；`internal/protocol` 负责 endpoint 规范化、OpenAI/Anthropic `/models` 探测和模型元数据读取。`[1m]` 是按槽位的扩展上下文标记；provider 级 context preset 由 `internal/claude/context_budget.go` 统一应用。

### Provider 与协议运行时边界

Claude Code 始终以 Anthropic Messages 请求进入。`internal/providersession` 根据 `Provider.Type` 与 `OAuthProvider` 决定直连或 loopback proxy，最终所有需要代理的运行时都只暴露本机 Anthropic Messages 入口。

- 普通 Anthropic API-key provider：Claude Code 直连上游，绕过 `oauthproxy`。
- 手动 OpenAI Chat provider：由 embedded CLIProxyAPI SDK 完成 Messages ↔ Chat Completions 转换。
- 手动 OpenAI Responses provider：由 CCL 自研 Codex Responses runtime 完成 Messages ↔ Responses、Codex headers/metadata、OAuth/API-key 鉴权、SSE 和错误/usage 处理；不要重新接回 CPA 的 Codex executor。
- `gpt` / Codex OAuth：登录入口可用 CPA authenticator，但运行时由 CCL 的 Codex Responses 实现读取所选 credential、401 后刷新一次并重试。
- `gemini`、`grok`、`kimi`、订阅 `claude`：使用 embedded CLIProxyAPI 对应 authenticator/executor。
- `copilot`：CCL 自己做 GitHub device flow、token exchange 和权威模型目录；按模型声明的 endpoint 路由，Responses 走 CCL Codex 转换，Chat/native Messages 走 CPA compatibility child。
- `qoder`：CCL 直接处理浏览器 OAuth、refresh、COSY/WAF 编码、模型发现和 Qoder SSE → Anthropic Messages；不调用或探测 `qodercli`。
- `kiro`：CCL 直接处理 Portal PKCE/Builder ID、credential refresh、模型目录、Messages → Amazon Q、限流轮换/重试和 AWS EventStream → Messages。

`internal/oauthproxy/doc.go` 记录了 CLIProxyAPI 兼容边界。尤其要保持：runtime 只绑定 `127.0.0.1` 并使用每会话随机 key；`Runtime.Stop` 先取消并等待 `Service.Run`，超时后才强制 Shutdown；嵌套 runtime 的 stdout/logging 隔离和 CPA 全局模型注册清理不能被破坏。Codex、Copilot、Qoder、Kiro 的 direct data plane 也有各自的流式/错误恢复约束。

### OAuth、模型和 cloud sync

- OAuth 凭据保存在 `~/.ccl/auth/*.json`；provider 通过 `oauthAccountCredential` 精确绑定一个文件，避免多账号共享 refresh token 或模型状态。`internal/oauthproxy/auth.go` 负责公共登录/后端归一化，provider-specific `*_auth.go` / `*_credentials.go` 负责各自凭据。
- `internal/codexidentity` 集中维护 Codex client version、Originator、User-Agent 和 turn headers；修改 Responses wire identity 时从这里检查，而不是在多个 runtime 中复制常量。
- `internal/cloudsync` 是独立的加密配置同步层，由 `cmd/cloud_sync.go` 调用；它处理 iCloud/Google Drive 多 remote、AES-GCM 压缩快照、冲突检测、恢复密钥和设备配对。同步范围主要是 `~/.ccl/config.yaml` 与 `~/.ccl/auth/*.json`，本地 key/token 不上传。

## 测试与升级回归

测试与实现同包放置，重点覆盖 `cmd`、`internal/claude`、`internal/providersession`、`internal/protocol`、`internal/oauthproxy` 和 `internal/cloudsync`。协议 adapter 的 converter、SSE、错误映射、credential rotation 和 runtime 生命周期通常有独立测试；改动这些边界时优先运行对应 package 的测试，再运行 `go test -race ./...`。

升级 `github.com/router-for-me/CLIProxyAPI/v7` 后至少运行：

```bash
go test ./internal/oauthproxy ./internal/claude ./cmd
```

并回归手动验证 `ccl oauth gpt`、`ccl oauth copilot`、`ccl oauth qoder`、`ccl oauth kiro`、一个 `openai_responses` API-key provider，以及一个 `openai(chat)` provider 的 streaming/tool calls。