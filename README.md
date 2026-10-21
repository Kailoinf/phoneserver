# Phone Control Server

把你自己的 Android 手机变成一台**可编程的设备**：一个跑在手机上的 root 守护进程，把遥测、屏幕、输入、应用、通知、相机麦克风、文件系统和 MCP 全部暴露成 HTTP / WebDAV / MCP 接口。

它是 Magisk / KernelSU 模块，随开机自启，被 watchdog 守护。所有数据留在手机上：**没有任何向远端服务器上传的功能，也没有云端依赖**。

---

## 目录

- [能力一览](#能力一览)
- [安全模型（先读这个）](#安全模型先读这个)
- [安装、升级、卸载](#安装升级卸载)
- [快速开始](#快速开始)
- [API](#api)
  - [遥测](#遥测)
  - [截图与实时画面](#截图与实时画面)
  - [输入与 UI 自动化](#输入与-ui-自动化)
  - [应用](#应用)
  - [设备控制](#设备控制)
  - [通知与消息](#通知与消息)
  - [相机与麦克风](#相机与麦克风)
  - [文件服务与 WebDAV](#文件服务与-webdav)
  - [受控端点：shell 与电源](#受控端点shell-与电源)
  - [配置](#配置)
  - [历史数据与数据质量](#历史数据与数据质量)
- [MCP（给 AI agent 用）](#mcp给-ai-agent-用)
- [WebUI](#webui)
- [已知限制](#已知限制)
- [常见故障排查](#常见故障排查)
- [运维](#运维)
- [开发者](#开发者)

---

## 能力一览

| 能力 | 端点 | 依赖 |
|---|---|---|
| 遥测（电池/CPU/GPU/内存/存储/温度/网络/Wi-Fi/进程/包） | `/api/v1/*` | 无 |
| 截图 | `GET /api/v1/screenshot` | 无（平台的 `screencap`） |
| 实时画面（硬件 H.264，低延时低流量） | `GET /api/v1/stream/screen` | 无（平台的 `screenrecord`） |
| 实时画面降级（webp 帧） | `GET /api/v1/stream/screen/mjpeg` | 无 |
| 输入（点击/滑动/手势/文本/按键） | `POST /api/v1/input/*` | 无（中文输入需要桥） |
| UI 自动化（控件树、按文字点击） | `/api/v1/ui/*` | 无（`uiautomator`） |
| 应用（列表/启动/停止/前台） | `/api/v1/apps*` | 无 |
| 设备控制（屏幕/音量/亮度/旋转/手电/振动/剪贴板/TTS/红外/媒体/Wi-Fi/传感器/定位） | `/api/v1/control/*` | 大部分需要 Termux:API 桥 |
| 通知（读取/发送） | `/api/v1/notifications*` | 无（root 读 `dumpsys notification`） |
| 消息（短信/通话记录/联系人） | `/api/v1/messages/*` | 无（content provider） |
| 相机（拍照、低帧率预览） | `/api/v1/camera/*` | Termux:API 桥 |
| 麦克风（录音） | `/api/v1/mic/*` | Termux:API 桥 |
| 文件服务（浏览/上传/下载） | `/files/` | 无 |
| WebDAV（映射为网络驱动器） | `/dav/` | 无 |
| MCP（stdio + HTTP） | `phoneserver -mcp`、`POST /mcp` | 无 |
| 本地历史（SQLite，带数据质量报告） | `/api/v1/history*` | 无 |
| 受控：root shell、重启/关机 | `/api/v1/shell`、`/api/v1/device/*` | 需配置开关 |

---

## 安全模型（先读这个）

**1）默认只监听本机。**

```json
{ "listen": "127.0.0.1:8080" }
```

装好之后，同一局域网的任何设备都连不上。要从电脑访问，必须**显式打开局域网开关**并把 `listen` 改成手机的局域网地址：

```json
{
  "listen": "192.168.5.29:8080",
  "security": { "allow_non_loopback_listen": true }
}
```

两道闸门是有意的：`listen` 写错了不会生效（服务会回落到 `127.0.0.1:<port>` 并在日志里说明），而且 `PUT /api/v1/config` 会直接拒绝在开关关闭时把 `listen` 设成非回环地址。打开了就意味着**能路由到这个端口的人都算进来，token 是唯一凭据**——别放在不可信网络里，也建议在路由器侧限制。

获取手机局域网 IP：

```sh
adb shell 'ip -4 addr show wlan0 | grep inet'
```

**2）token 是唯一凭据。**

首次启动自动生成 24 字节随机 token，存在 `/data/adb/phoneserver/config.json`（0600）。四种传法：

```sh
# header（推荐）
curl -H 'X-API-Key: <token>' http://127.0.0.1:8080/api/v1/all
# bearer
curl -H 'Authorization: Bearer <token>' http://127.0.0.1:8080/api/v1/system
# URL 参数（浏览器/链接用）
curl 'http://127.0.0.1:8080/api/v1/battery?key=<token>'
# HTTP Basic（WebDAV 客户端用；用户名任意，密码=token）
curl -u any:<token> 'http://127.0.0.1:8080/files/'
```

`GET /api/v1/health`、`/`（自带仪表盘）、`/ui/`（完整界面）、`/files/` 与 `/dav/` 的**页面本身**不需要 token，但页面里的每个数据请求都需要——所以浏览器打开后要把 token 填进页面里的输入框（存在 localStorage）。

**3）危险能力默认全关。**

| 开关 | 默认 | 打开后能做什么 |
|---|---|---|
| `security.allow_shell` | `false` | `POST /api/v1/shell` 以 root 执行任意命令 |
| `security.allow_power_commands` | `false` | 重启、进入 recovery/bootloader、关机 |
| `files.read_only` | `false` | 文件服务整体只读 |
| `files.allow_delete` | `false` | 允许删除文件/目录 |
| `files.webdav` / `files.http_files` | `true` | 分别控制两个前门 |

**4）文件服务没有目录穿越面。** URL 里只有「共享名」（`files.shares` 的白名单条目），从不接受裸文件系统路径，且每次请求解析后都会做符号链接解析并再次校验是否仍在共享根内。

---

## 安装、升级、卸载

```sh
# 交叉编译 + 打包（在仓库根目录）
./build.sh

# 安装（两种方式都可以）
#  a) 管理器里安装 phoneserver-module.zip
#  b) 命令行
adb push phoneserver-module.zip /data/local/tmp/
adb shell 'ksud module install /data/local/tmp/phoneserver-module.zip'

# 安装后需要重启手机：模块先落到 /data/adb/modules_update/，重启后才生效
adb reboot
```

重启后自检（`action.sh` 的按钮也会打印同样内容）：

```sh
adb shell 'sh /data/adb/modules/phoneserver/action.sh'
```

卸载：

```sh
adb shell 'ksud module uninstall phoneserver'
```

卸载会停掉进程，但**保留** `/data/adb/phoneserver/`（配置、token、日志、历史库），重新安装后原样继续用。要彻底清理就手动 `rm -rf /data/adb/phoneserver`。

---

## 快速开始

```sh
# 1. 取 token（只在手机本地可读）
adb shell 'grep -o "\"api_token\": \"[^\"]*\"" /data/adb/phoneserver/config.json'

# 2. 健康检查（不需要 token）
adb shell 'curl -s http://127.0.0.1:8080/api/v1/health'

# 3. 一切正常的话，端点索引会列出全部能力
adb shell 'curl -s -H "X-API-Key: <token>" http://127.0.0.1:8080/api/v1/endpoints'

# 4. 手机上的仪表盘 / 完整界面
adb shell 'curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8080/'
adb shell 'curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:8080/ui/'
```

打开局域网访问后，在电脑浏览器里：

```
http://192.168.5.29:8080/       # 轻量仪表盘（遥测 + 历史面板 + 截图）
http://192.168.5.29:8080/ui/    # 完整多页面界面（控制、实时画面、文件、配置…）
http://192.168.5.29:8080/files/ # 文件浏览/上传
```

`/ui/` 与 `/files/` 的页面会自己提示输入 token（Basic 或页面输入框）。

---

## API

全部端点都写死了方法：只读端点是 **GET-only**（用别的动词会得到 405 + `Allow`），写端点只收 POST/PUT。响应统一是 JSON（截图/音频/画面流除外），错误形如：

```json
{ "error": "...", "status": 403, "server_version": "2.0.0" }
```

时间戳一律 RFC3339（手机本地时区）。索引端点 `GET /api/v1/endpoints` 是权威清单，下面是分组说明。

### 遥测

```sh
curl -H 'X-API-Key: <t>' http://127.0.0.1:8080/api/v1/all          # 全部
curl -H 'X-API-Key: <t>' http://127.0.0.1:8080/api/v1/battery
curl -H 'X-API-Key: <t>' http://127.0.0.1:8080/api/v1/{device,props,power,display,cpu,gpu,memory,storage,thermal,network,network/wifi,system,processes,packages}
```

昂贵来源带 TTL 缓存（`getprop` 10 分钟、`cmd wifi status` 5 秒、`dumpsys power` 10 秒、包列表 5 分钟、`/proc` 扫描 10 秒），所以 3 秒一次的仪表盘轮询不会把系统敲疼。

### 截图与实时画面

```sh
# 截图：默认 webp q80；也支持 jpeg/png；meta=1 返回 JSON 元信息（含尺寸/耗时）
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/screenshot?meta=1&quality=70&scale=0.5'
curl -H 'X-API-Key: <t>' -o shot.png 'http://127.0.0.1:8080/api/v1/screenshot?format=png&display=0'
```

`FLAG_SECURE` 的窗口（银行、部分聊天应用）会被系统挡成黑帧——这是平台保护，本项目不去绕过。

实时画面用设备自带的硬件编码器（`screenrecord --output-format=h264`），码流是标准 Annex-B：

```sh
# 电脑上直接看（原始 H.264 直传）
mpv --no-cache --demuxer-lavf-format=h264 \
  "http://192.168.5.29:8080/api/v1/stream/screen?raw=1&key=<t>"
ffplay -fflags nobuffer -flags low_delay -i \
  "http://192.168.5.29:8080/api/v1/stream/screen?raw=1&key=<t>"

# 参数：size=720x1560 & bitrate=3000000 & display=<id> & wake=1 & raw=1
curl -H 'X-API-Key: <t>' -o out.h264 \
  'http://127.0.0.1:8080/api/v1/stream/screen?size=720x1560&bitrate=2000000'
```

非 `raw` 模式的响应体协议（供网页播放器实现，`X-Stream-Protocol: annexb-len-prefix`）：

1. 第一块是一行 UTF-8 JSON + `\n`：
   ```json
   {"protocol":"annexb-len-prefix","codec":"H.264","avc":"avc1.640028","width":720,"height":1560,
    "bitrate":3000000,"fps_hint":60,"size":"720x1560","started_at":"...","display_woken":true}
   ```
   `avc` 由 SPS 解析而来，直接喂给浏览器的 `VideoDecoder.configure({codec})`。
2. 之后每个访问单元（一帧）= 4 字节大端长度 + Annex-B 原始字节（含起始码）。

其它画面端点：

```sh
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/stream/status'   # 活跃编码器、实测帧率/码率
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/stream/display'  # 现在能不能开流（含真实探测结果）
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/stream/screen/mjpeg?fps=2&width=540'  # webp 帧降级
```

**熄屏时 `screenrecord` 会直接失败**（`ERROR: INVALID_LAYER_STACK`），所以开流前要先唤醒屏幕；`stream.wake_display=true`（默认）时服务会自己 `input keyevent KEYCODE_WAKEUP`，并在握手 JSON 里如实报告 `display_woken`。需要长时间看画面时打开 `stream.keep_display_on`，它会在流存活期间保持常亮、最后一个客户端断开后恢复原值。

### 输入与 UI 自动化

```sh
curl -H 'X-API-Key: <t>' -H 'Content-Type: application/json' \
  -d '{"x":640,"y":1386}' http://127.0.0.1:8080/api/v1/input/tap
curl -H 'X-API-Key: <t>' -H 'Content-Type: application/json' \
  -d '{"x1":640,"y1":2000,"x2":640,"y2":600,"duration_ms":300}' http://127.0.0.1:8080/api/v1/input/swipe
curl -H 'X-API-Key: <t>' -H 'Content-Type: application/json' \
  -d '{"points":[[100,1500],[500,1500],[500,900]],"duration_ms":600}' http://127.0.0.1:8080/api/v1/input/gesture
curl -H 'X-API-Key: <t>' -H 'Content-Type: application/json' \
  -d '{"text":"hello world"}' http://127.0.0.1:8080/api/v1/input/text
curl -H 'X-API-Key: <t>' -H 'Content-Type: application/json' \
  -d '{"key":"HOME"}' http://127.0.0.1:8080/api/v1/input/key
curl -H 'X-API-Key: <t>' -H 'Content-Type: application/json' \
  -d '{"keys":["HOME","BACK"]}' http://127.0.0.1:8080/api/v1/input/keys
```

- `gesture` 走真实的 `motionevent DOWN/MOVE.../UP` 序列（单条 `swipe` 表达不了拐弯的路径），手机支持的坐标是 1280×2772。
- **`input text` 只能注入 ASCII**：系统本身丢弃非 ASCII 字符。中文/emoji 走「剪贴板 + `KEYCODE_PASTE`」这条路（需要 Termux:API 桥），响应里的 `method` 字段会告诉你是哪条路。

UI 控件树与「按文字点」：

```sh
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/ui/dump'                 # 扁平化的控件列表（文本/边界/中心/可点）
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/ui/dump?all=1'           # 全部节点，不过滤
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/ui/find?text=登录&contains=1'
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/ui/current'              # 前台 app、焦点窗口、键盘、锁屏
curl -H 'X-API-Key: <t>' -H 'Content-Type: application/json' \
  -d '{"text":"登录"}' http://127.0.0.1:8080/api/v1/ui/click
```

一次 `uiautomator dump` 在这台设备上约 2 秒（它要起一个 Java 进程），所以结果有短时缓存（`?max_age_ms=` 可调）。**锁屏或熄屏时 dump 只会给出 keyguard 节点**，响应里的 `window.note` 会说明这一点，而不是假装「这个 app 没有文字」。

### 应用

```sh
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/apps?grep=termux&limit=20'
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/apps?package=com.termux'   # 附带解析出的启动 Activity
curl -H 'X-API-Key: <t>' http://127.0.0.1:8080/api/v1/apps/current
curl -H 'X-API-Key: <t>' http://127.0.0.1:8080/api/v1/apps/activity            # dumpsys activity 摘要
curl -H 'X-API-Key: <t>' -H 'Content-Type: application/json' \
  -d '{"package":"com.termux","wait_ms":800}' http://127.0.0.1:8080/api/v1/apps/launch
curl -H 'X-API-Key: <t>' -H 'Content-Type: application/json' \
  -d '{"package":"com.termux","force_stop_first":true}' http://127.0.0.1:8080/api/v1/apps/launch
curl -H 'X-API-Key: <t>' -H 'Content-Type: application/json' \
  -d '{"package":"com.termux"}' http://127.0.0.1:8080/api/v1/apps/stop
```

启动的响应会说真话：`foreground_before` / `foreground_after` / `changed_foreground`，如果 intent 被接受但前台是别的包（锁屏、权限弹窗抢焦点），会给出 `warning`。

包列表里**没有应用名**：读 APK 标签需要 `aapt`，这台设备没有。可用的标识是包名和 `path`。`pm list packages -3` 在 HyperOS 上包含小米预装（`/data/app`），所以本服务把它标成 `updated`，而不是「用户安装的」。

### 设备控制

```sh
POST /api/v1/control/screen      {"action":"on|off|toggle|wake|sleep"}
POST /api/v1/control/volume      {"action":"up|down|mute"}
POST /api/v1/control/brightness  {"value":120}     或 {"percent":50} 或 {"auto":true}
POST /api/v1/control/rotation    {"value":"0|1|2|3|auto"}
POST /api/v1/control/torch       {"on":true}
POST /api/v1/control/vibrate     {"ms":300}
POST /api/v1/control/toast       {"text":"hi"}
POST /api/v1/control/clipboard   {"action":"get"|"set","text":".."}
POST /api/v1/control/tts         {"text":"说点什么"}
POST /api/v1/control/ir          {"freq_hz":38000,"pattern":"9000,4500,560,560"}   或 {"frequencies":true}
POST /api/v1/control/media       {"action":"play|pause|stop|info","file":"/sdcard/x.mp3"}  或 {"action":"next|prev|playpause"}
POST /api/v1/control/wifi        {"on":true}
GET  /api/v1/control/sensors                    # 传感器列表；?sensor=<name>&n=5 取读数
GET  /api/v1/control/location                   # 最后已知位置
GET  /api/v1/control/audio                      # 音量流状态
```

白色的（靠平台命令实现，不需要桥）：屏幕开关、音量、亮度、旋转、媒体键。彩色的（靠 Termux:API 的 app 上下文实现）：手电、振动、Toast、剪贴板、TTS、红外、媒体播放、Wi-Fi 开关、传感器、定位——桥不可用时这些端点返回 503 并告诉你缺什么。

这台手机有 **IR 发射器**（`android.hardware.consumerir`），所以万能遥控类的自动化是可行的；`GET /api/v1/control/ir` 带 `frequencies:true` 可以读出设备支持的载波频率。

### 通知与消息

通知走 root 自己的 `dumpsys notification --noredact`，**不需要任何权限、不依赖任何 App**：

```sh
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/notifications?limit=20'
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/notifications?include_ongoing=1&grep=微信'
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/notifications?package=com.tencent.mm&since_ms=1792000000000'
curl -H 'X-API-Key: <t>' -H 'Content-Type: application/json' \
  -d '{"title":"服务器","text":"构建完成","id":"build"}' http://127.0.0.1:8080/api/v1/notifications/post
```

字段：`title` / `text` / `big_text` / `ticker_text` / `package` / `uid` / `channel` / `importance` / `when_ms` + `when`（本地时间）/ `ongoing` / `clearable` / `foreground_service` / `actions` / `progress`。解析器是防御式的：`dumpsys` 的文本格式不是稳定 API，读不懂的记录会带 `parse_incomplete: true`，`?raw=1` 直接把原始文本给你自己看。常驻通知默认被过滤（它们通常是前台服务噪声），`include_ongoing=1` 可以带上。

短信/通话/联系人走 content provider：

```sh
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/messages/list?source=sms&limit=20'
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/messages/list?source=calls&limit=20'
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/messages/list?source=contacts&limit=50'
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/messages/search?source=sms&q=验证码'
```

**这台手机没有插 SIM 卡**，所以短信与通话表就是空的——空结果会带 `empty_reason` 字段说明原因，不会让你猜是不是接口坏了。

### 相机与麦克风

相机和麦克风在 Android 上只能由**应用进程**使用，root 进程自己没有这两项权限。所以本服务把这两个能力委托给手机上已装的 Termux:API：守护进程以 Termux 的 uid 运行它的命令行客户端。

```sh
curl -H 'X-API-Key: <t>' http://127.0.0.1:8080/api/v1/bridge/status   # 桥是否可用、卡在哪一条策略
curl -X POST -H 'X-API-Key: <t>' http://127.0.0.1:8080/api/v1/bridge/fix   # 授权 + 唤醒锁 + 拉前台，一次修好
```

```sh
# 拍照：真实 JPEG（这台设备 4096x3072，约 5 MB），可选缩小/转码
curl -H 'X-API-Key: <t>' -o photo.jpg 'http://127.0.0.1:8080/api/v1/camera/photo'
curl -H 'X-API-Key: <t>' -o small.webp 'http://127.0.0.1:8080/api/v1/camera/photo?camera=1&format=webp&maxwidth=1024&quality=70'
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/camera/photo?meta=1'   # 只看元信息（尺寸/字节/耗时）
curl -H 'X-API-Key: <t>' http://127.0.0.1:8080/api/v1/camera/info             # 摄像头能力（支持的尺寸等）

# 低帧率预览（反复拍照再压成 webp，当作「看得到就行」的取景器）
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/camera/preview?fps=2&width=540&seconds=30'

# 录音：默认 10 秒；可指定编码器/码率/采样率/声道
curl -H 'X-API-Key: <t>' -o rec.m4a 'http://127.0.0.1:8080/api/v1/mic/record?seconds=10'
curl -H 'X-API-Key: <t>' -o hi.m4a  'http://127.0.0.1:8080/api/v1/mic/record?seconds=30&encoder=aac&bitrate=192&rate=48000&channels=2'
curl -H 'X-API-Key: <t>' -X POST -H 'Content-Type: application/json' -d '{}' http://127.0.0.1:8080/api/v1/mic/start
curl -H 'X-API-Key: <t>' http://127.0.0.1:8080/api/v1/mic/status
curl -H 'X-API-Key: <t>' -X POST http://127.0.0.1:8080/api/v1/mic/stop --output rec.m4a
```

两条设备策略必须满足，服务会自己摆好并如实报告：

| 能力 | 平台要求 | 服务怎么做到 |
|---|---|---|
| 麦克风 | 该 uid 至少是 `FOREGROUND_SERVICE` 状态 | `termux-wake-lock` |
| 相机 | 该 uid 处于前台（TOP） | `am start com.termux/.app.TermuxActivity`（可能点亮屏幕；`bridge.wake_screen_for_camera` 可控） |

不满足时的报错是可以直接看懂的，例如：

```
CAMERA_DISABLED (1): connectHelper: cannot open camera "0" from background (proc state 10)
```

首次使用建议先 `POST /api/v1/bridge/fix`：它会 `pm grant` 相机/麦克风/短信/通话/联系人/通知权限、设好 app-op、拿唤醒锁、把 Termux 拉到前台，并把结果原样报回来。没装 Termux:API 时这些端点返回 **503** + 修复提示，其余功能不受影响。

相机/麦克风的产物**不入库、不上传**：响应直接给字节，手机上的临时文件除非 `capture.keep_capture_files=true` 否则马上删掉。

### 文件服务与 WebDAV

两个前门，一套实现：

- `http(s)://<host>:<port>/files/` —— 浏览器友好：目录列表（HTML）、下载、上传（表单或 PUT）、新建目录、重命名、删除。
- `http(s)://<host>:<port>/dav/` —— WebDAV：可以挂载成电脑上的网络驱动器（Windows 映射网络驱动器 / macOS「连接服务器」/ rclone / davfs2 / 各类文件管理器）。

共享目录是白名单（默认 `sdcard` = `/storage/emulated/0`、`download` = `.../Download`、`tmp` = `/data/local/tmp`），可以自己增删：

```json
{ "files": { "shares": { "sdcard": "/storage/emulated/0", "tmp": "/data/local/tmp", "root": "/" } } }
```

用起来：

```sh
T='-H "X-API-Key: <t>"'
curl $T http://127.0.0.1:8080/files/                          # 共享列表（JSON；?listing=1 给 HTML）
curl $T 'http://127.0.0.1:8080/files/sdcard/Download/?json=1' # 目录内容
curl $T -o file.zip 'http://127.0.0.1:8080/files/sdcard/Download/file.zip'
curl $T -T ./local.bin 'http://127.0.0.1:8080/files/tmp/local.bin'          # PUT 上传
curl $T -T ./local.bin 'http://127.0.0.1:8080/files/tmp/a/b/c.bin?mkdir=1'  # 自动建父目录
curl $T -F 'file=@./local.bin' 'http://127.0.0.1:8080/files/tmp/'           # 表单上传（浏览器同样用法）
curl $T -X POST 'http://127.0.0.1:8080/files/tmp/newdir?op=mkcol'
curl $T -X POST 'http://127.0.0.1:8080/files/tmp/a.bin?op=move&dest=tmp/b.bin'
curl $T -X DELETE 'http://127.0.0.1:8080/files/tmp/old.bin'                 # 需要 allow_delete
curl $T -X GET -r 0-1023 'http://127.0.0.1:8080/files/sdcard/Download/file.zip'  # Range 续传
```

WebDAV：

```sh
curl -u any:<t> -X PROPFIND -H 'Depth: 1' http://127.0.0.1:8080/dav/sdcard/
curl -u any:<t> -X MKCOL http://127.0.0.1:8080/dav/tmp/newdir
curl -u any:<t> -T ./x.bin      http://127.0.0.1:8080/dav/tmp/x.bin
```

- Windows：资源管理器 →「映射网络驱动器」→ 地址填 `http://192.168.5.29:8080/dav/sdcard/`，用 token 作密码。
- Windows 需要 HTTP 基本认证走明文时才肯挂：`davclient` 默认拒绝 http。要么用 `net use` 命令，要么在注册表里放开 `BasicAuthLevel`（见下），要么只在手机本机用。
- macOS：Finder →「前往」→「连接服务器」→ `http://192.168.5.29:8080/dav/sdcard/`。
- rclone：`rclone config` → 选 `webdav` → url `http://192.168.5.29:8080/dav/sdcard/`，user 任意、pass = token。
- Linux/davfs2：`mount -t davfs http://192.168.5.29:8080/dav/sdcard/ /mnt/phone`。

工程上值得知道的几点：

- **写入是「先写临时文件再 rename」**，所以中断的传输不会留下半截文件覆盖掉原文件；替换已有文件时会沿用原文件的权限位。
- **`files.read_only` 关掉所有写**（both doors）；`files.allow_delete` 单独控制删除，上传仍可用。
- **`LOCK`/`UNLOCK` 会被应答但不强制**：Windows 与 macOS 在写入前要 LOCK，本服务返回一个合法形状的 lock token，但不维护锁表，因此**同一个目录被多个客户端同时写时不会有互斥**——这一点在响应体和日志里都写明了。
- 上传体积上限 `files.max_upload_mb`（默认 512），超了返回 413。
- 目录条目受 `files.max_list_entries` 限制（默认 2000），截断时响应会说明。

`/files/` 与 `/dav/` 的认证接受 `X-API-Key`、`?key=`、`Authorization: Bearer`，也接受 **HTTP Basic（用户名任意，密码=token）**——WebDAV 客户端通常只能给 Basic，浏览器也一样。

### 受控端点：shell 与电源

```sh
# 需要 security.allow_shell=true，否则 403 并说明开关名
curl -H 'X-API-Key: <t>' -H 'Content-Type: application/json' \
  -d '{"cmd":"ls -l /sdcard/Download","timeout_ms":10000}' http://127.0.0.1:8080/api/v1/shell

# 需要 security.allow_power_commands=true
curl -H 'X-API-Key: <t>' -H 'Content-Type: application/json' \
  -d '{"mode":"recovery"}' http://127.0.0.1:8080/api/v1/device/reboot
curl -H 'X-API-Key: <t>' -X POST http://127.0.0.1:8080/api/v1/device/shutdown
```

shell 输出有上限（`security.shell_max_output_bytes`，默认 256 KB）并会报告是否被截断；每次调用都写进日志（`shell: cmd=... code=...`）。默认关闭是有意的：这个进程是 root。

### 配置

```sh
curl -H 'X-API-Key: <t>' http://127.0.0.1:8080/api/v1/config           # 读（token 打码）
curl -H 'X-API-Key: <t>' -X PUT -H 'Content-Type: application/json' \
  -d '{"listen":"192.168.5.29:8080","security":{"allow_non_loopback_listen":true}}' \
  http://127.0.0.1:8080/api/v1/config
```

- 打码的 token（`abcd…wxyz`）不会写回文件，所以「读出来 → 改一个无关字段 → 写回去」不会把你的 token 弄丢。
- 把 `listen` 改成非回环地址而 `allow_non_loopback_listen` 仍是 `false` 会被 400 直接拒绝。
- `listen` / `log_dir` / `log_max_bytes` 改动需要重启进程（响应里 `restart_needed: true`；WebUI 与 `action.sh` 都有重启入口）。其余字段立即生效。
- 配置文件本体在手机上是 `/data/adb/phoneserver/config.json`（0600），可以直接编辑——但注意「先关服务再改」：进程启动时读一次，运行中改文件不会即时生效。

### 历史数据与数据质量

本地 SQLite（`/data/adb/phoneserver/history.db`）保存遥测时间序列。设计目标是**每个数字都能被信任、能被正确解读、长时间跨度查询不失真**。

采样是分级的：`history.interval_seconds`（默认 10 秒）写全部标量列；`history.full_seconds`（默认 60 秒）额外写一份完整文档（含全部温区、接口、进程列表）到快照表。廉价 tick 量不到的列**写 NULL 而不是 0**，读取时按需 carry forward（响应里 `carried_columns` 会说明）。

每一行都带可信度元数据：

| 列 | 含义 |
|---|---|
| `tick_gap_ms` | 与上一行的真实间隔（不是配置值），用于识别漏洞 |
| `mono_ms` | 单调时钟（elapsedRealtime），墙钟被改也能正确排序 |
| `tz_offset_min` | 采样时的时区偏移 |
| `measured_cols` / `absent_cols` / `rejected_cols` | 这一行真的测到 / 缺 / 被判废的列数 |
| `flags` | 位标记：廉价 tick、超出预期的间隔、检测到重启、值来自上一轮、墙钟跳变 |
| `source_version` | 写这一行的服务版本 |

取值会做合法性校验（温度、百分比、RSSI 等有范围；`net_rx_bytes`、`uptime`、充电计数等单调计数器不得倒退）。越界值**置 NULL 并计入 rejected**，而不是污染以后所有的统计；检测到重启（`uptime` 倒退）会打 `FLAG_BOOT_RESET` 并重置比较基线。同一秒的重复行会被去重（`ts` 上有唯一约束）。

```sh
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/history?metrics=battery_temp_c,cpu_usage_pct&since=-6h&limit=360'
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/history?metrics=battery_temp_c&since=-30d&bucket=1h'   # 分桶聚合，长跨度不失真
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/history/metrics'          # 可查询的指标清单
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/history/stats'            # 库大小/行数/增长速度/功耗估算
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/history/quality?since=-24h'   # 数据质量报告
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/history/gaps?since=-24h'      # 采样空洞
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/history/snapshot?ts=1792598000'
curl -H 'X-API-Key: <t>' 'http://127.0.0.1:8080/api/v1/history/logs?since=-1h&grep=history&limit=50'
```

- **不带 `bucket`** 时返回原始行（受 `limit` 约束）。
- **带 `bucket=10m|1h|1d`** 时在 SQL 内做分桶聚合，每桶每指标给出 `n / avg / min / max / last`，`limit` 作用于**桶数**而不是原始行数——所以「30 天」这种查询拿到的是一份诚实的聚合，不是被截断的开头一段。响应里会声明 `aggregation` 与参与聚合的 `source_rows`。
- 保留策略：原始样本 `history.retention_days`（默认 30 天）、快照 `history.snapshot_retention_days`（默认 3 天）、日志 `history.log_retention_days`（默认 7 天）、库总量上限 `history.max_db_mb`（默认 512）。删除原始样本**之前**会先把整小时 rolled up 到 `rollup_hourly` 表（`history.rollup_enabled`），所以原始数据过期后长期趋势仍然是真实测量值而不是猜测。
- `/api/v1/history/quality` 是读数据质量的地方：覆盖区间与覆盖率、每列 NULL 比例、被判废计数、flags 分布、最长的采样空洞、重复行数、库大小与每天增长估算。指标看着不对时先看它，再决定怪设备还是怪采样。

---

## MCP（给 AI agent 用）

两种传输，工具覆盖上面全部能力。

**stdio**（本地 agent 的标准接法）：

```sh
# 在电脑上跑（API 需要已开局域网访问）
phoneserver -mcp -config /path/to/config.json -mcp-url http://192.168.5.29:8080

# 或者直接在手机里跑（配合 adb / ssh）
adb shell '/data/adb/modules/phoneserver/bin/phoneserver -mcp -config /data/adb/phoneserver/config.json'
```

Claude Code / Codex CLI / 其它 MCP 客户端配置（`command` + `args` 形式）：

```json
{
  "mcpServers": {
    "phone": {
      "command": "phoneserver",
      "args": ["-mcp", "-config", "/path/to/config.json", "-mcp-url", "http://192.168.5.29:8080"]
    }
  }
}
```

**HTTP**（常驻服务自带，`mcp.http=true` 时可用）：

```sh
curl -s -X POST http://127.0.0.1:8080/mcp \
  -H 'X-API-Key: <t>' -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'
```

工具按能力分组（遥测 / 截图 / 画面信息 / UI 与输入 / 应用 / 设备控制 / 通知与消息 / 相机麦克风 / 文件与 WebDAV / 桥与配置 / 历史 / shell 与电源）。要点：

- 图片类工具（`phone_screenshot`、`phone_camera_photo`）返回 MCP 的 image 内容块；录音返回 audio 块。
- 串流不能塞进 `tools/call`（长连接），所以只提供「取串流信息」的工具，并在描述里给出 URL 与帧协议，让 agent 自己决定怎么消费。
- `mcp.read_only=true` 时只暴露只读工具，写工具在 `tools/list` 里不出现、被调用时也会拒绝并说明。
- 危险工具（`phone_shell` / `phone_reboot`）的描述里写明需要哪个配置开关，没开就是 403。

---

## WebUI

两个界面，同一套后端：

1. **管理器 WebUI**（KernelSU/SukiSU 里打开模块卡片，按「WebUI」）：多页面界面，通过 `window.ksu` 桥调用模块内的 `webui.sh`，因此**不需要局域网访问、不需要 token 输入**——它是本机 UI。包含概览、实时画面、操作控制、应用、消息与通知、相机与麦克风、文件与 WebDAV、历史数据、接口与 MCP、配置、日志。
2. **浏览器界面** `http://<host>:<port>/ui/`：同一份构建产物，由守护进程直接提供静态文件。此时没有 `window.ksu`，页面切到「直连 API」模式：填一次 API 基址与 token（存 localStorage），之后直接 `fetch /api/v1/*`。

实时画面页用浏览器的 **WebCodecs** 解码 H.264（Chromium 94+；这台设备的 WebView 是 143，浏览器更没问题），WebCodecs 不可用时自动降级到 mjpeg 端点。画面上点按/滑动会换算成真实坐标调用 `/api/v1/input/*`，所以这就是一个自制的 scrcpy。

配置页覆盖 `config.json` 的每一个字段，其中「允许局域网访问」开关旁边有醒目警告；改 `listen` 后会提示重启并提供一键重启。

WebUI 的源码在 `webui/`（Vite + Preact + TypeScript + Tailwind v4），构建产物提交在 `module/webroot/`；`./build.sh` 在有 `webui/node_modules` 时重新构建，否则直接用仓库里已构建的产物——**手机端永远不需要 node**。

### 改动 WebUI 时的约定（都是踩过的坑）

- 构建：`cd webui && pnpm install && pnpm build`，产物直接写进 `module/webroot/`。`vite.config.ts` 的 `base: './'` 不能改：同一份产物既要被管理器 WebView 从磁盘按相对路径加载，又要被守护进程在 `/ui/` 下提供。
- **断网也能构建**：`webui/node_modules` 可以从本机 pnpm 缓存符号链接出来（`preact`、`vite`、`@preact/preset-vite`、`tailwindcss`、`@tailwindcss/vite`、`@types/node` 都在缓存里），接完直接 `node node_modules/vite/bin/vite.js build`。TypeScript 检查用 `./node_modules/.bin/tsc --noEmit`。
- **两种路径写法**：`apiGet`/`apiPost` 收 API 相对路径（`/battery`），也接受完整写法（`/api/v1/battery`）——两者等价。但 **`/files/*` 与 `/mcp` 不在 `/api/v1` 之下**，必须用 `rawRequest(绝对路径, { method })`；用 `apiGet` 会被补成 `/api/v1/files/...` 并拿到 404（浏览器验证时踩到过）。
- 页面契约：每个文件 `default export` 一个无 props 组件；数据字段一律照抄真机响应（`/api/v1/all`、`/files/<share>/?json=1`、`/api/v1/history/quality`、`/api/v1/stream/status` …），不要凭记忆写字段名。
- 上传文件走 `fetch(apiURL(...), { method: 'POST', headers: { 'X-API-Key': token }, body: FormData })`；`apiPost` 只发 JSON。
- 验证方式：配置页临时打开局域网 → 浏览器打开 `http://<手机IP>:8080/ui/` → 填 token → 逐页看，控制台必须 0 error。**管理器 WebView 里的实际渲染只有用户能在手机上看**，别声称自己验过。

---

## 已知限制

每一条都是实测结论，不是推测：

- **截不到受保护窗口**：`FLAG_SECURE` 的界面（银行、部分聊天应用）截出来是黑帧。
- **锁屏时 UI 自动化基本失效**：`uiautomator dump` 只有 keyguard 节点；先解锁再自动化，`ui/current` 会告诉你当前是不是这个状态。
- **`input text` 只能 ASCII**：中文/emoji 必须走剪贴板 + `KEYCODE_PASTE`（需要 Termux:API 桥）。
- **实时画面要求屏幕处于有效显示状态**：熄屏时 `screenrecord` 报 `INVALID_LAYER_STACK`，需要先唤醒（服务默认会做）。
- **相机/麦克风依赖 Termux:API**：没装或权限没给就是 503；相机还要求该 uid 前台（服务会自动拉起 Termux 活动页，可能点亮屏幕）。
- **写 `/system`、`/system_ext`、`/product`、`/vendor` 不可能**：这些分区是只读 erofs 且不是独立挂载点，系统级修改只能通过模块挂载。
- **`/sdcard` 在锁屏状态下不可访问**（FUSE 视图解锁后才存在），涉及它的文件操作要等解锁。
- **没有 SIM 卡就没有短信/通话**：本机 `gsm.sim.state` 是空的，`source=sms|calls` 返回 0 行并说明原因。
- **包列表没有应用名**（缺 `aapt`），只有包名与路径。
- **WebDAV 锁不强制**（见上文），多客户端并发写同一路径要自己协调。
- **`POST /api/v1/shell` 与电源端点是 root 级能力**，默认关闭；打开前请确认这台手机只在你自己的网络里。

---

## 常见故障排查

| 症状 | 原因与处理 |
|---|---|
| 电脑连不上 | 默认只监听回环。打开 `security.allow_non_loopback_listen` 并把 `listen` 改成手机局域网 IP，然后重启服务 |
| 所有请求 401 | 缺 token。`grep api_token /data/adb/phoneserver/config.json`，或按 `action.sh` 按钮 |
| 相机/麦克风 503 | 桥不可用。`POST /api/v1/bridge/fix`，再 `GET /api/v1/bridge/status` 看 `missing_tools` 与 `permissions` |
| 相机 502 且提到 background | uid 不在前台。`bridge.foreground_for_camera` 打开，或先 `bridge/fix` |
| 麦克风只有几百字节 | uid 没到前台服务状态。`bridge.keep_awake_for_mic` 打开（默认），或先 `bridge/fix` |
| 画面流立刻断开 | 屏幕熄屏。带 `?wake=1`，或打开 `stream.wake_display` |
| 画面卡顿/流量大 | 降 `bitrate`（如 1500000）或 `size`（如 540x1170），或改用 mjpeg |
| 截图全黑 | 目标窗口是 `FLAG_SECURE`，平台行为 |
| `ui/dump` 没文本 | 锁屏或熄屏。先 `POST /api/v1/control/screen {"action":"on"}` 并解锁 |
| WebDAV 挂不上 | 用 Basic（用户名任意、密码 token）；Windows 需要放开 http 基本认证；确认 `/dav/<share>/` 结尾带斜杠 |
| 历史里有空洞 | 看 `/api/v1/history/gaps` 与 `/history/quality`；服务重启/关机期间本来就没有样本 |
| 改了配置不生效 | `listen`/`log_dir`/`log_max_bytes` 需要重启；直接改配置文件也要重启 |
| 端口被占 | `adb shell 'ss -lntp \| grep 8080'`，换个端口并重启 |

---

## 运维

```sh
DATA=/data/adb/phoneserver

# 状态、地址、token、桥与串流状态（KernelSU/Magisk 的 Action 按钮执行同一脚本）
adb shell "sh /data/adb/modules/phoneserver/action.sh"

# 日志（每请求一行；4 MB × 3 轮转）
adb shell "tail -n 50 $DATA/logs/phoneserver.log"
adb shell "tail -n 50 $DATA/logs/stdout.log"

# 重启（watchdog 会在 10 秒内把它拉回来）
adb shell 'pkill -f bin/phoneserver'

# 停止（连 watchdog 一起停）
adb shell "kill \$(cat $DATA/watchdog.pid); pkill -f bin/phoneserver"

# 备份配置与历史
adb pull $DATA/config.json
adb pull $DATA/history.db
```

- 进程由 `watchdog.sh` 守护：被杀后 10 秒内自动重启。
- 配置、日志、历史库都在 `/data/adb/phoneserver/`，**不在模块目录里**，所以升级/重装模块不会丢 token 和数据。
- 轮换 token：`PUT /api/v1/config` 写入新的 `api_token` 即可（或直接改 config.json 后重启）。
- 日志会被镜像进历史库（`/api/v1/history/logs` 可查历史日志），因此日志也能按时间线回溯。

---

## 开发者

```sh
./build.sh          # 交叉编译静态 arm64 → go test → 构建 WebUI → 打包 phoneserver-module.zip
```

- 守护进程是 `CGO_ENABLED=0` 的纯静态 Go 二进制（必须如此：Android 的 loader 是 `/system/bin/linker64`，任何带 `PT_INTERP` 的 PIE 都会以「No such file or directory」失败退出）。`build.sh` 会断言二进制 **0 个 INTERP 段**。
- 依赖只有三个，且都是纯 Go：`modernc.org/sqlite`（本地历史）、`golang.org/x/image`、`github.com/KarpelesLab/gowebp`（webp 编解码）。所以交叉编译不需要联网。
- 测试：`go test ./...`（`build.sh` 把测试当门禁，红了不会打包）。
- 真机端到端自检：`scripts/smoke.sh <base-url> <token> [--with-input] [--with-camera]`，逐条打印 PASS/FAIL（含「该被拒绝的端点是否真的拒绝了」）。
- 仓库：**https://github.com/Kailoinf/phoneserver**（源码为准，release 里是安装包）。WebUI 源码在 `webui/`，构建产物在 `module/webroot/`。
- 发布：`gh release create vX.Y.Z phoneserver-module.zip --title ... --notes ...`；版本号要同时改 `module/module.prop` 与 `main.go` 里的 `version` 常量（二进制会自报版本，`/api/v1/health` 可以核）。

许可证：MIT。