Phone Control Server 2.0.0 — 手机上的自托管控制器
====================================================

一个 root 守护进程，把本机能力开放成 HTTP / WebDAV / MCP 接口：
遥测、截图与实时画面、输入与 UI 自动化、应用启停、设备控制、
通知与消息、相机与麦克风、文件服务与 WebDAV、本地历史（含数据质量）、MCP。

不向任何远端服务器上传数据，也没有云端依赖。

默认只监听本机
--------------
    listen = 127.0.0.1:8080        （默认，局域网访问不到）

要从电脑访问，必须在配置里同时做两件事：
    1) security.allow_non_loopback_listen = true
    2) listen = <手机局域网IP>:8080        # 用 ip -4 addr show wlan0 查

打开了就意味着同网络能路由到该端口的人都能访问，token 是唯一凭据。
配置可以在 WebUI 的「配置」页改（带警告开关），也可以直接改
/data/adb/phoneserver/config.json 之后重启服务。

配置 / 日志 / 历史库都在这里（升级重装模块不会丢）
-------------------------------------------------
    /data/adb/phoneserver/config.json      配置（含 token，0600）
    /data/adb/phoneserver/history.db       历史库（SQLite）
    /data/adb/phoneserver/logs/            phoneserver.log（每请求一行）+ stdout.log

常用命令
--------
    # 状态 + 地址 + token + 桥/串流状态（KernelSU/Magisk 的 Action 按钮跑的就是它）
    sh /data/adb/modules/phoneserver/action.sh

    # 单独取 token
    grep -o '"api_token": "[^"]*"' /data/adb/phoneserver/config.json

    # 健康检查（不需要 token）
    curl -s http://127.0.0.1:8080/api/v1/health

    # 重启（watchdog 会在 10 秒内拉回来）
    pkill -f bin/phoneserver

    # 日志
    tail -n 50 /data/adb/phoneserver/logs/phoneserver.log

端点速查（T = token，用 -H "X-API-Key: $T" 或 ?key=$T）
-------------------------------------------------------
遥测      GET /api/v1/{all,device,props,battery,power,display,cpu,gpu,memory,
                        storage,thermal,network,network/wifi,system,processes,packages}

屏幕      GET /api/v1/screenshot                    截图（webp/jpeg/png，?meta=1 给元信息）
          GET /api/v1/stream/screen                实时画面（H.264 Annex-B；&raw=1 直传给 mpv）
          GET /api/v1/stream/screen/mjpeg         实时画面降级（webp 帧）
          GET /api/v1/stream/status                活跃编码器、实测帧率/码率
          GET /api/v1/stream/display               现在能不能开流（含真实探测）

输入      POST /api/v1/input/{tap,swipe,gesture,text,key,keys}
          GET  /api/v1/ui/dump                     控件树（文本/边界/中心）
          GET  /api/v1/ui/current                  前台 app / 焦点 / 锁屏状态
          GET  /api/v1/ui/find?text=…&contains=1   按文字找控件
          POST /api/v1/ui/click                    {"text":"登录"} 按文字点

应用      GET  /api/v1/apps?grep=&limit=            包列表（无应用名，只有包名/路径）
          GET  /api/v1/apps/current                前台包
          POST /api/v1/apps/launch|stop            {"package":"com.x"}

控制      POST /api/v1/control/{screen,volume,brightness,rotation,torch,vibrate,
                              toast,clipboard,tts,ir,media,wifi}
          GET  /api/v1/control/{sensors,location,audio}

消息      GET  /api/v1/notifications                通知（root 读 dumpsys，无需权限）
          POST /api/v1/notifications/post         {"title":"…","text":"…"}
          GET  /api/v1/messages/list?source=sms|calls|contacts
          GET  /api/v1/messages/search?source=sms&q=…
          （本机没插 SIM 卡 → 短信/通话为空是正常的，响应里有 empty_reason）

相机麦克风 GET /api/v1/camera/{info,photo,preview}   拍照 / 低帧率预览（需 Termux:API 桥）
          GET /api/v1/mic/record?seconds=10        录音（m4a/aac）
          POST /api/v1/mic/start|stop  ·  GET /api/v1/mic/status
          GET /api/v1/bridge/status                桥是否可用、卡在哪条策略
          POST /api/v1/bridge/fix                  一键修：授权 + 唤醒锁 + 拉前台

文件      GET  /files/                               共享目录列表（?listing=1 给网页）
          GET  /files/<share>/<path>                下载（支持 Range）/ 目录（?json=1）
          PUT  /files/<share>/<path>                上传（?mkdir=1 自动建目录）
          POST /files/<share>/<path>                表单上传；?op=mkcol|move|delete
          DELETE /files/<share>/<path>              删除（需 files.allow_delete）
WebDAV    /dav/<share>/                             挂载为网络驱动器：Basic 认证
                                                    （用户名随意，密码 = token）

历史      GET /api/v1/history?metrics=…&since=-6h&limit=360       原始样本
          GET /api/v1/history?metrics=…&since=-30d&bucket=1h      分桶聚合（长跨度不失真）
          GET /api/v1/history/{metrics,stats,logs,snapshot}
          GET /api/v1/history/{quality,gaps}                       数据质量 / 采样空洞
          POST /api/v1/history/prune                               立即执行保留策略

配置      GET /api/v1/config                        读（token 打码）
          PUT /api/v1/config                        改任意字段（listen 改动要重启）

MCP       POST /mcp                                 JSON-RPC（MCP over HTTP，需 token）
          bin/phoneserver -mcp -config <config>     stdio 传输（给本地 AI agent）

受控（默认关闭）
----------------
    POST /api/v1/shell                 需 security.allow_shell=true（root 命令）
    POST /api/v1/device/reboot|shutdown 需 security.allow_power_commands=true

WebDAV 挂载示例
---------------
    Windows   net use Z: http://<手机IP>:8080/dav/sdcard/ /user:any <token>
              或 资源管理器 → 映射网络驱动器（Windows 默认拒绝 http 基本认证，
              需要放开注册表 BasicAuthLevel 才能挂明文 http）
    macOS     Finder → 前往 → 连接服务器 → http://<手机IP>:8080/dav/sdcard/
    Linux     mount -t davfs http://<手机IP>:8080/dav/sdcard/ /mnt/phone
    rclone    rclone config → webdav → url http://<手机IP>:8080/dav/sdcard/
    curl      curl -u any:<token> -X PROPFIND -H 'Depth: 1' http://<手机IP>:8080/dav/sdcard/

    注意：LOCK/UNLOCK 会被应答但**不强制**，多客户端同时写同一路径没有互斥。

MCP（给 AI agent）
------------------
    # 电脑上跑（需要已开局域网访问）
    phoneserver -mcp -config <config.json> -mcp-url http://<手机IP>:8080

    # 手机本机跑
    /data/adb/modules/phoneserver/bin/phoneserver -mcp -config /data/adb/phoneserver/config.json

    客户端配置（Claude Code / Codex CLI 等）就是 command + args：
    {"mcpServers":{"phone":{"command":"phoneserver",
      "args":["-mcp","-config","/path/to/config.json","-mcp-url","http://<手机IP>:8080"]}}}

    mcp.read_only=true 时只暴露只读工具。

界面
----
    管理器 WebUI  KernelSU 里打开模块卡片 → 按 WebUI（多页面：概览/实时画面/控制/应用/
                  消息/相机麦克风/文件与 WebDAV/历史/接口与 MCP/配置/日志）
    浏览器        http://127.0.0.1:8080/       轻量仪表盘（遥测 + 历史 + 截图）
                  http://127.0.0.1:8080/ui/    完整多页面界面（含 WebCodecs 实时画面）
                  http://127.0.0.1:8080/files/  文件浏览与上传
    token 在页面里填一次（存 localStorage），或浏览器弹 Basic 认证时把 token 当密码。

WebUI 后端（webui.sh 子命令）
----------------------------
管理器里 WebUI 页面不直接读文件，而是通过 `sh <模块目录>/webui.sh <子命令>` 走 REST API。
所有子命令都能在 adb 上单独验证：

    status                给页面用的 JSON：version/running/pid/watchdog_pid/listen/port/lan_ip/token
    health                服务存活与能力概览
    get-config            当前配置（token 已脱敏）
    set-config [json]     PUT 配置；不给参数就从 stdin 读 JSON（推荐，避免 token 出现在命令行里）
    set-listen <host:port>  改监听地址；非 loopback 必须加 --yes 二次确认，改完自动重启并轮询 health
    set-port <port>       只改端口（保持 127.0.0.1）
    restart | start | stop  服务控制（restart 会轮询 /api/v1/health 确认真的起来了）
    bridge-status | bridge-fix    Termux:API 桥状态 / 一键修复权限+唤醒锁+前台
    stream-status        当前串流编码器的 fps/码率与屏幕状态
    api <GET 路径>        任意 GET，例：api /api/v1/battery
    api-post <路径> <base64 JSON>
    ui-click <base64 文本> / input-text <base64 文本>
    apps-list [匹配] | notifications [n] [--ongoing] | messages <sms|calls|contacts> [n]
    camera-photo [--meta] | mic-record <秒数>
    files-shares | files-list <share> <路径>
    mcp-info              MCP 端点与 stdio 启动命令
    history ... | history-stats | history-metrics | history-logs | history-snapshot | history-prune | history-panel
    screenshot [--meta] | logs [n] | api-log [n] | clear-logs | token

    写操作一律回读确认；连不上时区分「超时」与「没在监听」，不会假报 ok。

故障速修
--------
相机/麦克风 503 或报 background:
    POST /api/v1/bridge/fix          然后 GET /api/v1/bridge/status 看 permissions/missing_tools
    （麦克风要 uid ≥ 前台服务 → termux-wake-lock；相机要 uid 在前台 → 会自动拉起 Termux 活动页）

实时画面立刻断开:
    curl -X POST -H "X-API-Key: $T" -H 'Content-Type: application/json' \
      -d '{"action":"on"}' http://127.0.0.1:8080/api/v1/control/screen
    或开流时带 ?wake=1（默认 stream.wake_display=true 也会做）

ui/dump 没有文本 / 截图全黑:
    锁屏或熄屏 → 先唤醒（并解锁）；FLAG_SECURE 的窗口截图必然是黑的（平台保护）

电脑连不上:
    listen 还是 127.0.0.1，或者 allow_non_loopback_listen 没打开

改了配置没生效:
    listen / log_dir / log_max_bytes 需要重启：pkill -f bin/phoneserver

版本与升级
----------
    version 2.0.0   ·   versionCode 20000
    升级：管理器安装新的 phoneserver-module.zip，然后重启手机（模块先落 modules_update）
    卸载：配置、日志、历史库保留在 /data/adb/phoneserver/，要彻底清理请手动删除