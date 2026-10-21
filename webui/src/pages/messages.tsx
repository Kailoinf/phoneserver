/**
 * 消息与通知 —— 通知全量读取 + 发通知 + 短信/通话/联系人。
 *
 * 没有 SIM 或没有权限时后端会在 empty_reason 里说明原因，这里把原因显示出来，
 * 而不是渲染一张空表格假装"没有数据"。
 */
import { useEffect, useState } from "preact/hooks";
import { apiGet, apiPost, apiURL, state as apiState, useApiState } from "../api";
import { agoText } from "../utils";
import { Banner, Btn, Card, Field, INPUT, Pill, Row, Seg, SmallBtn, Stat, TableHead, Td } from "../components/ui";

interface Notif {
  key?: string;
  package?: string;
  uid?: number;
  id?: number;
  tag?: string;
  channel?: string;
  title?: string;
  text?: string;
  when?: string;
  when_ms?: number;
  ongoing?: boolean;
  clearable?: boolean;
  foreground_service?: boolean;
  group_summary?: boolean;
  importance?: string;
  visibility?: string;
  flags?: string;
  actions?: any[];
}
interface NotifDoc {
  captured_at?: string;
  count?: number;
  notifications?: Notif[];
  packages?: Record<string, number>;
  total_before_limit?: number;
  ongoing_included?: boolean;
  hint?: string;
  empty_reason?: string;
}

const dash = (v: any) => (v === undefined || v === null || v === "" ? "—" : v);

export default function Messages() {
  const s = useApiState();
  const [doc, setDoc] = useState<NotifDoc | null>(null);
  const [err, setErr] = useState("");
  const [limit, setLimit] = useState(30);
  const [ongoing, setOngoing] = useState(true);
  const [grep, setGrep] = useState("");
  const [src, setSrc] = useState<"sms" | "calls" | "contacts">("sms");
  const [msgs, setMsgs] = useState<any>(null);
  const [q, setQ] = useState("");
  const [post, setPost] = useState({ title: "", text: "", id: "" });
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState("");

  const loadNotifs = async () => {
    setBusy(true);
    const path =
      "/api/v1/notifications?limit=" + limit + "&include_ongoing=" + (ongoing ? "1" : "0") +
      (grep ? "&grep=" + encodeURIComponent(grep) : "");
    const r = await apiGet<NotifDoc>(path, 20000);
    if (r.ok) {
      setDoc(r.data as NotifDoc);
      setErr("");
    } else {
      setErr(r.raw || r.error || "读取通知失败");
    }
    setBusy(false);
  };

  const loadMsgs = async (source = src, query = q) => {
    const path = query
      ? "/api/v1/messages/search?source=" + source + "&q=" + encodeURIComponent(query) + "&limit=50"
      : "/api/v1/messages/list?source=" + source + "&limit=50";
    const r = await apiGet<any>(path, 20000);
    setMsgs(r.ok ? r.data : { error: r.raw || r.error });
  };

  useEffect(() => {
    void loadNotifs();
    void loadMsgs();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [s.base, s.token]);

  const send = async () => {
    const r = await apiPost("/api/v1/notifications/post", { title: post.title, text: post.text, id: post.id });
    setNote(r.ok ? "已发送通知" : "发送失败：" + (r.raw || r.error));
    if (r.ok) void loadNotifs();
  };

  const rows: any[] = msgs?.messages || [];
  const cols = rows.length ? Object.keys(rows[0]).filter((k) => !k.startsWith("_") || k === "_id").slice(0, 7) : [];

  return (
    <div className="space-y-5">
      <Card
        title="通知"
        action={
          <div className="flex items-center gap-2">
            <SmallBtn onClick={() => setOngoing(!ongoing)}>{ongoing ? "含常驻" : "仅可清除"}</SmallBtn>
            <SmallBtn onClick={() => void loadNotifs()} disabled={busy}>
              {busy ? "读取中…" : "刷新"}
            </SmallBtn>
          </div>
        }
      >
        {err ? <Banner tone="warn">{err}</Banner> : null}
        {doc?.empty_reason ? <Banner tone="warn">{doc.empty_reason}</Banner> : null}
        {doc?.hint ? <p className="text-xs text-slate-500 dark:text-slate-500">{doc.hint}</p> : null}
        <div className="flex flex-wrap items-end gap-3">
          <Field label="条数" className="w-24">
            <input className={INPUT} type="number" value={limit} onInput={(e) => setLimit(Number((e.target as HTMLInputElement).value))} />
          </Field>
          <Field label="过滤（包名/标题/正文）" className="flex-1">
            <input className={INPUT} value={grep} onInput={(e) => setGrep((e.target as HTMLInputElement).value)} />
          </Field>
          <Btn onClick={() => void loadNotifs()}>应用</Btn>
        </div>
        <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
          <Stat label="当前通知" value={dash(doc?.count)} />
          <Stat label="总量（截断前）" value={dash(doc?.total_before_limit)} tone="muted" />
          <Stat label="涉及应用" value={doc?.packages ? Object.keys(doc.packages).length : "—"} tone="muted" />
          <Stat label="采集时间" value={doc?.captured_at ? String(doc.captured_at).slice(11, 19) : "—"} tone="muted" />
        </div>
        <div className="space-y-2">
          {(doc?.notifications || []).map((n) => (
            <div key={(n.key || "") + String(n.id)} className="rounded-lg border border-slate-200 dark:border-slate-700 p-3 space-y-1">
              <div className="flex items-center justify-between gap-2">
                <span className="text-sm font-medium text-slate-800 dark:text-slate-200">{dash(n.title) || dash(n.package)}</span>
                <span className="text-xs text-slate-500 dark:text-slate-500">{agoText(n.when)}</span>
              </div>
              {n.text ? <div className="text-sm text-slate-600 dark:text-slate-400 break-all">{n.text}</div> : null}
              <div className="flex flex-wrap gap-2 pt-1">
                <Pill tone="info">{dash(n.package)}</Pill>
                {n.ongoing ? <Pill>常驻</Pill> : null}
                {n.foreground_service ? <Pill>前台服务</Pill> : null}
                {n.clearable ? <Pill tone="good">可清除</Pill> : <Pill tone="bad">不可清除</Pill>}
                {n.importance ? <Pill>{n.importance}</Pill> : null}
                {n.channel ? <Pill tone="idle">{n.channel}</Pill> : null}
              </div>
            </div>
          ))}
          {!doc?.notifications?.length && !doc?.empty_reason ? (
            <Row label="当前没有通知" value="" tone="muted" />
          ) : null}
        </div>
      </Card>

      <Card title="发一条通知">
        <div className="grid sm:grid-cols-3 gap-3">
          <Field label="标题">
            <input className={INPUT} value={post.title} onInput={(e) => setPost({ ...post, title: (e.target as HTMLInputElement).value })} />
          </Field>
          <Field label="正文">
            <input className={INPUT} value={post.text} onInput={(e) => setPost({ ...post, text: (e.target as HTMLInputElement).value })} />
          </Field>
          <Field label="tag" hint="可选，同一 tag 覆盖">
            <input className={INPUT} value={post.id} onInput={(e) => setPost({ ...post, id: (e.target as HTMLInputElement).value })} />
          </Field>
        </div>
        <div className="flex items-center gap-3">
          <Btn variant="primary" onClick={() => void send()} disabled={!post.title && !post.text}>发送</Btn>
          {note ? <span className="text-sm text-slate-500 dark:text-slate-400">{note}</span> : null}
        </div>
      </Card>

      <Card
        title="短信 / 通话 / 联系人"
        action={
          <div className="flex items-center gap-2">
            <Seg
              value={src}
              onChange={(v) => {
                setSrc(v as any);
                void loadMsgs(v as any, q);
              }}
              options={[
                { value: "sms", label: "短信" },
                { value: "calls", label: "通话" },
                { value: "contacts", label: "联系人" },
              ]}
            />
            <SmallBtn onClick={() => void loadMsgs()}>刷新</SmallBtn>
          </div>
        }
      >
        <div className="flex items-end gap-3">
          <Field label="搜索" hint="走 /messages/search，仅短信">
            <input className={INPUT} value={q} onInput={(e) => setQ((e.target as HTMLInputElement).value)} />
          </Field>
          <Btn onClick={() => void loadMsgs(src, q)}>搜索</Btn>
          <Btn variant="outline" onClick={() => { setQ(""); void loadMsgs(src, ""); }}>清除</Btn>
        </div>
        {msgs?.empty_reason ? <Banner tone="warn">{msgs.empty_reason}</Banner> : null}
        {msgs?.error ? <Banner tone="warn">{String(msgs.error).slice(0, 300)}</Banner> : null}
        {rows.length ? (
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <TableHead cols={cols} />
              <tbody>
                {rows.slice(0, 50).map((row: any, i: number) => (
                  <tr key={row._id ?? i} className="border-b border-slate-100 dark:border-slate-800">
                    {cols.map((c) => (
                      <Td key={c}>{String(row[c] ?? "—").slice(0, 80)}</Td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : !msgs?.error && !msgs?.empty_reason ? (
          <Row label="没有读取到条目" value={msgs?.count !== undefined ? "count=" + msgs.count : ""} tone="muted" />
        ) : null}
        <p className="text-xs text-slate-500 dark:text-slate-500">
          这些数据来自手机本地数据库（<code>content query</code>）；读取不到通常是权限或没有 SIM 卡，
          后端会把原因写在 empty_reason 里。
        </p>
      </Card>
    </div>
  );
}
