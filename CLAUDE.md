# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

`ccl`（Claude Code 多网关启动器）是一个 Go CLI：用户继续用 Claude Code 的界面，`ccl` 负责接入不同的模型来源（OpenAI 兼容网关、ChatGPT/Gemini/Grok/Copilot/Kimi/Qoder/Kiro 订阅账号等），需要时自动做协议翻译。用户配置在 `~/.ccl/config.yaml`（明文 API key），OAuth 凭据在 `~/.ccl/auth/`（每账号一个 JSON，0600 权限）。

## 常用命令

```bash
go build -o ccl .                 # 编译（README 建议 -o /tmp/ccl-debug 做本地验证）
go test ./...                     # 全部测试（CI 用 go test -race ./...）
go vet ./...                      # CI 会跑
gofmt -l .                        # CI 检查格式（必须零输出）
go test ./internal/oauthproxy     # 单个包
go test ./cmd -run TestVersionFlagIsHandledByCclNotForwarded   # 单个测试
```

升级 `github.com/router-for-me/CLIProxyAPI/v7` 后必须跑 `go test ./internal/oauthproxy ./internal/claude ./cmd`（见 `internal/oauthproxy/doc.go` 的兼容性边界清单）。

CI（`.github/workflows/ci.yml`）依次执行：gofmt 检查 → `go test -race ./...` → `go vet ./...` → `go build ./...`。推 `v*` tag 触发多平台发布（`.github/workflows/release.yml`）：构建各平台二进制 + GitHub Release，并用 OIDC 可信发布 npm 包 `@claudecodelaunch/ccl`（`bin/wrapper.js` 只是二进制包装器）。tag 必须是 `vMAJOR.MINOR.PATCH`（兼容旧四段式），npm 版本号取 tag 最后一段。

## 架构

数据流：`cmd/`（cobra 命令）→ `internal/claude`（拉起 Claude Code 进程 + 生成每会话 settings JSON）→ `internal/oauthproxy`（本机代理运行时）。`main.go` 只是 `cmd.Execute()`。

### 两条启动路径（`cmd/root.go` 的 `Execute`）

- 第一个参数是已知 ccl 命令 → 走 cobra 命令树。
- **否则所有参数原样透传给 Claude Code**（`ccl resume`、`ccl -p "..."` 都走这里）。`isCclCommand` 是显式白名单；新增子命令注册到 cobra 后自动被识别，但 `-v` 故意不拦截（属于 Claude Code）。**任何参数拼错都会启动真实计费会话** — 测试/验证时永远用 `--help` 或正确的子命令，别把参数当字符串传给 `./ccl`。
- `resolveProvider`：config.yaml 的 `active_provider` 优先于环境变量；无配置时才回退 `ANTHROPIC_*` / `OPENAI_*` 环境变量。

### 会话启动（`internal/claude/launcher.go`）

每个会话的核心流程（`Run`）：
1. `setupProvider`：OpenAI 系 provider 和所有 OAuth backend 启动内嵌 `oauthproxy.Runtime`（本机 127.0.0.1 + 每会话随机 API key，不写回配置）；纯 Anthropic 直连 provider 不启动代理。
2. `buildEnv` 生成环境变量（`ANTHROPIC_*_MODEL` 槽位映射、`ANTHROPIC_CUSTOM_MODEL_OPTION`、运行时默认 `CLAUDE_CODE_MAX_OUTPUT_TOKENS=32000`、`ENABLE_TOOL_SEARCH=false`、`CLAUDE_CODE_SUBAGENT_MODEL` 默认 Custom/Sonnet 等）。
3. 写每会话 settings 临时文件（`/tmp/claude_<id>_settings.json`，O_EXCL + 0600，`--settings` 传给 claude CLI）。
4. `buildProcessEnv` 抑制继承的、由 settings 文件权威管理的环境变量（防止 shell 里残留的 `ANTHROPIC_API_KEY` 覆盖代理传输值）。
5. `cmd.Run()` 结束后打印 token 用量摘要和会话日志路径。

Provider 槽位模型（`internal/provider/provider.go`）：`OpusModel`/`SonnetModel`/`HaikuModel`/`CustomModelID`/`SubagentModel` 对应 Claude Code 的 `ANTHROPIC_DEFAULT_*_MODEL` 环境变量；`Type` 是稳定的机器可读值（`anthropic` / `openai` / `openai_responses`），显示标签（`openai(chat)` / `openai(responses)`）由 `ProtocolLabel` 派生。`OAuthProvider` 值决定 runtime backend：`gpt|gemini|grok|kimi` 走 CLIProxyAPI，`copilot`/`qoder`/`kiro` 是 ccl 直连适配器（不调用任何第三方 CLI 子进程）。`AuthGroup` 是同 backend 多账号 token 池，`config.Load` 时把组成员水合进 `OAuthAccountCredentials`（仅运行时字段）。

模型名落地（`internal/modelrouting` 的 `MapModel`，被 launcher / runtime / provider 共用）：单个 configuredModel 完全覆盖映射；逗号分隔时是唯一候选池；否则用上游可用模型列表，按精确名 → tier 启发（opus/sonnet/haiku）→ 池首项匹配。空候选池返回 `""`，调用方不得发明厂商默认模型名。

### 内嵌运行时（`internal/oauthproxy/`）

Claude Code 只面对本机 Anthropic Messages 端点；上游协议在包内翻译：
- **CLIProxyAPI**（`runtime.go`）承载 openai(chat) / openai(responses) 和大部分 OAuth backend。
- **responses_compat.go**：Responses 上游前的兼容层（completed-only 流重写、message_start 时序、剥离 Codex 头）。这是对 SDK 缺陷的刻意 workaround — 升级 CLIProxyAPI 版本时按 `doc.go` 的清单回归。
- **Copilot**（`copilot_runtime.go`）：GitHub OAuth + 账号实时模型目录，按每个模型声明的端点选择 Responses/Chat/Messages。
- **Qoder**（`qoder_*.go`）：COSY 签名、WAF body 编码、SSE→Anthropic 转换全部内嵌。
- **Kiro**（`kiro_*.go`）：Messages→Amazon Q 适配器 + AWS EventStream 解码 + Smithy RPCv2 CBOR 模型目录。
- **codex_cooldown.go**：ccl 缩短 408/5xx→2s、401/429→10s 的冷却覆盖。

### 云同步（`internal/cloudsync`）

`ccl cloud login|push|pull` 的实现：把 `~/.ccl` 配置加密后同步到多个 Remote（Google Drive OAuth，或 iCloud 本地同步目录）。三层模型——**Profile**（一套配置 + 一把 256-bit 主密钥）、**Remote**（具体网盘账号/目录，一个 Profile 可镜像到多个）、**Device**（持密钥的设备，支持配对传钥）。默认操作只作用于 primary remote，跨网盘必须显式 `--all` / `--to` / `--from`；`logout` 只断本地连接，不删远端密文和主密钥。主密钥存系统 keychain（`keychain_darwin.go`），云端永不保存明文密钥或配置。设计约定与迁移规则见 `docs/cloud-sync-v2-design.md`（Phase 5 密钥轮换尚未实现）。

### 上下文管理

ccl 不再声明上下文大小：`CLAUDE_CODE_MAX_CONTEXT_TOKENS` / `CLAUDE_CODE_AUTO_COMPACT_WINDOW` 只在 provider 明确 `CCL_CONTEXT_BUDGET=manual` 时生效；旧版 context preset（300K/500K/1M 组合）在 `IsCclContextPreset` 识别后被丢弃。这些 key 同时写入 settings 文件并导出到子进程环境（settings 文件通道对它们不可靠）。

## 测试注意事项

- 测试用 `t.Setenv("HOME", t.TempDir())` 隔离，不碰真实 `~/.ccl`。仓库里有 `CCL_TEST_HOME` 的说法但只存在于 README 的手动验证示例中，Go 测试不用它。
- 命令测试多为源码级（直接调 cobra 命令的 RunE 或 helper），不是 exec 子进程。
- 手动验证命令时（`./ccl ...`）：未知参数会透传启动真实 Claude Code 计费会话；裸 `ccl lang` / `ccl set` / `ccl map` 是交互式 TUI，非终端下行为不同。只跑 `--help` / `ccl doctor` / `ccl preview` 这类只读命令，或设置 `HOME` 到临时目录。**不要**对用户真实配置执行 `map auto`、`set`、`oauth sync` 等写操作。
- `~/.ccl/config.yaml` 含明文 API key，不要把配置内容整段回显到输出。

## 其他约定

- 中文是项目的一等语言：README、TUI 文案、注释混用中英文；`internal/locale/` 处理语言检测（`CCL_LANG` 环境变量 > config.yaml > 系统语言）。新增面向用户的字符串通常要双语文案。
- 旧命令兼容别名（`ccl auth` → `oauth`，`ccl login/push/pull` → `cloud` 系，`bypass` 取代 `auto`）在 `cmd/root.go` 和各命令里维护，改动别名时保持旧脚本可用。
- 配置文件读写是原子的（`writeFileAtomic`，临时文件 + rename + 0600）。
