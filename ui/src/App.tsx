import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { fetchDetail, fetchGates, fetchRuns, type Gate, type Run, type RunDetail } from "./api";
import { ago, ball, ballCopy, clock, cx, dur, harvestLinks, retro, usd, type Ball } from "./lib";
import { Card, Chip, Dot, Label, Mono, statusCls } from "./components/bits";
import { GateCard } from "./components/GateCard";
import { RunFlow } from "./components/RunFlow";

/* Main view = one card per THREAD (F4): the piece of work, not the run.
   Ball trichotomy loudest (F5), your-turn sorts first, deep links as chips
   (F8), auto-retro on finished threads (F9). View state lives in the URL
   hash so refresh restores it. */

interface Thread { key: string; runs: Run[]; gates: Gate[]; }

function group(runs: Run[], gates: Gate[]): Thread[] {
  const by = new Map<string, Run[]>();
  runs.forEach(r => {
    const k = r.Thread || r.ID; // ungrouped runs are single-run threads
    by.set(k, [...(by.get(k) ?? []), r]);
  });
  const threads = [...by.entries()].map(([key, rs]) => ({
    key,
    runs: rs.sort((a, b) => a.CreatedAt.localeCompare(b.CreatedAt)),
    gates: gates.filter(g => rs.some(r => r.ID === g.RunID)),
  }));
  const rank = (t: Thread) => {
    const b = ball(t.runs, t.gates);
    return b === "your-turn" ? 0 : b === "blocked" ? 1 : b === "machine" ? 2 : b === "world" ? 3 : b === "dead" ? 4 : 5;
  };
  return threads.sort((a, b) => rank(a) - rank(b) ||
    Math.max(...b.runs.map(r => +new Date(r.UpdatedAt))) - Math.max(...a.runs.map(r => +new Date(r.UpdatedAt))));
}

const readHash = () => {
  const m = location.hash.match(/^#t=([^&]+)(?:&r=([^&]+))?$/);
  return { thread: m ? decodeURIComponent(m[1]) : null, run: m?.[2] ? decodeURIComponent(m[2]) : null };
};

export default function App() {
  const [runs, setRuns] = useState<Run[]>([]);
  const [gates, setGates] = useState<Gate[]>([]);
  const [details, setDetails] = useState<Record<string, RunDetail>>({});
  const [sel, setSel] = useState(readHash());
  const [err, setErr] = useState<string | null>(null);
  const [tick, setTick] = useState(0);
  const [filter, setFilter] = useState<Ball | "all">("all");
  const selRef = useRef(sel);
  selRef.current = sel;

  const refresh = useCallback(async () => {
    try {
      const [rs, gs] = await Promise.all([fetchRuns(), fetchGates()]);
      setRuns(rs ?? []);
      setGates(gs ?? []);
      setErr(null);
      const cur = selRef.current;
      if (cur.thread) {
        const wanted = (rs ?? []).filter(r => (r.Thread || r.ID) === cur.thread);
        const ds = await Promise.all(wanted.map(r => fetchDetail(r.ID)));
        setDetails(Object.fromEntries(ds.map(d => [d.run.ID, d])));
      }
    } catch (e) {
      setErr(String(e));
    }
  }, []);

  useEffect(() => {
    void refresh();
    const iv = setInterval(() => { setTick(t => t + 1); void refresh(); }, 4000);
    const onHash = () => setSel(readHash());
    window.addEventListener("hashchange", onHash);
    return () => { clearInterval(iv); window.removeEventListener("hashchange", onHash); };
  }, [refresh]);

  const nav = (thread: string | null, run: string | null = null) => {
    const h = thread ? `#t=${encodeURIComponent(thread)}${run ? `&r=${encodeURIComponent(run)}` : ""}` : "";
    history.replaceState(null, "", location.pathname + h);
    setSel({ thread, run });
    void refresh();
  };

  const threads = useMemo(() => group(runs, gates), [runs, gates]);
  const current = threads.find(t => t.key === sel.thread);

  return (
    <div className="mx-auto max-w-5xl px-5 pb-24">
      <header className="sticky top-0 z-10 -mx-5 mb-5 flex items-baseline gap-3 border-b border-zinc-800 bg-[#0b0e13]/95 px-5 py-3 backdrop-blur">
        <span className="text-[14px] font-bold tracking-[3px]">KRAKOA</span>
        <span className="text-zinc-500">
          {sel.thread ? <><a className="cursor-pointer text-blue-400" onClick={() => nav(null)}>threads</a> / <Mono>{sel.thread}</Mono></> : "threads"}
        </span>
        <span className="ml-auto text-[11px] text-zinc-600">{err ? `daemon unreachable (${err})` : `updated ${new Date().toLocaleTimeString()}`}</span>
      </header>

      {/* Attention (real decisions, undelivered pings) above infra noise —
          13 identical watcher gates once WERE the page. */}
      <UndeliveredStrip gates={gates} />
      {engineGates(gates).attention.map(g => <div key={g.ID} className="mb-4"><GateCard gate={g} onDone={refresh} /></div>)}
      <InfraStrip gates={engineGates(gates).infra} onDone={refresh} />

      {!current ? (
        <div className="flex flex-col gap-3">
          <FilterBar value={filter} onChange={setFilter} counts={countByBall(threads)} />
          {threads.filter(t => filter === "all" || ball(t.runs, t.gates) === filter)
                  .map(t => <ThreadCard key={t.key} t={t} onOpen={() => nav(t.key)} />)}
          {!threads.length && !err && <div className="py-20 text-center text-zinc-600">no runs yet</div>}
        </div>
      ) : (
        <ThreadDetail t={current} details={details} selRun={sel.run} onRun={id => nav(sel.thread, id)} onGate={refresh} />
      )}
    </div>
  );
}

/* Engine-level gates split by what they are: a decision you owe (attention)
   vs a watcher telling you it is broken (infra). Infra collapses to one row
   per watcher with a count. */
function engineGates(gates: Gate[]) {
  const engine = gates.filter(g => !g.RunID);
  return {
    attention: engine.filter(g => !g.State.startsWith("watcher:")),
    infra: engine.filter(g => g.State.startsWith("watcher:")),
  };
}

function InfraStrip({ gates, onDone }: { gates: Gate[]; onDone: () => void }) {
  const [open, setOpen] = useState<string | null>(null);
  if (!gates.length) return null;
  const byWatcher = new Map<string, Gate[]>();
  gates.forEach(g => byWatcher.set(g.State, [...(byWatcher.get(g.State) ?? []), g]));
  return (
    <div className="mb-4 flex flex-col gap-2">
      {[...byWatcher.entries()].map(([state, gs]) => (
        <div key={state} className="rounded-xl border border-zinc-800 bg-zinc-900/40 px-3 py-2 text-[12px]">
          <div className="flex items-baseline gap-2">
            <Chip cls="bg-zinc-700/60 text-zinc-300">infra</Chip>
            <span className="text-zinc-300">{state.replace("watcher:", "")}</span>
            <span className="text-zinc-500">{gs.length > 1 ? `${gs.length} open gates` : "1 open gate"}</span>
            <a className="ml-auto cursor-pointer text-blue-400"
               onClick={() => setOpen(open === state ? null : state)}>{open === state ? "hide" : "expand"}</a>
          </div>
          <div className="mt-1 text-zinc-500">{gs[gs.length - 1].Payload}</div>
          {open === state && (
            <div className="mt-2 flex flex-col gap-2">
              {gs.map(g => <GateCard key={g.ID} gate={g} onDone={onDone} />)}
            </div>
          )}
        </div>
      ))}
    </div>
  );
}

/* An undelivered gate exists ONLY here — nothing else in the world knows it
   was never seen. So it gets the loudest treatment on the page. */
function UndeliveredStrip({ gates }: { gates: Gate[] }) {
  const bad = gates.filter(g => Object.values(g.Delivery ?? {}).some(v => v !== "ok"));
  if (!bad.length) return null;
  return (
    <div className="mb-4 rounded-xl border-2 border-red-500 bg-red-950/40 p-3">
      <div className="text-[13px] font-semibold text-red-200">
        ⚠ {bad.length} gate{bad.length > 1 ? "s were" : " was"} never delivered — visible only on this page
      </div>
      {bad.map(g => (
        <div key={g.ID} className="mt-1 text-[12px] text-red-300">
          <Mono>{g.ID}</Mono> {g.Payload.slice(0, 120)}
        </div>
      ))}
    </div>
  );
}

function countByBall(threads: Thread[]) {
  const c: Record<string, number> = {};
  threads.forEach(t => { const b = ball(t.runs, t.gates); c[b] = (c[b] ?? 0) + 1; });
  return c;
}

function FilterBar({ value, onChange, counts }: {
  value: Ball | "all"; onChange: (b: Ball | "all") => void; counts: Record<string, number>;
}) {
  const opts: (Ball | "all")[] = ["all", "your-turn", "blocked", "machine", "world", "dead", "done"];
  return (
    <div className="flex flex-wrap gap-2 text-[12px]">
      {opts.map(o => (
        <button key={o} onClick={() => onChange(o)}
          className={cx("rounded-md border px-2 py-0.5",
            value === o ? "border-blue-500 bg-blue-500/10 text-blue-200" : "border-zinc-800 bg-zinc-900 text-zinc-400 hover:border-zinc-600")}>
          {o === "all" ? "all" : ballCopy[o].label.toLowerCase()}
          {o !== "all" && counts[o] ? ` ${counts[o]}` : ""}
        </button>
      ))}
    </div>
  );
}

function ThreadCard({ t, onOpen }: { t: Thread; onOpen: () => void }) {
  const b = ball(t.runs, t.gates);
  const copy = ballCopy[b];
  const latest = t.runs[t.runs.length - 1];
  const line = plainLanguage(t, b);
  return (
    <Card onClick={onOpen} className={cx(b === "your-turn" && "border-amber-400/60", b === "dead" && "border-red-500/40")}>
      <div className="flex items-baseline justify-between gap-3">
        <span className="text-[15px] font-semibold">{t.key}</span>
        <Chip cls={copy.cls}>{copy.label}</Chip>
      </div>
      <div className="mt-1 text-[12px] text-zinc-400">{line}</div>
      <div className="mt-3 flex flex-wrap items-center gap-2 text-[12px]">
        {byWorkflow(t.runs).map(w => (
          <span key={w.workflow} className="rounded-md border border-zinc-800 bg-zinc-950/60 px-2 py-0.5">
            <span className="text-zinc-500">{w.workflow}</span>
            {w.count > 1 && <span className="text-zinc-600"> ×{w.count}</span>}
            {" · "}{w.newest.State} <Chip cls={statusCls[w.newest.Status]}>{w.newest.Status}</Chip>
          </span>
        ))}
        <span className="ml-auto text-zinc-500">{ago(latest.UpdatedAt)} ago</span>
      </div>
      {t.gates.length > 0 && (
        <div className="mt-2 text-[12px] text-amber-300">⚑ {t.gates.length} gate{t.gates.length > 1 ? "s" : ""} waiting for you — open the thread to act</div>
      )}
    </Card>
  );
}

/* Six sweeper runs used to read "review-sweeper · done done" six times. One
   line per WORKFLOW, with a count and the newest run's state. */
function byWorkflow(runs: Run[]) {
  const by = new Map<string, Run[]>();
  runs.forEach(r => by.set(r.Workflow, [...(by.get(r.Workflow) ?? []), r]));
  return [...by.entries()].map(([workflow, rs]) => ({
    workflow,
    count: rs.length,
    newest: rs.reduce((a, b) => (a.UpdatedAt > b.UpdatedAt ? a : b)),
  }));
}

/* F5's plain-language line: what is happening + what (if anything) is needed. */
function plainLanguage(t: Thread, b: ReturnType<typeof ball>): string {
  if (b === "your-turn") return `${t.gates.length} decision${t.gates.length > 1 ? "s" : ""} waiting on you`;
  if (b === "machine") {
    const r = t.runs.find(x => x.Status === "running")!;
    return `${r.Workflow}: agent working in ${r.State} (${ago(r.UpdatedAt)})`;
  }
  if (b === "world") {
    const r = t.runs.find(x => x.Status === "waiting")!;
    return `waiting on the world in ${r.State} — ${ago(r.UpdatedAt)} so far, nothing needed from you`;
  }
  if (b === "blocked") {
    const r = t.runs.find(x => x.Status === "blocked")!;
    const why = (r.Context?.blocked as { check?: string; detail?: string } | undefined);
    return `blocked on ${why?.check ?? "a prerequisite"} in ${r.State} — resumes by itself when it passes${why?.detail ? ` (${why.detail})` : ""}`;
  }
  if (b === "dead") {
    const r = t.runs.find(x => x.Status === "failed" || x.Status === "canceled");
    return r ? `ended without finishing in ${r.State} — ${deadReason(r)}` : "ended without finishing";
  }
  return "all runs finished";
}

/* A dead run said "ended without finishing" and nothing else. Say why. */
function deadReason(r: Run): string {
  const blocked = r.Context?.blocked as { check?: string } | undefined;
  if (blocked?.check) return `last blocked on ${blocked.check}`;
  if (r.Status === "canceled") return "canceled";
  return `abandoned at ${r.State}`;
}

function ThreadDetail({ t, details, selRun, onRun, onGate }: {
  t: Thread; details: Record<string, RunDetail>; selRun: string | null;
  onRun: (id: string) => void; onGate: () => void;
}) {
  const active = selRun ?? t.runs[t.runs.length - 1].ID;
  const detail = details[active];
  const steps = Object.values(details).flatMap(d => d.steps ?? []);
  const links = harvestLinks(steps, t.runs);
  const cost = steps.reduce((a, s) => a + (s.CostUSD || 0), 0);
  const b = ball(t.runs, t.gates);
  const finished = b === "done" || b === "dead";

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-baseline gap-3">
        <Chip cls={ballCopy[b].cls}>{ballCopy[b].label}</Chip>
        <span className="text-zinc-400">{plainLanguage(t, b)}</span>
        {cost > 0 && <span className="ml-auto text-zinc-500">{usd(cost)} total</span>}
      </div>

      {links.length > 0 && (
        <div className="flex flex-wrap gap-2">
          {links.map(l => (
            <a key={l.href} href={l.href} target="_blank" rel="noopener"
               className="rounded-md border border-zinc-700 bg-zinc-900 px-2 py-0.5 text-[12px] text-blue-300 hover:border-blue-500">
              {l.label} ↗
            </a>
          ))}
        </div>
      )}

      {t.gates.map(g => <GateCard key={g.ID} gate={g} onDone={onGate} />)}

      <div className="flex flex-wrap gap-2">
        {t.runs.map(r => (
          <button key={r.ID} onClick={() => onRun(r.ID)}
            className={cx("rounded-lg border px-3 py-1.5 text-[12px]",
              r.ID === active ? "border-blue-500 bg-blue-500/10" : "border-zinc-800 bg-zinc-900 hover:border-zinc-600")}>
            <Dot cls={r.Status === "running" ? "bg-blue-400" : r.Status === "waiting" ? "bg-amber-400" : r.Status === "done" ? "bg-emerald-400" : "bg-red-400"}
                 live={r.Status === "running" || r.Status === "waiting"} />
            {r.Workflow} <Mono className="text-zinc-500">{r.ID.slice(-8)}</Mono>
          </button>
        ))}
      </div>

      {detail ? <RunFlow detail={detail} /> : <div className="py-10 text-center text-zinc-600">loading…</div>}

      {finished && <RetroCard t={t} details={details} />}
    </div>
  );
}

/* F9 — the auto-retro on a finished thread. */
function RetroCard({ t, details }: { t: Thread; details: Record<string, RunDetail> }) {
  const r = retro(
    t.runs,
    Object.fromEntries(Object.entries(details).map(([k, d]) => [k, d.steps ?? []])),
    Object.fromEntries(Object.entries(details).map(([k, d]) => [k, d.events ?? []])),
  );
  if (!r.wall) return null;
  return (
    <Card className="border-zinc-700">
      <Label>retro</Label>
      <div className="mb-2">
        {t.key} took <b>{dur(r.wall)}</b> wall time: <b>{dur(r.agentMs)}</b> of agent work, <b>{dur(r.waitMs)}</b> waiting on the world,
        {" "}<b>{usd(r.cost) || "$0"}</b> spent, {r.interventions.length} intervention{r.interventions.length === 1 ? "" : "s"}.
      </div>
      {r.interventions.length > 0 && (
        <ul className="list-disc pl-5 text-[12px] text-zinc-400">
          {r.interventions.map((i, n) => <li key={n}>{i}</li>)}
        </ul>
      )}
    </Card>
  );
}
