# wpsdav-upload：右键上传到 WPS

[`wpsdav-upload.py`](wpsdav-upload.py) 配合 [`wpsdav-upload.desktop`](wpsdav-upload.desktop)，
在 KDE/Dolphin 里给任意文件或文件夹加一个右键菜单：上传到 WPS 云盘的固定目录，
并在云端按本地家目录结构自动建子目录。

## 特性

- 支持单个/多个文件，也支持文件夹递归上传。
- 默认从 `$HOME` 计算相对路径：
  `~/Documents/foo.pdf` → `<root>/Documents/foo.pdf`
- 默认云端根目录：`wps:右键上传`。
- 两个归档模式：
  - **覆盖**：同名文件直接覆盖。
  - **按时间归档**：`<root>/YYYY-MM-DD_HH-MM-SS/...`，保留历史。
- 本地文件名不会被修改；WPS 不接受的名字只对云端路径做规范化：
  - `\ : * ? " < > |` → `_`
  - 开头的点 → 前缀 `_`（`.env` → `_env`）
  - 末尾空格/点去掉
  - 映射写入日志，重名自动加 `~2`、`~3`
- 文件夹递归默认包含点文件/点目录；需要跳过时用 `--exclude-hidden`。
- 空文件跳过（当前 WPS 上传 0 字节文件会返回 411）。
- 符号链接文件跟随；符号链接目录在递归时跳过，避免循环。
- 大文件/多文件显示 KDE 进度窗口，包含百分比、速度、ETA。
- 成功用桌面通知，失败用错误弹窗；失败详情写入日志。
- 选择里包含 `~/.ssh` 时，会先弹确认框。
- 单次大体积/多文件上传会先弹确认框（默认 5 GiB 或 5000 个文件）。

## 安装

```bash
install -m755 examples/wpsdav-upload.py ~/.local/bin/wpsdav-upload
install -m644 examples/wpsdav-upload.desktop \
  ~/.local/share/kio/servicemenus/wpsdav-upload.desktop
```

KDE 5 的菜单目录是 `~/.local/share/kservices5/ServiceMenus/`。

如果 `wpsdav-upload` 不在 KDE 的 `PATH` 里，把 `.desktop` 里的 `TryExec` 和 `Exec`
改成绝对路径，例如 `/home/you/.local/bin/wpsdav-upload`。

刷新菜单缓存：

```bash
kbuildsycoca6 --noincremental
# KDE 5: kbuildsycoca5 --noincremental
```

如果 Dolphin 里暂时看不到，关掉再打开 Dolphin，或重新登录一次。

## 右键菜单

选中文件/文件夹 → 右键 → **上传到 WPS 云盘**，有两个动作：

| 动作 | 行为 |
|---|---|
| 上传（同名文件直接覆盖） | 直接写入 `<root>/相对路径`，同名文件被替换 |
| 上传到新的日期文件夹（保留历史） | 写入 `<root>/YYYY-MM-DD_HH-MM-SS/相对路径`，不会覆盖旧文件 |

文件夹里的点文件/点目录默认也会上传。

默认 `root` 是 `wps:右键上传`，可以在命令行用 `--root` 修改，或设置环境变量
`WPS_UPLOAD_ROOT`。

## 命令行用法

```bash
# 单个文件
wpsdav-upload ~/Documents/report.pdf

# 多个文件/文件夹
wpsdav-upload ~/Pictures/2026 ~/Documents/notes

# 按时间归档
wpsdav-upload --mode timestamp ~/workspace/project

# 只看看会传到哪
wpsdav-upload --dry-run --base "$HOME" --root wps:右键上传 ~/Documents/foo.pdf

# 跳过隐藏文件
wpsdav-upload --exclude-hidden ~/.config/myapp
```

常用参数：

| 参数 | 说明 |
|---|---|
| `--base PATH` | 计算相对路径的根，默认 `$HOME` |
| `--root REMOTE` | 云端根，默认 `wps:右键上传` |
| `--mode overwrite\|timestamp` | 覆盖或按时间归档，默认覆盖 |
| `--exclude-hidden` | 递归文件夹时跳过点文件/点目录（默认包含） |
| `--no-gui` | 不弹窗、不显示进度条 |
| `-y, --yes` | 跳过敏感文件/大体积确认 |
| `-v, --verbose` | 逐文件输出成功/失败 |
| `--dry-run` | 只打印映射，不传输 |

## 路径与日志

- 日志：`~/.local/state/wpsdav-upload/last.log`
- 包含：本地原路径 → 云端路径、跳过统计、rclone 命令、失败详情、平均速度。
- 云端路径规范化只在云端发生，本地文件不变；重命名数量会出现在成功弹窗里。

## 进度与弹窗

- 总大小 ≥ 1 MiB：显示 KDE 进度窗口，实时显示
  `45%  1.2 GiB/3.4 GiB  8.8 MiB/s  ETA 4m12s`。
- 上传完成：桌面通知，包含文件数、总大小、平均速度和云端目录。
- 上传失败：错误弹窗，包含失败数量和日志路径。
- 由 KDE 服务菜单启动时，弹窗使用当前桌面会话的 D-Bus；从 SSH 等无图形环境运行时
  请加 `--no-gui`。

## 安全确认

只对 `~/.ssh` 触发确认（`-y` 可跳过）。其他路径如果被显式选中，会直接上传。

默认 5 GiB 或 5000 个文件以上也会先确认，可用环境变量调整：

```bash
WPS_UPLOAD_CONFIRM_GB=20 WPS_UPLOAD_CONFIRM_FILES=20000 wpsdav-upload ...
```

## WPS 登录

如果弹窗提示 `userNotLogin / 用户未登录`：

```bash
wpsdav cookie -c ~/.config/wpsdav/config.json
wpsdav doctor -c ~/.config/wpsdav/config.json
```

注意：wpsdav 的配置查找顺序会优先使用当前目录的 `./wpsdav.json`。在包含该文件的仓库
目录里运行 `wpsdav doctor` 时，如果不加 `-c`，读到的可能不是服务使用的用户配置。
排查登录问题时建议 `cd /tmp` 或者显式 `-c ~/.config/wpsdav/config.json`。

wpsdav 的 `cookie` 模式不会自动续期；`buaa` 模式会使用登录后的 `rtk` Cookie
通过 `grant_token` 自动续期。刷新令牌失效后需要重新执行
`wpsdav login --provider buaa 学号 -c ~/.config/wpsdav/config.json`。
`login` 只写 Cookie，不会修改 `auth.mode`；要启用自动续期，配置里需要
`"auth": {"mode": "buaa"}`。

## 已知限制

- 空文件不能上传（WPS 返回 411）。
- 云端文件名会被规范化，和本地不一定完全一致；日志里有完整映射。
- 覆盖模式下同名文件没有历史版本；需要历史时用“按时间归档”。
- 不保留权限、属主、xattr；符号链接目录不会递归。
- 云端重命名/移动不会反向影响本地（这是单向上传工具）。
- 上传是单次任务，不做去重/增量；重复上传同一文件夹会再次传输。需要增量备份请用
  restic/kopia。

## 许可

与仓库其余部分一致，使用 MIT License。
