#!/system/bin/sh
# KernelSU / Magisk "Action" button: print status, address and API token.
MODDIR=${0%/*}
DATA=/data/adb/phoneserver
BIN="$MODDIR/bin/phoneserver"
CFG="$DATA/config.json"

port_from_cfg() {
    sed -n 's/.*"listen"[[:space:]]*:[[:space:]]*"[^"]*:\([0-9]*\)".*/\1/p' "$CFG" 2>/dev/null | head -1
}
token_from_cfg() {
    sed -n 's/.*"api_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CFG" 2>/dev/null | head -1
}

PORT=$(port_from_cfg)
[ -n "$PORT" ] || PORT=8080
TOKEN=$(token_from_cfg)
IP=$(ip -4 addr show wlan0 2>/dev/null | sed -n 's/.*inet \([0-9.]*\).*/\1/p' | head -1)

echo "$(sed -n 's/^name=//p' "$MODDIR/module.prop" 2>/dev/null | head -1) $(sed -n 's/^version=//p' "$MODDIR/module.prop" 2>/dev/null | head -1)"
echo "======================="
PID=$(pgrep -f "$BIN" 2>/dev/null | head -1)
if [ -n "$PID" ]; then
    echo "service  : RUNNING (pid $PID)"
else
    echo "service  : STOPPED"
fi
echo "address  : http://${IP:-<phone-ip>}:$PORT"
echo "local    : http://127.0.0.1:$PORT"
if [ -n "$TOKEN" ]; then
    echo "token    : $TOKEN"
else
    echo "token    : (none — API is open to anyone who can reach the port)"
fi

if [ -f "$CFG" ]; then
    PUSH=$(sed -n '/"push"/,/}/p' "$CFG" 2>/dev/null | grep -o '"url"[^,]*' | head -1)
    echo "push     : ${PUSH:-disabled}"
fi

if command -v curl >/dev/null 2>&1; then
    H=$(curl -s --max-time 3 "http://127.0.0.1:$PORT/api/v1/health" 2>/dev/null)
    [ -n "$H" ] && echo "health   : $H"
fi

echo
echo "restart  : pkill -f bin/phoneserver   (the watchdog restarts it)"
echo "dashboard: open the address above in a browser"