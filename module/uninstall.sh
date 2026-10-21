#!/system/bin/sh
# Stop everything when the module is removed. /data/adb/phoneserver is left in
# place on purpose: a reinstall then keeps the same API token, config and the
# history database.
MODDIR=${0%/*}
DATA=/data/adb/phoneserver
BIN="$MODDIR/bin/phoneserver"

[ -f "$DATA/watchdog.pid" ] && kill "$(cat "$DATA/watchdog.pid")" 2>/dev/null
pkill -f "$BIN" 2>/dev/null
pkill -f "$MODDIR/watchdog.sh" 2>/dev/null
rm -f "$DATA/watchdog.pid"
echo "phoneserver stopped; config and logs kept in $DATA"