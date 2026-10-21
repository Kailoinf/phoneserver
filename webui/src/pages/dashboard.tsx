/**
 * 概览 —— 一个设备一张页面：身份、电量与温度、性能、存储、网络、历史库状态。
 *
 * 一次 /api/v1/all + /health + /history/stats，自动刷新可关。所有数字直接来自
 * 响应，缺字段就显示 "—" 而不是编一个值。
 */
import { useEffect, useState } from "preact/hooks";
import { apiGet, useApiState } from "../api";
import { fmtBytes, fmtDur, fmtHz, fmtAgo, fmtNum, fmtPct, fmtRate } from "../utils";
import { Banner, Bar, Card, Pill, Row, SmallBtn, Stat, TableHead, Td } from "../components/ui";

interface AllDoc {
  collected_at?: string;
  device_id?: string;
  server_version?: string;
  server_uptime_seconds?: number;
  device?: Record<string, any>;
  battery?: Record<string, any>;
  power?: Record<string, any>;
  display?: Record<string, any>;
  cpu?: Record<string, any>;
  gpu?: Record<string, any>;
  memory?: Record<string, any>;
  storage?: any[];
  thermal?: Record<string, any>;
  network?: Record<string, any>;
  wifi?: Record<string, any>;
  system?: Record<string, any>;
  processes?: Record<string, any>;
  packages?: Record<string, any>;
}
interface Health {
  status?: string;
  server_version?: string;
  listen?: string;
  port?: string | number;
  lan_reachable?: boolean;
  auth_required?: boolean;
  stream_enabled?: boolean;
  mcp_http?: boolean;
  file_shares?: string[];
  file_service?: Record<string, any>;
  capture?: Record<string, any>;
  history_enabled?: boolean;
  server_uptime_s?: number;
}
interface HStats {
  enabled?: boolean;
  ticks?: number;
  full_ticks?: number;
  interval_seconds?: number;
  full_seconds?: number;
  store?: Record<string, any>;
  cost_estimate?: Record<string, any>;
  timing?: Record<string, any>;
}

const dash = (v: any) => (v === undefined || v === null || v === "" ? "—" : v);

export default function Dashboard() {
  const s = useApiState();
  const [all, setAll] = useState<AllDoc | null>(null);
  const [health, setHealth] = useState<Health | null>(null);
  const [stats, setStats] = useState<HStats | null>(null);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [auto, setAuto] = useState(true);
  const [when, setWhen] = useState("");

  const load = async () => {
    setBusy(true);
    const [a, h, st] = await Promise.all([
      apiGet<AllDoc>("/api/v1/all", 20000),
      apiGet<Health>("/api/v1/health", 8000),
      apiGet<HStats>("/api/v1/history/stats", 8000),
    ]);
    if (a.ok && a.data) setAll(a.data as AllDoc);
    if (h.ok && h.data) setHealth(h.data as Health);
    if (st.ok && st.data) setStats(st.data as HStats);
    setErr(a.ok ? "" : a.error || "无法读取 /api/v1/all");
    setWhen(new Date().toLocaleTimeString());
    setBusy(false);
  };

  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [s.base, s.token]);

  useEffect(() => {
    if (!auto) return;
    const t = window.setInterval(() => void load(), 10000);
    return () => window.clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [auto, s.base, s.token]);

  const d = all?.device || {};
  const b = all?.battery || {};
  const cpu = all?.cpu || {};
  const gpu = all?.gpu || {};
  const mem = all?.memory || {};
  const th = all?.thermal || {};
  const wifi = all?.wifi || {};
  const sys = all?.system || {};
  const net = all?.network || {};
  const st = stats?.store || {};
  const cap = Number(b.capacity_percent ?? NaN);

  return (
    <div className="space-y-5">
      <Card
        title="设备"
        action={
          <div className="flex items-center gap-2">
            <span className="text-xs text-slate-500 dark:text-slate-400">{when && "更新于 " + when}</span>
            <SmallBtn onClick={() => setAuto(!auto)}>{auto ? "暂停刷新" : "自动刷新"}</SmallBtn>
            <SmallBtn onClick={() => void load()} disabled={busy}>
              {busy ? "读取中…" : "刷新"}
            </SmallBtn>
          </div>
        }
      >
        {err ? <Banner tone="warn">{err}</Banner> : null}
        <div className="grid grid-cols-2 sm:grid-cols-3 gap-3">
          <Stat label="型号" value={dash(d.market_name || d.model)} />
          <Stat label="Android" value={dash(d.android_release)} unit={d.android_sdk ? "SDK " + d.android_sdk : ""} />
          <Stat label="内核版本" value={dash(d.kernel || (sys.kernel ? String(sys.kernel).match(/Linux version (\S+)/)?.[1] : ""))} />
          <Stat label="设备代号" value={dash(d.device)} />
          <Stat label="开机时长" value={fmtDur(sys.uptime_seconds)} tone="muted" />
          <Stat label="服务" value={health?.server_version ? "v" + health.server_version : "—"} tone={health ? "good" : "bad"} />
        </div>
        <div className="flex flex-wrap gap-2 pt-1">
          <Pill tone={all?.display?.screen_on ? "good" : "idle"}>{all?.display?.screen_on ? "屏幕亮" : "屏幕灭"}</Pill>
          <Pill tone={health?.listen?.startsWith("127.") ? "good" : "bad"}>
            {health?.listen ? "监听 " + health.listen : "监听 —"}
          </Pill>
          <Pill tone={health?.stream_enabled ? "info" : "idle"}>实时画面 {health?.stream_enabled ? "已启用" : "关闭"}</Pill>
          <Pill tone={health?.mcp_http ? "info" : "idle"}>MCP {health?.mcp_http ? "/mcp" : "关闭"}</Pill>
          <Pill tone={st.enabled || health?.history_enabled ? "info" : "idle"}>历史采样 {health?.history_enabled ? "开" : "关"}</Pill>
          <Pill tone={health?.capture?.camera ? "info" : "idle"}>相机 {health?.capture?.camera ? "可用" : "不可用"}</Pill>
        </div>
      </Card>

      <Card title="电量与温度">
        <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
          <Stat label="电量" value={Number.isFinite(cap) ? cap : "—"} unit="%" tone={cap <= 15 ? "bad" : "normal"} />
          <Stat label="电池温度" value={dash(b.temp_c)} unit="°C" />
          <Stat label="电流" value={dash(b.current_now_a)} unit="A" />
          <Stat label="循环次数" value={dash(b.cycle_count)} unit="次" />
        </div>
        {Number.isFinite(cap) ? <Bar pct={cap} tone={cap <= 15 ? "rose" : "emerald"} /> : null}
        <div className="grid sm:grid-cols-2 gap-x-6 gap-y-2">
          <Row label="状态" value={dash(b.status)} />
          <Row label="健康度" value={dash(b.health)} tone={b.health === "Good" ? "good" : "normal"} />
          <Row label="电压" value={dash(b.voltage_v) + " V"} mono />
          <Row label="充/放完还剩" value={b.time_to_empty_seconds ? fmtDur(b.time_to_empty_seconds) : "—"} tone="muted" />
          <Row label="当前电量容量" value={fmtNum(b.charge_counter_mah, 0) + " / " + fmtNum(b.charge_full_mah, 0) + " mAh"} mono />
          <Row label="设计容量" value={fmtNum(b.charge_full_design_mah, 0) + " mAh"} mono />
        </div>
      </Card>

      <Card title="性能">
        <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
          <Stat label="CPU 占用" value={fmtPct(cpu.usage_percent)} />
          <Stat label="GPU 占用" value={fmtNum(gpu.busy_percent, 0)} unit="%" />
          <Stat label="GPU 频率" value={fmtHz(Number(gpu.current_hz) / 1e6)} />
          <Stat label="GPU 温度" value={dash(gpu.temp_c)} unit="°C" />
        </div>
        {Array.isArray(cpu.per_core_percent) && cpu.per_core_percent.length ? (
          <div className="space-y-1">
            {cpu.per_core_percent.map((p: number, i: number) => (
              <div className="flex items-center gap-2" key={i}>
                <span className="w-14 text-xs text-slate-500 dark:text-slate-400 tabular-nums">核心 {i}</span>
                <div className="flex-1">
                  <Bar pct={Number(p) || 0} tone={Number(p) > 85 ? "rose" : "sky"} />
                </div>
                <span className="w-24 text-xs text-right text-slate-500 dark:text-slate-400 tabular-nums">
                  {fmtPct(p)} · {fmtHz(Number(cpu.cores?.[i]?.cur_khz) / 1000)}
                </span>
              </div>
            ))}
          </div>
        ) : null}
        <div className="grid sm:grid-cols-2 gap-x-6 gap-y-2">
          <Row label="SoC" value={dash(cpu.model)} />
          <Row label="核心数" value={dash(cpu.core_count)} />
          <Row label="内存" value={fmtBytes(Number(mem.total_kb) * 1024) + " 总 / 已用 " + fmtPct(mem.used_percent)} />
          <Row label="可用内存" value={fmtBytes(Number(mem.available_kb) * 1024)} />
        </div>
      </Card>

      <Card title="温度与存储">
        <div className="grid grid-cols-2 sm:grid-cols-3 gap-3">
          <Stat label="最高温" value={dash(th.max_temp_c)} unit="°C" />
          <Stat label="最热区域" value={dash(th.hottest_zone)} />
          <Stat label="温度区数" value={dash(th.temperature_zone_count)} tone="muted" />
        </div>
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <TableHead cols={["挂载点", "文件系统", "总量", "可用", "已用"]} />
            <tbody>
              {(all?.storage || []).map((m: any) => (
                <tr key={m.mount} className="border-b border-slate-100 dark:border-slate-800">
                  <Td mono>{m.mount}</Td>
                  <Td>{m.fstype}{m.writable ? "" : " · 只读"}</Td>
                  <Td mono>{fmtBytes(m.total_bytes)}</Td>
                  <Td mono>{m.writable ? fmtBytes(m.free_bytes) : "—"}</Td>
                  <Td mono>{fmtPct(m.used_percent)}</Td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Card>

      <Card title="网络">
        <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
          <Stat label="Wi-Fi" value={wifi.connected ? dash(wifi.ssid) : wifi.enabled ? "未连接" : "关闭"} />
          <Stat label="信号" value={dash(wifi.rssi_dbm)} unit="dBm" />
          <Stat label="链路速率" value={dash(wifi.link_speed_mbps)} unit="Mbps" />
          <Stat label="频段" value={dash(wifi.band)} unit={wifi.channel ? "信道 " + wifi.channel : ""} />
        </div>
        <div className="grid sm:grid-cols-2 gap-x-6 gap-y-2">
          <Row label="本机 IP" value={dash(wifi.ipv4 || (health as any)?.lan_ip)} mono />
          <Row label="协议" value={dash(wifi.standard)} tone="muted" />
          <Row label="是否计费网络" value={wifi.metered === undefined ? "—" : wifi.metered ? "是" : "否"} />
          <Row label="认证状态" value={dash(wifi.supplicant_state)} tone="muted" />
        </div>
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <TableHead cols={["接口", "状态", "下行", "上行", "累计收/发"]} />
            <tbody>
              {(net.interfaces || [])
                .filter((i: any) => i.up || Number(i.rx_bytes) > 0)
                .slice(0, 6)
                .map((i: any) => (
                  <tr key={i.name} className="border-b border-slate-100 dark:border-slate-800">
                    <Td mono>{i.name}</Td>
                    <Td>{i.up ? "up" : "down"}</Td>
                    <Td mono>{fmtRate(i.rx_bytes_per_sec)}</Td>
                    <Td mono>{fmtRate(i.tx_bytes_per_sec)}</Td>
                    <Td mono>{fmtBytes(i.rx_bytes) + " / " + fmtBytes(i.tx_bytes)}</Td>
                  </tr>
                ))}
            </tbody>
          </table>
        </div>
      </Card>

      <Card title="系统与历史库">
        <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
          <Stat label="进程数" value={dash(all?.processes?.count)} tone="muted" />
          <Stat label="已安装应用" value={dash(all?.packages?.total)} unit={all?.packages ? "用户 " + all.packages.user : ""} />
          <Stat label="历史库占用" value={fmtBytes(Number(st.db_mb) * 1024 * 1024)} />
          <Stat label="采样节奏" value={dash(stats?.interval_seconds)} unit={"s / 全量 " + dash(stats?.full_seconds) + "s"} />
        </div>
        <div className="grid sm:grid-cols-2 gap-x-6 gap-y-2">
          <Row label="开机时间" value={dash(sys.boot_time)} mono />
          <Row label="样本行数" value={dash(st.samples)} mono />
          <Row label="快照行数" value={dash(st.snapshots)} mono />
          <Row label="日志行数" value={dash(st.logs)} mono />
          <Row label="最早样本" value={st.oldest_ts ? fmtAgo(Math.floor(Date.now() / 1000) - Number(st.oldest_ts)) : "—"} tone="muted" />
          <Row
            label="预计到容量上限"
            value={st.est_days_to_cap ? fmtNum(st.est_days_to_cap) + " 天" : "基准未满 30 分钟"}
            tone="muted"
          />
          <Row label="本页采样耗时" value={stats?.timing?.last_collect_ms !== undefined ? stats.timing.last_collect_ms + " ms" : "—"} tone="muted" />
          <Row
            label="单日 CPU 估算"
            value={stats?.cost_estimate?.avg_cpu_percent_of_core !== undefined ? fmtPct(stats.cost_estimate.avg_cpu_percent_of_core) + " 单核" : "—"}
            tone="muted"
          />
        </div>
        {Array.isArray(all?.processes?.top_by_rss) && all!.processes!.top_by_rss.length ? (
          <details className="text-sm">
            <summary className="cursor-pointer text-slate-500 dark:text-slate-400">内存占用最高的进程</summary>
            <div className="pt-2 space-y-1">
              {all!.processes!.top_by_rss.slice(0, 8).map((p: any) => (
                <Row key={p.pid} label={p.name + " (" + p.pid + ")"} value={fmtBytes(Number(p.rss_kb) * 1024)} mono />
              ))}
            </div>
          </details>
        ) : null}
      </Card>
    </div>
  );
}
