/**
 * 配置 —— main.go 里 Config 的每一个字段都在这里，没有遗漏也没有多余。
 *
 * 字段表由 PATHS 声明（键路径 + 类型 + 说明），渲染和提交都从这张表走，所以
 * 加一个配置项只需要加一行；提交时只发被改动的那一份子树，避免整体覆盖。
 * 「允许局域网访问」是唯一能把服务暴露到网络上的开关，因此带二次确认。
 */
import { useEffect, useState } from "preact/hooks";
import { apiGet, rawRequest, shJSON, useApiState } from "../api";
import { Banner, Btn, Card, Field, INPUT, Modal, Row, SmallBtn, Toggle } from "../components/ui";

type Kind = "str" | "int" | "bool" | "size" | "enum";
interface Path {
  p: string;
  label: string;
  kind: Kind;
  help?: string;
  opts?: string[];
}

const PATHS: Path[] = [
  // 网络与安全
  { p: "listen", label: "监听地址", kind: "str", help: "127.0.0.1:8080 = 只有手机自己；改成 0.0.0.0:8080 才能被局域网访问（需要下面的开关）" },
  { p: "security.allow_non_loopback_listen", label: "允许局域网访问", kind: "bool", help: "打开后同网络里任何人都能访问这个服务，token 是唯一凭据" },
  { p: "security.allow_shell", label: "允许受控 shell", kind: "bool", help: "默认关。开了等于把 root shell 交给 token 持有者" },
  { p: "security.allow_power_commands", label: "允许关机/重启", kind: "bool" },
  { p: "security.shell_timeout_ms", label: "shell 超时", kind: "int", help: "毫秒" },
  { p: "security.shell_max_output_bytes", label: "shell 输出上限", kind: "int", help: "字节" },
  { p: "device_id", label: "设备标识", kind: "str", help: "留空则用手机序列号" },
  // 日志与缓存
  { p: "log_dir", label: "日志目录", kind: "str", help: "改这里需要重启" },
  { p: "log_requests", label: "记录每个请求", kind: "bool" },
  { p: "log_max_bytes", label: "单文件日志上限", kind: "size", help: "字节，超出后轮转" },
  { p: "getprop_ttl_seconds", label: "getprop 缓存", kind: "int", help: "秒" },
  { p: "wifi_ttl_seconds", label: "Wi-Fi 缓存", kind: "int", help: "秒" },
  { p: "power_ttl_seconds", label: "电源状态缓存", kind: "int", help: "秒" },
  { p: "packages_ttl_seconds", label: "应用列表缓存", kind: "int", help: "秒" },
  { p: "procs_ttl_seconds", label: "进程列表缓存", kind: "int", help: "秒" },
  { p: "storage_mounts", label: "统计的挂载点", kind: "str", help: "逗号分隔，如 /data,/storage/emulated/0" },
  // 控制 / 采集 / 串流
  { p: "control.enabled", label: "启用控制端点", kind: "bool" },
  { p: "control.default_timeout_ms", label: "控制默认超时", kind: "int", help: "毫秒" },
  { p: "control.max_timeout_ms", label: "控制最大超时", kind: "int", help: "毫秒" },
  { p: "capture.camera_enabled", label: "允许拍照", kind: "bool" },
  { p: "capture.mic_enabled", label: "允许录音", kind: "bool" },
  { p: "capture.default_camera", label: "默认摄像头", kind: "int", help: "0 后置 / 1 前置（按设备）" },
  { p: "capture.photo_format", label: "照片格式", kind: "enum", opts: ["jpeg", "webp", "png"] },
  { p: "capture.photo_quality", label: "照片质量", kind: "int", help: "1-100" },
  { p: "capture.photo_max_width", label: "照片最大宽度", kind: "int", help: "0 = 原始尺寸" },
  { p: "capture.preview_fps", label: "预览帧率", kind: "int" },
  { p: "capture.preview_max_width", label: "预览最大宽度", kind: "int" },
  { p: "capture.preview_quality", label: "预览质量", kind: "int" },
  { p: "capture.mic_default_seconds", label: "录音默认时长", kind: "int", help: "秒" },
  { p: "capture.mic_max_seconds", label: "录音最长时长", kind: "int", help: "秒" },
  { p: "capture.mic_encoder", label: "录音编码", kind: "enum", opts: ["aac", "amr"] },
  { p: "capture.mic_bitrate_kbps", label: "录音码率", kind: "int", help: "kbps" },
  { p: "capture.mic_rate_hz", label: "录音采样率", kind: "int", help: "Hz" },
  { p: "capture.mic_channels", label: "录音声道", kind: "int" },
  { p: "capture.keep_capture_files", label: "保留拍摄临时文件", kind: "bool", help: "默认关：拍完立刻删，响应里直接给字节" },
  { p: "stream.enabled", label: "启用实时画面", kind: "bool" },
  { p: "stream.max_clients", label: "并发观看数", kind: "int" },
  { p: "stream.default_size", label: "默认画面尺寸", kind: "str", help: "如 720x1560" },
  { p: "stream.default_bitrate", label: "默认码率", kind: "int", help: "bit/s，低延时低流量就调小" },
  { p: "stream.default_fps", label: "默认帧率", kind: "int" },
  { p: "stream.idle_timeout_seconds", label: "空闲断开", kind: "int", help: "秒，没有客户端后多久停编码" },
  { p: "stream.wake_display", label: "开流前唤醒屏幕", kind: "bool", help: "熄屏时 screenrecord 会报 INVALID_LAYER_STACK" },
  { p: "stream.keep_display_on", label: "观看期间常亮", kind: "bool" },
  { p: "stream.mjpeg_fps", label: "MJPEG 帧率", kind: "int", help: "降级通道：不支持 WebCodecs 的浏览器用" },
  { p: "stream.mjpeg_max_width", label: "MJPEG 最大宽度", kind: "int" },
  { p: "stream.mjpeg_quality", label: "MJPEG 质量", kind: "int" },
  { p: "notify.enabled", label: "启用通知读取", kind: "bool" },
  { p: "notify.include_ongoing", label: "默认包含常驻通知", kind: "bool" },
  { p: "notify.max", label: "通知条数上限", kind: "int" },
  { p: "notify.cache_seconds", label: "通知缓存", kind: "int", help: "秒" },
  { p: "mcp.http", label: "启用 MCP HTTP 端点", kind: "bool" },
  { p: "mcp.read_only", label: "MCP 只读模式", kind: "bool", help: "打开后 AI agent 只能读，不能操作手机" },
  { p: "mcp.path", label: "MCP 路径", kind: "str", help: "默认 /mcp" },
  { p: "bridge.enabled", label: "启用 Termux:API 桥", kind: "bool" },
  { p: "bridge.prefix", label: "Termux 前缀", kind: "str" },
  { p: "bridge.home", label: "Termux HOME", kind: "str" },
  { p: "bridge.tmp_dir", label: "桥临时目录", kind: "str" },
  { p: "bridge.auto_grant_permissions", label: "自动授权权限", kind: "bool", help: "auto-grant 6 个运行时权限（camera/mic/sms/calllog/contacts/notifications）" },
  { p: "bridge.keep_awake_for_mic", label: "录音时保持唤醒锁", kind: "bool", help: "麦克风需要 uid ≥ 前台服务" },
  { p: "bridge.foreground_for_camera", label: "拍照前拉 Termux 到前台", kind: "bool", help: "相机需要 uid 在前台" },
  { p: "bridge.wake_screen_for_camera", label: "拍照前亮屏", kind: "bool" },
  { p: "bridge.command_timeout_seconds", label: "桥命令超时", kind: "int", help: "秒" },
  { p: "bridge.tools_cache_seconds", label: "桥工具缓存", kind: "int", help: "秒" },
  { p: "files.enabled", label: "启用文件服务", kind: "bool" },
  { p: "files.http_files", label: "HTTP 文件浏览/上传", kind: "bool" },
  { p: "files.webdav", label: "WebDAV", kind: "bool", help: "可挂载成电脑上的网络驱动器" },
  { p: "files.read_only", label: "只读（禁写）", kind: "bool", help: "同时管住上传与目录操作" },
  { p: "files.allow_delete", label: "允许删除", kind: "bool" },
  { p: "files.max_upload_mb", label: "上传上限", kind: "int", help: "MB" },
  { p: "files.show_hidden", label: "显示隐藏文件", kind: "bool" },
  { p: "files.listing", label: "目录列表 HTML", kind: "bool" },
  { p: "files.max_list_entries", label: "列表条目上限", kind: "int" },
  { p: "history.enabled", label: "启用历史采样", kind: "bool" },
  { p: "history.db_path", label: "历史库路径", kind: "str", help: "/data/adb/phoneserver/ 下，升级不丢" },
  { p: "history.interval_seconds", label: "采样间隔", kind: "int", help: "秒（廉价采样：sysfs+proc）" },
  { p: "history.full_seconds", label: "全量文档间隔", kind: "int", help: "秒（含 getprop/dumpsys，重一些）" },
  { p: "history.retention_days", label: "样本保留", kind: "int", help: "天" },
  { p: "history.snapshot_retention_days", label: "快照保留", kind: "int", help: "天" },
  { p: "history.log_retention_days", label: "日志入库保留", kind: "int", help: "天" },
  { p: "history.max_db_mb", label: "库容量上限", kind: "int", help: "MB，超了会淘汰最老的行" },
  { p: "history.backfill_logs", label: "回填历史日志", kind: "bool" },
  { p: "history.prune_every_seconds", label: "清理与聚合间隔", kind: "int", help: "秒" },
  { p: "history.rollup_enabled", label: "小时级聚合", kind: "bool", help: "把每小时折成 avg/min/max/last，删原始样本后仍能看长期趋势" },
];

const get = (o: any, path: string) => path.split(".").reduce((x: any, k) => (x == null ? undefined : x[k]), o);
const setIn = (o: any, path: string, v: any) => {
  const parts = path.split(".");
  let cur = o;
  for (const k of parts.slice(0, -1)) {
    if (typeof cur[k] !== "object" || cur[k] === null) cur[k] = {};
    cur = cur[k];
  }
  cur[parts[parts.length - 1]] = v;
};

export default function Config() {
  const s = useApiState();
  const [cfg, setCfg] = useState<any>(null);
  const [raw, setRaw] = useState("");
  const [msg, setMsg] = useState<{ tone: "ok" | "warn" | "info"; text: string } | null>(null);
  const [dirty, setDirty] = useState<Set<string>>(new Set());
  const [shareAdd, setShareAdd] = useState({ name: "", path: "" });
  const [confirmLan, setConfirmLan] = useState(false);
  const [restarting, setRestarting] = useState(false);

  const load = async () => {
    const r = await apiGet<any>("/api/v1/config", 10000);
    if (r.ok && r.data) {
      const c = (r.data as any).config || r.data;
      setCfg(c);
      setRaw(JSON.stringify(c, null, 2));
      setDirty(new Set());
      setMsg({ tone: "info", text: "已读取当前配置（token 已脱敏，不会显示明文）" });
    } else {
      setMsg({ tone: "warn", text: r.error || "读取配置失败" });
    }
  };
  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [s.base, s.token]);

  const change = (p: string, v: any) => {
    const next = JSON.parse(JSON.stringify(cfg));
    setIn(next, p, v);
    setCfg(next);
    setDirty(new Set([...dirty, p]));
  };

  const buildPatch = (only?: string[]) => {
    const wanted = only ? new Set(only) : dirty;
    const patch: any = {};
    for (const p of wanted) setIn(patch, p, get(cfg, p));
    return patch;
  };

  const save = async (only?: string[], after?: () => Promise<void> | void) => {
    const body = buildPatch(only);
    if (!Object.keys(body).length) {
      setMsg({ tone: "info", text: "没有改动需要保存" });
      return;
    }
    const r = await rawRequest<any>("/api/v1/config", { method: "PUT", body });
    if (r.ok) {
      setMsg({ tone: "ok", text: "已保存：" + Object.keys(body).join(", ") + (after ? "；" : "") });
      await load();
      if (after) await after();
    } else {
      setMsg({ tone: "warn", text: "保存失败：" + (r.raw || r.error || "HTTP 错误") });
    }
  };

  const restart = async () => {
    setRestarting(true);
    const r = await shJSON<any>("restart", [], 30000);
    setRestarting(false);
    if (r.ok && r.data?.restarted) setMsg({ tone: "ok", text: "服务已重启，健康检查 " + r.data.health });
    else setMsg({ tone: "warn", text: "重启请求的结果：" + (r.raw || r.error || "无输出") + "（浏览器直连模式下需要在手机上手动重启，或用配置里的 restart）" });
  };

  const renderField = (f: Path) => {
    const v = get(cfg, f.p);
    if (f.kind === "bool") {
      return (
        <Toggle
          key={f.p}
          label={f.label}
          checked={!!v}
          hint={f.help}
          onChange={(b) => {
            if (f.p === "security.allow_non_loopback_listen" && b) setConfirmLan(true);
            change(f.p, b);
          }}
        />
      );
    }
    if (f.kind === "enum") {
      return (
        <Field key={f.p} label={f.label} hint={f.help}>
          <select className={INPUT} value={String(v ?? "")} onChange={(e) => change(f.p, (e.target as HTMLSelectElement).value)}>
            {(f.opts || []).map((o) => (
              <option key={o} value={o}>{o}</option>
            ))}
          </select>
        </Field>
      );
    }
    if (f.kind === "int" || f.kind === "size") {
      return (
        <Field key={f.p} label={f.label} hint={f.help}>
          <input
            className={INPUT}
            type="number"
            value={Number.isFinite(Number(v)) ? Number(v) : 0}
            onInput={(e) => change(f.p, Number((e.target as HTMLInputElement).value))}
          />
        </Field>
      );
    }
    return (
      <Field key={f.p} label={f.label} hint={f.help}>
        <input
          className={INPUT}
          value={Array.isArray(v) ? v.join(",") : v === undefined || v === null ? "" : String(v)}
          onInput={(e) =>
            change(
              f.p,
              Array.isArray(v)
                ? (e.target as HTMLInputElement).value.split(",").map((x) => x.trim()).filter(Boolean)
                : (e.target as HTMLInputElement).value,
            )
          }
        />
      </Field>
    );
  };

  const group = (title: string, paths: string[], extra?: any) => {
    if (!cfg) return null;
    return (
      <Card title={title} action={<SmallBtn onClick={() => void save(paths)}>保存本组</SmallBtn>}>
        <div className="grid sm:grid-cols-2 gap-3">
          {PATHS.filter((f) => paths.includes(f.p)).map(renderField)}
        </div>
        {extra}
      </Card>
    );
  };

  if (!cfg) {
    return (
      <Card title="配置">
        <Banner tone="info">读取中…如果长时间没反应，检查是否连上了服务端。</Banner>
      </Card>
    );
  }

  const loopback = String(cfg.listen || "").startsWith("127.") || String(cfg.listen || "").startsWith("localhost");
  const shares: Record<string, string> = cfg.files?.shares || {};

  return (
    <div className="space-y-5">
      {msg ? <Banner tone={msg.tone}>{msg.text}</Banner> : null}

      <Card title="网络与安全">
        <Banner tone={loopback && !cfg.security?.allow_non_loopback_listen ? "ok" : "warn"}>
          当前监听 <code>{String(cfg.listen)}</code>，局域网开关
          {cfg.security?.allow_non_loopback_listen ? "已打开" : "关闭"}。
          默认只监听本机；打开局域网访问后，同一网络里的任何设备都能访问这个 API，token 是唯一凭据。
        </Banner>
        <div className="grid sm:grid-cols-2 gap-3">
          {renderField(PATHS.find((f) => f.p === "listen")!)}
          {renderField(PATHS.find((f) => f.p === "security.allow_non_loopback_listen")!)}
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Btn variant="primary" onClick={() => void save(["listen", "security.allow_non_loopback_listen"])}>保存并应用</Btn>
          <Btn onClick={() => void restart()} disabled={restarting}>{restarting ? "重启中…" : "立即重启服务"}</Btn>
          <SmallBtn onClick={() => setCfg({ ...cfg, listen: "127.0.0.1:8080" })}>改回只监听本机</SmallBtn>
        </div>
        <div className="grid sm:grid-cols-2 gap-3">
          {PATHS.filter((f) => f.p.startsWith("security.") && f.p !== "security.allow_non_loopback_listen").map(renderField)}
        </div>
        <Row label="API token（脱敏显示）" value={String(cfg.api_token || "")} mono tone="muted" />
        {renderField(PATHS.find((f) => f.p === "device_id")!)}
        <p className="text-xs text-slate-500 dark:text-slate-500">
          重新生成 token：在手机上执行 <code>bin/phoneserver -init-config</code> 不会覆盖已有值，
          直接改 config.json 里的 api_token 再重启即可。旧 token 会立即失效。
        </p>
      </Card>

      {group("日志与缓存", ["log_dir", "log_requests", "log_max_bytes", "getprop_ttl_seconds", "wifi_ttl_seconds", "power_ttl_seconds", "packages_ttl_seconds", "procs_ttl_seconds", "storage_mounts"])}
      {group("控制", ["control.enabled", "control.default_timeout_ms", "control.max_timeout_ms"])}
      {group("相机与麦克风", PATHS.filter((f) => f.p.startsWith("capture.")).map((f) => f.p))}
      {group("实时画面", PATHS.filter((f) => f.p.startsWith("stream.")).map((f) => f.p))}
      {group("通知", PATHS.filter((f) => f.p.startsWith("notify.")).map((f) => f.p))}
      {group("MCP（给 AI agent）", PATHS.filter((f) => f.p.startsWith("mcp.")).map((f) => f.p))}
      {group("Termux:API 桥", PATHS.filter((f) => f.p.startsWith("bridge.")).map((f) => f.p))}

      <Card title="文件服务与 WebDAV" action={<SmallBtn onClick={() => void save(["files"])}>保存本组</SmallBtn>}>
        <div className="grid sm:grid-cols-2 gap-3">
          {PATHS.filter((f) => f.p.startsWith("files.") && f.p !== "files.shares").map(renderField)}
        </div>
        <div className="space-y-2">
          <div className="text-sm text-slate-500 dark:text-slate-400">共享目录（share 名 → 手机上的绝对路径）</div>
          {Object.keys(shares).map((name) => (
            <div className="flex items-center gap-2" key={name}>
              <input className={INPUT + " w-32"} value={name} readOnly />
              <input
                className={INPUT}
                value={shares[name]}
                onInput={(e) => {
                  const next = JSON.parse(JSON.stringify(cfg));
                  next.files.shares[name] = (e.target as HTMLInputElement).value;
                  setCfg(next);
                  setDirty(new Set([...dirty, "files"]));
                }}
              />
              <SmallBtn
                onClick={() => {
                  const next = JSON.parse(JSON.stringify(cfg));
                  delete next.files.shares[name];
                  setCfg(next);
                  setDirty(new Set([...dirty, "files"]));
                }}
              >
                删除
              </SmallBtn>
            </div>
          ))}
          <div className="flex items-center gap-2">
            <input className={INPUT + " w-32"} placeholder="share 名" value={shareAdd.name} onInput={(e) => setShareAdd({ ...shareAdd, name: (e.target as HTMLInputElement).value })} />
            <input className={INPUT} placeholder="/storage/emulated/0/Movies" value={shareAdd.path} onInput={(e) => setShareAdd({ ...shareAdd, path: (e.target as HTMLInputElement).value })} />
            <Btn
              onClick={() => {
                if (!shareAdd.name || !shareAdd.path) return;
                const next = JSON.parse(JSON.stringify(cfg));
                next.files.shares[shareAdd.name.replace(/[\/\\ ]/g, "")] = shareAdd.path;
                setCfg(next);
                setDirty(new Set([...dirty, "files"]));
                setShareAdd({ name: "", path: "" });
              }}
            >
              添加
            </Btn>
          </div>
        </div>
        <Row label="浏览入口" value={"http://" + (s.listen || cfg.listen) + "/files/"} mono tone="muted" />
        <Row label="WebDAV 挂载" value={"http://" + (s.listen || cfg.listen) + "/dav/<share>/"} mono tone="muted" />
      </Card>

      {group("历史数据", PATHS.filter((f) => f.p.startsWith("history.")).map((f) => f.p), (
        <p className="text-xs text-slate-500 dark:text-slate-500">
          采样间隔决定磁盘与电量的代价：便宜采样只读 sysfs/proc，全量采样会跑 getprop/iw/dumpsys。
          小时聚合打开后，即使原始样本被保留策略删掉，长期趋势仍然查得到。
        </p>
      ))}

      <Card title="完整配置（原始 JSON，只读）">
        <pre className="overflow-auto rounded-md border border-slate-200 dark:border-slate-700 bg-slate-50 dark:bg-black p-3 text-xs max-h-72 whitespace-pre-wrap break-all">{raw}</pre>
      </Card>

      <Modal open={confirmLan} onClose={() => setConfirmLan(false)} title="确认打开局域网访问？">
        <p className="text-sm text-slate-600 dark:text-slate-400">
          打开后，同一网络（家里/公司的 Wi-Fi）里的任何设备都能访问这个 API。唯一的凭据是 token，
          请确认 token 没有泄露，并且把 listen 改成 <code>0.0.0.0:8080</code> 后保存、重启服务。
        </p>
        <div className="flex justify-end gap-2 pt-3">
          <Btn
            variant="outline"
            onClick={() => {
              change("security.allow_non_loopback_listen", false);
              setConfirmLan(false);
            }}
          >
            取消
          </Btn>
          <Btn
            variant="primary"
            onClick={() => {
              change("security.allow_non_loopback_listen", true);
              change("listen", "0.0.0.0:8080");
              setConfirmLan(false);
            }}
          >
            我明白，打开
          </Btn>
        </div>
      </Modal>
    </div>
  );
}
