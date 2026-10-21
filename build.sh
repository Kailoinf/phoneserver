#!/bin/sh
# Build the daemon for Android, build the WebUI, and package the module zip.
#
#   ./build.sh            -> phoneserver-arm64 + phoneserver-module.zip
#
# Requires Go. The cross-compile needs no network (the daemon uses the standard
# library plus three pinned dependencies). The WebUI is a Vite project under
# webui/: it is rebuilt when its node_modules are present, otherwise the bundle
# committed in module/webroot/ ships as-is — the phone never needs node.
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

echo "==> running the test suite"
if ! go test ./... ; then
    echo "FATAL: tests failed — not packaging a build that fails its own tests." >&2
    exit 1
fi

echo "==> building the WebUI (webui/ -> module/webroot)"
if [ -f webui/package.json ]; then
    if [ -d webui/node_modules ]; then
        # `pnpm build` runs its own "verify deps before run" install first, which
        # needs the network: with no internet that step hangs forever and the
        # release never gets built. Disable it, bound the step, and fall back to
        # calling tsc/vite directly (both work offline from a wired node_modules).
        if (cd webui && timeout 240 pnpm --config.verify-deps-before-run=false build); then
            echo "   WebUI rebuilt from webui/ (pnpm)"
        elif (cd webui && timeout 240 ./node_modules/.bin/tsc --noEmit && timeout 240 node node_modules/vite/bin/vite.js build); then
            echo "   WebUI rebuilt from webui/ (direct tsc + vite build)"
        else
            echo "!! WebUI build failed; shipping the committed module/webroot as-is"
        fi
    else
        echo "   webui/node_modules is missing (run: cd webui && pnpm install)"
        echo "   shipping the committed module/webroot as-is"
    fi
fi
if [ -f module/webroot/index.html ]; then
    echo "   webroot: $(find module/webroot -type f | wc -l) file(s), $(du -sh module/webroot | cut -f1)"
else
    echo "!! module/webroot/index.html is missing — /ui/ will not be served" >&2
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
