/**
 * The shell: header, navigation and the route switch.
 *
 * Multi-page on purpose — the user asked for one function per page instead of a
 * wall of cards, so every entry below renders a different screen and nothing
 * else. Routing is a hash so a page can be linked and survives a WebView
 * reload; the KernelSU WebView cannot do history.pushState reliably.
 */
import { useEffect, useState } from "preact/hooks";
import { boot, useApiState, touch, apiGet, servedOrigin, state as apiState } from "./api";
import { Banner, Btn, Card, Field, INPUT, Row, SmallBtn } from "./components/ui";
import Dashboard from "./pages/dashboard";
import Stream from "./pages/stream";
import Control from "./pages/control";
import Apps from "./pages/apps";
import Messages from "./pages/messages";
import Capture from "./pages/capture";
import Files from "./pages/files";
import History from "./pages/history";
import Endpoints from "./pages/endpoints";
import Config from "./pages/config";
import Logs from "./pages/logs";

export const ROUTES = [
  { id: "dashboard", label: "概览", view: Dashboard },
  { id: "stream", label: "实时画面", view: Stream },
  { id: "control", label: "操作控制", view: Control },
  { id: "apps", label: "应用", view: Apps },
  { id: "messages", label: "消息与通知", view: Messages },
  { id: "capture", label: "相机与麦克风", view: Capture },
  { id: "files", label: "文件与 WebDAV", view: Files },
  { id: "history", label: "历史数据", view: History },
  { id: "endpoints", label: "接口与 MCP", view: Endpoints },
  { id: "config", label: "配置", view: Config },
  { id: "logs", label: "日志", view: Logs },
] as const;

type RouteId = (typeof ROUTES)[number]["id"];

function routeFromHash(): RouteId {
  const raw = (window.location.hash || "").replace(/^#\/?/, "").split("?")[0];
  const hit = ROUTES.find((r) => r.id === raw);
  return (hit ? hit.id : "dashboard") as RouteId;
}

export function useRoute(): [RouteId, (id: RouteId) => void] {
  const [route, setRoute] = useState<RouteId>(routeFromHash);
  useEffect(() => {
    const on = () => setRoute(routeFromHash());
    window.addEventListener("hashchange", on);
    return () => window.removeEventListener("hashchange", on);
  }, []);
  return [
    route,
    (id: RouteId) => {
      window.location.hash = "/" + id;
      setRoute(id);
    },
  ];
}

export function go(id: RouteId): void {
  window.location.hash = "/" + id;
}

function PageMissing({ id }: { id: string }) {
  return (
    <Card title={"页面还没有实现：" + id}>
      <p className="text-sm text-slate-500 dark:text-slate-400">
        这个页面文件存在但还没有内容，说明构建时它是个占位。请重新构建 WebUI。
      </p>
    </Card>
  );
}

export function App() {
  const s = useApiState();
  const [route, nav] = useRoute();
  const [conn, setConn] = useState<{ ok: boolean; text: string }>({ ok: false, text: "连接中…" });
  const [showSetup, setShowSetup] = useState(false);
  const [base, setBase] = useState(apiState.base);
  const [tok, setTok] = useState(apiState.token);

  useEffect(() => {
    void boot();
  }, []);

  // A quick liveness probe after boot, so the header never claims a connection
  // that is not there. Errors are shown verbatim instead of being swallowed.
  useEffect(() => {
    if (!s.ready) return;
    let alive = true;
    const probe = async () => {
      const r = await apiGet<{ server_version?: string }>("/health", 6000);
      if (!alive) return;
      setConn(
        r.ok
          ? { ok: true, text: "已连接 " + (r.data?.server_version ? "v" + r.data.server_version : "") }
          : { ok: false, text: r.error || "无法连接" },
      );
    };
    void probe();
    const t = window.setInterval(probe, 20000);
    return () => {
      alive = false;
      window.clearInterval(t);
    };
  }, [s.ready, s.base, s.token]);

  const View = (ROUTES.find((r) => r.id === route)?.view ?? null) as
    | ((p: Record<string, never>) => preact.JSX.Element)
    | null;

  const saveSetup = async () => {
    apiState.base = base.trim().replace(/\/+$/, "");
    apiState.token = tok.trim();
    try {
      window.localStorage.setItem("phoneserver.base", apiState.base);
      window.localStorage.setItem("phoneserver.token", apiState.token);
      window.localStorage.setItem("phoneserver.mode", "http");
      // Typing an address is a deliberate choice: keep it even though a served
      // page would otherwise follow its own origin.
      window.localStorage.setItem("phoneserver.base.manual", "1");
    } catch {
      /* private mode */
    }
    apiState.mode = "http";
    touch();
    setShowSetup(false);
  };

  return (
    <div className="min-h-screen bg-slate-50 dark:bg-black text-slate-800 dark:text-slate-200">
      <header className="sticky top-0 z-40 border-b border-slate-200 dark:border-slate-700 bg-white dark:bg-black">
        <div className="max-w-5xl mx-auto px-4 py-3 flex items-center justify-between gap-3">
          <div className="min-w-0">
            <h1 className="font-semibold text-slate-900 dark:text-slate-100 truncate">手机控制台</h1>
            <p className="text-xs text-slate-500 dark:text-slate-500 truncate">
              {s.mode === "ksu" ? "管理器 WebView 模式" : "浏览器直连模式"} · {s.base}
              {s.version ? " · v" + s.version : ""}
            </p>
          </div>
          <div className="flex items-center gap-2 shrink-0">
            <span
              className={
                "text-xs px-2 py-1 rounded-md border " +
                (conn.ok
                  ? "border-emerald-300 dark:border-emerald-700 text-emerald-600 dark:text-emerald-400"
                  : "border-rose-300 dark:border-rose-700 text-rose-500 dark:text-rose-400")
              }
            >
              {conn.ok ? "在线" : "离线"}
            </span>
            <SmallBtn onClick={() => setShowSetup(true)}>连接</SmallBtn>
          </div>
        </div>
        <nav className="max-w-5xl mx-auto px-4 pb-2 flex gap-1 overflow-x-auto">
          {ROUTES.map((r) => (
            <button
              key={r.id}
              type="button"
              onClick={() => nav(r.id)}
              className={
                "px-2.5 py-1.5 rounded-lg text-sm whitespace-nowrap transition-colors " +
                (route === r.id
                  ? "bg-sky-600 text-white"
                  : "text-slate-600 dark:text-slate-400 hover:bg-slate-100 dark:hover:bg-black")
              }
            >
              {r.label}
            </button>
          ))}
        </nav>
      </header>

      <main className="max-w-5xl mx-auto px-4 py-6 space-y-5">
        {!conn.ok && (
          <Banner tone="warn">
            连不上服务端：{conn.text}. 服务端默认只监听 127.0.0.1，浏览器要在手机本机打开，或者先在配置页里允许局域网访问。
          </Banner>
        )}
        {s.bootError ? <Banner tone="warn">{s.bootError}</Banner> : null}
        {View ? <View /> : <PageMissing id={route} />}
      </main>

      <footer className="max-w-5xl mx-auto px-4 pb-8 text-xs text-slate-500 dark:text-slate-500">
        所有数据留在这台手机上；本服务不上传任何内容到远端。
      </footer>

      {showSetup && (
        <div className="fixed inset-0 z-50 bg-black/50 p-4">
          <div className="flex min-h-full items-center justify-center">
            <div className="w-full max-w-md rounded-xl border border-slate-200 dark:border-slate-700 bg-white dark:bg-black p-4 shadow-xl space-y-3">
              <h2 className="font-semibold text-slate-900 dark:text-slate-100">连接设置</h2>
              <Field label="API 地址" hint="浏览器直连模式用">
                <input className={INPUT} value={base} onInput={(e) => setBase((e.target as HTMLInputElement).value)} />
              </Field>
              <Field label="API token" hint="配置页可查看，或 grep api_token config.json">
                <input
                  className={INPUT}
                  type="password"
                  value={tok}
                  onInput={(e) => setTok((e.target as HTMLInputElement).value)}
                />
              </Field>
              {servedOrigin() ? (
                <Row label="页面地址（推荐）" value={servedOrigin()} mono tone="good" />
              ) : (
                <Row label="页面来源" value="本地文件（管理器 WebView 或下载的页面）" tone="muted" />
              )}
              <Row label="当前模式" value={s.mode === "ksu" ? "管理器 WebView" : "浏览器直连"} tone="muted" />
              <div className="flex justify-end gap-2">
                {servedOrigin() ? (
                  <Btn
                    variant="outline"
                    onClick={() => {
                      setBase(servedOrigin());
                      setTok(apiState.token);
                      try {
                        window.localStorage.removeItem("phoneserver.base.manual");
                        window.localStorage.setItem("phoneserver.base", servedOrigin());
                      } catch {
                        /* private mode */
                      }
                      apiState.base = servedOrigin();
                      touch();
                    }}
                  >
                    用当前页面地址
                  </Btn>
                ) : null}
                <Btn variant="outline" onClick={() => setShowSetup(false)}>
                  取消
                </Btn>
                <Btn variant="primary" onClick={() => void saveSetup()}>
                  保存并切换直连
                </Btn>
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
