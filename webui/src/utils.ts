export function fmtBytes(n: unknown, digits = 1): string {
  const v = Number(n);
  if (!isFinite(v)) return "—";
  const abs = Math.abs(v);
  if (abs < 1024) return `${v} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let i = -1;
  let x = v;
  do {
    x /= 1024;
    i++;
  } while (Math.abs(x) >= 1024 && i < units.length - 1);
  return `${x.toFixed(digits)} ${units[i]}`;
}

export function fmtNum(n: unknown, digits = 1): string {
  const v = Number(n);
  if (!isFinite(v)) return "—";
  return v.toFixed(digits);
}

export function fmtPct(n: unknown, digits = 1): string {
  const v = Number(n);
  if (!isFinite(v)) return "—";
  return `${v.toFixed(digits)}%`;
}

export function fmtHz(khz: unknown): string {
  const v = Number(khz);
  if (!isFinite(v) || v <= 0) return "—";
  return v >= 1000 ? `${(v / 1000).toFixed(2)} GHz` : `${v} MHz`;
}

export function fmtAgo(seconds: unknown): string {
  const s = Math.max(0, Math.round(Number(seconds) || 0));
  if (s < 60) return `${s} 秒`;
  if (s < 3600) return `${Math.floor(s / 60)} 分 ${s % 60} 秒`;
  if (s < 86400) return `${Math.floor(s / 3600)} 小时 ${Math.floor((s % 3600) / 60)} 分`;
  return `${Math.floor(s / 86400)} 天 ${Math.floor((s % 86400) / 3600)} 小时`;
}

/** uptime-style seconds -> "3h 27m" */
export function fmtDur(seconds: unknown): string {
  const s = Math.max(0, Math.round(Number(seconds) || 0));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s % 60}s`;
  return `${s}s`;
}

export function fmtTime(ts: unknown): string {
  if (ts === null || ts === undefined || ts === "") return "—";
  const n = Number(ts);
  if (!isFinite(n) || n <= 0) return String(ts);
  const ms = n > 1e12 ? n : n * 1000; // accept seconds or milliseconds
  const d = new Date(ms);
  const p = (x: number) => String(x).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

export function fmtClock(ts: unknown): string {
  const n = Number(ts);
  if (!isFinite(n) || n <= 0) return "—";
  const d = new Date(n > 1e12 ? n : n * 1000);
  const p = (x: number) => String(x).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}`;
}

export function agoText(iso: unknown): string {
  if (!iso) return "—";
  const t = Date.parse(String(iso));
  if (!isFinite(t)) return String(iso);
  const s = (Date.now() - t) / 1000;
  if (s < 0) return "刚刚";
  return fmtAgo(s) + "前";
}

/** 0 -> "0 B/s", 1e6 -> "976.6 KB/s" */
export function fmtRate(bytesPerSec: unknown): string {
  const v = Number(bytesPerSec);
  if (!isFinite(v)) return "—";
  return fmtBytes(v, 1) + "/s";
}

export function fmtUptime(seconds: unknown): string {
  const s = Math.max(0, Math.floor(Number(seconds) || 0));
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d}天 ${h}小时`;
  if (h > 0) return `${h}小时 ${m}分`;
  return `${m}分 ${s % 60}秒`;
}

/** Render any unknown value as one short line of text. */
export function text(v: unknown): string {
  if (v === null || v === undefined) return "—";
  if (typeof v === "boolean") return v ? "是" : "否";
  if (typeof v === "number") return isFinite(v) ? String(v) : "—";
  if (typeof v === "string") return v;
  if (Array.isArray(v)) return v.map(text).join(", ");
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

export function clampInt(value: unknown, min: number, max: number, fallback: number): number {
  const n = Math.round(Number(value));
  if (!isFinite(n)) return fallback;
  return Math.min(max, Math.max(min, n));
}

export function shortMs(ms: unknown): string {
  const v = Number(ms);
  if (!isFinite(v)) return "—";
  if (v < 1000) return `${Math.round(v)}ms`;
  return `${(v / 1000).toFixed(2)}s`;
}

/** Human duration for the request log ("since" style) */
export function sinceLabel(s: string): string {
  return s;
}

/** Percent value -> integer 0..100 for the SVG bars. */
export function pctOf(v: unknown): number {
  const n = Number(v);
  if (!isFinite(n)) return 0;
  return Math.min(100, Math.max(0, n));
}

export function errText(e: unknown): string {
  if (e instanceof Error) return e.message;
  return text(e);
}