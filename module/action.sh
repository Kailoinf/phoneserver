#!/system/bin/sh
# KernelSU / Magisk "Action" button: print status, address, token and the two
# services that need a decision (file shares, bridge) — a local read-only screen.
MODDIR=${0%/*}
DATA=/data/adb/phoneserver
BIN="$MODDIR/bin/phoneserver"
CFG="$DATA/config.json"

cfg_str() {
    sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$CFG" 2>/dev/null | head -1
}
cfg_bool() {
    sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\(true\|false\).*/\1/p" "$CFG" 2>/dev/null | head -1
}
json_num() {  # json_num <json> <key>
    printf '%s' "$1" | tr -d '\n ' | sed -n "s/.*\"$2\":\([0-9]*\).*/\1/p"
}
json_str() {  # json_str <json> <key>
    printf '%s' "$1" | tr -d '\n ' | sed -n "s/.*\"$2\":\"\([^\"]*\)\".*/\1/p"
}

LISTEN=$(cfg_str listen)
[ -n "$LISTEN" ] || LISTEN="127.0.0.1:8080"
PORT=${LISTEN##*:}
case "$PORT" in ''|*[!0-9]*) PORT=8080 ;; esac
HOST=${LISTEN%%:*}
case "$HOST" in ''|0.0.0.0|'*'|'[::]'|::) HOST=127.0.0.1 ;; esac

ALLOW_LAN=$(cfg_bool allow_non_loopback_listen)
[ -n "$ALLOW_LAN" ] || ALLOW_LAN=false
IP=$(ip -4 addr show wlan0 2>/dev/null | sed -n 's/.*inet \([0-9.]*\).*/\1/p' | head -1)
TOKEN=$(cfg_str api_token)

echo "$(sed -n 's/^name=//p' "$MODDIR/module.prop" 2>/dev/null | head -1) $(sed -n 's/^version=//p' "$MODDIR/module.prop" 2>/dev/null | head -1)"
echo "======================="
PID=$(pgrep -f "$BIN" 2>/dev/null | head -1)
if [ -n "$PID" ]; then
    echo "service  : RUNNING (pid $PID)"
else
    echo "service  : STOPPED"
fi
echo "listen   : $LISTEN"

# Print only the address that is really reachable: loopback unless the user
# turned the LAN switch on.
if [ "$ALLOW_LAN" = "true" ] && [ "$HOST" != "127.0.0.1" ] && [ "$HOST" != "localhost" ]; then
    echo "lan addr : http://${HOST:-${IP:-<phone-ip>}}:$PORT"
    echo "           (局域网访问已打开：能路由到该端口的人都算进来，token 是唯一凭据)"
else
    echo "local    : http://127.0.0.1:$PORT"
    echo "           (只监听本机；要局域网访问请在 WebUI 配置页打开「允许局域网访问」)"
fi

if [ -n "$TOKEN" ]; then
    echo "token    : $TOKEN"
else
    echo "token    : (none — API 对能连上该端口的人完全开放)"
fi

if command -v curl >/dev/null 2>&1; then
    H=$(curl -s --max-time 4 "http://127.0.0.1:$PORT/api/v1/health" 2>/dev/null)
    if [ -n "$H" ]; then
        echo "health   : $H"
        SH=$(printf '%s' "$H" | tr -d '\n ' | sed -n 's/.*"file_shares":\[\([^]]*\)\].*/\1/p')
        [ -n "$SH" ] && echo "shares   : $SH"
    fi

    # Termux:API bridge: the camera and microphone depend on it, so its state
    # belongs on this screen instead of only inside the WebUI.
    BR=$(curl -s --max-time 8 -H "X-API-Key: $TOKEN" "http://127.0.0.1:$PORT/api/v1/bridge/status" 2>/dev/null)
    if [ -n "$BR" ]; then
        AVAIL=$(json_str "$BR" available)
        [ -n "$AVAIL" ] || AVAIL=false
        echo "bridge   : available=$AVAIL uid=$(json_num "$BR" termux_uid) state=$(json_str "$BR" uid_state)"
    fi

    ST=$(curl -s --max-time 8 -H "X-API-Key: $TOKEN" "http://127.0.0.1:$PORT/api/v1/stream/status" 2>/dev/null)
    [ -n "$ST" ] && echo "stream   : $(json_num "$ST" clients) active client(s)"
fi

echo
echo "restart  : pkill -f bin/phoneserver   (the watchdog restarts it)"
echo "webui    : open the module card in the manager and press WebUI"
echo "browser  : http://127.0.0.1:$PORT/ui/   (paste the token in the page)"
echo "files    : http://127.0.0.1:$PORT/files/   ·   WebDAV: http://127.0.0.1:$PORT/dav/<share>"
echo "mcp      : $BIN -mcp -config $CFG   (stdio)  ·  POST http://127.0.0.1:$PORT/mcp"