import { useState } from "react";
import { answerGate, type Gate } from "../api";
import { Btn, Card, Chip, Mono } from "./bits";

/* F1 — act on gates from the UI. Options render as buttons; question gates
   get a free-text answer box ("question = answer" per line also works).
   First-response-wins is engine-side; a stale click shows the 409 message. */
export function GateCard({ gate, onDone }: { gate: Gate; onDone: () => void }) {
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const [text, setText] = useState("");
  const att = gate.Payload.startsWith("needs attention");

  const send = async (response: string, answers: Record<string, string> = {}) => {
    setBusy(true);
    const err = await answerGate(gate.ID, response, answers);
    setBusy(false);
    setMsg(err ? (err.includes("already resolved") ? "already resolved elsewhere" : err) : "sent ✓");
    if (!err) setTimeout(onDone, 400);
  };

  const sendAnswers = () => {
    const answers: Record<string, string> = {};
    const lines = text.split("\n").map(l => l.trim()).filter(Boolean);
    lines.forEach((l, i) => {
      const eq = l.indexOf("=");
      if (eq > 0) answers[l.slice(0, eq).trim()] = l.slice(eq + 1).trim();
      else answers[`answer_${i + 1}`] = l;
    });
    void send("answered", answers);
  };

  const failedDeliveries = Object.entries(gate.Delivery ?? {}).filter(([, v]) => v !== "ok");

  return (
    <Card className={att ? "border-red-500/60 bg-red-950/20" : "border-amber-500/50 bg-amber-950/10"}>
      <div className="mb-2 flex items-baseline gap-2">
        <Chip cls={att ? "bg-red-500/25 text-red-200" : "bg-amber-400/25 text-amber-200"}>
          {att ? "needs attention" : gate.Kind}
        </Chip>
        <Mono className="text-zinc-500">{gate.ID}</Mono>
      </div>
      <div className="mb-3 whitespace-pre-wrap">{gate.Payload}</div>
      {failedDeliveries.map(([ch, err]) => (
        <div key={ch} className="mb-2 text-[12px] text-red-300">⚠ {ch} delivery failed — seen only here <span className="text-zinc-500">({err})</span></div>
      ))}
      {gate.Kind === "question" ? (
        <div className="flex flex-col gap-2">
          <textarea
            value={text}
            onChange={e => setText(e.target.value)}
            onClick={e => e.stopPropagation()}
            rows={3}
            placeholder={"answers — one per line, or  question = answer"}
            className="w-full rounded-md border border-zinc-700 bg-zinc-950 p-2 font-mono text-[12px] outline-none focus:border-blue-500"
          />
          <div><Btn tone="primary" onClick={sendAnswers} disabled={busy || !text.trim()}>Send answers</Btn></div>
        </div>
      ) : (
        <div className="flex flex-wrap gap-2">
          {(gate.Options ?? ["approved", "rejected"]).map(o => (
            <Btn
              key={o}
              tone={o === "retry" || o === "approved" ? "primary" : o === "abandon" || o === "rejected" ? "danger" : "default"}
              onClick={() => void send(o)}
              disabled={busy}
            >
              {o}
            </Btn>
          ))}
        </div>
      )}
      {msg && <div className="mt-2 text-[12px] text-zinc-400">{msg}</div>}
    </Card>
  );
}
