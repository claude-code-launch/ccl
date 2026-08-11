# ccl: Claude Code 多网关智能代理启动器

`ccl` 是 **Claude Code**（Anthropic 官方 CLI）的多模型网关启动器。

用一句话理解它：

> 你继续用 Claude Code 的界面和习惯，`ccl` 负责帮你接上不同的模型来源（DeepSeek / OpenRouter / ChatGPT 订阅 / Gemini / Grok / Copilot / Kimi 等），并在需要时自动做协议翻译。

适合这些场景：

- 想用更便宜的 OpenAI 兼容网关跑 Claude Code
- 想用 ChatGPT / Gemini / Grok / Copilot / Kimi 等订阅账号
- 需要在多个网关 / 多个账号之间快速切换
- 不想手写复杂的环境变量和模型映射

---

## 5 分钟上手（新手优先看这里）

### 1. 安装

任选一种方式：

```bash
# 推荐：npm 全局安装
npm install -g @claudecodelaunch/ccl

# 或：Go 安装
go install github.com/claude-code-launch/ccl@latest

# 或：从源码编译
git clone https://github.com/claude-code-launch/ccl.git
cd ccl
go build -o ccl .
```

也可以从 [GitHub Releases](https://github.com/claude-code-launch/ccl/releases) 下载对应平台的二进制：

| 平台 | 文件名 |
|------|--------|
| macOS Intel | `ccl-darwin-amd64` |
| macOS Apple Silicon | `ccl-darwin-arm64` |
| Linux amd64 | `ccl-linux-amd64` |
| Linux arm64 | `ccl-linux-arm64` |
| Windows x64 | `ccl-win32-x64.exe` |
| Windows arm64 | `ccl-win32-arm64.exe` |

```bash
chmod +x ccl-darwin-arm64
mv ccl-darwin-arm64 /usr/local/bin/ccl
```

安装后检查：

```bash
ccl version
ccl doctor
```

`ccl doctor` 会检查本地依赖与当前 provider；如果还没装 Claude Code CLI，会尝试自动安装。

### 2. 选一条入门路径

#### 路径 A：用订阅账号登录（最简单）

```bash
# 任选其一
ccl oauth gpt        # GPT / Codex (OpenAI 订阅)
ccl oauth gemini     # Google Gemini
ccl oauth grok       # xAI Grok
ccl oauth copilot    # GitHub Copilot
ccl oauth qoder      # Qoder 浏览器 OAuth（不需要 Qoder CLI）
ccl oauth kimi       # Kimi / Moonshot
ccl oauth kiro       # Kiro Portal（Google / GitHub）
ccl oauth claude     # Anthropic Claude 订阅

# 登录成功后直接启动
ccl
```

登录成功后，`ccl` 会自动创建并切换到对应 provider。多账号可以加别名：

```bash
ccl oauth gpt work
ccl oauth gpt personal
ccl use work
```

#### 路径 B：用 API Key / 第三方网关

```bash
# 交互式配置（推荐）
ccl set

# 或指定名称
ccl set deepseek
```

按提示填写：

1. **Endpoint URL**（例如 `https://api.deepseek.com`）
2. **API Key**
3. 选择 **Auto**（自动映射模型）或 **Manual**（自己指定 Opus / Sonnet / Haiku）
4. 在最后一页核对并保存

然后启动：

```bash
ccl
```

### 3. 日常三板斧

```bash
ccl                 # 用当前 provider 启动 Claude Code
ccl ls              # 看看有哪些 provider
ccl use deepseek    # 切换 provider
ccl doctor          # 连不通时先跑诊断
```

可选：信任环境里不想每次点权限确认时：

```bash
ccl bypass on       # 启动时自动加 --dangerously-skip-permissions
ccl bypass          # 查看状态
ccl bypass off      # 关闭
```

> **注意**：`bypass` 会跳过 Claude Code 的交互式权限确认，只在你信任的环境开启。

### 4. 新手最常卡的点

| 现象 | 建议 |
|------|------|
| 不知道从哪开始 | 有订阅就 `ccl oauth ...`；有 API Key 就 `ccl set` |
| 启动后模型不对 | `ccl map` 或 `ccl set` 重新映射 Opus / Sonnet / Haiku |
| 连不上 / 鉴权失败 | `ccl doctor`，再 `ccl preview` 看注入了什么环境变量 |
| 多个账号互相覆盖 | 登录时加别名：`ccl oauth gpt work` |
| 想换中英文界面 | `ccl lang zh` / `ccl lang en` |
| 旧文档里的 `ccl auto` | 已更名为 **`ccl bypass`**，配置字段是 `bypass_mode` |

---

## 它具体帮你做什么？

1. **智能多档模型映射**  
   未手动配置时，自动拉取上游模型列表，按关键词分配到：
   - 💎 Opus 强推理档
   - 🚀 Sonnet 黄金档
   - ⚡ Haiku 极速档  
   用 `ccl set` / `ccl map` 手动指定后，对应档位以手动为准。

2. **协议翻译与流式代理**  
   OpenAI Chat、Codex Responses 与订阅 provider 统一暴露本机 `/v1/messages`。Responses 的请求转换、Codex 身份、SSE 解析和错误透传由 CCL 自研实现；OpenAI Chat 及部分订阅协议内嵌 CLIProxyAPI Go SDK；Kiro、Qoder 使用各自的 CCL 直接适配器；Anthropic 兼容网关保持直连。

3. **交互式 TUI 配置**  
   全屏向导配置 endpoint、协议、模型槽位、上下文压缩等；支持中文 / English（`ccl lang`）。

4. **环境诊断**  
   `ccl doctor` 检查依赖、连通性、鉴权，并批量测模型可用性。

5. **多通道 / 多账号**  
   配置在 `~/.ccl/config.yaml`；OAuth 凭据在 `~/.ccl/auth`。可随时 `use` / `ls` / `cp` / `mv` / `rm`。

6. **订阅 OAuth 一键接入**  
   `gpt` / `gemini` / `grok` / `copilot` / `qoder` / `kimi` / `kiro` / `claude`，支持多账号别名；token 会在运行时刷新。

### 协议与运行时边界

Claude Code 始终从 Anthropic Messages 侧进入。CCL 的统一 Provider Session 先决定直连还是启动本机 runtime，并负责模型补全、loopback 地址、随机会话 key 和清理生命周期。之后的最新边界如下：

| 接入类型 | 登录、凭据与刷新 | 模型目录 | 请求路由与协议转换 | CPA 边界 |
|---|---|---|---|---|
| Anthropic API Key 网关 | CCL 保存用户 API Key | CCL 直查 Anthropic `/v1/models` | Claude Code 直连 Messages | 不参与 |
| OpenAI Chat API Key 网关 | CCL 保存用户 API Key | CCL 直查 OpenAI `/models` | CPA `openai-compatibility` 完成 Messages ↔ Chat Completions | 完整数据面转换 |
| Codex Responses API Key 网关 | CCL 保存用户 API Key | CCL 直查上游 `/models` | CCL 完成 Messages ↔ Responses、Codex 身份头、SSE 与错误透传 | 不参与数据面 |
| GPT 订阅 | CPA authenticator 仅负责登录；CCL 绑定凭据、刷新 token | CCL 使用 provider 槽位构建本机会话模型目录 | CCL 完成 Messages ↔ Responses，并携带账号 ID | CPA 仅负责登录入口，不参与运行时数据面 |
| Gemini / Grok / Kimi / Claude 订阅 | CPA 对应 authenticator 登录、刷新；CCL 只绑定单个凭据文件 | CPA backend 注册模型，CCL 从本机 `/models` 读取 | CPA 对应 executor 完成 Messages ↔ 各自上游协议 | 登录、刷新、模型注册和数据面均由 CPA |
| GitHub Copilot 订阅 | CCL 自研 GitHub device flow、Copilot 换票与凭据状态 | CCL 直查 Copilot 模型目录并读取每个模型声明的 endpoint | CCL 按模型路由；Responses 使用 CCL Codex 转换，Chat / 原生 Messages 使用 CPA | CPA 只参与非 Responses 模型，是混合栈 |
| Kiro 订阅 | CCL 自研 Portal PKCE / Builder ID、刷新和单凭据运行时 | CCL 调 Kiro Portal / Amazon Q 模型接口并缓存 | CCL 完成 Messages → Amazon Q、重试、AWS EventStream → Messages | 请求数据面不经过 CPA |
| Qoder 订阅 | CCL 自研浏览器 OAuth、刷新和单凭据运行时 | CCL 直查 Qoder 模型目录，失败时使用最小兼容目录 | CCL 完成 COSY 签名、WAF 编码、Messages → Qoder、Qoder SSE → Messages | 请求数据面不经过 CPA |

CCL 还统一负责 provider 选择、模型槽位映射、可用性探测、上下文元数据、日志、usage 汇总和 runtime 生命周期。CPA runtime 由 Go SDK 内嵌启动，不依赖外部 `CLIProxyAPI` 进程。

错误恢复跟随数据面，不设置跨协议的 CCL 全局策略：Codex Responses 的 GPT OAuth 只在 401 后刷新并重试一次，API Key 网关及其余 403、429、5xx 不做全局重试，保留原状态和 `Retry-After`；其他 CPA-backed provider 继续由 CPA 管理；Copilot gateway 自己负责换票与切换凭据；Qoder 自己负责刷新、切换凭据和队列错误映射；Kiro 针对瞬时限流先轮换凭据，再按 1、2、4 秒重试整轮。

所有 `openai_responses` 网关都按 **Codex Responses** 处理，不再区分 `codex-api-key` / `generic-api-key` executor：CCL 统一注入自己维护的 `Originator: codex_cli_rs`、Codex User-Agent、`Version`、`session-id` / `thread-id` / request ID，以及当前 Codex `client_metadata`，并固定 `stream=true`、`store=false`。这些值不再由 CPA 版本间接决定。协议仍由 provider `type` 明确选择，不根据 endpoint 的 `/codex` 路径猜测。GPT 订阅与 API Key 网关共享同一套转换和 SSE 实现，区别只在鉴权：GPT 使用 OAuth token 与 `Chatgpt-Account-Id`，网关使用用户 API Key。

---

## 命令速查

下面是当前二进制实际注册的**完整命令面**：

```text
ccl [Claude Code 参数...]             启动 Claude Code；未知命令和参数会透传
├─ set [name]                         新增或修改 provider
├─ ls [-a|--all]                      列出 provider
├─ use <provider>                     切换 active provider
├─ cp <source> <target> [-y]          复制 provider
├─ mv <source> <target> [-y]          重命名 provider
├─ rm <name> [-y]                     删除 provider
├─ map [provider]
│  ├─ map auto [provider]             自动填充模型槽位
│  └─ --opus/--sonnet/--haiku/--custom/--subagent
├─ models [-a|--all]                  列出并检测模型
├─ env <KEY> <VALUE>                  设置 provider 环境变量
│  ├─ env ls                          列出环境变量
│  ├─ env rm <KEY> [-y]               删除环境变量
│  └─ env mv <OLD> <NEW> [-y]         重命名环境变量
├─ preview                            预览注入 Claude Code 的 settings JSON
├─ doctor                             检查环境、provider 和订阅健康
│
├─ provider                           上述 provider 命令的命名空间形式
│  ├─ set / ls / use
│  ├─ cp / mv / rm
│  ├─ map / models / preview
│  └─ env <KEY> <VALUE> | ls | rm | mv
│
├─ oauth <provider> [alias]           登录订阅；别名：auth
│  └─ provider: gpt | gemini | grok | copilot | qoder | kimi | kiro | claude
│
├─ bypass [on|off]                    权限确认旁路
├─ log [on|off]                       运行时日志；别名：debug
│  └─ --level debug|info|warn|error
├─ lang [zh|zh-TW|en]                 显示语言
│
├─ cloud                              加密云同步
│  ├─ login <icloud|google-drive> [alias]
│  ├─ logout [alias]
│  ├─ push
│  ├─ pull
│  ├─ tag [name]
│  ├─ status [remote]
│  ├─ key
│  │  ├─ export
│  │  └─ import [recovery-key]
│  ├─ device
│  │  ├─ request
│  │  ├─ ls
│  │  ├─ approve <code>
│  │  ├─ deny <code>
│  │  ├─ complete <code>
│  │  └─ pending
│  └─ remote
│     ├─ ls
│     ├─ use <alias>
│     ├─ rename <old-alias> <new-alias>
│     └─ set <alias>
│
├─ login / logout                     cloud 兼容入口
├─ push / pull / tag / status         cloud 兼容入口
├─ key / device                       cloud 兼容入口，保留各自子命令
│
├─ update                             更新 ccl
├─ version                            打印版本
├─ completion                         生成 shell 补全
│  └─ bash | fish | powershell | zsh
└─ help [command]                     命令帮助
```

`ccl oauth` 只负责登录并创建绑定单个凭据的 provider，不提供凭据导入或目录对账命令。`ccl status` 是云同步状态；provider 体检使用 `ccl doctor`。根命令支持 `--help` 和 `--version`，每个子命令都支持 `-h/--help`。

---

### 启动 Claude Code

```bash
ccl                              # 启动
ccl resume                       # 透传参数给 Claude Code
ccl --dangerously-skip-permissions
```

### `ccl bypass` — 权限确认旁路（原 `ccl auto`）

```bash
ccl bypass          # 查看状态
ccl bypass on       # 开启
ccl bypass off      # 关闭
```

全局开关，写入 `~/.ccl/config.yaml` 的 `bypass_mode`。开启后，所有由 `ccl` 拉起的 Claude Code 会话都会自动带上 `--dangerously-skip-permissions`。

> 旧版命令 `ccl auto` / 字段 `auto_mode` 已更名为 `ccl bypass` / `bypass_mode`。

### `ccl log` — 会话级运行时日志

```bash
ccl log                  # 查看状态
ccl log on               # 开启，默认 INFO 级别
ccl log --level debug    # DEBUG：额外记录最终请求与失败响应正文
ccl log --level warn     # 只记录 WARN 及以上
ccl log --level error    # 只记录 ERROR
ccl log off              # 关闭
```

`log` 默认关闭，只有显式执行 `ccl log on` 或 `ccl log --level <level>` 后才记录。它是全局阈值设置，写入 `~/.ccl/config.yaml` 的 `log_level`。`ccl log on` 本身不会创建共享的 `ccl-debug.log`；每个由 `ccl` 拉起的 Claude Code 临时会话或独立 provider runtime 才会获得一个带后缀的日志文件，Claude 会话默认命名为 `~/.ccl/logs/ccl-debug-claude_<id>.log`。一个会话内的全部日志级别都写入同一文件。可用 `CCL_LOG_FILE=/path/file.log` 覆盖文件名模板（实际文件仍会加入会话后缀）。日志由 Go 标准库 `slog` 输出，带时间戳、级别和消息；运行结束时会打印 Claude 会话的实际文件路径。

`INFO`（`ccl log on` 的默认值）记录 session/runtime 启动退出、数据面类型、模型路由、OAuth refresh 与上下文设置；4xx/cooldown 按 `WARN`、5xx/代理故障按 `ERROR` 记录，成功的逐请求状态只在 `DEBUG` 出现。Kiro、Qoder、Copilot 的请求会获得会话内唯一的 `request_id`，入口、凭据尝试、上游响应、切号/重试和最终结果都可用这个字段串联。常用事件包括 `request_failed`、`upstream_response`、`upstream_retry_decision`、`credential_refresh`、`model_queued` 和 `stream_conversion_failed`。

一次故障建议这样查：

```bash
grep -E 'level=(WARN|ERROR)' ~/.ccl/logs/ccl-debug-claude_<id>.log
grep 'request_id=r1' ~/.ccl/logs/ccl-debug-claude_<id>.log
```

第一条先找失败摘要，第二条用摘要里的 `request_id` 展开完整链路。Codex Responses、Kiro、Qoder、Copilot 都会记录上游状态、`retry_after` 和适配器采取的动作；其余 CPA 数据面以筛选后的 CPA 诊断为准。日志中的 endpoint 会移除 userinfo、query 和 fragment。

日志覆盖存在明确边界：普通 Anthropic API-key provider 是 Claude Code 直连，`provider_ready` 会显示 `data_plane=direct upstream_errors_visible=false`，它的 429/503 正文只能从 Claude Code 终端看到。Codex Responses、Kiro、Qoder、Copilot 的 CCL 数据面可用 `request_id` 串联入口、转换、上游响应、刷新/重试和最终状态；OpenAI Chat 及其余 CPA 数据面只保留筛选后的 `cpa_diagnostic`，CPA 当前没有向 CCL 暴露逐请求 ID。

日志不会主动记录 access token、refresh token、Authorization header、API key 或 URL 查询参数。`DEBUG` 对 Codex Responses、Copilot、Kiro、Qoder 自研/混合运行时额外记录最终上游请求体与失败响应体；payload 仍可能包含提示词、工具结果或用户输入的敏感信息，应只在本机短时开启。

旧的 `debug_mode`/`debug_verbose` 配置会在读取时迁移；DEBUG 的命令入口统一为 `ccl log --level debug`，不保留 `ccl debug verbose`。

### `ccl oauth` — 登录订阅账号

```bash
ccl oauth gpt
ccl oauth gemini
ccl oauth grok
ccl oauth copilot
ccl oauth qoder
ccl oauth kimi
ccl oauth kiro
ccl oauth claude

# 多账号别名
ccl oauth gpt work
ccl oauth gemini personal

# 可选
ccl oauth gpt --no-browser
ccl oauth gpt --callback-port 1455
ccl oauth kiro --kiro-auth builder  # 可选：AWS Builder ID device-code
```

| provider | backend | 协议 | 登录方式 |
| --- | --- | --- | --- |
| `gpt` | codex | `openai(responses)` | OpenAI OAuth 回调 |
| `copilot` | copilot | 自动选择 `responses` / `chat` / `messages` | GitHub device-code |
| `qoder` | qoder | `anthropic` | Qoder 浏览器 PKCE device flow |
| `gemini` | antigravity | `openai(chat)` | Google/Antigravity OAuth |
| `grok` | xai | `openai(chat)` | xAI device-code |
| `kimi` | kimi | `openai(chat)` | Kimi/Moonshot device-code |
| `kiro` | kiro | `anthropic` | Kiro Portal PKCE（默认，Google / GitHub）或 AWS Builder ID device-code |
| `claude` | claude | `anthropic` | Anthropic OAuth 回调 |

说明：

- 不带别名时，会从凭据文件名派生 provider 名（如 `gpt-alice@example.com`），避免多账号互相覆盖。
- 每条 provider 通过 `oauthAccountCredential` 绑定具体账号文件。
- 不再提供 `--protocol` 覆盖；各 OAuth backend 协议固定。
- 旧版 `ccl oauth chatgpt` 仍可用，会规范为 `gpt`。
- **GPT 默认槽位**（空槽位时写入；已有手动映射会保留；`chatgpt` 为兼容别名）：
  - Opus / Custom → `gpt-5.6-sol`
  - Sonnet → `gpt-5.6-terra`
  - Haiku → `gpt-5.6-luna`
- **Grok 默认槽位**（空槽位时写入；已有手动映射会保留）：
  - Opus / Custom → `grok-4.5`
  - Sonnet → `grok-4.3`
  - Haiku → `grok-3-mini`
- **Gemini 默认槽位**（空槽位时写入；已有手动映射会保留）：
  - Opus / Custom → `claude-opus-4-6-thinking`
  - Sonnet → `claude-sonnet-4-6`
  - Haiku → `gemini-3.1-pro-low`
- **Kiro 默认槽位**（空槽位时写入；已有手动映射会保留）：
  - Opus / Custom → `claude-opus-4-6`
  - Sonnet → `claude-sonnet-4-6`
  - Haiku → `claude-haiku-4-5`
- 启动时若上游 model list 没有对应首选模型，会清除该首选默认并回退自动发现映射。
- **Fast mode**（约 1.5x 速度、更高用量）仅 `gpt` 有意义：可在 `ccl set` 单页的 Runtime 区用 `←→` 调整，也可在 Claude Code 内用 `/fast` 开关。
- **Copilot** 使用独立的 GitHub OAuth 凭据和 `api.githubcopilot.com`；登录写盘前会验证账号确实拥有可用的 Copilot 模型。启动时读取账号实际模型目录，并根据每个模型声明的端点选择 Responses、Chat Completions 或 Anthropic Messages；该目录是 `ccl models --all` 的权威来源，不会混入本地兼容层的内建模型。配置里的 `type: openai_responses` 仅是本地调度兼容字段，`ccl ls` / `doctor` 显示为 `copilot(auto)`。
- **Qoder** 完全由 ccl 直接接入：`ccl oauth qoder` 打开 Qoder 授权页并轮询 OAuth token；运行时直接刷新 token、读取账号模型目录、生成 COSY 签名、编码请求并把 Qoder SSE 转换为 Anthropic Messages。不会调用、探测或读取 `qodercli`，系统无需安装 Qoder CLI。模型目录由账号实时返回；`ccl models` 会显示 Qoder 展示名、内部模型 ID、Credit 倍率以及 New / 错峰优惠标记。暂时无法读取目录时使用最小兼容目录启动。

`ccl oauth kiro` 默认打开 Kiro Portal，通过 PKCE 登录 Google / GitHub 账号；这样运行时和
Web Portal `ListAvailableModels` 使用同一身份，可返回该账号完整的模型及 Credit 倍率。
若需要 AWS Builder ID，使用：

```bash
ccl oauth kiro --kiro-auth builder
```

Kiro provider 的本地 `GET /v1/models` 会优先调用 Kiro Web Portal 的
Smithy RPCv2 CBOR `ListAvailableModels`，返回实际模型、描述、Credit 倍率/单位和支持的输入类型；
无法建立 Web 会话时回退到 Amazon Q `ListAvailableModels`，结果按凭据缓存一小时。

### `ccl cloud` — 端到端加密云同步

可以使用 iCloud Drive，或直接通过浏览器授权 Google Drive。每个网盘连接都有独立别名：

> 兼容：根级 `ccl login` / `ccl push` / `ccl pull` / `ccl status` / `ccl key` / `ccl device` / `ccl logout` / `ccl tag` 仍可用，等价于对应的 `ccl cloud ...`。

```bash
ccl cloud login icloud icloud-main        # macOS 已登录并启用 iCloud Drive
ccl cloud login google-drive personal     # 自动打开浏览器；不需要 OAuth JSON
ccl cloud login google-drive work
ccl cloud login google-drive --passphrase # 可选：首次创建 profile 时使用口令模式
ccl cloud remote ls
ccl cloud remote use personal             # 选择 primary
ccl cloud remote rename personal home
ccl cloud remote set work --no-mirror

ccl cloud key export                      # 输出离线恢复密钥
ccl cloud key export -o ~/ccl-recovery-key.txt
ccl cloud key import                      # 新设备粘贴恢复密钥
ccl cloud key import -f ~/ccl-recovery-key.txt
ccl cloud key import --provider google-drive
ccl cloud status

ccl cloud tag release-2026-07       # 给当前本地快照打标签
ccl cloud tag                       # 不写名称时使用 latest
ccl cloud push                      # 默认推送 primary
ccl cloud push --to work
ccl cloud push --all                # 同一份密文镜像到所有 mirror Remote
ccl cloud pull                      # 默认从 primary 拉取 latest
ccl cloud pull --from work
ccl cloud pull --from work --tag release-2026-07
ccl cloud status --all

ccl cloud logout work               # 只删除本地 token/cache/cursor
ccl cloud logout work --revoke      # 同时撤销 OAuth 授权
ccl cloud logout work --delete-remote --yes
```

`ccl cloud login google-drive` 使用 CCL 内置的公开 Desktop OAuth 客户端 ID，启动本机随机端口回调并使用 PKCE；用户不需要下载、传入或保管 Desktop OAuth JSON。它只申请 `drive.appdata` 最小权限，不能读取普通网盘文件。每个 Remote 的刷新令牌以 `0600` 独立保存在本机；Google Drive 中只保存应用隐藏目录里的 `ccl-sync.bundle` 和短期配对 envelope。这条链路不依赖 rclone 或 Google Drive SDK。

Cloud Sync v2 会把每个网盘的 token、缓存和同步游标隔离在 `~/.ccl/cloud/remotes/<remote-id>/`。旧版 `cloud.json`、`cloud.key`、`cloud-state.json` 和 Google 授权文件会在第一次 cloud 命令时执行本地无损迁移；迁移本身不会上传、下载或删除远端数据，并保留 `*.v1.bak`。

内置 Desktop OAuth 客户端的 `client_secret` 也会编译进二进制（Google 的 installed 客户端在换码/刷新时仍要求该字段，但它不是保密边界；真正保护授权码的是 PKCE）。用户不需要下载或传入 OAuth JSON。可选覆盖：发布构建可设置 Actions secret `GOOGLE_OAUTH_CLIENT_SECRET`（`-ldflags` 注入），本地开发可设置 `CCL_GOOGLE_OAUTH_CLIENT_SECRET`。

macOS 默认生成随机 256-bit 主密钥，并以 `0600` 保存在权威路径 `~/.ccl/cloud/profiles/<profile-id>/key`。首次登录在 v2 注册表建立前可能短暂写入 `~/.ccl/cloud.key`，迁移/注册完成后会删除根目录副本，避免双 key 漂移。不需要口令或 Touch ID。Linux 和 Windows 默认使用至少 12 个字符的 passphrase，通过 scrypt 派生 AES-256-GCM 密钥；macOS 也可以显式使用 `--passphrase`。`--passphrase-file` 和 `CCL_SYNC_PASSPHRASE` 可用于非交互登录。

`ccl cloud key export` 会把当前主密钥编码成与 profile 绑定、带校验码的恢复密钥。恢复密钥与 profile key 都不会上传；请离线保存，任何获得恢复密钥的人都能解密同步数据。Google Drive 新设备先运行 `ccl cloud login google-drive` 授权账号；若远端已有 profile，本机缺少 key，随后运行 `ccl cloud key import`，ccl 会自动选择已授权的 Google Drive，也可以使用 `--provider google-drive` 明确指定。恢复密钥通过远端加密 verifier 验证成功后，才会写入本机登录状态。

也可以让一台已经授权的设备批准新设备，不需要输入恢复密钥：

```bash
# 新设备：先登录同一个云账号；ccl 会提示缺少 profile key
ccl cloud login google-drive personal
ccl cloud device request --via personal --name "New MacBook"

# 已授权设备：必须输入新设备显示的完整 12 位代码
ccl cloud device ls --all
ccl cloud device approve J7KM-P4QX-2R9D

# 新设备
ccl cloud device complete J7KM-P4QX-2R9D
```

配对请求 10 分钟过期。协议使用 X25519、HKDF-SHA256 和 XChaCha20-Poly1305；网盘只能看到一次性公钥和密文。所有已授权设备都丢失时，离线恢复密钥仍是唯一恢复手段。

配置、授权、标签和版本索引会先 gzip 压缩再用 AES-256-GCM 加密。远端明文只包含格式版本、随机 profile ID，以及口令模式所需的随机 KDF 盐，不包含用户配置、主密钥、passphrase 或恢复密钥。

同步范围：

- `~/.ccl/config.yaml`
- `~/.ccl/auth/*.json`（只包含第一层）

profile key（以及遗留的 `cloud.key` 备份）、Google OAuth 令牌、设备状态和本地备份不会上传。

版本与冲突行为：

- 没有手动 `ccl tag` 时，`ccl cloud push` 自动使用可移动的 `latest` 标签。
- 相同标签和相同内容不会重复上传。
- 命名标签默认不可覆盖；内容不同会要求换一个标签，或显式使用 `ccl cloud push --force`。
- 如果本地和远端都从上次同步后发生变化，push/pull 会停止并报告冲突，不会静默覆盖。
- `ccl cloud pull --force` 会覆盖未同步的本地变化，但覆盖前会在 `~/.ccl/backups/` 留下一份同样经过压缩加密的本地快照。
- `ccl cloud status` 会显示当前 provider、解锁模式和同步状态。
- `ccl cloud status --all` 会逐个检查 Remote；一个网盘不可达不会掩盖其他网盘的状态。
- `ccl cloud push --all` 会先预检所有目标，并复用同一个 snapshot ID 和密文；部分提交通过本地 operation journal 重试。
- `ccl cloud pull` 永远只从一个明确来源读取，不会在多个分叉网盘间自动猜测“最新”。
- 默认 `ccl cloud logout` 不删除远端密文或 Profile key。
- `ccl cloud key import` 也接受位置参数、`--file`、`CCL_SYNC_RECOVERY_KEY` 或 stdin。
- iCloud 使用 Drive 中的 `ccl-sync/`；Google Drive 使用该 OAuth 应用专属、用户界面不可见的 `appDataFolder`。不同 Google 账号之间不会共享这一目录。
- `CCL_ICLOUD_DRIVE_DIR` 仅用于测试或自定义 iCloud Drive 挂载位置。

### `ccl set` — 添加 / 更新 Provider

```bash
ccl set                 # 交互选择已有或新建
ccl set my-provider     # 指定名称
```

TUI 是**单页配置**：顶部填写 Endpoint 与 API Key，点击 **Auto Configure** 后自动识别协议、鉴权方式与模型池（只访问 `/models` 元数据端点，不消耗额度），并推荐 Opus / Sonnet / Haiku / Custom / Subagent 槽位；随后可在同一页逐项修改：

- **Model Mapping**：每个槽位右侧显示模型（可 `enter` 进筛选弹层），`Space` 切换 `[1m]` 扩展上下文徽标。**Test Model Availability** 行为可选项——会为每个模型发送一次最小请求（消耗额度），测试后槽位旁显示 `✓`/`✗` 状态。
- **Context & Compact**：`←→` 在 Default / Balanced 间切换 provider 级压缩预算（按槽位 `[1m]` 独立）。
- **Runtime**：Protocol / Fast / Tools / Tool Search 均可 `←→` 调整。
- 底部 **Save & Activate** / **Cancel**。高度不足时页面滚动，操作栏保持可达。新配置未填写连接时，Model Mapping / Runtime 区置灰不可编辑。

Context & Compact：

1. **Extended Context `[1m]`**（按槽位）：声明该模型 ID 支持扩展上下文。
2. **Context & Compact**（Provider 全局）只有两档：

| 预设 | 行为 | 环境变量 |
|------|------|----------|
| Default | 不注入上下文变量，使用 Claude Code 原生的 200K / `[1m]` 1M 行为 | 无 |
| Balanced 500K / 400K | 500K 上下文，在 80%（约 400K）自动压缩 | `CLAUDE_CODE_MAX_CONTEXT_TOKENS=500000`、`CLAUDE_CODE_AUTO_COMPACT_WINDOW=500000`、`CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=80` |

旧版 300K、1M、Custom 等组合不再提供；再次保存 provider 时会归入 Default 并清除旧上下文变量。

### Provider 管理

```bash
ccl ls
ccl ls -a
ccl use provider-name
ccl cp source target
ccl mv old-name new-name
ccl rm name

# 完整语义入口（效果相同）
ccl provider ls
ccl provider use my-provider
ccl provider set my-provider
ccl provider map
ccl provider models
ccl provider env
ccl doctor
ccl provider preview
```

### `ccl map` — 快速映射模型槽位

```bash
ccl map                                          # 交互式 TUI
ccl map auto                                     # 自动填充前几个槽位
ccl map --opus gpt-5.1 --sonnet gpt-5.1-mini
ccl map --haiku gpt-4o-mini
ccl map --custom gpt-5.1 my-provider
ccl map --subagent gpt-5.4-mini
```

OAuth provider 不需要先运行 `ccl set`：`ccl map` / `ccl map auto` 会临时启动对应 OAuth runtime，并直接使用账号的实时模型目录。选择器显示上游展示名、内部 ID、倍率与活动标记，但槽位中只保存请求所需的内部 ID；临时 endpoint 和会话 key 不会写入配置。

### `ccl models` / `ccl doctor` / `ccl preview`

```bash
ccl models              # 测试已配置模型
ccl models --all        # 查看并测试 provider 全部模型
ccl doctor              # 环境 + provider 状态 + 连通性
ccl preview             # 预览将注入 Claude Code 的 settings JSON
```

模型目录带有展示元数据时，输出优先显示易读名称，同时保留配置请求所需的内部模型 ID；倍率和活动标记直接来自本次上游目录，不使用本地硬编码。

### `ccl env` — 环境变量

```bash
ccl env ls
ccl env KEY VALUE
ccl env mv OLD_KEY NEW_KEY
ccl env rm KEY
```

### 其它

```bash
ccl lang                # 交互切换语言
ccl lang zh
ccl lang en

ccl update              # 升级
ccl version             # 版本
ccl completion zsh      # shell 补全（也支持 bash/fish/powershell）
```

语言优先级：`CCL_LANG` 环境变量 > `config.yaml` > 系统语言。

---

## 配置文件

路径：`~/.ccl/config.yaml`

```yaml
active_provider: deepseek
lang: zh-CN
bypass_mode: false
providers:
  deepseek:
    name: deepseek
    type: openai
    endpoint: https://api.deepseek.com
    apikey: sk-xxx
    model: deepseek-chat,deepseek-reasoner
    opusModel: deepseek-reasoner
    sonnetModel: deepseek-chat
  sensenova:
    name: sensenova
    type: anthropic
    endpoint: https://token.sensenova.cn
    apikey: sk-xxx
    anthropicAuth: bearer
  gpt:
    name: gpt
    type: openai_responses
    endpoint: oauth://codex
    oauthProvider: gpt
    oauthAccountCredential: codex-user@example.com.json
```

字段要点：

- `type: openai`（显示 `openai(chat)`）：经 CLIProxyAPI 转到上游 Chat Completions。
- `type: openai_responses`（显示 `openai(responses)`）：经 CCL 自研 Codex Responses runtime 走 Responses API。协议由 `type` 明确选择，不根据 endpoint 路径猜测；可在核对页切换 Chat / Responses。
- `type: anthropic`：普通 API-key provider 由 Claude Code 直连；`oauthProvider: kiro` 使用本机 Messages → Amazon Q 适配器；`oauthProvider: qoder` 使用本机 Messages → Qoder 直接适配器。
- `oauthProvider`：使用已保存的 OAuth 凭据；运行时使用本机会话地址与随机 key，不写回配置。
- `oauthAccountCredential`：该订阅 provider 精确绑定的 `~/.ccl/auth/` 凭据文件名。
- `bypass_mode`：全局是否自动附加 `--dangerously-skip-permissions`。
- Anthropic 直连时 `endpoint` 建议裸域名（如 `https://token.sensenova.cn`），避免拼出 `/v1/v1/messages`。
- 运行时默认：子代理模型优先 Custom/Sonnet；工具并发默认 `3`；`ENABLE_TOOL_SEARCH=false`。可在配置页或 `ccl env` 覆盖。输出上限由 Claude Code、协议转换层和上游模型管理，CCL 不设置默认值。

OAuth 凭据目录：`~/.ccl/auth/`（每个账号一个 JSON）。

---

## 推荐工作流示例

### 只用 DeepSeek 便宜跑

```bash
ccl set deepseek
# Endpoint: https://api.deepseek.com
# 填 API Key → Auto 映射 → 保存
ccl
```

### ChatGPT 订阅 + 本地 API 网关并存

```bash
ccl oauth gpt work
ccl set openrouter
ccl ls
ccl use work          # 切到订阅
ccl use openrouter    # 切到网关
```

### 排查「为什么没用上我想要的模型」

```bash
ccl preview           # 看最终注入配置
ccl models            # 看哪些模型真正可用
ccl map               # 重新绑定槽位
ccl doctor            # 连通性 / 鉴权
```

---

## 本地验证（开发者）

```bash
go test ./...
go build -o /tmp/ccl-debug .

export CCL_TEST_HOME="$(mktemp -d)"
HOME="$CCL_TEST_HOME" /tmp/ccl-debug set sensenova
HOME="$CCL_TEST_HOME" /tmp/ccl-debug preview
HOME="$CCL_TEST_HOME" /tmp/ccl-debug doctor
HOME="$CCL_TEST_HOME" /tmp/ccl-debug models --all
```

Anthropic 兼容网关建议确认：

- `endpoint` 为裸域名，不带 `/v1`
- Bearer 认证时 `preview` 出现 `ANTHROPIC_AUTH_TOKEN`，而不是 `ANTHROPIC_API_KEY`
- `ccl set` 不再写入 `effortLevel` / `CLAUDE_CODE_EFFORT_LEVEL`
- 配置了 Custom model 时，`preview` 顶层 `model` 与 `ANTHROPIC_CUSTOM_MODEL_OPTION` 一致

---

## CI/CD

推送 `v*` tag 触发多平台构建与发布：

```bash
git tag v1.2.0
git push origin v1.2.0
```

GitHub Actions 会构建 6 个平台二进制，并发布到 GitHub Releases + npm。

---

## 目录结构

```text
├── cmd/
│   ├── advanced_config.go     # TUI 配置向导
│   ├── auth.go                # 订阅 OAuth 登录
│   ├── auth_import.go         # 导入并规范化已有 OAuth 文件
│   ├── auth_sync.go           # auth 目录与配置同步
│   ├── cloud_sync.go          # iCloud/Google Drive 登录、恢复密钥与同步命令
│   ├── bypass.go              # ccl bypass（权限旁路开关）
│   ├── log.go                 # ccl log（统一 slog 日志配置）
│   ├── provider.go            # provider 子命令
│   ├── env.go                 # 环境变量管理
│   ├── set.go                 # set 命令
│   ├── select.go              # 通用 TUI 选择器
│   ├── doctor.go              # 环境与连通性自检
│   ├── install.go             # Claude CLI 自动安装
│   ├── lang_cmd.go            # 语言切换
│   ├── list.go                # ls
│   ├── map.go                 # 模型槽位映射
│   ├── models.go              # 模型列表与可用性
│   ├── root.go                # 主入口 + passthrough
│   ├── preview.go             # 预览 settings JSON
│   ├── update.go              # 升级
│   ├── use.go                 # 切换 provider
│   └── version.go             # 版本
├── internal/
│   ├── cloudsync/             # 压缩、AES-GCM 加密、快照和冲突处理
│   ├── claude/                # Claude Code 进程拉起
│   ├── config/                # yaml 配置读写
│   ├── locale/                # 多语言
│   ├── modelrouting/          # 档位启发式映射
│   ├── oauthproxy/            # OAuth 登录、CLIProxyAPI、Kiro/Qoder Messages 运行时
│   ├── protocol/              # endpoint 规范化与探测
│   └── provider/              # Provider / Config 结构
└── main.go
```

---

## 开源许可

MIT。CLIProxyAPI SDK、Kiro/Qoder 参考实现的第三方许可见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
