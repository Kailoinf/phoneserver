#!/system/bin/sh
# Keeps phoneserver alive. Started detached by service.sh so that it outlives
# the boot script; killing the server on purpose is therefore harmless — the
# loop brings it straight back, which doubles as the "restart" mechanism.
MODDIR=${0%/*}
DATA=/data/adb/phoneserver
BIN="$MODDIR/bin/phoneserver"

mkdir -p "$DATA/logs"
echo $$ > "$DATA/watchdog.pid"

while true; do
    "$BIN" -config "$DATA/config.json" >>"$DATA/logs/stdout.log" 2>&1
    rc=$?
    echo "$(date '+%Y-%m-%d %H:%M:%S') phoneserver exited rc=$rc; restarting in 10s" >>"$DATA/logs/stdout.log"
    sleep 10
done