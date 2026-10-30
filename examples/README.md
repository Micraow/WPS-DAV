# wpsdav-sync 参考脚本

[`wpsdav-sync.py`](wpsdav-sync.py) 是一个建立在 rclone 之上的“非对称同步”辅助脚本，
适合手动、偶尔把本地目录同步到 WPS 云盘。它不是 wpsdav 服务的一部分，只作为参考实现。

## 它解决的问题

普通的 `rclone sync 本地 远端` 是单向镜像：远端有、本地没有的文件会被**删除**。
`rclone bisync` 是双向的，但删除也会双向传播。这个脚本按下面的规则工作：

| 情况 | 行为 |
|---|---|
| 本地新增 / 修改 | 上传到云端 |
| 本地删除 | 删除云端对应文件 |
| 云端新增 / 修改 | 下载到本地 |
| 云端删除 | 本地保留，不自动重新上传 |
| 两边都修改，大小相同 | 视为已一致 |
| 两边都修改，大小不同 | 保留本地并上传 |
| 本地删除，但云端已修改 | 下载云端版本，避免丢数据 |
| 云端删除，但本地已修改 | 保留本地，记录冲突，不自动重传 |

因此它更像“本地是权威删除源，云端是权威新增源”的折中方案，而不是严格的双向镜像。

## 依赖

- Python 3.8+
- [rclone](https://rclone.org/)（需要支持 `--files-from-raw`、`--no-check-dest`、
  `--no-traverse`、`--use-json-log`）
- `git`（仅 `--gitignore` 需要）
- 一个可用的 rclone 远端。以 wpsdav 为例：

  ```bash
  rclone config create wps webdav \
    url=http://127.0.0.1:8705/dav \
    vendor=other \
    user=wpsdav \
    pass=$(rclone obscure '你的密码')
  ```

## 用法

先试运行，只读、不写状态、不创建远端目录：

```bash
python3 examples/wpsdav-sync.py \
  --local ~/Documents/MyDocs \
  --remote wps:backup/MyDocs \
  --exclude cache \
  --exclude private/secret.txt \
  --dry-run
```

确认计划后去掉 `--dry-run`：

```bash
python3 examples/wpsdav-sync.py \
  --local ~/Documents/MyDocs \
  --remote wps:backup/MyDocs \
  --exclude cache \
  --exclude private/secret.txt
```

也可以安装到 `PATH`：

```bash
install -m755 examples/wpsdav-sync.py ~/.local/bin/wpsdav-sync
wpsdav-sync --remote wps:backup/MyDocs --dry-run
```

`--local` 默认是当前目录，所以可以直接在每个要同步的目录里运行，并分别指定云端目标：

```bash
cd ~/src/project-a && wpsdav-sync --remote wps:code/project-a --gitignore
cd ~/src/project-b && wpsdav-sync --remote wps:code/project-b --gitignore
```

不同的 `(本地目录, 云端目录)` 组合使用各自的状态文件和锁，可以同时运行。
同一个组合不能并发运行两个实例。

常用参数：

| 参数 | 说明 |
|---|---|
| `--local` | 本地目录，默认当前目录 |
| `--remote` | 云端目标目录（rclone 远端路径），必填；例如 `wps:code/project-a` |
| `--exclude PATH` | 不同步的相对路径前缀，可重复。例如 `cache` 会排除根目录下的整个 `cache/`；`private/secret.txt` 只排除这个文件 |
| `--exclude-dir NAME` | 不同步的目录名，任意层级匹配，可重复。例如 `--exclude-dir node_modules --exclude-dir build` |
| `--gitignore` | 跳过 Git 忽略的文件和目录；支持根目录不是仓库、以及子目录里的嵌套仓库 |
| `--transfers N` | 并发传输数，默认 8；被 WPS 限流时降到 4 |
| `--checkers N` | 并发检查/列目录数，默认 48；云端目录多时越大越快，过高可能被限流 |
| `--listing-cache SECONDS` | 复用最近 SECONDS 秒内的云端列表快照（0=关闭）。缓存期间的新变化要等下次运行才会发现 |
| `--refresh-listing` | 忽略列表缓存，强制重新读取云端列表 |
| `--buffer-size SIZE` | 每个传输的内存缓冲，默认 32M |
| `--max-delete-pct N` | 单次最多删除云端文件的比例，默认 10% |
| `-v, --verbose` | 逐文件输出成功/失败详情；文件多时建议重定向到日志 |
| `--dry-run` | 只显示计划，不传输、不写状态 |
| `--force` | 跳过删除数量和空目录等安全阈值 |

## 固定排除项

除了 `--exclude` / `--exclude-dir` / `--gitignore` 指定的内容，脚本还会跳过：

- 所有点开头的文件和目录（WPS 拒绝点开头的名字，`.git`、`.vscode`、
  `.venv`、`.ipynb_checkpoints` 等也不会上传）；
- `~$*` 开头的 Office 临时文件；
- 符号链接（避免把同步根目录之外的内容重复上传，或产生循环）；
- 非普通文件（socket、FIFO、设备等）；
- 非 UTF-8 文件名，以及含控制字符的文件名（WebDAV 无法可靠表示）；
- 名字含 `\ : * ? " < > |` 的文件/目录（WPS 返回 `illegalFname`）；
- 任一路径段末尾带空格或点的文件/目录（WPS 会悄悄去掉末尾空格/点，导致路径错位）；
- 0 字节文件（当前 wpsdav 上传空文件时 WPS 返回 `411 MissingContentLength`）；
- 空目录（文件所在目录会自动创建，但纯空目录不会同步）。

含非法字符或末尾空格/点的文件不会被自动改名，脚本只跳过并在报告中列出。把名字改成
不含这些字符的形式后重跑即可同步。空文件本身没有内容，跳过不会丢失数据；如果确实需要
保留空文件这个“存在性”，需要先修 wpsdav 的空文件上传路径，或放一个占位文件。

被跳过的路径会写入每次运行的报告，方便排查。

`--gitignore` 按 Git 自己的判断取忽略列表，分三种情况：

- `--local` 在某个仓库内：用该仓库的规则，包括父目录的 `.gitignore`、
  `.git/info/exclude` 和全局 excludes；已跟踪的文件即使匹配 `.gitignore` 也不会被跳过。
- `--local` 不是仓库：用一个临时索引解析整棵树里的 `.gitignore`，所以即使没有 `.git`
  目录，子目录里的 `.gitignore` 也会生效。
- 子目录里是独立的嵌套仓库：递归进入各自的仓库读取规则，路径按相对 `--local` 归一化。

被 Git 忽略的文件即使以前上传过，也不会因为这次“看不见”而从云端删除。

`--exclude-dir` 只匹配目录名，不匹配文件名；如果只想排除根目录下的某个目录，
用 `--exclude path/to/dir` 更精确。

## 状态与缓存

脚本不会缓存文件内容，也不使用 `rclone mount` 或 VFS 缓存。它只维护一个很小的 JSON 状态文件：

```text
~/.local/state/wpsdav-sync/state-<hash>.json
```

其中记录路径、大小、本地 mtime，用于判断“本地改了什么、云端改了什么”。
每次运行的报告在：

```text
~/.local/state/wpsdav-sync/last-run-<hash>.log
```

删除状态文件会让下次运行变成“首次同步”：两边同时存在的文件如果大小相同会被视为一致，
大小不同则保留本地。一般不需要手动删除。

上传时 wpsdav 会临时把单个文件落盘计算哈希，下载时 rclone 默认写临时文件再改名；
这些是瞬时占用，不是持久缓存。

## 安全保护

- **同一个 (本地, 云端) 组合加锁**：该组合不能并发运行两个实例；不同组合可以同时运行。
- **本地扫描到 0 个文件**：如果历史状态显示以前有文件，脚本会中止，避免把云端全部当成
  “本地已删除”。确认本地确实清空后加 `--force`。
- **云端列表为空**：历史状态显示云端曾有文件时会中止，避免接口异常导致误判。
- **删除阈值**：单次计划删除云端超过 `max(100, 10%)` 个文件时中止，确认后加 `--force`。
- **传输失败不推进状态**：失败的文件会在下次运行重试；已成功的文件会增量写入状态。

## 性能

- `--no-traverse` + `--no-check-dest` + `--files-from-raw`：只直接访问本次变更的文件，
  不会为了几个改动重新遍历整个远端目录。
- 默认 8 个并发传输、32M 缓冲；WPS 对高频写入敏感，出现限流时降低 `--transfers`。
  wpsdav 会先按 `upload_retries` 自行重试；仍失败时返回真实的 4xx/5xx，rclone 也会重试。
- 云端列目录仍是主要开销；首次同步和每次增量都需要完整列一次云端。列目录失败会直接中止，
  不会把失败当成空目录。
- **列目录基准**（4,078 个目录 / 44,072 个文件 / 26.7GB）：`--checkers 16` 约 99 秒，
  `--checkers 48` 约 49 秒；继续提高到 64 没有明显收益。WPS 的目录接口每页固定 200 条，
  也没有递归列举接口，所以每个目录至少一次请求，冷启动耗时的下限由目录数量决定。
- **文件夹 mtime 不能用来判断变化**：实测新增、覆盖、删除子文件后，文件夹的
  `getlastmodified` 始终等于创建时间；依赖它会漏同步。文件自身的 mtime 在上传时会更新，
  但仍需先列目录才能看到，无法减少请求数。
- **重复运行**：加 `--listing-cache 300` 可复用 5 分钟内的云端快照，命中时列表阶段
  从几十秒降到几十毫秒；`--refresh-listing` 强制刷新。

## 已知限制

- **无法识别云端同尺寸修改**：WPS 不提供可靠的 checksum，云端 mtime 又是上传时间，
  所以只能按大小和本地 mtime 判断。云端把文件改成相同大小的另一个内容时，脚本看不出来。
- **云端重命名/移动会本地重复**：云端旧路径被视为“云端删除”（本地保留），新路径被视为
  “云端新增”（下载），所以本地会同时留下旧文件和新文件。这是“云端删除不删本地”的必然结果。
- **WPS 会规范化文件名**：除了拒绝 `\ : * ? " < > |`，还会去掉名字末尾的空格/点。脚本会
  提前跳过这类路径，避免云端路径和本地路径不一致。
- **0 字节文件无法上传**：当前 wpsdav 上传空文件会得到 `411 MissingContentLength`，脚本
  默认跳过并报告。
- **两边同尺寸但内容不同**：无法校验，会按“已一致”处理。
- **不保留权限、属主、xattr、符号链接**。
- 脚本主要在 Linux/macOS 上验证；Windows 下已做路径分隔符归一化，但单实例锁会退化为
  无锁，且未做完整测试。
- **不做冲突文件重命名**：冲突只记录在报告里，由用户决定。
- 脚本假设远端最终一致延迟不严重；WPS 目录列表短暂滞后时，下一次运行会自动收敛。
- **`--gitignore` 不会跨越被忽略的目录**：如果外层仓库把某个目录整个忽略，该目录下的
  嵌套仓库也不会再读取自己的规则；整个目录本来就不会同步。

## 退出码

| 退出码 | 含义 |
|---:|---|
| 0 | 成功（可能包含已记录的冲突） |
| 1 | 致命错误，未开始或已中止传输 |
| 2 | 部分文件传输失败，详见报告 |
| 130 | 用户中断 |

## 许可

与仓库其余部分一致，使用 MIT License。
