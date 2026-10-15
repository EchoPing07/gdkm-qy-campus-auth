# gdkm-qy-campus-auth

广东科贸职业学院（清远校区）校园网认证保活工具。Go 单文件静态编译，CGO=0，仅标准库、零第三方依赖。

只服务 `10.0.128.3`（km-bras）这一个 Portal，协议面收敛为 3 个免凭据请求：

| # | 请求 | 用途 |
|---|---|---|
| 1 | `GET http://5.5.5.5/`（禁跟随重定向） | 三态判定唯一依据；302 Location 即认证所需全部 BRAS 参数 |
| 2 | `POST https://10.0.128.3/api/portal/webauth` | 登录（11 字段，参数现取现用） |
| 3 | `POST https://10.0.128.3/api/portal/webdisconn` | 下线（distoken 取自探测 302 新鲜值，pageid 可空） |

## 构建

```bash
bash build.sh                        # Linux / macOS / Git Bash
powershell -ExecutionPolicy Bypass -File build.ps1   # Windows PowerShell
```

两个脚本等价，均输出 `dist/` 下 5 个产物：`gdkm-qy-campus-auth-{linux-amd64, linux-arm64, windows-amd64.exe, darwin-amd64, darwin-arm64}`。版本号通过环境变量 `VERSION` 注入（如 `VERSION=v1.2.3 bash build.sh`），缺省 `dev`；GitHub Actions 推送 `v*` 标签时自动以标签名注入。版本号显示于网页标题旁与启动日志。

## 运行

```bash
./gdkm-qy-campus-auth                  # 配置 = 二进制同目录 config.json
./gdkm-qy-campus-auth --config /path/to/config.json
```

- 首次运行自动生成默认 `config.json`（WebUI 管理密码 `password`，认证账号 `admin` / `password`，均为占位默认值，请按需修改）；
- 启动即常驻 + WebUI，无 CLI；手动操作全走网页；
- 日志 stdout + `campus-auth.log` 双写，单文件超 4MB 滚动为 `.old`，只保留 1 份；
- 日志 / 配置文件相对路径均基于**二进制所在目录**解析。

## 配置

```json
{
  "web": { "listen": "0.0.0.0:10086", "password": "password" },
  "log": { "level": "info", "file": "campus-auth.log", "max_mb": 4 },
  "check_interval": 15,
  "login_retries": 3,
  "retry_interval": 10,
  "reject_cooldown": 600,
  "interfaces": [
    { "name": "以太网", "mode": "manual", "user": "admin", "password": "password" },
    { "name": "wlan0",  "mode": "auto",   "user": "admin", "password": "password" }
  ]
}
```

| 字段 | 规则 |
|---|---|
| `web.listen` / `web.password` | 非空 |
| `log.level` | ∈ {debug, info, error} |
| `check_interval` | 数值 > 0 |
| `login_retries` | 整数 ≥ 0 |
| `retry_interval` | 数值 > 0 |
| `reject_cooldown` | 数值 ≥ 0 |
| `interfaces[]` | 非空数组；`name`/`mac` 二选一非空；`mode` ∈ {auto, manual}（缺省 manual）；`user`/`password` 非空；`probe` 缺省 true |

## WebUI

浏览器访问 `http://<listen地址>/`，标题旁显示构建版本号：

- **路由**：登录 `/login`、状态 `/status`、日志 `/logs`、设置 `/settings`，支持深链接与浏览器前进后退，未登录自动跳转登录页；
- **状态**：每网口卡片（模式、状态徽标、IP/MAC、账号、最近动作结果原文）+ 按钮：立即探测 / 登录 / 下线 / 模式切换；
- **日志**：本页会话事件（动作结果、状态变化）；
- **设置**：表单编辑全部配置，保存时全量校验，合法则落盘 `config.json` 并即时生效，保存成功 / 失败在右下角弹窗提示；`web.listen` 变更需重启进程。「出口网口绑定」的定位值（系统网口名 / 网口物理地址）支持手填与「从本机选择」两种方式，点「识别本机」刷新本机网口清单（名称 / MAC / IPv4 / 启用状态）。

行为要点：

- `manual` 模式：按配置探测并展示状态，不做自动登录；网页「登录」单发一次请求；
- `auto` 模式：循环探测，发现未认证自动登录（默认 3 次重试 × 10s 间隔），明确拒绝进入 600s 冷却；
- `manual + probe=false`：不探测，界面显示「未探测」，登录/下线按钮触发前先临时探测一次取参；
- 下线：每次先探测取 distoken，再提交 `webdisconn`；
- 多网口每网口独立协程，请求绑定该口源 IP，互不影响。

## API

页面路由：`GET /`、`/login`、`/status`、`/logs`、`/settings`（同一份嵌入页，前端按路径渲染）。

以下接口均需 cookie 会话（`POST /api/login` 换取）：

```
GET  /api/status                     状态快照
GET  /api/netifs                     本机网口清单（设置页「从本机选择」数据源）
GET  /api/config                     读配置
PUT  /api/config                     写配置（全量校验 + 落盘 + 即时生效）
POST /api/probe/{网口}               立即探测
POST /api/login/{网口}               登录（manual 单发；auto 触发登录序列）
POST /api/logout/{网口}              下线
POST /api/mode/{网口}                模式切换（manual↔auto，落盘）
```

## 自启

暂无相关集成计划，如有需要请参考平台示例自行配置

### Linux

**Linux systemd** `/etc/systemd/system/gdkm-qy-campus-auth.service`：

```ini
[Unit]
Description=Campus Auth (GDKM QY)
After=network-online.target
Wants=network-online.target

[Service]
WorkingDirectory=/opt/gdkm-qy-campus-auth
ExecStart=/opt/gdkm-qy-campus-auth/gdkm-qy-campus-auth
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

`systemctl daemon-reload && systemctl enable --now gdkm-qy-campus-auth`

### Windows
**Windows**（管理员）：计划任务

```
schtasks /Create /TN "GdkmQyCampusAuth" /TR "C:\gdkm-qy-campus-auth\gdkm-qy-campus-auth.exe" /SC ONLOGON /RL HIGHEST /F
```

或把 exe 快捷方式放入「启动」文件夹（`shell:startup`）。

### MacOS

**MacOS launchd** `~/Library/LaunchAgents/com.campus.gdkm-qy-auth.plist`：

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.campus.gdkm-qy-auth</string>
  <key>ProgramArguments</key><array>
    <string>/Users/you/gdkm-qy-campus-auth/gdkm-qy-campus-auth</string>
  </array>
  <key>WorkingDirectory</key><string>/Users/you/gdkm-qy-campus-auth</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict>
</plist>
```

`launchctl load ~/Library/LaunchAgents/com.campus.gdkm-qy-auth.plist`

### Android

**Android（root，Magisk 模块）**：复用 `linux-arm64` 二进制，模块 `service.sh`：

```sh
#!/system/bin/sh
MODDIR=${0%/*}
(while true; do
  "$MODDIR/gdkm-qy-campus-auth" --config "$MODDIR/config.json"
  sleep 5
done) &
```

二进制与 `config.json` 放入模块目录并 `chmod 755`。

## 安全注意

- WebUI 为 HTTP 明文 + 明文密码比对，无 CSRF/防暴破/会话过期，仅限可信网络使用；
- `config.json` 与日志含明文账号密码，注意文件权限，勿提交版本库（已在 `.gitignore`）；
- 请勿在代码、配置或文档中提交真实校园网账号密码。
