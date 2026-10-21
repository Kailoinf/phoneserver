#!/system/bin/sh
# Boot entry point. Magisk runs this at late_start service; KernelSU runs it in
# its service stage. Both run it as root, asynchronously.
MODDIR=${0%/*}
DATA=/data/adb/phoneserver

mkdir -p "$DATA/logs"
chmod 0700 "$DATA" "$DATA/logs" 2>/dev/null
# Module scripts are invoked through `sh` everywhere, but restore the exec bit
# anyway so they can also be run directly (the installer may reset modes).
chmod 0755 "$MODDIR/bin/phoneserver" "$MODDIR/watchdog.sh" "$MODDIR/service.sh" \
    "$MODDIR/action.sh" "$MODDIR/webui.sh" 2>/dev/null

# `cmd wifi status` and `dumpsys power` only return real values once
# system_server is up, so don't collect anything before boot completes.
i=0
while [ "$(getprop sys.boot_completed)" != "1" ] && [ "$i" -lt 90 ]; do
    sleep 2
    i=$((i + 1))
done

# Make sure a config (and therefore an API token) exists before the server runs.
[ -f "$DATA/config.json" ] || "$MODDIR/bin/phoneserver" -init-config -config "$DATA/config.json" >/dev/null 2>&1

# Kill a leftover instance from a previous boot, then start the watchdog
# detached so it survives this script exiting.
pkill -f "$MODDIR/bin/phoneserver" 2>/dev/null
[ -f "$DATA/watchdog.pid" ] && kill "$(cat "$DATA/watchdog.pid")" 2>/dev/null

if command -v setsid >/dev/null 2>&1; then
    setsid "$MODDIR/watchdog.sh" </dev/null >>"$DATA/logs/stdout.log" 2>&1 &
else
    "$MODDIR/watchdog.sh" </dev/null >>"$DATA/logs/stdout.log" 2>&1 &
fi

exit 0