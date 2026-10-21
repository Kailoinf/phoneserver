/**
 * 日志 —— 服务自身的日志（入到了历史库里，所以能按级别/关键字查），
 * 以及开机以来的 stdout。
 */
import { useEffect, useState } from "preact/hooks";
import { apiGet, rawRequest, shJSON, useApiState } from "../api";
import { Banner, Btn, Card, Code, Field, INPUT, Pill, Row, SmallBtn, Stat, Toggle } from "../components/ui";

interface Line {
  ts_ms?: number;
  time?: string;
  level?: string;
  source?: string;
  line?: string;
}

export default function Logs() {
  const s = useApiState();
  const [lines, setLines] = useState<Line[]>([]);
  const [level, setLevel] = useState("");
  const [grep, setGrep] = useState("");
  const [limit, setLimit] = useState(200);
  const [auto, setAuto] = useState(true);
  const [err, setErr] = useState("");
  const [note, setNote] = useState("");
  const [levels, setLevels] = useState<string[]>([]);
  const [stdout, setStdout] = useState("");

  const load = async () => {
    const r = await apiGet<any>(
      "/history/logs?limit=" + limit + (level ? "&level=" + encodeURIComponent(level) : "") + (grep ? "&grep=" + encodeURIComponent(grep) : ""),
      20000,
    );
    if (r.ok && r.data) {
      const ls: Line[] = (r.data as any).lines || [];
      setLines(ls);
      const found = Array.from(new Set(ls.map((l) => l.level).filter(Boolean))) as string[];
      if (found.length) setLevels((prev) => Array.from(new Set([...prev, ...found])));
      setErr("");
    } else setErr(r.raw || r.error || "读取失败");
  };

  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [s.base, s.token]);
  useEffect(() => {
    if (!auto) return;
    const t = window.setInterval(() => void load(), 5000);
    return () => window.clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [auto, level, grep, limit, s.base, s.token]);

  const loadStdout = async () => {
    const r = await shJSON<any>("logs", ["120"], 30000);
    setStdout(r.ok ? String(r.data ?? r.raw) : "读取 stdout 失败：" + (r.raw || r.error) + "（浏览器直连模式下不能用，需要在手机上执行 webui.sh logs）");
  };

  const clearLogs = async () => {
    const r = await shJSON<any>("clear-logs", [], 20000);
    setNote(r.ok ? "已清空日志文件（历史库里的行按保留策略滚动）" : "清空失败：" + (r.raw || r.error));
  };

  return (
    <div className="space-y-5">
      <Card
        title={"服务日志（" + lines.length + " 行）"}
        action={
          <div className="flex items-center gap-2">
            <Toggle label="5s 自动刷新" checked={auto} onChange={setAuto} />
            <SmallBtn onClick={() => void load()}>刷新</SmallBtn>
          </div>
        }
      >
        {err ? <Banner tone="warn">{err}</Banner> : null}
        {note ? <Banner tone="ok">{note}</Banner> : null}
        <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
          <Stat label="显示行数" value={lines.length} />
          <Stat label="级别" value={level || "全部"} tone="muted" />
          <Stat label="来源" value={Array.from(new Set(lines.map((l) => l.source).filter(Boolean))).join("/") || "—"} tone="muted" />
          <Stat label="最早一行" value={lines.length ? String(lines[lines.length - 1].time || "") : "—"} tone="muted" />
        </div>
        <div className="flex flex-wrap items-end gap-3">
          <Field label="条数" className="w-28">
            <input className={INPUT} type="number" value={limit} onInput={(e) => setLimit(Number((e.target as HTMLInputElement).value))} />
          </Field>
          <Field label="关键字" className="flex-1">
            <input className={INPUT} value={grep} onInput={(e) => setGrep((e.target as HTMLInputElement).value)} placeholder="例如 error、/api/v1/battery" />
          </Field>
          <div className="flex flex-wrap gap-1">
            <Btn variant={level === "" ? "primary" : "outline"} onClick={() => setLevel("")}>全部</Btn>
            {levels.slice(0, 6).map((l) => (
              <Btn key={l} variant={level === l ? "primary" : "outline"} onClick={() => setLevel(l)}>
                {l}
              </Btn>
            ))}
          </div>
        </div>
        <Code max="max-h-96">
          {lines.length
            ? lines.map((l, i) => (
                <div key={i}>
                  {l.time} [{l.level}] {l.line}
                </div>
              ))
            : "（没有匹配的日志行）"}
        </Code>
        <div className="flex flex-wrap gap-2">
          <Btn variant="outline" onClick={() => void loadStdout()}>读取 stdout.log</Btn>
          <Btn variant="danger" onClick={() => void clearLogs()}>清空日志文件</Btn>
        </div>
        {stdout ? <Code max="max-h-72">{stdout}</Code> : null}
        <p className="text-xs text-slate-500 dark:text-slate-500">
          日志会被镜像进历史库（history.log_retention_days 天），所以退出重进、重启后都还能查。
          上面的「清空日志文件」只动 /data/adb/phoneserver/logs/*，库里的行按保留策略滚动。
        </p>
      </Card>
    </div>
  );
}
