# Configurable workflow stages

A project defines one or more **job types**, and each job type is an ordered
list of **stages**. A card picks its job type from a Plane label (e.g.
`type:config`) or the project default, and the board shows a stage strip for
that type: completed stages ticked, the current stage highlighted, and the
built-in review round when it is running.

```
Plan ✓ · Code ✓ · Review (round 2) · UAT
```

The control plane derives the strip from the job state, the stage the executor
last reported and the ticket/PR resolution. Configuration lives in the database
and is edited from **Settings → Workflow stages**, the HTTP API or MCP; the same
values are readable and writable everywhere (writes need a write-scope token,
are validated and are audit-logged).

## Shipped templates

A project that has not saved its own config uses two templates.

| Job type | Stages |
| --- | --- |
| `web_pr` | Queued, Planning, Coding, PR review, UAT, Finished |
| `config_ops` | Queued, Planning, Apply, Verify, Approve, Finished — no pull request |

The default label mapping routes `type:config` to `config_ops`. The default job
type is `web_pr`.

## Stage fields

Each stage carries:

- `id` and `name` — the label shown in the strip.
- `kind` — one of `queued`, `planning`, `coding`, `review`, `uat`, `apply`,
  `verify`, `approve`, `finished`. The kind drives derivation; the id and name
  are free-form.
- `agent` and `model` — the executor and model for that stage. Empty means the
  project default.
- `session` — `continue` (same agent session as the previous stage) or `fresh`
  (an independent one). `review` defaults to `fresh`; everything else to
  `continue`.
- `human_approval` — the stage waits for a person.
- `review_profile` — the built-in review profile for a `review` stage.

## Reading and writing

- UI: **Settings → Workflow stages**.
- HTTP: `GET/PUT /api/v1/workflows?project=X`, `GET/PUT/DELETE
  /api/v1/job-types?project=X` — see [the HTTP API](api.md).
- MCP: `workflows_get`, `workflows_put`, `job_types_list`, `job_types_put`,
  `job_types_delete` — see [the MCP server](mcp.md).

A job records the job type resolved at intake, so changing a project's config
does not move existing cards.

## Stage resolution

- **Queued** — the job is waiting for a worker.
- **Planning / Coding / Apply / Verify** — the executor reports the stage it
  reached through its step result (`stage`, `review_round`) or the card follows
  the job state.
- **PR review** — the pull request is open and the built-in review is running
  or blocked; the strip shows the round. A review that exhausts its budget
  needs attention.
- **UAT / Approve** — review passed and a human check is pending.
- **Finished** — only once the PR merges or the Plane ticket is Done. A
  succeeded pull-request job waits in UAT, not Finished.
- **Needs attention** — blocked, failed, timed out, interrupted, or awaiting
  input. The stage stays visible next to the alert.