#!/system/bin/sh
# phoneserver WebUI backend (2.0) — the KernelSU/KernelSU-Next manager runs this
# from the module card's WebUI button. It is a thin, honest wrapper around the
# local REST API: it never invents a result, every write is read back, and the
# token never leaves the device except through `token`.
#
#   status              JSON for the UI: version, running, port, listen, lan_ip, token, ...
#   health              /api/v1/health
#   get-config          current config (token masked)
#   set-config [json]   PUT a full or partial config (also reads JSON on stdin)
#   set-listen <host:port>  change the bind address; enables LAN only if asked
#   restart|start|stop  service control
#   logs [n]            stdout.log tail (service output)
#   api-log [n]         phoneserver.log tail (the daemon's own log)
#   clear-logs          truncate both logs
#   bridge-status       Termux:API bridge readiness
#   bridge-fix          grant permissions, wake lock, raise uid to foreground
#   stream-status       active stream encoders, fps, bitrate
#   api <GET path>      any GET endpoint, e.g. api /api/v1/battery
#   api-post <path> <base64 json>  POST arbitrary JSON
#   ui-click <base64 text>         find an element by text and tap it
#   input-text <base64 text>       type into the focused field
#   apps-list [grep]    launchable packages
#   notifications [n] [--ongoing]
#   messages <sms|calls|contacts> [n]
#   camera-photo [--meta]   take a photo (base64 comes back in JSON with --meta)
#   mic-record <seconds>    record and answer the audio
#   files-shares        the configured shares
#   files-list <share> <path>
#   mcp-info            MCP endpoint + stdio command
#   history [metrics] [since] [limit]
#   history-stats | history-metrics | history-logs [grep] | history-snapshot [ts] | history-prune
#   screenshot [--meta]  still screenshot (webp)
#   token               print the API token
#   set-port <port>     loopback port only (kept for compatibility)
#
# Nothing here talks to any remote server: there is no upload path and no cloud
# dependency in 2.0, and the push subcommands of 1.x are gone.

MODDIR=${0%/*}
DATA=/data/adb/phoneserver
CFG=$DATA/config.json

[ -f "$CFG" ] || { echo '{"ok":false,"error":"no config at /data/adb/phoneserver/config.json; start the service once"}'; exit 1; }

# --- token + listen, read from the config the daemon is actually using --------
read_cfg() { sed -n 's/.*"'"$1"'": *"\([^"]*\)".*/\1/p' "$CFG" | head -1; }
TOKEN=$(read_cfg api_token)
LISTEN=$(read_cfg listen)
PORT=$(printf '%s' "$LISTEN" | sed -n 's/.*:\([0-9]*\)$/\1/p')
[ -n "$PORT" ] || PORT=8080
BASE=http://127.0.0.1:$PORT

# Single-quote a value for sh, so nothing a caller passes can escape.
q() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }

# req <method> <path> [json-body] [extra curl args...]
# Prints the response body. On a transport failure it prints a JSON error that
# distinguishes "not listening" from "timed out", and returns non-zero.
req() {
    M=$1; P=$2
    # Argument 1 and 2 are dropped one shift at a time, and the body slot (if the
    # caller passed one) is dropped only when it exists: mksh aborts the whole
    # function on `shift N` with N > $#, which made every API-backed subcommand
    # print nothing at all on the phone instead of failing loudly.
    shift
    shift
    if [ $# -gt 0 ]; then
        BODY=$1
        shift
    else
        BODY=
    fi
    if [ -n "$BODY" ]; then
        OUT=$(curl -s -m 25 -X "$M" -H "X-API-Key: $TOKEN" -H 'Content-Type: application/json' \
              -d "$BODY" "$BASE$P" "$@" 2>&1)
    else
        OUT=$(curl -s -m 25 -X "$M" -H "X-API-Key: $TOKEN" "$BASE$P" "$@" 2>&1)
    fi
    RC=$?
    if [ $RC -ne 0 ]; then
        if [ $RC -eq 28 ]; then
            printf '{"ok":false,"error":"request timed out after 25s: %s %s"}\n' "$M" "$P"
        else
            printf '{"ok":false,"error":"cannot reach %s (the service is not listening or has exited; try: restart)"}\n' "$BASE"
        fi
        return 1
    fi
    printf '%s\n' "$OUT"
    return 0
}

# code <method> <path> -> HTTP status only
code() {
    curl -s -m 25 -o /dev/null -w '%{http_code}' -X "$1" -H "X-API-Key: $TOKEN" "$BASE$2"
}

# running -> pid of the daemon (empty when it is not up).
# Matches on the config path rather than on argv[0]: the watchdog starts the
# binary with an absolute path, but Android's ps may render ARGS without it, so
# a pattern like 'bin/phoneserver' reported "not running" for a live daemon.
running() {
    for P in $(ps -A -o PID,ARGS 2>/dev/null | grep -v grep | grep -F "$CFG" | awk '{print $1}'); do
        printf '%s ' "$P"
    done
}

lan_ip() {
    ip -4 addr show 2>/dev/null | sed -n 's/.*inet \([0-9.]*\)\/.*/\1/p' | grep -v '^127\.' | head -1
}

token() { printf '%s' "$TOKEN"; }

# ------------------------------------------------------------------ subcommands

cmd_status() {
    PID=$(running)
    [ -n "$PID" ] && RUN=true || RUN=false
    VER=$("$MODDIR/bin/phoneserver" -version 2>/dev/null | head -1)
    LAN=$(lan_ip)
    # The token is included on purpose: the manager WebView has to authenticate,
    # and it is the same value already stored in a 0600 file.
    printf '{\n'
    printf '  "ok": true,\n'
    printf '  "version": %s,\n' "$(printf '%s' "$VER" | sed 's/^/"/;s/$/"/')"
    printf '  "running": %s,\n' "$RUN"
    printf '  "pid": %s,\n' "${PID:-0}"
    printf '  "watchdog_pid": %s,\n' "$([ -f "$DATA/watchdog.pid" ] && cat "$DATA/watchdog.pid" || echo 0)"
    printf '  "listen": %s,\n' "$(printf '%s' "$LISTEN" | sed 's/^/"/;s/$/"/')"
    printf '  "port": %s,\n' "$PORT"
    printf '  "lan_ip": "%s",\n' "$LAN"
    printf '  "token": "%s",\n' "$TOKEN"
    printf '  "config_path": "%s",\n' "$CFG"
    printf '  "module_dir": "%s",\n' "$MODDIR"
    printf '  "data_dir": "%s"\n' "$DATA"
    printf '}\n'
}

cmd_restart() {
    "$MODDIR/bin/phoneserver" -stop -config "$CFG" >/dev/null 2>&1
    for P in $(running); do kill "$P" 2>/dev/null; done
    sleep 1
    if [ -x "$MODDIR/watchdog.sh" ]; then
        if command -v setsid >/dev/null 2>&1; then
            setsid "$MODDIR/watchdog.sh" </dev/null >>"$DATA/logs/stdout.log" 2>&1 &
        else
            "$MODDIR/watchdog.sh" </dev/null >>"$DATA/logs/stdout.log" 2>&1 &
        fi
    fi
    # Poll until it answers, and say what actually happened instead of claiming success.
    i=0
    while [ $i -lt 15 ]; do
        C=$(code GET /api/v1/health)
        if [ "$C" = "200" ]; then
            printf '{"ok":true,"restarted":true,"health":200,"waited_s":%s}\n' "$i"
            return 0
        fi
        sleep 1
        i=$((i + 1))
    done
    printf '{"ok":false,"error":"restarted, but %s did not answer /api/v1/health within 15s","tries":%s}\n' "$BASE" "$i"
    return 1
}

cmd_start() {
    [ -n "$(running)" ] && { printf '{"ok":true,"already_running":true}\n'; return 0; }
    cmd_restart
}

cmd_stop() {
    for P in $(running); do kill "$P" 2>/dev/null; done
    [ -f "$DATA/watchdog.pid" ] && kill "$(cat "$DATA/watchdog.pid")" 2>/dev/null
    sleep 1
    printf '{"ok":true,"stopped":true,"still_running":"%s"}\n' "$(running)"
}

cmd_set_listen() {
    SPEC=$1
    case "$SPEC" in
        *:*) ;;
        *) printf '{"ok":false,"error":"need host:port, e.g. 127.0.0.1:8080 or 0.0.0.0:8080"}\n'; return 1 ;;
    esac
    HOST=${SPEC%:*}
    case "$HOST" in
        127.0.0.1|localhost|::1) LAN=false ;;
        *) LAN=true ;;
    esac
    if [ "$LAN" = "true" ]; then
        if [ "$2" != "--yes" ]; then
            printf '{"ok":false,"need_confirm":true,"error":"%s is not loopback: anyone on this network could reach the API; the token is the only credential. Re-run with --yes to confirm."}\n' "$SPEC"
            return 1
        fi
        BODY=$(printf '{"listen":"%s","security":{"allow_non_loopback_listen":true}}' "$SPEC")
    else
        BODY=$(printf '{"listen":"%s","security":{"allow_non_loopback_listen":false}}' "$SPEC")
    fi
    req PUT /api/v1/config "$BODY" >/dev/null || return 1
    # Read back what the daemon now has, so a rejected value is visible.
    NOW=$(req GET /api/v1/config | sed -n 's/.*"listen": *"\([^"]*\)".*/\1/p' | head -1)
    cmd_restart >/dev/null
    printf '{"ok":true,"requested":"%s","configured":"%s","restarted":true}\n' "$SPEC" "$NOW"
}

cmd_set_port() {
    cmd_set_listen "127.0.0.1:$1"
}

cmd_get_config() { req GET /api/v1/config; }

cmd_set_config() {
    if [ -n "$1" ]; then BODY=$1; else BODY=$(cat); fi
    [ -n "$BODY" ] || { printf '{"ok":false,"error":"no JSON given"}\n'; return 1; }
    OUT=$(req PUT /api/v1/config "$BODY") || return 1
    printf '%s' "$OUT" | grep -q '"status": *400' && { printf '%s\n' "$OUT"; return 1; }
    printf '%s\n' "$OUT"
}

cmd_logs() {
    N=${1:-40}
    tail -n "$N" "$DATA/logs/stdout.log" 2>/dev/null || printf 'no stdout.log yet\n'
}

cmd_api_log() {
    N=${1:-80}
    tail -n "$N" "$DATA/logs/phoneserver.log" 2>/dev/null || printf 'no phoneserver.log yet\n'
}

cmd_clear_logs() {
    : >"$DATA/logs/stdout.log" 2>/dev/null
    : >"$DATA/logs/phoneserver.log" 2>/dev/null
    printf '{"ok":true,"cleared":true}\n'
}

b64d() { printf '%s' "$1" | base64 -d 2>/dev/null; }

cmd_api() { req GET "$1"; }

cmd_api_post() {
    BODY=$(b64d "$2")
    req POST "$1" "$BODY"
}

cmd_ui_click() {
    TEXT=$(b64d "$1")
    BODY=$(printf '{"text":"%s"}' "$TEXT")
    req POST /api/v1/ui/click "$BODY"
}

cmd_input_text() {
    TEXT=$(b64d "$1")
    BODY=$(printf '{"text":"%s"}' "$TEXT")
    req POST /api/v1/input/text "$BODY"
}

cmd_apps_list() {
    if [ -n "$1" ]; then req GET "/api/v1/apps?limit=300&grep=$(printf '%s' "$1" | sed 's/ /%20/g')"; else req GET "/api/v1/apps?limit=300"; fi
}

cmd_notifications() {
    N=${1:-20}
    case "$2" in --ongoing|1) ONG=1 ;; *) ONG=0 ;; esac
    req GET "/api/v1/notifications?limit=$N&include_ongoing=$ONG"
}

cmd_messages() {
    SRC=${1:-sms}
    N=${2:-20}
    req GET "/api/v1/messages/list?source=$SRC&limit=$N"
}

cmd_camera_photo() {
    case "$1" in --meta|-m) req GET '/api/v1/camera/photo?format=webp&maxwidth=720&quality=70&meta=1' ;;
    *) req GET '/api/v1/camera/photo?format=webp&maxwidth=720&quality=70' --output "$DATA/logs/last-photo.webp" ;;
    esac
}

cmd_mic_record() {
    S=${1:-3}
    req GET "/api/v1/mic/record?seconds=$S&meta=1"
}

cmd_bridge_status() { req GET /api/v1/bridge/status; }
cmd_bridge_fix() { req POST /api/v1/bridge/fix '{}'; }
cmd_stream_status() { req GET /api/v1/stream/status; }

cmd_files_shares() { req GET '/files/' ; }
cmd_files_list() { req GET "/files/$1/$2?json=1"; }

cmd_mcp_info() {
    printf '{\n  "endpoint": "%s/mcp",\n  "http": "POST /mcp (JSON-RPC 2.0)",\n  "stdio": "bin/phoneserver -mcp -config %s",\n  "tools": "GET %s/api/v1/endpoints lists the REST surface every tool wraps"\n}\n' "$BASE" "$CFG" "$BASE"
}

cmd_history() {
    M=${1:-ts,battery_capacity_pct}
    S=${2:--6h}
    L=${3:-60}
    req GET "/api/v1/history?metrics=$M&since=$S&limit=$L"
}
cmd_history_stats()    { req GET /api/v1/history/stats; }
cmd_history_metrics()  { req GET /api/v1/history/metrics; }
cmd_history_logs()     { req GET "/api/v1/history/logs?limit=${1:-80}"; }
cmd_history_snapshot() { req GET "/api/v1/history/snapshot?ts=${1:-0}"; }
cmd_history_prune()    { req POST /api/v1/history/prune '{}'; }
cmd_history_panel()    { req GET "/api/v1/history/quality?since=-24h"; }

cmd_screenshot() {
    case "$1" in
        --meta|-m) req GET '/api/v1/screenshot?meta=1&quality=60&scale=0.5' ;;
        *) req GET '/api/v1/screenshot?quality=60&scale=0.5' --output "$DATA/logs/last-shot.webp" ;;
    esac
}

cmd_data() { req GET "${1:-/api/v1/all}"; }
cmd_snapshot() { req GET /api/v1/all; }
cmd_health() { req GET /api/v1/health; }

# ----------------------------------------------------------------- dispatch ---
case "$1" in
    status)            cmd_status ;;
    health)            cmd_health ;;
    data)              shift; cmd_data "$@" ;;
    snapshot)          cmd_snapshot ;;
    restart)           cmd_restart ;;
    start)             cmd_start ;;
    stop)              cmd_stop ;;
    set-listen)        shift; cmd_set_listen "$@" ;;
    set-port)          shift; cmd_set_port "$@" ;;
    get-config)        cmd_get_config ;;
    set-config)        shift; cmd_set_config "$@" ;;
    bridge-status)     cmd_bridge_status ;;
    bridge-fix)        cmd_bridge_fix ;;
    stream-status)     cmd_stream_status ;;
    api)               shift; cmd_api "$@" ;;
    api-post)          shift; cmd_api_post "$@" ;;
    ui-click)          shift; cmd_ui_click "$@" ;;
    input-text)        shift; cmd_input_text "$@" ;;
    apps-list)         shift; cmd_apps_list "$@" ;;
    notifications)     shift; cmd_notifications "$@" ;;
    messages)          shift; cmd_messages "$@" ;;
    camera-photo)      shift; cmd_camera_photo "$@" ;;
    mic-record)        shift; cmd_mic_record "$@" ;;
    files-shares)      cmd_files_shares ;;
    files-list)        shift; cmd_files_list "$@" ;;
    mcp-info)          cmd_mcp_info ;;
    history)           shift; cmd_history "$@" ;;
    history-stats)     cmd_history_stats ;;
    history-metrics)   cmd_history_metrics ;;
    history-logs)      shift; cmd_history_logs "$@" ;;
    history-snapshot)  shift; cmd_history_snapshot "$@" ;;
    history-prune)     cmd_history_prune ;;
    history-panel)     cmd_history_panel ;;
    screenshot)        shift; cmd_screenshot "$@" ;;
    logs)              shift; cmd_logs "$@" ;;
    api-log)           shift; cmd_api_log "$@" ;;
    clear-logs)        cmd_clear_logs ;;
    token)             token; echo ;;
    *) cat <<EOF
{"ok":false,"error":"unknown subcommand: $1","usage":"status|health|get-config|set-config|set-listen|set-port|restart|start|stop|logs|api-log|clear-logs|bridge-status|bridge-fix|stream-status|api|api-post|ui-click|input-text|apps-list|notifications|messages|camera-photo|mic-record|files-shares|files-list|mcp-info|history*|screenshot|token"}
EOF
       exit 1 ;;
esac
