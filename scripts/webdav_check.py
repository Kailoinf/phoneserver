#!/usr/bin/env python3
"""WebDAV and file-service checks against a running phoneserver."""
import hashlib
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request
import xml.etree.ElementTree as ET

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8080"
TOK = sys.argv[2] if len(sys.argv) > 2 else ""
NS = "{DAV:}"


def hdr(headers, name):
    """Header lookup that does not care about case (Go sends Dav, not DAV)."""
    for k, v in headers.items():
        if k.lower() == name.lower():
            return v
    return None


def req(method, path, data=None, headers=None, auth=True, timeout=30):
    url = BASE + path
    h = {"X-API-Key": TOK} if auth and TOK else {}
    h.update(headers or {})
    r = urllib.request.Request(url, data=data, method=method)
    for k, v in h.items():
        r.add_header(k, v)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            return resp.status, resp.read(), dict(resp.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read(), dict(e.headers)


def show(name, ok, detail):
    print(("PASS  " if ok else "FAIL  ") + name.ljust(38) + detail)


def main():
    # OPTIONS must reach the WebDAV handler and advertise DAV compliance.
    st, body, hdrs = req("OPTIONS", "/dav/tmp/")
    print("OPTIONS status=%s DAV=%r Allow=%r" % (st, hdr(hdrs, "DAV"), hdr(hdrs, "Allow")))
    show("webdav OPTIONS", st == 200 and "1" in (hdr(hdrs, "DAV") or ""), "status=%s dav=%r" % (st, hdr(hdrs, "DAV")))

    # PROPFIND depth 1 and the XML must parse with the DAV namespace.
    st, body, _ = req("PROPFIND", "/dav/tmp/", headers={"Depth": "1"})
    try:
        root = ET.fromstring(body)
        hrefs = [h.text for h in root.iter(NS + "href")]
        parsed = True
    except Exception as e:  # noqa: BLE001
        hrefs, parsed = [], False
        print("propfind parse error:", e)
    show("webdav PROPFIND depth1", st == 207 and parsed and len(hrefs) >= 1,
         "status=%s resources=%d %s" % (st, len(hrefs), hrefs[:3]))

    # Upload via WebDAV, read back through the HTTP file view, compare hashes.
    payload = os.urandom(200000)
    st, _, _ = req("PUT", "/dav/tmp/hello.bin", data=payload)
    st2, got, _ = req("GET", "/files/tmp/hello.bin")
    same = hashlib.sha256(payload).hexdigest() == hashlib.sha256(got).hexdigest()
    show("PUT then GET round-trip", st in (200, 201, 204) and same,
         "put=%s get=%s bytes=%d sha_match=%s" % (st, st2, len(got), same))

    # Range request.
    st, part, hdrs = req("GET", "/files/tmp/hello.bin", headers={"Range": "bytes=0-99"})
    show("Range 0-99", st == 206 and len(part) == 100,
         "status=%s bytes=%d content-range=%s" % (st, len(part), hdrs.get("Content-Range")))

    # HEAD must not return a body.
    st, body, hdrs = req("HEAD", "/files/tmp/hello.bin")
    show("HEAD", st == 200 and len(body) == 0,
         "status=%s body=%d len=%s" % (st, len(body), hdrs.get("Content-Length")))

    # MKCOL, MOVE, then DELETE while deletion is still disabled.
    st, _, _ = req("MKCOL", "/dav/tmp/d1")
    show("MKCOL", st in (201, 405), "status=%s" % st)
    st, _, _ = req("MOVE", "/dav/tmp/hello.bin", headers={"Destination": "/dav/tmp/d1/hello2.bin"})
    show("MOVE into new dir", st in (201, 204), "status=%s" % st)
    st, body, _ = req("DELETE", "/dav/tmp/d1/hello2.bin")
    show("DELETE refused (allow_delete=false)", st == 403, "status=%s body=%s" % (st, body[:80]))

    # Turn deletion on, delete for real, then turn it back off.
    st, _, _ = req("PUT", "/api/v1/config", data=json.dumps({"files": {"allow_delete": True}}).encode(),
                   headers={"Content-Type": "application/json"})
    st2, _, _ = req("DELETE", "/files/tmp/d1/hello2.bin")
    st3, _, _ = req("PUT", "/api/v1/config", data=json.dumps({"files": {"allow_delete": False}}).encode(),
                    headers={"Content-Type": "application/json"})
    show("DELETE works once allow_delete=true", st2 in (200, 204),
         "config=%s delete=%s restore=%s" % (st, st2, st3))

    # Directory listing in both shapes.
    st, body, _ = req("GET", "/files/tmp/?json=1")
    ok = st == 200
    try:
        listing = json.loads(body)
        ok = ok and "entries" in listing
    except Exception:  # noqa: BLE001
        listing = {}
    show("files listing (?json=1)", ok, "status=%s entries=%s" % (st, len(listing.get("entries", []))))

    # Traversal attempts: none may succeed, and none may leak the config.
    attempts = [
        "/files/tmp/../../../../data/adb/phoneserver/config.json",
        "/files/tmp/%2e%2e%2f%2e%2e%2f%2e%2e%2fdata%2fadb%2fphoneserver%2fconfig.json",
        "/files/tmp/..%2f..%2fdata%2fadb%2fphoneserver%2fconfig.json",
    ]
    leaked = False
    codes = []
    for a in attempts:
        st, body, _ = req("GET", a, auth=True)
        codes.append(st)
        if b"api_token" in body:
            leaked = True
    show("traversal GET refused, no leak",
         all(c in (400, 403, 404) for c in codes) and not leaked, "codes=%s leaked=%s" % (codes, leaked))

    # A traversal in a *PUT* target: Go's ServeMux cleans the path and answers a
    # redirect, so what matters is where the request finally lands and that it
    # never creates anything outside the share. curl is used here because it
    # follows the 307 with the same method (a plain urllib PUT does not).
    out = subprocess.run(
        ["curl", "-s", "-L", "-o", "/dev/null", "-w", "%{http_code}", "-X", "PUT",
         "-H", "X-API-Key: " + TOK, "--data-binary", "x",
         BASE + "/files/tmp/../../data/adb/phoneserver/evil.txt"],
        capture_output=True, text=True, timeout=30).stdout.strip()
    show("traversal PUT lands inside the share only", out in ("400", "403", "404", "409"),
         "final status=%s" % out)

    # Basic auth is what WebDAV clients use; the username must be ignored.
    st, _, _ = req("GET", "/files/tmp/?json=1", auth=False, headers={"Authorization": "Basic " + __import__("base64").b64encode(("any:" + TOK).encode()).decode()})
    show("HTTP Basic auth accepted", st == 200, "status=%s" % st)
    st, _, _ = req("GET", "/files/tmp/?json=1", auth=False, headers={"Authorization": "Basic " + __import__("base64").b64encode(b"any:wrong").decode()})
    show("wrong Basic password refused", st == 401, "status=%s" % st)


main()