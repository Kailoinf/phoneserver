#!/system/bin/sh
# Keeps phoneserver alive. Started detached by service.sh so that it outlives the
# boot script; killing the server on purpose is therefore harmless — the loop
# brings it straight back, which doubles as the "restart" mechanism.
#
# Two guards exist here because both were needed on a real device:
#   1. a pid-file lock, so a second watchdog cannot start (two watchdogs each
#      spawn a daemon, the loser's bind fails with EADDRINUSE, and the pair flap
#      every 10 s forever — 15 rc=1 exits were logged before this was added);
#   2. a "is a daemon for this config already alive?" check before spawning, so a
#      watchdog that starts next to a healthy daemon does not create a duplicate.
MODDIR=${0%/*}
DATA=/data/adb/phoneserver
BIN="$MODDIR/bin/phoneserver"
PIDFILE="$DATA/watchdog.pid"
LOG="$DATA/logs/stdout.log"

mkdir -p "$DATA/logs"

if [ -f "$PIDFILE" ]; then
    OTHER=$(cat "$PIDFILE" 2>/dev/null)
    if [ -n "$OTHER" ] && [ "$OTHER" != "$$" ] && kill -0 "$OTHER" 2>/dev/null; then
        echo "$(date '+%F %T') watchdog: another watchdog (pid $OTHER) is alive; exiting" >>"$LOG"
        exit 0
    fi
fi
echo $$ >"$PIDFILE"

daemon_pid() {
    ps -A -o PID,ARGS 2>/dev/null | grep -v grep | grep -F "$DATA/config.json" | awk '{print $1}'
}

while true; do
    if [ -n "$(daemon_pid)" ]; then
        sleep 5
        continue
    fi
    "$BIN" -config "$DATA/config.json" >>"$LOG" 2>&1
    rc=$?
    echo "$(date '+%Y-%m-%d %H:%M:%S') phoneserver exited rc=$rc; restarting in 10s" >>"$LOG"
    # rc=1 is what a lost bind race looks like; backing off longer keeps the log
    # readable and stops two instances from hammering the port in lockstep.
    if [ "$rc" = "1" ]; then
        sleep 30
    else
        sleep 10
    fi
done
