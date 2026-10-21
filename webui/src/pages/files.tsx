/**
 * 文件与 WebDAV —— 浏览共享目录、下载、上传、新建/重命名/删除，以及挂载地址。
 *
 * share 名是唯一可以出现在 URL 里的东西；服务端永远不接受裸文件系统路径，
 * 也从 share 根之外读任何文件（越权尝试会被 400 挡掉）。
 */
import { useEffect, useState } from "preact/hooks";
import { apiGet, apiURL, fileURL, rawRequest, state as apiState, useApiState } from "../api";
import { fmtBytes } from "../utils";
import { Banner, Btn, Card, CopyBtn, Field, INPUT, Pill, Row, SmallBtn, Stat, TableHead, Td, Toggle } from "../components/ui";

interface Share {
  name: string;
  path: string;
  exists?: boolean;
  readable?: boolean;
  writable?: boolean;
  entries?: number;
}
interface Entry {
  name: string;
  type?: string;
  size?: number;
  mtime?: string;
  mode?: string;
}

export default function Files() {
  const s = useApiState();
  const [root, setRoot] = useState<any>(null);
  const [share, setShare] = useState("");
  const [path, setPath] = useState("");
  const [listing, setListing] = useState<any>(null);
  const [err, setErr] = useState("");
  const [note, setNote] = useState("");
  const [newName, setNewName] = useState("");
  const [busy, setBusy] = useState(false);
  const [hidden, setHidden] = useState(false);

  const loadRoot = async () => {
    // The file service lives at /files/, NOT under /api/v1 — apiGet would request
    // /api/v1/files/ and get a 404 (seen in the browser during verification).
    const r = await rawRequest<any>("/files/", { timeoutMs: 15000 });
    if (r.ok) {
      setRoot(r.data);
      const shares: Share[] = (r.data as any).shares || [];
      if (!share && shares.length) setShare(shares[0].name);
      setErr("");
    } else setErr(r.raw || r.error || "无法读取 /files/");
  };

  const browse = async (sh = share, p = path) => {
    if (!sh) return;
    setBusy(true);
    const r = await rawRequest<any>("/files/" + sh + "/" + p + "?json=1", { timeoutMs: 20000 });
    setBusy(false);
    if (r.ok && r.data) {
      // A single file answers with meta, a directory with entries.
      if ((r.data as any).entries) setListing(r.data);
      else setListing({ count: 1, entries: [{ name: (r.data as any).name || p, type: "file", size: (r.data as any).size }], single: r.data });
      setShare(sh);
      setPath(p);
      setErr("");
    } else {
      setErr(r.raw || r.error || "浏览失败");
    }
  };

  useEffect(() => {
    void loadRoot();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [s.base, s.token]);

  useEffect(() => {
    if (share) void browse(share, "");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [share, s.base, s.token]);

  const op = async (label: string, target: string, query: string) => {
    // The target of mkcol/delete/move is the URL path, not a query parameter:
    // POST /files/<share>/<target>?op=mkcol|delete|move&dest=<share>/<path>.
    const r = await rawRequest<any>("/files/" + share + "/" + target + query, { method: "POST" });
    setNote(r.ok ? label + " 成功" : label + " 失败：" + (r.raw || r.error || ""));
    if (r.ok) void browse();
  };

  const upload = async (f: File) => {
    const fd = new FormData();
    fd.append("file", f, f.name);
    const res = await fetch(apiURL("/files/" + share + "/" + path + "/"), {
      method: "POST",
      headers: apiState.token ? { "X-API-Key": apiState.token } : {},
      body: fd,
    });
    const text = await res.text();
    setNote(res.ok ? "已上传 " + f.name + "（" + fmtBytes(f.size) + "）" : "上传失败 HTTP " + res.status + "：" + text.slice(0, 200));
    void browse();
  };

  const entries: Entry[] = (listing?.entries || []).filter((e: Entry) => hidden || !e.name.startsWith("."));
  const parent = path.replace(/\/$/, "").split("/").slice(0, -1).join("/");
  const crumbs = path.replace(/\/$/, "").split("/").filter(Boolean);
  const base = apiState.base || s.base;

  return (
    <div className="space-y-5">
      <Card
        title="共享目录"
        action={<SmallBtn onClick={() => void loadRoot()}>刷新</SmallBtn>}
      >
        {err ? <Banner tone="warn">{err}</Banner> : null}
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <TableHead cols={["share", "手机路径", "可读", "可写", "条目数", ""]} />
            <tbody>
              {(root?.shares || []).map((sh: Share) => (
                <tr key={sh.name} className={"border-b border-slate-100 dark:border-slate-800 " + (sh.name === share ? "bg-slate-50 dark:bg-black" : "")}>
                  <Td mono>{sh.name}</Td>
                  <Td mono>{sh.path}</Td>
                  <Td>{sh.readable === false ? "否" : "是"}</Td>
                  <Td>{sh.writable === false ? "否" : "是"}</Td>
                  <Td mono>{sh.entries ?? "—"}</Td>
                  <Td>
                    <SmallBtn onClick={() => { setShare(sh.name); setPath(""); }}>浏览</SmallBtn>
                  </Td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <div className="flex flex-wrap gap-2">
          <Pill tone={root?.read_only ? "bad" : "good"}>写操作 {root?.read_only ? "已禁用（read_only）" : "允许"}</Pill>
          <Pill tone={root?.allow_delete ? "good" : "idle"}>删除 {root?.allow_delete ? "允许" : "禁用（allow_delete=false）"}</Pill>
          <Pill tone="info">上传上限 {root?.max_upload_mb ?? "—"} MB</Pill>
        </div>
        {root?.hint ? <p className="text-xs text-slate-500 dark:text-slate-500">{root.hint}</p> : null}
      </Card>

      <Card
        title={"浏览 " + (share || "—") + " / " + (path || "（根）")}
        action={
          <div className="flex items-center gap-2">
            <Toggle label="显示隐藏文件" checked={hidden} onChange={setHidden} />
            <SmallBtn onClick={() => void browse()} disabled={busy}>{busy ? "读取中…" : "刷新"}</SmallBtn>
          </div>
        }
      >
        <div className="flex flex-wrap items-center gap-1 text-sm">
          <Btn variant="quiet" onClick={() => void browse(share, "")}>根</Btn>
          {crumbs.map((c, i) => (
            <span key={i} className="flex items-center gap-1">
              <span className="text-slate-400">/</span>
              <button
                className="text-sky-600 dark:text-sky-400 hover:underline"
                onClick={() => void browse(share, crumbs.slice(0, i + 1).join("/"))}
              >
                {c}
              </button>
            </span>
          ))}
        </div>
        <div className="flex flex-wrap items-end gap-3">
          <Field label="新目录名">
            <input className={INPUT} value={newName} onInput={(e) => setNewName((e.target as HTMLInputElement).value)} />
          </Field>
          <Btn onClick={() => void op("新建目录", (path ? path.replace(/\/$/, "") + "/" : "") + newName, "?op=mkcol")} disabled={!newName}>新建</Btn>
          <Btn variant="outline" onClick={() => void op("重命名", path, "?op=move&dest=" + encodeURIComponent(share + "/" + (parent ? parent + "/" : "") + newName))} disabled={!newName}>
            把当前目录重命名为该名
          </Btn>
          <Btn variant="danger" onClick={() => void op("删除当前目录", path, "?op=delete")}>删除当前目录</Btn>
          <label className="inline-flex items-center gap-2 text-sm text-slate-600 dark:text-slate-400">
            <input
              type="file"
              className="text-sm"
              onChange={(e) => {
                const f = (e.target as HTMLInputElement).files?.[0];
                if (f) void upload(f);
              }}
            />
          </label>
        </div>
        {note ? <Banner tone={note.includes("失败") ? "warn" : "ok"}>{note}</Banner> : null}
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <TableHead cols={["名称", "类型", "大小", "修改时间", "操作"]} />
            <tbody>
              {parent !== "" || path ? (
                <tr className="border-b border-slate-100 dark:border-slate-800">
                  <Td>
                    <button className="text-sky-600 dark:text-sky-400 hover:underline" onClick={() => void browse(share, parent)}>.. 上一级</button>
                  </Td>
                  <Td>—</Td>
                  <Td>—</Td>
                  <Td>—</Td>
                  <Td>—</Td>
                </tr>
              ) : null}
              {entries.map((e) => {
                const full = (path ? path.replace(/\/$/, "") + "/" : "") + e.name;
                const isDir = e.type === "directory" || e.type === "dir";
                return (
                  <tr key={e.name} className="border-b border-slate-100 dark:border-slate-800">
                    <Td>
                      {isDir ? (
                        <button className="text-sky-600 dark:text-sky-400 hover:underline" onClick={() => void browse(share, full)}>
                          {e.name}/
                        </button>
                      ) : (
                        <a className="text-slate-800 dark:text-slate-200 hover:underline" href={fileURL(share, full)} target="_blank" rel="noreferrer">
                          {e.name}
                        </a>
                      )}
                    </Td>
                    <Td>{isDir ? "目录" : e.type || "文件"}</Td>
                    <Td mono>{isDir ? "—" : fmtBytes(e.size)}</Td>
                    <Td mono>{e.mtime ? String(e.mtime).replace("T", " ").slice(0, 16) : "—"}</Td>
                    <Td>
                      {!isDir ? (
                        <>
                          <a className="text-xs text-slate-500 hover:text-slate-800 dark:text-slate-400" href={fileURL(share, full, "download=1")} target="_blank" rel="noreferrer">下载</a>
                          <span className="text-slate-300 dark:text-slate-700 px-1">|</span>
                        </>
                      ) : null}
                      <button
                        className="text-xs text-rose-500 hover:underline"
                        onClick={() => void op("删除 " + e.name, full, "?op=delete")}
                      >
                        删除
                      </button>
                    </Td>
                  </tr>
                );
              })}
              {!entries.length ? (
                <tr>
                  <Td>（空目录）</Td>
                  <Td>—</Td>
                  <Td>—</Td>
                  <Td>—</Td>
                  <Td>—</Td>
                </tr>
              ) : null}
            </tbody>
          </table>
        </div>
      </Card>

      <Card title="挂载为电脑上的网络驱动器（WebDAV）">
        <Row label="地址" value={base + "/dav/" + (share || "<share>") + "/"} mono />
        <div className="flex flex-wrap gap-2">
          <CopyBtn value={base + "/dav/" + (share || "") + "/"} label="复制地址" />
        </div>
        <div className="text-sm text-slate-600 dark:text-slate-400 space-y-1">
          <div>Windows：映射网络驱动器 → <code>{base + "/dav/" + (share || "sdcard") + "/"}</code></div>
          <div>macOS/Linux：<code>mount -t davfs {base + "/dav/" + (share || "sdcard") + "/"} /mnt/phone</code></div>
          <div>认证：用户名任意，密码填 API token（HTTP Basic）。</div>
        </div>
        <Banner tone="info">
          LOCK/UNLOCK 会被回答但不强制（单用户场景下够用）；删除需要 files.allow_delete=true，
          写操作受 files.read_only 控制，两个开关都在「配置」页。
        </Banner>
      </Card>
    </div>
  );
}
