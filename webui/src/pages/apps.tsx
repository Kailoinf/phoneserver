/**
 * 应用 —— 安装列表、搜索、启动/停止，以及当前前台应用与活动栈。
 */
import { useEffect, useState } from "preact/hooks";
import { apiGet, apiPost } from "../api";
import { Banner, Btn, Card, Field, INPUT, Pill, Row, SmallBtn, Stat, TableHead, Td } from "../components/ui";

interface AppInfo {
  package: string;
  path?: string;
  system?: boolean;
  disabled?: boolean;
}

export default function Apps() {
  const [apps, setApps] = useState<AppInfo[]>([]);
  const [grep, setGrep] = useState("");
  const [only, setOnly] = useState<"all" | "user" | "system">("all");
  const [cur, setCur] = useState<any>(null);
  const [act, setAct] = useState<any>(null);
  const [note, setNote] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);

  const load = async (g = grep) => {
    setBusy(true);
    const r = await apiGet<any>("/apps?limit=1000" + (g ? "&grep=" + encodeURIComponent(g) : ""), 30000);
    setBusy(false);
    if (r.ok && r.data) {
      setApps((r.data as any).apps || []);
      setErr("");
    } else setErr(r.raw || r.error || "读取失败");
  };
  const loadCur = async () => {
    const c = await apiGet<any>("/apps/current", 12000);
    if (c.ok) setCur(c.data);
    const a = await apiGet<any>("/apps/activity", 15000);
    if (a.ok) setAct(a.data);
  };

  useEffect(() => {
    void load("");
    void loadCur();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const act1 = async (label: string, path: string, body: any) => {
    const r = await apiPost(path, body);
    setNote(r.ok ? label + " 成功" + (r.data ? "：" + JSON.stringify(r.data).slice(0, 160) : "") : label + " 失败：" + (r.raw || r.error));
    if (r.ok) void loadCur();
  };

  const filtered = apps.filter((a) => (only === "all" ? true : only === "system" ? a.system : !a.system));

  return (
    <div className="space-y-5">
      <Card
        title="当前前台"
        action={<SmallBtn onClick={() => void loadCur()}>刷新</SmallBtn>}
      >
        <div className="grid grid-cols-2 sm:grid-cols-3 gap-3">
          <Stat label="前台应用" value={cur?.package || cur?.window?.foreground_app || "—"} />
          <Stat label="Activity" value={(cur?.activity || "—").toString().split(".").slice(-1)[0]} />
          <Stat label="锁屏" value={cur?.window?.keyguard_showing === undefined ? (cur?.keyguard_showing ? "是" : "否") : cur.window.keyguard_showing ? "是" : "否"} tone="muted" />
        </div>
        {act ? (
          <details className="text-xs">
            <summary className="cursor-pointer text-slate-500 dark:text-slate-400">活动/任务栈摘要</summary>
            <pre className="pt-2 whitespace-pre-wrap break-all">{JSON.stringify(act, null, 2).slice(0, 4000)}</pre>
          </details>
        ) : null}
      </Card>

      <Card
        title={"已安装应用（" + filtered.length + " / " + apps.length + "）"}
        action={
          <div className="flex items-center gap-2">
            <SmallBtn onClick={() => setOnly(only === "all" ? "user" : only === "user" ? "system" : "all")}>
              {only === "all" ? "全部" : only === "user" ? "仅用户" : "仅系统"}
            </SmallBtn>
            <SmallBtn onClick={() => void load()} disabled={busy}>{busy ? "读取中…" : "刷新"}</SmallBtn>
          </div>
        }
      >
        {err ? <Banner tone="warn">{err}</Banner> : null}
        <div className="flex flex-wrap items-end gap-3">
          <Field label="搜索包名" className="flex-1">
            <input className={INPUT} value={grep} onInput={(e) => setGrep((e.target as HTMLInputElement).value)} />
          </Field>
          <Btn onClick={() => void load()}>搜索</Btn>
          <Btn variant="outline" onClick={() => { setGrep(""); void load(""); }}>清除</Btn>
        </div>
        {note ? <Banner tone={note.includes("失败") ? "warn" : "ok"}>{note}</Banner> : null}
        <div className="overflow-x-auto max-h-[32rem] overflow-y-auto">
          <table className="w-full text-sm">
            <TableHead cols={["包名", "来源", "状态", "操作"]} />
            <tbody>
              {filtered.slice(0, 400).map((a) => (
                <tr key={a.package} className="border-b border-slate-100 dark:border-slate-800">
                  <Td mono>{a.package}</Td>
                  <Td>{a.system ? <Pill>系统</Pill> : <Pill tone="info">用户</Pill>}</Td>
                  <Td>{a.disabled ? <Pill tone="bad">已禁用</Pill> : <Pill tone="good">正常</Pill>}</Td>
                  <Td>
                    <button className="text-xs text-sky-600 dark:text-sky-400 hover:underline" onClick={() => void act1("启动 " + a.package, "/apps/launch", { package: a.package })}>启动</button>
                    <span className="text-slate-300 dark:text-slate-700 px-1">|</span>
                    <button className="text-xs text-rose-500 hover:underline" onClick={() => void act1("停止 " + a.package, "/apps/stop", { package: a.package, force: false })}>停止</button>
                  </Td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <p className="text-xs text-slate-500 dark:text-slate-500">
          列表超过 400 条只显示前 400 条，用搜索缩小范围。「停止」默认是普通停止（force=false），
          系统应用可能拒绝或被系统立刻拉回。
        </p>
      </Card>
    </div>
  );
}
