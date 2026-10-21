/**
 * 历史数据 —— 查询样本、看趋势、看数据质量。
 *
 * 数据质量那块不是装饰：覆盖率、缺口、被拒的值、被前向填充的列、小时聚合状态，
 * 都是后端从已存的行里算出来的，画面上直接照搬，不加解释性美化。
 */
import { useEffect, useState } from "preact/hooks";
import { apiGet, apiPost, useApiState } from "../api";
import { fmtBytes, fmtDur } from "../utils";
import { Banner, Btn, Card, Field, INPUT, Pill, Row, Seg, SmallBtn, Stat, TableHead, Td } from "../components/ui";

interface Series {
  columns?: string[];
  points?: any[][];
  count?: number;
  carried_columns?: string[];
  note?: string;
  truncated?: boolean;
}

const COLORS = ["#0284c7", "#059669", "#e11d48", "#7c3aed", "#d97706", "#0891b2"];

function Line({ series, i }: { series: { name: string; vals: [number, number][] }; i: number }) {
  const w = 640;
  const h = 140;
  if (!series.vals.length) return null;
  const xs = series.vals.map((v) => v[0]);
  const ys = series.vals.map((v) => v[1]);
  const x0 = Math.min(...xs);
  const x1 = Math.max(...xs);
  const y0 = Math.min(...ys);
  const y1 = Math.max(...ys);
  const sx = (x: number) => (x1 === x0 ? 0 : ((x - x0) / (x1 - x0)) * (w - 8) + 4);
  const sy = (y: number) => (y1 === y0 ? h / 2 : h - 4 - ((y - y0) / (y1 - y0)) * (h - 8));
  const pts = series.vals.map((v) => sx(v[0]).toFixed(1) + "," + sy(v[1]).toFixed(1)).join(" ");
  return (
    <svg viewBox={"0 0 " + w + " " + h} className="w-full h-36" preserveAspectRatio="none">
      <polyline points={pts} fill="none" stroke={COLORS[i % COLORS.length]} strokeWidth="1.5" />
      <text x="6" y="14" fontSize="11" fill={COLORS[i % COLORS.length]}>
        {series.name} · min {y0} / max {y1} / last {ys[ys.length - 1]}
      </text>
    </svg>
  );
}

export default function History() {
  const s = useApiState();
  const [metrics, setMetrics] = useState<string[]>([]);
  const [picked, setPicked] = useState<string[]>([]);
  const [since, setSince] = useState("-6h");
  const [bucket, setBucket] = useState("300");
  const [series, setSeries] = useState<Series | null>(null);
  const [quality, setQuality] = useState<any>(null);
  const [gaps, setGaps] = useState<any>(null);
  const [prune, setPrune] = useState<any>(null);
  const [err, setErr] = useState("");

  const loadMeta = async () => {
    const m = await apiGet<any>("/history/metrics", 15000);
    if (m.ok && m.data) {
      setMetrics((m.data as any).metrics || []);
      if (!picked.length) setPicked(((m.data as any).default || []).slice(0, 3));
    }
  };
  const loadSeries = async (ms = picked, sc = since, bk = bucket) => {
    if (!ms.length) return;
    const r = await apiGet<Series>(
      "/history?metrics=" + encodeURIComponent(ms.join(",")) + "&since=" + sc + "&limit=2000&bucket=" + (bk === "0" ? "" : bk),
      30000,
    );
    if (r.ok && r.data) {
      setSeries(r.data as Series);
      setErr("");
    } else setErr(r.raw || r.error || "查询失败");
  };
  const loadQuality = async () => {
    const r = await apiGet<any>("/history/quality?since=" + since, 30000);
    if (r.ok) setQuality((r.data as any)?.report || r.data);
    const g = await apiGet<any>("/history/gaps?since=" + since, 20000);
    if (g.ok) setGaps(g.data);
  };

  useEffect(() => {
    void loadMeta();
    void loadQuality();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  useEffect(() => {
    if (picked.length) void loadSeries();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [picked.join(","), since, bucket]);

  const cols = series?.columns || [];
  const chartSeries = cols.slice(1).map((c, i) => {
    const idx = i + 1;
    const vals: [number, number][] = (series?.points || [])
      .map((p) => [Number(p[0]), Number(p[idx])] as [number, number])
      .filter((v) => Number.isFinite(v[1]));
    return { name: c, vals };
  });

  const cov = quality?.coverage;
  const runPrune = async () => {
    const r = await apiPost("/history/prune", {});
    setPrune(r.ok ? r.data : { error: r.raw || r.error });
    void loadQuality();
  };

  return (
    <div className="space-y-5">
      <Card
        title="样本查询"
        action={
          <div className="flex items-center gap-2">
            <Seg
              value={since}
              onChange={setSince}
              options={[
                { value: "-1h", label: "1 小时" },
                { value: "-6h", label: "6 小时" },
                { value: "-24h", label: "24 小时" },
                { value: "-7d", label: "7 天" },
              ]}
            />
            <SmallBtn onClick={() => void loadSeries()}>查询</SmallBtn>
          </div>
        }
      >
        {err ? <Banner tone="warn">{err}</Banner> : null}
        <div className="grid grid-cols-2 sm:grid-cols-3 gap-3">
          <Stat label="返回点数" value={series?.count ?? "—"} />
          <Stat label="指标列" value={Math.max(0, cols.length - 1)} tone="muted" />
          <Stat label="被前向填充的列" value={series?.carried_columns?.length ?? 0} tone="muted" />
        </div>
        <div className="flex flex-wrap items-end gap-3">
          <Field label="时间窗" className="w-36">
            <input className={INPUT} value={since} onInput={(e) => setSince((e.target as HTMLInputElement).value)} placeholder="-6h 或 unix 秒" />
          </Field>
          <Field label="聚合粒度（秒）" hint="600=10 分钟一格；留空=原始行" className="w-44">
            <input className={INPUT} value={bucket} onInput={(e) => setBucket((e.target as HTMLInputElement).value)} />
          </Field>
        </div>
        <div className="flex flex-wrap gap-2">
          {metrics.slice(0, 24).map((m) => (
            <button
              key={m}
              type="button"
              onClick={() => setPicked(picked.includes(m) ? picked.filter((x) => x !== m) : [...picked, m])}
              className={
                "text-xs px-2 py-1 rounded-md border " +
                (picked.includes(m)
                  ? "border-sky-500 text-sky-600 dark:text-sky-400"
                  : "border-slate-300 dark:border-slate-600 text-slate-500 dark:text-slate-400")
              }
            >
              {m}
            </button>
          ))}
          <SmallBtn onClick={() => setPicked([])}>清空选择</SmallBtn>
        </div>
        {chartSeries.length ? (
          <div className="space-y-1">
            {chartSeries.map((c, i) => (
              <Line key={c.name} series={c} i={i} />
            ))}
          </div>
        ) : (
          <Row label="没有数据" value={series?.note || "选几个指标再看看"} tone="muted" />
        )}
        {series?.note ? <p className="text-xs text-slate-500 dark:text-slate-500">{series.note}</p> : null}
      </Card>

      <Card
        title="数据质量"
        action={
          <div className="flex items-center gap-2">
            <SmallBtn onClick={() => void loadQuality()}>重新计算</SmallBtn>
            <Btn variant="outline" onClick={() => void runPrune()}>立即清理与聚合</Btn>
          </div>
        }
      >
        {quality ? (
          <>
            <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
              <Stat label="覆盖率" value={cov?.coverage_pct ?? "—"} unit="%" tone={(cov?.coverage_pct ?? 100) < 90 ? "bad" : "good"} />
              <Stat label="已存行" value={cov?.rows ?? "—"} />
              <Stat label="按节奏应有" value={cov?.expected_rows ?? "—"} tone="muted" />
              <Stat label="结构 / 期望" value={String(quality.schema_version ?? "—") + " / " + String(quality.schema_expected ?? "—")} tone="muted" />
            </div>
            <div className="grid sm:grid-cols-2 gap-x-6 gap-y-2">
              <Row label="指标列数" value={quality.metric_columns ?? "—"} mono />
              <Row label="缺口数" value={quality.gaps_found ?? "—"} mono tone={(quality.gaps_found || 0) > 0 ? "bad" : "good"} />
              <Row label="被拒的行 / 值" value={(quality.rejected_rows ?? 0) + " / " + Object.keys(quality.rejected_by_column || {}).length} mono />
              <Row label="重复时间戳行" value={quality.duplicate_ts_rows ?? "—"} mono tone="muted" />
              <Row label="迁移时去重" value={quality.deduped_rows_at_migration ?? "—"} mono tone="muted" />
              <Row label="采样间隔" value={(quality.interval_seconds ?? "—") + " 秒"} mono tone="muted" />
            </div>
            {quality.rollup ? (
              <div className="grid sm:grid-cols-2 gap-x-6 gap-y-2">
                {Object.keys(quality.rollup).map((k) => (
                  <Row key={k} label={"聚合 · " + k} value={String(quality.rollup[k]).slice(0, 60)} mono tone="muted" />
                ))}
              </div>
            ) : null}
            {Array.isArray(quality.null_ratio_top) && quality.null_ratio_top.length ? (
              <div className="overflow-x-auto">
                <div className="text-sm text-slate-500 dark:text-slate-400 pb-1">空值最多的列</div>
                <table className="w-full text-sm">
                  <TableHead cols={["列", "总行", "空行", "空值比例", "被拒值"]} />
                  <tbody>
                    {quality.null_ratio_top.map((r: any) => (
                      <tr key={r.metric} className="border-b border-slate-100 dark:border-slate-800">
                        <Td mono>{r.metric}</Td>
                        <Td mono>{r.rows}</Td>
                        <Td mono>{r.null_rows}</Td>
                        <Td mono>{Math.round(r.null_ratio * 100) + "%"}</Td>
                        <Td mono>{r.rejected_values}</Td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            ) : null}
            {Array.isArray(quality.notes) && quality.notes.length ? (
              <details className="text-xs">
                <summary className="cursor-pointer text-slate-500 dark:text-slate-400">说明（{quality.notes.length} 条）</summary>
                <ul className="pt-2 list-disc pl-4 space-y-1">
                  {quality.notes.map((n: string, i: number) => (
                    <li key={i}>{n}</li>
                  ))}
                </ul>
              </details>
            ) : null}
          </>
        ) : (
          <Row label="读取中…" value="" tone="muted" />
        )}
        {gaps?.result?.gaps?.length ? (
          <div className="overflow-x-auto">
            <div className="text-sm text-slate-500 dark:text-slate-400 pb-1">最长的采样缺口</div>
            <table className="w-full text-sm">
              <TableHead cols={["从", "到", "缺口", "缺失行"]} />
              <tbody>
                {gaps.result.gaps.slice(0, 8).map((g: any, i: number) => (
                  <tr key={i} className="border-b border-slate-100 dark:border-slate-800">
                    <Td mono>{new Date(g.from_ts * 1000).toLocaleString()}</Td>
                    <Td mono>{new Date(g.to_ts * 1000).toLocaleString()}</Td>
                    <Td mono>{fmtDur(g.gap_ms / 1000)}</Td>
                    <Td mono>{g.rows_missing}</Td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
        {prune ? (
          <details open>
            <summary className="cursor-pointer text-sm text-slate-500 dark:text-slate-400">上次清理结果</summary>
            <pre className="pt-2 overflow-auto text-xs whitespace-pre-wrap break-all max-h-48">{JSON.stringify(prune, null, 2)}</pre>
          </details>
        ) : null}
      </Card>
    </div>
  );
}
