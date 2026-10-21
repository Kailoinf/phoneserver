# Phone Data Server

A Magisk / KernelSU module that turns the phone into a read-only telemetry
endpoint: every metric it can read as root is exposed as JSON over HTTP, and the
same document can be POSTed to a server you own on a timer.

Everything is a single statically linked Go binary (linux/arm64, no runtime
dependencies, no Python/Node needed on the device).

## Install

KernelSU / SukiSU manager: install this zip as a module, then reboot.

    ksud module install /sdcard/phoneserver-1.0.0.zip

State lives in `/data/adb/phoneserver/` (config + logs) — deliberately **outside**
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

Read endpoints are GET-only (anything else is a 405) and require the token.

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
phoneserver/1.0.0`, and `Authorization: Bearer …` when a bearer token is set.
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

## Build

    # from the module source tree
    GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o ps-static .
    cp ps-static module/bin/phoneserver

**Do not add `-buildmode=pie`.** Go's PIE output carries
`PT_INTERP=/lib/ld-linux-aarch64.so.1`, which does not exist on Android (the
loader is `/system/bin/linker64`), so the kernel fails the exec with ENOENT and
the process dies with exit 126. A plain static build (`ET_EXEC`, no interpreter)
runs fine, including from `/data`.

## Credit

Requires Magisk ≥ 20.4 or KernelSU/SukiSU. Module format follows the Magisk
module spec, so `customize.sh`, `service.sh`, `action.sh` and `uninstall.sh` are
honoured by both installers.