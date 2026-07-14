# ccl: Claude Code 多网关智能代理启动器 (Multi-Provider Launcher)

`ccl` 是一个专门为 Anthropic 官方 CLI 工具 **Claude Code** 开发的多模型网关代理与极速启动器。

它可以帮助你在运行 Claude Code 时，无缝对接 OpenAI 兼容格式的网关（如 DeepSeek、SiliconFlow、OpenRouter、OneAPI 等），实现超低成本运行。

## ✨ 核心亮点

1. **智能多档模型映射 (无需复杂配置)**
   - 当槽位未手动配置时，`ccl` 自动进入 **「智能协议代理映射模式」**。
   - 自动在启动时拉取接口提供商的可用模型库，按关键词动态分析并分配到各档位：
     - 💎 **Opus 强推理档** → 优先匹配 `deepseek-reasoner` (R1) 或 `o1`、`o3-mini`、`gpt-4o`
     - 🚀 **Sonnet 黄金档** → 优先匹配 `deepseek-chat` (V3)、`gpt-4o`、`claude-3-5-sonnet`
     - ⚡ **Haiku 极速档** → 优先匹配 `gpt-4o-mini`、`gpt-3.5-turbo`
   - 若通过 `ccl set` 手动为某个档位指定了模型，则该档位的自动映射被覆盖，其余未配置的档位仍走自动映射。

2. **零感协议翻译与流式代理**
   - 源码嵌入 CLIProxyAPI Go SDK。OpenAI Chat、OpenAI Responses、Codex 与 OAuth provider 统一由 SDK 暴露本机 `/v1/messages`，负责请求、流式响应、工具调用和模型别名转换；Anthropic provider 保持直连。

3. **交互式 TUI 配置向导**
   - 全新的 bubbletea 驱动的全屏 TUI：多页表单，键盘导航（方向键 / Tab / Enter / Esc），实时协议探测与模型拉取。
   - 支持 Default + 6 档 Reasoning Effort（`low` ~ `ultracode`）：Default 不注入 `CLAUDE_CODE_EFFORT_LEVEL`，允许 Claude 内部设置生效。
   - 自动识别 Anthropic 兼容网关的认证方式：官方 `x-api-key` 或 Bearer token（`ANTHROPIC_AUTH_TOKEN`）。
   - **多语言支持**：中文 / English，运行时通过 `ccl lang` 随时切换。

4. **智能环境探针与诊断 (`ccl doctor`)**
   - 自动检查本地环境依赖（Node.js, Claude CLI）。
   - 如果系统未安装 Claude CLI，`ccl` 将触发**全自动静默安装**。
   - 提供连接探针，对各 Provider 的 Endpoint 连通性、API 鉴权密钥进行安全测试。
   - **模型可用性检测**：并发批量测试所有配置的模型（50 并发 / 10s 超时），自动将可用模型排在配置文件前列。
   - **实时进度条**：模型测试时显示 `[████████░░░░░░] 45/100 ✓38 ✗7` 进度条。

5. **多通道配置与灵活切换**
   - 支持添加、切换、列出、复制、重命名、删除以及管理多个独立网关。
   - 配置统一存储在 `~/.ccl/config.yaml`，方便备份与迁移。

6. **统一 CLIProxyAPI 运行时与 OAuth**
   - 源码集成 CLIProxyAPI Go SDK，无需安装或管理第二个代理进程。
   - 支持 `ccl auth chatgpt` 和 `ccl auth gemini`；ChatGPT 在底层使用 CLIProxyAPI 的 Codex OAuth backend。
   - OAuth 凭据保存在 `~/.ccl/auth`，运行时仅绑定本机回环地址，并使用随机会话 key。
   - ChatGPT 默认走 OpenAI Responses，Gemini 默认走 OpenAI Chat；手动 API key provider 与 OAuth provider 共用同一套 SDK 运行时。

---

## 🚀 安装与编译

### 快速安装
```bash
npm install -g @claudecodelaunch/ccl
```

### 预编译二进制
前往 [GitHub Releases](https://github.com/claude-code-launch/ccl/releases) 下载适合您平台的压缩包：

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

### 源码编译
```bash
git clone https://github.com/claude-code-launch/ccl.git
cd ccl
go build -o ccl .
```

### Go 安装
```bash
go install github.com/claude-code-launch/ccl@latest
```

---

## 🛠️ 命令参考

### `ccl auth` — 登录订阅账号

```bash
# ChatGPT OAuth，默认使用 OpenAI Responses
ccl auth chatgpt

# Google OAuth，默认通过 OpenAI Chat 使用 Gemini 模型
ccl auth gemini

# 不自动打开浏览器
ccl auth chatgpt --no-browser

# 覆盖回调端口或上游协议
ccl auth chatgpt --callback-port 1455
ccl auth chatgpt --protocol chat
ccl auth gemini --protocol responses
```

登录成功后，ccl 会创建或更新同名 provider 并立即设为当前 provider。`chatgpt` 使用 CLIProxyAPI 的 Codex OAuth backend；`gemini` 使用 CLIProxyAPI v7 的 Google/Antigravity OAuth backend。旧版创建的 `oauthProvider: codex` 仍可运行，并会在下次执行 `ccl auth chatgpt` 时迁移为 `chatgpt`。启动 `ccl`、`ccl set <provider>`、`ccl models` 或 `ccl doctor` 时，内嵌代理按需启动，并在命令退出时关闭，不需要常驻服务。`ccl set` 会通过临时本地 endpoint 和会话 key 获取、测试模型，但不会把它们写回配置。多个 OAuth backend 可以同时登录，但每个 provider 只加载、刷新和调度自己的凭据与模型。

### `ccl set` — 添加/更新 Provider

```bash
# 交互式选择已有 provider 或新建
ccl set

# 直接指定名称新建或更新
ccl set my-provider
```

无参数时弹出 **Provider 选择框**（↑↓ 选择，Enter 确认）：

```
┌─ Select a provider or create new: ─────────────────┐
│                                                     │
│  ▸ + Create new provider                            │
│    deepseek                                         │
│    oc1 (active)                                     │
│    zhipu                                            │
│                                                     │
│  ↑↓ choose · enter confirm · esc cancel             │
└─────────────────────────────────────────────────────┘
```

选择后进入**全屏 TUI 配置向导**，分 6 步完成：

| 步骤 | 内容 | 操作 |
|------|------|------|
| Step 1 | **凭据配置** — Endpoint URL + API Key | ↑↓ 切换输入框 · Enter 下一步 |
| Step 2 | **配置模式** — Auto / Manual | ↑↓ 选择 · Enter 确认 |
| Step 3 | **Slot 映射** — Opus / Sonnet / Haiku / Custom / Subagent | ↑↓ 选槽位 · Enter 进入模型列表 · 打字过滤 · Enter 锁定 |
| Step 4 | **Context & Compact** — 上下文槽位与自动压缩预设 | Enter 切换槽位或轮换预设 |
| Step 5 | **Reasoning Effort** — Default + low ~ ultracode | ↑↓ 选择 · Enter 确认 |
| Step 6 | **核对保存** — 确认配置并设为激活 | ←→ 切换是/否 · Enter 保存 |

Context & Compact 提供以下 Provider 级预设：

| 预设 | 上下文窗口 | 自动压缩 | 说明 |
|------|-----------:|---------:|------|
| Preserve | 保留现值 | 保留现值 | 默认保护自定义或旧配置，不做静默迁移 |
| Confirmed 200K | 200,000 | 70%（约 140K） | 仅在你确认上游至少支持 200K 时选择 |
| 1M | 1,000,000 | 90%（约 900K） | 为选中的槽位添加 `[1m]` |
| Off | 未管理 | 未管理 | 明确移除 ccl 管理的 compact 设置 |

ccl 只对精确模型 ID `gpt-5.6-sol`、`gpt-5.6-terra`、`gpt-5.6-luna` 显示 1M / 90% 推荐；未知模型不会自动假定为 200K。Compact 环境变量作用于整个 Provider，而 `[1m]` 是每个槽位的标记，因此混合不同上下文能力的模型时应按最小已确认容量设置。旧的 `[1m] + CLAUDE_CODE_AUTO_COMPACT_WINDOW=1000000` 配置继续兼容，缺少百分比时显示为 legacy，不会在查看配置时自动改写。

页面间通过 `Tab` / `Shift+Tab` 或底部按钮 `[Next]` / `[Back]` 导航。

### Provider 配置管理

```bash
# 列出所有 provider
ccl ls
ccl ls -a             # 展开详情与完整模型池（默认显示扫描表）
ccl provider ls       # 完整语义入口，输出同 ccl ls

# 复制配置
ccl cp source target
ccl provider cp source target

# 重命名
ccl mv old-name new-name
ccl provider mv old-name new-name

# 删除
ccl rm name
ccl provider rm name

# 其他 provider 子命令
ccl provider set my-provider
ccl provider use my-provider
ccl provider map
ccl provider models
ccl provider env
ccl provider doctor
ccl provider preview
```

### `ccl env` — 环境变量管理

```bash
# 列出所有环境变量
ccl env ls

# 设置/修改
ccl env KEY VALUE
ccl provider env KEY VALUE

# 重命名
ccl env mv OLD_KEY NEW_KEY
ccl provider env mv OLD_KEY NEW_KEY

# 删除
ccl env rm KEY
ccl provider env rm KEY
```

### `ccl use` — 切换激活 Provider

```bash
ccl use provider-name
```

### `ccl lang` — 切换显示语言

```bash
# 交互式选择
ccl lang

# 直接指定
ccl lang zh       # 中文
ccl lang en       # English
```

设置后立即生效并持久化到 `~/.ccl/config.yaml`。优先级：`CCL_LANG` 环境变量 > config.yaml > 系统语言。

### `ccl doctor` — 环境诊断

```bash
ccl doctor
ccl provider doctor
```

检查本地依赖、Endpoint 连通性、API 鉴权。**并发测试所有配置模型**，自动将可用模型排在配置前列，显示实时进度条。如果 Claude CLI 未安装，自动触发一键安装。

### `ccl models` — 查看可用模型

```bash
# 查看已配置模型的可用性
ccl models

# 查看 Provider 全部模型
ccl models --all
ccl provider models --all
```

并发测试每个模型，显示 `✓`（可用）或 `✗ (unavailable)`（不可用），带实时进度条。

### `ccl map` — 快速设置 Slot 模型映射

```bash
# 交互式 TUI — 直接进入 Slot 映射页面
ccl map
ccl provider map

# 自动填充 — 自动检测可用模型并填入前 4 个槽位
ccl map auto
ccl map auto my-provider
ccl provider map auto

# 直接指定 — 通过 CLI 参数快速映射
ccl map --opus gpt-5.1 --sonnet gpt-5.1-codex-max
ccl map --opus gpt-5.1 --sonnet gpt-5.1-mini --haiku gpt-4o-mini
ccl map --custom gpt-5.1 my-provider
ccl map --subagent gpt-5.4-mini my-provider
ccl provider map --custom gpt-5.1 my-provider
```

三种模式：交互式 TUI（直接跳转到 Slot 映射页面）、自动检测填充、CLI 参数直接映射。

```bash
ccl ls
```

### `ccl preview` — 预览 Claude Code 注入配置

```bash
ccl preview
ccl provider preview
```

输出当前激活 Provider 会生成的 settings JSON，适合检查 `ANTHROPIC_BASE_URL`、认证变量、slot 模型和 effort 注入结果。

### `ccl update` — 升级

```bash
ccl update
```

支持通过 `npm` / `go install` 一键升级。

### `ccl version` — 查看版本

```bash
ccl version
```

显示当前二进制版本。Release 构建会从 tag 注入版本号。

### `ccl completion` — Shell 补全脚本

```bash
ccl completion zsh
ccl completion bash
ccl completion fish
ccl completion powershell
```

由 Cobra 自动生成对应 shell 的补全脚本。

### `ccl` — 启动 Claude Code

```bash
# 直接启动
ccl

# 透传参数
ccl resume
ccl --dangerously-skip-permissions
ccl claude --dangerously-skip-permissions
```

---

## ⚙️ 配置存储

所有配置统一存储在 `~/.ccl/config.yaml`：

```yaml
active_provider: deepseek
lang: zh-CN
providers:
  deepseek:
    name: deepseek
    type: openai
    endpoint: https://api.deepseek.com
    apikey: sk-xxx
    model: deepseek-chat,deepseek-reasoner
    opusModel: deepseek-reasoner
    sonnetModel: deepseek-chat
    effortLevel: max
  sensenova:
    name: sensenova
    type: anthropic
    endpoint: https://token.sensenova.cn
    apikey: sk-xxx
    anthropicAuth: bearer
  chatgpt:
    name: chatgpt
    type: openai_responses
    endpoint: oauth://codex
    oauthProvider: chatgpt
```

配置字段说明：

- `type: openai`（显示为 `openai(chat)`）：`ccl` 启动内嵌 CLIProxyAPI 的 OpenAI Compatibility runtime。Claude Code 直接连接 SDK 的 `/v1/messages`，由 SDK 转换到上游 **Chat Completions**；`model`、各 Claude slot、Subagent 与 `[1m]` 名称会注册为 SDK 模型路由或别名。
- `type: openai_responses`（显示为 `openai(responses)`）：Claude Code 同样直接连接 CLIProxyAPI，但 SDK 使用 **Responses** API（`/v1/responses`）和 Codex API-key executor。它负责 Codex 客户端标识、请求规范化、流式响应与工具调用转换；`ccl set` 获取模型后，可在核对页的 Protocol 行用 ←→ / Enter 在 `openai(chat)` 与 `openai(responses)` 之间切换。
- Codex 专用路由应填写服务商给出的生成基址，例如 `https://example.com/codex`。ccl 获取模型时会请求 `https://example.com/codex/models`，不会把额外路径写回 endpoint；若填写成 `/codex/v1`，ccl 会提示正确地址并停止，而不是静默修改。Codex 路径在最终核对页默认选择 Responses，仍可手动切换为 Chat。
- 以 `/claude` 或 `/anthropic` 结尾的网关基址按 Anthropic 处理，模型列表请求会自动使用 `<endpoint>/v1/models`；其他 OpenAI 基址使用 `<endpoint>/models`。
- `type: anthropic`：Claude Code 直连该 endpoint，`ccl` 不在请求链路中做协议转换；`model` 只作为 `ccl` 的本地模型池，用于 TUI 列表、`map auto`、默认 slot 映射和可用性检测。Claude Code 访问 `/v1/models` 时看到的是 provider 自己返回的结果。
- `oauthProvider`：让相同的 CLIProxyAPI runtime 使用已保存的 OAuth 凭据。运行时 `endpoint` 和上游凭据会替换为仅本次会话有效的本机地址与随机 Bearer token，不会写回配置文件；OpenAI Chat/Responses provider 也使用相同的会话认证方式，避免 Claude Code 把代理 token 当作新的 Anthropic API key 反复确认。
- Claude Code 运行时默认使用当前 Custom/Sonnet 映射作为子代理模型，将工具并发限制为 `3`，设置 `ENABLE_TOOL_SEARCH=false`，并将 `CLAUDE_CODE_MAX_OUTPUT_TOKENS` 固定为 Claude Code 默认值 `32000`。输出上限与 200K/1M 上下文窗口是两个独立概念；可通过 `ccl env` 覆盖输出上限，但必须位于 `1..128000`。这些值会在手动配置的 Review & Apply 页显示。
- Anthropic 直连时 `endpoint` 建议使用裸域名，例如 `https://token.sensenova.cn`；`ccl set` 会自动去掉常见的 `/v1`、`/v1/messages`、`/v1/models` 后缀，避免 Claude Code 运行时拼成 `/v1/v1/messages`。

## ✅ 本地验证清单

开发时可以用临时 `HOME` 验证配置，不会污染真实 `~/.ccl/config.yaml`：

```bash
go test ./...
go build -o /tmp/ccl-debug .

export CCL_TEST_HOME="$(mktemp -d)"
HOME="$CCL_TEST_HOME" /tmp/ccl-debug set sensenova
HOME="$CCL_TEST_HOME" /tmp/ccl-debug preview
HOME="$CCL_TEST_HOME" /tmp/ccl-debug doctor
HOME="$CCL_TEST_HOME" /tmp/ccl-debug models --all
```

Anthropic 兼容网关（例如 `https://token.sensenova.cn`）应确认：

- `endpoint` 保存为裸域名，不带 `/v1`。
- Bearer 认证时 `preview` 里出现 `ANTHROPIC_AUTH_TOKEN`，不出现 `ANTHROPIC_API_KEY`。
- Effort 选择 Default 时，`preview` 里不出现 `CLAUDE_CODE_EFFORT_LEVEL`。
- 配置了 Custom model 时，`preview` 顶层 `model` 与 `ANTHROPIC_CUSTOM_MODEL_OPTION` 保持一致。

---

## 🔧 CI/CD

推送符合 `v*` 规范的 tag 触发自动编译发布：

```bash
git tag v1.2.0
git push origin v1.2.0
```

GitHub Actions 自动构建 6 个平台二进制并发布到 GitHub Releases + npm。

---

## 📁 目录结构

```text
├── cmd/
│   ├── advanced_config.go     # TUI 配置向导（5 页表单 + 协议探测）
│   ├── auth.go                # Codex / ChatGPT / Gemini OAuth 登录
│   ├── provider.go            # Provider 子命令入口（cp/ls/mv/rm/set/map/models/env/doctor/preview）
│   ├── env.go                 # 环境变量管理（ls/rm/mv）
│   ├── set.go                 # set 命令入口 + RunProviderSet 共享逻辑
│   ├── select.go              # 通用 TUI 选择器组件
│   ├── doctor.go              # 环境及密钥连通性自检
│   ├── install.go             # Claude CLI 自动安装
│   ├── lang_cmd.go            # ccl lang 命令
│   ├── list.go                # ls 命令
│   ├── map.go                 # ccl map 命令（交互式/自动/CLI 三种映射模式）
│   ├── models.go              # 模型列表展示 + 可用性检测
│   ├── root.go                # ccl 主入口 + passthrough 模式
│   ├── preview.go             # 预览 settings JSON
│   ├── update.go              # 自动升级
│   ├── use.go                 # 切换激活 provider
│   └── version.go             # 版本信息
├── internal/
│   ├── claude/                # Claude Code 进程拉起 & 端口注入
│   ├── config/                # yaml 配置文件读写
│   ├── locale/                # 多语言支持（中文 / English）
│   ├── oauthproxy/            # CLIProxyAPI SDK 登录与内嵌运行时
│   ├── protocol/              # Endpoint、模型发现与协议辅助逻辑
│   ├── provider/              # Provider & Config 数据结构
│   └── proxy/                 # 本地 TCP 代理服务
└── main.go
```

## 📄 开源许可

MIT。CLIProxyAPI SDK 的第三方许可见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
