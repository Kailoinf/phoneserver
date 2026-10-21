# Phone Data Server · 手机数据服务

> Turn an Android phone's telemetry into a JSON REST API, and optionally POST it to a server you own.
> 把手机的运行数据变成 RESTful JSON 接口，并可按间隔主动推送到你自己的服务器。

KernelSU / Magisk 模块形态，主体是**一个静态链接的 Go 二进制**（linux/arm64）。手机上不需要 Python、Node 或任何运行时；宿主机也不需要联网就能交叉编译（只用标准库）。

- **REST 接口**：18 个数据段，全部只读 GET，另有 PUT 改配置、POST 触发推送
- **管理器 WebUI**：在 KernelSU / SukiSU 管理器里直接打开，实时仪表盘 + 服务控制
- **网页仪表盘**：浏览器打开 `http://<手机IP>:8080/` 即可，深色卡片、3 秒自动刷新
- **云端推送**：按间隔把同一份文档 POST 给你的服务器，失败重试，结果可查
- **令牌鉴权**：默认生成随机令牌，支持 `X-API-Key` / `Bearer` / `?key=`
- **看门狗**：服务崩了 10 秒内自动拉起，开机自启

---

## 安装

从 [Releases](../../releases) 下载 `phoneserver-module.zip`，在 KernelSU / Magisk 管理器里当作模块安装，然后**重启一次**（KernelSU 会先放进 `modules_update/`，重启才搬进 `modules/` 并生效）。

命令行方式：

```sh
adb push phoneserver-module.zip /data/local/tmp/
adb shell su -c 'ksud module install /data/local/tmp/phoneserver-module.zip'
adb reboot
```

配置与日志放在 `/data/adb/phoneserver/`（**刻意放在模块目录之外**，所以重装/升级模块不会丢令牌和推送设置）。

## 快速开始

```sh
# 取令牌（首次开机会自动生成）
adb shell su -c 'grep api_token /data/adb/phoneserver/config.json'
# 或者点管理器里模块卡片上的 Action 按钮，它会打印地址和令牌

curl http://192.168.1.50:8080/api/v1/health                       # 免鉴权
curl -H 'X-API-Key: <令牌>' http://192.168.1.50:8080/api/v1/all   # 全部数据
```

浏览器直接打开 `http://192.168.1.50:8080/` 就是实时仪表盘。

## API 端点

| 方法 | 路径 | 内容 |
|---|---|---|
| GET | `/api/v1/health` | 存活、服务时长、是否开了鉴权/推送（免鉴权） |
| GET | `/api/v1/endpoints` | 机器可读的端点索引 |
| GET | `/api/v1/all` | 全部 18 段合成一份文档 |
| GET | `/api/v1/device` | 品牌/机型/代号、ROM、版本、内核、基带、分区槽位、解锁状态 |
| GET | `/api/v1/props` | 完整 `getprop`（1000+ 键，缓存 10 分钟） |
| GET | `/api/v1/battery` | 电量、状态、健康度%、循环次数、mAh、温度、电流、电压、剩余时间 |
| GET | `/api/v1/power` | 所有 `/sys/class/power_supply` 节点 + usb/wireless + 屏幕唤醒状态 |
| GET | `/api/v1/display` | 屏幕开关、亮度与上限 |
| GET | `/api/v1/cpu` | 每核当前/最低/最高频率、调度器、总与单核占用、负载 |
| GET | `/api/v1/gpu` | Adreno 占用%、频率、温度、可用档位 |
| GET | `/api/v1/memory` | `/proc/meminfo` 全项 |
| GET | `/api/v1/storage` | 各挂载点总量/可用/占用%/文件系统/是否可写 |
| GET | `/api/v1/thermal` | 热区（摄氏度，含原始值），非温度项单独列出 |
| GET | `/api/v1/network` | 每张网卡状态/MAC/IP/累计计数 + **实时速率** |
| GET | `/api/v1/network/wifi` | SSID、BSSID、IP、RSSI（含百分比）、协商速率、频段/信道、协议、收发包 |
| GET | `/api/v1/system` | 运行时长、开机时刻、内核、SELinux、负载、进程数、时区 |
| GET | `/api/v1/processes` | 进程数 + RSS 前 15 |
| GET | `/api/v1/packages` | 安装包统计（总/系统/用户/禁用） |
| GET | `/api/v1/config` | 当前配置（密钥脱敏） |
| PUT | `/api/v1/config` | 改推送地址/间隔/令牌/TTL/端口 |
| GET | `/api/v1/push` | 推送状态：计数、上次结果、上次错误 |
| POST | `/api/v1/push` | 立刻推送一次，返回结果 |

读端点限定 GET，其他方法返回 405；除 `/api/v1/health` 外都需要令牌。

## 数据可信度：单位和坑（重要）

这个模块在真实设备上调试出来，下面几条是实测结论，不是猜测：

- **电源节点的单位不统一。** 内核 `power_supply` 约定是微伏/微安（`usb/voltage_max=4998000` 即 5V），但同一颗芯片上有些节点用毫伏（某些机型的 `wireless` 报 `3827` 即 3.827V）。所以每个值都同时给**原始整数**（`*_raw`）和换算值（`*_v` / `*_a`）。数字看着不对时以 `*_raw` 为准。
- **`/sys/class/thermal/` 里不是每个"热区"都是温度。** 充电 IC 会把**电流限制档位**和**电池电压**也通过这个接口暴露出来（如 `pm7550ba-ibat-lvl0` 的 `187` 是 187mA，`vbat` 的 `3737` 是 3.737V）。盲信目录名会得到"187°C"这种荒谬读数。本模块会做数值合理性判断（-40…150°C）并按名字模式排除 `ibat/vbat/volt/curr/adc/lvlN`，被排除的项放进 `non_temperature_zones` 并保留原始值，绝不会出现在 `max_temp_c` 里。
- **热区的单位也不统一。** 同一台机器上，多数区域报毫摄氏度（`36800` = 36.8°C），个别区域直接报整度（`58` = 58°C），还有 `-273000`（-273°C，即"传感器不可用"的哨兵值）。换算规则：绝对值 ≥ 1000 按毫度除 1000，否则原样（因为小于 1000 的值不可能是毫度温度）。
- **CPU 与网络的"实时值"是两次请求之间的差值**，所以带 `sample_interval_ms`；服务刚启动时的第一次响应没有参照，速率为 0。
- **`current_now` 的正负号由驱动决定**，不要用符号判断充放电——用 `status`；`power.usb.online` 才表示"是否真的接入了外部电源"。插着一个输出能力不足的 USB 口（比如电脑的 5V/0.5A）时，很可能 `online=1` 但 `status=Discharging`——因为手机功耗大于输入功率，这是真的在净放电。

## 云端推送

```sh
curl -X PUT -H 'X-API-Key: <令牌>' -H 'Content-Type: application/json' \
  -d '{"push":{"enabled":true,"url":"https://example.com/ingest","interval_seconds":60,
               "bearer_token":"可选共享密钥","retries":3}}' \
  http://<手机IP>:8080/api/v1/config
```

每个间隔 POST 一次：

```json
{
  "device_id": "25053RT47C-9f3c21ab",
  "seq": 42,
  "collected_at": "2026-10-21T12:20:08+08:00",
  "payload": { "device": {}, "battery": {}, "cpu": {}, "...": {} }
}
```

请求头：`Content-Type: application/json`、`X-Device-Id`、`X-Seq`、`User-Agent: phoneserver/<版本>`，配了 bearer 就带 `Authorization: Bearer <...>`。非 2xx 会按 `retries` 次以 1s/2s/4s 退避重试，结果永远能在 `GET /api/v1/push` 查到。**失败的推送不会排队**——需要完整历史就轮询拉取。

最小接收端（三行 Python）：

```python
import http.server, json
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        print(body["device_id"], body["seq"], body["payload"]["battery"]["capacity_percent"])
        self.send_response(200); self.end_headers(); self.wfile.write(b'{"ok":true}')
http.server.HTTPServer(("0.0.0.0", 9099), H).serve_forever()
```

## 管理器 WebUI

在 KernelSU / SukiSU 管理器里打开模块卡片 → **WebUI**：实时指标 + 服务控制（重启/停止/启动）、切换监听端口、编辑推送设置、立即推送、查看/复制令牌、看日志。

页面本身只是渲染层：每个按钮都通过 KernelSU 的 `window.ksu.exec(cmd, '{}', callbackName)` 桥执行模块里的 `webui.sh` 子命令。所以同一套后端也能在命令行用：

```sh
sh /data/adb/modules/phoneserver/webui.sh snapshot          # {status:{...},data:{...}}，界面刷新只调这一次
sh .../webui.sh status | data battery | logs 60 | token
sh .../webui.sh restart | start | stop
sh .../webui.sh set-push <url> <秒> <bearer> <0|1>
sh .../webui.sh set-port 9090
```

`restart` / `start` / `set-port` **不会假报成功**：它们会轮询 `/api/v1/health` 直到服务真的在新状态上应答，否则返回 `ok:false` 和原因。

> 注意：`webroot/` 的权限和 SELinux 上下文由 KernelSU 在安装时设置，**不要自己 chmod**。

## 构建

```sh
./build.sh          # 产出 phoneserver-arm64 与 phoneserver-module.zip
```

需要 Go。因为只用标准库，交叉编译不需要联网。

**绝对不要用 `-buildmode=pie`。** Go 在 linux/arm64 上的 PIE 产物带 `PT_INTERP=/lib/ld-linux-aarch64.so.1`，而 Android 的加载器是 `/system/bin/linker64`，该路径不存在 → 内核拒绝 exec，报的是很有迷惑性的 `No such file or directory`（其实是解释器缺失）并返回 126。必须是静态 `ET_EXEC`。`build.sh` 里有这个断言（`readelf -l … | grep -c INTERP` 必须为 0）。

另外：Go 里要 `import _ "time/tzdata"`，否则 Android 上没有 zoneinfo 目录，`time.LoadLocation` 全部失败、所有时间戳会静默变成 UTC。

## 目录结构

```
.
├── build.sh              # 交叉编译 + 打包成可安装模块
├── main.go               # HTTP 服务、路由、鉴权、配置
├── collect.go            # 所有采集器（sysfs / proc / getprop / dumpsys）
├── push.go               # 云端推送与重试
├── dashboard.go          # 浏览器仪表盘（内嵌 HTML）
└── module/               # 模块骨架
    ├── module.prop       # 模块元数据（管理器里显示的名称/版本/作者）
    ├── customize.sh      # 安装时执行
    ├── service.sh        # 开机入口：等系统起完 → 拉起看门狗
    ├── watchdog.sh       # 常驻重启循环
    ├── action.sh         # 管理器 Action 按钮显示的内容
    ├── uninstall.sh      # 卸载时停服务
    ├── webui.sh          # WebUI 后端（也能在命令行直接用）
    ├── webroot/index.html# 管理器里的界面
    └── META-INF/…        # Magisk 安装引导文件
```

## 配置项（`/data/adb/phoneserver/config.json`）

| 键 | 默认 | 说明 |
|---|---|---|
| `listen` | `0.0.0.0:8080` | 监听地址，改后需重启服务 |
| `api_token` | 随机生成 | 留空则**不鉴权**（任何能路由到该端口的人都能读） |
| `device_id` | 由机型+序列号推导 | 推送时带上的设备标识，可自定义 |
| `log_requests` | `true` | 每个 HTTP 请求记一行日志 |
| `*_ttl_seconds` | 见文件 | 昂贵数据源的缓存时长（getprop/wifi/dumpsys/包列表/进程） |
| `storage_mounts` | `/data`,`/`,`/system`,`/storage/emulated/0` | 要统计的挂载点 |
| `push.*` | 关闭 | 接收地址、间隔、Bearer、超时、重试次数、额外请求头 |

## 安全说明

- 服务**以 root 运行**并监听所有网络接口。务必保留 `api_token`；清空它等于把设备状态公开给能访问该端口的人。
- **没有"任意文件读取"接口**，这是刻意的：一个以 root 运行、又开在网络端口上的服务如果带这个功能，等于远程文件泄露。只暴露命名好的、经过筛选的资源。再比如端口映射/公网暴露，请自己在服务器侧终止 TLS，推送地址用 `https://`。
- `GET /api/v1/config` 会脱敏令牌；`PUT` 拒绝把脱敏占位符写回真令牌，所以"读出来改一改再写回去"不会毁掉凭证。

## 已验证环境

在 REDMI Turbo 4 Pro / POCO F7（`onyx`，SM8735）+ HyperOS 3（Android 16）+ SukiSU Ultra v4.2.1 上实测：

开机自启与看门狗自愈（kill 掉 10 秒内自动拉起）、17 个读端点全部 200、管理器 WebUI 正常渲染、网页仪表盘正常渲染、带 Bearer 的推送到达真实接收端并重试正确、端口切换后自愈、`GET /config → PUT 改动 → 回读` 不丢令牌、SELinux 强制模式下 root 读取正常。

## 许可

MIT © 2026 Kairo