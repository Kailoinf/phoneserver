/**
 * api.ts — the one place that knows how the WebUI talks to the phone.
 *
 * Two modes, decided at boot and switchable by the user:
 *
 *   ksu  — the page is inside the KernelSU/SukiSU manager WebView, so
 *          `window.ksu.exec` exists and every shell action is one
 *          `sh <module>/webui.sh <subcommand>` call. `status` hands back the
 *          listen port and the api token, which is what the few actions that
 *          cannot go through a shell (the H.264 stream, an upload, a
 *          screenshot <img>) need to talk to the local REST API directly.
 *
 *   http — the page is a plain browser tab on http://<host>:<port>/ui/, so
 *          there is no bridge at all and everything is fetch() against the
 *          REST API with the token the user pasted (kept in localStorage).
 *
 * Rules this file follows: never invent a success, never report ok for
 * something that did not happen, and always surface the real stderr.
 */

import { useEffect, useState } from "preact/hooks";

export type Mode = "ksu" | "http";

export interface ShellResult {
  errno: number;
  stdout: string;
  stderr: string;
}

export interface JsonResult<T = any> {
  ok: boolean;
  data: T | null;
  raw: string;
  error: string;
  ms: number;
}

export interface ApiState {
  mode: Mode;
  ready: boolean;
  version: string;
  /** HTTP origin the REST API and the media endpoints live on. */
  base: string;
  token: string;
  port: number;
  listen: string;
  lanIp: string;
  moduleDir: string;
  running: boolean;
  /** Set when the ksu bridge is present but `webui.sh status` failed. */
  bootError: string;
  /** Human readable description of where the credentials came from. */
  source: string;
}

const LS = {
  base: "phoneserver.base",
  token: "phoneserver.token",
  moddir: "phoneserver.moddir",
  mode: "phoneserver.mode",
  realW: "phoneserver.realw",
  realH: "phoneserver.realh",
};

export const DEFAULT_BASE = "http://127.0.0.1:8080";
export const DEFAULT_MODDIR = "/data/adb/modules/phoneserver";

function ls(key: string, fallback = ""): string {
  try {
    const v = window.localStorage.getItem(key);
    return v === null ? fallback : v;
  } catch {
    return fallback;
  }
}

function lsSet(key: string, value: string): void {
  try {
    window.localStorage.setItem(key, value);
  } catch {
    /* private mode: the value simply does not persist */
  }
}

// ---------------------------------------------------------------- store ----

const listeners = new Set<() => void>();

export const state: ApiState = {
  mode: "http",
  ready: false,
  version: "",
  base: DEFAULT_BASE,
  token: "",
  port: 8080,
  listen: "",
  lanIp: "",
  moduleDir: DEFAULT_MODDIR,
  running: false,
  bootError: "",
  source: "",
};

function emit(): void {
  for (const l of listeners) l();
}

export function touch(): void {
  emit();
}

export function subscribe(fn: () => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

/** Re-render the calling component whenever the connection state changes. */
export function useApiState(): ApiState {
  const [, force] = useState(0);
  useEffect(() => subscribe(() => force((n: number) => n + 1)), []);
  return state;
}

// ------------------------------------------------------------- helpers ----

/** Single-quote a value for `sh`, so nothing the user typed can escape. */
export function q(value: string): string {
  return "'" + String(value).replace(/'/g, "'\\''") + "'";
}

/** base64 of a UTF-8 string (what webui.sh expects for JSON and text args). */
export function b64(value: string): string {
  const bytes = new TextEncoder().encode(value);
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return window.btoa(bin);
}

export function unb64(value: string): string {
  const bin = window.atob(value);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return new TextDecoder().decode(bytes);
}

export function hasKsuBridge(): boolean {
  const w = window as any;
  return !!(w.ksu && typeof w.ksu.exec === "function");
}

/**
 * The module directory, derived from the page URL when the manager loads the
 * bundle from disk (file:///data/adb/modules/phoneserver/webroot/index.html),
 * otherwise the standard install path. A user override is honoured because
 * some managers mount the module somewhere else.
 */
export function moduleDir(): string {
  const override = ls(LS.moddir, "");
  if (override) return override;
  try {
    const p = decodeURIComponent(window.location.pathname || "");
    const m = p.match(/^(\/.*?\/modules\/[^/]+)\//);
    if (m) return m[1];
  } catch {
    /* ignore */
  }
  return DEFAULT_MODDIR;
}

function str(v: unknown): string {
  if (typeof v === "string") return v;
  if (v === null || v === undefined) return "";
  if (typeof v === "number") return String(v);
  // Some bridges hand back a byte array instead of a string.
  if (Array.isArray(v) || v instanceof Uint8Array) {
    try {
      return new TextDecoder().decode(Uint8Array.from(v as number[]));
    } catch {
      return String(v);
    }
  }
  return String(v);
}

// ------------------------------------------------------------ ksu exec ----

let seq = 0;

/** One `window.ksu.exec` call, with a hard timeout and an honest failure. */
export function ksuExec(command: string, timeoutMs = 25000): Promise<ShellResult> {
  return new Promise<ShellResult>((resolve) => {
    const w = window as any;
    if (!hasKsuBridge()) {
      resolve({ errno: -1, stdout: "", stderr: "NO_BRIDGE" });
      return;
    }
    const name = "__ps_cb_" + ++seq;
    let settled = false;
    const timer = window.setTimeout(() => {
      if (settled) return;
      settled = true;
      try {
        delete w[name];
      } catch {
        /* ignore */
      }
      resolve({ errno: -1, stdout: "", stderr: `TIMEOUT after ${timeoutMs}ms` });
    }, timeoutMs);

    w[name] = (errno: unknown, stdout: unknown, stderr: unknown) => {
      if (settled) return;
      settled = true;
      window.clearTimeout(timer);
      try {
        delete w[name];
      } catch {
        /* ignore */
      }
      resolve({
        errno: Number(errno === undefined || errno === null ? 0 : errno),
        stdout: str(stdout),
        stderr: str(stderr),
      });
    };

    try {
      w.ksu.exec(command, "{}", name);
    } catch (e) {
      if (settled) return;
      settled = true;
      window.clearTimeout(timer);
      resolve({ errno: -1, stdout: "", stderr: "EXEC_FAILED: " + String(e) });
    }
  });
}

/** `sh <module>/webui.sh <sub> <args...>` — every shell action goes through here. */
export async function run(
  sub: string,
  args: (string | number)[] = [],
  timeoutMs = 25000,
): Promise<ShellResult & { command: string }> {
  const script = moduleDir() + "/webui.sh";
  const command =
    "sh " + q(script) + " " + [sub, ...args.map((a) => String(a))].map(q).join(" ");
  const res = await ksuExec(command, timeoutMs);
  return { ...res, command };
}

function pickJSON(text: string): any | null {
  const t = (text || "").trim();
  if (!t) return null;
  try {
    return JSON.parse(t);
  } catch {
    /* keep digging */
  }
  const a = t.indexOf("{");
  const b = t.lastIndexOf("}");
  if (a >= 0 && b > a) {
    try {
      return JSON.parse(t.slice(a, b + 1));
    } catch {
      return null;
    }
  }
  return null;
}

/** Run a webui.sh subcommand that prints JSON, and parse it. */
export async function shJSON<T = any>(
  sub: string,
  args: (string | number)[] = [],
  timeoutMs = 25000,
): Promise<JsonResult<T>> {
  const t0 = performance.now();
  const res = await run(sub, args, timeoutMs);
  const ms = Math.round(performance.now() - t0);
  const parsed = pickJSON(res.stdout);
  if (parsed !== null) {
    const ok = res.errno === 0 && parsed.ok !== false;
    return {
      ok,
      data: parsed,
      raw: res.stdout,
      error: ok ? "" : parsed.error || res.stderr || `webui.sh ${sub} exited ${res.errno}`,
      ms,
    };
  }
  return {
    ok: false,
    data: null,
    raw: res.stdout,
    error:
      res.stderr.trim() ||
      res.stdout.trim() ||
      `webui.sh ${sub} returned nothing (errno ${res.errno})`,
    ms,
  };
}

/** Run a webui.sh subcommand that prints plain text. */
export async function shText(
  sub: string,
  args: (string | number)[] = [],
  timeoutMs = 25000,
): Promise<{ ok: boolean; text: string; errno: number; error: string; ms: number }> {
  const t0 = performance.now();
  const res = await run(sub, args, timeoutMs);
  const ms = Math.round(performance.now() - t0);
  const text = res.stdout;
  return {
    ok: res.errno === 0,
    text,
    errno: res.errno,
    error: res.stderr.trim() || (res.errno !== 0 ? `exited ${res.errno}` : ""),
    ms,
  };
}

// --------------------------------------------------------- http layer ----

export function apiURL(path: string): string {
  // path is API-relative: "/health" -> <base>/api/v1/health
  return state.base.replace(/\/+$/, "") + "/api/v1" + (path.startsWith("/") ? path : "/" + path);
}

export function rawURL(path: string): string {
  return state.base.replace(/\/+$/, "") + (path.startsWith("/") ? path : "/" + path);
}

function authQuery(): string {
  return state.token ? "key=" + encodeURIComponent(state.token) : "";
}

/** A URL an <img>/<audio>/fetch can use directly, token included. */
export function mediaURL(path: string, params: Record<string, string | number | boolean> = {}): string {
  const u = new URL(rawURL(path));
  if (state.token) u.searchParams.set("key", state.token);
  for (const [k, v] of Object.entries(params)) u.searchParams.set(k, String(v));
  return u.toString();
}

export function fileURL(share: string, path: string, query = ""): string {
  const suffix = (path || "").split("/").map(encodeURIComponent).join("/");
  const base = rawURL(`/files/${encodeURIComponent(share)}/${suffix}`) + (query ? "?" + query : "");
  if (!state.token) return base;
  return base + (query ? "&" : "?") + authQuery();
}

async function httpFetch(path: string, init: RequestInit = {}, timeoutMs = 20000): Promise<JsonResult> {
  const t0 = performance.now();
  const ctl = new AbortController();
  const timer = window.setTimeout(() => ctl.abort(), timeoutMs);
  try {
    const headers: Record<string, string> = { ...((init.headers as Record<string, string>) || {}) };
    if (state.token) headers["X-Api-Key"] = state.token;
    const res = await fetch(rawURL(path), { ...init, headers, signal: ctl.signal });
    const text = await res.text();
    const ms = Math.round(performance.now() - t0);
    const parsed = pickJSON(text);
    if (!res.ok) {
      return {
        ok: false,
        data: parsed,
        raw: text,
        error:
          (parsed && (parsed.error || parsed.message)) ||
          `HTTP ${res.status} ${res.statusText}${text ? ": " + text.slice(0, 300) : ""}`,
        ms,
      };
    }
    return { ok: true, data: parsed !== null ? parsed : text, raw: text, error: "", ms };
  } catch (e: any) {
    const ms = Math.round(performance.now() - t0);
    const msg =
      e && e.name === "AbortError"
        ? `请求超时（${timeoutMs}ms）：服务没应答，或地址/端口不对`
        : `网络失败：${String(e && e.message ? e.message : e)}（base=${state.base}）`;
    return { ok: false, data: null, raw: "", error: msg, ms };
  } finally {
    window.clearTimeout(timer);
  }
}

// --------------------------------------------------------- public api ----

/**
 * Normalise an API path. Callers may pass either the short form ("/battery",
 * what webui.sh and this module's docs use) or the full form
 * ("/api/v1/battery", what the HTTP endpoint index shows) — writing the full
 * one used to produce "/api/v1/api/v1/battery" and a silent 404.
 */
export function apiPath(path: string): string {
  const p = path.startsWith("/") ? path : "/" + path;
  return p === "/api/v1" || p.startsWith("/api/v1/") ? p : "/api/v1" + p;
}

/**
 * GET an API resource. In ksu mode this is `webui.sh api <path>` so the WebView
 * never needs an HTTP origin; in http mode it is a plain fetch.
 */
export async function apiGet<T = any>(
  path: string,
  timeoutMs = 25000,
): Promise<JsonResult<T>> {
  const full = apiPath(path);
  if (state.mode === "ksu") {
    return shJSON<T>("api", [full], timeoutMs);
  }
  return httpFetch(full, {}, timeoutMs);
}

/**
 * POST to an API resource. `path` is API-relative. The body is JSON.
 * In ksu mode the JSON travels as base64 through `webui.sh api-post`.
 */
export async function apiPost<T = any>(
  path: string,
  body: unknown = {},
  timeoutMs = 25000,
): Promise<JsonResult<T>> {
  const full = apiPath(path);
  if (state.mode === "ksu") {
    return shJSON<T>("api-post", [full, b64(JSON.stringify(body ?? {}))], timeoutMs);
  }
  return httpFetch(
    full,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body ?? {}),
    },
    timeoutMs,
  );
}

/**
 * A request against an absolute path that is not under /api/v1 (the file
 * service). In ksu mode `webui.sh api` passes an absolute path straight to curl.
 */
export async function rawRequest<T = any>(
  absPath: string,
  opts: { method?: string; body?: unknown; timeoutMs?: number } = {},
): Promise<JsonResult<T>> {
  const method = (opts.method || "GET").toUpperCase();
  const timeoutMs = opts.timeoutMs || 25000;
  if (state.mode === "ksu") {
    if (method === "GET") return shJSON<T>("api", [absPath], timeoutMs);
    return shJSON<T>("api-post", [absPath, b64(JSON.stringify(opts.body ?? {}))], timeoutMs);
  }
  const init: RequestInit = { method };
  if (method !== "GET" && method !== "HEAD") {
    init.headers = { "Content-Type": "application/json" };
    init.body = JSON.stringify(opts.body ?? {});
  }
  return httpFetch(absPath, init, timeoutMs);
}

export interface Health {
  status: string;
  server_version: string;
  listen: string;
  port: string;
  lan_reachable: boolean;
  auth_required: boolean;
  stream_enabled: boolean;
  mcp_http: boolean;
  mcp_endpoint: string;
  file_shares: string[];
  file_service: { http: boolean; webdav: boolean; read_only: boolean; allow_delete: boolean };
  capture: { camera: boolean; mic: boolean };
  history_enabled: boolean;
  server_uptime_s: number;
}

/** Reach the API by whatever means this mode has, and report honestly. */
export async function ping(timeoutMs = 8000): Promise<{ ok: boolean; health: Health | null; error: string; ms: number }> {
  const r = await apiGet<Health>("/health", timeoutMs);
  return { ok: r.ok && !!r.data, health: r.data as Health, error: r.error, ms: r.ms };
}

// ------------------------------------------------ connection lifecycle ----

export interface BootStatus {
  version?: string;
  running?: boolean;
  pid?: number;
  watchdog_pid?: number;
  listen?: string;
  port?: number;
  lan_ip?: string;
  token?: string;
  config_path?: string;
  module_dir?: string;
}

export async function boot(): Promise<void> {
  const forced = ls(LS.mode, "");
  const bridge = hasKsuBridge();
  state.moduleDir = moduleDir();

  if (bridge && forced !== "http") {
    state.mode = "ksu";
    state.source = "KernelSU 桥取得（webui.sh status）";
    const st = await shJSON<BootStatus>("status", [], 25000);
    if (st.ok && st.data) {
      state.version = String(st.data.version || "");
      state.running = !!st.data.running;
      state.port = Number(st.data.port || 8080);
      state.listen = String(st.data.listen || "");
      state.lanIp = String(st.data.lan_ip || "");
      state.token = String(st.data.token || "");
      state.base = "http://127.0.0.1:" + state.port;
      state.bootError = "";
    } else {
      state.bootError = st.error || "webui.sh status 没有返回 JSON";
      state.base = ls(LS.base, DEFAULT_BASE);
      state.token = ls(LS.token, "");
      state.source = "桥调用失败，退回 localStorage 里的地址";
    }
  } else {
    state.mode = "http";
    state.base = ls(LS.base, DEFAULT_BASE);
    state.token = ls(LS.token, "");
    state.port = portOf(state.base, 8080);
    state.source = "直连 API（localStorage）";
    state.bootError = "";
  }
  state.ready = true;
  emit();
}

export function portOf(u: string, fallback = 8080): number {
  try {
    const url = new URL(u);
    if (url.port) return Number(url.port);
    return url.protocol === "https:" ? 443 : 80;
  } catch {
    return fallback;
  }
}

export function setConnection(opts: {
  mode?: Mode;
  base?: string;
  token?: string;
  moddir?: string;
}): void {
  if (opts.mode) {
    state.mode = opts.mode;
    lsSet(LS.mode, opts.mode === "http" ? "http" : "ksu");
  }
  if (opts.base !== undefined) {
    state.base = opts.base.trim().replace(/\/+$/, "") || DEFAULT_BASE;
    lsSet(LS.base, state.base);
    state.port = portOf(state.base, 8080);
  }
  if (opts.token !== undefined) {
    state.token = opts.token.trim();
    lsSet(LS.token, state.token);
  }
  if (opts.moddir !== undefined) {
    lsSet(LS.moddir, opts.moddir.trim());
    state.moduleDir = moduleDir();
  }
  state.source = state.mode === "ksu" ? "KernelSU 桥 + 本地 REST" : "直连 API（localStorage）";
  emit();
}

/** Trigger a fresh `webui.sh status` and adopt what it says. */
export async function refreshStatus(): Promise<JsonResult<BootStatus>> {
  if (state.mode !== "ksu") return { ok: false, data: null, raw: "", error: "当前是直连 API 模式", ms: 0 };
  const st = await shJSON<BootStatus>("status", [], 25000);
  if (st.ok && st.data) {
    state.version = String(st.data.version || state.version);
    state.running = !!st.data.running;
    state.port = Number(st.data.port || state.port);
    state.listen = String(st.data.listen || state.listen);
    state.lanIp = String(st.data.lan_ip || state.lanIp);
    state.token = String(st.data.token || state.token);
    state.base = "http://127.0.0.1:" + state.port;
    state.bootError = "";
    emit();
  }
  return st;
}

// ------------------------------------------------------ small utilities ----

export function realSize(): { w: number; h: number } {
  return {
    w: Number(ls(LS.realW, "0")) || 0,
    h: Number(ls(LS.realH, "0")) || 0,
  };
}

export function setRealSize(w: number, h: number): void {
  lsSet(LS.realW, String(w));
  lsSet(LS.realH, String(h));
}

/**
 * The panel resolution in the coordinate space `input tap` uses. Neither
 * /api/v1/display nor /api/v1/stream/status carries it on this build, but
 * /api/v1/screenshot?meta=1 does: source_width/source_height is the full
 * screencap before scaling, i.e. exactly the panel. Cheap (scale=0.1) and
 * cached, with a manual override as the last resort.
 */
export async function resolveRealSize(force = false): Promise<{ w: number; h: number; source: string }> {
  const cached = realSize();
  if (!force && cached.w > 0 && cached.h > 0) return { ...cached, source: "cache" };
  const r = await apiGet<any>("/screenshot", 60000);
  // /screenshot is a GET with query parameters; apiGet cannot carry them, so
  // build the request by hand below when the first attempt comes back odd.
  if (r.ok && r.data && r.data.source_width > 0 && r.data.source_height > 0) {
    setRealSize(r.data.source_width, r.data.source_height);
    return { w: r.data.source_width, h: r.data.source_height, source: "screenshot meta" };
  }
  const r2 = await screenshotMeta(0.1);
  if (r2.ok && r2.w > 0 && r2.h > 0) {
    setRealSize(r2.w, r2.h);
    return { w: r2.w, h: r2.h, source: "screenshot meta" };
  }
  return { w: 0, h: 0, source: "unknown" };
}

export interface SSMeta {
  ok: boolean;
  w: number;
  h: number;
  error: string;
  meta: any;
}

/** GET /api/v1/screenshot?meta=1 with the query string intact. */
export async function screenshotMeta(scale = 0.1): Promise<SSMeta> {
  const qs = `?meta=1&format=png&scale=${scale}`;
  if (state.mode === "ksu") {
    const r = await shJSON<any>("api", ["/api/v1/screenshot" + qs], 60000);
    const d = r.data || {};
    return {
      ok: r.ok,
      w: Number(d.source_width || 0),
      h: Number(d.source_height || 0),
      error: r.error,
      meta: d,
    };
  }
  const r = await httpFetch("/api/v1/screenshot" + qs, {}, 60000);
  const d = (r.data || {}) as any;
  return {
    ok: r.ok,
    w: Number(d.source_width || 0),
    h: Number(d.source_height || 0),
    error: r.error,
    meta: d,
  };
}

/** Copy text, with a fallback for WebViews that refuse the async clipboard. */
export async function copyText(text: string): Promise<boolean> {
  try {
    const nav = window.navigator as any;
    if (nav.clipboard && typeof nav.clipboard.writeText === "function") {
      await nav.clipboard.writeText(text);
      return true;
    }
  } catch {
    /* fall through */
  }
  try {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "readonly");
    ta.className = "fixed top-0 left-0 h-px w-px opacity-0";
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(ta);
    return ok;
  } catch {
    return false;
  }
}