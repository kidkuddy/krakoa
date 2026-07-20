import { useMemo, useState } from "react";
import type { Event, RunDetail, Step } from "../api";
import { actor, clock, cx, dur, narrate, usd, waitInfo } from "../lib";
import { Card, Chip, Dot, Label, Mono, statusCls } from "./bits";

/* One node per state visit, folded from the event spine — wait states are
   first-class (F2), never invisible. */
interface Seg { state: string; type: "agent" | "wait" | "gate" | "park"; start: number; end: number; live: boolean; key: string; }

function segments(events: Event[]): Seg[] {
  const marks: Record<string, Seg["type"] | "start"> = {
    "run-admitted": "start", "step-started": "agent", "wait-armed": "wait",
    "gate-opened": "gate", parked: "park",
  };
  const segs: Seg[] = [];
  let cur: Seg | null = null;
  for (const e of events) {
    const type = marks[e.Kind];
    if (!type || !e.State) continue;
    const t = +new Date(e.At);
    if (cur && cur.state === e.State) {
      if (type === "park") cur.type = "park";
      continue;
    }
    if (cur) cur.end = t;
    cur = { state: e.State, type: type === "start" ? "agent" : type, start: t, end: 0, live: false, key: "" };
    segs.push(cur);
  }
  const done = events.some(e => e.Kind === "run-finished");
  if (cur) {
    cur.end = done ? +new Date(events[events.length - 1].At) : Date.now();
    cur.live = !done;
  }
  segs.forEach((s, i) => (s.key = `${i}:${s.state}`));
  return segs;
}

const stepFor = (steps: Step[], seg: Seg) =>
  steps.filter(s => s.State === seg.state && +new Date(s.StartedAt) >= seg.start - 3000 && +new Date(s.StartedAt) <= seg.end + 1000).pop();

const typeDot: Record<Seg["type"], string> = {
  agent: "bg-blue-400", wait: "bg-amber-400", gate: "bg-purple-400", park: "bg-red-400",
};

export function RunFlow({ detail }: { detail: RunDetail }) {
  const { run } = detail;
  const steps = detail.steps ?? [];
  const events = detail.events ?? [];
  const timers = detail.timers ?? [];
  const segs = useMemo(() => segments(events), [events]);
  const [sel, setSel] = useState<string | null>(null);
  const selected = segs.find(s => s.key === sel) ?? segs[segs.length - 1];

  return (
    <div className="grid grid-cols-1 gap-4 md:grid-cols-[260px_minmax(0,1fr)]">
      <div className="overflow-hidden rounded-xl border border-zinc-800">
        {segs.map(s => {
          const st = stepFor(steps, s);
          const agents = events.filter(e => e.Kind === "step-started" && e.Data?.step === st?.ID);
          const agentName = agents.length ? String(agents[0].Data?.agent ?? "") : "";
          return (
            <div
              key={s.key}
              onClick={() => setSel(s.key)}
              className={cx(
                "cursor-pointer border-b border-zinc-800 px-3 py-2 last:border-b-0 hover:bg-zinc-900",
                selected?.key === s.key && "border-l-2 border-l-blue-400 bg-zinc-900",
              )}
            >
              <div className="flex items-baseline justify-between gap-2">
                <span className="truncate">
                  <Dot cls={typeDot[s.type]} live={s.live} />
                  {s.state}
                  {st?.Outcome && <span className="text-zinc-400"> → {st.Outcome}</span>}
                  {s.type === "agent" && s.live && !st?.Outcome && <span className="text-zinc-500"> …</span>}
                </span>
                <Mono className="text-zinc-500">{dur(s.end - s.start)}</Mono>
              </div>
              <div className="text-[11px] text-zinc-500">
                {s.type === "agent" ? agentName : s.type}
                {st?.CostUSD ? ` · ${usd(st.CostUSD)}` : ""}
              </div>
            </div>
          );
        })}
        {!segs.length && <div className="p-4 text-zinc-500">no activity yet</div>}
      </div>

      <div>{selected && <NodeDetail seg={selected} detail={detail} />}</div>

      <details className="md:col-span-2">
        <summary className="cursor-pointer text-[12px] text-zinc-500">raw events ({events.length})</summary>
        <div className="mt-2 overflow-x-auto rounded-lg border border-zinc-800">
          {events.map(e => (
            <div key={e.ID} className="flex gap-3 border-b border-zinc-800/60 px-3 py-1 font-mono text-[11px] last:border-b-0">
              <span className="text-zinc-500">{clock(e.At)}</span>
              <ActorTag e={e} />
              <span className="w-40 shrink-0">{e.Kind}</span>
              <span className="w-28 shrink-0 text-zinc-400">{e.State}</span>
              <span className="break-all text-zinc-500">
                {Object.entries(e.Data ?? {}).map(([k, v]) => `${k}=${JSON.stringify(v)}`).join(" ")}
              </span>
            </div>
          ))}
        </div>
      </details>
    </div>
  );
}

const ActorTag = ({ e }: { e: Event }) => {
  const a = actor(e);
  const cls = { engine: "text-zinc-500", agent: "text-blue-400", human: "text-amber-300", external: "text-purple-300" }[a];
  return <span className={cx("w-14 shrink-0", cls)}>{a}</span>;
};

function NodeDetail({ seg, detail }: { seg: Seg; detail: RunDetail }) {
  const { run } = detail;
  const steps = detail.steps ?? [];
  const events = detail.events ?? [];
  const st = stepFor(steps, seg);
  const narration = events
    .filter(e => e.State === seg.state && +new Date(e.At) >= seg.start - 1000 && +new Date(e.At) <= seg.end + 1000)
    .map(e => ({ e, text: narrate(e) }))
    .filter((x): x is { e: Event; text: string } => !!x.text);

  return (
    <Card>
      <div className="mb-1 flex items-baseline gap-2">
        <span className="text-[15px] font-semibold">{seg.state}</span>
        {st?.Outcome && <Chip cls={st.Error ? statusCls.failed : statusCls.done}>{st.Outcome}</Chip>}
      </div>

      {seg.type === "wait" && seg.live && <LiveWait detail={detail} />}

      {st && (
        <div className="text-[12px] text-zinc-400">
          attempt {st.Attempt}{st.CostUSD ? ` · ${usd(st.CostUSD)}` : ""} · {clock(st.StartedAt)}{st.EndedAt ? ` → ${clock(st.EndedAt)}` : " (running)"}
          {st.SessionID && (
            <div>session <Mono className="select-all">{st.SessionID}</Mono> · resume: <Mono className="select-all">claude --resume {st.SessionID}</Mono></div>
          )}
        </div>
      )}

      {narration.length > 0 && (
        <>
          <Label>what happened</Label>
          {narration.map(({ e, text }) => (
            <div key={e.ID} className="text-[12px] text-zinc-300">
              <span className="text-zinc-500">{clock(e.At)}</span> {text}
            </div>
          ))}
        </>
      )}

      {st?.Error && (<><Label>error</Label><div className="whitespace-pre-wrap text-red-300">{st.Error}</div></>)}
      {st && Object.keys(st.Result ?? {}).filter(k => k !== "outcome").length > 0 && (
        <>
          <Label>result</Label>
          {Object.entries(st.Result).filter(([k]) => k !== "outcome").map(([k, v]) => <Field key={k} k={k} v={v} />)}
        </>
      )}
      {st && Object.keys(st.Inputs ?? {}).length > 0 && (
        <details className="mt-3">
          <summary className="cursor-pointer text-[11px] uppercase tracking-widest text-zinc-500">inputs</summary>
          {Object.entries(st.Inputs).map(([k, v]) => <Field key={k} k={k} v={v} />)}
        </details>
      )}
    </Card>
  );
}

/* F2 — a waiting run renders as ALIVE. */
function LiveWait({ detail }: { detail: RunDetail }) {
  const info = waitInfo(detail.run, detail.events ?? [], detail.timers ?? []);
  const lastResult = info.lastProbe?.Data?.status ?? info.lastProbe?.Data?.outcome;
  return (
    <div className="my-2 rounded-lg border border-amber-500/30 bg-amber-950/10 p-3 text-[12px]">
      <div><Dot cls="bg-amber-400" live /> waiting since {clock(info.since)} ({dur(Date.now() - +new Date(info.since))})</div>
      {info.checks > 0 && (
        <div className="mt-1 text-zinc-400">
          {info.checks} check{info.checks > 1 ? "s" : ""} so far
          {lastResult ? ` · last: ${lastResult} at ${clock(info.lastProbe!.At)}` : ""}
          {info.nextCheck ? ` · next in ~${info.nextCheck}` : ""}
        </div>
      )}
      {info.timeoutAt && <div className="mt-1 text-zinc-500">timeout arm fires at {clock(info.timeoutAt)}</div>}
    </div>
  );
}

function Field({ k, v }: { k: string; v: unknown }) {
  const s = typeof v === "string" ? v : JSON.stringify(v, null, 2);
  const big = s.length > 100 || s.includes("\n");
  return big ? (
    <div className="mb-2">
      <div className="text-[11px] uppercase tracking-widest text-zinc-500">{k}</div>
      <pre className="mt-1 max-h-80 overflow-auto whitespace-pre-wrap rounded-lg border border-zinc-800 bg-zinc-950 p-3 font-mono text-[12px]">{s}</pre>
    </div>
  ) : (
    <div className="text-[12px]"><span className="text-zinc-500">{k}</span> {s}</div>
  );
}
