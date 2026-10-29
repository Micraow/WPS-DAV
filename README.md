# wpsdav

[![GitHub](https://img.shields.io/badge/GitHub-Micraow%2FWPS--DAV-181717?logo=github)](https://github.com/Micraow/WPS-DAV)
[![Release](https://github.com/Micraow/WPS-DAV/actions/workflows/release.yml/badge.svg)](https://github.com/Micraow/WPS-DAV/actions/workflows/release.yml)
![Go](https://img.shields.io/badge/Go-1.21%2B-00ADD8?logo=go&logoColor=white)
![Platform](https://img.shields.io/badge/platform-Linux%20%7C%20macOS%20%7C%20Windows-lightgrey)
![Protocol](https://img.shields.io/badge/protocol-WebDAV-blue)
![rclone](https://img.shields.io/badge/works%20with-rclone-1f425f)
![License](https://img.shields.io/badge/license-MIT-blue)
![Status](https://img.shields.io/badge/status-experimental-orange)

**把金山文档（WPS / Kdocs）的云文档空间映射为标准 WebDAV 端点。**

*Map your Jinshan Docs / WPS cloud drive as a standard WebDAV endpoint.*

用 rclone、Finder、Windows 资源管理器、RaiDrive、Cyberduck 或任何 WebDAV 客户端，
把「我的云文档」和团队文档库直接挂载成本地盘符。

> ⚠️ 本项目基于**非官方逆向接口**实现，上游随时可能变更或失效。请勿用于关键数据，
> 使用前请自行评估账号风险。

## ✨ 为什么用 wpsdav

- 🚀 **下载不经过中间层**：文件读取返回 302 跳转到 WPS 预签名直链，数据从 WPS CDN
  直达客户端。挂载在低配 VPS 上也不会成为带宽瓶颈。
- 🗂️ **真正的文件系统语义**：目录、重命名、移动、复制、删除全部映射到云端原生操作，
  不是前缀模拟出来的假目录。
- 🔁 **可长期无人值守**：Cookie 热加载；可选 SSO Provider 自动登录与续期。
- 🧩 **面向所有人**：个人版和 WPS 365 企业版都支持，浏览器 Cookie 即可上手。
- 🪶 **单文件部署**：一个静态编译的二进制，没有运行时依赖。

## 🚀 快速开始

### 1. 安装

Arch Linux（AUR）：

```bash
yay -S wpsdav-bin
# 或 paru -S wpsdav-bin
```

其他平台可以从 [Releases](https://github.com/Micraow/WPS-DAV/releases) 下载预编译二进制：
Linux x86_64、Windows x86_64、macOS arm64（Apple Silicon）。

从源码构建：

```bash
git clone https://github.com/Micraow/WPS-DAV.git && cd WPS-DAV
go build -o wpsdav .
```

### 2. 初始化

```bash
wpsdav init
# 生成 ~/.config/wpsdav/config.json（0600）和随机 WebDAV 密码
```

`init` 会打印生成的用户名和密码，并把 Cookie 文件放到
`~/.local/state/wpsdav/cookie.txt`。需要系统级配置时用
`sudo wpsdav init --system`（写入 `/etc/wpsdav/config.json`）。

配置查找顺序（第一个匹配生效）：

```
--config PATH  >  $WPSDAV_CONFIG  >  ./wpsdav.json  >  ./config.json
               >  ~/.config/wpsdav/config.json  >  /etc/wpsdav/config.json
```

启动时会打印实际加载的配置文件路径，避免“不知道用的哪份配置”。

### 3. 导入 Cookie

浏览器打开 [金山文档](https://www.kdocs.cn)，登录后按 `F12` → Network → 任意请求 →
复制请求头里的整串 `Cookie`：

```bash
wpsdav cookie
# 粘贴后按 Ctrl-D（Windows 按 Ctrl-Z 回车）
```

也可以直接写在参数里：`wpsdav cookie 'u=...; wps_sid=...; ...'`。

### 4. 自检

```bash
wpsdav doctor
```

会依次检查登录态、云盘列表、目录读取和下载直链。看到
`recommendation: download_mode=redirect` 就说明下载可以完全不经过中间层。

### 5. 启动

```bash
wpsdav serve
# listening on 127.0.0.1:8705 (dav path /dav, download mode redirect)
```

## 🔌 挂载

### rclone

最简配置与挂载：

```bash
rclone config create wpsdav webdav \
  url=http://127.0.0.1:8705/dav \
  vendor=other \
  user=wpsdav \
  pass=$(rclone obscure '你的密码')

rclone mount wpsdav: /mnt/wps --vfs-cache-mode writes
```

就这两条命令即可日常使用。

<details>
<summary>🚀 高性能 + 低磁盘占用配置（推荐长期挂载）</summary>

```bash
rclone mount wpsdav: /mnt/wps \
  --vfs-cache-mode writes \
  --vfs-cache-max-size 5G \
  --vfs-cache-max-age 24h \
  --dir-cache-time 1m \
  --poll-interval 0 \
  --transfers 8 \
  --checkers 12 \
  --buffer-size 32M \
  --vfs-read-ahead 32M \
  --vfs-read-chunk-size 16M \
  --vfs-read-chunk-size-limit 128M \
  --size-only \
  --filter "- .*"
```

| 参数 | 作用与取值理由 |
|---|---|
| `--vfs-cache-mode writes` | **只缓存待上传的写入**，读取直接流式走 302 直链，不占读缓存。磁盘占用最小，适合不想让挂载吃掉空间的场景 |
| `--vfs-cache-max-size 5G` | 写缓存上限；按可用磁盘调整。写的文件上传完成后缓存会被回收 |
| `--vfs-cache-max-age 24h` | 缓存最长保留时间，兜底清理 |
| `--dir-cache-time 1m` | 目录列表缓存 1 分钟；调大可减少 WPS 请求，但看不到别人刚分享的文件 |
| `--poll-interval 0` | WebDAV 后端不支持变更轮询，设为 0 避免无意义请求 |
| `--transfers 8` | 并发上传数。WPS 对高频写敏感，8 是较稳妥的上限，遇到限流可降到 4 |
| `--checkers 12` | 并发元数据检查数；16 核机器上 12 足够快，也不至于压垮接口 |
| `--buffer-size 32M` | 每个传输的内存缓冲；大文件顺序读写更顺（8 并发约 256M 内存） |
| `--vfs-read-ahead 32M` | 预读窗口；下载走 302 直链，预读能充分利用带宽 |
| `--vfs-read-chunk-size 16M` | 起始 Range 块大小，随后按需翻倍 |
| `--vfs-read-chunk-size-limit 128M` | Range 块增长上限，避免单次请求过大 |
| `--size-only` | **必加**：云端修改时间不可控，按大小比较可避免重复上传（见下方 FAQ） |
| `--filter "- .*"` | **必加**：WPS 拒绝点开头的文件名，过滤后可避免报错 |

> 💡 如果你经常随机读大文件、或用 Office 类软件反复编辑同一个文档，可以把
> `--vfs-cache-mode` 改为 `full` 获得更好的随机读性能；此时读数据也会落盘，
> 建议同时设置 `--vfs-cache-max-size 20G` 之类的上限。

如果只是偶尔同步（不是挂载），推荐：

```bash
rclone sync /本地目录 wpsdav:备份目录 --size-only --filter "- .*" --transfers 8
```

</details>

### 其他客户端

| 客户端 | 地址 |
|---|---|
| macOS Finder | `⌘K` → `http://127.0.0.1:8705/dav` |
| Windows 资源管理器 | 添加网络位置 → `http://127.0.0.1:8705/dav`（或映射网络驱动器 `\\127.0.0.1@8705\dav`） |
| RaiDrive / Cyberduck / WinSCP | 新建 WebDAV 连接，填同一地址 |

## 🧭 工作原理

```
WebDAV 客户端 ──PROPFIND/PUT/MOVE──▶ wpsdav ──Cookie──▶ WPS API
      │                                │
      └────── GET → 302 ──▶ 预签名直链 ──▶ WPS 对象存储
```

- **元数据**（列目录、建目录、改名、移动、复制、删除）由 wpsdav 调用 WPS API 完成。
- **下载**返回 302，客户端直接向 WPS CDN 取数据，中间层只承担几百字节的控制流量。
- **上传**必须经过 wpsdav：WPS 要求先提交文件哈希和大小换取直传地址，客户端无法
  独立完成。上传后 wpsdav 会把新文件直接注入目录缓存，规避云端列表的最终一致延迟。

## 🔐 认证

默认使用浏览器 Cookie，开箱即用。登录方式被抽象成独立的 Provider，可按需切换：

| Provider | 适用场景 | 说明 |
|---|---|---|
| `cookie` | 所有人、个人版、企业版 | 从浏览器复制 Cookie；文件变化会自动热加载 |
| `buaa` | 北京航空航天大学账号（可选示例） | 通过学校统一身份认证登录，自动获取并续期 Cookie |

北航 SSO 只是一个内置示例，Provider 接口与具体身份源解耦，欢迎贡献其他学校或
IdP 的实现。

```bash
# 使用北航统一身份认证登录（交互式输入密码，不落盘）
wpsdav login --provider buaa 学号

# 日常使用 Cookie 模式即可
wpsdav cookie
```

不保存密码也能长期运行：登录一次后，`serve` 会使用缓存的 Cookie，并在后台通过
刷新令牌自动续期，直到刷新令牌失效才需要重新登录。

## ⚙️ 配置

| 字段 | 默认值 | 说明 |
|---|---|---|
| `mode` | `personal` | `personal`（个人版接口）或 `business`（WPS 365 接口） |
| `listen` | `127.0.0.1:8705` | HTTP 监听地址 |
| `dav_path` | `/dav` | WebDAV 挂载路径 |
| `username` / `password` | — | WebDAV 端点的 Basic Auth 凭据（必填） |
| `cookie_file` | XDG state 目录 | Cookie 缓存文件；用户配置默认 `~/.local/state/wpsdav/cookie.txt`，`/etc` 配置默认 `/var/lib/wpsdav/cookie.txt` |
| `auth.mode` | `cookie` | `cookie` 或 `buaa` |
| `auth.sso.username` / `password` | — | SSO 账号（也可用环境变量，见下） |
| `auth.sso.entry_url` | `https://wps.buaa.edu.cn/` | SSO 入口 |
| `auth.sso.refresh_minutes` | `240` | 后台续期间隔 |
| `root_group_id` | `0` | 挂载指定分组/团队文档；`0` 表示自动选择 |
| `company_id` | `0` | 企业 ID，`0` 表示自动识别 |
| `download_mode` | `redirect` | `redirect`（302 直链）或 `proxy`（中间层转发） |
| `cache_ttl_seconds` | `60` | 目录缓存时间 |
| `custom_ua` | — | 自定义 User-Agent |
| `spool_dir` | 系统临时目录 | 上传缓冲目录 |

SSO 密码也可以只放在环境变量里：

```bash
export WPSDAV_SSO_USERNAME=学号
export WPSDAV_SSO_PASSWORD=密码
```

## 🧰 命令

```text
wpsdav init      生成配置文件与随机 WebDAV 密码
wpsdav serve     启动 WebDAV / 控制服务
wpsdav doctor    只读自检：登录态、云盘列表、目录读取、下载直链
wpsdav groups    列出当前账号可见的全部分组和团队文档
wpsdav cookie    安装浏览器 Cookie（参数或 stdin）
wpsdav login     通过指定 Provider 登录（目前内置 buaa）
wpsdav version   查看版本信息
```

通用选项：`-c, --config` 指定配置文件；`-p, --provider` 选择登录 Provider。
配置里的相对路径按配置文件所在目录解析。

## 🛠 运维

- 健康检查：`curl -u user:pass http://127.0.0.1:8705/healthz?quota=1`
- 刷新缓存：`curl -u user:pass -X POST http://127.0.0.1:8705/admin/refresh`
- 更新 Cookie：直接覆盖 `cookie.txt`，或
  `curl -u user:pass -X PUT --data-binary @cookie.txt http://127.0.0.1:8705/admin/cookie`
- 用户级服务：安装包（0.1.1+）自带 systemd 用户单元，`systemctl --user enable --now wpsdav` 即可开机自启（配置需先用 `wpsdav init` 生成）。
- 远程部署：监听 `0.0.0.0` 时请置于 TLS 反向代理之后，Cookie 等价于完整账号权限。

## ❗ 已知限制

| 限制 | 说明 |
|---|---|
| 修改时间只读 | 云端不提供设置 mtime 的接口，同步请用 `--size-only` |
| 无校验和 | 不提供可靠的 checksum/ETag，`--checksum` 不可用 |
| 点文件被拒 | `.gitignore`、`.DS_Store` 等无法上传，用过滤规则规避 |
| 列表最终一致 | 写入后目录可能短暂滞后，wpsdav 会主动注入缓存 |
| Cookie 会过期 | `cookie` 模式需重新从浏览器导出；`buaa` 模式可自动续期 |
| 接口非官方 | 上游改版可能导致功能失效，请关注 Release |

## 💡 常见问题

**为什么看不到上传/下载速度？**

- **下载**：`redirect` 模式下 wpsdav 只返回 302，数据由 rclone 直接从 WPS CDN 传输，
  中间层看不到字节，所以无法在 wpsdav 端显示速度。用 rclone 自己的进度显示：
  `rclone mount ... --progress`（或 `--stats 1s --stats-one-line`）。
  如果确实需要中间层测速，可切换 `download_mode: "proxy"`，但会失去直链带宽优势。
- **上传**：数据经过 wpsdav（先落盘算哈希再传），完成后会输出一行测速，例如：
  `[upload] /资料/报告.pdf  12.5 MiB in 2.1s (6.0 MiB/s)`。

**CLI 颜色**

wpsdav 会自动检测终端：交互式终端下输出彩色，管道/日志文件中保持纯文本。
设置 `NO_COLOR=1` 或 `TERM=dumb` 可以强制关闭颜色。

**修改时间为什么总是不对？会不会导致 rclone 重复上传？**

WPS 没有提供设置 mtime 的接口，云端保存的是“上传时间”，所以每次上传后文件的修改时间
都会变成当时的时间。rclone 默认按“大小 + 修改时间”判断文件是否变化，这会导致本地文件
每次都显得“不一样”，从而反复重传。

解决办法是加 `--size-only`，让 rclone 只比较文件大小。挂载模式（`rclone mount`）本身
不会主动重新上传任何文件，只有你写入或修改文件时才会触发上传，因此不存在“挂载导致
反复上传”的问题；需要留意的是**同尺寸内容修改不会被识别**（备份场景通常可以接受）。

**下载速度慢？** 确认 `download_mode` 为 `redirect`，并检查客户端是否能直连
WPS 的 CDN 域名。

**上传大文件失败？** 确认 `spool_dir` 所在磁盘有足够空间——上传需要先落盘计算哈希。

**看不到刚上传的文件？** WPS 目录列表有短暂延迟。wpsdav 会缓存写入结果，稍等或
调用 `/admin/refresh` 刷新。

**团队文档库怎么挂载？** 运行 `wpsdav groups` 查看分组 ID，填到 `root_group_id`。
企业接口另需将 `mode` 设为 `business`。

**会不会影响我在网页版的使用？** 不会修改网页端数据；但重新登录可能使旧会话失效。
删除操作进入 WPS 回收站，可恢复。

## 🙏 致谢

本项目的接口细节和协议实现参考了以下开源项目与实践，在此致谢：

- [OpenList](https://github.com/OpenListTeam/OpenList) — WPS 驱动的文件操作与上传流程
- [wps365-drive-gateway](https://github.com/sasonwong/wps365-drive-gateway) — 网关设计与 rclone 适配经验
- 北航统一身份认证协议实现（comate-gateway 项目）

## ⚠️ 免责声明

本项目与金山办公、WPS、Kdocs 无任何关联，使用非官方接口，未获得官方授权。
使用者需自行承担账号安全、服务条款合规与数据完整性风险。删除操作虽然进入回收站，
但仍请做好备份。请遵守所在组织的相关规定与金山办公的服务条款。
