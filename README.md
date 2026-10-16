# ccl

**[中文](#中文) · [English](#english)**

---

## 中文

`ccl` 是 [Claude Code](https://code.claude.com/) 的多模型启动器和本地协议代理：继续用 Claude Code 的界面，由 `ccl` 接入 OpenAI 兼容网关、Anthropic 网关或 GPT / Gemini / Grok / Kimi 等订阅账号，并在需要时自动做协议翻译。

### 安装

```bash
npm install -g @claudecodelaunch/ccl                 # 推荐
go install github.com/claude-code-launch/ccl@latest  # 或 Go
```

也可从 [Releases](https://github.com/claude-code-launch/ccl/releases) 下载对应平台的 `.gz`（macOS / Linux / Windows，x64 与 arm64），解压后放入 `PATH`，`SHA256SUMS` 校验的是 `.gz`。npm 安装请勿加 `--omit=optional`，否则缺少平台二进制。

```bash
ccl version
ccl doctor     # 只读体检；加 --probe 才会逐个测模型（计费）
```

需要已安装 Claude Code（`claude --version`）。未安装时，在终端运行 `ccl` 会询问是否自动安装。

### 快速开始

```bash
# A. 订阅账号（任选其一；加别名可多账号：ccl oauth gpt work）
ccl oauth gpt | gemini | grok | qoder | kimi | kiro | workbuddy | autoclaw | zed

# B. API Key / 第三方网关：单页 TUI，填 Endpoint 与 Key → Auto Configure → Save & Activate
ccl set deepseek

ccl            # 用当前 provider 启动 Claude Code
```

新安装默认 provider 加载为 off：尚未激活 provider 时，`ccl` 直接以 Claude Code 自身登录启动；`ccl oauth`、`ccl set` 选择 Save & Activate 或 `ccl use <name>` 激活 provider 后自动切换为 on。

未知命令和参数会原样透传给 Claude Code（`ccl resume`、`ccl -p "hi"`），并启动真实会话。

### 常用命令

| 命令 | 作用 |
|---|---|
| `ccl set [name]` | 新增 / 修改 provider（TUI） |
| `ccl ls [-a]` / `ccl use <name>` | 列出 / 切换 provider |
| `ccl map [--opus/--sonnet/--haiku/--fable/--custom/--subagent <model>]` | 映射模型槽位；`ccl map auto` 自动填充 |
| `ccl oauth <name> [alias]` / `ccl oauth prune` | 订阅登录 / 清理未使用凭据 |
| `ccl provider cp\|mv\|rm\|models\|preview\|env\|effort\|ultracode` | provider 管理；`preview` 查看注入的 settings |
| `ccl provider on\|off` | 是否加载 provider（off = Claude Code 自身登录；新安装默认 off） |
| `ccl proxy -p <name>` / `status` / `stop` | 后台代理，让直接运行的 `claude` 使用指定 provider |
| `ccl acp [install]` | Agent Client Protocol（Xcode 27 等） |
| `ccl cloud login\|push\|pull\|status\|key\|device\|remote` | 端到端加密云同步（iCloud / Google Drive） |
| `ccl bypass [on\|off]` | 启动时附加 `--dangerously-skip-permissions`，仅在可信环境使用 |
| `ccl log [on\|off] [--level debug]` | 会话日志（默认关闭，写入 `~/.ccl/logs/`） |
| `ccl lang [zh\|zh-TW\|en]` | 界面语言（`CCL_LANG` > 配置 > 系统） |
| `ccl doctor` / `update` / `version` / `completion` | 诊断 / 升级 / 版本 / 补全 |

每个子命令都支持 `--help`。根级 `cp/mv/env/preview/models` 与 `login/push/pull/...` 是已隐藏的废弃别名，请用 `ccl provider ...` / `ccl cloud ...`。

### 后台代理

```bash
ccl proxy -p deepseek   # 改写 ~/.claude/settings.json（支持 CLAUDE_CONFIG_DIR）
claude                  # 任意终端直接运行，共用该代理
ccl proxy stop          # 逐字段恢复原配置
```

- 修改 provider（`set` / `map` / `provider env`）后需重新 `ccl proxy start -p <name>`；切换或停止后重启已打开的 Claude 会话。
- `ccl use` 只影响 `ccl` 启动，不会切换运行中的代理。
- 异常退出后，下次运行任意 `ccl` 命令会按 `~/.ccl/proxy/state.json` 自动恢复；启动诊断见 `~/.ccl/proxy/proxy.log`。

### Xcode 27 / ACP

```bash
ccl acp install        # 需 Node.js >= 22；装到 ~/.ccl/acp-adapter
ccl use --acp <name>   # 可选：为 ACP 单独固定 provider；--follow 恢复跟随
```

Xcode → Settings → Intelligence → Agents → Add an Agent：`Executable` 填 `ccl` 的**绝对路径**，`Arguments` 填 `acp`。ACP 协议与 Agent 行为由上游 [claude-agent-acp](https://github.com/agentclientprotocol/claude-agent-acp) 实现，ccl 只注入 provider / 模型 / 环境；运行中修改配置会在下一条空闲 prompt 前生效。`--adapter` 或 `CCL_ACP_ADAPTER` 可指定适配器路径。`ccl bypass` 不适用于 ACP。

### 协议与接入

Claude Code 始终说 Anthropic Messages。Anthropic API-key 网关在普通启动时直连；其余在本机 `127.0.0.1` 起 loopback runtime（随机会话 key，不落盘）完成转换，不启动任何外部 provider 进程。

| 来源 | 协议转换 |
|---|---|
| Anthropic 网关 | 直连（`ccl proxy` 下本地透传） |
| OpenAI Chat 网关 / Kimi / WorkBuddy / Gemini | Messages ↔ Chat Completions |
| Codex Responses 网关 / GPT / Grok | Messages ↔ Responses（CCL 统一注入 Codex 身份头） |
| Zed | 按模型分派 Anthropic / OpenAI / xAI / Google |
| Kiro / Qoder | 各自的直接适配器（Amazon Q / COSY） |
| AutoClaw | Messages → OpenAI Chat（managed proxy） |

429 / 5xx 会快速重试两次（500ms、1s），仍失败则透传上游状态；401 由各后端刷新 OAuth 凭据。

### 配置

- `~/.ccl/config.yaml`：`active_provider`、`acp_provider`（空 = 跟随）、`provider_off`、`lang`、`bypass_mode`、`log_level`、`providers`。**含明文 API Key，勿提交或外传。**
- `~/.ccl/auth/*.json`：OAuth 凭据（0600）。
- `provider.type`：`openai`（Chat）/ `openai_responses` / `anthropic`，由用户显式选择，不按 URL 猜测；Anthropic 直连的 `endpoint` 用裸域名（不带 `/v1`），Bearer 鉴权设 `anthropicAuth: bearer`。
- `--settings` 优先级高于用户设置，因此 `outputStyle` / `language` / `statusLine` / `modelPicker` 仅在你自己没设置时由 ccl 提供。启动时会丢弃 shell 里继承的 `CLAUDE_CODE_USE_*`、`ANTHROPIC_*MODEL` 等变量；需要的值写进 provider（`ccl map` / `ccl provider env`）。
- 上下文预设：Default / Balanced 500K / Balanced 800K（在 80% 自动压缩）。

### 云同步

`ccl cloud login`（默认 Google Drive；macOS 也可 `ccl cloud login icloud`）之后 `push` / `pull`。没有浏览器的环境（SSH、Cloud Shell、无图形界面的 Linux）会自动改用设备码登录：终端显示二维码和短代码，在手机或电脑上打开 `google.com/device` 输入即可；`--device` / `--device=false` 可强制选择。同步 `config.yaml` 与 `auth/*.json`，gzip 后用 AES-256-GCM 加密；Google Drive 仅申请 `drive.appdata` 权限。新设备用 `ccl cloud key export/import` 的离线恢复密钥，或 `ccl cloud device request/approve/complete` 配对。恢复密钥丢失且无授权设备时无法解密。冲突时 push / pull 会停止，不会静默覆盖。

### 开发

```bash
gofmt -l .            # 必须为空
go vet ./... && go test -race ./...
go build -o /tmp/ccl-verify .
node --test scripts/npm-packages.test.js scripts/acp-adapter.test.mjs
```

- 不要把 `ccl` 当测试工具直接跑：未知参数会启动计费的 Claude 会话；用 `go test` 与 `--help`，并设置临时 `HOME` / `CLAUDE_CONFIG_DIR`。
- 架构与约定见 [CLAUDE.md](CLAUDE.md)；协议说明见 `internal/oauthproxy/doc.go`；npm 发布见 [docs/npm-release.md](docs/npm-release.md)；云同步设计见 [docs/cloud-sync-v2-design.md](docs/cloud-sync-v2-design.md)。
- 推送 `v*` tag 触发 CI 构建 6 个平台二进制并发布到 GitHub Releases 与 npm。

### 许可

MIT。第三方参考实现见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。

---

## English

`ccl` is a multi-model launcher and local protocol proxy for [Claude Code](https://code.claude.com/). Keep Claude Code's UI; `ccl` connects it to OpenAI-compatible gateways, Anthropic gateways, or subscription accounts (GPT / Gemini / Grok / Kimi, …), translating protocols when needed.

### Install

```bash
npm install -g @claudecodelaunch/ccl                 # recommended
go install github.com/claude-code-launch/ccl@latest  # or Go
```

Or download the `.gz` for your platform from [Releases](https://github.com/claude-code-launch/ccl/releases) (macOS / Linux / Windows, x64 and arm64), decompress it onto your `PATH`; `SHA256SUMS` covers the `.gz` files. Do not install via npm with `--omit=optional`, or the platform binary will be missing.

```bash
ccl version
ccl doctor     # read-only checks; --probe tests each model (billed)
```

Claude Code must be installed (`claude --version`). If it is missing, running `ccl` in a terminal offers to install it.

### Quick start

```bash
# A. Subscription login (pick one; add an alias for multiple accounts: ccl oauth gpt work)
ccl oauth gpt | gemini | grok | qoder | kimi | kiro | workbuddy | autoclaw | zed

# B. API key / third-party gateway: single-page TUI — Endpoint + Key → Auto Configure → Save & Activate
ccl set deepseek

ccl            # launch Claude Code with the active provider
```

On a fresh install provider loading is off: until a provider is activated, `ccl` starts Claude Code on its own login. `ccl oauth`, Save & Activate in `ccl set`, or `ccl use <name>` activates one and turns loading on.

Unknown commands and flags are forwarded verbatim to Claude Code (`ccl resume`, `ccl -p "hi"`) and start a real session.

### Commands

| Command | Purpose |
|---|---|
| `ccl set [name]` | Add / edit a provider (TUI) |
| `ccl ls [-a]` / `ccl use <name>` | List / switch providers |
| `ccl map [--opus/--sonnet/--haiku/--fable/--custom/--subagent <model>]` | Map model slots; `ccl map auto` fills them |
| `ccl oauth <name> [alias]` / `ccl oauth prune` | Subscription login / remove unused credentials |
| `ccl provider cp\|mv\|rm\|models\|preview\|env\|effort\|ultracode` | Provider management; `preview` shows the injected settings |
| `ccl provider on\|off` | Load the provider or not (off = Claude Code's own login; off by default on a fresh install) |
| `ccl proxy -p <name>` / `status` / `stop` | Background proxy so a plain `claude` uses the chosen provider |
| `ccl acp [install]` | Agent Client Protocol (Xcode 27, …) |
| `ccl cloud login\|push\|pull\|status\|key\|device\|remote` | End-to-end encrypted sync (iCloud / Google Drive) |
| `ccl bypass [on\|off]` | Add `--dangerously-skip-permissions` on launch; trusted environments only |
| `ccl log [on\|off] [--level debug]` | Per-session logs (off by default, in `~/.ccl/logs/`) |
| `ccl lang [zh\|zh-TW\|en]` | UI language (`CCL_LANG` > config > system) |
| `ccl doctor` / `update` / `version` / `completion` | Diagnose / upgrade / version / completions |

Every subcommand supports `--help`. Root-level `cp/mv/env/preview/models` and `login/push/pull/...` are hidden deprecated aliases; use `ccl provider ...` / `ccl cloud ...`.

### Background proxy

```bash
ccl proxy -p deepseek   # rewrites ~/.claude/settings.json (honors CLAUDE_CONFIG_DIR)
claude                  # run it from any terminal; they share the proxy
ccl proxy stop          # restores your original settings field by field
```

- After changing a provider (`set` / `map` / `provider env`), run `ccl proxy start -p <name>` again; restart open Claude sessions after switching or stopping.
- `ccl use` only affects `ccl` launches, not a running proxy.
- After a crash, the next `ccl` command restores settings from `~/.ccl/proxy/state.json`; startup diagnostics are in `~/.ccl/proxy/proxy.log`.

### Xcode 27 / ACP

```bash
ccl acp install        # needs Node.js >= 22; installs to ~/.ccl/acp-adapter
ccl use --acp <name>   # optional: pin a provider for ACP; --follow unpins
```

Xcode → Settings → Intelligence → Agents → Add an Agent: set `Executable` to the **absolute path** of `ccl` and `Arguments` to `acp`. The ACP protocol and agent behavior come from upstream [claude-agent-acp](https://github.com/agentclientprotocol/claude-agent-acp); ccl only injects provider / model / environment. Config changes apply before the next idle prompt. Use `--adapter` or `CCL_ACP_ADAPTER` to point at the adapter. `ccl bypass` does not apply to ACP.

### Protocols

Claude Code always speaks Anthropic Messages. Anthropic API-key gateways are called directly on normal launches; everything else runs through a loopback runtime on `127.0.0.1` (random per-session key, never persisted). No external provider process is started.

| Source | Translation |
|---|---|
| Anthropic gateway | Direct (local passthrough under `ccl proxy`) |
| OpenAI Chat gateway / Kimi / WorkBuddy / Gemini | Messages ↔ Chat Completions |
| Codex Responses gateway / GPT / Grok | Messages ↔ Responses (ccl injects the Codex identity headers) |
| Zed | Per-model dispatch: Anthropic / OpenAI / xAI / Google |
| Kiro / Qoder | Dedicated adapters (Amazon Q / COSY) |
| AutoClaw | Messages → OpenAI Chat (managed proxy) |

429 / 5xx are retried twice quickly (500 ms, 1 s), then the upstream status is relayed; 401s are handled by each backend's OAuth refresh.

### Configuration

- `~/.ccl/config.yaml`: `active_provider`, `acp_provider` (empty = follow), `provider_off`, `lang`, `bypass_mode`, `log_level`, `providers`. **Contains plaintext API keys — never commit or share it.**
- `~/.ccl/auth/*.json`: OAuth credentials (0600).
- `provider.type`: `openai` (Chat) / `openai_responses` / `anthropic`, chosen explicitly, never guessed from the URL. For direct Anthropic gateways use a bare-domain `endpoint` (no `/v1`) and set `anthropicAuth: bearer` for Bearer auth.
- `--settings` outranks user settings, so `outputStyle` / `language` / `statusLine` / `modelPicker` are provided by ccl only when you have not set them yourself. Inherited shell variables such as `CLAUDE_CODE_USE_*` and `ANTHROPIC_*MODEL` are dropped at launch; put needed values in the provider (`ccl map` / `ccl provider env`).
- Context presets: Default / Balanced 500K / Balanced 800K (auto-compact at 80%).

### Cloud sync

`ccl cloud login` (Google Drive by default; `ccl cloud login icloud` on macOS), then `push` / `pull`. Without a browser (SSH, Cloud Shell, Linux without a display) Google Drive login uses the device flow: the terminal shows a QR code and a short code to enter at `google.com/device` on a phone or another computer; `--device` / `--device=false` forces either way. It syncs `config.yaml` and `auth/*.json`, gzip-compressed and encrypted with AES-256-GCM; Google Drive uses only the `drive.appdata` scope. Add a new device with the offline recovery key (`ccl cloud key export/import`) or by pairing (`ccl cloud device request/approve/complete`). If the recovery key is lost and no authorized device remains, data cannot be decrypted. On conflicts push / pull stop instead of overwriting.

### Development

```bash
gofmt -l .            # must be empty
go vet ./... && go test -race ./...
go build -o /tmp/ccl-verify .
node --test scripts/npm-packages.test.js scripts/acp-adapter.test.mjs
```

- Do not use `ccl` itself as a test harness: unknown args start a billed Claude session. Use `go test` and `--help` with a throwaway `HOME` / `CLAUDE_CONFIG_DIR`.
- Architecture and conventions: [CLAUDE.md](CLAUDE.md); protocol notes: `internal/oauthproxy/doc.go`; npm release: [docs/npm-release.md](docs/npm-release.md); cloud sync design: [docs/cloud-sync-v2-design.md](docs/cloud-sync-v2-design.md).
- Pushing a `v*` tag makes CI build six platform binaries and publish to GitHub Releases and npm.

### License

MIT. Third-party references: [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
