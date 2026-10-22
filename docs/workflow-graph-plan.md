# Plan: workflow graphs for OhMyMachinist

Adapting the Gazpacho workflow map (`Gazpacho Workflow Map.md`, v1.2: 11
workflows, 107 steps, 13 actors, labeled edges, sub-workflow invocations,
retry-exhaustion edges, policies, joins) onto Machinist's execution model.

Machinist today runs a workflow as an ordered list of commands (`docs/workflows.md`):
1–32 steps, `approval = "before"`, a step-result contract with
`outcome: complete | blocked | failed`, artifact snapshots, retries and
interrupt recovery. There is no branching, no sub-workflow invocation, no
join, and retries are operator-driven.

The Gazpacho idea is Kubernetes/CDK for software factories: a declarative
spec, a compiler that validates it, and a reconciler that advances one run
through a graph. That is a better fit than the current list, and most of the
runtime (leases, artifacts, approvals, lanes, triggers) already exists.

## Concept mapping

| Gazpacho | Machinist today | Needed |
| --- | --- | --- |
| workflow | `[workflows.NAME] steps = [...]` | graph form: `nodes`, `edges`, `retry`, `policy`, `scope` |
| step | command + `approval` | node: `command` or `workflow`, `retry`, `policy`, `inputs` |
| actor | executor + prompt | `actor` label routing to a command template; metadata, not a runtime |
| labeled edge (`approved`, `rework`) | `outcome` (3 values) | step returns `labels`; edges match labels |
| double-edged box | none | child job: `parent_id`, await terminal label |
| dotted retry-exhaustion edge | manual Retry | `max_attempts` + `on_exhausted` edge |
| policy `interrupt` | approval gate + request changes | gate with question, answer recorded |
| policy `contract_change` | none | typed gate requiring approval + artifact |
| policy `persistence` | artifact retention | per-workflow retention scope |
| `spawn_lanes` / `fire_join` | lanes exist, no join | join node over child jobs |
| triggers | github/interval/cron | label events (`lane_complete`, `ticket_blocked`) |
| scope `project` | repository | project scope on the job |

CDK/Kubernetes framing we adopt:

- **Spec → synth → apply.** `machinist workflow validate|synth|apply` compiles
  the YAML/TOML into a canonical graph IR and refuses an invalid graph before
  anything runs. Submitted jobs pin an immutable definition hash.
- **Reconciler, not orchestrator code.** One function computes the next node
  from job state; no per-workflow code paths.
- **Status conditions.** Job status carries `Ready`, `Progressing`, `Blocked`,
  `AwaitingApproval` with reasons, instead of ad-hoc flags.
- **Pipelines as code.** `.machinist/workflows.yaml` in the repository is the
  source of truth; the control plane compiles and applies it at submit time.

## Phases

### P0 — Spec, IR, validator (no behavior change)

- `internal/config/workflows.go`: accept `[[workflows.NAME.nodes]]` with
  `id`, `command` | `workflow`, `actor`, `retry {max_attempts, on_exhausted}`,
  `policy`, `inputs`, `scope`; keep `steps` and compile it to a linear chain.
- New `internal/workflowgraph`: IR types, canonical JSON, SHA-256, validation:
  reachable nodes, exactly one start, terminal nodes declared, every label has
  an edge, `on_exhausted` resolves, cycles only through explicit retry/loop
  nodes, max 256 nodes, sub-workflow depth ≤ 4, no orphan edges.
- Store the compiled graph with the job (new column + migration); the job page
  reads it from the snapshot, never from live config.
- CLI: `machinist workflow validate FILE`, `synth FILE` (prints IR JSON).
- Tests: linear config compiles identically to today; invalid graphs produce
  precise line-addressed errors; the Gazpacho library YAML validates.

### P1 — Labeled transitions inside one job

- `protocol.StepResult`: optional `labels []string`; `complete` remains valid
  without labels.
- Reconciler: pick the edge whose label is in the step result; no match and no
  fallback edge ⇒ `blocked` with the reported labels.
- UI: graph view on the run page (nodes, edges, current node, attempt badges).
- Tests: branch on labels, unknown label blocks, linear chain unchanged.

### P2 — Retries and exhaustion

- Per-node `max_attempts`; each attempt is a new run (existing history model).
- On exhaustion follow `on_exhausted` (consult/escalate nodes); if absent, fail.
- Tests: retry budget, exhaustion edge, exhaustion without edge.

### P3 — Sub-workflow invocation

- Node kind `workflow`: spawn a child job with `parent_id` and the node's
  inputs; parent waits on the child's terminal label.
- Idempotent spawn keyed by `(job, node, attempt)`; no duplicate children on
  reconciler replays.
- Depth limit and cycle detection at compile time; runtime guard too.
- Tests: child spawn, terminal-label mapping, cancel propagation, restart
  recovery.

### P4 — Joins and lane spawning

- `spawn_lanes` node expands to N child jobs (one per lane/ticket).
- `join` node waits on children with `accept: all|any`, reads their labels,
  emits one label (`accepted|adjusted|escalated`).
- Interacts with capacity/fairness (issue #556): joins must not deadlock a lane.
- Tests: all/any, one child blocked, lane capacity exhaustion, no orphan children.

### P5 — Policies and human gates

- `interrupt`: pause node, surface the question, resume with an answer recorded
  as an artifact and in the attempt history.
- `contract_change`: approval gate that requires a contract artifact before the
  node proceeds.
- `persistence`: artifact retention scope per workflow.
- Tests: pause/resume, stale gate rejection, artifact requirement.

### P6 — Label events and triggers

- Internal event bus emits node/child labels; triggers can select a label and a
  project scope, not only cron/interval/GitHub.
- Tests: `lane_complete` triggers `monitor`, `ticket_blocked` triggers
  `decide_blocked`, no duplicate firing.

### P7 — Pipelines as code

- Load `.machinist/workflows.yaml` at submit; `machinist apply` validates and
  records the applied revision; drift between repo and control plane is reported.
- Tests: apply, reject invalid, revision pinning.

## Compatibility and risks

- **Workers:** new result fields are optional, so old workers stay valid; graph
  jobs still require a worker that advertises graph support, as workflow jobs
  already gate on capability.
- **Existing workflows:** `steps = [...]` must keep working unchanged; the
  linear compiler is the compatibility path.
- **Exactly-once:** Machinist already refuses to promise exactly-once external
  actions on recovery. Graph execution inherits that; child spawn is the only
  new idempotency requirement.
- **Joins vs lanes:** a join that waits on children must not hold a lane; the
  coordinator node runs on a control-plane-side lease, not a worker lane.
- **Validation surface:** a graph language invites complexity. Keep the node
  set closed (command, workflow, gate, join, spawn) and reject anything else.

## Non-goals

- Full BPMN, arbitrary code in the spec, dynamic graph construction at runtime.
- Multi-tenant isolation.
- Automatic merge or deployment decisions; human gates stay explicit.

## Done when

The Gazpacho library (11 workflows, 107 steps) compiles to IR, validates, and
runs end to end: a greenfield bootstrap job reaches `done`, a feature delivery
job reaches `accepted_exit`, an increment spawns lanes and joins, and an
exhausted engineering loop lands on `cohort_consult` — with the graph visible in
the UI and the definition pinned per job.
