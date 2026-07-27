# CCL Cloud Sync v2 设计

状态：Phase 1–4 已实现，Phase 5 密钥轮换待实现  
范围：多网盘连接、远端别名、同步生命周期、设备配对  
兼容目标：现有单 Google Drive / iCloud 配置必须无损迁移

## 1. 设计目标

Cloud Sync v2 将三个此前耦合的概念拆开：

- **Profile**：一套被同步的 CCL 配置及其主密钥。
- **Remote**：一个具体的网盘账号或同步目录，例如个人 Google Drive。
- **Device**：持有 Profile 主密钥、能够加解密配置的本地设备。

第一阶段只支持一个活动 Profile，但允许它同时镜像到多个 Remote。这样既满足多个网盘备份，也避免同一台设备同时切换多套配置带来的状态复杂度。数据模型保留 `profile_id`，以后可以增加多 Profile，而无需再次改 Remote 格式。

核心约束：

1. 同一 Profile 的所有 Remote 使用同一把 256-bit 主密钥。
2. OAuth token、远端缓存和同步游标必须按 Remote 隔离。
3. 默认操作只作用于 primary Remote；跨网盘操作必须显式使用 `--all` 或 `--to/--from`。
4. `logout` 默认只移除本地连接，不删除远端密文，也不删除主密钥。
5. 云端永远不保存明文主密钥、恢复密钥或 CCL 配置。
6. 设备配对只是安全传递现有主密钥；设备全部丢失时，离线恢复密钥仍是最后恢复方式。

非目标：

- 不在 v2 第一阶段支持同时活动的多个独立 Profile。
- 不合并两个 Remote 上分叉的配置；冲突必须由用户选择来源。
- 不承诺跨云事务。多网盘推送只能做到预检后逐个提交，并明确报告部分成功。
- 不把 Google Authenticator、系统账号密码或 OAuth token 当作 CCL 加密密钥。

## 2. 用户命令

### 2.1 登录与远端管理

```text
ccl login google-drive personal
ccl login google-drive work
ccl login icloud icloud-main

ccl cloud ls
ccl cloud use personal
ccl cloud rename personal home
ccl cloud set work --mirror
ccl cloud set work --no-mirror
```

兼容现有命令：

```text
ccl login google-drive
```

没有提供别名时：

- 当前没有同类型 Remote：使用 `google-drive` 或 `icloud` 作为默认别名。
- 已有同类型 Remote：交互终端中要求选择现有 Remote 或输入新别名。
- 非交互环境中若存在歧义则报错，要求显式提供别名。

别名规则：

- 长度 1–48。
- 允许字母、数字、`.`、`_`、`-`。
- 大小写不敏感且必须唯一。
- `all`、`primary`、`latest` 为保留字。
- 别名只用于展示和选择；内部引用使用随机 `remote_id`，因此改名不会移动 token 或状态。

`ccl cloud ls` 示例：

```text
Cloud remotes
  NAME       PROVIDER       ROLE      MIRROR  STATE
* personal   google-drive   primary   yes     up to date
  work       google-drive   mirror    yes     behind
  icloud     icloud         mirror    no      signed out
```

### 2.2 Push、Pull 与状态

```text
ccl push                    # 推送到 primary
ccl push --to work          # 推送到指定 Remote
ccl push --all              # 推送到所有启用 mirror 的 Remote
ccl push --all --best-effort

ccl pull                    # 从 primary 拉取
ccl pull --from work
ccl pull --from work --tag release-2026-07

ccl status                  # primary 的状态
ccl status work
ccl status --all
```

约束：

- `--to` 与 `--all` 互斥。
- `pull` 不提供隐式的“从所有网盘自动选最新”。不同 Remote 可能发生分叉，默认自动选择会掩盖回滚或覆盖风险。
- `push --all` 先检查全部目标。任何目标冲突时默认不开始写入。
- `--best-effort` 会跳过冲突或不可用的 Remote，继续处理其他目标。
- 即使全部预检成功，网络故障仍可能导致部分提交。CLI 必须逐 Remote 报告结果，且保留可重试的操作日志。

### 2.3 Logout

```text
ccl logout personal
ccl logout personal --revoke
ccl logout personal --delete-remote
ccl logout --all
```

默认 `logout <alias>`：

- 删除该 Remote 的本地 OAuth token、缓存和游标。
- 从本地注册表移除该 Remote。
- 不删除远端加密数据。
- 不删除 Profile 主密钥。
- 不影响其他 Remote。

如果被移除的是 primary：

- 有其他已启用 Remote 时，按注册顺序选择下一个并明确提示。
- 没有其他 Remote 时，primary 留空；本地主密钥仍保留。

`--revoke`：

- 在删除本地 token 前，尽力调用提供商的 token revoke API。
- revoke 失败时默认停止，除非同时指定 `--force-local`。
- Google 可能把同一 OAuth client 与同一账号的其他授权一并影响，因此执行前必须提示。
- 不删除远端密文。

`--delete-remote`：

- 永久删除该 Remote 中属于 CCL 的 bundle 与配对对象。
- 必须提供精确别名；不允许与 `--all` 一起使用。
- 交互终端要求确认；自动化必须同时提供 `--yes`。
- 先完成远端删除，再移除本地 token。

主密钥销毁不放进 `logout`，使用单独的高风险命令：

```text
ccl key purge
```

仅在没有已连接 Remote 时允许执行，并在删除前验证用户已经导出恢复密钥或显式使用 `--force`。

## 3. 本地数据模型

### 3.1 文件布局

```text
~/.ccl/
└── cloud/
    ├── registry.json
    ├── profiles/
    │   └── <profile-id>/
    │       ├── key
    │       └── state.json
    ├── remotes/
    │   └── <remote-id>/
    │       ├── remote.json
    │       ├── state.json
    │       ├── auth.json
    │       └── cache/
    ├── operations/
    │   └── <operation-id>/
    │       ├── operation.json
    │       └── snapshot.ccl
    └── pairing/
        └── <request-id>.json
```

权限要求：

- `~/.ccl/cloud` 及所有子目录：`0700`。
- 所有配置、token、密钥、状态和临时密文：`0600`。
- 安全相关读写拒绝符号链接。
- 注册表、状态和操作日志必须使用同目录临时文件 + `fsync` + 原子 rename。
- JSON 中保存相对路径，不接受 `..` 或越过 `~/.ccl/cloud` 的路径。

### 3.2 注册表

`registry.json`：

```json
{
  "version": 2,
  "active_profile_id": "8f9d...",
  "primary_remote_id": "44d3...",
  "device": {
    "id": "3dbca...",
    "name": "Yalla MacBook"
  },
  "aliases": {
    "personal": "44d3...",
    "work": "f28a..."
  },
  "remote_order": ["44d3...", "f28a..."]
}
```

注册表只存索引，不存 token 或密钥。`remote_order` 用于稳定展示，以及 primary 被移除后的确定性选择。

### 3.3 Profile

`profiles/<profile-id>/state.json`：

```json
{
  "version": 2,
  "profile_id": "8f9d...",
  "key_mode": "local",
  "last_local_hash": "b0d4...",
  "pending_tag": "",
  "pending_hash": "",
  "explicit_tag": false,
  "last_operation": "push",
  "last_sync_at": "2026-07-26T12:00:00Z"
}
```

`key` 保存当前 Profile 的 32-byte 主密钥。macOS 默认使用本地 `cloud.key` 语义，即依赖文件权限保护，不依赖 Apple Team entitlement。Linux/Windows 可以继续使用 passphrase 派生模式；当前实现为便于非交互同步，会把已经派生的 32-byte key 以 `0600` 保存。

`key_mode` 记录主密钥最初的取得方式，不改变远端密文格式：

- `local`
- `passphrase`
- `recovery`
- 兼容迁移用的 `keychain`

### 3.4 Remote

`remotes/<remote-id>/remote.json`：

```json
{
  "version": 2,
  "id": "44d3...",
  "alias": "personal",
  "provider": "google-drive",
  "profile_id": "8f9d...",
  "enabled": true,
  "mirror": true,
  "account_id": "provider-stable-id",
  "account_hint": "ha***@gmail.com",
  "provider_config": {}
}
```

说明：

- `account_id` 使用提供商的稳定账号标识；不使用 access token，也不依赖可能变化的邮箱。
- `account_hint` 只用于展示，可为空。
- Google Drive 的 `auth.json` 独立保存 refresh token。
- iCloud 没有 OAuth token，`provider_config` 保存同步目录标识。
- 每个 Remote 有独立 cache，两个 Google 账号绝不能共用一个 `ccl-sync.bundle` 缓存路径。

`remotes/<remote-id>/state.json`：

```json
{
  "version": 2,
  "last_seen_remote_id": "ec15...",
  "last_pushed_snapshot_id": "ec15...",
  "last_pulled_snapshot_id": "ec15...",
  "last_remote_hash": "b0d4...",
  "last_operation": "push",
  "last_sync_at": "2026-07-26T12:00:00Z",
  "last_error": ""
}
```

单远端实现中的 `LastRemoteID` 不能放在 Profile state 中。每个网盘可能暂时停留在不同 snapshot，必须独立追踪。

## 4. 远端抽象

保留当前“核心同步逻辑操作本地目录/缓存”的优点，但把下载与提交从 `Manager` 中拆出：

```go
type RemoteSession interface {
    ID() string
    Alias() string
    Provider() string
    CacheDir() string

    Refresh(context.Context) (RemoteRevision, error)
    Commit(context.Context, ExpectedRevision) (RemoteRevision, error)
    Revoke(context.Context) error
    DeleteRemoteData(context.Context) error

    PairingStore() PairingStore
}
```

`RemoteRevision` 至少包含：

- 提供商可用的 ETag、version 或 generation。
- bundle 大小和修改时间。
- 若提供商不支持条件更新，则明确标记为 best-effort。

Google Drive 当前实现会在下载时记录单调递增的文件 `version`，上传前再次读取并比较。Drive v3 的公开上传接口没有稳定承诺可供这里使用的原子 `If-Match` 条件提交，因此仍存在很小的检查—写入竞争窗口，必须标记为 best-effort。iCloud Drive 文件系统同样不提供可靠的跨设备事务，实现会在提交前再次读取远端 index，并保留现有冲突检测。

`Manager` 改成 Profile 级服务：

```go
type Service struct {
    Registry Registry
    Profile  Profile
    Remotes  map[RemoteID]RemoteSession
}
```

一次命令显式选择一个或多个 Remote，而不是让一个 `Manager` 永久绑定唯一 Remote。

## 5. 登录流程

### 5.1 第一个 Remote

1. 创建 Remote 临时目录，完成 OAuth 或找到 iCloud 目录。
2. 下载远端 bundle。
3. 远端为空：
   - 本地已有活动 Profile：把它连接为新镜像，但不隐式覆盖非空远端。
   - 本地没有 Profile：生成主密钥、`profile_id` 和 verifier。
4. 远端已有 Profile：
   - 本地没有 Profile：要求通过 passphrase、`ccl key import` 或设备配对取得密钥。
   - 本地 Profile ID 相同：验证 verifier 后连接。
   - 本地 Profile ID 不同：默认拒绝。
5. token、Remote config 和 registry 最后原子落盘。中途失败不得破坏现有连接。

### 5.2 增加另一个 Remote

远端为空时，连接到当前活动 Profile；首次上传仍由用户运行 `ccl push --to <alias>` 或确认登录后的上传提示。

远端存在数据时：

- Profile ID 相同：验证 key 后连接。
- Profile ID 不同：默认拒绝并显示两个 Profile ID 的短标识。
- 只有显式 `--replace-remote` 才允许用当前 Profile 覆盖远端；该选项属于破坏性操作，需确认。

登录 Google Drive 时应强制展示账号选择器。每个别名保存自己的 refresh token 和 cache。可以通过 Drive `about` 的稳定用户标识检测同一 Google 账号被重复添加，并提示用户，而不是静默覆盖旧 Remote。

## 6. Push 算法

### 6.1 单 Remote

1. 收集允许同步的本地文件并计算 canonical hash。
2. 解析 tag；未显式 tag 时使用 `latest`。
3. 刷新目标 Remote 缓存。
4. 解密并验证远端 profile、verifier 和 index。
5. 根据该 Remote 的 state 检查冲突。
6. 只生成一次 snapshot ID 和加密 snapshot。
7. 更新缓存中的 index。
8. 使用条件更新提交 bundle。
9. 成功后更新 Remote state 和 Profile state。

### 6.2 多 Remote

`push --all`：

1. 生成同一份 canonical payload。
2. 生成一次 snapshot ID 和密文；所有目标复用它。
3. 并行刷新目标，但串行输出稳定的结果顺序。
4. 对全部目标完成 Profile 验证和冲突预检。
5. 默认只有全部预检通过才进入提交阶段。
6. 为本次推送创建 operation journal。
7. 逐个或有限并发提交，并在每个成功后原子更新 journal。
8. 全部完成后删除 journal 与临时 snapshot。

Operation journal：

```json
{
  "version": 1,
  "id": "op-...",
  "profile_id": "8f9d...",
  "snapshot_id": "ec15...",
  "hash": "b0d4...",
  "tag": "latest",
  "targets": ["44d3...", "f28a..."],
  "completed": ["44d3..."],
  "created_at": "2026-07-26T12:00:00Z"
}
```

临时文件只保存已经加密的 snapshot，不保存明文配置。重试时先刷新未完成目标，若没有新冲突则复用相同 snapshot；已经成功的 Remote 不重复提交。

跨云无法原子提交，因此退出码约定：

- `0`：所有请求目标成功或本来就是相同内容。
- `1`：没有目标成功。
- `2`：部分成功，可重试。

## 7. Pull 与冲突

`pull` 一次只允许一个来源：

1. 解析 `--from`；未指定时使用 primary。
2. 刷新并验证远端。
3. 解析 tag，验证 snapshot 与 hash。
4. 检查本地未同步修改。
5. 有冲突时默认停止；`--force` 先创建本地加密备份，再覆盖。
6. 原子应用文件并更新来源 Remote 的 state。

不自动合并两个 Remote。若 `personal` 与 `work` 的 `latest` 不同：

```text
ccl status --all
ccl pull --from work
ccl push --to personal
```

由用户明确选择权威来源，再把结果镜像到其他 Remote。

## 8. 设备配对

### 8.1 命令

新设备：

```text
ccl login google-drive personal
ccl device request --via personal --name "New MacBook"

Waiting for approval: 482-731
```

已有设备：

```text
ccl device ls
ccl device ls --all
ccl device approve 482-731
ccl device deny 482-731
```

新设备轮询到响应后取得主密钥、验证远端 verifier，并完成登录。

### 8.2 密钥协议

每个 Profile 有一个专用于配对的静态 X25519 密钥对：

- 私钥由 Profile 主密钥使用 HKDF-SHA256 派生，不单独同步。
- 公钥写入远端 `profile.json`。
- 配对用途与 snapshot 加密用途使用不同 HKDF `info`，保证密钥隔离。

新设备：

1. 生成一次性 X25519 key pair。
2. 从远端读取 Profile pairing public key。
3. 计算 shared secret。
4. 使用 HKDF-SHA256 派生 request key 与 response key。
5. 用 request key 加密设备名称、nonce、request ID 和过期时间。
6. 上传不含秘密的 request envelope。
7. 只在本地保存一次性 private key，权限 `0600`。

已有设备：

1. 用主密钥派生 Profile pairing private key。
2. 与新设备 public key 计算相同 shared secret。
3. 解密 request 并向用户显示设备名称、来源 Remote、创建时间和短验证码。
4. 用户批准后，用 response key 加密：
   - Profile 主密钥
   - Profile ID
   - verifier 摘要
   - 批准设备 ID
   - request ID 与过期时间
5. 上传一次性 response envelope。

新设备解密响应并验证 Profile ID 与 verifier。成功后立即删除本地一次性 private key，并尽力删除远端 request/response。

算法：

- Key agreement：X25519。
- KDF：HKDF-SHA256。
- AEAD：XChaCha20-Poly1305。
- request ID：128-bit CSPRNG。
- 有效期：10 分钟。
- 每个 request 只能成功消费一次。
- request ID、双方 public key、Profile ID、版本和过期时间全部放入 AEAD AAD。

`482-731` 是方便人在两台设备间确认的短码，不作为加密密钥。它由 request ID、Profile ID 和新设备 public key 的哈希截取生成。批准操作本身才是授权边界。

### 8.3 远端对象

配对对象不能塞进会整体覆盖的 `ccl-sync.bundle`，否则设备轮询和普通 push 会互相覆盖。

远端需要独立对象：

```text
pairing/requests/<request-id>.ccl
pairing/responses/<request-id>.ccl
```

Google Drive 在 `appDataFolder` 中分别创建、查询和删除这些对象。iCloud 使用同步目录下对应子目录。对象名称中的 request ID 必须严格验证为固定长度十六进制，禁止任意路径。

### 8.4 威胁边界

云提供商能看到：

- 存在一个配对请求。
- request ID、协议版本、时间、密文大小。
- 一次性 public key。

云提供商不能得到：

- Profile 主密钥。
- CCL 配置明文。
- 恢复密钥。
- 加密后的设备名称。

只取得 Google/Microsoft/iCloud 账号的攻击者可以删除或伪造请求造成拒绝服务，但没有已授权设备上的主密钥，无法生成可被新设备验证的响应。

“移除设备”不能让已经取得主密钥的设备失去解密未来数据的能力。真正撤销必须：

1. 生成新主密钥。
2. 重加密当前 snapshot、index 和 verifier。
3. 原子更新所有 Remote。
4. 只把新 key 分发给保留设备。

因此第一阶段只提供 `ccl device forget`（删除可信设备元数据），不把它称为 revoke。真正的 `ccl device revoke` 必须与 `ccl key rotate` 一起实现。

离线恢复密钥仍必须保留。若所有已授权设备丢失且没有恢复密钥，零知识设计下无法恢复数据。

## 9. v1 无损迁移

触发条件：

- 不存在 `~/.ccl/cloud/registry.json`。
- 存在现有 `~/.ccl/cloud.json`。

迁移步骤：

1. 以只读方式解析并验证 v1 `cloud.json`、`cloud-state.json`、`cloud.key`。
2. 根据 provider 生成默认别名。
3. 生成 `remote_id`，建立 v2 临时目录。
4. 复制 key、OAuth auth 和 cache；迁移阶段不上传、不拉取、不 revoke。
5. 把 v1 `LastRemoteID` 放入新 primary Remote state。
6. 用现有 key 验证本地 cache 或远端 verifier。
7. 原子写入 v2 registry。
8. 把 v1 文件保留为 `*.v1.bak`，权限 `0600`。

迁移必须：

- 幂等：中断后再次运行不会创建重复 Remote。
- 可回滚：registry 写入前的错误不改变 v1。
- 无网络副作用：迁移本身不写远端。
- 对当前已工作的 Google Drive 配置提供真实格式 fixture 测试。

迁移完成后的旧文件至少保留一个发布周期。确认 v2 稳定后再提供显式清理命令，而不是自动删除。

## 10. 状态与 Doctor

`ccl status --all` 对每个 Remote 独立计算：

- `up to date`
- `local ahead`
- `remote ahead`
- `diverged`
- `empty`
- `signed out`
- `unreachable`
- `profile mismatch`
- `partial push pending`

一个 Remote 不可达不能阻止显示其他 Remote。

`ccl doctor` 增加：

- registry schema 与别名唯一性。
- primary 是否存在并属于活动 Profile。
- key 长度、权限和 Profile ID。
- Remote config、token、cache、state 是否互相隔离。
- Remote 指向的 Profile 是否一致。
- 未完成 operation journal 是否可安全重试。
- 过期 pairing request 是否可清理。
- v1/v2 同时存在时是否已经完成迁移。
- symlink、目录穿越和过宽权限检查。

Doctor 默认只诊断。修复必须使用现有的确认机制，并且绝不自动删除远端数据或主密钥。

## 11. 实施阶段

### Phase 1：注册表与迁移（已实现）

- 新增 v2 types、secure path helpers 与 repository 层。
- 实现 v1 只读迁移。
- 将现有单 Remote Google/iCloud 装入 `RemoteSession`。
- 保持原有 `login/push/pull/status/key` 行为不变。

### Phase 2：多 Remote CLI（已实现）

- 实现带别名的 login。
- 实现 `ccl cloud ls/use/rename/set`。
- 实现 `logout`。
- 实现 `push --to/--all`、`pull --from`、`status --all`。
- 加入 operation journal 与部分成功退出码。

### Phase 3：远端并发安全（已实现 best-effort）

- Google Drive 文件 version 乐观冲突检查。
- iCloud 二次检查与 best-effort 标识。
- 故障注入测试：超时、token 失效、磁盘满、提交中断。

### Phase 4：设备配对（已实现）

- 扩展 Remote provider 的 PairingStore。
- 实现 X25519/HKDF/XChaCha20-Poly1305 envelope。
- 实现 request/list/approve/deny/poll/cleanup。
- 增加过期、重放、伪造、错误 Profile 和并发请求测试。

### Phase 5：密钥轮换

- 实现 `ccl key rotate`。
- 在轮换完成前不提供具有误导性的 `device revoke`。

## 12. 验收标准

至少覆盖：

1. v1 Google Drive 登录状态无网络写入迁移到 v2。
2. 两个 Google 账号拥有独立 token、cache 和 state。
3. Google + iCloud 同时镜像同一个 snapshot ID 和密文。
4. 一个 Remote 冲突时，默认 `push --all` 不写任何目标。
5. 提交中途失败后能够只重试未完成目标。
6. `pull --from` 不修改其他 Remote 的游标。
7. 默认 logout 不删除远端密文和 Profile key。
8. `--delete-remote` 未确认时不执行。
9. 配对响应只能由持有主密钥的设备生成。
10. request 过期、重复消费或 public key 被替换时失败。
11. 所有敏感文件保持 `0600`，目录保持 `0700`，符号链接被拒绝。
12. `go test ./...`、`go vet ./...`、race tests 与 darwin/linux/windows 交叉编译通过。

## 13. 已确定的产品决策

- 一个活动 Profile 可以连接多个 Remote。
- 第一个 Remote 自动成为 primary。
- `push` 和 `pull` 默认只操作 primary。
- `push --all` 只操作已启用 mirror 的 Remote。
- `pull` 不自动跨网盘选择“最新”。
- logout 默认只清理本地连接。
- Profile key 的删除与 logout 分离。
- 设备配对通过任意已连接 Remote 传递端到端加密 envelope。
- 所有旧设备丢失时，恢复密钥是唯一保证可恢复的机制。
- 设备真正撤销依赖主密钥轮换，不能用删除设备记录来假装完成。
