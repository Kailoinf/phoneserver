/**
 * The shared component set. Card / Field / SmallBtn / INPUT are the exact
 * components and the exact class strings the salary-calc project uses (see its
 * STYLE.md); everything else here is built from the same palette and the same
 * spacing/radius rules, so no page invents its own look:
 *
 *   rounded-xl cards & modals · rounded-lg buttons · rounded-md inputs
 *   bg-white / dark:bg-black · text-slate-800/900 · text-slate-500/600
 *   sky-600 accents · emerald-600 for healthy · rose-500 for danger
 *   border-slate-200 / dark:border-slate-700 · px-4 py-6 / space-y-5 / p-4 / gap-3
 *
 * No custom CSS, no inline styles, no new colours.
 */

import { useEffect, useState } from "preact/hooks";
import type { ComponentChildren } from "preact";
import { copyText } from "../api";

export const INPUT =
  "px-2 py-1.5 rounded-md border border-slate-300 dark:border-slate-600 bg-white dark:bg-black text-sm focus:border-sky-500 dark:focus:border-sky-400 focus:outline-none focus:ring-1 focus:ring-sky-500 dark:focus:ring-sky-400 w-full";

const BTN_BASE =
  "inline-flex items-center justify-center gap-1 px-3 py-1.5 rounded-lg text-sm font-medium transition-colors disabled:opacity-50";

const BTN_VARIANT = {
  primary: "bg-sky-600 border border-sky-600 text-white hover:bg-sky-700",
  secondary:
    "border border-slate-200 dark:border-slate-700 bg-slate-100 dark:bg-black text-slate-700 dark:text-slate-300 hover:bg-slate-200 dark:hover:bg-black",
  outline:
    "border border-slate-300 dark:border-slate-600 bg-white dark:bg-black text-slate-700 dark:text-slate-300 hover:bg-slate-100 dark:hover:bg-black",
  danger: "border border-rose-300 text-rose-500 hover:bg-rose-50 dark:hover:bg-black",
  quiet: "text-slate-500 dark:text-slate-400 hover:text-slate-800 dark:hover:text-slate-100",
} as const;

export type BtnVariant = keyof typeof BTN_VARIANT;

/** 白色圆角分区卡片，可带右上角操作按钮 */
export function Card({
  title,
  action,
  children,
}: {
  title?: string;
  action?: ComponentChildren;
  children: ComponentChildren;
}) {
  return (
    <section className="bg-white dark:bg-black rounded-xl border border-slate-200 dark:border-slate-700 p-4 shadow-sm dark:shadow-none space-y-3">
      {title && (
        <div className="flex items-center justify-between gap-3">
          <h2 className="font-semibold text-slate-900 dark:text-slate-100">{title}</h2>
          {action}
        </div>
      )}
      {children}
    </section>
  );
}

/** 通用输入字段：label + 任意 input/select 子元素 */
export function Field({
  label,
  hint,
  className,
  children,
}: {
  label: string;
  hint?: string;
  className?: string;
  children: ComponentChildren;
}) {
  return (
    <label
      className={
        "flex flex-col gap-1 text-sm text-slate-600 dark:text-slate-400" + (className ? " " + className : "")
      }
    >
      <span>
        {label}
        {hint ? <span className="ml-1 text-xs text-slate-500 dark:text-slate-500">{hint}</span> : null}
      </span>
      {children}
    </label>
  );
}

/** 卡片右上角小按钮（设置/导出） */
export function SmallBtn({
  onClick,
  children,
  disabled,
}: {
  onClick: () => void;
  children: ComponentChildren;
  disabled?: boolean;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className="text-xs px-2 py-1 rounded-md border border-slate-300 dark:border-slate-600 text-slate-500 dark:text-slate-400 hover:bg-slate-100 dark:hover:bg-black transition-colors disabled:opacity-50"
    >
      {children}
    </button>
  );
}

export function Btn({
  onClick,
  variant = "secondary",
  disabled,
  type = "button",
  className,
  children,
}: {
  onClick?: () => void;
  variant?: BtnVariant;
  disabled?: boolean;
  type?: "button" | "submit";
  className?: string;
  children: ComponentChildren;
}) {
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      className={`${BTN_BASE} ${BTN_VARIANT[variant]}` + (className ? " " + className : "")}
    >
      {children}
    </button>
  );
}

export function Toggle({
  label,
  checked,
  onChange,
  hint,
}: {
  label: string;
  checked: boolean;
  onChange: (b: boolean) => void;
  hint?: string;
}) {
  return (
    <label className="flex flex-col gap-1 text-sm text-slate-600 dark:text-slate-400">
      <span className="inline-flex items-center gap-1.5">
        <input
          type="checkbox"
          checked={!!checked}
          onChange={(e) => onChange((e.target as HTMLInputElement).checked)}
          className="rounded"
        />
        {label}
      </span>
      {hint ? <span className="text-xs text-slate-500 dark:text-slate-500">{hint}</span> : null}
    </label>
  );
}

/** 只读一行：左侧标签、右侧值 */
export function Row({
  label,
  value,
  tone = "normal",
  mono,
}: {
  label: string;
  value: ComponentChildren;
  tone?: "normal" | "good" | "bad" | "muted";
  mono?: boolean;
}) {
  const cls =
    tone === "good"
      ? "text-emerald-600 dark:text-emerald-400"
      : tone === "bad"
        ? "text-rose-500 dark:text-rose-400"
        : tone === "muted"
          ? "text-slate-500 dark:text-slate-500"
          : "text-slate-800 dark:text-slate-200";
  return (
    <div className="flex items-start justify-between gap-3 text-sm">
      <span className="text-slate-500 dark:text-slate-400">{label}</span>
      <span className={cls + " text-right break-all" + (mono ? " tabular-nums" : "")}>{value}</span>
    </div>
  );
}

/** 小统计块：label 在上、值放大 */
export function Stat({
  label,
  value,
  unit,
  tone = "normal",
}: {
  label: string;
  value: ComponentChildren;
  unit?: string;
  // "muted" exists because the pages use it for secondary numbers; the palette
  // is unchanged (slate-500 is already the secondary text colour).
  tone?: "normal" | "good" | "bad" | "muted";
}) {
  const cls =
    tone === "good"
      ? "text-emerald-600 dark:text-emerald-400"
      : tone === "bad"
        ? "text-rose-500 dark:text-rose-400"
        : tone === "muted"
          ? "text-slate-500 dark:text-slate-500"
          : "text-slate-900 dark:text-slate-100";
  return (
    <div className="rounded-lg border border-slate-200 dark:border-slate-700 p-3 space-y-1">
      <div className="text-xs text-slate-500 dark:text-slate-400">{label}</div>
      <div className={"text-lg font-semibold tabular-nums " + cls}>
        {value}
        {unit ? <span className="ml-1 text-xs font-normal text-slate-500 dark:text-slate-500">{unit}</span> : null}
      </div>
    </div>
  );
}

/** 状态胶囊：ok=good / warn=bad / info=sky / idle=slate */
export function Pill({
  tone = "idle",
  children,
}: {
  tone?: "good" | "bad" | "info" | "idle";
  children: ComponentChildren;
}) {
  const cls =
    tone === "good"
      ? "border-emerald-200 dark:border-emerald-900 text-emerald-600 dark:text-emerald-400"
      : tone === "bad"
        ? "border-rose-300 dark:border-rose-900 text-rose-500 dark:text-rose-400"
        : tone === "info"
          ? "border-sky-500 text-sky-600 dark:text-sky-400"
          : "border-slate-300 dark:border-slate-600 text-slate-500 dark:text-slate-400";
  return (
    <span className={"inline-block rounded-md border px-2 py-0.5 text-xs whitespace-nowrap " + cls}>
      {children}
    </span>
  );
}

/** 提示条：info=sky、warn=rose、ok=emerald */
export function Banner({
  tone = "info",
  children,
}: {
  tone?: "info" | "warn" | "ok";
  children: ComponentChildren;
}) {
  const cls =
    tone === "warn"
      ? "bg-rose-50 dark:bg-black border-rose-300 dark:border-rose-900 text-rose-500 dark:text-rose-400"
      : tone === "ok"
        ? "bg-emerald-50 dark:bg-black border-emerald-200 dark:border-emerald-900 text-emerald-700 dark:text-emerald-400"
        : "bg-slate-50 dark:bg-black border-slate-200 dark:border-slate-700 text-slate-600 dark:text-slate-400";
  return <div className={"rounded-lg border px-3 py-2 text-sm " + cls}>{children}</div>;
}

/** 等宽代码块（日志、JSON、shell 命令） */
export function Code({
  children,
  max = "",
}: {
  children: ComponentChildren;
  max?: string;
}) {
  return (
    <pre
      className={
        "overflow-auto rounded-md border border-slate-200 dark:border-slate-700 bg-slate-50 dark:bg-black p-3 text-xs leading-relaxed text-slate-700 dark:text-slate-300 whitespace-pre-wrap break-all " +
        max
      }
    >
      {children}
    </pre>
  );
}

/** 弹窗：遮罩 + 居中内卡，规则与 STYLE.md 一致 */
export function Modal({
  open,
  onClose,
  title,
  size = "max-w-md",
  children,
}: {
  open: boolean;
  onClose: () => void;
  title: string;
  size?: "max-w-sm" | "max-w-md" | "max-w-xl" | "max-w-4xl";
  children: ComponentChildren;
}) {
  useEffect(() => {
    if (!open) return;
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = prev;
    };
  }, [open]);
  if (!open) return null;
  return (
    <div className="fixed inset-0 z-50 overflow-y-auto bg-black/50 p-4">
      <div className="flex min-h-full items-center justify-center">
        <div
          className={
            "bg-white dark:bg-black rounded-xl border border-slate-200 dark:border-slate-700 p-4 shadow-xl w-full space-y-3 my-8 " +
            size
          }
        >
          <div className="flex items-center justify-between gap-3">
            <h2 className="font-semibold text-slate-900 dark:text-slate-100">{title}</h2>
            <button
              type="button"
              onClick={onClose}
              className="text-xs px-2 py-1 rounded-md border border-slate-300 dark:border-slate-600 text-slate-500 dark:text-slate-400 hover:bg-slate-100 dark:hover:bg-black"
            >
              关闭
            </button>
          </div>
          {children}
        </div>
      </div>
    </div>
  );
}

/** 分段选择器（页面/视图切换用） */
export function Seg<T extends string>({
  value,
  onChange,
  options,
}: {
  value: T;
  onChange: (v: T) => void;
  options: { value: T; label: string }[];
}) {
  return (
    <div className="flex flex-wrap gap-1">
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          onClick={() => onChange(o.value)}
          className={
            "px-2 py-1 rounded-md text-xs border transition-colors " +
            (o.value === value
              ? "bg-sky-600 border-sky-600 text-white"
              : "border-slate-300 dark:border-slate-600 text-slate-600 dark:text-slate-400 hover:bg-slate-100 dark:hover:bg-black")
          }
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

/** 百分比条：SVG 矩形（宽度用属性表达，不用 inline style） */
export function Bar({ pct, tone = "sky" }: { pct: number; tone?: "sky" | "emerald" | "rose" }) {
  const v = Math.min(100, Math.max(0, Number(pct) || 0));
  const cls =
    tone === "emerald"
      ? "text-emerald-600 dark:text-emerald-400"
      : tone === "rose"
        ? "text-rose-500 dark:text-rose-400"
        : "text-sky-600 dark:text-sky-400";
  return (
    <svg
      viewBox="0 0 100 6"
      preserveAspectRatio="none"
      className={"h-1.5 w-full " + cls}
      role="img"
      aria-label={`${v.toFixed(0)}%`}
    >
      <rect x="0" y="0" width="100" height="6" className="text-slate-200 dark:text-slate-700" fill="currentColor" />
      <rect x="0" y="0" width={v} height="6" fill="currentColor" />
    </svg>
  );
}

/** 折线图：只读 SVG，点集来自 API 返回的 [[ts, v...], ...] */
export function Spark({
  series,
  color = "sky",
  height = 120,
}: {
  series: { points: number[]; label: string }[];
  color?: "sky" | "emerald" | "rose";
  height?: number;
}) {
  const w = 300;
  const h = height;
  const all = series.flatMap((s) => s.points);
  if (!series.length || all.length === 0) {
    return <div className="text-sm text-slate-500 dark:text-slate-400">没有采样点</div>;
  }
  const min = Math.min(...all);
  const max = Math.max(...all);
  const span = max - min || 1;
  const step = series[0].points.length > 1 ? w / (series[0].points.length - 1) : w;
  const cls =
    color === "emerald"
      ? "text-emerald-600 dark:text-emerald-400"
      : color === "rose"
        ? "text-rose-500 dark:text-rose-400"
        : "text-sky-600 dark:text-sky-400";
  return (
    <svg viewBox={`0 0 ${w} ${h}`} className={"w-full " + cls} role="img">
      <rect x="0" y="0" width={w} height={h} className="text-slate-50 dark:text-black" fill="currentColor" />
      {series.map((s, i) => (
        <polyline
          key={i}
          fill="none"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinejoin="round"
          points={s.points
            .map((v, j) => `${(j * step).toFixed(1)},${(h - ((v - min) / span) * (h - 8) - 4).toFixed(1)}`)
            .join(" ")}
        />
      ))}
    </svg>
  );
}

/** 复制按钮，带一次“已复制”反馈 */
export function CopyBtn({ value, label = "复制" }: { value: string; label?: string }) {
  const [done, setDone] = useState(false);
  return (
    <SmallBtn
      onClick={async () => {
        const ok = await copyText(value);
        setDone(ok);
        window.setTimeout(() => setDone(false), 1500);
      }}
    >
      {done ? "已复制" : label}
    </SmallBtn>
  );
}

/** 加载/错误/空态的统一样子 */
export function State({
  loading,
  error,
  empty,
  emptyText = "没有数据",
}: {
  loading?: boolean;
  error?: string;
  empty?: boolean;
  emptyText?: string;
}) {
  if (loading) return <div className="text-sm text-slate-500 dark:text-slate-400">加载中…</div>;
  if (error) return <Banner tone="warn">请求失败：{error}</Banner>;
  if (empty) return <div className="text-sm text-slate-500 dark:text-slate-400">{emptyText}</div>;
  return null;
}

/** 表格骨架：统一边框、字号与对齐 */
export function TableHead({ cols }: { cols: string[] }) {
  return (
    <thead>
      <tr className="border-b border-slate-200 dark:border-slate-700">
        {cols.map((c) => (
          <th
            key={c}
            className="py-2 pr-3 text-left text-xs font-medium text-slate-500 dark:text-slate-400 whitespace-nowrap"
          >
            {c}
          </th>
        ))}
      </tr>
    </thead>
  );
}

export function Td({
  children,
  mono,
  right,
  className,
}: {
  children: ComponentChildren;
  mono?: boolean;
  right?: boolean;
  className?: string;
}) {
  return (
    <td
      className={
        "py-2 pr-3 align-top text-sm text-slate-800 dark:text-slate-200 " +
        (mono ? "tabular-nums " : "") +
        (right ? "text-right " : "") +
        (className || "")
      }
    >
      {children}
    </td>
  );
}

export function Err({ text }: { text: string }) {
  if (!text) return null;
  return <Banner tone="warn">{text}</Banner>;
}

export function ResultBox({ label, text }: { label: string; text: string }) {
  if (!text) return null;
  return (
    <div className="space-y-1">
      <div className="text-xs text-slate-500 dark:text-slate-400">{label}</div>
      <Code max="max-h-72">{text}</Code>
    </div>
  );
}