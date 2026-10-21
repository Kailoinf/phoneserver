#!/system/bin/sh
# Magisk / KernelSU installer hook. Runs once, at install time.
MODPATH=${0%/*}
[ -d "$MODPATH" ] || MODPATH=$PWD

DATA=/data/adb/phoneserver

# Private home for config + logs. Kept outside the module dir on purpose, so
# updating or reinstalling the module never loses the API token or the history
# database.
mkdir -p "$DATA/logs"
chmod 0700 "$DATA" "$DATA/logs" 2>/dev/null

chmod 0755 "$MODPATH" 2>/dev/null
for f in bin/phoneserver service.sh watchdog.sh action.sh uninstall.sh webui.sh; do
    [ -f "$MODPATH/$f" ] && chmod 0755 "$MODPATH/$f"
done
# webroot/ is deliberately left alone: KernelSU sets its permissions and SELinux
# context itself, and overriding them breaks the WebUI.
chmod 0644 "$MODPATH/module.prop" 2>/dev/null

msg="- Phone Control Server installed (linux/arm64, statically linked)"
if command -v ui_print >/dev/null 2>&1; then
    ui_print "$msg"
    ui_print "- Config + logs + history: $DATA"
    ui_print "- Starts on boot; listens on 127.0.0.1:8080 only (loopback)"
    ui_print "- LAN access is opt-in: WebUI > 配置 > 允许局域网访问"
    ui_print "- WebUI: open the module card and press WebUI (multi-page UI)"
    ui_print "- Browser UI: http://127.0.0.1:8080/ui/ (paste the token)"
    ui_print "- MCP for AI agents: bin/phoneserver -mcp -config $DATA/config.json"
    ui_print "- API token: tap Action, or run:"
    ui_print "    grep api_token $DATA/config.json"
else
    echo "$msg"
fi