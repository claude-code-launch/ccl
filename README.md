# ccl

**[中文](#中文) · [English](#english)**

---

## 中文

`ccl` 为 Grok Build 和 Codex 接入 OpenAI 兼容网关与 GPT、Gemini、Grok 等订阅账号，通过本地代理转换协议。

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

普通终端启动需要安装 Grok Build。Codex 接入使用你现有的 Codex 客户端。 [Grok Build](https://x.ai/cli)

### 快速开始

```bash
# A. 订阅账号（示例登录 GPT 账号并设别名）
ccl oauth gpt work

# B. API Key / 第三方网关：单页 TUI，填 Endpoint 与 Key → Auto Configure → Save & Activate
ccl set deepseek

ccl            # 用当前 provider 启动 Grok Build
ccl --provider deepseek -p "你好"   # 仅本次使用 deepseek，不改变默认选择
```

新安装默认使用 Grok Build 自身的登录。通过 `ccl oauth`、`ccl set` 中的 Save & Activate 或 `ccl use <name>` 选择 provider，运行 `ccl use default` 可切回 Grok Build 自身的登录。

额外参数会透传给 Grok Build，可能启动计费会话。`--provider` 放在 Grok 参数前，亦可写成 `--provider=deepseek`。用 `ccl grok doctor` 调用 Grok 自己的同名子命令。

### 常用命令

| 命令 | 作用 |
|---|---|
| `ccl set [name]` | 新增 / 修改 provider（TUI） |
| `ccl ls [-a]` / `ccl use <name>` | 列出 / 切换 provider；`ccl use default` 使用 Grok Build 自身登录 |
| `ccl --provider <name> [Grok 参数]` | 本次启动指定 provider，不改变默认选择 |
| `ccl map [--opus/--sonnet/--haiku/--fable/--custom/--subagent <model>]` | 映射模型槽位；`ccl map auto` 自动填充 |
| `ccl oauth <name> [alias]` / `ccl oauth prune` | 订阅登录 / 清理未使用凭据 |
| `ccl provider cp\|mv\|rm\|models\|preview\|env\|effort` | provider 管理；`preview` 查看 Grok 模型配置 |
| `ccl codex start` / `-p <name>` / `status` / `stop` | Codex 模型接入：向模型选择器加入当前配置的模型 |
| `ccl serve [-p <name>] [--workspace <dir>]` | 在本地浏览器中使用 Grok Build 会话 |
| `ccl cloud login\|push\|pull\|status\|remote` | 加密云同步（iCloud / Google Drive） |
| `ccl bypass [on\|off]` | 启动时附加 `--always-approve`，仅在可信环境使用 |
| `ccl log [on\|off] [--level debug]` | 会话日志（默认关闭，写入 `~/.ccl/logs/`） |
| `ccl lang [zh\|zh-TW\|en]` | 界面语言（`CCL_LANG` > 配置 > 系统） |
| `ccl doctor` / `update` / `version` / `completion` | 诊断 / 升级 / 版本 / 补全 |

每个子命令都支持 `--help`。

### 浏览器会话

```bash
ccl serve -p deepseek --workspace .
```

在浏览器里新建会话、查看流式回复、停止回合，或对 Grok 发出的工具审批选择允许一次或拒绝。启动时指定已配置的 Provider，省略 `-p` 时使用当前选中的 Provider。可重复指定 `--workspace`，默认用当前目录。服务仅监听本机 `127.0.0.1`，访问需使用终端打印的会话链接。`--no-open` 只打印链接，`--port` 可指定端口。

会话保存在 `~/.ccl/serve/grok/`，用同一 Provider 和工作目录重启服务即可继续。需要安装支持 ACP v1 会话加载的 Grok Build。

### Codex 模型接入

```bash
ccl codex start        # 使用 ccl use 当前选择的配置
ccl codex -p deepseek  # 或显式指定本次代理的 provider
# 重启 Codex 后选择 CCL 模型
ccl codex stop        # 恢复 Codex 原配置
```

- 已绑定 provider 的配置变化会自动刷新模型目录和路由；新请求使用新 Runtime，正在进行的输出继续完成。客户端是否立即显示新模型由 Codex 决定，本地 JSON 目录不承诺进程内重载，必要时重启 Codex。
- 多提供商接入：`ccl codex start deepseek --with-provider gemini --with-provider grok`。每个模型绑定各自账号，后续启动沿用这些额外绑定；`--with-provider ''` 清空。`ccl codex start --refresh` 主动重新获取模型。
- 默认使用本地目录。独立模式可试用 `ccl codex start --independent --catalog remote`；`--catalog local` 回退。Shared 模式不能把内置 OpenAI 的远程目录 URL 指向 localhost，因此继续使用合并 JSON。远程目录已验证获取与后台更新，但不承诺 Desktop 选择器即时刷新。所有模式都区分 CCL 路由/目录更新与客户端列表更新。
- OpenAI 额度耗尽后要使用其他提供商，可运行 `ccl codex start --independent` 并重启 Codex。独立模式使用 CCL 自己的 provider 和会话凭据，列表只显示 CCL 模型；`ccl codex start --shared` 切回原生与 CCL 共用的列表。代理运行期间，后续启动、菜单切换沿用当前模式；停止后不保存模式，下次不带模式参数启动默认使用 shared。
- Codex 会话绑定创建时的 model provider。独立模式会话使用 `ccl_proxy`，切回 shared 或停止代理后，该定义会移除，旧会话可能提示 provider 未配置或 `Model unavailable`。继续原会话需恢复 `ccl codex start --independent` 并重启 Codex；跨模式继续相同历史请分叉新会话，不要修改会话记录。参见 [OpenAI Docs](https://learn.chatgpt.com/docs/codex-sdk#resume-a-thread-without-loading-its-history)。
- `ccl use` 只影响 `ccl` 启动，不会切换运行中的代理。
- 先打开 Codex 一次建立配置；启动代理会更新 `~/.codex/config.toml`（支持 `CODEX_HOME`）。重启 Codex 后，同一模型选择器中同时显示原生模型和当前配置的第三方模型。列表只显示模型名称；与原生模型同名时加简短的 `CCL` 标记。WorkBuddy 的同名内部版本合并为一项。
- Codex 的 CCL 模型沿用“上下文与压缩”设置：Default 为 200K，模型池或槽位中带 `[1m]` 的模型为 1M；Balanced 为所选的 500K / 800K，均在 85% 自动压缩。转换协议的模型由 CCL 生成可回放摘要，原生 Responses 模型保留上游压缩。
- 转换协议的模型支持 Codex 客户端工具搜索：CCL 将搜索转换为普通函数调用，由 Codex 检索本地工具并返回选中的定义，避免一次发送全部延迟加载工具。实际工具仍由 Codex 执行并检查权限；模型需要能正确使用搜索函数和返回的工具定义。
- 支持图片的模型按上游声明开放截图读取；转换模型沿用本机原生目录提供的客户端工具模式，computer use 仍由 Codex 客户端执行并检查权限。shared 模式内，常规对话可在转换模型与原生 GPT 之间切换；GPT 的原生加密压缩内容无法解密；转换模型会校验同一会话的本地记录与压缩项，再恢复有边界的可见历史摘录，并提示旧细节可能缺失。缺少匹配记录时明确报错。
- 转换模型的 effort 档位来自 models.dev 元数据缓存，按模型匹配，不再统一为低、中、高。OAuth 登录和 `ccl set` 保存时按需刷新；也可运行 `ccl set --refresh-models` 单独更新。网络失败使用 `~/.ccl/cache/modelsdev.json`；未匹配或尚无缓存时保留兼容设置。运行中的代理会刷新元数据，本地 JSON 模式重启 Codex 加载新目录。原生 Responses 模型保留自身元数据。
- `ccl oauth gemini` 接入 Google Antigravity。CCL 使用绑定账号的订阅档位获取目录，缓存真实模型 ID 与元数据；Gemini 4 Argon 等模型按账号实际返回的思考档位解析。目录请求失败时，先使用该账号缓存和已有配置，最后才使用静态兼容列表。Codex 网关每五分钟刷新目录，也可用 `--refresh` 主动刷新。
- Codex 保留原生 OpenAI Provider 与登录。原生请求继续走原有 ChatGPT / OpenAI 服务，只有独立白名单中的 `ccl/<provider>/<model>` 发往 CCL；转发到 CCL 时移除 Codex 凭据，WebSocket 用 426 回退 HTTP Responses。
- 原生模型信息来自现有目录或 `models_cache.json`，必要时读取已安装 Codex 的 `debug models --bundled`，不写死原生模型名称。缺少缓存时需安装较新的 Codex CLI，或让 CCL 能找到桌面 App 自带的 Codex。原有自定义 Provider 或默认 profile 的目录覆盖会明确报错，避免改变其路由。
- macOS 状态栏菜单可切换配置或停止 Codex 接入。切换成功后同步更新 CCL 的当前选择；Codex 保持运行，需要手动重启以加载新目录。新配置启动失败会恢复原代理并提示错误。代理退出或异常停止后图标消失。无桌面环境可用 `CCL_PROXY_NO_MENUBAR=1` 关闭图标。
- 代理仅管理 Codex 配置，普通 Grok Build 启动独立运行。停止或异常恢复时只恢复仍等于代理写入值的 Codex 字段，保留后续用户修改；升级旧版共用代理时会先恢复其遗留的 Claude 设置。启动、切换或停止后，请重启 Codex 以重新载入目录。
- 异常退出后，下次运行任意 `ccl` 命令会按 `~/.ccl/proxy/state.json` 自动恢复；启动诊断见 `~/.ccl/proxy/proxy.log`。

### 协议与接入

CCL 在 `127.0.0.1` 提供 Responses 和 Chat Completions。每个代理使用随机会话凭据。Responses 是转换各后端时使用的协议。

| 来源 | 上游协议 |
|---|---|
| OpenAI Chat 网关 / Kimi / WorkBuddy | Chat Completions |
| OpenAI Responses 网关 / GPT / Grok | Responses，CCL 使用对应后端的请求标识 |
| Gemini | Google Antigravity |
| Cursor | Connect 双向 Agent 流 |
| Kiro / Qoder | Amazon Q / COSY |
| AutoClaw | 受管 Chat Completions 代理 |

旧的 Claude Code 启动、`ccl acp` 和 Zed OAuth 入口已退役。旧 Messages 网关需要改用服务商的 Responses 或 Chat Completions 地址。旧配置若指定了默认 Claude 账号，运行 `ccl use default` 可显式切换到 Grok Build 自身登录。

429 / 5xx 默认最多快速重试两次（500ms、1s）；上游提供更长的 `Retry-After` 时立即透传等待提示，不提前重试。401 由各后端刷新 OAuth 凭据，Qoder 刷新失败会保留原来的认证错误。Qoder / WorkBuddy 的认证 API、刷新和模型请求拒绝跨来源重定向；账号绑定到指定凭据文件，停用或变更类型后不继续刷新。

### 配置

- `~/.ccl/config.yaml` 保存 provider、当前选择、语言、权限绕过和日志设置。**含明文 API Key，勿提交或外传。**
- `~/.ccl/auth/*.json` 保存 OAuth 凭据，权限为 0600。
- API-key provider 的 `type` 可选 `openai`（Chat Completions）或 `openai_responses`。OAuth provider 由其后端决定。
- `ccl provider preview` 显示传给 Grok Build 的默认模型、辅助模型和 reasoning effort。
- Codex 上下文预设为 Default、Balanced 500K 和 Balanced 800K。


### 云同步

`ccl cloud login`（默认 Google Drive；macOS 也可 `ccl cloud login icloud`）之后 `push` / `pull`。没有浏览器的环境（SSH、Cloud Shell、无图形界面的 Linux）会自动改为打印授权链接：在任意设备的浏览器中打开并授权，随后浏览器跳转到一个打不开的 `127.0.0.1` 地址，把地址栏里的完整地址粘贴回终端即可；`--no-browser` / `--no-browser=false` 可强制选择。同步 `config.yaml` 与 `auth/*.json`，gzip 后用 AES-256-GCM 加密；Google Drive 仅申请 `drive.appdata` 权限。密钥随云端 profile 一起保存，新设备只需用同一账号 `ccl cloud login` 即可 `pull`，无需密码或配对；相应地，能登录该云账号（或持有其 ccl 授权）的人也能解密同步的 provider 凭据。冲突时 push / pull 会停止，不会静默覆盖。

### 开发

```bash
gofmt -l .            # 必须为空
go vet ./... && go test -race ./...
go build -o /tmp/ccl-verify .
node --test scripts/npm-packages.test.js
```

- 不要把 `ccl` 当测试工具直接跑：未知参数会启动计费的 Grok 会话；用 `go test` 与 `--help`，并设置临时 `HOME` / `USERPROFILE`。
- 架构与约定见 [AGENTS.md](AGENTS.md) 与 [docs/development-architecture.md](docs/development-architecture.md)；协议说明见 `internal/oauthproxy/doc.go`；npm 发布见 [docs/npm-release.md](docs/npm-release.md)；云同步设计见 [docs/cloud-sync-v2-design.md](docs/cloud-sync-v2-design.md)。
- 推送 `v*` tag 触发 CI 构建 6 个平台二进制并发布到 GitHub Releases 与 npm。

### 许可

MIT。第三方参考实现见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。

---

## English

`ccl` connects Grok Build and Codex to OpenAI-compatible gateways and subscription providers such as GPT, Gemini, and Grok through a local protocol proxy.

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

Terminal launches require Grok Build. Codex integration uses your existing Codex client. [Grok Build](https://x.ai/cli)

### Quick start

```bash
# A. Subscription login (example with a GPT account alias)
ccl oauth gpt work

# B. API key / third-party gateway: single-page TUI — Endpoint + Key → Auto Configure → Save & Activate
ccl set deepseek

ccl            # launch Grok Build with the active provider
ccl --provider deepseek -p "hello"  # use deepseek once without changing the selection
```

A fresh install uses Grok Build's own login. Select a provider with `ccl oauth`, Save & Activate in `ccl set`, or `ccl use <name>`. Use `ccl use default` to return to Grok Build's own login.

Extra arguments reach Grok Build and can start a billed session. Put `--provider` before Grok's arguments, or use `--provider=deepseek`. Use `ccl grok doctor` to reach Grok's command of the same name.

### Commands

| Command | Purpose |
|---|---|
| `ccl set [name]` | Add / edit a provider (TUI) |
| `ccl ls [-a]` / `ccl use <name>` | List / switch providers; `ccl use default` uses Grok Build's own login |
| `ccl --provider <name> [Grok args]` | Use a provider for one launch without changing the selection |
| `ccl map [--opus/--sonnet/--haiku/--fable/--custom/--subagent <model>]` | Map model slots; `ccl map auto` fills them |
| `ccl oauth <name> [alias]` / `ccl oauth prune` | Subscription login / remove unused credentials |
| `ccl provider cp\|mv\|rm\|models\|preview\|env\|effort` | Provider management; `preview` shows the Grok model overlay |
| `ccl codex start` / `-p <name>` / `status` / `stop` | Connect the selected provider's models to Codex |
| `ccl serve [-p <name>] [--workspace <dir>]` | Use Grok Build conversations in a local browser |
| `ccl cloud login\|push\|pull\|status\|remote` | Encrypted sync (iCloud / Google Drive) |
| `ccl bypass [on\|off]` | Add `--always-approve` on launch; trusted environments only |
| `ccl log [on\|off] [--level debug]` | Per-session logs (off by default, in `~/.ccl/logs/`) |
| `ccl lang [zh\|zh-TW\|en]` | UI language (`CCL_LANG` > config > system) |
| `ccl doctor` / `update` / `version` / `completion` | Diagnose / upgrade / version / completions |

Every subcommand supports `--help`.

### Browser sessions

```bash
ccl serve -p deepseek --workspace .
```

Start a session, stream replies, stop a turn, and allow or deny the tool approvals that Grok requests. Select a configured provider with `-p`, or use the active one. Repeat `--workspace` to enable multiple directories. The default is the current directory. The server listens on `127.0.0.1` and requires the session link printed in the terminal. Use `--no-open` to print the link without opening it or `--port` to select a port.

Sessions are stored in `~/.ccl/serve/grok/`. Restart with the same provider and workspace to continue them. Grok Build must support ACP v1 session loading.

### Codex model integration

```bash
ccl codex start        # use the provider selected with ccl use
ccl codex -p deepseek  # or explicitly select this proxy's provider
# restart Codex and select a CCL model
ccl codex stop        # restores your original Codex settings
```

- Bound provider edits automatically refresh the catalog and routes. New requests use new runtimes while existing responses finish. Codex decides when its picker refreshes; local JSON does not promise in-process reload, so restart Codex if its list is stale.
- Attach multiple providers: `ccl codex start deepseek --with-provider gemini --with-provider grok`. Each route retains its own account. Later starts inherit additional bindings; `--with-provider ''` clears them. Use `ccl codex start --refresh` to rediscover models explicitly.
- Local catalogs are the default. Independent mode can opt into `ccl codex start --independent --catalog remote`; `--catalog local` restores the fallback. Shared mode keeps merged JSON because built-in OpenAI rejects localhost remote catalog URLs. Remote fetching and background app-server updates are verified; immediate Desktop picker refresh is not promised. Gateway/catalog updates and client-picker reload are separate capabilities in every mode.
- To use another provider after OpenAI quota is exhausted, run `ccl codex start --independent` and restart Codex. The custom CCL provider has its own session credential and a CCL-only picker. `ccl codex start --shared` restores the merged native/CCL picker. Starts and menu switches inherit the running mode; after stopping, a start without a mode flag defaults to shared.
- Codex chats resume with their saved model provider. Independent chats use `ccl_proxy`; switching to shared or stopping removes that definition, so those chats can report an unconfigured provider or `Model unavailable`. Restore `ccl codex start --independent` and restart Codex to resume the original chat, or fork its history to use the other provider. Do not rewrite chat records. See [OpenAI Docs](https://learn.chatgpt.com/docs/codex-sdk#resume-a-thread-without-loading-its-history).
- `ccl use` only affects `ccl` launches, not a running proxy.
- Open Codex once to create its profile; startup updates `~/.codex/config.toml` (honors `CODEX_HOME`). Restart Codex to see native models and the selected provider's models in one picker. Labels show only model names, with a short `CCL` marker for a collision with a native name. WorkBuddy's internal revisions sharing a name appear once.
- CCL models in Codex follow the provider's context/compaction setting: Default uses 200K, or 1M for models marked `[1m]` in the saved pool or slots; Balanced uses the selected 500K / 800K budget. All compact at 85%. CCL generates replayable summaries for translated protocols; native Responses models retain upstream compaction.
- Translated models support Codex client tool search: CCL adapts search to an ordinary function call, Codex searches local tools, and CCL loads the returned definitions instead of sending all deferred tools up front. Codex executes actual tools and enforces permissions. The model must be able to use the search function and the discovered definitions correctly.
- Models that advertise image input can read screenshots. Translated models follow the installed native catalog's client tool mode; Codex still executes computer use and checks permissions. Within shared mode, normal conversation history can switch between translated models and native GPT. Native GPT compaction cannot be decrypted. Converted models can validate the matching local chat/checkpoint and recover a bounded visible-history excerpt with a notice that older details may be missing. Missing or mismatched records fail explicitly.
- Translated models use per-model effort tiers from the models.dev metadata cache. OAuth login and confirmed `ccl set` saves refresh it; `ccl set --refresh-models` updates metadata alone. Network failures use `~/.ccl/cache/modelsdev.json`; missing/unmatched metadata retains compatibility settings. The running proxy refreshes metadata; restart Codex to reload a local JSON catalog. Native Responses models retain their own metadata.
- `ccl oauth gemini` connects to Google Antigravity. Discovery uses the bound account's subscription tier and caches real wire IDs and metadata. Gemini 4 Argon and other families resolve only to thinking variants that account returned. Discovery failure uses its last good cache and saved models before the static compatibility fallback. The Codex gateway refreshes the catalog every five minutes or on `--refresh`.
- Codex retains its native OpenAI provider and login. Native requests keep their original ChatGPT / OpenAI route; only the separate `ccl/<provider>/<model>` allow-list goes to CCL, with native credentials removed. WebSockets receive 426 to fall back to HTTP Responses.
- Native metadata comes from an existing catalog or `models_cache.json`, with the installed Codex's `debug models --bundled` as fallback; native names are never hardcoded. Without a cache, install a recent Codex CLI or make the desktop app's bundled Codex discoverable. A conflicting custom provider or default-profile catalog override is reported instead of changing its routing.
- macOS shows a menu bar icon with the checked current provider, configuration switching and Disconnect Codex. A switch updates the CCL selection and leaves Codex running; restart Codex yourself to load the new catalog. A failed proxy start restores the previous proxy and reports the error. The icon exits with the proxy, including crashes. Set `CCL_PROXY_NO_MENUBAR=1` in a headless environment to disable it.
- The proxy manages only Codex configuration; ordinary Grok Build launches remain independent. Stop and crash recovery restore only unchanged Codex fields owned by the proxy, retaining later edits. Upgrading an older shared proxy also restores its legacy Claude settings once. Restart Codex after starting, switching or stopping to reload its catalog.
- After a crash, the next `ccl` command restores settings from `~/.ccl/proxy/state.json`; startup diagnostics are in `~/.ccl/proxy/proxy.log`.

### Protocols

CCL serves Responses and Chat Completions on `127.0.0.1` with a random per-session key. Responses is the common input for provider adapters.

| Source | Upstream protocol |
|---|---|
| OpenAI Chat gateway / Kimi / WorkBuddy | Chat Completions |
| OpenAI Responses gateway / GPT / Grok | Responses with backend-specific identity headers |
| Gemini | Google Antigravity |
| Cursor | Connect bidirectional agent stream |
| Kiro / Qoder | Amazon Q / COSY |
| AutoClaw | Managed Chat Completions proxy |

Claude Code launches, the `ccl acp` command, and Zed OAuth have been retired. A legacy Messages gateway requires a Responses or Chat Completions endpoint. If an existing configuration points to a default Claude account, use `ccl use default` to select Grok Build’s own login.

429 / 5xx normally get at most two quick retries (500 ms, 1 s). A longer `Retry-After` is relayed immediately instead of retrying early. Each backend handles 401 with its OAuth refresh; Qoder preserves the original authentication error if refresh fails. Qoder / WorkBuddy refuse cross-origin redirects for credential API, refresh and model requests. Accounts stay bound to the selected credential file, and disabled or changed credential types are not refreshed.

### Configuration

- `~/.ccl/config.yaml` stores provider settings, the active selection, language, bypass mode, and log level. **It contains plaintext API keys.**
- `~/.ccl/auth/*.json` stores OAuth credentials with 0600 permissions.
- Use `openai` for Chat Completions or `openai_responses` for a Responses API-key provider. An OAuth backend determines its own protocol.
- `ccl provider preview` displays the default and helper models and reasoning effort sent to Grok Build.
- Codex context presets are Default, Balanced 500K, and Balanced 800K.


### Cloud sync

`ccl cloud login` (Google Drive by default; `ccl cloud login icloud` on macOS), then `push` / `pull`. Without a browser (SSH, Cloud Shell, Linux without a display) Google Drive login prints the authorization URL instead: approve it in a browser on any device, which then lands on a `127.0.0.1` address that fails to load; paste that whole address back into the terminal. `--no-browser` / `--no-browser=false` forces either way. It syncs `config.yaml` and `auth/*.json`, gzip-compressed and encrypted with AES-256-GCM; Google Drive uses only the `drive.appdata` scope. The key is stored with the cloud profile, so a new device only needs `ccl cloud login` with the same account before `pull`, with no passphrase or pairing; in turn, anyone who can sign in to that cloud account (or holds its ccl authorization) can decrypt the synced provider credentials. On conflicts push / pull stop instead of overwriting.

### Development

```bash
gofmt -l .            # must be empty
go vet ./... && go test -race ./...
go build -o /tmp/ccl-verify .
node --test scripts/npm-packages.test.js
```

- Do not use `ccl` itself as a test harness: unknown args start a billed Grok session. Use `go test` and `--help` with a throwaway `HOME` / `USERPROFILE`.
- Architecture and conventions: [AGENTS.md](AGENTS.md) and [docs/development-architecture.md](docs/development-architecture.md); protocol notes: `internal/oauthproxy/doc.go`; npm release: [docs/npm-release.md](docs/npm-release.md); cloud sync design: [docs/cloud-sync-v2-design.md](docs/cloud-sync-v2-design.md).
- Pushing a `v*` tag makes CI build six platform binaries and publish to GitHub Releases and npm.

### License

MIT. Third-party references: [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
