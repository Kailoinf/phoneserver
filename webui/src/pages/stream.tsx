/**
 * 实时画面 —— 屏幕 H.264 裸流 + WebCodecs 解码，低延时低流量。
 *
 * 协议（已冻结）：响应体第一行是 UTF-8 JSON 握手，之后每帧 = 4 字节大端长度
 * + Annex-B 数据。这里按这个协议自己拆帧；浏览器不支持 WebCodecs 时降级到
 * multipart WebP（MJPEG 通道），用 <img> 直接看。
 *
 * 画面上的点击/拖拽会换算成手机的触摸事件（面板尺寸来自截图 meta，取一次缓存）。
 */
import { useEffect, useRef, useState } from "preact/hooks";
import { apiGet, apiPost, apiURL, mediaURL, state as apiState, useApiState } from "../api";
import { Banner, Btn, Card, CopyBtn, Field, INPUT, Pill, Row, Seg, SmallBtn, Stat, Toggle } from "../components/ui";

interface Hello {
  protocol?: string;
  codec?: string;
  avc?: string;
  width?: number;
  height?: number;
  bitrate?: number;
  display_woken?: boolean;
  size?: string;
  started_at?: string;
}

type Mode = "auto" | "codecs" | "mjpeg";

export default function Stream() {
  const s = useApiState();
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  const decoderRef = useRef<any>(null);

  const [mode, setMode] = useState<Mode>("auto");
  const [size, setSize] = useState("720x1560");
  const [bitrate, setBitrate] = useState(2000000);
  const [wake, setWake] = useState(true);
  const [running, setRunning] = useState(false);
  const [hello, setHello] = useState<Hello | null>(null);
  const [err, setErr] = useState("");
  const [stats, setStats] = useState({ frames: 0, fps: 0, kbps: 0, queue: 0 });
  const [status, setStatus] = useState<any>(null);
  const [display, setDisplay] = useState<any>(null);
  const [panel, setPanel] = useState<{ w: number; h: number }>(() => {
    try {
      const v = JSON.parse(window.localStorage.getItem("phoneserver.panel") || "null");
      return v && v.w ? v : { w: 0, h: 0 };
    } catch {
      return { w: 0, h: 0 };
    }
  });
  const [inputNote, setInputNote] = useState("");
  const tapStart = useRef<{ x: number; y: number; t: number } | null>(null);

  const codecsOK = typeof (window as any).VideoDecoder !== "undefined";
  const effective: Mode = mode === "auto" ? (codecsOK ? "codecs" : "mjpeg") : mode;

  const refreshStatus = async () => {
    const st = await apiGet<any>("/api/v1/stream/status", 8000);
    if (st.ok) setStatus(st.data);
    const d = await apiGet<any>("/api/v1/stream/display", 8000);
    if (d.ok) setDisplay(d.data);
  };
  useEffect(() => {
    void refreshStatus();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [s.base, s.token]);

  const grabPanel = async () => {
    const r = await apiGet<any>("/api/v1/screenshot?meta=1&scale=0.2&quality=40", 15000);
    const d: any = r.data;
    if (r.ok && d && d.source_width && d.source_height) {
      const v = { w: d.source_width, h: d.source_height };
      setPanel(v);
      try {
        window.localStorage.setItem("phoneserver.panel", JSON.stringify(v));
      } catch {}
      setInputNote("面板尺寸 " + v.w + "×" + v.h + "（触摸坐标按此换算）");
    } else {
      setInputNote("取面板尺寸失败（截图端点无响应），触摸会按画面尺寸直接发送");
    }
  };
  useEffect(() => {
    if (!panel.w) void grabPanel();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const draw = (frame: any) => {
    const c = canvasRef.current;
    if (!c) return;
    const w = frame.displayWidth || frame.codedWidth;
    const h = frame.displayHeight || frame.codedHeight;
    if (c.width !== w || c.height !== h) {
      c.width = w;
      c.height = h;
    }
    const ctx = c.getContext("2d");
    if (ctx) ctx.drawImage(frame, 0, 0, w, h);
  };

  const keyFrame = (data: Uint8Array) => {
    // Annex-B: find start codes and read the NAL type; 5 = IDR, 7 = SPS.
    for (let i = 0; i + 4 < data.length; i++) {
      if (data[i] === 0 && data[i + 1] === 0 && data[i + 2] === 1) {
        const t = data[i + 3] & 0x1f;
        if (t === 5 || t === 7) return true;
        i += 2;
      }
    }
    return false;
  };

  const start = async () => {
    setErr("");
    setInputNote("");
    if (effective === "mjpeg") {
      setRunning(true);
      return;
    }
    if (!codecsOK) {
      setErr("这个浏览器没有 WebCodecs（VideoDecoder），已切到 MJPEG 通道。");
      setRunning(true);
      return;
    }
    const ac = new AbortController();
    abortRef.current = ac;
    setRunning(true);
    setStats({ frames: 0, fps: 0, kbps: 0, queue: 0 });

    const url =
      apiURL("/api/v1/stream/screen") + "?size=" + size + "&bitrate=" + bitrate + (wake ? "&wake=1" : "");
    let res: Response;
    try {
      res = await fetch(url, { signal: ac.signal, headers: apiState.token ? { "X-API-Key": apiState.token } : {} });
    } catch (e) {
      setErr("连接失败：" + String(e));
      setRunning(false);
      return;
    }
    if (!res.ok || !res.body) {
      const t = await res.text().catch(() => "");
      setErr("HTTP " + res.status + "：" + t.slice(0, 200));
      setRunning(false);
      return;
    }

    const reader = res.body.getReader();
    let pending = new Uint8Array(0);
    const take = (n: number) => {
      const out = pending.slice(0, n);
      pending = pending.slice(n);
      return out;
    };
    // read until the handshake line (\n) arrives
    let asText = "";
    while (!asText.includes("\n")) {
      const { done, value } = await reader.read();
      if (done) {
        setErr("流在握手前就结束了");
        setRunning(false);
        return;
      }
      const merged = new Uint8Array(pending.length + value.length);
      merged.set(pending, 0);
      merged.set(value, pending.length);
      pending = merged;
      const nl = pending.indexOf(10);
      if (nl >= 0) {
        asText = new TextDecoder().decode(pending.slice(0, nl));
        pending = pending.slice(nl + 1);
      }
    }
    let h: Hello;
    try {
      h = JSON.parse(asText);
    } catch {
      setErr("握手不是 JSON：" + asText.slice(0, 120));
      setRunning(false);
      return;
    }
    setHello(h);

    const dec = new (window as any).VideoDecoder({
      output: (frame: any) => {
        draw(frame);
        setStats((st) => ({ ...st, frames: st.frames + 1 }));
        frame.close();
      },
      error: (e: any) => setErr("解码器错误：" + String(e && e.message ? e.message : e)),
    });
    decoderRef.current = dec;
    try {
      dec.configure({ codec: h.avc || "avc1.42E01E", optimizeForLatency: true, hardwareAcceleration: "prefer-hardware" });
    } catch (e) {
      setErr("VideoDecoder 配置失败（" + (h.avc || "?") + "）：" + String(e));
      setRunning(false);
      return;
    }

    let bytes = 0;
    let t0 = performance.now();
    let idx = 0;
    const tick = window.setInterval(() => {
      const dt = (performance.now() - t0) / 1000;
      if (dt >= 1) {
        setStats((st) => ({ ...st, fps: Math.round((st.frames - 0) / dt) || 0, kbps: Math.round((bytes * 8) / dt / 1000), queue: dec.decodeQueueSize }));
        t0 = performance.now();
        bytes = 0;
      }
    }, 1000);

    try {
      for (;;) {
        if (ac.signal.aborted) break;
        if (pending.length < 4) {
          const { done, value } = await reader.read();
          if (done) break;
          const merged = new Uint8Array(pending.length + value.length);
          merged.set(pending, 0);
          merged.set(value, pending.length);
          pending = merged;
          continue;
        }
        const len = new DataView(pending.buffer, pending.byteOffset, 4).getUint32(0, false);
        if (len <= 0 || len > 4 * 1024 * 1024) {
          setErr("帧长度异常：" + len + "，已停止");
          break;
        }
        if (pending.length < 4 + len) {
          const { done, value } = await reader.read();
          if (done) break;
          const merged = new Uint8Array(pending.length + value.length);
          merged.set(pending, 0);
          merged.set(value, pending.length);
          pending = merged;
          continue;
        }
        take(4);
        const payload = take(len);
        bytes += 4 + len;
        idx++;
        if (dec.decodeQueueSize > 8) {
          // Drop nothing, just stop reading for a moment: reading faster than the
          // decoder drains is what turns "low latency" into "ten seconds behind".
          await new Promise((r) => window.setTimeout(r, 8));
        }
        dec.decode(new (window as any).EncodedVideoChunk({ type: keyFrame(payload) ? "key" : "delta", timestamp: idx * 33333, data: payload }));
      }
    } catch (e) {
      if (!ac.signal.aborted) setErr("读流中断：" + String(e));
    }
    window.clearInterval(tick);
    setRunning(false);
  };

  const stop = () => {
    abortRef.current?.abort();
    abortRef.current = null;
    try {
      decoderRef.current?.close();
    } catch {}
    decoderRef.current = null;
    setRunning(false);
  };

  const sendTap = async (x: number, y: number) => {
    const c = canvasRef.current;
    if (!c) return;
    const sx = panel.w ? (x * panel.w) / c.clientWidth : x;
    const sy = panel.h ? (y * panel.h) / c.clientHeight : y;
    const r = await apiPost("/api/v1/input/tap", { x: Math.round(sx), y: Math.round(sy) });
    setInputNote(r.ok ? "已点击 (" + Math.round(sx) + ", " + Math.round(sy) + ")" : "点击失败：" + (r.raw || r.error));
  };

  const onDown = (e: PointerEvent) => {
    const c = canvasRef.current;
    if (!c) return;
    const rect = c.getBoundingClientRect();
    tapStart.current = { x: e.clientX - rect.left, y: e.clientY - rect.top, t: Date.now() };
  };
  const onUp = async (e: PointerEvent) => {
    const c = canvasRef.current;
    const st = tapStart.current;
    tapStart.current = null;
    if (!c || !st) return;
    const rect = c.getBoundingClientRect();
    const ex = e.clientX - rect.left;
    const ey = e.clientY - rect.top;
    const dist = Math.hypot(ex - st.x, ey - st.y);
    if (dist < 12) {
      void sendTap(ex, ey);
      return;
    }
    const sx = panel.w ? (st.x * panel.w) / c.clientWidth : st.x;
    const sy = panel.h ? (st.y * panel.h) / c.clientHeight : st.y;
    const tx = panel.w ? (ex * panel.w) / c.clientWidth : ex;
    const ty = panel.h ? (ey * panel.h) / c.clientHeight : ey;
    const r = await apiPost("/api/v1/input/swipe", {
      x1: Math.round(sx),
      y1: Math.round(sy),
      x2: Math.round(tx),
      y2: Math.round(ty),
      duration_ms: Math.max(80, Math.min(1200, Date.now() - st.t)),
    });
    setInputNote(r.ok ? "已滑动" : "滑动失败：" + (r.raw || r.error));
  };

  const mjpegURL = mediaURL("/api/v1/stream/screen/mjpeg", { fps: 2, width: 540, quality: 60 });

  return (
    <div className="space-y-5">
      <Card
        title="实时画面"
        action={
          <div className="flex items-center gap-2">
            <Pill tone={running ? "good" : "idle"}>{running ? "观看中" : "已停止"}</Pill>
            {running ? (
              <Btn variant="danger" onClick={stop}>停止</Btn>
            ) : (
              <Btn variant="primary" onClick={() => void start()}>开始</Btn>
            )}
          </div>
        }
      >
        {err ? <Banner tone="warn">{err}</Banner> : null}
        {display?.note ? <Banner tone="info">{display.note}</Banner> : null}
        <div className="flex flex-wrap items-end gap-3">
          <Field label="尺寸" className="w-36">
            <input className={INPUT} value={size} onInput={(e) => setSize((e.target as HTMLInputElement).value)} />
          </Field>
          <Field label="码率" hint="bit/s，越小越省流量" className="w-36">
            <input className={INPUT} type="number" step={250000} value={bitrate} onInput={(e) => setBitrate(Number((e.target as HTMLInputElement).value))} />
          </Field>
          <Seg
            value={mode}
            onChange={(v) => setMode(v as Mode)}
            options={[
              { value: "auto", label: "自动" },
              { value: "codecs", label: "WebCodecs" },
              { value: "mjpeg", label: "MJPEG" },
            ]}
          />
          <Toggle label="开流前唤醒屏幕" checked={wake} onChange={setWake} />
          <Btn variant="outline" onClick={() => void apiPost("/api/v1/control/screen", { action: "on" })}>点亮屏幕</Btn>
          <SmallBtn onClick={() => void refreshStatus()}>刷新服务端状态</SmallBtn>
        </div>

        {effective === "mjpeg" ? (
          <div className="space-y-2">
            <img className="w-full rounded-lg border border-slate-200 dark:border-slate-700" src={running ? mjpegURL : undefined} alt="MJPEG 画面" />
            <p className="text-xs text-slate-500 dark:text-slate-500">
              这是 multipart WebP 降级通道（服务端 fps/质量取自 stream.mjpeg_* 配置），
              延时比 H.264 高但兼容性好。上面的尺寸/码率对它无效。
            </p>
          </div>
        ) : (
          <div className="space-y-2">
            <canvas
              ref={canvasRef}
              className="w-full rounded-lg border border-slate-200 dark:border-slate-700 bg-slate-100 dark:bg-black touch-none"
              onPointerDown={onDown as any}
              onPointerUp={onUp as any}
            />
            <p className="text-xs text-slate-500 dark:text-slate-500">
              直接在画面上点击=点按，按住拖动=滑动。{inputNote ? " " + inputNote : ""}
              {panel.w ? "" : "（点「取面板尺寸」可让触摸更准）"}
            </p>
            {!panel.w ? (
              <SmallBtn onClick={() => void grabPanel()}>取面板尺寸</SmallBtn>
            ) : (
              <SmallBtn onClick={() => void grabPanel()}>重新取面板尺寸（当前 {panel.w}×{panel.h}）</SmallBtn>
            )}
          </div>
        )}

        <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
          <Stat label="解码帧数" value={stats.frames} />
          <Stat label="解码帧率" value={stats.fps} unit="fps" tone={stats.fps > 0 ? "good" : "normal"} />
          <Stat label="实测码率" value={stats.kbps} unit="kbps" />
          <Stat label="解码队列" value={stats.queue} tone={stats.queue > 6 ? "bad" : "normal"} />
        </div>
        {hello ? (
          <div className="grid sm:grid-cols-2 gap-x-6 gap-y-2">
            <Row label="握手协议" value={hello.protocol || "—"} mono />
            <Row label="编码" value={(hello.codec || "—") + " / " + (hello.avc || "—")} mono />
            <Row label="画面尺寸" value={(hello.width || "?") + "×" + (hello.height || "?")} mono />
            <Row label="请求码率" value={(hello.bitrate || 0) + " bit/s"} mono />
            <Row label="开流时屏幕已唤醒" value={hello.display_woken ? "是" : "否"} tone={hello.display_woken ? "good" : "normal"} />
            <Row label="开始时间" value={hello.started_at || "—"} tone="muted" />
          </div>
        ) : null}
      </Card>

      <Card title="服务端串流状态" action={<SmallBtn onClick={() => void refreshStatus()}>刷新</SmallBtn>}>
        <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
          <Stat label="观看客户端" value={status?.clients ?? "—"} />
          <Stat label="并发上限" value={status?.max_clients ?? "—"} tone="muted" />
          <Stat label="空闲断开" value={status?.idle_timeout_seconds ?? "—"} unit="秒" tone="muted" />
          <Stat label="编码器" value={Array.isArray(status?.encoders) ? status.encoders.length : "—"} tone="muted" />
        </div>
        {(status?.encoders || []).map((e: any, i: number) => (
          <div key={i} className="grid sm:grid-cols-2 gap-x-6 gap-y-2 border-t border-slate-100 dark:border-slate-800 pt-2">
            <Row label="pid / 尺寸" value={(e.pid ?? "—") + " / " + (e.size || "—")} mono />
            <Row label="实测 fps" value={e.fps ?? e.frames_per_second ?? "—"} mono />
            <Row label="实测码率" value={(e.kbps ?? e.bitrate_kbps ?? "—") + " kbps"} mono />
            <Row label="客户端数" value={e.clients ?? "—"} mono />
          </div>
        ))}
        {status ? (
          <details className="text-xs">
            <summary className="cursor-pointer text-slate-500 dark:text-slate-400">原始 /api/v1/stream/status</summary>
            <pre className="pt-2 whitespace-pre-wrap break-all">{JSON.stringify(status, null, 2)}</pre>
          </details>
        ) : null}
        <div className="flex flex-wrap items-center gap-2">
          <Row label="H.264 裸流地址" value={apiState.base + "/api/v1/stream/screen"} mono tone="muted" />
          <CopyBtn value={apiState.base + "/api/v1/stream/screen?key=" + (apiState.token ? "<token>" : "")} label="复制（含 token 占位）" />
        </div>
        <p className="text-xs text-slate-500 dark:text-slate-500">
          ffplay 直接看：<code>ffplay -fflags nobuffer -flags low_delay "http://{apiState.base.replace(/^http:\/\//, "")}/api/v1/stream/screen?key=&lt;token&gt;"</code>
        </p>
      </Card>
    </div>
  );
}
