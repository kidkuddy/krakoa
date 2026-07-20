import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { fetchDetail, fetchGates, fetchRuns, type Gate, type Run, type RunDetail } from "./api";
import { ago, ball, ballCopy, clock, cx, dur, harvestLinks, retro, usd } from "./lib";
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
    return b === "your-turn" ? 0 : b === "machine" ? 1 : b === "world" ? 2 : b === "dead" ? 3 : 4;
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

      {/* engine-level gates (watcher failures) always on top */}
      {gates.filter(g => !g.RunID).map(g => <div key={g.ID} className="mb-4"><GateCard gate={g} onDone={refresh} /></div>)}

      {!current ? (
        <div className="flex flex-col gap-3">
          {threads.map(t => <ThreadCard key={t.key} t={t} onOpen={() => nav(t.key)} />)}
          {!threads.length && !err && <div className="py-20 text-center text-zinc-600">no runs yet</div>}
        </div>
      ) : (
        <ThreadDetail t={current} details={details} selRun={sel.run} onRun={id => nav(sel.thread, id)} onGate={refresh} />
      )}
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
        {t.runs.map(r => (
          <span key={r.ID} className="rounded-md border border-zinc-800 bg-zinc-950/60 px-2 py-0.5">
            <span className="text-zinc-500">{r.Workflow}</span> · {r.State} <Chip cls={statusCls[r.Status]}>{r.Status}</Chip>
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
  if (b === "dead") return "ended without finishing";
  return "all runs finished";
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
