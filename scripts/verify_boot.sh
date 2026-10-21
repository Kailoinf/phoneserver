#!/bin/sh
# scripts/verify_boot.sh <serial> [expected-version] [binary]
#
# The acceptance test that matters for a module: after a real reboot the daemon
# must come up by itself, listen only where it is configured to, and be brought
# back by the watchdog if it dies. Run it from the host, after `adb reboot`
# completed. It reports and does not fix anything, except one deliberate kill.
#
#   sh scripts/verify_boot.sh 192.168.5.29:33813 2.0.0 module/bin/phoneserver
#
# Every remote command is passed to `adb shell` as ONE argument. Splitting it
# into argv words (`adb shell sh -c "cmd"`) makes the device shell re-parse the
# pieces, `sh -c` ends up with a single word, and reads silently come back empty.
set -u

SERIAL=${1:?usage: verify_boot.sh <serial> [expected-version] [binary]}
WANT=${2:-}
BIN=${3:-module/bin/phoneserver}
MOD=/data/adb/modules/phoneserver
DATA=/data/adb/phoneserver
A="adb -s $SERIAL"
pass=0
fail=0
ok()  { pass=$((pass+1)); printf 'PASS  %-44s %s\n' "$1" "$2"; }
bad() { fail=$((fail+1)); printf 'FAIL  %-44s %s\n' "$1" "$2"; }
# dev <single full command string>
dev() { $A shell "$1" 2>/dev/null | tr -d '\r'; }

echo "== boot acceptance: $SERIAL"

# 1. the device really booted *now* (sys.boot_completed alone can still be the
#    previous boot's value if you poll too early)
i=0
while [ "$(dev 'getprop sys.boot_completed')" != "1" ] && [ $i -lt 90 ]; do sleep 2; i=$((i+1)); done
up=$(dev 'cut -d" " -f1 /proc/uptime')
if [ -z "$up" ]; then
    bad "device answers adb" "no /proc/uptime"
elif awk "BEGIN{exit !($up < 900)}"; then
    ok "device rebooted, uptime" "${up%.*}s"
else
    bad "device uptime" "${up%.*}s — it did not reboot (or you are looking at the old boot)"
fi

# 2. the module has been activated (moved out of modules_update)
[ "$(dev "[ -d $MOD ] && echo yes")" = "yes" ] && ok "module dir present" "$MOD" || bad "module dir present" "missing $MOD"
[ "$(dev "[ -f $MOD/update ] && echo yes")" = "yes" ] && bad "module activated" "still staged (modules_update marker present)" || ok "module activated" "no update marker"
[ "$(dev "[ -f $MOD/disable ] && echo yes")" = "yes" ] && bad "module enabled" "disable marker present" || ok "module enabled" ""

# 3. the binary is the one that was built, and executable
if [ -f "$BIN" ]; then
    want_sum=$(sha256sum "$BIN" | cut -d' ' -f1)
    got_sum=$(dev "sha256sum $MOD/bin/phoneserver" | cut -d' ' -f1)
    if [ -z "$got_sum" ]; then
        printf 'SKIP  %-44s %s\n' "binary matches the build" "no sha256sum on the device"
    elif [ "$want_sum" = "$got_sum" ]; then
        ok "binary matches the build" "sha256 ${want_sum%${want_sum#??????}}…"
    else
        bad "binary matches the build" "device ${got_sum%${got_sum#??????}}… ≠ local ${want_sum%${want_sum#??????}}…"
    fi
fi
mode=$(dev "stat -c '%a' $MOD/bin/phoneserver")
case "$mode" in
    755|0755) ok "binary mode" "$mode" ;;
    *) bad "binary mode" "${mode:-unknown}, want 755" ;;
esac

# 4. boot started it: one daemon, one watchdog, nothing manual
pidof() { dev "ps -A -o PID,ARGS | grep -v grep | grep -F '$DATA/config.json' | awk '{print \$1}' | tr '\n' ' '"; }
wdof()  { dev "ps -A -o PID,ARGS | grep -v grep | grep -c watchdog.sh"; }
pids=$(pidof)
n=$(echo "$pids" | wc -w | tr -d ' ')
[ "$n" = "1" ] && ok "one daemon running after boot" "pid ${pids% }" || bad "exactly one daemon" "${n:-0} matched (pids: ${pids:-none})"
[ "$(wdof | tr -d ' ')" = "1" ] && ok "watchdog running" "" || bad "watchdog running" "$(wdof)"

# 5. the *running* binary reports the expected version
if [ -n "$WANT" ]; then
    v=$(dev "$MOD/bin/phoneserver -version")
    case "$v" in *"$WANT"*) ok "running binary version" "$v" ;; *) bad "running binary version" "${v:-empty}, want $WANT" ;; esac
fi

# 6. it listens where the config says — and the default is loopback only
listen=$(dev "sed -n 's/.*\"listen\": *\"\([^\"]*\)\".*/\1/p' $DATA/config.json | head -1")
lan=$(dev "sed -n 's/.*\"allow_non_loopback_listen\": *\([a-z]*\).*/\1/p' $DATA/config.json | head -1")
port=${listen##*:}
bound=""
if [ -n "$port" ]; then
    bound=$(dev "ss -lnt | grep -F ':$port' | tr -s ' ' | cut -d' ' -f4 | tr '\n' ' '")
fi
case "$listen" in
    127.0.0.1:*|"")
        case " ${bound} " in
            *" $listen "*) ok "bound to loopback only" "${bound% }" ;;
            *) bad "bound to loopback only" "config=$listen bound=${bound:-nothing}" ;;
        esac ;;
    *)
        if [ "$lan" = "true" ]; then
            ok "bound to a non-loopback address (switch on)" "${bound% }"
        elif [ "$bound" = "127.0.0.1:$port " ]; then
            ok "non-loopback listen gated back to loopback" "${bound% }"
        else
            bad "non-loopback listen gate" "config=$listen allow=${lan:-?} bound=${bound:-nothing}"
        fi ;;
esac

# 7. it answers, from the device itself, and refuses without a token
tok=$(dev "sed -n 's/.*\"api_token\": *\"\([^\"]*\)\".*/\1/p' $DATA/config.json | head -1")
code=$(dev "curl -s -o /dev/null -w '%{http_code}' -H 'X-API-Key: $tok' http://127.0.0.1:$port/api/v1/health")
[ "$code" = "200" ] && ok "health after boot" "200" || bad "health after boot" "${code:-no answer}"
code=$(dev "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$port/api/v1/all")
[ "$code" = "401" ] && ok "unauthenticated call refused" "401" || bad "unauthenticated call refused" "${code:-no answer}"

# 8. the watchdog really restarts it after a kill
before=$(pidof)
[ -n "${before% }" ] || { bad "watchdog restart test" "no pid to kill"; echo; echo "== $pass passed, $fail failed"; exit 1; }
dev "kill -9 ${before% }" >/dev/null 2>&1
sleep 13
after=$(pidof)
if [ -n "${after% }" ] && [ "${after% }" != "${before% }" ]; then
    ok "watchdog restarted the daemon" "${before% } -> ${after% }"
else
    bad "watchdog restarted the daemon" "before=${before% } after=${after:-none}"
fi
code=$(dev "curl -s -o /dev/null -w '%{http_code}' -H 'X-API-Key: $tok' http://127.0.0.1:$port/api/v1/health")
[ "$code" = "200" ] && ok "health after the forced kill" "200" || bad "health after the forced kill" "${code:-no answer}"

echo
echo "== $pass passed, $fail failed"
[ "$fail" = "0" ]
