# ccl: Claude Code 多网关智能代理启动器 (Multi-Provider Launcher)

`ccl` 是一个专门为 Anthropic 官方 CLI 工具 **Claude Code** 开发的多模型网关代理与极速启动器。

它可以帮助你在运行 Claude Code 时，无缝对接 OpenAI 兼容格式的网关（如官方 DeepSeek、SiliconFlow、OpenRouter、OneAPI 等），实现超低成本运行。

## ✨ 核心亮点

1. **智能多档模型映射 (无需复杂配置)**
   - 当槽位未手动配置时，`ccl` 自动进入 **「智能协议代理映射模式」**。
   - 自动在启动时拉取接口提供商的可用模型库，按关键词动态分析并分配到各档位：
     - 💎 **Opus 强推理档** → 优先匹配 `deepseek-reasoner` (R1) 或 `o1`、`o3-mini`、`gpt-4o`
     - 🚀 **Sonnet 黄金档** → 优先匹配 `deepseek-chat` (V3)、`gpt-4o`、`claude-3-5-sonnet`
     - ⚡ **Haiku 极速档** → 优先匹配 `gpt-4o-mini`、`gpt-3.5-turbo`
   - 若通过 `ccl set` 手动为某个档位指定了模型，则该档位的自动映射被覆盖，其余未配置的档位仍走自动映射。

   > **两种模式的关系**：自动映射是兜底策略，手动槽位配置优先级更高。你可以只配置 Opus 档（指向推理模型），让 Sonnet / Haiku 继续走自动映射——完全按需混用。

2. **零感协议翻译与流式代理**
   - 采用本地轻量级的高性能并发 socket 服务（TCP），自动拦截并完美将 Anthropic 专有的 `Messages` 协议以及 `Streaming (SSE)` 转换为标准的 `OpenAI / Chat Completions` 协议。
   - 完美适配 Claude Code CLI 所有的 Tools（工具调用）和 System Prompt，使用体验 100% 丝滑。

3. **精细模型槽位配置 (`ccl set` 高级模式)**
   - 支持对 Claude Code 的每个模型档位（Default / Opus / Sonnet / Haiku / Custom）独立指定模型。
   - 全新的 **双栏 TUI 交互界面**：左侧选项（1M 开关 + Done），右侧模型列表（支持实时输入过滤）。
   - **一键启用 1M 上下文**：为指定槽位开启 `[x] 1M` 后，自动在模型名添加 `[1m]` 后缀，并注入 `CLAUDE_CODE_AUTO_COMPACT_WINDOW=1000000` 环境变量。
   - 外层槽位列表实时显示已启用 1M 的槽位红色 `⚡1M` 标识。

4. **智能环境探针与诊断 (`ccl doctor`)**
   - 自动检查本地环境依赖（Node.js, Claude CLI）。
   - 如果系统未安装 Claude CLI，`ccl` 将触发**全自动静默安装**，无需你手动运行 `npm install -g`。
   - 提供连接探针，对各 Provider 的 Endpoint 连通性、API 鉴权密钥进行安全测试。

5. **多通道配置与灵活切换**
   - 支持添加、切换、列出、删除以及管理多个独立网关。
   - 极简 CLI 交互界面，支持漂亮的终端可视化菜单。

---

## 🚀 安装与编译

### 快速安装
```bash
npm install -g @claudecodelaunch/ccl
```

### 方法一：直接下载预编译二进制
我们利用 GitHub Actions 实现了完美的 CI/CD 流程，所有发布版本均包含多平台的开箱即用二进制。

请前往 [GitHub Releases](https://github.com/claude-code-launch/ccl/releases) 页面，下载适合您平台的压缩包：
- **Apple macOS**: `ccl-darwin-amd64` (Intel) / `ccl-darwin-arm64` (Apple Silicon M1/M2/M3)
- **Linux**: `ccl-linux-amd64` / `ccl-linux-arm64`
- **Windows**: `ccl-windows-amd64.exe`

下载后将其移动到您的系统 `PATH` 目录（例如 macOS/Linux 下的 `/usr/local/bin`），并赋予执行权限：
```bash
chmod +x ccl-darwin-arm64
mv ccl-darwin-arm64 /usr/local/bin/ccl
```

### 方法二：本地源码编译
如果您希望从源码编译，确保您本地已经安装了 Go (推荐 1.22+)。

```bash
git clone https://github.com/claude-code-launch/ccl.git
cd ccl
go build -o ccl main.go
```

### 方法三：Go 安装
```bash
go install github.com/claude-code-launch/ccl@latest
```

⚠️ 安装完成后，需将 `$GOPATH/bin` 添加到系统 `PATH`：

```bash
export GOPATH=$(go env GOPATH)
export PATH=$PATH:$GOPATH/bin
source ~/.zshrc  # 或 ~/.bashrc
```

---

## 🛠️ 快速上手

### 极速免配置模式 (推荐 🚀)
如果你已经在终端的环境变量中配置了 `OPENAI_API_KEY` 和 `OPENAI_BASE_URL`，`ccl` 将会自动识别并直接以此作为服务源，**完全零配置运行**：

```bash
export OPENAI_API_KEY="sk-your-deepseek-api-key"
export OPENAI_BASE_URL="https://api.deepseek.com"
ccl
```

### 交互配置模式

#### 1. 添加 / 更新 Provider

```bash
ccl set
# 或直接指定名称
ccl set deepseek
```

向导会引导你依次填写：

| 步骤 | 说明 |
|------|------|
| Provider Name | 唯一标识符，如 `deepseek`、`openrouter` |
| Endpoint URL | API 地址，如 `https://api.deepseek.com` |
| API Key | 密钥，本地存储 |
| 协议自动探测 | 自动识别 Anthropic / OpenAI 兼容协议 |
| 模型池 | 自动从接口拉取，或手动输入（逗号分隔） |
| **高级槽位配置** | 见下方说明（默认开启） |
| Effort Level | `low / medium / high / xhigh / max / ultracode` |

#### 高级槽位配置（双栏 TUI）

进入高级配置后，外层以列表形式展示五个槽位：

```
Claude Slot Mapping
Select a slot to configure its model, or select [ ] 1m to toggle.

> Default (general) — current: devstral-2512  ⚡1M
  Opus — current: mistral-medium-2505
  Sonnet — current: (not set)
  Haiku — current: (not set)
  Custom (user-defined slot) — current: (not set)
  Done
```

选中某个槽位后，进入**双栏 TUI**：

```
Configure: Opus

╭────────────────────────────╮  ╭───────────────────────────╮
│ Slot: Opus                 │  │ / type to filter...       │
│ Model: glm-5.2             │  │   codestral-2512          │
│                            │  │ > glm-5.2                 │
│ > [x] Enable 1M Context    │  │   glm-4.5-air             │
│   Done                     │  │   mistral-medium-2505     │
│                            │  │   …  3/59                 │
│   Tab → model list         │  ╰───────────────────────────╯
╰────────────────────────────╯

↑↓ Navigate  Enter Select  Tab → model list  Esc Cancel
```

| 操作 | 说明 |
|------|------|
| `↑ ↓` | 在左侧选项（1M / Done）间导航 |
| `Enter` on `[x] Enable 1M Context` | 切换 1M 开关 |
| `Enter` on `Done` | 确认并返回槽位列表 |
| `Tab` | 切换到右侧模型列表 |
| 右侧输入字符 | 实时过滤模型 |
| `Enter`（右侧） | 选中模型，自动跳回左侧 |
| `Esc` | 取消，返回槽位列表 |

> 启用 1M 后，该槽位模型名自动追加 `[1m]` 后缀（如 `glm-5.2[1m]`），并在 provider 环境变量中注入 `CLAUDE_CODE_AUTO_COMPACT_WINDOW=1000000`。

#### 2. 查看与切换 Provider

```bash
# 查看所有已添加的服务商（* 为当前激活）
ccl list

# 切换到指定 provider
ccl use deepseek
```

#### 3. 环境诊断

```bash
ccl doctor
```

如果检测到本地没有全局安装 `@anthropic-ai/claude-code`，会提示并尝试一键静默安装。

#### 4. 启动 Claude Code

```bash
# 直接启动
ccl

# 透传参数给 Claude Code
ccl resume
ccl --dangerously-skip-permissions
ccl claude --dangerously-skip-permissions
```

#### 5. 一键升级

```bash
ccl update
```

自动查询最新版本，支持通过 `npm` 或 `go install` 一键覆盖更新。

---

## 🔧 自动化发布 (CI/CD)

推送符合 `v*` 规范的 tag 即可触发 GitHub Actions 自动编译多平台包并发布：

```bash
git tag v1.1.0
git push origin v1.1.0
```

---

## 📁 目录结构

```text
├── cmd/
│   ├── set.go              # 添加/更新提供商（交互式向导 + 高级槽位配置）
│   ├── slot_config_tui.go  # 双栏 TUI：模型选择 + 1M 上下文开关
│   ├── del.go              # 删除提供商
│   ├── doctor.go           # 环境及密钥连通性自检
│   ├── list.go             # 列表展示提供商
│   ├── root.go             # ccl 主入口及 Claude 进程拉起
│   ├── update.go           # 自动检查并更新 ccl 版本
│   └── use.go              # 快速切换激活提供商
├── internal/
│   ├── claude/             # Claude Code CLI 自动安装、进程拉起及端口注入
│   ├── config/             # yaml 配置文件读写
│   ├── protocol/           # Anthropic ↔ OpenAI 协议转换 & Stream 事件转换
│   ├── provider/           # 提供商实体定义（含槽位模型字段）
│   └── proxy/              # 本地并发 TCP 代理服务、模型自动感知与映射
└── main.go
```

## 📄 开源许可

本项目采用 MIT 协议开源。

