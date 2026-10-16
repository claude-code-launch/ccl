# ccl Privacy Policy / 隐私政策

Effective date / 生效日期: 2026-10-16

[English](#english) · [中文](#中文)

## English

ccl is an open-source command-line tool that runs on your own computer
(<https://github.com/claude-code-launch/ccl>). It has no servers and no
accounts of its own. The developers of ccl do not receive, collect or store
any of your data.

### Data ccl handles on your computer

- ccl keeps its configuration in `~/.ccl/` on your computer: model provider
  settings, API keys, and OAuth tokens for the services you connect. These
  files are written with owner-only permissions (0600).
- ccl sends requests only to services you configure (for example, model
  providers you add, Google Drive or iCloud Drive for sync) and to GitHub when
  you run `ccl update`.
- ccl contains no analytics, telemetry or advertising.

### Google user data (cloud sync with Google Drive)

When you run `ccl cloud login` and approve access, ccl requests one Google
OAuth scope:

- `https://www.googleapis.com/auth/drive.appdata` — the app's own hidden
  application data folder in your Google Drive.

ccl does not request your name, email address, profile or any other Google
scope, and it cannot see or change other files in your Drive.

**What is accessed and why.** ccl reads and writes a single file,
`ccl-sync.bundle`, in that application data folder. It holds your ccl
configuration (`config.yaml` and `auth/*.json`), compressed and encrypted with
AES-256-GCM, so you can sync it between your own computers.

**Encryption key.** The encryption key is stored in the same cloud profile, so
a new computer only needs to sign in to the same Google account. This means
anyone who can access that Google account, or who holds the ccl authorization
token from one of your computers, can decrypt the synced data.

**Storage.** The encrypted file stays in your Google Drive. The Google OAuth
token is stored only on your computer under `~/.ccl/cloud/` with owner-only
permissions. Nothing is sent to the ccl developers or any other third party.

**Sharing.** ccl does not share, sell or transfer Google user data to anyone,
and does not use it for advertising or to train AI or machine-learning models.

ccl's use and transfer of information received from Google APIs adheres to the
[Google API Services User Data Policy](https://developers.google.com/terms/api-services-user-data-policy),
including the Limited Use requirements.

**Revoking access and deleting data.**

- `ccl cloud logout --delete-remote --yes` permanently deletes ccl's data from
  the application data folder.
- `ccl cloud logout --revoke` revokes ccl's Google authorization and removes
  the local token.
- You can also remove ccl's access at
  <https://myaccount.google.com/permissions>. Deleting the `~/.ccl/` directory
  removes all local data.

### iCloud Drive

On macOS, `ccl cloud login icloud` stores the same encrypted files in your
iCloud Drive. They are subject to your Apple account and Apple's terms.

### Changes and contact

Changes to this policy are published in this file, and its history is
visible in the repository. Questions:
<https://github.com/claude-code-launch/ccl/issues>.

## 中文

ccl 是一个在你自己电脑上运行的开源命令行工具
（<https://github.com/claude-code-launch/ccl>）。它没有服务器，也没有自己的账号体系。ccl 的开发者不会接收、收集或存储你的任何数据。

### ccl 在你电脑上处理的数据

- ccl 的配置保存在你电脑的 `~/.ccl/` 目录中，包括模型 provider 设置、API Key，以及你所连接服务的 OAuth 令牌。这些文件只有所有者可读写（0600）。
- ccl 只向你配置的服务（例如你添加的模型 provider、用于同步的 Google Drive 或 iCloud Drive）发送请求；运行 `ccl update` 时会访问 GitHub。
- ccl 不包含任何分析、遥测或广告。

### Google 用户数据（Google Drive 云同步）

运行 `ccl cloud login` 并同意授权时，ccl 只申请一个 Google OAuth 权限：

- `https://www.googleapis.com/auth/drive.appdata`：你的 Google Drive 中属于本应用的隐藏应用数据文件夹。

ccl 不申请你的姓名、邮箱、个人资料或其他任何 Google 权限，也无法查看或修改你 Drive 中的其他文件。

**访问什么、为什么访问。** ccl 只在该应用数据文件夹中读写一个文件 `ccl-sync.bundle`。其中是你的 ccl 配置（`config.yaml` 与 `auth/*.json`），经压缩并用 AES-256-GCM 加密，用于在你自己的多台电脑之间同步。

**加密密钥。** 加密密钥保存在同一个云端 profile 中，因此新电脑只需登录同一个 Google 账号即可。这也意味着：能访问该 Google 账号的人，或持有你某台电脑上 ccl 授权令牌的人，可以解密同步的数据。

**存储。** 加密文件保存在你自己的 Google Drive 中。Google OAuth 令牌只保存在你电脑的 `~/.ccl/cloud/` 下，仅所有者可读写。任何数据都不会发送给 ccl 开发者或其他第三方。

**共享。** ccl 不会向任何人共享、出售或转让 Google 用户数据，也不会将其用于广告或训练 AI / 机器学习模型。

ccl 对从 Google API 获得的信息的使用和传输，遵守
[Google API 服务用户数据政策](https://developers.google.com/terms/api-services-user-data-policy)，包括其中的“有限使用”（Limited Use）要求。

**撤销授权与删除数据。**

- `ccl cloud logout --delete-remote --yes` 会永久删除 ccl 在应用数据文件夹中的数据。
- `ccl cloud logout --revoke` 会撤销 ccl 的 Google 授权并删除本地令牌。
- 你也可以在 <https://myaccount.google.com/permissions> 移除 ccl 的访问权限。删除 `~/.ccl/` 目录即可清除全部本地数据。

### iCloud Drive

在 macOS 上，`ccl cloud login icloud` 会把同样的加密文件存放在你的 iCloud Drive 中，受你的 Apple 账号及 Apple 条款约束。

### 变更与联系

本政策的变更会发布在此文件中，修改历史可在仓库中查看。如有问题，请联系：
<https://github.com/claude-code-launch/ccl/issues>。
