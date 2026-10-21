#!/system/bin/sh
# Phone Data Server — WebUI backend.
#
# The KernelSU WebUI page calls this through `ksu.exec`, one subcommand per
# request, and renders whatever JSON comes back. Every subcommand is also
# usable by hand over `adb shell`, which is how it is tested.
#
#   snapshot                 {status:{...}, data:{...}}   (one call per refresh)
#   status                   service/config state as JSON
#   data [section]           the REST API's JSON, proxied through curl
#   restart | stop | start   service control (restart/start confirm the service
#                            actually answers before reporting ok)
#   push-now                 trigger a cloud push now, return the result
#   set-push <url> <secs> <bearer> <0|1>   write push settings
#   set-port <port>          change the listen port, restart, confirm
#   logs [n] | api-log [n]   tail the daemon logs
#   clear-logs               truncate the logs
#   token                    print the API token
#
#   history-stats            local history db: size, rows, growth, battery cost
#   history [metrics] [since] [n]    e.g. "battery_temp_c,cpu_usage_pct" -6h 240
#   history-logs [n] [grep] [since]  stored log lines (live + backfilled)
#   history-snapshot [ts] [section]  the full document stored nearest ts
#   history-prune            apply the retention policy now
#   history-panel [metric] [since]   {stats,series,logs} in one call (UI polling)
#
# Rule for this script: never report ok:true for something that did not happen.
set -u

MODDIR=${0%/*}
DATA=/data/adb/phoneserver
BIN="$MODDIR/bin/phoneserver"
CFG="$DATA/config.json"
# Read the version from module.prop instead of hardcoding it, so the WebUI can
# never display a version that disagrees with the module actually installed.
VERSION=$(sed -n 's/^version=//p' "$MODDIR/module.prop" 2>/dev/null | head -1)
[ -n "$VERSION" ] || VERSION=unknown

cfg_str() {
    sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$CFG" 2>/dev/null | head -1
}
cfg_bool() {
    sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\(true\|false\).*/\1/p" "$CFG" 2>/dev/null | head -1
}
cfg_num() {
    sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p" "$CFG" 2>/dev/null | head -1
}
# Escape a string for embedding in JSON.
jesc() { printf '%s' "${1:-}" | sed 's/\\/\\\\/g; s/"/\\"/g'; }

# listen is a quoted string ("0.0.0.0:8080"), so it must not be parsed as a number.
listen_val() { L=$(cfg_str listen); [ -n "$L" ] || L="0.0.0.0:8080"; echo "$L"; }
port() { P=${1:-$(listen_val)}; P=${P##*:}; case "$P" in ''|*[!0-9]*) P=8080 ;; esac; echo "$P"; }
api_host() {
    H=${1:-$(listen_val)}; H=${H%%:*}
    case "$H" in ''|0.0.0.0|'*'|'[::]'|::) H=127.0.0.1 ;; esac
    echo "$H"
}
api_url() { echo "http://$(api_host)"; }
token() { cfg_str api_token; }
daemon_pid() { pgrep -f "$BIN -config" 2>/dev/null | head -1; }
watchdog_pid() { cat "$DATA/watchdog.pid" 2>/dev/null; }
lan_ip() { ip -4 addr show wlan0 2>/dev/null | sed -n 's/.*inet \([0-9.]*\).*/\1/p' | head -1; }

# api <path> [port]  — GET one API resource, empty string when unreachable.
api() {
    P=${2:-$(port)}
    t=$(token)
    [ -n "$t" ] || { printf '{"error":"config has no api_token"}'; return 1; }
    curl -s --max-time 8 -H "X-Api-Key: $t" "http://$(api_host):$P/api/v1/$1"
}

# api_call <METHOD> <json-or-empty> [port] -> prints "HTTPCODE<newline>BODY"
api_call() {
    M=$1; BODY=${2:-}; P=${3:-$(port)}
    t=$(token)
    if [ -n "$BODY" ]; then
        curl -s --max-time 8 -w '\nHTTP%{http_code}' -X "$M" -H "X-Api-Key: $t" \
            -H 'Content-Type: application/json' -d "$BODY" "http://$(api_host):$P/api/v1/config"
    else
        curl -s --max-time 8 -w '\nHTTP%{http_code}' -X "$M" -H "X-Api-Key: $t" \
            "http://$(api_host):$P/api/v1/push"
    fi
}
body_of() { printf '%s' "$1" | sed '$d'; }
code_of() { printf '%s' "$1" | tail -n1 | sed 's/HTTP//'; }
# Report a failed API call honestly (empty output = nothing is listening).
fail_call() {
    BODY=$(body_of "$1"); CODE=$(code_of "$1")
    if [ -n "$BODY" ]; then
        printf '%s' "$BODY"
    else
        printf '{"ok":false,"error":"config API not reachable on %s:%s (HTTP %s)"}' "$(api_host)" "$(port)" "$CODE"
    fi
}

wait_health() {  # wait_health [port] [seconds] -> 0 when /health answers
    P=${1:-$(port)}; S=${2:-15}
    i=0
    while [ "$i" -lt "$S" ]; do
        if curl -s --max-time 2 -o /dev/null "http://$(api_host):$P/api/v1/health" 2>/dev/null; then
            return 0
        fi
        sleep 1
        i=$((i + 1))
    done
    return 1
}

cmd_status() {
    PID=$(daemon_pid)
    WPID=$(watchdog_pid)
    PUSH_EN=$(cfg_bool enabled); [ -n "$PUSH_EN" ] || PUSH_EN=false
    LAST="null"
    [ -n "$PID" ] && LAST=$(api push 2>/dev/null)
    [ -n "$LAST" ] || LAST="null"
    printf '{\n'
    printf '  "version": "%s",\n' "$VERSION"
    printf '  "running": %s,\n' "$([ -n "$PID" ] && echo true || echo false)"
    printf '  "pid": %s,\n' "${PID:-0}"
    printf '  "watchdog_pid": %s,\n' "${WPID:-0}"
    printf '  "listen": "%s",\n' "$(listen_val)"
    printf '  "port": %s,\n' "$(port)"
    printf '  "lan_ip": "%s",\n' "$(lan_ip)"
    printf '  "token": "%s",\n' "$(token)"
    printf '  "config_path": "%s",\n' "$CFG"
    printf '  "module_dir": "%s",\n' "$MODDIR"
    printf '  "push": {"enabled": %s, "url": "%s", "interval_seconds": %s, "bearer_set": %s},\n' \
        "$PUSH_EN" "$(jesc "$(cfg_str url)")" "$(cfg_num interval_seconds)" \
        "$([ -n "$(cfg_str bearer_token)" ] && echo true || echo false)"
    printf '  "last_push": %s\n' "$LAST"
    printf '}\n'
}

cmd_data() {
    S=${1:-all}
    case "$S" in
        all|device|props|battery|power|display|cpu|gpu|memory|storage|thermal|network|system|processes|packages) ;;
        network/wifi) ;;
        *) printf '{"error":"unknown section: %s"}' "$(jesc "$S")"; return 1 ;;
    esac
    [ -n "$(daemon_pid)" ] || {
        printf '{"error":"service is not running","hint":"press Start, or run: pkill -f bin/phoneserver"}'
        return 1
    }
    OUT=$(api "$S" 2>/dev/null)
    [ -n "$OUT" ] || { printf '{"error":"no answer from the API on port %s"}' "$(port)"; return 1; }
    printf '%s' "$OUT"
}

cmd_snapshot() {
    printf '{"status":%s,"data":%s}\n' "$(cmd_status)" "$(cmd_data "${1:-all}" 2>/dev/null || printf '{"error":"unavailable"}')"
}

cmd_restart() {
    PID=$(daemon_pid)
    if [ -z "$PID" ]; then
        cmd_start
        return $?
    fi
    kill -9 "$PID" 2>/dev/null
    if wait_health "$(port)" 16; then
        printf '{"ok":true,"action":"restarted","killed_pid":%s,"confirmed":true}\n' "$PID"
    else
        printf '{"ok":false,"action":"restarted","killed_pid":%s,"error":"service did not answer within 16s; check logs"}\n' "$PID"
        return 1
    fi
}

cmd_stop() {
    WPID=$(watchdog_pid)
    [ -n "$WPID" ] && kill "$WPID" 2>/dev/null
    PID=$(daemon_pid)
    [ -n "$PID" ] && kill "$PID" 2>/dev/null
    rm -f "$DATA/watchdog.pid"
    sleep 1
    if [ -n "$(daemon_pid)" ] || [ -n "$(watchdog_pid)" ]; then
        printf '{"ok":false,"error":"a process survived the stop; retry"}\n'
        return 1
    fi
    printf '{"ok":true,"action":"stopped"}\n'
}

cmd_start() {
    if [ -n "$(daemon_pid)" ]; then
        printf '{"ok":true,"action":"already-running","pid":%s}\n' "$(daemon_pid)"
        return 0
    fi
    [ -f "$CFG" ] || "$BIN" -init-config -config "$CFG" >/dev/null 2>&1
    setsid "$MODDIR/watchdog.sh" </dev/null >>"$DATA/logs/stdout.log" 2>&1 &
    if wait_health "$(port)" 20; then
        printf '{"ok":true,"action":"started","confirmed":true}\n'
    else
        printf '{"ok":false,"action":"started","error":"started but no answer within 20s; check logs"}\n'
        return 1
    fi
}

cmd_push_now() {
    [ -n "$(daemon_pid)" ] || { printf '{"ok":false,"error":"service is not running"}'; return 1; }
    RES=$(api_call POST "" "$(port)")
    BODY=$(body_of "$RES"); CODE=$(code_of "$RES")
    [ -n "$BODY" ] || { printf '{"ok":false,"error":"no answer from the API"}'; return 1; }
    printf '%s\n' "$BODY"
    case "$CODE" in 2*) return 0 ;; *) return 1 ;; esac
}

cmd_set_push() {
    NEW_URL=${1:-}
    NEW_IV=${2:-60}
    NEW_BEARER=${3:-}
    NEW_EN=${4:-0}
    case "$NEW_IV" in ''|*[!0-9]*) NEW_IV=60 ;; esac
    [ "$NEW_IV" -lt 10 ] && NEW_IV=10
    case "$NEW_EN" in 1|true) EN=true ;; *) EN=false ;; esac
    if [ "$EN" = "true" ]; then
        case "$NEW_URL" in
            http://*|https://*) ;;
            *) printf '{"ok":false,"error":"url must start with http:// or https://"}'; return 1 ;;
        esac
    fi
    BODY=$(printf '{"push":{"enabled":%s,"url":"%s","interval_seconds":%s,"bearer_token":"%s"}}' \
        "$EN" "$(jesc "$NEW_URL")" "$NEW_IV" "$(jesc "$NEW_BEARER")")
    RES=$(api_call PUT "$BODY" "$(port)")
    CODE=$(code_of "$RES")
    if [ "$CODE" != "200" ]; then
        fail_call "$RES"
        return 1
    fi
    printf '%s\n' "$(body_of "$RES")"
}

cmd_set_port() {
    NEW_PORT=${1:-}
    case "$NEW_PORT" in ''|*[!0-9]*) printf '{"ok":false,"error":"port must be a number"}'; return 1 ;; esac
    if [ "$NEW_PORT" -lt 1024 ] || [ "$NEW_PORT" -gt 65535 ]; then
        printf '{"ok":false,"error":"port must be between 1024 and 65535"}'; return 1
    fi
    CUR=$(port)
    if [ "$NEW_PORT" = "$CUR" ]; then
        printf '{"ok":true,"listen":"0.0.0.0:%s","detail":"already using this port"}' "$NEW_PORT"
        return 0
    fi
    if ss -lnt 2>/dev/null | grep -q ":$NEW_PORT "; then
        printf '{"ok":false,"error":"port %s is already in use by another process"}' "$NEW_PORT"
        return 1
    fi
    BODY=$(printf '{"listen":"0.0.0.0:%s"}' "$NEW_PORT")
    RES=$(api_call PUT "$BODY" "$CUR")
    CODE=$(code_of "$RES")
    if [ "$CODE" != "200" ]; then
        fail_call "$RES"
        return 1
    fi
    PID=$(daemon_pid)
    [ -n "$PID" ] && kill -9 "$PID" 2>/dev/null
    if wait_health "$NEW_PORT" 20; then
        printf '{"ok":true,"listen":"0.0.0.0:%s","confirmed":true}\n' "$NEW_PORT"
    else
        printf '{"ok":false,"listen":"0.0.0.0:%s","error":"saved but the service has not answered on the new port after 20s"}\n' "$NEW_PORT"
        return 1
    fi
}

cmd_logs() {
    N=${1:-40}
    case "$N" in ''|*[!0-9]*) N=40 ;; esac
    tail -n "$N" "$DATA/logs/stdout.log" 2>/dev/null || echo "(no stdout.log yet)"
}

cmd_api_log() {
    N=${1:-40}
    case "$N" in ''|*[!0-9]*) N=40 ;; esac
    tail -n "$N" "$DATA/logs/phoneserver.log" 2>/dev/null || echo "(no phoneserver.log yet)"
}

cmd_clear_logs() {
    : > "$DATA/logs/stdout.log" 2>/dev/null
    : > "$DATA/logs/phoneserver.log" 2>/dev/null
    printf '{"ok":true,"action":"logs-cleared"}\n'
}

# ---- history (SQLite-backed) ----
# history-stats                    db size, row counts, growth, battery-cost estimate
# history [metrics] [since] [n]    sample series, e.g. "battery_temp_c,cpu_usage_pct" -6h 240
# history-logs [n] [grep] [since]  stored log lines
# history-snapshot [ts] [section]  the full stored document nearest ts
# history-prune                    run the retention policy now
# history-panel [metric] [since]   {stats,series,logs} in one call (what the UI polls)
hist_api() {  # hist_api <path-with-query>
    [ -n "$(daemon_pid)" ] || { printf '{"ok":false,"error":"service is not running"}'; return 1; }
    OUT=$(api "$1" 2>/dev/null)
    if [ -z "$OUT" ]; then
        printf '{"ok":false,"error":"no answer from the API on port %s"}' "$(port)"
        return 1
    fi
    printf '%s' "$OUT"
}

cmd_history_stats() { hist_api "history/stats" ; }
cmd_history_metrics() { hist_api "history/metrics" ; }

cmd_history() {
    M=${1:-}
    S=${2:--1h}
    L=${3:-360}
    case "$L" in ''|*[!0-9]*) L=360 ;; esac
    Q="since=$(jesc "$S")&limit=$L&stats=1"
    [ -n "$M" ] && Q="metrics=$(jesc "$M")&$Q"
    hist_api "history?$Q"
}

cmd_history_logs() {
    N=${1:-50}
    G=${2:-}
    S=${3:--6h}
    case "$N" in ''|*[!0-9]*) N=50 ;; esac
    Q="since=$(jesc "$S")&limit=$N"
    [ -n "$G" ] && Q="$Q&grep=$(jesc "$G")"
    hist_api "history/logs?$Q"
}

cmd_history_snapshot() {
    TS=${1:-}
    SEC=${2:-}
    Q=""
    [ -n "$TS" ] && Q="ts=$(jesc "$TS")"
    [ -n "$SEC" ] && Q="${Q:+$Q&}section=$(jesc "$SEC")"
    hist_api "history/snapshot${Q:+?$Q}"
}

cmd_history_prune() {
    [ -n "$(daemon_pid)" ] || { printf '{"ok":false,"error":"service is not running"}'; return 1; }
    OUT=$(curl -s --max-time 15 -w '\nHTTP%{http_code}' -X POST \
        -H "X-Api-Key: $(token)" "http://$(api_host):$(port)/api/v1/history/prune")
    C=$(code_of "$OUT")
    B=$(body_of "$OUT")
    if [ -z "$B" ]; then
        printf '{"ok":false,"error":"no answer from the API on port %s"}' "$(port)"
        return 1
    fi
    printf '%s\n' "$B"
    case "$C" in 2*) return 0 ;; *) return 1 ;; esac
}

cmd_history_panel() {
    M=${1:-battery_temp_c}
    S=${2:--6h}
    L=${3:-240}
    case "$L" in ''|*[!0-9]*) L=240 ;; esac
    [ -n "$(daemon_pid)" ] || {
        printf '{"ok":false,"error":"service is not running"}'
        return 1
    }
    ST=$(api history/stats 2>/dev/null)
    SE=$(api "history?metrics=$(jesc "$M")&since=$(jesc "$S")&limit=$L&stats=1" 2>/dev/null)
    LG=$(api "history/logs?since=-1h&limit=15" 2>/dev/null)
    printf '{"ok":true,"stats":%s,"series":%s,"logs":%s}\n' \
        "${ST:-null}" "${SE:-null}" "${LG:-null}"
}

SUB=${1:-status}
shift 2>/dev/null || true
case "$SUB" in
    snapshot)   cmd_snapshot "$@" ;;
    status)     cmd_status ;;
    data)       cmd_data "$@" ;;
    restart)    cmd_restart ;;
    start)      cmd_start ;;
    stop)       cmd_stop ;;
    push-now)   cmd_push_now ;;
    set-push)   cmd_set_push "$@" ;;
    set-port)   cmd_set_port "$@" ;;
    logs)       cmd_logs "$@" ;;
    api-log)    cmd_api_log "$@" ;;
    clear-logs) cmd_clear_logs ;;
    history)    cmd_history "$@" ;;
    history-stats)    cmd_history_stats ;;
    history-metrics)  cmd_history_metrics ;;
    history-logs)     cmd_history_logs "$@" ;;
    history-snapshot) cmd_history_snapshot "$@" ;;
    history-prune)    cmd_history_prune ;;
    history-panel)    cmd_history_panel "$@" ;;
    token)      token; echo ;;
    *)          printf '{"error":"unknown subcommand: %s"}' "$(jesc "$SUB")"; exit 1 ;;
esac