/**
 * 相机与麦克风 —— 拍照/预览/录音，以及 Termux:API 桥的可用性与一键修复。
 *
 * 相机要 uid 在前台、麦克风要唤醒锁；失败时把后端的原话贴出来（不吞错误），
 * 并给一个「一键修复」按钮。
 */
import { useEffect, useState } from "preact/hooks";
import { apiGet, apiPost, mediaURL, state as apiState, useApiState } from "../api";
import { fmtBytes } from "../utils";
import { Banner, Btn, Card, Field, INPUT, Pill, Row, SmallBtn, Stat, TableHead, Td, Toggle } from "../components/ui";

// Kept out of the JSX so the attribute stays a plain call: an indexed access
// inside a JSX attribute expression tripped the transform's parser.
const maxJpeg = (c: any) => {
  const s = (c.jpeg_output_sizes || [])[0];
  return s ? s.width + "×" + s.height : "—";
};

export default function Capture() {
  const s = useApiState();
  const [bridge, setBridge] = useState<any>(null);
  const [camInfo, setCamInfo] = useState<any>(null);
  const [camera, setCamera] = useState(0);
  const [quality, setQuality] = useState(70);
  const [maxw, setMaxw] = useState(720);
  const [format, setFormat] = useState("webp");
  const [shotURL, setShotURL] = useState("");
  const [shotMeta, setShotMeta] = useState<any>(null);
  const [preview, setPreview] = useState(false);
  const [micSecs, setMicSecs] = useState(3);
  const [mic, setMic] = useState<any>(null);
  const [micStatus, setMicStatus] = useState<any>(null);
  const [audioURL, setAudioURL] = useState("");
  const [note, setNote] = useState("");
  const [keep, setKeep] = useState(false);

  const nonce = () => Date.now();

  const loadBridge = async () => {
    const r = await apiGet<any>("/bridge/status", 20000);
    if (r.ok) setBridge(r.data);
    else setBridge({ error: r.raw || r.error });
  };
  const loadCamInfo = async () => {
    const r = await apiGet<any>("/camera/info", 30000);
    if (r.ok) setCamInfo(r.data);
  };
  const loadMicStatus = async () => {
    const r = await apiGet<any>("/mic/status", 12000);
    if (r.ok) setMicStatus(r.data);
  };

  useEffect(() => {
    void loadBridge();
    void loadCamInfo();
    void loadMicStatus();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [s.base, s.token]);

  const fix = async () => {
    const r = await apiPost("/bridge/fix", {});
    setNote(r.ok ? "修复完成：" + JSON.stringify(r.data).slice(0, 300) : "修复失败：" + (r.raw || r.error));
    void loadBridge();
  };

  const takePhoto = async () => {
    setNote("");
    const r = await apiGet<any>(
      "/camera/photo?meta=1&camera=" + camera + "&format=" + format + "&quality=" + quality + (maxw ? "&maxwidth=" + maxw : ""),
      60000,
    );
    if (r.ok && r.data) {
      setShotMeta(r.data);
      setShotURL(
        mediaURL("/api/v1/camera/photo", {
          camera,
          format,
          quality,
          maxwidth: maxw || 0,
          n: nonce(),
        }),
      );
      setNote("拍摄完成（" + (r.data as any).capture_ms + " ms，编码 " + (r.data as any).encode_ms + " ms）");
    } else {
      setNote("拍照失败：" + (r.raw || r.error || "").slice(0, 300));
      void loadBridge();
    }
  };

  const record = async () => {
    setNote("录音中 " + micSecs + " 秒…");
    const r = await apiGet<any>("/mic/record?seconds=" + micSecs + "&meta=1", (micSecs + 20) * 1000);
    if (r.ok && r.data) {
      setMic(r.data);
      setAudioURL(mediaURL("/api/v1/mic/record", { seconds: micSecs, format: "m4a", n: nonce() }));
      setNote("录音完成：" + (r.data as any).bytes + " 字节，编码 " + (r.data as any).encoder);
    } else {
      setNote("录音失败：" + (r.raw || r.error || "").slice(0, 300));
      void loadBridge();
    }
  };

  const cams = camInfo?.cameras || [];
  const perms = bridge?.permissions || bridge?.status?.permissions || {};

  return (
    <div className="space-y-5">
      <Card
        title="Termux:API 桥"
        action={
          <div className="flex items-center gap-2">
            <SmallBtn onClick={() => void loadBridge()}>刷新</SmallBtn>
            <Btn variant="primary" onClick={() => void fix()}>一键修复</Btn>
          </div>
        }
      >
        {bridge?.error ? <Banner tone="warn">{String(bridge.error).slice(0, 300)}</Banner> : null}
        {note ? <Banner tone={note.includes("失败") ? "warn" : "ok"}>{note}</Banner> : null}
        <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
          <Stat label="桥可用" value={bridge?.available ? "是" : "否"} tone={bridge?.available ? "good" : "bad"} />
          <Stat label="Termux uid" value={bridge?.termux_uid ?? "—"} tone="muted" />
          <Stat label="相机现在可用" value={bridge?.camera_allowed_now ? "是" : "否"} tone={bridge?.camera_allowed_now ? "good" : "bad"} />
          <Stat label="麦克风现在可用" value={bridge?.mic_allowed_now ? "是" : "否"} tone={bridge?.mic_allowed_now ? "good" : "bad"} />
        </div>
        <div className="grid sm:grid-cols-2 gap-x-6 gap-y-2">
          <Row label="uid 状态" value={bridge?.uid_state ?? "—"} mono />
          <Row label="缺失工具" value={(bridge?.missing_tools || []).length ? String(bridge.missing_tools).slice(0, 120) : "无"} tone={(bridge?.missing_tools || []).length ? "bad" : "good"} />
          <Row label="策略说明" value={bridge?.policies ?? "—"} tone="muted" />
          <Row label="唤醒锁 / 前台修复次数" value={(bridge?.mic_wake_lock_fixes ?? "—") + " / " + (bridge?.camera_foreground_fixes ?? "—")} mono tone="muted" />
        </div>
        <div className="flex flex-wrap gap-2">
          {Object.keys(perms).map((k) => (
            <Pill key={k} tone={perms[k] ? "good" : "bad"}>{k}{perms[k] ? " 已授权" : " 未授权"}</Pill>
          ))}
        </div>
      </Card>

      <Card title="摄像头" action={<SmallBtn onClick={() => void loadCamInfo()}>读取能力</SmallBtn>}>
        <div className="grid sm:grid-cols-2 gap-3">
          <Field label="摄像头 id" hint={"可用：" + (cams.map((c: any) => c.id + "(" + c.facing + ")").join(", ") || "未读取")}>
            <input className={INPUT} type="number" value={camera} onInput={(e) => setCamera(Number((e.target as HTMLInputElement).value))} />
          </Field>
          <Field label="格式">
            <select className={INPUT} value={format} onChange={(e) => setFormat((e.target as HTMLSelectElement).value)}>
              {["webp", "jpeg", "png"].map((f) => (
                <option key={f} value={f}>{f}</option>
              ))}
            </select>
          </Field>
          <Field label="质量" hint="1-100">
            <input className={INPUT} type="number" min={1} max={100} value={quality} onInput={(e) => setQuality(Number((e.target as HTMLInputElement).value))} />
          </Field>
          <Field label="最大宽度" hint="0 = 原始尺寸（4096 宽约 5 MB）">
            <input className={INPUT} type="number" value={maxw} onInput={(e) => setMaxw(Number((e.target as HTMLInputElement).value))} />
          </Field>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Btn variant="primary" onClick={() => void takePhoto()}>拍一张</Btn>
          <Btn onClick={() => setPreview(!preview)}>{preview ? "停止预览" : "开始预览"}</Btn>
          <Toggle label="保留手机上的临时文件" checked={keep} onChange={setKeep} hint="默认关：响应直接给字节，拍完即删" />
        </div>
        {shotMeta ? (
          <div className="grid sm:grid-cols-2 gap-x-6 gap-y-2">
            <Row label="输出尺寸" value={(shotMeta.width || "?") + "×" + (shotMeta.height || "?")} mono />
            <Row label="原始尺寸" value={(shotMeta.source_width || "?") + "×" + (shotMeta.source_height || "?")} mono />
            <Row label="字节数" value={fmtBytes(shotMeta.bytes)} mono />
            <Row label="拍摄 / 编码耗时" value={(shotMeta.capture_ms ?? "—") + " ms / " + (shotMeta.encode_ms ?? "—") + " ms"} mono />
            <Row label="stored / pushed" value={String(shotMeta.stored) + " / " + String(shotMeta.pushed)} tone="muted" />
            <Row label="备注" value={(shotMeta.notes || []).join("；") || "—"} tone="muted" />
          </div>
        ) : null}
        {shotURL ? <img className="w-full max-w-md rounded-lg border border-slate-200 dark:border-slate-700" src={shotURL} alt="照片" /> : null}
        {preview ? (
          <img className="w-full max-w-md rounded-lg border border-slate-200 dark:border-slate-700" src={mediaURL("/api/v1/camera/preview", { fps: 2, width: 540, quality: 60, seconds: 30 })} alt="预览" />
        ) : null}
        {cams.length ? (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <TableHead cols={["id", "朝向", "焦距", "最大 JPEG"]} />
              <tbody>
                {cams.map((c: any) => (
                  <tr key={c.id} className="border-b border-slate-100 dark:border-slate-800">
                    <Td mono>{c.id}</Td>
                    <Td>{c.facing}</Td>
                    <Td mono>{(c.focal_lengths || []).map((f: number) => f.toFixed(1)).join(", ") || "—"}</Td>
                    <Td mono>{maxJpeg(c)}</Td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
      </Card>

      <Card title="麦克风" action={<SmallBtn onClick={() => void loadMicStatus()}>读取状态</SmallBtn>}>
        <div className="flex flex-wrap items-end gap-3">
          <Field label="时长（秒）" className="w-28">
            <input className={INPUT} type="number" value={micSecs} onInput={(e) => setMicSecs(Number((e.target as HTMLInputElement).value))} />
          </Field>
          <Btn variant="primary" onClick={() => void record()}>录音</Btn>
        </div>
        <div className="grid sm:grid-cols-2 gap-x-6 gap-y-2">
          <Row label="录音状态" value={micStatus?.recorder_state ? String(micStatus.recorder_state).slice(0, 80) : "—"} mono tone="muted" />
          {mic ? (
            <>
              <Row label="时长（请求/实际字节）" value={mic.seconds_requested + "s / " + fmtBytes(mic.bytes)} mono />
              <Row label="编码" value={(mic.encoder || "—") + " " + (mic.bitrate_kbps || "?") + " kbps " + (mic.rate_hz || "?") + "Hz " + (mic.channels || "?") + "ch"} mono />
              <Row label="文件保留" value={String(mic.kept)} tone="muted" />
              <Row label="stored / pushed" value={String(mic.stored) + " / " + String(mic.pushed)} tone="muted" />
            </>
          ) : null}
        </div>
        {audioURL ? <audio className="w-full" controls src={audioURL} /> : null}
        <p className="text-xs text-slate-500 dark:text-slate-500">
          录音会先取一次唤醒锁（termux-wake-lock），否则 Android 会拒掉后台 uid 的麦克风；
          相机还额外要求 uid 在前台，桥会在拍照前把 Termux 活动页拉到前台。
        </p>
      </Card>
    </div>
  );
}
