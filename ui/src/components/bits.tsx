import { cx } from "../lib";
import type { ReactNode } from "react";

export const statusCls: Record<string, string> = {
  running: "bg-blue-500/15 text-blue-300",
  waiting: "bg-amber-500/15 text-amber-300",
  gated: "bg-amber-400/25 text-amber-200",
  queued: "bg-zinc-700/50 text-zinc-400",
  blocked: "bg-orange-500/20 text-orange-300",
  done: "bg-emerald-500/15 text-emerald-300",
  failed: "bg-red-500/15 text-red-300",
  "needs-attention": "bg-red-500/25 text-red-200",
  canceled: "bg-zinc-700/50 text-zinc-400",
};

export const Chip = ({ children, cls }: { children: ReactNode; cls?: string }) => (
  <span className={cx("inline-flex items-center rounded-full px-2 py-px text-[11px] font-semibold", cls ?? "bg-zinc-800 text-zinc-300")}>
    {children}
  </span>
);

export const Card = ({ children, className, onClick }: { children: ReactNode; className?: string; onClick?: () => void }) => (
  <div
    onClick={onClick}
    className={cx(
      "rounded-xl border border-zinc-800 bg-zinc-900/60 p-4",
      onClick && "cursor-pointer transition-colors hover:border-zinc-600",
      className,
    )}
  >
    {children}
  </div>
);

export const Btn = ({ children, onClick, tone = "default", disabled }: {
  children: ReactNode; onClick: () => void; tone?: "default" | "primary" | "danger"; disabled?: boolean;
}) => (
  <button
    onClick={e => { e.stopPropagation(); onClick(); }}
    disabled={disabled}
    className={cx(
      "rounded-md px-3 py-1 text-[12px] font-semibold transition-colors disabled:opacity-40",
      tone === "primary" && "bg-blue-600 text-white hover:bg-blue-500",
      tone === "danger" && "bg-red-900/60 text-red-200 hover:bg-red-800/60",
      tone === "default" && "bg-zinc-800 text-zinc-200 hover:bg-zinc-700",
    )}
  >
    {children}
  </button>
);

export const Mono = ({ children, className }: { children: ReactNode; className?: string }) => (
  <span className={cx("font-mono text-[12px]", className)}>{children}</span>
);

export const Dot = ({ cls, live }: { cls: string; live?: boolean }) => (
  <span className={cx("mr-1.5 inline-block h-2 w-2 rounded-full align-[1px]", cls, live && "animate-pulse")} />
);

export const Label = ({ children }: { children: ReactNode }) => (
  <div className="mb-1 mt-4 text-[10px] font-semibold uppercase tracking-widest text-zinc-500">{children}</div>
);
