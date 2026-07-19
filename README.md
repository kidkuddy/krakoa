# Krakoa

A durable workflow manager for autonomous, agent-driven work. You describe a
workflow as a declarative state machine; Krakoa runs instances of it durably —
spawning clean-context [Claude Code](https://claude.com/claude-code) agents
for each step, watching external systems with deterministic probes, and
contacting you exactly when a human decision is needed.

The engine is **use-case blind**. It knows nothing about your ticket tracker,
your git host, or your chat tool. Everything concrete lives in **workspaces**
— separate data repos holding workflow definitions, agent specs, skills, and
scripts. Krakoa is the engine; your workflows are your data.

## How it works

```
workspace repo                     krakoa engine                    the world
──────────────                     ─────────────                    ─────────
workflows/*.yaml   ──loaded──▶  pure interpreter                 Claude Code
agents/*.yaml                   (state, event) → actions   ──▶   headless agents
skills/*/SKILL.md               executor + scheduler             workspace scripts
scripts/*                       SQLite (runs, steps,       ◀──   events, emits,
watchers/*.yaml                 gates, events, timers)           watcher sweeps
```

- **Durable state, not durable code.** Every decision round-trips SQLite
  (WAL). A crash loses at most the in-flight step attempt; recovery reloads
  runs, re-arms timers, and re-attempts with check-first semantics. No
  workflow server, no replay determinism constraints.
- **One execution primitive for mutations and judgment**: a step that touches
  the world is a Claude Code agent carrying markdown skills from the
  workspace. Agents run isolated (`--setting-sources project`) — only the
  persona and skills you authored apply.
- **Deterministic command probes for pure observation**: watchers and wait
  probes can be plain workspace scripts (JSON on stdout, direct exec, no
  session, no cost) on a fast cadence.
- **Failures are gates.** Retries exhausted, budgets exceeded, derailed
  probes — the run parks as needs-attention and you get contacted through
  the same channels as any approval. Never silence.
- **Everything is an event.** Transitions, attempts, gate deliveries,
  watcher observations, costs — one append-only SQLite table, one
  `sqlite3` query away.

## Concepts

| Concept | What it is |
|---|---|
| **WorkflowDefinition** | YAML state machine. States carry one step: `agent`, `gate` (question/approval/choice), or `wait` (event / timer / probe arms — timeout arm mandatory). Transitions on outcomes; loop budgets per edge; per-workflow concurrency with FIFO queueing. Runs pin the definition hash. |
| **AgentSpec** | Named agent: persona (becomes the agent's CLAUDE.md), skills carried, working folder, optional git-worktree isolation, model/effort knobs. |
| **Skill** | Markdown instruction set (same shape as Claude Code skills), copied into the agent's home per step. Skills encode environment specifics — that's why the engine doesn't have to. |
| **Watcher** | Scheduled probe emitting deduped events — an agent (judgment) or a workspace command script (pure observation). Two modes: spawn a run per observation, or resume waiting runs by correlation key. |
| **Gate** | Parked human interaction, delivered via contact channels (console, Slack relay). First response wins. Answer with `krakoactl answer`. |
| **Handoff** | Agents report by writing `result.json` (`{"outcome": ...}`) into a per-step handoff dir. Invalid result → the same session is resumed once with the error; still invalid → needs-attention. Session ids and transcript paths are stored per step. |

## Quick start

```bash
make build          # bin/krakoad, bin/krakoactl  (or: make install)

# validate + simulate a workspace (walks EVERY transition edge synthetically)
krakoactl workspace validate  path/to/workspace
krakoactl workspace dry-run   path/to/workspace my-workflow

# run the daemon
KRAKOA_WORKSPACES=path/to/workspace bin/krakoad

# drive it
krakoactl run my-workflow --workspace my-ws --input idea='fix the thing'
krakoactl runs
krakoactl gates
krakoactl answer <gate-id> <response> [--answers k=v]
krakoactl why <run-id>          # full timeline: steps, sessions, costs, events
krakoactl emit <event> --workspace my-ws --key <correlation-key>
krakoactl doctor                # engine + workspace-declared preflight checks
```

The audit UI lives at `http://127.0.0.1:7770/ui` — run cards, per-run step
sidebar with a markdown reading pane, open gates with their unblock
commands. Self-contained, localhost only.

## A minimal workspace

```
my-workspace/
  workspace.yaml            # name, description, optional doctor: checks
  policies.yaml             # optional gatekeepers (class -> allowed agents)
  workflows/review.yaml
  agents/reviewer.yaml
  skills/review-style/SKILL.md
  scripts/sweep             # deterministic watcher probe (JSON on stdout)
  watchers/pr-watch.yaml
```

```yaml
# workflows/review.yaml
name: review
trigger: {kind: watcher, watcher: pr-watch}
inputs:
  pr: {type: string, required: true}
concurrency: 2
start: reviewing
states:
  reviewing:
    step: agent
    agent: reviewer
    instruction: Review PR $input.pr per your skill.
    on: {pass: done, fail: requesting-changes}
  requesting-changes:
    step: agent
    agent: reviewer
    instruction: Post the findings.
    on: {ok: done}
  done: {terminal: true}
```

Validation enforces the invariants (terminal states, timeout arms, budget
edges, gatekeeper policies, script existence), and `dry-run` executes every
declared edge against a synthetic runner before anything touches the world.

## Configuration (env)

| Var | Default | |
|---|---|---|
| `KRAKOA_WORKSPACES` | — (required) | comma-separated workspace dirs |
| `KRAKOA_DB` | `~/.krakoa/krakoa.db` | SQLite path |
| `KRAKOA_DATA_DIR` | `~/.krakoa/data` | per-step agent homes + handoffs |
| `KRAKOA_HTTP_ADDR` | `127.0.0.1:7770` | ingress (CLI, emits, UI) |
| `KRAKOA_NIFFTY_URL` / `KRAKOA_NIFFTY_TO` | — | optional Slack relay channel |
| `KRAKOA_CLAUDE_BIN` | auto-resolved | claude binary override |
| `KRAKOA_ADDR` | `http://127.0.0.1:7770` | krakoactl → daemon |

Logs to stdout; supervise with launchd/systemd
(`deploy/com.krakoa.krakoad.plist.example`).

## Development

```bash
make test    # interpreter tables + hermetic end-to-end seam tests on fakes
make vet
```

The interpreter is a pure function with exhaustive table tests; the seam
tests run full workflows against a scripted runner, fake clock, and
in-memory SQLite — including kill-mid-run recovery, timer re-arming,
correlation routing, budget parks, and watcher failure strikes.
