# wpsdav

把 WPS 云文档（个人版 / WPS 365）通过 **WebDAV** 暴露给 rclone 等客户端挂载。

核心设计是**控制面 / 数据面分离**：

```
rclone ──PROPFIND/PUT/MOVE/COPY/DELETE──▶ wpsdav ──Cookie──▶ WPS API
   │                                        │
   └──────── GET → 302 ──▶ 预签名存储 URL ──▶ WPS 对象存储
```

- 元数据操作走 wpsdav（轻量 JSON 请求）。
- 下载时 wpsdav 返回 `302` 指向 WPS 的预签名临时 URL，数据从 WPS CDN 直达 rclone。
  实测该 URL 对 rclone UA、curl UA、甚至无 UA 的客户端都返回 206，因此
  `download_mode: "redirect"` 下**大流量完全不经过中间层**，wpsdav 可以部署成任意
  位置的轻量控制面。
- 上传必须经过 wpsdav：WPS 的三段式上传要求先提交 sha1/sha256/md5 和大小换取
  存储直传地址，客户端无法独立完成。

## 快速开始

```bash
go build -o wpsdav .
cp config.example.json wpsdav.json     # 改 username/password
```

配置放在当前目录（`wpsdav.json`，找不到则用 `config.json`），所以日常不需要传参数：

```bash
wpsdav cookie 'u=...; wps_sid=...; ...'   # 从浏览器 DevTools 复制整串 Cookie
wpsdav doctor                             # 只读诊断：登录态、分组、列表、下载 URL
wpsdav serve                              # 启动
```

命令行遵循 GNU 惯例：长选项用 `--`，短选项用单横线（例如 `-c, --config`、
`-p, --provider`）。`--config` 指向其他目录时，配置里的相对路径（`cookie_file`、
`spool_dir`）按配置文件所在目录解析。

`doctor` 会用 rclone 风格的 UA（不带 Referer）探测下载链接。通过则保持
`download_mode: "redirect"`；若 CDN 拒绝非浏览器客户端，改为 `"proxy"`（数据会经过
wpsdav）。

## 认证 Provider

认证被抽象成独立的 provider 层（`internal/auth`），HTTP、缓存、CLI 均不感知登录
方式。`auth.mode` 选择实现：

| mode | 行为 |
|---|---|
| `cookie`（默认） | 从 `cookie_file` 读取；文件变化 3 秒内热加载 |
| `buaa` | 通过北航统一身份认证（`wps.buaa.edu.cn` → CAS）登录，`wps_sid` 自动获取与续期；登录结果写回 `cookie_file` 作为缓存 |

`buaa` 模式示例：

```json
{
  "auth": {
    "mode": "buaa",
    "sso": {
      "username": "学号",
      "password": "密码",
      "entry_url": "https://wps.buaa.edu.cn/",
      "refresh_minutes": 240
    }
  }
}
```

登录是显式选择 provider 的命令（目前支持北航）：

```bash
wpsdav login --provider buaa 学号   # 交互式输入密码，写入 cookie_file（-p 为短选项）
```

密码可以只放在环境变量里，避免落盘：

```bash
export WPSDAV_SSO_USERNAME=...
export WPSDAV_SSO_PASSWORD=...
wpsdav login --provider buaa
wpsdav serve
```

也可以完全不配置密码：先用 `wpsdav login --provider buaa` 登录一次，之后
`serve` 直接使用 `cookie_file` 里的缓存 Cookie。只要缓存里的 `rtk` 还有效，
后台会用 `grant_token` 自动续期；`rtk` 失效时才需要重新登录。

SSO 链路：CAS 登录页 → 提交学号密码 → ticket → `verify_active` 换 ssid →
ECDSA P-256 签名登录 → `.wps.cn` 全套 Cookie；之后用 `rtk` 走 `grant_token` 续期，
失效则自动重登。密码不落盘、不进日志（除非写进配置文件）。该流程移植自
`comate-gateway` 已验证的协议实现；连续输错会触发验证码，需先在浏览器登录解除。

## 模块结构

```
main.go            命令行与装配（serve/doctor/groups/cookie/login）
internal/config    配置加载与校验
internal/auth      Provider 接口 + cookie / buaa 两个实现
internal/sso       北航 CAS 协议客户端（纯 HTTP，无配置/文件依赖）
internal/wps       WPS API 客户端（列表/下载链接/上传/文件操作）
internal/kvfs      WebDAV 文件系统适配 + 路径解析 + 目录缓存
internal/server    HTTP 层：Basic Auth、DAV 挂载、302 拦截、控制端点
```

依赖方向单向：`main → server/auth → kvfs/sso → wps → config`。`sso` 只做协议，
`auth` 只产 Cookie，`server` 只做 HTTP，`kvfs` 只做路径/缓存语义。

## rclone 配置

```ini
[wpsdav]
type = webdav
url = http://127.0.0.1:8080/dav
vendor = other
user = wpsdav
pass = <rclone obscure 后的密码>
```

挂载：

```bash
rclone mount wpsdav: /mnt/wps \
  --vfs-cache-mode writes \
  --dir-cache-time 1m \
  --poll-interval 0 \
  --transfers 4
```

同步：

```bash
rclone sync /local wpsdav: --size-only --filter "- .*"
```

- `--size-only` 是必须的：WPS API 无法设置 mtime，云端修改时间恒为上传时间。
  注意 `size_only` 写进 rclone.conf 会被部分版本静默忽略。
- `--filter "- .*"` 过滤点开头的文件：WPS 拒绝此类文件名（返回 400“文件名为空”）。

## 运维

- **Cookie 更新**：覆盖 `cookie.txt`（自动热加载），或
  `curl -u user:pass -X PUT --data-binary @cookie.txt http://127.0.0.1:8080/admin/cookie`。
- **健康检查**：`curl -u user:pass http://127.0.0.1:8080/healthz?quota=1`。
- **清缓存**：`curl -u user:pass -X POST http://127.0.0.1:8080/admin/refresh`。
- **凭据自愈**：serve 每 2 分钟校验一次登录态；失效时按 provider 自动重登
  （cookie 模式会打警告，buaa 模式会重新登录）。
- **远程部署**：监听 `0.0.0.0:8080` 时必须放在 TLS 反代后面；Cookie 等价于完整
  账号权限，`cookie.txt` 保持 0600。
- **管理多个盘**：`wpsdav groups` 列出全部分组/团队文档；把目标 id 填进
  `root_group_id` 即可挂载。`mode: "business"` 走 WPS 365 企业接口，需要
  `root_group_id`；`personal`（默认）自动挂载“我的企业文档”。

## 已知限制

| 限制 | 说明 |
|---|---|
| mtime 只读 | 上传后修改时间为服务端时间；用 `--size-only` 比较 |
| 无校验和 | 云端不提供可靠 checksum/ETag，`--checksum` 不可用 |
| 点文件被拒 | `.DS_Store`、`.gitignore` 等无法上传，用过滤规则规避 |
| 覆盖语义 | 同名上传走 `up_new_ver` 覆盖而不是生成 `name(1)` |
| 最终一致 | 写操作后目录列表可能短暂滞后，wpsdav 会把写入项注入缓存 |
| Cookie 过期 | cookie 模式无 refresh token；buaa 模式自动续期 |
| 频率限制 | 403 `fileTaskDuplicated` 自动退避重试；批量操作避免连发 |

## 协议选择

当前实现是 WebDAV：WPS 本身是树形文件系统，WebDAV 的目录/文件/MOVE/COPY 语义
与之一一对应，协议面小、正确性容易保证；且 GET 支持 302，天然满足“下载不落
中间层”。S3 语义是扁平 key + 前缀模拟目录，需要目录标记、prefix/delimiter 分页、
multipart 映射，且多数 S3 客户端不期望 GetObject 返回 302（下载只能代理）。
若将来需要给非 rclone 的 S3 客户端使用，可在 `internal/kvfs` + `internal/wps`
之上再加一个 S3 适配层——核心引擎与协议无关。
