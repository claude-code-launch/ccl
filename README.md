# ccl: Claude Code 多网关智能代理启动器 (Multi-Provider Launcher)

`ccl` 是一个专门为 Anthropic 官方 CLI 工具 **Claude Code** 开发的多模型网关代理与极速启动器。

它可以帮助你在运行 Claude Code 时，无缝对接 OpenAI 兼容格式的网关（如官方 DeepSeek、SiliconFlow、OpenRouter、OneAPI 等），实现超低成本运行。

## ✨ 核心亮点

1. **智能多档模型映射 (无需复杂配置)**
   - 当 `config.yaml` 里的 `model` 字段留空时，`ccl` 将进入 **「智能协议代理映射模式」**。
   - 自动在启动时拉取接口提供商的可用模型库。
   - 动态分析 Claude Code 的模型档位（Opus / Sonnet / Haiku），匹配最佳替代：
     - 💎 **Opus 强推理档** (Claude Opus, Claude 4.8 / 4.7 等) $\Rightarrow$ 优先匹配 `deepseek-reasoner` (R1) 或 `o1`、`o3-mini`、`gpt-4o`。
     - 🚀 **Sonnet 黄金档** (Claude 3.5 Sonnet 等) $\Rightarrow$ 优先匹配 `deepseek-chat` (V3)、`gpt-4o`、`claude-3-5-sonnet`。
     - ⚡ **Haiku 极速档** (Claude 3.5 Haiku 等) $\Rightarrow$ 优先匹配 `gpt-4o-mini`、`gpt-3.5-turbo`。

2. **零感协议翻译与流式代理**
   - 采用本地轻量级的高性能并发 socket 服务（TCP），自动拦截并完美将 Anthropic 专有的 `Messages` 协议以及 `Streaming (SSE)` 转换为标准的 `OpenAI / Chat Completions` 协议。
   - 完美适配 Claude Code CLI 所有的 Tools（工具调用）和 System Prompt，使用体验 100% 丝滑。

3. **智能环境探针与诊断 (`ccl doctor`)**
   - 自动检查本地环境依赖（Node.js, Claude CLI）。
   - 如果系统未安装 Claude CLI，`ccl` 将触发**全自动静默安装**，无需你手动运行 `npm install -g`。
   - 提供连接探针，对各 Provider 的 Endpoint 连通性、API 鉴权密钥进行安全测试。

4. **多通道配置与灵活切换**
   - 支持添加、切换、列出、删除以及管理多个独立网关。
   - 极简 CLI 交互界面，支持漂亮的终端可视化菜单。

---

## 🚀 安装与编译

### 快速安装
```
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
# 克隆仓库
git clone https://github.com/claude-code-launch/ccl.git
cd claude-code-launch

# 编译生成 ccl 执行文件
go build -o ccl main.go
```
### 方法三：Go 安装
确保您本地已经安装了 Go（推荐 1.22+）。

```
go install github.com/claude-code-launch/ccl@latest
```
⚠️ 注意：安装完成后，您需要将 GOBIN 目录添加到系统的 PATH 环境变量中，否则可能无法在终端直接运行 ccl 命令。

如何配置 PATH 环境变量？
根据您的操作系统，选择对应的配置方式：

macOS / Linux (Bash/Zsh)
打开终端并运行以下命令（根据你使用的 shell，修改 ~/.zshrc 或 ~/.bashrc）：

```
export GOPATH=$(go env GOPATH)
export PATH=$PATH:$GOPATH/bin
source ~/.zshrc # 如果使用的是 bash，请改为 source ~/.bashrc
```

Windows (PowerShell)
在 PowerShell 中运行以下命令（仅对当前用户永久生效）：

```PowerShell
[Environment]::SetEnvironmentVariable("Path", $env:Path + ";$(go env GOPATH)\bin", "User")
```
注：重启终端后生效。

## 🛠️ 自动化发布指南 (CI/CD)

项目配置了 GitHub Actions 自动化工作流。当需要发布新版本时，无需手动编译多平台包，只需直接在本地推送标签即可：

```bash
# 1. 创建符合 v* 规范的版本 tag
git tag v1.0.1

# 2. 推送到 GitHub
git push origin v1.0.1
```
GitHub Actions 会自动触发并执行以下操作：
- 拉取代码，配置 Go 1.24 运行环境。
- 使用 `-ldflags="-s -w"` 深度压缩二进制体积（剔除符号调试表，缩减 35%+）。
- 跨平台交叉编译 macOS、Linux、Windows 5 大核心架构的目标文件。
- 自动提取标签之间的 Commit 历史生成发布日志并发布。

---

## 🛠️ 快速上手

### 极速免配置模式 (推荐 🚀)
如果你已经在终端的环境变量中配置了 `OPENAI_API_KEY` 和 `OPENAI_BASE_URL`，`ccl` 将会自动识别并直接以此作为服务源，**完全零配置运行**！

```bash
# 1. 注入你的环境变量（例如使用 DeepSeek 官方）
export OPENAI_API_KEY="sk-your-deepseek-api-key"
export OPENAI_BASE_URL="https://api.deepseek.com" # 选填，默认指向官方 OpenAI

# 2. 直接一行启动
ccl
```
> 💡 在此模式下，依旧享受超强的 **「智能模型映射」**：常规对话走 `deepseek-chat`，深度推理全自动路由至 `deepseek-reasoner`！

---

### 交互配置模式
如果你需要管理多个网关通道，可以使用 `ccl` 的配置管理系统：

#### 1. 添加你的 AI 接口服务商 (Provider)

运行 `ccl add` 命令，开始添加。如果你使用的是 DeepSeek 官方，可以配置如下：

```bash
./ccl add
```

交互引导中填入信息：
* **Provider Name**: `deepseek` (名字可自定义)
* **Provider Type**: 选择 `openai` (哪怕是 DeepSeek、OpenRouter 均选此项以启用本地协议代理)
* **API Key**: 填入你的 DeepSeek API Key (形如 `sk-...`)
* **Endpoint**: 填入 `https://api.deepseek.com` 或中转服务地址（不带 `/v1/chat/completions` 后缀）
* **Model**: **[推荐留空]** 直接按回车跳过。这样代理层将为你开启全自动的「智能模型映射」，在发送普通对话时跑 `deepseek-chat` (V3)，在调用深度推理时完美、低延迟地跑 `deepseek-reasoner` (R1)。

### 2. 查看与切换 Provider

你可以管理和随时切换当前处于 Active 激活状态的服务商：

```bash
# 查看所有已添加的服务商 (带有 * 的为当前激活)
./ccl list

# 切换到指定的 provider
./ccl use deepseek
```

### 3. 环境诊断

在正式跑 Claude 之前，可以测试网关的健康度和密钥是否有效：

```bash
./ccl doctor
```

如果检测到本地没有全局安装 `@anthropic-ai/claude-code`，它会提示并尝试为你一键静默安装。

### 4. 开启 Claude Code 奇妙旅程

直接输入 `ccl`，即可丝滑进入 Claude Code CLI 原生界面：

```bash
# 启动 Claude Code 交互，所有请求均自动经本地 ccl 代理安全转换
./ccl

# 支持将后续的所有参数直接透传给 Claude Code：
./ccl resume
./ccl --dangerously-skip-permissions

# 也可以显式使用 claude 命令来透传后面的所有参数：
./ccl claude resume
./ccl claude --dangerously-skip-permissions

# 你也可以像原来一样跟上其他的子命令或路径：
./ccl --help
./ccl /compact
```

---

## 📁 目录结构

```text
├── cmd/                # CLI 命令定义 (Cobra)
│   ├── add.go          # 添加/更新提供商 (交互式)
│   ├── delete.go       # 删除提供商
│   ├── doctor.go       # 环境及密钥连通性自检
│   ├── list.go         # 列表展示提供商
│   ├── root.go         # ccl 主入口及 Claude 进程拉起
│   └── use.go          # 快速切换激活提供商
├── internal/
│   ├── claude/         # Claude Code CLI 自动安装、进程拉起及端口注入逻辑
│   ├── config/         # 极简 yaml 配置文件加解密与载入
│   ├── protocol/       # Anthropic <=> OpenAI 核心协议数据结构转换与 Stream 事件转换
│   ├── provider/       # 提供商实体定义
│   └── proxy/          # 本地自愈并发 TCP 代理服务、模型自动感知与映射
└── main.go             # 引导文件
```

## 📄 开源许可

本项目采用 MIT 协议开源。
