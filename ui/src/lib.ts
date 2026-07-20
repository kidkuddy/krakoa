import type { Event, Gate, Run, Step, Timer } from "./api";

export const dur = (ms: number): string => {
  if (!isFinite(ms) || ms < 0) return "";
  const s = Math.round(ms / 1000);
  if (s < 90) return `${s}s`;
  const m = Math.round(s / 60);
  if (m < 90) return `${m}m`;
  const h = Math.floor(m / 60);
  return `${h}h ${m - h * 60}m`;
};
export const ago = (t: string) => dur(Date.now() - +new Date(t));
export const clock = (t: string) =>
  t ? new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) : "";
export const usd = (x: number) => (x ? `$${x.toFixed(x < 0.1 ? 4 : 2)}` : "");

/* F5 — who has the ball. Every thread is exactly one of these. */
export type Ball = "your-turn" | "machine" | "world" | "done" | "dead";

export function ball(runs: Run[], gates: Gate[]): Ball {
  const ids = new Set(runs.map(r => r.ID));
  if (gates.some(g => ids.has(g.RunID))) return "your-turn";
  if (runs.some(r => r.Status === "running")) return "machine";
  if (runs.some(r => r.Status === "waiting" || r.Status === "queued")) return "world";
  if (runs.some(r => r.Status === "failed" || r.Status === "canceled") &&
      !runs.some(r => r.Status === "done")) return "dead";
  return "done";
}

export const ballCopy: Record<Ball, { label: string; cls: string }> = {
  "your-turn": { label: "YOUR TURN", cls: "bg-amber-400 text-black" },
  machine: { label: "machine working", cls: "bg-blue-500/20 text-blue-300" },
  world: { label: "world working — nothing needed", cls: "bg-zinc-700/60 text-zinc-300" },
  done: { label: "done", cls: "bg-emerald-500/15 text-emerald-300" },
  dead: { label: "ended without finishing", cls: "bg-red-500/15 text-red-300" },
};

/* F6 — actor attribution per event kind. */
export function actor(e: Event): "engine" | "agent" | "human" | "external" {
  const k = e.Kind;
  if (k.startsWith("step-") || k === "schema-violation") return "agent";
  if (k === "gate-answered") return "human";
  if (k === "emit" || k === "watcher-observed" || k === "signal-buffered" || k === "signal-consumed") return "external";
  return "engine";
}

/* F6 — self-narration for detect-and-fix and notable moments. */
export function narrate(e: Event): string | null {
  const d = e.Data ?? {};
  switch (e.Kind) {
    case "step-retry": return `attempt failed → retrying (attempt ${d.attempt})`;
    case "schema-violation": return `handoff invalid → resumed the same session to fix it`;
    case "probe-failed": return `probe errored → cadence retries (timeout arm is the backstop)`;
    case "watcher-failed": return `watcher sweep derailed → retried`;
    case "parked": return `stuck → parked for you: ${d.reason ?? ""}`;
    case "recovered": return `daemon restarted → step re-attempted (check-first)`;
    case "gate-late-response": return `late answer ignored (first response already won)`;
    case "thread-stamped": return `joined thread ${d.thread}`;
    default: return null;
  }
}

/* F8 — deep links harvested from anything the runs know. */
export function harvestLinks(steps: Step[], runs: Run[]): { label: string; href: string }[] {
  const out = new Map<string, string>();
  const consider = (v: unknown) => {
    if (typeof v !== "string") return;
    const m = v.match(/https?:\/\/[^\s"')\]]+/g);
    m?.forEach(u => {
      let label = "link";
      if (u.includes("/merge_requests/")) label = `!${u.split("/merge_requests/")[1]?.split(/[^0-9]/)[0] ?? "MR"}`;
      else if (u.includes("pipeline")) label = "pipeline";
      else if (u.match(/issue|ticket/i)) label = "ticket";
      else label = new URL(u).hostname.replace("www.", "");
      if (!out.has(u)) out.set(u, label);
    });
  };
  steps.forEach(s => { Object.values(s.Result ?? {}).forEach(consider); Object.values(s.Inputs ?? {}).forEach(consider); });
  runs.forEach(r => Object.values(r.Inputs ?? {}).forEach(consider));
  return [...out.entries()].map(([href, label]) => ({ href, label }));
}

/* Wait liveness (F2/F9): what a waiting run is doing right now. */
export interface WaitInfo {
  since: string;
  checks: number;
  lastProbe?: Event;
  nextCheck?: string; // ms countdown label
  timeoutAt?: string;
}
export function waitInfo(run: Run, events: Event[], timers: Timer[]): WaitInfo {
  const inState = events.filter(e => e.State === run.State);
  const armed = [...inState].reverse().find(e => e.Kind === "wait-armed");
  const probes = inState.filter(e =>
    (e.Kind === "probe-pending" || e.Kind === "probe-outcome" || e.Kind === "probe-failed") &&
    (!armed || +new Date(e.At) >= +new Date(armed.At)));
  const info: WaitInfo = { since: armed?.At ?? run.UpdatedAt, checks: probes.length };
  info.lastProbe = probes[probes.length - 1];
  for (const t of timers) {
    if (t.State !== run.State) continue;
    if (t.Kind === "probe") info.nextCheck = dur(+new Date(t.FireAt) - Date.now());
    if (t.Kind === "timeout") info.timeoutAt = t.FireAt;
  }
  return info;
}

/* F9 — auto-retro over a finished thread's events. */
export interface Retro {
  wall: number; agentMs: number; waitMs: number; cost: number;
  interventions: string[];
}
export function retro(runs: Run[], stepsByRun: Record<string, Step[]>, eventsByRun: Record<string, Event[]>): Retro {
  const all = runs.flatMap(r => eventsByRun[r.ID] ?? []);
  if (!all.length) return { wall: 0, agentMs: 0, waitMs: 0, cost: 0, interventions: [] };
  const times = all.map(e => +new Date(e.At));
  const wall = Math.max(...times) - Math.min(...times);
  let agentMs = 0, cost = 0;
  const interventions: string[] = [];
  runs.forEach(r => (stepsByRun[r.ID] ?? []).forEach(s => {
    if (s.EndedAt && s.StartedAt) agentMs += +new Date(s.EndedAt) - +new Date(s.StartedAt);
    cost += s.CostUSD || 0;
  }));
  all.forEach(e => {
    const d = e.Data ?? {};
    if (e.Kind === "gate-answered" && d.responder !== "dry-run")
      interventions.push(`${clock(e.At)} human answered ${e.State} (${d.response})`);
    if (e.Kind === "step-retry") interventions.push(`${clock(e.At)} retry in ${e.State}`);
    if (e.Kind === "schema-violation") interventions.push(`${clock(e.At)} handoff fix in ${e.State}`);
    if (e.Kind === "parked") interventions.push(`${clock(e.At)} parked in ${e.State}`);
  });
  return { wall, agentMs, waitMs: Math.max(wall - agentMs, 0), cost, interventions };
}

export const cx = (...xs: (string | false | undefined)[]) => xs.filter(Boolean).join(" ");
