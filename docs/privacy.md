# ccl Privacy Policy / 隐私政策

Effective date / 生效日期: 2026-10-21

[English](#english) · [中文](#中文) · [Home / 首页](https://claude-code-launch.github.io)

## English

This policy explains how **ccl** (<https://claude-code-launch.github.io>), the
application shown as "ccl" on the Google sign-in screen, accesses, uses,
stores and shares data, including data from your Google account.

### 1. About ccl

ccl is a free, open-source command-line tool for software developers. It
starts Anthropic's Claude Code coding assistant and connects it to the model
provider you choose. Source code: <https://github.com/claude-code-launch/ccl>.

ccl runs entirely on your own computer. Its developers operate **no servers,
no user accounts, no analytics and no telemetry**, and do not receive,
collect or store any of your data.

### 2. Data ccl processes on your computer

| Data | Where it is kept | Why |
| --- | --- | --- |
| Model provider settings, API keys and OAuth tokens for services you connect | `~/.ccl/` on your computer, owner-only permissions (0600) | To start Claude Code with the provider you chose |
| Google OAuth token (only if you turn on Google Drive sync) | `~/.ccl/cloud/` on your computer, owner-only permissions | To read and write ccl's sync file in your Google Drive |

ccl sends network requests only to services you configure (model providers
you add, Google Drive or iCloud Drive for sync) and to GitHub when you run
`ccl update`. It contains no analytics, telemetry or advertising.

### 3. Google user data

Google Drive sync is optional and is off until you run `ccl cloud login` and
approve access in your browser.

**Scope requested.** ccl requests exactly one Google OAuth scope:

- `https://www.googleapis.com/auth/drive.appdata` — the hidden application
  data folder that belongs to ccl in your Google Drive.

ccl does not request your name, email address, profile, contacts or any
other Google scope. With this scope ccl cannot see, open or change any other
file in your Google Drive.

**Data accessed.** ccl creates, reads, updates and deletes one file,
`ccl-sync.bundle`, in that application data folder. The file contains
encrypted snapshots of your ccl configuration (`config.yaml` and
`auth/*.json`, that is, your provider settings and tokens) and the profile
key used to decrypt them. ccl does not read or collect any other Google user
data.

**How it is used.** The file is used for one purpose only: so you can copy
your ccl configuration between your own computers (`ccl cloud push`,
`ccl cloud pull`). It is not used for anything else.

**Storage and security.** The configuration is compressed and encrypted with
AES-256-GCM on your computer before upload, and it is sent to Google only
over HTTPS. The encrypted file is stored in your own Google Drive. The
profile key is kept in the same application data folder, so a new computer
only needs to sign in to the same Google account. This means anyone who can
access your Google account, or who holds the ccl authorization token from one
of your computers, can decrypt the synced data. Sign-in uses Google's OAuth
2.0 flow with PKCE in your own browser; ccl never sees your Google password.

**Sharing and disclosure.** ccl does not share, sell, transfer or disclose
Google user data to the ccl developers or to any third party. Google user
data is not sent to any AI or machine-learning model, is not used to train
or improve such models, and is not used for advertising, credit decisions or
any purpose other than the sync feature described above. No human reads it.

**No image generation or NCII.** ccl works only with text and source code.
It does not use Google APIs, or Google user data, to generate, edit or
process images, video or audio, and never for AI-generated non-consensual
intimate imagery (NCII) or any other sexual content.

**Limited Use.** ccl's use and transfer to any other app of information
received from Google APIs will adhere to the
[Google API Services User Data Policy](https://developers.google.com/terms/api-services-user-data-policy),
including the Limited Use requirements.

**Retention and deletion.** The sync file stays in your Google Drive until
you delete it. The local Google token stays on your computer until you sign
out. You can:

- run `ccl cloud logout --delete-remote --yes` to permanently delete all of
  ccl's data from the application data folder;
- run `ccl cloud logout --revoke` to revoke ccl's Google authorization and
  delete the local token;
- remove ccl's access at any time at
  <https://myaccount.google.com/permissions> (Google then also lets you
  delete the app's hidden data from Drive settings → Manage apps);
- delete the `~/.ccl/` directory to remove all local data.

### 4. iCloud Drive

On macOS, `ccl cloud login icloud` stores the same encrypted files in your
iCloud Drive instead. They are subject to your Apple account and Apple's
terms.

### 5. Children

ccl is a developer tool and is not directed to children under 13.

### 6. Changes and contact

Changes to this policy are published on this page with a new effective date,
and the full history is public in
<https://github.com/claude-code-launch/claude-code-launch.github.io>.
Questions or requests: <https://github.com/claude-code-launch/ccl/issues>.

## 中文

本政策说明 **ccl**（<https://claude-code-launch.github.io>，在 Google 登录页面上显示为“ccl”的应用）如何访问、使用、存储和共享数据，包括来自你 Google 账号的数据。

### 1. 关于 ccl

ccl 是一款免费、开源的开发者命令行工具，用来启动 Anthropic 的编程助手 Claude Code，并让它连接你选择的模型 provider。源代码：<https://github.com/claude-code-launch/ccl>。

ccl 完全在你自己的电脑上运行。其开发者**没有服务器、没有账号体系、没有分析或遥测**，不会接收、收集或存储你的任何数据。

### 2. ccl 在你电脑上处理的数据

| 数据 | 存放位置 | 用途 |
| --- | --- | --- |
| 模型 provider 设置、API Key、你所连接服务的 OAuth 令牌 | 你电脑的 `~/.ccl/`，仅所有者可读写（0600） | 以你选择的 provider 启动 Claude Code |
| Google OAuth 令牌（仅在开启 Google Drive 同步时） | 你电脑的 `~/.ccl/cloud/`，仅所有者可读写 | 读写 ccl 在你 Google Drive 中的同步文件 |

ccl 只向你配置的服务（你添加的模型 provider、用于同步的 Google Drive 或 iCloud Drive）发送请求；运行 `ccl update` 时访问 GitHub。ccl 不包含任何分析、遥测或广告。

### 3. Google 用户数据

Google Drive 同步是可选功能，只有在你运行 `ccl cloud login` 并在浏览器中同意授权后才会开启。

**申请的权限。** ccl 只申请一个 Google OAuth 权限：

- `https://www.googleapis.com/auth/drive.appdata`：你 Google Drive 中属于 ccl 的隐藏应用数据文件夹。

ccl 不申请你的姓名、邮箱、个人资料、联系人或其他任何 Google 权限；凭此权限也无法查看、打开或修改你 Drive 中的其他文件。

**访问的数据。** ccl 只在该文件夹中创建、读取、更新和删除一个文件 `ccl-sync.bundle`。其中是你 ccl 配置（`config.yaml` 与 `auth/*.json`，即 provider 设置和令牌）的加密快照，以及用于解密的 profile 密钥。ccl 不读取或收集其他任何 Google 用户数据。

**用途。** 该文件只有一个用途：在你自己的多台电脑之间复制 ccl 配置（`ccl cloud push`、`ccl cloud pull`），不作他用。

**存储与安全。** 配置在你电脑上压缩并用 AES-256-GCM 加密后才上传，只通过 HTTPS 发送给 Google；加密文件保存在你自己的 Google Drive 中。profile 密钥保存在同一个应用数据文件夹里，因此新电脑只需登录同一个 Google 账号即可。这也意味着：能访问你 Google 账号的人，或持有你某台电脑上 ccl 授权令牌的人，可以解密同步的数据。登录在你自己的浏览器中通过带 PKCE 的 Google OAuth 2.0 完成，ccl 不会接触你的 Google 密码。

**共享与披露。** ccl 不会向 ccl 开发者或任何第三方共享、出售、转让或披露 Google 用户数据。Google 用户数据不会发送给任何 AI / 机器学习模型，不用于训练或改进这类模型，也不用于广告、信用评估或上述同步功能之外的任何目的，不会有人工查看。

**不生成图片，不涉及 NCII。** ccl 只处理文本和源代码，不会使用 Google API 或 Google 用户数据生成、编辑或处理图片、视频、音频，绝不用于 AI 生成的非自愿亲密影像（NCII）或任何色情内容。

**有限使用。** ccl 对从 Google API 获得的信息的使用及向其他应用的传输，将遵守
[Google API 服务用户数据政策](https://developers.google.com/terms/api-services-user-data-policy)，包括其中的“有限使用”（Limited Use）要求。

**保留与删除。** 同步文件会保留在你的 Google Drive 中，直到你删除；本地 Google 令牌会保留到你退出登录。你可以：

- 运行 `ccl cloud logout --delete-remote --yes`，永久删除 ccl 在应用数据文件夹中的全部数据；
- 运行 `ccl cloud logout --revoke`，撤销 ccl 的 Google 授权并删除本地令牌；
- 随时在 <https://myaccount.google.com/permissions> 移除 ccl 的访问权限（之后也可以在 Drive 设置 → 管理应用 中删除该应用的隐藏数据）；
- 删除 `~/.ccl/` 目录，清除全部本地数据。

### 4. iCloud Drive

在 macOS 上，`ccl cloud login icloud` 会把同样的加密文件存放在你的 iCloud Drive 中，受你的 Apple 账号及 Apple 条款约束。

### 5. 儿童

ccl 是开发者工具，不面向 13 岁以下儿童。

### 6. 变更与联系

本政策的变更会发布在本页面并更新生效日期，完整修改历史公开于
<https://github.com/claude-code-launch/claude-code-launch.github.io>。
如有问题或请求，请联系：<https://github.com/claude-code-launch/ccl/issues>。
