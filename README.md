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

`ccl doctor` 会检查本地依赖；如果还没装 Claude Code CLI，会尝试自动安装。

### 2. 选一条入门路径

#### 路径 A：用订阅账号登录（最简单）

```bash
# 任选其一
ccl auth gpt        # GPT / Codex (OpenAI 订阅)
ccl auth gemini     # Google Gemini
ccl auth grok       # xAI Grok
ccl auth copilot    # GitHub Copilot
ccl auth kimi       # Kimi / Moonshot
ccl auth claude     # Anthropic Claude 订阅

# 登录成功后直接启动
ccl
```

登录成功后，`ccl` 会自动创建并切换到对应 provider。多账号可以加别名：

```bash
ccl auth gpt work
ccl auth gpt personal
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
| 不知道从哪开始 | 有订阅就 `ccl auth ...`；有 API Key 就 `ccl set` |
| 启动后模型不对 | `ccl map` 或 `ccl set` 重新映射 Opus / Sonnet / Haiku |
| 连不上 / 鉴权失败 | `ccl doctor`，再 `ccl preview` 看注入了什么环境变量 |
| 多个账号互相覆盖 | 登录时加别名：`ccl auth gpt work` |
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
   内嵌 CLIProxyAPI Go SDK。OpenAI Chat、OpenAI Responses、Codex 与 OAuth provider 统一暴露本机 `/v1/messages`；Anthropic 兼容网关保持直连。

3. **交互式 TUI 配置**  
   全屏向导配置 endpoint、协议、模型槽位、上下文压缩等；支持中文 / English（`ccl lang`）。

4. **环境诊断**  
   `ccl doctor` 检查依赖、连通性、鉴权，并批量测模型可用性。

5. **多通道 / 多账号**  
   配置在 `~/.ccl/config.yaml`；OAuth 凭据在 `~/.ccl/auth`。可随时 `use` / `ls` / `cp` / `mv` / `rm`。

6. **订阅 OAuth 一键接入**  
   `gpt` / `gemini` / `grok` / `copilot` / `kimi` / `claude`，支持多账号别名；token 会在运行时刷新。

---

## 命令速查

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

### `ccl auth` — 登录订阅账号

```bash
ccl auth gpt
ccl auth gemini
ccl auth grok
ccl auth copilot
ccl auth kimi
ccl auth claude

# 多账号别名
ccl auth gpt work
ccl auth gemini personal

# 可选
ccl auth gpt --no-browser
ccl auth gpt --callback-port 1455
```

| provider | backend | 协议 | 登录方式 |
| --- | --- | --- | --- |
| `gpt` | codex | `openai(responses)` | OpenAI OAuth 回调 |
| `copilot` | codex | `openai(responses)` | GitHub device-code |
| `gemini` | antigravity | `openai(chat)` | Google/Antigravity OAuth |
| `grok` | xai | `openai(chat)` | xAI device-code |
| `kimi` | kimi | `openai(chat)` | Kimi/Moonshot device-code |
| `claude` | claude | `anthropic` | Anthropic OAuth 回调 |

说明：

- 不带别名时，会从凭据文件名派生 provider 名（如 `gpt-alice@example.com`），避免多账号互相覆盖。
- 每条 provider 通过 `oauthAccountCredential` 绑定具体账号文件。
- 不再提供 `--protocol` 覆盖；各 OAuth backend 协议固定。
- 旧版 `ccl auth chatgpt` 仍可用，会规范为 `gpt`。
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
- 启动时若上游 model list 没有对应首选模型，会清除该首选默认并回退自动发现映射。
- **Fast mode**（约 1.5x 速度、更高用量）仅 `gpt` / `copilot` 有意义：可在 `ccl set` 的「核对并应用 / Review & Apply」页编辑，也可在 Claude Code 内用 `/fast` 开关。

### `ccl set` — 添加 / 更新 Provider

```bash
ccl set                 # 交互选择已有或新建
ccl set my-provider     # 指定名称
```

TUI 大致流程：

| 步骤 | 内容 |
|------|------|
| Step 1 | Endpoint + API Key |
| Step 2 | Auto / Manual 配置模式 |
| Step 3 | Opus / Sonnet / Haiku / Custom / Subagent 映射 |
| Step 4 | 扩展上下文 `[1m]` + Auto Compact 预设 |
| Step 5 | 核对 Connection / Mapping / Runtime 并保存 |

Context & Compact：

1. **Extended Context `[1m]`**（按槽位）：声明该模型 ID 支持扩展上下文。
2. **Auto Compact**（Provider 全局）：设置默认上下文与绝对压缩窗口。

| 压缩预设 | 默认上下文 | 自动压缩窗口 | 说明 |
|---------|-----------:|---------------:|------|
| Custom (preserve) | 保留现值 | 保留现值 | 保护自定义配置 |
| Claude default | 未管理 | 未管理 | 删除 ccl 覆盖 |
| Switch-safe 300K / 200K | 300,000 | 200,000 | 常切换标准上下文时较稳妥 |
| Balanced 500K / 400K | 500,000 | 400,000 | 容量与余量平衡 |
| Maximum 1M / 900K | 1,000,000 | 900,000 | 超长会话 |

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
ccl provider doctor
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

### `ccl models` / `ccl doctor` / `ccl preview`

```bash
ccl models              # 测试已配置模型
ccl models --all        # 查看并测试 provider 全部模型
ccl doctor              # 依赖 + 连通性 + 鉴权 + 模型可用性
ccl preview             # 预览将注入 Claude Code 的 settings JSON
```

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
```

字段要点：

- `type: openai`（显示 `openai(chat)`）：经 CLIProxyAPI 转到上游 Chat Completions。
- `type: openai_responses`（显示 `openai(responses)`）：经 SDK 走 Responses API；Codex 路径默认选 Responses，可在核对页切换。
- `type: anthropic`：Claude Code 直连 endpoint，不做协议转换。
- `oauthProvider`：使用已保存的 OAuth 凭据；运行时使用本机会话地址与随机 key，不写回配置。
- `bypass_mode`：全局是否自动附加 `--dangerously-skip-permissions`。
- Anthropic 直连时 `endpoint` 建议裸域名（如 `https://token.sensenova.cn`），避免拼出 `/v1/v1/messages`。
- 运行时默认：子代理模型优先 Custom/Sonnet；工具并发默认 `3`；`ENABLE_TOOL_SEARCH=false`；`CLAUDE_CODE_MAX_OUTPUT_TOKENS` 默认 `32000`。可在 Review & Apply 页或 `ccl env` 覆盖。

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
ccl auth gpt work
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
│   ├── bypass.go              # ccl bypass（权限旁路开关）
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
│   ├── claude/                # Claude Code 进程拉起
│   ├── config/                # yaml 配置读写
│   ├── locale/                # 多语言
│   ├── modelrouting/          # 档位启发式映射
│   ├── oauthproxy/            # CLIProxyAPI 登录与内嵌运行时
│   ├── protocol/              # endpoint 规范化与探测
│   └── provider/              # Provider / Config 结构
└── main.go
```

---

## 开源许可

MIT。CLIProxyAPI SDK 的第三方许可见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
