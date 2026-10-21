# Phone Data Server

A Magisk / KernelSU module that turns the phone into a read-only telemetry
endpoint: every metric it can read as root is exposed as JSON over HTTP, and the
same document can be POSTed to a server you own on a timer.

Everything is a single statically linked Go binary (linux/arm64, no runtime
dependencies, no Python/Node needed on the device).

## Install

KernelSU / SukiSU manager: install this zip as a module, then reboot.

    ksud module install /sdcard/phoneserver-module.zip

State lives in `/data/adb/phoneserver/` (config + history.db + logs) — deliberately **outside**
the module directory, so updating or reinstalling the module keeps your API token
and push settings.

## Quick start

    # find the token (it is generated on first boot)
    adb shell su -c 'grep api_token /data/adb/phoneserver/config.json'
    # or tap the module's Action button in the manager

    curl http://PHONE_IP:8080/api/v1/health                 # no auth needed
    curl -H 'X-API-Key: TOKEN' http://PHONE_IP:8080/api/v1/all
    # a browser gets a live dashboard at http://PHONE_IP:8080/

The port is 8080 on all interfaces by default. `X-API-Key`, `Authorization:
Bearer …` and `?key=…` are all accepted.

## Web UI (inside the KernelSU / SukiSU manager)

The module ships a WebUI: open the module card in the manager and press
**WebUI**. It shows the same live metrics as the browser dashboard (battery, CPU
per-core, memory, thermal, Wi-Fi, network, storage, GPU, processes) plus
controls — restart / stop / start the service, switch the listen port, edit the
cloud-push settings, push immediately, reveal or copy the API token, and tail
the logs.

The page is a thin renderer: every button runs
`/data/adb/modules/phoneserver/webui.sh <subcommand>` through KernelSU's
`window.ksu.exec(cmd, '{}', callbackName)` bridge, so the same backend is
scriptable from a shell and testable over adb:

    sh webui.sh snapshot                 # {status:{...},data:{...}} — one call per refresh
    sh webui.sh status                   # service + config state
    sh webui.sh data [battery|cpu|...]   # proxied REST resource
    sh webui.sh restart | start | stop
    sh webui.sh push-now
    sh webui.sh set-push <url> <seconds> <bearer> <0|1>
    sh webui.sh set-port 9090            # saves, restarts, confirms
    sh webui.sh logs 60 | api-log 60 | clear-logs
    sh webui.sh token
    sh webui.sh history-panel <metric> <since>   # {stats,series,logs} — one call per refresh
    sh webui.sh history battery_temp_c,cpu_usage_pct -6h 240
    sh webui.sh history-stats | history-metrics
    sh webui.sh history-logs 50 <grep> <since>
    sh webui.sh history-snapshot <ts> <section> | history-prune

`restart`, `start` and `set-port` do not report success until the API actually
answers on the new state — if something is wrong you get `ok:false` plus the
reason, never a false green.

Do **not** chmod or relabel `webroot/` yourself: KernelSU sets its permissions
and SELinux context at install time, and the page will not load if you override
them.

## Endpoints

| method | path | content |
|---|---|---|
| GET | `/api/v1/health` | liveness, server uptime, whether auth/push are on (no auth) |
| GET | `/api/v1/endpoints` | machine-readable index of everything below |
| GET | `/api/v1/all` | the whole snapshot in one document |
| GET | `/api/v1/device` | brand/model/codename, ROM, build, kernel, baseband, slot, bootloader state |
| GET | `/api/v1/props` | the full `getprop` dump (cached 10 min) |
| GET | `/api/v1/battery` | charge, status, health %, cycle count, mAh, temp, current, voltage, time-to-full/empty |
| GET | `/api/v1/power` | every `/sys/class/power_supply` node, plus `usb`/`wireless` views and screen wakefulness |
| GET | `/api/v1/display` | screen on/off, brightness and its max, brightness source path |
| GET | `/api/v1/cpu` | per-core current/min/max kHz, governor, total + per-core usage (delta-based), loadavg |
| GET | `/api/v1/gpu` | Adreno busy %, clock, max clock, temp, power levels, available frequencies |
| GET | `/api/v1/memory` | `/proc/meminfo` (total/free/available/buffers/cached/shmem/dirty/slab/swap) |
| GET | `/api/v1/storage` | statfs per mount: total/free/used, %, writable, fstype |
| GET | `/api/v1/thermal` | **all** thermal zones in Celsius, sorted hottest-first, + cooling device count |
| GET | `/api/v1/network` | every interface: state, MAC, MTU, IPv4/IPv6, byte/packet counters **and live per-second rates** |
| GET | `/api/v1/network/wifi` | SSID, BSSID, MAC, IP, RSSI (dBm + %), link speed, frequency, band, channel, standard, security type, supplicant state, tx/rx packet counters |
| GET | `/api/v1/system` | uptime, boot time, kernel, SELinux mode, loadavg, process count, timezone |
| GET | `/api/v1/processes` | process count and the top 15 by RSS |
| GET | `/api/v1/packages` | installed package counts (total / system / user / disabled) |
| GET | `/api/v1/config` | current config, secrets redacted |
| PUT | `/api/v1/config` | change push settings, token, TTLs, storage mounts |
| GET | `/api/v1/push` | push state: counters, last result, last error |
| POST | `/api/v1/push` | push the current snapshot right now (returns the result) |
| GET | `/api/v1/history` | query stored samples (`metrics`, `since`, `until`, `limit`, `stats`) |
| GET | `/api/v1/history/metrics` | the queryable metric column names |
| GET | `/api/v1/history/logs` | query stored log lines (`grep`, `level`, `since`, `limit`) |
| GET | `/api/v1/history/snapshot` | the full stored document nearest `ts` (or `section=<name>`) |
| GET | `/api/v1/history/stats` | db size, row counts, growth rate, battery-cost estimate |
| POST | `/api/v1/history/prune` | apply the retention policy now |

Read endpoints are GET-only (anything else is a 405) and require the token.

## Screenshots

```
curl -H "X-Api-Key: $TOKEN" -o shot.webp http://<phone>:8080/api/v1/screenshot
curl -H "X-Api-Key: $TOKEN" -o small.webp 'http://<phone>:8080/api/v1/screenshot?quality=60&scale=0.5'
curl -H "X-Api-Key: $TOKEN" 'http://<phone>:8080/api/v1/screenshot?meta=1'
```

`format` = `webp` (default) / `jpeg` / `png`, `quality` = 1-100 (default 80),
`scale` = 0.1-1, `display` = an explicit display id, `meta=1` for JSON metadata
only, `base64=1` to get the image inline in that JSON, `download=1` to save it.

A frame only exists while a request is being served: there is **no background
capture, nothing is written to the history database, and nothing is pushed**. The
push payload is the telemetry document and has no image field at all. The
response states it too (`X-Screenshot-Stored` / `X-Screenshot-Pushed`, and
`stored` / `pushed` in the JSON).

Measured on this device (1280x2772): webp q80 **166 KB** (3.5% of the PNG),
q70 93 KB, q60 67 KB, q60 + `scale=0.5` 12 KB, jpeg q85 438 KB, png 4.8 MB
(passthrough). Capture ~350 ms (over 2 s if the display was asleep), encode
150-360 ms. Encoding is pure Go (`github.com/KarpelesLab/gowebp`, no cgo - the
binary has to stay cgo-free and static to exec on Android); lossless webp is
pointless for screenshots, so the default is lossy VP8.

Protected windows (`FLAG_SECURE`: banking apps, some video, password fields) come
back as a black frame - SurfaceFlinger protects the content and this does not try
to get around it. Requests are logged like any other endpoint.

## Pushing to your own server

    curl -X PUT -H 'X-API-Key: TOKEN' -H 'Content-Type: application/json' \
      -d '{"push":{"enabled":true,"url":"https://example.com/ingest",
                   "interval_seconds":60,"bearer_token":"shared-secret",
                   "retries":3}}' \
      http://PHONE_IP:8080/api/v1/config

Every interval the phone POSTs:

```json
{
  "device_id": "25053RT47C-9f3c21ab",
  "seq": 42,
  "collected_at": "2026-10-21T12:20:08+08:00",
  "payload": { "device": {}, "battery": {}, "cpu": {}, "…": {} }
}
```

Headers: `Content-Type: application/json`, `X-Device-Id`, `X-Seq`, `User-Agent:
phoneserver/1.2.0`, and `Authorization: Bearer …` when a bearer token is set.
Non-2xx answers are retried `retries` times with a 1s/2s/… backoff, and the last
outcome is always visible at `GET /api/v1/push`. Failed pushes are not queued —
pull the API if you need a complete history.

A minimal ingest receiver is three lines of Python:

```python
import http.server, json
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        print(body["device_id"], body["seq"], body["payload"]["battery"]["capacity_percent"])
        self.send_response(200); self.end_headers(); self.wfile.write(b'{"ok":true}')
http.server.HTTPServer(("0.0.0.0", 9099), H).serve_forever()
```

## Local history (SQLite)

The service keeps its own history on the phone, so you do not need a cloud
server to look at a curve. Everything lives in one SQLite database:

    table       contents                                                   default retention
    samples     one row per tick, ~65 scalar metrics flattened to columns  30 days
    snapshots   the complete /api/v1/all document, gzipped into a BLOB     3 days
    logs        log lines, mirrored live and backfilled from the existing  7 days
                log files on first start

The database is `/data/adb/phoneserver/history.db` — outside the module
directory on purpose, so reinstalling or upgrading the module never loses it.

The driver is `modernc.org/sqlite`: SQLite compiled to **pure Go**. That was
forced by the build method — the binary has to be CGO-free and statically linked
to be exec'd on Android at all (see Build below), and mattn/go-sqlite3 needs
CGO plus the NDK. Verified on a real device: WAL on f2fs, cross-process
durability, and a standard `SQLite format 3` header, so any SQLite client can
open it (`sqlite3 history.db`, or Python's `sqlite3` module).

    curl -H 'X-API-Key: TOKEN' 'http://PHONE_IP:8080/api/v1/history?metrics=battery_temp_c,cpu_usage_pct&since=-6h&stats=1'
    curl -H 'X-API-Key: TOKEN' 'http://PHONE_IP:8080/api/v1/history/logs?since=-24h&grep=error'
    curl -H 'X-API-Key: TOKEN' 'http://PHONE_IP:8080/api/v1/history/snapshot?section=battery'

`since`/`until` accept relative offsets (`-30m`, `-6h`, `-7d`), unix seconds, or
RFC3339. `points` comes back as `[ts, metric1, metric2, ...]`, oldest first, so
it can be plotted directly; `columns` maps the positions to names.

`/api/v1/history/stats` also carries a `timing` block that splits one cycle into
the wait, the collection and the database write (last + average). Steady state
here is a 9.5 s gap, 278 ms of collection and ~2 ms of writing. **The first few
cycles after a restart are much slower (20-30 s)** because the TTL caches start
cold: the first snapshot has to run `getprop`, `pm list packages` and `dumpsys`
for real. Check `timing.avg_gap_ms` before concluding the cadence is wrong.

**Battery cost, measured rather than guessed** (Snapdragon 8s Gen 4):

    idle 30 s                          0 ticks of process CPU — it is parked
    one full collection (/api/v1/all)  ~146 ms of process CPU (it spawns getprop,
                                       cmd wifi and dumpsys)
    wall time per collection           ~250–300 ms
    at the default 10 s interval       8640/day ≈ 1261 s CPU/day ≈ 1.46% of one core
    for comparison, WebUI open (3 s)   ~4.9% of one core

Disk with the default retention: samples ≈ 4 MB/day, snapshots ≈ 44 MB/day
(~5 KB gzipped each), logs by request volume, and a hard 512 MB cap that evicts
the oldest snapshots first so it can never fill the device.

If 1.46% of a core matters more to you than completeness, raise
`history.full_seconds` (keep `interval_seconds` at 10): the in-between ticks then
take a **cheap path** that only reads sysfs and /proc, no subprocesses, and the
scalar metrics keep their 10 s cadence. At 10 s / 60 s the estimate drops to
about 0.3% of a core. Every row carries a `full` column so you always know which
kind of tick produced it.

### Sampling tiers: 10 s for what matters, 60 s for the rest

`interval_seconds` (default **10**) is the sampling period; `full_seconds`
(default **60**) is how often the *complete document* is stored. The reason is
measured, not aesthetic: the complete document is 22.6 KB (every thermal zone,
every interface, the process list) while the scalar row is 65 numbers, and at a
10 s cadence the document is the entire disk cost.

    every 10 s  (cheap tick) 65 scalar metrics - battery, temps, CPU, memory,
                             rates, screen, charging
    every 60 s  (full tick)  the complete document, plus the columns the cheap
                             tick cannot measure

    Measured: 66 MB/day -> 13.5 MB/day, and 1261 -> ~270 CPU-seconds/day.

Every row carries a `full` column saying which kind it is. Setting
`full_seconds: 10` gives a complete document on every tick.

### Columns a cheap tick cannot see: NULL, never 0

A cheap tick does not spawn `cmd wifi` or `pm list packages`, so 14 columns are
simply not measurable in those rows: `wifi_ssid`, `wifi_connected`,
`wifi_rssi_dbm`, `wifi_signal_pct`, `wifi_link_mbps`, `wifi_freq_mhz`,
`wifi_channel`, `pkg_total`, `pkg_user`, `pkg_system`, `pkg_disabled`,
`proc_count`, `battery_time_to_full_s`, `battery_time_to_empty_s`.

They are stored as **NULL, not 0** - a zero would read as a real measurement
("0 % signal", "0 apps installed"). `/api/v1/history` carries the last measured
value forward so a series has no holes, and names the columns it did that for:

    "carried_columns": ["pkg_total", "proc_count", "wifi_signal_pct"]

The `stats` min/avg/max are computed over genuinely measured values only, so
those stay clean.

## Units and conventions

- Every value is the raw kernel value where units are unambiguous, with a
  converted twin: `charge_full_mah`, `temp_c`, `voltage_v`, `rssi_dbm`,
  `max_temp_c`, `rx_bytes_per_sec`, `cur_khz`, `total_bytes`.
- Power-supply integers are the raw sysfs numbers, named `*_raw`, next to the
  `*_v` / `*_a` conversions that assume the kernel convention (µV / µA) — vendor
  drivers deviate (this device's `wireless` node reports mV), so trust `*_raw`
  when a number looks wrong. The device's `battery` node is the reliable one.
- CPU and network usage rates are **deltas between consecutive requests**;
  `sample_interval_ms` tells you the window, and the first response after start
  has no window (rates are 0).
- Thermal temperatures are converted from the driver's millidegrees, with a
  guard for the few zones that report tenths or whole degrees.
- Expensive sources (`getprop`, `cmd wifi status`, `dumpsys power`, `pm list
  packages`, `/proc` scan) are cached behind TTLs — see `*_ttl_seconds` in the
  config. `/api/v1/all` costs roughly 0.3–0.6 s on a Snapdragon 8s Gen 4.

## Security notes

- The server runs **as root** and binds every interface. Keep the API token set;
  clear `api_token` only if you understand that anyone who can route to the port
  can read your device state.
- There is deliberately **no arbitrary-file-read endpoint**. Adding one would
  turn a root process on a network port into a remote file disclosure.
- `GET /api/v1/config` redacts the token and bearer secret, and `PUT` refuses to
  overwrite a real secret with a redacted placeholder, so a read-modify-write
  round trip cannot destroy your credentials.
- HTTP only. If you push over the internet, terminate TLS at your own server (or
  put a reverse proxy in front) and use `https://` as the push URL.

## Files

    /data/adb/modules/phoneserver/
      module.prop  customize.sh  service.sh  watchdog.sh  action.sh  uninstall.sh
      bin/phoneserver            # static arm64 binary
      webui.sh                   # WebUI backend (also scriptable over adb)
      webroot/index.html         # WebUI page shown by the manager (permissions are
                                 # set by KernelSU — do not chmod it yourself)
    /data/adb/phoneserver/
      config.json                # 0600
      history.db                 # SQLite history (+ history.db-wal / -shm)
      logs/phoneserver.log       # rotating, 4 MB × 3
      logs/stdout.log            # binary stdout + watchdog restarts
      watchdog.pid

## Operations

- Status, address and token: tap **Action** in the manager, or run `action.sh`.
- Restart: `pkill -f bin/phoneserver` — `watchdog.sh` restarts it within 10 s.
- Stop for good: remove the module (or kill the watchdog pid first).
- Logs: `tail -f /data/adb/phoneserver/logs/phoneserver.log` (includes one line
  per HTTP request when `log_requests` is on).
- Change the port: `"listen": "0.0.0.0:9090"` then restart the service.
- `-once` prints a full snapshot to stdout without starting the server, which is
  handy for testing on the device:
  `bin/phoneserver -once -config /data/adb/phoneserver/config.json`
- History: `sh webui.sh history-stats` for size/rows/growth, `history-prune` to
  apply retention now. Turning history off entirely is `"history":{"enabled":false}`
  in the config (takes effect without a restart); changing `db_path` needs one.

## Troubleshooting

- **`curl` gets nothing**: is the service running (`action.sh`)? Check
  `/data/adb/phoneserver/logs/stdout.log` for a bind error — another process on
  8080, or the module was not enabled after install.
- **401 on every call**: the token in your request does not match
  `/data/adb/phoneserver/config.json`.
- **`wifi.error` / empty `wifi`**: `cmd wifi status` failed. It needs root; if it
  is missing entirely, check `pm list features` for Wi-Fi support.
- **Battery values all zero**: wrong sysfs path for your device — the collector
  expects `/sys/class/power_supply/battery/`.
- **`packages.error`**: `pm list packages` timed out (30 s); counts are cached.
- **`/api/v1/history/*` answers 503**: history is switched off (`history.enabled`)
  or the database could not be opened. Running the service by hand prints the
  reason to stderr; `/api/v1/history/stats` reports it in `last_error`.

## Build

    ./build.sh          # cross-compile + stage into module/ + produce the installable zip
    # equivalent by hand:
    GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o ps-static .
    cp ps-static module/bin/phoneserver

The only dependency is the pure-Go SQLite driver (`modernc.org/sqlite`); the
first build fetches it (in China: `export GOPROXY=https://goproxy.cn,direct`),
after that the module cache makes builds offline.

**Do not add `-buildmode=pie`.** Go's PIE output carries
`PT_INTERP=/lib/ld-linux-aarch64.so.1`, which does not exist on Android (the
loader is `/system/bin/linker64`), so the kernel fails the exec with ENOENT and
the process dies with exit 126. A plain static build (`ET_EXEC`, no interpreter)
runs fine, including from `/data`.

## Credit

Requires Magisk ≥ 20.4 or KernelSU/SukiSU. Module format follows the Magisk
module spec, so `customize.sh`, `service.sh`, `action.sh` and `uninstall.sh` are
honoured by both installers.