/**
 * 接口与 MCP —— 92 个端点的索引（可搜索、可直接试调），以及给 AI agent 的 MCP 说明。
 *
 * 「试一下」发的是真实请求并把响应原文贴出来；失败就显示失败，不伪造结果。
 */
import { useEffect, useState } from "preact/hooks";
import { apiGet, state as apiState, rawRequest, shJSON, useApiState } from "../api";
import { Banner, Btn, Card, Code, CopyBtn, Field, INPUT, Pill, Row, SmallBtn, Stat, Toggle } from "../components/ui";

interface Ep {
  method: string;
  path: string;
  desc?: string;
}

export default function Endpoints() {
  const s = useApiState();
  const [eps, setEps] = useState<Ep[]>([]);
  const [q, setQ] = useState("");
  const [filter, setFilter] = useState<"all" | "get" | "write">("all");
  const [out, setOut] = useState("");
  const [outLabel, setOutLabel] = useState("");
  const [tools, setTools] = useState<any>(null);
  const [err, setErr] = useState("");
  const [probing, setProbing] = useState(true);

  useEffect(() => {
    (async () => {
      const r = await apiGet<any>("/endpoints", 20000);
      setProbing(false);
      if (r.ok && r.data) setEps((r.data as any).endpoints || []);
      else setErr(r.raw || r.error || "读取端点索引失败");
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [s.base, s.token]);

  const tryIt = async (e: Ep) => {
    setOutLabel(e.method + " " + e.path);
    const isWrite = e.method !== "GET";
    const r = isWrite
      ? await rawRequest<any>(e.path.replace("<share>", "sdcard").replace("<path>", ""), { method: e.method, body: {} })
      : await rawRequest<any>(e.path.replace("<share>", "sdcard").replace("<path>", ""));
    const text = r.ok ? JSON.stringify(r.data, null, 2) : r.raw || r.error;
    setOut("[" + new Date().toLocaleTimeString() + "] " + e.method + " " + e.path + " → " + (r.ok ? "" : "失败 ") + (text || "").slice(0, 6000));
  };

  const askTools = async () => {
    const body = { jsonrpc: "2.0", id: 1, method: "tools/list", params: {} };
    const r = await rawRequest<any>("/mcp", { method: "POST", body });
    if (r.ok && r.data) {
      const list = (r.data as any).result?.tools || [];
      setTools({ count: list.length, names: list.slice(0, 24).map((t: any) => t.name) });
    } else setTools({ error: r.raw || r.error });
  };

  const filtered = eps.filter((e) => {
    if (filter === "get" && e.method !== "GET") return false;
    if (filter === "write" && e.method === "GET") return false;
    if (!q) return true;
    const t = q.toLowerCase();
    return e.path.toLowerCase().includes(t) || (e.desc || "").toLowerCase().includes(t);
  });

  return (
    <div className="space-y-5">
      <Card
        title={"REST 端点（" + (probing && !eps.length ? "读取中…" : eps.length + " 个") + "）"}
        action={<SmallBtn onClick={() => setQ("")}>清除搜索</SmallBtn>}
      >
        {err ? <Banner tone="warn">{err}</Banner> : null}
        <div className="flex flex-wrap items-end gap-3">
          <Field label="搜索路径或说明" className="flex-1">
            <input className={INPUT} value={q} onInput={(e) => setQ((e.target as HTMLInputElement).value)} />
          </Field>
          <div className="flex gap-1">
            {(["all", "get", "write"] as const).map((f) => (
              <Btn key={f} variant={filter === f ? "primary" : "outline"} onClick={() => setFilter(f)}>
                {f === "all" ? "全部" : f === "get" ? "只读" : "写操作"}
              </Btn>
            ))}
          </div>
        </div>
        <div className="overflow-x-auto max-h-[34rem] overflow-y-auto">
          <table className="w-full text-sm">
            <tbody>
              {filtered.map((e) => (
                <tr key={e.method + e.path} className="border-b border-slate-100 dark:border-slate-800">
                  <td className="py-2 pr-3 align-top">
                    <Pill tone={e.method === "GET" ? "info" : e.method === "DELETE" ? "bad" : "idle"}>{e.method}</Pill>
                  </td>
                  <td className="py-2 pr-3 align-top">
                    <div className="font-mono text-xs break-all text-slate-800 dark:text-slate-200">{e.path}</div>
                    <div className="text-xs text-slate-500 dark:text-slate-500">{e.desc}</div>
                  </td>
                  <td className="py-2 align-top">
                    <button className="text-xs text-sky-600 dark:text-sky-400 hover:underline" onClick={() => void tryIt(e)}>
                      试一下
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <div className="flex items-center gap-2">
          <CopyBtn value={apiState.base + "/api/v1/endpoints"} label="复制端点索引地址" />
        </div>
      </Card>

      <Card title="MCP（让 AI agent 直接操作手机）" action={<SmallBtn onClick={() => void askTools()}>真实调用 tools/list</SmallBtn>}>
        <div className="grid sm:grid-cols-2 gap-x-6 gap-y-2">
          <Row label="HTTP 端点" value={apiState.base + "/mcp"} mono />
          <Row label="协议" value="JSON-RPC 2.0（MCP 2025-06-18）" tone="muted" />
        </div>
        <Field label="stdio 启动命令" hint="Claude Code / Codex / Hermes 直接挂这个">
          <Code>{"bin/phoneserver -mcp -config /data/adb/phoneserver/config.json"}</Code>
        </Field>
        <div className="flex flex-wrap gap-2">
          <CopyBtn value={"bin/phoneserver -mcp -config /data/adb/phoneserver/config.json"} label="复制 stdio 命令" />
          <CopyBtn value={apiState.base + "/mcp"} label="复制 HTTP 端点" />
        </div>
        {tools?.error ? <Banner tone="warn">{String(tools.error).slice(0, 300)}</Banner> : null}
        {tools?.count ? (
          <>
            <Stat label="工具数量" value={tools.count} tone="good" />
            <div className="flex flex-wrap gap-2">
              {tools.names.map((n: string) => (
                <Pill key={n} tone="info">{n}</Pill>
              ))}
            </div>
          </>
        ) : null}
        <Banner tone="info">
          工具覆盖 REST 的全部能力（遥测、输入、UI、应用、通知、相机麦克风、文件、历史、配置）。
          需要只读时在配置页打开 <code>mcp.read_only</code>，AI 就只能读不能操作。
        </Banner>
      </Card>

      {out ? (
        <Card title={"试调结果：" + outLabel} action={<SmallBtn onClick={() => setOut("")}>清空</SmallBtn>}>
          <pre className="overflow-auto rounded-md border border-slate-200 dark:border-slate-700 bg-slate-50 dark:bg-black p-3 text-xs whitespace-pre-wrap break-all max-h-96">{out}</pre>
        </Card>
      ) : null}
    </div>
  );
}
