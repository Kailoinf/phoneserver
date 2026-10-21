#!/bin/sh
# scripts/smoke.sh — end-to-end check of a running 2.0 build.
#
#   ./scripts/smoke.sh http://127.0.0.1:8080 <token> [--with-input] [--with-camera]
#
# Run it from the host against a phone whose LAN access is on, or from the phone
# itself (adb shell 'sh /data/local/tmp/smoke.sh http://127.0.0.1:8080 <token>').
#
# Every check prints PASS/FAIL with the HTTP status it saw. A check that is
# *expected* to be refused (shell without the config switch, WebDAV DELETE
# without allow_delete) is reported as PASS when it is refused — the point is to
# verify that the switch still works, not to make it fail.
#
# Destructive checks are opt-in: --with-input taps the centre of the screen,
# --with-camera takes a real photo. Neither is run by default.
set -u

BASE=${1:-http://127.0.0.1:8080}
TOK=${2:-}
WITH_INPUT=0
WITH_CAMERA=0
for a in "$@"; do
    case "$a" in
        --with-input) WITH_INPUT=1 ;;
        --with-camera) WITH_CAMERA=1 ;;
    esac
done

pass=0
fail=0
LAST=
# 200, as a regex, is not valid in a `case` pattern; use an explicit variable so
# the retry helper below can compare status codes.
WANT_SHOT=200

# Where the response body is buffered. /data/local/tmp exists for the adb shell
# on Android; /tmp is the fallback for a desktop run.
BODYFILE=/data/local/tmp/.phoneserver-smoke.body
[ -d /data/local/tmp ] || BODYFILE=/tmp/.phoneserver-smoke.body

# req <name> <expected-status-regex> <method> <path> [body] [extra-curl-args...]
#
# Shifts are done one at a time on purpose: mksh (Android's /system/bin/sh)
# aborts the whole function when `shift N` exceeds the number of arguments, and
# the single `shift 5` this used to do made every check on the phone print
# nothing at all instead of failing loudly.
req() {
    name=$1; want=$2; method=$3; path=$4
    body=
    i=0
    while [ $i -lt 4 ] && [ $# -gt 0 ]; do shift; i=$((i+1)); done
    # The 5th slot is always the body, even when the caller writes '' and puts
    # curl flags after it: leaving the empty string in "$@" made curl treat it
    # as a URL and fail with an empty response.
    if [ $# -gt 0 ]; then body=$1; shift; fi

    if [ -n "$body" ]; then
        out=$(curl -s -o "$BODYFILE" -w '%{http_code}' -X "$method" \
            -H "X-API-Key: $TOK" -H 'Content-Type: application/json' -d "$body" \
            "$BASE$path" "$@" 2>/dev/null)
    else
        out=$(curl -s -o "$BODYFILE" -w '%{http_code}' -X "$method" -H "X-API-Key: $TOK" \
            "$BASE$path" "$@" 2>/dev/null)
    fi
    LAST=$out
    if [ -z "$out" ]; then
        fail=$((fail+1))
        printf 'ERROR %-42s curl produced nothing (host unreachable?)\n' "$name"
        return
    fi
    case "$out" in
        $want) pass=$((pass+1)); printf 'PASS  %-42s %s\n' "$name" "$out" ;;
        *)     fail=$((fail+1)); printf 'FAIL  %-42s got %s want %s :: %s\n' "$name" "$out" "$want" "$(head -c 200 "$BODYFILE" | tr '\n' ' ')" ;;
    esac
}

echo "== smoke: $BASE (token $([ -n "$TOK" ] && echo set || echo EMPTY)) =="
echo "-- no token must be refused --"
saved=$TOK; TOK=
req "unauth /api/v1/all" 401 GET /api/v1/all
TOK=$saved

echo "-- telemetry --"
req "health"                 200 GET /api/v1/health
req "endpoints"              200 GET /api/v1/endpoints
req "all"                    200 GET /api/v1/all
req "device"                 200 GET /api/v1/device
req "battery"                200 GET /api/v1/battery
req "cpu"                    200 GET /api/v1/cpu
req "memory"                 200 GET /api/v1/memory
req "storage"                200 GET /api/v1/storage
req "thermal"                200 GET /api/v1/thermal
req "network"                200 GET /api/v1/network
req "wifi"                   200 GET /api/v1/network/wifi
req "packages"               200 GET /api/v1/packages
req "processes"              200 GET /api/v1/processes

echo "-- screen --"
# A 429 here means another client (a WebUI page, a second smoke run) is already
# capturing; that is the guard doing its job, so retry once before judging.
shot() {
    i=0
    while [ $i -lt 3 ]; do
        req "screenshot (meta)"      200 GET '/api/v1/screenshot?meta=1&quality=60&scale=0.3'
        [ "$LAST" = "$WANT_SHOT" ] && return
        [ "$LAST" = "429" ] || return
        # Somebody else (a WebUI page, a second smoke run) is capturing; the
        # guard returns 429 on purpose, so wait for them and try again.
        fail=$((fail-1)); i=$((i+1)); sleep 2
    done
    fail=$((fail+1))
    printf 'FAIL  %-42s still 429 after 3 tries\n' "screenshot (meta)"
}
shot
req "stream/display"         200 GET /api/v1/stream/display
req "stream/status"          200 GET /api/v1/stream/status

echo "-- ui and control --"
req "ui/current"             200 GET /api/v1/ui/current
req "ui/dump"                200 GET '/api/v1/ui/dump'
req "apps (grep)"            200 GET '/api/v1/apps?grep=termux&limit=5'
req "apps/current"           200 GET /api/v1/apps/current
req "control/audio"          200 GET /api/v1/control/audio
req "input/key HOME"         200 POST /api/v1/input/key '{"key":"HOME"}'
if [ "$WITH_INPUT" = "1" ]; then
    req "input/tap centre"   200 POST /api/v1/input/tap '{"x":640,"y":1386}'
else
    echo "SKIP  input/tap (add --with-input)"
fi

echo "-- messages and bridge --"
req "notifications"          200 GET '/api/v1/notifications?limit=10'
req "messages sms"           200 GET '/api/v1/messages/list?source=sms&limit=5'
req "messages calls"         200 GET '/api/v1/messages/list?source=calls&limit=5'
req "bridge/status"          200 GET /api/v1/bridge/status

echo "-- files and webdav --"
req "files index"            200 GET /files/
req "files list json"        200 GET '/files/sdcard/Download/?json=1'
req "webdav OPTIONS"         200 OPTIONS '/dav/sdcard/' '' -H 'Depth: 0'
req "webdav PROPFIND"        207 PROPFIND '/dav/sdcard/' '' -H 'Depth: 0'

echo "-- camera and microphone --"
req "camera/info"            200 GET /api/v1/camera/info
if [ "$WITH_CAMERA" = "1" ]; then
    req "camera/photo"       200 GET '/api/v1/camera/photo?meta=1&format=webp&maxwidth=640&quality=60'
else
    echo "SKIP  camera/photo (add --with-camera)"
fi
req "mic/status"             200 GET /api/v1/mic/status

echo "-- history and config --"
req "history"                200 GET '/api/v1/history?metrics=battery_temp_c&since=-1h&limit=5'
req "history/stats"          200 GET /api/v1/history/stats
req "history/quality"        200 GET '/api/v1/history/quality?since=-24h'
req "history/gaps"           200 GET '/api/v1/history/gaps?since=-24h'
req "config (redacted)"      200 GET /api/v1/config

echo "-- gated endpoints stay gated --"
req "shell refused by default" 403 POST /api/v1/shell '{"cmd":"id"}'
req "reboot refused by default" 403 POST /api/v1/device/reboot '{"mode":"normal"}'

echo "-- mcp over http --"
req "mcp initialize"         200 POST /mcp '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"1"}}}'
req "mcp tools/list"         200 POST /mcp '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
req "mcp rejects GET"        405 GET /mcp

echo "-- webui bundle --"
req "dashboard /"            200 GET /
req "/ui/ (200 once webroot is built)" 200 GET /ui/

echo
echo "== $pass passed, $fail failed =="
rm -f "$BODYFILE"
[ "$fail" = "0" ]