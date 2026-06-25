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
   - 若通过 `ccl set` / `ccl conf set` 手动为某个档位指定了模型，则该档位的自动映射被覆盖，其余未配置的档位仍走自动映射。

2. **零感协议翻译与流式代理**
   - 采用本地轻量级的高性能并发 socket 服务（TCP），自动拦截并完美将 Anthropic 专有的 `Messages` 协议以及 `Streaming (SSE)` 转换为标准的 `OpenAI / Chat Completions` 协议。

3. **交互式 TUI 配置向导**
   - 全新的 bubbletea 驱动的全屏 TUI：多页表单，键盘导航（方向键 / Tab / Enter / Esc），实时协议探测与模型拉取。
   - 支持 6 档 Reasoning Effort（`low` ~ `ultracode`）、每个槽位独立配置模型、一键启用 1M 上下文。
   - **多语言支持**：中文 / English，运行时通过 `ccl lang` 随时切换。

4. **智能环境探针与诊断 (`ccl doctor`)**
   - 自动检查本地环境依赖（Node.js, Claude CLI）。
   - 如果系统未安装 Claude CLI，`ccl` 将触发**全自动静默安装**。
   - 提供连接探针，对各 Provider 的 Endpoint 连通性、API 鉴权密钥进行安全测试。

5. **多通道配置与灵活切换**
   - 支持添加、切换、列出、复制、重命名、删除以及管理多个独立网关。
   - 配置统一存储在 `~/.ccl/config.yaml`，方便备份与迁移。

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

### `ccl set` / `ccl conf set` — 添加/更新 Provider

```bash
# 交互式选择已有 provider 或新建
ccl set

# 直接指定名称新建或更新
ccl set my-provider
ccl conf set my-provider
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

选择后进入**全屏 TUI 配置向导**，分 5 页完成：

| 页面 | 内容 | 操作 |
|------|------|------|
| Page 0 | **凭据配置** — Endpoint URL + API Key | ↑↓ 切换输入框 · Enter 下一步 |
| Page 1 | **Slot 映射** — Opus / Sonnet / Haiku / Custom 模型选择 | ↑↓ 选槽位 · Enter 进入模型列表 · 打字过滤 · Enter 锁定 |
| Page 2 | **1M 上下文** — 每槽位独立开关 | Space 切换 · Enter 下一步 |
| Page 3 | **Reasoning Effort** — low ~ ultracode 6 档 | ↑↓ 选择 · Enter 确认 |
| Page 4 | **核对保存** — 确认配置并设为激活 | ←→ 切换是/否 · Enter 保存 |

页面间通过 `Tab` / `Shift+Tab` 或底部按钮 `[Next]` / `[Back]` 导航。

### `ccl conf` — Provider 配置管理

```bash
# 列出所有 provider
ccl conf ls
ccl conf ls -a        # 显示全部模型（默认只显示前 3 个）

# 复制配置
ccl conf cp source target

# 重命名
ccl conf mv old-name new-name

# 删除
ccl conf rm name
```

### `ccl env` — 环境变量管理

```bash
# 列出所有环境变量
ccl env ls

# 设置/修改
ccl env KEY VALUE

# 重命名
ccl env mv OLD_KEY NEW_KEY

# 删除
ccl env rm KEY
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
```

检查本地依赖、Endpoint 连通性、API 鉴权。如果 Claude CLI 未安装，自动触发一键安装。

### `ccl list` — 查看所有 Provider

```bash
ccl list
# 或
ccl conf ls
```

### `ccl update` — 升级

```bash
ccl update
```

支持通过 `npm` / `go install` 一键升级。

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
```

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
│   ├── conf.go                # Provider 配置管理（cp/ls/mv/rm/set）
│   ├── env.go                 # 环境变量管理（ls/rm/mv）
│   ├── set.go                 # set 命令入口 + RunConfSet 共享逻辑
│   ├── select.go              # 通用 TUI 选择器组件
│   ├── doctor.go              # 环境及密钥连通性自检
│   ├── install.go             # Claude CLI 自动安装
│   ├── lang_cmd.go            # ccl lang 命令
│   ├── list.go                # list 命令
│   ├── models.go              # 模型列表展示
│   ├── root.go                # ccl 主入口 + passthrough 模式
│   ├── settings.go            # 预览 settings.json
│   ├── update.go              # 自动升级
│   ├── use.go                 # 切换激活 provider
│   └── version.go             # 版本信息
├── internal/
│   ├── claude/                # Claude Code 进程拉起 & 端口注入
│   ├── config/                # yaml 配置文件读写
│   ├── locale/                # 多语言支持（中文 / English）
│   ├── protocol/              # Anthropic ↔ OpenAI 协议转换
│   ├── provider/              # Provider & Config 数据结构
│   └── proxy/                 # 本地 TCP 代理服务
└── main.go
```

## 📄 开源许可

MIT
