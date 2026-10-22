# ccl

**[中文](#中文) · [English](#english)**

---

## 中文

`ccl` 是 [Claude Code](https://code.claude.com/) 的多模型启动器和本地协议代理：继续用 Claude Code 的界面，由 `ccl` 接入 OpenAI 兼容网关、Anthropic 网关或 GPT / Gemini / Grok / Kimi 等订阅账号，并在需要时自动做协议翻译。

### 安装

```bash
npm install -g @claudecodelaunch/ccl                 # 推荐，需 Node.js >= 22
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
ccl oauth gpt | gemini | grok | qoder | kimi | kiro | workbuddy | workbuddycn | autoclaw | zed

# B. API Key / 第三方网关：单页 TUI，填 Endpoint 与 Key → Auto Configure → Save & Activate
ccl set deepseek

ccl            # 用当前 provider 启动 Claude Code
```

新安装默认使用 `default`，即 Claude Code 自身配置（claude.ai 订阅与 `~/.claude` 设置）：`ccl` 与 `ccl acp` 都直接以它启动。`ccl oauth`、`ccl set` 选择 Save & Activate 或 `ccl use <name>` 切换到 provider，`ccl use default` / `ccl use --acp default` 切回。`ccl proxy` 运行时，`default` 看到的就是代理写入的配置。

未知命令和参数会原样透传给 Claude Code（`ccl resume`、`ccl -p "hi"`），并启动真实会话。

### 常用命令

| 命令 | 作用 |
|---|---|
| `ccl set [name]` | 新增 / 修改 provider（TUI） |
| `ccl ls [-a]` / `ccl use <name>` | 列出 / 切换 provider；`ccl use default` = Claude Code 自身配置 |
| `ccl map [--opus/--sonnet/--haiku/--fable/--custom/--subagent <model>]` | 映射模型槽位；`ccl map auto` 自动填充 |
| `ccl oauth <name> [alias]` / `ccl oauth prune` | 订阅登录 / 清理未使用凭据 |
| `ccl provider cp\|mv\|rm\|models\|preview\|env\|effort\|ultracode` | provider 管理；`preview` 查看注入的 settings |
| `ccl proxy -p <name>` / `status` / `stop` | 后台代理，让直接运行的 `claude` 使用指定 provider |
| `ccl acp [install]` | Agent Client Protocol（Xcode 27 等） |
| `ccl web [--port 8099]` | 浏览器里使用 ccl 会话（HTTP + SSE；默认只监听回环地址） |
| `ccl cloud login\|push\|pull\|status\|remote` | 加密云同步（iCloud / Google Drive） |
| `ccl bypass [on\|off]` | 启动时附加 `--dangerously-skip-permissions`，仅在可信环境使用 |
| `ccl sign [on\|off] [--default]` | 提交 / PR 的 Claude 署名（默认关闭）；`--default` 同时改 `~/.claude/settings.json` |
| `ccl log [on\|off] [--level debug]` | 会话日志（默认关闭，写入 `~/.ccl/logs/`） |
| `ccl lang [zh\|zh-TW\|en]` | 界面语言（`CCL_LANG` > 配置 > 系统） |
| `ccl doctor` / `update` / `version` / `completion` | 诊断 / 升级 / 版本 / 补全 |

每个子命令都支持 `--help`。

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

### 浏览器（ccl web）

```bash
ccl web                    # http://127.0.0.1:8099，自动打开浏览器，不需要 token
ccl web --host 0.0.0.0 --no-open --token "$(openssl rand -hex 24)"   # 容器
```

一个对话就是一个 ccl 终端实例：它在 `--workspace` 目录读写、运行工具，每个回合起一个新的 Claude Code 进程并用 `--resume` 续接同一会话，所以 provider / 模型 / effort 在回合边界生效，停止就是结束进程。历史存放在 `~/.ccl/web/sessions/<id>/`。页面布局参照 claude.ai/code：左侧会话列表，中间消息流，右侧环境/用量/连接器面板，外加居中的设置弹窗（通用、Providers、订阅登录、连接器；新建和编辑 provider 在单独的弹窗里填写 endpoint、API key、模型槽位、effort）。输入框的 ＋ 菜单可以添加文件或照片（⌘U，也支持拖放和粘贴；图片直接发给模型，其他文件以路径交给 Claude Code）、选择斜杠命令、查看已加载的 MCP 连接器。界面语言跟随 `ccl lang`。此外：工具调用可展开查看 diff、待办和计划；需要确认的工具会弹出允许/拒绝卡片（权限模式可选逐次询问、自动接受编辑、计划模式或跳过，默认跟随 `ccl bypass`）；右侧可浏览和编辑工作区文件、做 git 暂存/提交/切换分支；能导入本工作区已有的 Claude Code 会话、定时发送消息、导出 Markdown/HTML；设置里可以用订阅登录（`ccl oauth`）和管理 MCP 连接器。页面带网页终端（默认开启，`--no-shell` 关闭）。设置弹窗通过 `/api/config` 写 `~/.ccl/config.yaml`，同样走文件锁；正在运行的回合使用它启动时的配置快照。工作区（项目）：默认是当前目录，`--workspace` 可以重复传入，`--workspace-root <dir>` 会把目录下每个子目录都列为工作区（新克隆的仓库几秒内出现）；侧栏顶部切换工作区，也可以“新建项目”或“打开其他文件夹”，添加的文件夹保存在 `~/.ccl/web/workspaces.json`，`--lock-workspaces` 禁止从页面添加。每个对话固定在它创建时的工作区里运行。绑定回环地址时不要求 token，其它地址必须提供 `--token` 或 `CCL_WEB_TOKEN`。

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

- `~/.ccl/config.yaml`：`active_provider`、`acp_provider`（空 = 跟随；`default` = Claude Code 自身配置）、`lang`、`bypass_mode`、`sign_on`、`log_level`、`providers`。**含明文 API Key，勿提交或外传。**
- `~/.ccl/auth/*.json`：OAuth 凭据（0600）。
- `provider.type`：`openai`（Chat）/ `openai_responses` / `anthropic`，由用户显式选择，不按 URL 猜测；Anthropic 直连的 `endpoint` 用裸域名（不带 `/v1`），Bearer 鉴权设 `anthropicAuth: bearer`。
- `--settings` 优先级高于用户设置，因此 `outputStyle` / `language` / `statusLine` / `modelPicker` 仅在你自己没设置时由 ccl 提供。启动时会丢弃 shell 里继承的 `CLAUDE_CODE_USE_*`、`ANTHROPIC_*MODEL` 等变量；需要的值写进 provider（`ccl map` / `ccl provider env`）。
- 上下文预设：Default / Balanced 500K / Balanced 800K（在 80% 自动压缩）。

### 云同步

`ccl cloud login`（默认 Google Drive；macOS 也可 `ccl cloud login icloud`）之后 `push` / `pull`。没有浏览器的环境（SSH、Cloud Shell、无图形界面的 Linux）会自动改为打印授权链接：在任意设备的浏览器中打开并授权，随后浏览器跳转到一个打不开的 `127.0.0.1` 地址，把地址栏里的完整地址粘贴回终端即可；`--no-browser` / `--no-browser=false` 可强制选择。同步 `config.yaml` 与 `auth/*.json`，gzip 后用 AES-256-GCM 加密；Google Drive 仅申请 `drive.appdata` 权限。密钥随云端 profile 一起保存，新设备只需用同一账号 `ccl cloud login` 即可 `pull`，无需密码或配对；相应地，能登录该云账号（或持有其 ccl 授权）的人也能解密同步的 provider 凭据。冲突时 push / pull 会停止，不会静默覆盖。

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
npm install -g @claudecodelaunch/ccl                 # recommended, needs Node.js >= 22
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
ccl oauth gpt | gemini | grok | qoder | kimi | kiro | workbuddy | workbuddycn | autoclaw | zed

# B. API key / third-party gateway: single-page TUI — Endpoint + Key → Auto Configure → Save & Activate
ccl set deepseek

ccl            # launch Claude Code with the active provider
```

A fresh install uses `default`, Claude Code's own configuration (claude.ai subscription and `~/.claude` settings), for both `ccl` and `ccl acp`. `ccl oauth`, Save & Activate in `ccl set`, or `ccl use <name>` switches to a provider; `ccl use default` / `ccl use --acp default` switches back. While `ccl proxy` runs, `default` sees the configuration the proxy wrote.

Unknown commands and flags are forwarded verbatim to Claude Code (`ccl resume`, `ccl -p "hi"`) and start a real session.

### Commands

| Command | Purpose |
|---|---|
| `ccl set [name]` | Add / edit a provider (TUI) |
| `ccl ls [-a]` / `ccl use <name>` | List / switch providers; `ccl use default` = Claude Code's own configuration |
| `ccl map [--opus/--sonnet/--haiku/--fable/--custom/--subagent <model>]` | Map model slots; `ccl map auto` fills them |
| `ccl oauth <name> [alias]` / `ccl oauth prune` | Subscription login / remove unused credentials |
| `ccl provider cp\|mv\|rm\|models\|preview\|env\|effort\|ultracode` | Provider management; `preview` shows the injected settings |
| `ccl proxy -p <name>` / `status` / `stop` | Background proxy so a plain `claude` uses the chosen provider |
| `ccl acp [install]` | Agent Client Protocol (Xcode 27, …) |
| `ccl web [--port 8099]` | ccl conversations in a browser (HTTP + SSE; loopback by default) |
| `ccl cloud login\|push\|pull\|status\|remote` | Encrypted sync (iCloud / Google Drive) |
| `ccl bypass [on\|off]` | Add `--dangerously-skip-permissions` on launch; trusted environments only |
| `ccl sign [on\|off] [--default]` | Claude's commit / PR signature (off by default); `--default` also edits `~/.claude/settings.json` |
| `ccl log [on\|off] [--level debug]` | Per-session logs (off by default, in `~/.ccl/logs/`) |
| `ccl lang [zh\|zh-TW\|en]` | UI language (`CCL_LANG` > config > system) |
| `ccl doctor` / `update` / `version` / `completion` | Diagnose / upgrade / version / completions |

Every subcommand supports `--help`.

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

### Browser (ccl web)

```bash
ccl web                    # http://127.0.0.1:8099, opens the browser, no token
ccl web --host 0.0.0.0 --no-open --token "$(openssl rand -hex 24)"   # container
```

One conversation is one ccl terminal session: it reads and writes inside `--workspace` and runs tools. Every turn starts a new Claude Code process and resumes the same session with `--resume`, so provider / model / effort take effect at turn boundaries and stopping a turn ends the process. History lives in `~/.ccl/web/sessions/<id>/`. The layout follows claude.ai/code: sessions on the left, the thread in the middle, an environment / usage / connectors panel on the right, plus a centered settings dialog (general, providers, subscription login, connectors; providers are created and edited in their own dialog with endpoint, API key, model slots and effort). The composer's + menu attaches files or photos (⌘U, drag and drop, or paste; images go to the model directly, other files reach Claude Code by path), picks slash commands, and lists the MCP connectors that loaded. The page follows `ccl lang`. Tool calls open to diffs, todo lists and plans; tools that need approval show allow/deny cards (permission modes: ask, accept edits, plan, bypass — the default follows `ccl bypass`); a side panel browses and edits workspace files and stages, commits and switches git branches; existing Claude Code sessions of the workspace can be imported; messages can be scheduled; transcripts export as Markdown or HTML; settings log in to subscriptions (`ccl oauth`) and manage MCP connectors. The page includes a terminal (on by default; `--no-shell` turns it off). Settings write `~/.ccl/config.yaml` through `/api/config` under the same file lock; a running turn keeps the configuration snapshot it started with. Workspaces (projects): the current directory by default; `--workspace` can be repeated, and `--workspace-root <dir>` offers every directory inside it (a fresh clone shows up within seconds). The sidebar switches between them and can create a new project or open another folder; added folders are kept in `~/.ccl/web/workspaces.json`, and `--lock-workspaces` stops the page from adding any. Each conversation stays in the workspace it was created in. A loopback listener needs no token; any other address requires `--token` or `CCL_WEB_TOKEN`.

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

- `~/.ccl/config.yaml`: `active_provider`, `acp_provider` (empty = follow; `default` = Claude Code's own configuration), `lang`, `bypass_mode`, `sign_on`, `log_level`, `providers`. **Contains plaintext API keys — never commit or share it.**
- `~/.ccl/auth/*.json`: OAuth credentials (0600).
- `provider.type`: `openai` (Chat) / `openai_responses` / `anthropic`, chosen explicitly, never guessed from the URL. For direct Anthropic gateways use a bare-domain `endpoint` (no `/v1`) and set `anthropicAuth: bearer` for Bearer auth.
- `--settings` outranks user settings, so `outputStyle` / `language` / `statusLine` / `modelPicker` are provided by ccl only when you have not set them yourself. Inherited shell variables such as `CLAUDE_CODE_USE_*` and `ANTHROPIC_*MODEL` are dropped at launch; put needed values in the provider (`ccl map` / `ccl provider env`).
- Context presets: Default / Balanced 500K / Balanced 800K (auto-compact at 80%).

### Cloud sync

`ccl cloud login` (Google Drive by default; `ccl cloud login icloud` on macOS), then `push` / `pull`. Without a browser (SSH, Cloud Shell, Linux without a display) Google Drive login prints the authorization URL instead: approve it in a browser on any device, which then lands on a `127.0.0.1` address that fails to load; paste that whole address back into the terminal. `--no-browser` / `--no-browser=false` forces either way. It syncs `config.yaml` and `auth/*.json`, gzip-compressed and encrypted with AES-256-GCM; Google Drive uses only the `drive.appdata` scope. The key is stored with the cloud profile, so a new device only needs `ccl cloud login` with the same account before `pull`, with no passphrase or pairing; in turn, anyone who can sign in to that cloud account (or holds its ccl authorization) can decrypt the synced provider credentials. On conflicts push / pull stop instead of overwriting.

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
