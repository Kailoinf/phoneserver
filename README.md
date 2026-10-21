# Phone Data Server · 手机数据服务

> Turn an Android phone's telemetry into a JSON REST API, and optionally POST it to a server you own.
> 把手机的运行数据变成 RESTful JSON 接口，并可按间隔主动推送到你自己的服务器。

KernelSU / Magisk 模块形态，主体是**一个静态链接的 Go 二进制**（linux/arm64）。手机上不需要 Python、Node 或任何运行时；宿主机也不需要联网就能交叉编译（只用标准库）。

- **REST 接口**：18 个数据段，全部只读 GET，另有 PUT 改配置、POST 触发推送
- **管理器 WebUI**：在 KernelSU / SukiSU 管理器里直接打开，实时仪表盘 + 服务控制
- **网页仪表盘**：浏览器打开 `http://<手机IP>:8080/` 即可，深色卡片、3 秒自动刷新
- **云端推送**：按间隔把同一份文档 POST 给你的服务器，失败重试，结果可查
- **本地历史**：默认每 10 秒把数据写进手机本地的 SQLite 库（标量指标 + 完整文档 + 日志），不依赖云端也能回看曲线，自动按保留策略清理
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
| GET | `/api/v1/history` | 查询历史样本（`metrics`/`since`/`until`/`limit`/`stats`） |
| GET | `/api/v1/history/metrics` | 可查询的指标列清单 |
| GET | `/api/v1/history/logs` | 查询库内日志（`grep`/`level`/`since`） |
| GET | `/api/v1/history/snapshot` | 取回某个时刻的完整文档（可只取一段） |
| GET | `/api/v1/history/stats` | 库大小、行数、增长速度、耗电估算 |
| GET | `/api/v1/screenshot` | 当前屏幕画面，默认 WebP（**只在请求时产生、不入库、不推送**） |
| POST | `/api/v1/history/prune` | 立刻执行保留策略 |

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

## 本地历史（SQLite）

服务默认把数据写进手机本地的 SQLite 库，**不需要云端也能回看历史**。

    table        内容                                           默认保留
    samples      每个周期的标量指标（电量/温度/CPU/内存/网络速率…共 65 列）  30 天
    snapshots    完整的 /api/v1/all 文档，gzip 压缩后存 BLOB            3 天
    logs         日志行（实时镜像；首次启动还会回填已有的日志文件）          7 天

库文件是 `/data/adb/phoneserver/history.db`，与配置、日志同目录——**刻意放在模块目录之外**，升级模块不会丢历史。

驱动用 `modernc.org/sqlite`，即**纯 Go 实现的 SQLite**，不是常见的 mattn/go-sqlite3。这是被构建方式逼出来的选择：二进制必须无 CGO、静态链接才能在 Android 上 exec（见下文构建一节），而 mattn 需要 CGO + NDK。真机已验证：WAL 在 f2fs 上可用、跨进程持久、文件头是标准的 `SQLite format 3`，可直接用任何 SQLite 客户端打开（`sqlite3 history.db`，或 Python 的 `sqlite3` 模块）。

    # 最近 6 小时的电池温度与 CPU 占用，带 min/avg/max
    curl -H 'X-API-Key: <token>' 'http://<手机IP>:8080/api/v1/history?metrics=battery_temp_c,cpu_usage_pct&since=-6h&stats=1'
    # since/until 可以是 -30m / -6h / -7d 这类相对写法，也可以是 unix 秒或 RFC3339
    curl -H 'X-API-Key: <token>' 'http://<手机IP>:8080/api/v1/history/logs?since=-24h&grep=error&limit=50'
    curl -H 'X-API-Key: <token>' 'http://<手机IP>:8080/api/v1/history/snapshot?section=battery'
    curl -X POST -H 'X-API-Key: <token>' 'http://<手机IP>:8080/api/v1/history/prune'

`points` 是 `[ts, 指标1, 指标2, ...]`，时间戳在前、由旧到新，便于直接画图；`columns` 给出列名对应关系。

`/api/v1/history/stats` 里的 `timing` 把**一个采样周期拆成三段**（最近一次 + 均值）：间隔、采集、写库。实机稳态是「间隔 9.5 秒 · 采集 278ms · 写库 2ms」。

> 注意：**每次重启后的前几个周期会明显变慢（20–30 秒）**。原因是 TTL 缓存是冷的，第一份快照要现跑 `getprop`、`pm list packages`、`dumpsys`。等它预热完就回到配置的间隔。所以别在刚重启后立刻采样就断定"节奏不对"——先看 `timing.avg_gap_ms`。

### 耗电与磁盘：实测，不是估算

结论先说：**服务挂着本身不耗电，耗电只跟"采集了多少次"成正比。**

    闲置 30 秒                          进程 CPU 增量 0 ticks（完全睡着）
    一次完整采集（/api/v1/all）           约 146 ms 进程 CPU 时间（含 getprop / cmd wifi / dumpsys 子进程）
    每次采集的墙钟耗时                    约 250–300 ms
    按默认每 10 秒一次                    8640 次/天 ≈ 1261 秒 CPU/天 ≈ 单核 1.46%
    对照：WebUI 打开时（3 秒轮询）         单核约 4.9%

    磁盘（按默认保留策略，1.3.0 起已优化）
    快照 table    约 22.6 KB 原始 → gzip9 约 4.5 KB，×8640/天 ≈ 36.8 MB/天 × 3 天
    样本 table    约 3.4 MB/天 × 30 天
    日志 table    随访问量而定（每行 62 B 左右 + 哈希索引）
    WAL           上限 1 MB（wal_autocheckpoint=256）
    硬上限        512 MB（超过就按"最老的快照优先"逐出，永远不会把手机塞满）

### 存储占用：做过什么、量到什么

先看清楚钱花在哪（真机库 dbstat 按表/索引统计）：

    快照 table              68.8%   ← 大头
    日志 table + 唯一索引    23.2%   ← 索引几乎和正文一样大
    样本 table               5.0%

据此做了四件事，每项都在真机上量过：

| 改动 | 效果 | 代价 |
|---|---|---|
| 快照 gzip 等级 1 → 9（BestSpeed → BestCompression） | 平均 **5,244 B → 4,465 B（−15.1%）**，43.2 → **36.8 MB/天** | 压缩耗时 0.24 → 0.61 ms/次（10 秒一次，可忽略） |
| 日志去重索引 (ts_ms, line) → (ts_ms, line_hash) | 索引每行 **90.7 B → 27.3 B（−70%）** | 无（语义不变，插入时算一个 64 位 FNV） |
| wal_autocheckpoint=256 | WAL 常驻从 ~2.5 MB 降到上限 1 MB | 无明显代价 |
| auto_vacuum=INCREMENTAL + prune 后 incremental_vacuum | 删除后**页面真的还给系统**，库大小不再停在历史最高水位 | 切换时一次 VACUUM（库小，毫秒级） |

关于"按字段去重"：量过，**收益很小**。快照里完全不变的段（device/wifi/gpu/display/packages）占原始文本的 9.8%，但 gzip 之后只占 17.7% 的压缩体积——**gzip 已经在做"不变的部分几乎不花空间"这件事**。所以继续抠字段意义不大。

**真正的杠杆是频次**，见下一节。

### 想更省电/更省空间：降低"完整文档"的频率

配置里有两个旋钮：`interval_seconds`（采样周期，默认 10 秒）和 `full_seconds`（采集**完整文档**的周期，默认也是 10 秒，也就是严格按"全部数据每 10 秒一次"）。

把 `full_seconds` 调成 60：中间的 tick 只走**廉价路径**——只读 sysfs 和 /proc，不 spawn getprop/dumpsys/wifi 子进程；标量指标仍然每 10 秒一条。两个效果都量过：

    电：每天 CPU 1261 秒 → 约 270 秒（单核 1.46% → 0.31%）
    盘：快照 36.8 MB/天 → 约 6.1 MB/天（样本那 3.4 MB/天不变）

权衡说清楚：10 秒与 60 秒之间，**那 22.6 KB 的完整文档（热区全表、网络全表、进程列表…）只有 1 分钟一个采样点**；而 65 个标量指标（电量/温度/CPU/内存/速率…）仍然 10 秒一条。库里每行都有 `full` 列标明是完整采集还是廉价采集，绝不含糊。

    { "history": { "interval_seconds": 10, "full_seconds": 60 } }

## 截图

```bash
curl -H "X-Api-Key: $TOKEN" -o shot.webp http://192.168.5.29:8080/api/v1/screenshot
curl -H "X-Api-Key: $TOKEN" -o small.webp 'http://192.168.5.29:8080/api/v1/screenshot?quality=60&scale=0.5'
curl -H "X-Api-Key: $TOKEN" 'http://192.168.5.29:8080/api/v1/screenshot?meta=1'   # 只看元数据
```

| 参数 | 说明 |
|---|---|
| `format` | `webp`（默认）/ `jpeg` / `png` |
| `quality` | 1–100，默认 80（`png` 忽略） |
| `scale` | 0.1–1，默认 1（原始尺寸） |
| `display` | 指定 display id（默认主屏） |
| `meta=1` | 返回 JSON 元数据，不返回图片 |
| `base64=1` | 返回 JSON 并把图片内联为 base64（管理器 WebUI 用这个） |
| `download=1` | 让浏览器直接另存 |

**只在请求时产生画面。** 没有后台截图，不写入历史数据库，也不参与云端推送——推送载荷是遥测文档，里面没有任何图片字段（`push.go` 里 grep 不到 screenshot/image/base64）。响应也会明说：响应头 `X-Screenshot-Stored: false` / `X-Screenshot-Pushed: false`，JSON 里是 `"stored": false` / `"pushed": false`。

实机实测（1280×2772，锁屏+壁纸这种有内容的画面）：

| 请求 | 输出 | 相对原始 PNG | 采集 | 编码 |
|---|---|---|---|---|
| `quality=80`（默认） | **166 KB** | 3.5% | ~350 ms | ~340 ms |
| `quality=70` | 93 KB | 1.9% | ~350 ms | ~350 ms |
| `quality=60` | 67 KB | 1.4% | ~350 ms | ~300 ms |
| `quality=60&scale=0.5` | 12 KB | 0.4%（640×1386） | ~350 ms | ~90 ms |
| `format=jpeg&quality=85` | 438 KB | 9.1% | ~350 ms | ~160 ms |
| `format=png` | 4.8 MB | 100%（原样透传） | ~370 ms | 0 |

- 编码器是**纯 Go 的 WebP**（`github.com/KarpelesLab/gowebp`，无 CGO——这个二进制必须静态无 CGO 才能在 Android 上 exec）。无损 WebP 对截图没有意义（只比 PNG 略小、还慢 5 倍），所以默认是有损 VP8。同质量档下它比成熟的 libwebp 大约 40%，但相对 PNG 已经是 3.5%；比 JPEG 小 2.6 倍。
- **刚从熄屏/息屏显示唤醒时**第一次采集可能要 2 秒多（显示要上电），之后约 350 ms。服务端 20 秒超时；同一时刻只允许一个采集，第二个请求得到 **429** 和 `Retry-After`。
- **受保护的画面会全黑**：标了 `FLAG_SECURE` 的窗口（银行、部分视频、密码输入界面）由 SurfaceFlinger 直接返回黑帧——那是系统在保护内容，本模块不绕过。
- 每次请求都会进访问日志，和其它端点一样。

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

需要 Go。除纯 Go 的 SQLite 驱动（`modernc.org/sqlite`）外没有其他依赖；那一个依赖需要联网拉取一次（国内建议 `export GOPROXY=https://goproxy.cn,direct`），之后走本地模块缓存，编译本身仍是离线交叉编译。

**绝对不要用 `-buildmode=pie`。** Go 在 linux/arm64 上的 PIE 产物带 `PT_INTERP=/lib/ld-linux-aarch64.so.1`，而 Android 的加载器是 `/system/bin/linker64`，该路径不存在 → 内核拒绝 exec，报的是很有迷惑性的 `No such file or directory`（其实是解释器缺失）并返回 126。必须是静态 `ET_EXEC`。`build.sh` 里有这个断言（`readelf -l … | grep -c INTERP` 必须为 0）。

另外：Go 里要 `import _ "time/tzdata"`，否则 Android 上没有 zoneinfo 目录，`time.LoadLocation` 全部失败、所有时间戳会静默变成 UTC。

## 目录结构

```
.
├── build.sh              # 交叉编译 + 打包成可安装模块
├── main.go               # HTTP 服务、路由、鉴权、配置
├── collect.go            # 所有采集器（sysfs / proc / getprop / dumpsys）
├── store.go              # SQLite 层：建表、写入、查询、保留策略、日志镜像
├── history.go            # 采样循环（完整 tick / 廉价 tick）、成本估算、日志回填
├── historyapi.go         # /api/v1/history/* 的处理函数与时间参数解析
├── push.go               # 云端推送与重试
└── dashboard.go          # 浏览器仪表盘（内嵌 HTML，含历史曲线面板）
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
| `history.enabled` | `true` | 是否启用本地 SQLite 历史 |
| `history.db_path` | `/data/adb/phoneserver/history.db` | 库文件位置（改后需重启） |
| `history.interval_seconds` | `10` | 采样周期 |
| `history.full_seconds` | `10` | 采集完整文档的周期；大于采样周期时中间的 tick 走廉价路径 |
| `history.retention_days` | `30` | 样本保留天数 |
| `history.snapshot_retention_days` | `3` | 快照保留天数 |
| `history.log_retention_days` | `7` | 日志保留天数 |
| `history.max_db_mb` | `512` | 硬上限，超过就逐出最老的快照 |
| `history.backfill_logs` | `true` | 首次启动是否把已有日志文件回填进库 |

## 安全说明

- 服务**以 root 运行**并监听所有网络接口。务必保留 `api_token`；清空它等于把设备状态公开给能访问该端口的人。
- **没有"任意文件读取"接口**，这是刻意的：一个以 root 运行、又开在网络端口上的服务如果带这个功能，等于远程文件泄露。只暴露命名好的、经过筛选的资源。再比如端口映射/公网暴露，请自己在服务器侧终止 TLS，推送地址用 `https://`。
- `GET /api/v1/config` 会脱敏令牌；`PUT` 拒绝把脱敏占位符写回真令牌，所以"读出来改一改再写回去"不会毁掉凭证。

## 已验证环境

在 REDMI Turbo 4 Pro / POCO F7（`onyx`，SM8735）+ HyperOS 3（Android 16）+ SukiSU Ultra v4.2.1 上实测：

开机自启与看门狗自愈（kill 掉 10 秒内自动拉起）、17 个读端点全部 200、管理器 WebUI 正常渲染、网页仪表盘正常渲染、带 Bearer 的推送到达真实接收端并重试正确、端口切换后自愈、`GET /config → PUT 改动 → 回读` 不丢令牌、SELinux 强制模式下 root 读取正常。

本地历史另外单独验证过：SQLite 在设备上创建/写入/跨进程持久化正常（`PRAGMA integrity_check` = ok，`journal_mode` = wal）、采样节奏实测为稳定的 10.0 秒、旧日志文件被正确回填进库且时间戳按本机时区解析、六个 `/api/v1/history/*` 端点全部 200 且返回真实曲线、保留策略与 checkpoint 工作正常。

## 许可

MIT © 2026 Kairo