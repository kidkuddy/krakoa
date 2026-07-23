---
name: krakoa
description: Drive the Krakoa workflow daemon — start durable agent workflows (e.g. "krakoa, <task idea>" files, builds, reviews, merges, and notifies autonomously), surface and answer gates, and inspect runs with why. Use when the user prefixes a task with "krakoa", asks to run a task through Krakoa, asks about Krakoa runs/gates, or dumps a bare feature/bug task description that should be routed to the task-lifecycle workflow.
---

# krakoa — front-end to the Krakoa daemon

Krakoa runs durable, agent-driven workflows. You talk to it exclusively
through the `krakoactl` CLI (on PATH; override with `$KRAKOA_CTL`). The
daemon address comes from `$KRAKOA_ADDR` (default `http://127.0.0.1:7770`).
Never hardcode repo or install paths — the CLI and env vars are the whole
interface.

## Preflight (first krakoa action in a session)

```bash
krakoactl doctor
```
If krakoad or a prerequisite is down, show the doctor output — it prints
the exact fix commands (launchctl load, /callab-init, etc.). Don't
improvise fixes.

## Start a task (the main flow)

Trigger: "krakoa, <task idea>" — or a bare task/bug description the user
confirmed should run via Krakoa.

```bash
krakoactl run task-lifecycle --workspace callab \
  --input idea='<the task text, verbatim>' \
  --input repo='<gitlab repo path, only if the user named one>' \
  --input env='<dev|staging|prod, only if the user named one>'
```

- Pass the user's idea text **verbatim** — refinement is the workflow's
  job, not yours.
- `repo` is optional (defaults to the dashboard repo); set it only when
  the user names a repo.
- `env` is optional (defaults to `dev`). It decides the trunk the MR targets
  and the namespace the rollout probe watches — only set it when the user
  says which environment.
- A run rejected as **duplicate** ("active run <id> already holds ...") is the
  guard doing its job: the same work is already in flight (a watcher may have
  spawned it). Report the existing run id; never retry to force a second one.
- Report the run id back. Do NOT promise a time for the refine questions —
  refinement is two agent rounds plus a revise loop, and "a couple of
  minutes" was wrong by half an hour. The gate announces itself in-thread the
  moment it opens; you do not have to predict it.

```bash
krakoactl gates
```

## Gates — surface and answer in chat

`krakoactl gates` lists open gates across all runs. Relay each gate's
payload to the user verbatim. When they answer:

- Question gate (refine questions):
  `krakoactl answer <gate-id> answered --answers 'q1=<answer>' --answers 'q2=<answer>'`
- Choice gate (workflow-declared, e.g. dispatch-stalled: retry | abandon):
  `krakoactl answer <gate-id> <option>`
- Needs-attention gate: `retry` | `abandon`.

Some gates belong to no run — the engine raises them about the system itself:

- **Alert gate** (`acknowledged`): an event the workspace declared un-droppable
  (`mr-ready`, `mr-unlinked`, `mr-mistargeted`) fired with nothing waiting to
  consume it. One gate per (event, key), payload updated in place. It means a
  real MR is sitting there with no run behind it — say what the event names
  before acknowledging; `acknowledged` silences that key permanently.
- **Watcher gate** (`retry` | `pause-24h` | `disable`): a watcher has failed N
  consecutive sweeps, so nothing it drives is advancing. The options act.
- **Signal-stall gate** (`advance` | `discard`): a run holds a buffered signal
  no reachable state can consume. `advance` jumps to the state that handles it.

First response wins; a "gate already resolved" error just means it was
answered elsewhere (e.g. Slack) — say so.

## Inspect

- `krakoactl runs` — all runs; `krakoactl runs --status gated,needs-attention` — what needs the user.
  Statuses: `queued` `running` `waiting` `gated` `blocked` `done` `failed`
  `needs-attention` `canceled`.
- `krakoactl threads` — runs grouped by the work they serve (one line per
  thread: state summary, runs, cost, age). Use this, not `runs`, when the user
  asks "what's going on" — a ticket is usually several runs.
- `krakoactl cancel <run-id> [reason...]` — stop a run wherever it stands
  (running step included). Terminal and irreversible: only on an explicit
  "cancel/kill/stop that run" from the user, never as cleanup of your own.
- `krakoactl checks` — the live prerequisite board: what is failing, how many
  runs it is holding, and the fix. This is the first thing to look at when a
  run is `blocked`.
- `krakoactl resume <run-id>` — manual un-block / re-enter for a run parked on
  a check or on needs-attention.
- `krakoactl why <run-id>` — full timeline: per-step outcomes, Claude
  session ids + transcript paths, handoff dirs, costs, every event. Use
  this whenever the user asks "what's happening with <run>" or after a
  run finishes.
- Deep post-mortem: resume a step's session with
  `claude --resume <session-id>` from the step's listed session.

## Emit (rare, manual)

External signal into a waiting run:
`krakoactl emit <event> --workspace callab --key <correlation-key>`

## Rules

- Never bypass Krakoa (never file the ticket / run glab yourself) once the
  user routed a task here — the workflow owns the lifecycle end to end.
- **Krakoa merges.** The state after review-flips-the-MR-ready merges by
  itself; the done message says krakoa merged it. Never tell the user a merge
  is theirs to do, and never merge on krakoa's behalf.
- **Only your own task's gates.** `krakoactl gates` is global. Discuss the
  gates that belong to the task this session owns and ignore every other one
  unless the user explicitly asks something like "are there open questions
  anywhere?". A gate from another thread mentioned here is noise in someone
  else's conversation.
- **One autonomy rule.** Harness-state repairs are done immediately, with a
  one-line note afterwards: restoring a ticket status krakoa expects, clearing
  a stale gate, re-emitting a signal, re-dispatching a build that never
  enqueued, resuming a blocked run once its check passes. All of it is
  reversible and none of it is your judgment. Scope, cost and product
  decisions gate and nag: never widen a ticket, never spend on a rerun the
  user did not ask for, never decide a product question the refiner raised.
- **Blocked is not your turn.** A run in `blocked` is waiting on the world
  (an expired token, a dead daemon), not on a decision. Say what check it is
  blocked on and the fix command from `krakoactl checks`; it resumes on its
  own once the check passes. Do not open gates or invent workarounds.
- Don't poll aggressively; check gates/runs when the user asks or after
  starting a run. Slack (niffty) pings the user when a gate opens.
- After a run reaches `done`, offer `krakoactl why <run>` as the review.

## Slack agent mode (niffty-spawned sessions)

When this session was spawned by niffty from a Slack DM (the spawn context
names the Slack thread), and the message is a task/bug description:

1. Start the run: `krakoactl run task-lifecycle --workspace callab --input idea='<message verbatim>'`
2. Nothing to bind by hand — `krakoactl run` binds the Slack thread itself
   from `$NIFFTY_TASK` when a niffty session starts the run. Only bind
   manually (`krakoactl bind <run-id> --slack-ts <ts>`) if that env var is
   absent and gates are landing top-level.
3. Say nothing about "routing via krakoa" — krakoa posts its own start line
   in the thread, and two receipts for one event is the duplicate-render bug.
   Reply in-thread only if you have something krakoa did not say.

Krakoa hands lifecycle events to THIS session (gate questions, merged and
deployed, a stuck step) rather than posting them itself. When one arrives:
render it in one or two short lines, or act on it. A stuck-step event asks you
to try the recovery yourself first — re-emit the signal, restore the ticket
status, retry the step — and to report only what you could not fix.

In-thread follow-ups route conversationally:
- "done" (after editing a question canvas) → `krakoactl harvest <gate-id>`
  (find the open gate with `krakoactl gates`).
- Option words ("retry", "abandon", "ship it" ≈ approve) →
  `krakoactl answer <gate-id> <option>`.
- Status questions → `krakoactl runs --thread <key>` / `why`, summarize
  in-thread.

## Changing what Krakoa does (not this skill's job)

Workflows, agents, watchers, checks and probe scripts live in the workspace
repo (`~/krakoa-workflows/workspaces/<name>`), not in the engine. Editing them
is the **krakoa-create-workflow** skill — use it, and note that the daemon
loads workspaces only at start, so any edit needs
`launchctl kickstart -k gui/$UID/com.krakoa.krakoad` before it exists at
runtime. In-flight runs keep the definition they started with.

## Install locations (do not assume the caller's shell)

`krakoactl`/`krakoad` are installed by `make install` to `~/.local/bin/`
with a symlink at `/opt/homebrew/bin/krakoactl` so clean-PATH sessions
(Slack agent mode) resolve it without any shell profile. If `krakoactl` is
not found, use `/opt/homebrew/bin/krakoactl` explicitly — never assume
`KRAKOA_*` env vars are set (defaults work: daemon at 127.0.0.1:7770).
