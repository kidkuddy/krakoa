// Typed veneer over krakoad's JSON API. The UI is a pure driving adapter:
// nothing here that krakoactl couldn't do.

export interface Run {
  ID: string; Workspace: string; Workflow: string;
  State: string; Status: string; Thread: string;
  DefHash: string; WSVersion: string; Parent: string;
  Inputs: Record<string, unknown>; Context: Record<string, unknown>;
  CreatedAt: string; UpdatedAt: string;
}
export interface Step {
  ID: number; RunID: string; State: string; Kind: string; Attempt: number;
  Inputs: Record<string, unknown>; Outcome: string; Result: Record<string, unknown>;
  Error: string; SessionID: string; SessionPath: string; CostUSD: number;
  StartedAt: string; EndedAt: string;
}
export interface Event {
  ID: number; RunID: string; State: string; Kind: string;
  Data: Record<string, unknown> | null; At: string;
}
export interface Gate {
  ID: string; RunID: string; State: string; Kind: string; Payload: string;
  Options: string[] | null; Status: string; Delivery: Record<string, string> | null;
  CreatedAt: string;
}
export interface Timer {
  ID: number; RunID: string; State: string; Kind: string;
  FireAt: string; Every: number; // ns
}
export interface ThreadSummary {
  Thread: string; Runs: number; Statuses: string; CostUSD: number;
  FirstSeen: string; LastSeen: string;
}
export interface RunDetail { run: Run; steps: Step[] | null; events: Event[] | null; timers: Timer[] | null; }

const j = <T,>(p: string): Promise<T> => fetch(p).then(r => {
  if (!r.ok) throw new Error(`${r.status}`);
  return r.json();
});

export const fetchRuns = () => j<Run[] | null>("/v1/runs");
export const fetchGates = () => j<Gate[] | null>("/v1/gates");
export const fetchDetail = (id: string) => j<RunDetail>(`/v1/runs/${id}`);

export async function answerGate(id: string, response: string, answers: Record<string, string>): Promise<string | null> {
  const r = await fetch(`/v1/gates/${id}/answer`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ response, answers, responder: "ui" }),
  });
  if (r.ok) return null;
  const body = await r.json().catch(() => ({}));
  return body.error ?? `HTTP ${r.status}`;
}
