/**
 * 操作控制 —— 输入、屏幕、音量、亮度、旋转、手电、振动、剪贴板、TTS、媒体、Wi-Fi，
 * 以及默认关闭的受控端点（shell / 关机 / 重启）。
 *
 * 每一次动作都把后端的原文贴出来：403 就显示 403 的原因，不吞错误。
 */
import { useEffect, useState } from "preact/hooks";
import { apiGet, apiPost } from "../api";
import { Banner, Btn, Card, Field, INPUT, Modal, Row, Seg, SmallBtn, Stat, Toggle } from "../components/ui";

interface Res {
  ok: boolean;
  data: any;
  raw: string;
  error: string;
}

export default function Control() {
  const [log, setLog] = useState<string>("");
  const [audio, setAudio] = useState<any>(null);
  const [sensors, setSensors] = useState<any>(null);
  const [bright, setBright] = useState(50);
  const [volStream, setVolStream] = useState("music");
  const [volValue, setVolValue] = useState(5);
  const [toast, setToast] = useState("");
  const [tts, setTts] = useState("");
  const [clip, setClip] = useState("");
  const [mediaFile, setMediaFile] = useState("");
  const [shellCmd, setShellCmd] = useState("id");
  const [confirm, setConfirm] = useState<"reboot" | "shutdown" | null>(null);
  const [danger, setDanger] = useState<{ shell: boolean; power: boolean }>({ shell: false, power: false });

  const say = (label: string, r: Res) => {
    const body = r.ok ? JSON.stringify(r.data ?? r.raw, null, 2) : r.raw || r.error;
    setLog("[" + new Date().toLocaleTimeString() + "] " + label + " → " + (r.ok ? "" : "失败 ") + body.slice(0, 1200));
  };

  const post = async (label: string, path: string, body?: any) => {
    const r = (await apiPost(path, body)) as Res;
    if (r.ok && r.data && (r.data as any).status && (r.data as any).status >= 400) {
      // apiPost reports HTTP errors as ok:false, but a few endpoints answer 200
      // with a status field; surface those too rather than showing success.
      say(label, { ok: false, data: r.data, raw: r.raw, error: r.error });
      return r;
    }
    say(label, r);
    return r;
  };

  const refresh = async () => {
    const a = await apiGet<any>("/api/v1/control/audio", 10000);
    if (a.ok) setAudio(a.data);
    const s = await apiGet<any>("/api/v1/control/sensors", 15000);
    if (s.ok) setSensors(s.data);
    const c = await apiGet<any>("/api/v1/config", 8000);
    if (c.ok) {
      const sec = (c.data?.config || c.data)?.security || {};
      setDanger({ shell: !!sec.allow_shell, power: !!sec.allow_power_commands });
    }
  };

  useEffect(() => {
    void refresh();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div className="space-y-5">
      <Card
        title="屏幕与显示"
        action={<SmallBtn onClick={() => void refresh()}>读取状态</SmallBtn>}
      >
        <div className="flex flex-wrap gap-2">
          <Btn variant="primary" onClick={() => void post("屏幕开", "/api/v1/control/screen", { action: "on" })}>点亮</Btn>
          <Btn onClick={() => void post("屏幕关", "/api/v1/control/screen", { action: "off" })}>熄屏</Btn>
          <Btn onClick={() => void post("屏幕切换", "/api/v1/control/screen", { action: "toggle" })}>切换</Btn>
          <Btn onClick={() => void post("截图", "/api/v1/screenshot", {})}>截图（含 meta）</Btn>
        </div>
        <div className="grid sm:grid-cols-2 gap-3 pt-1">
          <Field label="亮度" hint={"当前 " + bright + " / 255"}>
            <input
              className={INPUT}
              type="range"
              min={0}
              max={255}
              value={bright}
              onInput={(e) => setBright(Number((e.target as HTMLInputElement).value))}
            />
          </Field>
          <div className="flex items-end gap-2">
            <Btn onClick={() => void post("设置亮度", "/api/v1/control/brightness", { value: bright })}>应用亮度</Btn>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-sm text-slate-500 dark:text-slate-400">旋转</span>
          {["0", "1", "2", "3", "auto"].map((r) => (
            <Btn key={r} onClick={() => void post("旋转 " + r, "/api/v1/control/rotation", { value: r })}>{r}</Btn>
          ))}
        </div>
      </Card>

      <Card title="音量与音频">
        <div className="flex flex-wrap items-end gap-3">
          <Field label="音频流" className="w-40">
            <select className={INPUT} value={volStream} onChange={(e) => setVolStream((e.target as HTMLSelectElement).value)}>
              {["music", "ring", "alarm", "notification", "call", "system"].map((s) => (
                <option key={s} value={s}>{s}</option>
              ))}
            </select>
          </Field>
          <Field label="音量值" className="w-28">
            <input className={INPUT} type="number" min={0} max={15} value={volValue} onInput={(e) => setVolValue(Number((e.target as HTMLInputElement).value))} />
          </Field>
          <Btn variant="primary" onClick={() => void post("设置音量", "/api/v1/control/volume", { stream: volStream, action: "set", value: volValue })}>设值</Btn>
          <Btn onClick={() => void post("音量+", "/api/v1/control/volume", { stream: volStream, action: "up" })}>+1</Btn>
          <Btn onClick={() => void post("音量-", "/api/v1/control/volume", { stream: volStream, action: "down" })}>-1</Btn>
          <Btn onClick={() => void post("静音", "/api/v1/control/volume", { stream: volStream, action: "mute" })}>静音</Btn>
        </div>
        <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
          {(audio?.streams || []).slice(0, 8).map((st: any) => (
            <Stat key={st.stream ?? st.name} label={st.stream ?? st.name} value={st.volume ?? st.index} unit={st.muted ? "静音" : ""} />
          ))}
        </div>
      </Card>

      <Card title="输入与提示">
        <div className="flex flex-wrap gap-2">
          {["HOME", "BACK", "MENU", "APP_SWITCH", "ENTER", "POWER", "WAKEUP"].map((k) => (
            <Btn key={k} onClick={() => void post("按键 " + k, "/api/v1/input/key", { key: k })}>{k}</Btn>
          ))}
        </div>
        <div className="grid sm:grid-cols-2 gap-3">
          <Field label="Toast 文本">
            <input className={INPUT} value={toast} onInput={(e) => setToast((e.target as HTMLInputElement).value)} />
          </Field>
          <div className="flex items-end gap-2">
            <Btn onClick={() => void post("Toast", "/api/v1/control/toast", { text: toast })}>弹出提示</Btn>
            <Btn onClick={() => void post("振动", "/api/v1/control/vibrate", { ms: 300 })}>振动 300ms</Btn>
            <Btn onClick={() => void post("手电开", "/api/v1/control/torch", { on: true })}>手电开</Btn>
            <Btn onClick={() => void post("手电关", "/api/v1/control/torch", { on: false })}>手电关</Btn>
          </div>
          <Field label="TTS 文本">
            <input className={INPUT} value={tts} onInput={(e) => setTts((e.target as HTMLInputElement).value)} />
          </Field>
          <div className="flex items-end gap-2">
            <Btn onClick={() => void post("朗读", "/api/v1/control/tts", { text: tts })}>朗读</Btn>
          </div>
          <Field label="剪贴板文本">
            <input className={INPUT} value={clip} onInput={(e) => setClip((e.target as HTMLInputElement).value)} />
          </Field>
          <div className="flex items-end gap-2">
            <Btn onClick={() => void post("写入剪贴板", "/api/v1/control/clipboard", { action: "set", text: clip })}>写入</Btn>
            <Btn onClick={() => void post("读取剪贴板", "/api/v1/control/clipboard", { action: "get" })}>读取</Btn>
          </div>
        </div>
      </Card>

      <Card title="媒体与网络">
        <div className="flex flex-wrap gap-2">
          {["play", "pause", "stop", "next", "prev", "info"].map((a) => (
            <Btn key={a} onClick={() => void post("媒体 " + a, "/api/v1/control/media", { action: a })}>{a}</Btn>
          ))}
          <Btn onClick={() => void post("Wi-Fi 开", "/api/v1/control/wifi", { on: true })}>Wi-Fi 开</Btn>
          <Btn onClick={() => void post("Wi-Fi 关", "/api/v1/control/wifi", { on: false })}>Wi-Fi 关</Btn>
        </div>
        <Field label="媒体文件路径" hint="如 /sdcard/Music/a.mp3">
          <input className={INPUT} value={mediaFile} onInput={(e) => setMediaFile((e.target as HTMLInputElement).value)} placeholder="/sdcard/…" />
        </Field>
        <Btn onClick={() => void post("播放文件", "/api/v1/control/media", { action: "play", file: mediaFile })}>播放该文件</Btn>
        {sensors ? (
          <details className="text-sm">
            <summary className="cursor-pointer text-slate-500 dark:text-slate-400">传感器（{(sensors.sensors || []).length} 个）</summary>
            <div className="pt-2 flex flex-wrap gap-2">
              {(sensors.sensors || []).slice(0, 12).map((x: any) => (
                <Btn key={x.name} onClick={() => void post("传感器 " + x.name, "/api/v1/control/sensors?sensor=" + encodeURIComponent(x.name) + "&n=1", undefined)}>
                  {x.name}
                </Btn>
              ))}
            </div>
          </details>
        ) : null}
      </Card>

      <Card title="受控端点（默认关闭）">
        <Banner tone="warn">
          shell、关机与重启默认被 security 开关拦下。下面的按钮会真的执行——403 时把服务端的原话显示出来，
          开关在「配置」页里。
        </Banner>
        <div className="flex flex-wrap gap-2">
          <Toggle label="allow_shell" checked={danger.shell} onChange={() => {}} hint="配置页可改" />
          <Toggle label="allow_power_commands" checked={danger.power} onChange={() => {}} hint="配置页可改" />
        </div>
        <Field label="受控 shell" hint="需要 security.allow_shell=true">
          <input className={INPUT} value={shellCmd} onInput={(e) => setShellCmd((e.target as HTMLInputElement).value)} />
        </Field>
        <div className="flex flex-wrap gap-2">
          <Btn variant="danger" onClick={() => void post("执行 shell", "/api/v1/shell", { cmd: shellCmd })}>执行</Btn>
          <Btn variant="danger" onClick={() => setConfirm("reboot")}>重启手机</Btn>
          <Btn variant="danger" onClick={() => setConfirm("shutdown")}>关机</Btn>
        </div>
      </Card>

      <Card title="最近一次操作结果" action={log ? <SmallBtn onClick={() => setLog("")}>清空</SmallBtn> : undefined}>
        <pre className="overflow-auto rounded-md border border-slate-200 dark:border-slate-700 bg-slate-50 dark:bg-black p-3 text-xs whitespace-pre-wrap break-all max-h-64">
          {log || "还没有操作。"}
        </pre>
      </Card>

      <Modal open={confirm !== null} onClose={() => setConfirm(null)} title={confirm === "shutdown" ? "确认关机？" : "确认重启？"}>
        <p className="text-sm text-slate-600 dark:text-slate-400">
          这是破坏性操作，手机上的所有会话都会中断。需要 security.allow_power_commands=true，否则服务端会返回 403。
        </p>
        <div className="flex justify-end gap-2 pt-3">
          <Btn variant="outline" onClick={() => setConfirm(null)}>取消</Btn>
          <Btn
            variant="danger"
            onClick={() => {
              const mode = confirm === "shutdown" ? "/api/v1/device/shutdown" : "/api/v1/device/reboot";
              void post(confirm === "shutdown" ? "关机" : "重启", mode, { mode: "normal" });
              setConfirm(null);
            }}
          >
            确认
          </Btn>
        </div>
      </Modal>
    </div>
  );
}
