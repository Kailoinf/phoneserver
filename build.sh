#!/bin/sh
# Build the daemon for Android and package it as an installable module zip.
#
#   ./build.sh            -> phoneserver-arm64 + phoneserver-module.zip
#
# Requires Go. Cross-compiles offline: the program uses the standard library
# only, so there are no module downloads.
set -eu
cd "$(dirname "$0")"

OUT=phoneserver-arm64
echo "==> building $OUT (linux/arm64, static)"
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$OUT" .

# A Go PIE build on linux/arm64 carries PT_INTERP=/lib/ld-linux-aarch64.so.1.
# That loader does not exist on Android (it is /system/bin/linker64), so the
# kernel fails the exec with a confusing ENOENT ("No such file or directory")
# and exit 126. Never add -buildmode=pie; assert the binary is static.
if readelf -l "$OUT" 2>/dev/null | grep -q INTERP; then
    echo "FATAL: $OUT has a PT_INTERP segment — Android cannot exec it." >&2
    echo "Remove -buildmode=pie: a static ET_EXEC is what Android needs." >&2
    exit 1
fi

echo "==> staging into the module tree"
mkdir -p module/bin
cp "$OUT" module/bin/phoneserver
chmod 0755 module/bin/phoneserver
# Keep the historical alias in sync too. Two build outputs with different names
# is exactly how a stale binary ends up deployed: the md5 check then compares it
# against the other stale copy and "passes". One canonical artifact, one name.
cp "$OUT" ps-static
chmod 0755 ps-static

cd module
chmod 0755 ./*.sh bin/phoneserver 2>/dev/null || true
rm -f ../phoneserver-module.zip
# Explicit file list so nothing stray (Scratch files, editor backups) is shipped
# and webroot/ keeps the permissions KernelSU needs.
zip -r -X ../phoneserver-module.zip \
    module.prop customize.sh service.sh watchdog.sh action.sh uninstall.sh \
    webui.sh README.md bin webroot META-INF >/dev/null
cd ..

echo "==> done"
ls -l "$OUT" phoneserver-module.zip
sha256sum phoneserver-module.zip
