---
name: krakoa-create-workflow
description: Author or edit a Krakoa workflow — the YAML state machine, its agents, watchers, probe scripts and workspace wiring — then validate, dry-run and deploy it. Use when adding a new automated loop to a Krakoa workspace, changing an existing workflow's states/gates/waits, adding an agent or watcher, or debugging why a workspace refuses to load.
---

# Author a Krakoa workflow

Krakoa is a durable workflow manager. A workflow is a **declarative YAML state
machine** in a **workspace data repo**; the engine is use-case blind and never
learns about your tracker, git host or chat tool. You almost never touch Go —
authoring is writing data.

## The two repos (never blur them)

| | |
|---|---|
| **engine** `~/Desktop/github/krakoa` | Go: interpreter, scheduler, store, runner, CLI, UI. Changes here are for *capabilities the DSL lacks*. |
| **workspace** `~/krakoa-workflows/workspaces/<name>` | YAML + markdown + scripts: workflows, agents, skills, watchers, checks. Everything environment-specific lives here. Own git repo, own commits. |

If a task needs an env var, a tool name, a repo path or a ticket convention in
the engine, you took a wrong turn — it belongs in the workspace.

```
workspaces/callab/
  workspace.yaml          # name, checks, alerts, envs, repos
  policies.yaml           # optional gatekeepers (class -> allowed agents)
  workflows/*.yaml        # the state machines
  agents/*.yaml           # personas + skills + working folder
  skills/<name>/SKILL.md  # copied into the agent's home per step
  watchers/*.yaml         # scheduled probes that emit events
  scripts/*               # executable, JSON on stdout, $0
```

## The authoring loop

```bash
WS=~/krakoa-workflows/workspaces/callab
krakoactl workspace validate $WS            # structure + refs; a failing workspace is REFUSED wholesale
krakoactl workspace dry-run  $WS <workflow> # walks every declared edge on a synthetic runner
```

Both must be green before anything lands. Then deploy — **the daemon loads
workspaces once, at start**; a workflow edit is invisible until it restarts:

```bash
launchctl kickstart -k gui/$UID/com.krakoa.krakoad
krakoactl doctor                            # confirms daemon up + workspace checks
```

In-flight runs pin the definition hash they started with, so a restart never
mutates a running workflow — edits apply to runs started after. (`env` context
is the one thing backfilled onto older runs.)

## Workflow reference

```yaml
name: task-lifecycle          # defaults to filename
trigger: {kind: manual}       # manual | schedule (cron:, skip_if_running:) | watcher (watcher: <name>)
inputs:
  idea: {type: text, required: true}
  env:  {type: string, default: dev}
  repo: {type: string, default: callab.ai/callab-dashboard}
concurrency: 10               # 0 = unlimited; excess runs queue FIFO
thread: "$filing.ticket_id"   # groups runs serving one piece of work; stamped when it resolves
unique: "$input.mr_url"       # two ACTIVE runs may not share this key — dedupes manual vs watcher starts
requires: [multica-auth, glab, niffty]   # workspace checks that must pass at admission
start: refining
entries:                      # named verbs beside the default `start` (optional)
  followup:
    start: following-up       # the state this verb begins in
    inputs:                   # merged OVER the workflow's; redeclare to drop `required`
      idea: {type: text}
      ticket_id: {type: string, required: true}
    seed:                     # state -> result, templated over $input
      filing: {ticket_id: $input.ticket_id}
states: {...}
```

`requires` failing → the run goes **blocked** (not failed): it releases its
concurrency slot and auto-resumes when the check passes again. Put a check on
the *state* that needs it, not the workflow, when only one step needs it
(`rollout-wait` needs the cluster; refining does not).

### Entries (verbs)

Start with none. Add one when the same piece of work needs re-entering
partway down — a followup on a shipped ticket, an MR that arrived without a
lifecycle run behind it. `krakoactl run <workflow>:<entry>`.

A verb is only which state a run begins in, plus a `seed` standing in for
states an earlier run already walked. Seeding `filing.ticket_id` is not a
fiction: the ticket really was filed, by the run this one follows — which is
why `thread:`, every `correlate:` and every later `$filing.ticket_id`
resolves from the first step, unchanged.

- **A verb is not a new workflow.** If the states below the entry differ, you
  want a separate workflow. If they are identical, you want a verb — the back
  half of a lifecycle is where the scar tissue lives and it must not be
  copied.
- **A verb is not a new scope.** Different work → the default entry. Same work,
  one more pass → a verb.
- Each entry is another reachability root, so a verb's own entry state is
  legitimately unreachable from `start`. Dry-run walks every entry.
- Seed keys must name real states; the validator rejects a typo instead of
  seeding context nothing reads.

### States

Every state carries at most one step and its transitions. Non-terminal states
need ≥1 transition; every state must be reachable from `start` or from a
declared entry; ≥1 terminal state; state names `input`, `last`, `env` are
reserved.

**agent** — a Claude Code session doing work in the world.

```yaml
  merging:
    step: agent
    agent: code-operator
    class: code-review          # optional; policies.yaml can restrict which agents bind
    instruction: >
      Merge the ticket's MR per your glab-ops skill... Outcomes: merged,
      not-approved, precondition-fail, conflict.
    in: {ticket_id: $filing.ticket_id, trunk: $env.trunk}
    retry: 1                    # attempts before parking as needs-attention
    requires: [aws-sso]
    on: {merged: pipeline-wait, not-approved: approval-stalled, conflict: retriggering}
```

The instruction MUST enumerate the outcomes, and they must exactly match the
`on:` keys — an outcome with no transition parks the run. `in:` values are
templates; they arrive as a JSON `Inputs:` block in the prompt.

**gate** — ask the human. Run parks as `gated`; first response wins.

```yaml
  asking:
    step: gate
    gate: {kind: question, payload: "refine needs answers on $input.repo ($env.name)"}
    on: {answered: refining}
    budgets: {answered: 3}
  dispatch-stalled:
    step: gate
    gate:
      kind: choice
      payload: "dispatch found no builder. retry = list agents and assign again (~$1, ~1m) — recommended; abandon = close unfinished"
      options: [retry, abandon]
    on: {retry: dispatching, abandon: abandoned}
```

- `question` → transitions on `answered`; answers accumulate across rounds.
- `approval` → transitions on the response (`approved`/`rejected`).
- `choice` → requires `options`, transitions on the option word.

Payloads are **decision-ready**: what happened, what each option does, cost and
time estimate, and which one you recommend. The human reads it in Slack with no
other context.

**wait** — park on the world. Arms race; first to fire wins. **A timeout arm is
mandatory** (validator enforces it).

```yaml
  building:
    step: wait
    arms:
      - {event: mr-ready, correlate: $filing.ticket_id}   # external signal, routed by key
      - probe: {command: "scripts/builder-check $filing.ticket_id", every: 5m}
      - probe: {agent: reviewer, instruction: "...", every: 10m}   # judgment; costs money
      - {timeout: 8h}
    on: {mr-ready: merging, builder-dead: builder-stalled, timeout: stalled}
```

Exactly one of `event` / `timeout` / `probe` per arm. Event arms need a
transition named for the event; the timeout arm needs `on: {timeout: ...}`.
Probe outcomes route like any outcome; the reserved outcome `pending` means
"not yet" and just waits for the next cadence. Prefer a **command probe** —
direct exec, no session, structurally $0 — and reach for an agent probe only
when the answer needs judgment.

Arm both orders when a signal can arrive before or after a status flip; a
long-lived wait with a single 8h timeout is how a dead builder looked alive for
eight hours.

**notify** — one line to the human through the gate channels. Instant, $0,
thread-aware. Never spawn an agent to say a sentence.

```yaml
  notifying:
    step: notify
    message: "$filing.ticket_id merged to $env.trunk and rolled out — $merging.mr_url"
    on: {ok: done}
```

**terminal** — `done: {terminal: true}`. No step, no transitions.

### Templates and context

Refs resolve against the run: `$input.<name>` for inputs, `$<state>.<field>`
for a completed state's result, `$env.<field>` for the bound environment,
`$last` for the result that caused the current transition (the only safe source
for a state with several incoming edges, e.g. a question gate fed by two
agents). `$ref?` is optional — unresolvable resolves to null instead of parking
the run (use it for "prior round" inputs on the first pass).

In `in:` maps an unresolvable ref parks the run; inside payload/message strings
it renders verbatim.

`budgets: {outcome: N}` caps traversals of one edge — exceeding it parks the
run as needs-attention. Every loop back to an earlier state needs one.

## Agents

```yaml
# agents/refiner.yaml   (name defaults to the filename)
persona: |
  You are a senior engineer at Callab AI who writes autonomous scrum tickets...
  Follow your refine-headless skill exactly.
skills: [refine-headless, callab-dev]     # must exist in skills/
working_folder: "$input.repo"             # templated, then resolved through workspace repos:
worktree: true                            # fresh detached git worktree per step, removed after
model: haiku                              # optional
effort: low                               # optional
result_schema: '{...}'                    # optional JSON Schema, appended to the prompt
```

The persona becomes the agent's `CLAUDE.md`; the carried skills are copied to
`.claude/skills/`. Agents run with `--setting-sources project` — the user's
global CLAUDE.md, settings and hooks are deliberately NOT loaded. Anything the
step must know goes in the persona, the skill, or the instruction.

**Handoff contract** (appended to every persona by the runner): the agent writes
`$KRAKOA_HANDOFF/result.json` atomically (tmp + rename) with
`{"outcome": "<one of the state's outcomes>", ...fields}`. That whole object
lands in `run.Context[<state>]`, which is what `$<state>.field` reads. An
invalid or missing result resumes the same session once with the error; still
invalid → needs-attention. Skills for expensive steps must say: write
result.json the moment the outcome is known, before any cleanup.

An agent with no `working_folder` is told plainly it has no checkout — don't
ask such an agent to read code.

## Watchers and scripts

```yaml
# watchers/draft-mr-watch.yaml
command: scripts/draft-mr-sweep    # or: agent: <name> + instruction:
every: 60s
mode: spawn                        # spawn a run per observation | resume waiting runs by key
workflow: review-sweeper           # spawn mode only
spawn_on: [mr-draft-pending]       # these events spawn; every other event routes as a correlation resume
```

Script contracts (executable, workspace-relative, JSON on stdout, exit 0):

```jsonc
// watcher sweep
{"outcome": "ok", "events": [{"event": "mr-ready", "key": "CAL-653", "payload": {...}}]}
// wait-arm probe
{"outcome": "builder-dead", "detail": "..."}    // or {"outcome": "pending", ...}
```

Events route by `key` against wait arms' `correlate:` template. Spawn-mode
events are deduped per key. `krakoactl emit <event> --workspace <ws> --key <k>`
injects the same thing by hand for recovery/replay.

## workspace.yaml

```yaml
name: callab
checks:                            # one registry, three consumers: requires:, doctor, the unblocker
  glab:
    command: glab auth status
    fail: [not logged]             # substrings that fail even on exit 0
    fix: glab auth login
  niffty:
    url: http://127.0.0.1:7777/healthz
    fix: "launchctl load ~/Library/LaunchAgents/com.krakoa.niffty.plist"
alerts: [mr-ready, mr-unlinked]    # an event of these names with NO consumer raises a gate instead of vanishing
envs:
  staging: {trunk: staging, ns_suffix: -us-east-2-staging}
repos:
  callab.ai/callab-dashboard: /Users/niemand/Desktop/work/clusterlab/callab-dashboard
```

A check needs exactly one of `command`/`url`. `expect` is a substring that must
appear; `fail` are substrings that fail regardless of exit code (CLIs that exit
0 on dead tokens). `policies.yaml` gatekeepers restrict a `class:` to named
agents — use it where binding the wrong agent is a real risk (code review).

## Design rules

- **Fewest states that model the real work.** No speculative gates, no state
  that only renames an outcome. Every gate is a human interrupt — earn it.
- **Judgment = agent, observation = script.** A command probe is free and never
  hallucinates; use one wherever the answer is mechanical.
- **Failure is a gate, never silence.** Retries exhausted, budget blown, probe
  derailed → needs-attention with `retry`/`abandon`. If you find yourself adding
  a "just log it" path, add an `alerts:` entry instead.
- **Preconditions before spend.** `requires:` a check rather than letting an
  agent burn a session discovering an expired token.
- **Timeouts are honest, not decorative.** A timeout arm that routes to a state
  claiming success is a lie in the thread; route it to a caveat or a gate.
- Set `thread:` on anything with a natural work key, and `unique:` wherever the
  same job can be started twice (manual + watcher, or a verb racing the run
  it follows).
- **Re-entry over duplication.** Work that needs another pass through states
  you already wrote is an `entries:` verb. Copying the back half into a second
  workflow is how the two drift.

## Checklist

- [ ] states minimal, every outcome in the instruction has a transition
- [ ] every wait has a timeout arm; every loop edge has a budget
- [ ] gate payloads decision-ready (options, cost/time, recommendation)
- [ ] agents exist, skills exist, working folders template through `repos:`
- [ ] probe/watcher scripts executable and printing the right JSON shape
- [ ] `requires:` on the states that actually need the world
- [ ] `krakoactl workspace validate` green
- [ ] `krakoactl workspace dry-run` walks every edge green
- [ ] committed in the workspace repo, daemon kickstarted, `krakoactl doctor` clean
