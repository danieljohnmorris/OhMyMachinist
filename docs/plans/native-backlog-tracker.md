# Native lightweight tracker ("Backlog") — implementation plan

Status: **draft for approval** · 2026-10-13 · branch `feat/native-backlog`
Evidence base: source inspection of `main` @ `f22655d` (MACH-32) and `makeplane/plane` (AGPL checkout), 2026-10-13. Findings below were verified against source, not inferred, unless marked `[INFERENCE]`.

## Objective

Give OhMyMachinist a built-in, lightweight issue tracker so a repo can be driven by:

1. the **native backlog** (included, zero external services), or
2. **Plane**, or
3. **Linear**

per repository, with provider-neutral identity so completion reconciliation, webhooks and UI links stop being Plane-only.

## Current state (verified)

| Fact | Where |
|---|---|
| Adapter seam exists: `Source` (FetchIssue/PostComment) + optional `StateSetter`/`LabelPoller` | `internal/issues/issues.go` |
| Plane REST client: fetch, poll, comment, state read/write, 429 retry | `internal/issues/plane.go`, `plane_keys.go` |
| Linear GraphQL client: fetch, poll, comment, state write — but `first:100` without pagination, no GraphQL `errors` handling, no state read, mutation success unchecked | `internal/issues/linear.go` |
| No persisted issue entity: tasks are `task_inputs(job_id, …)`; submitting a task authorizes execution | `internal/controlplane/store.go`, `task_inputs.go` |
| Completion reconciliation hardcodes `"plane"` kinds | `internal/controlplane/resolution.go:169-171,251` |
| Webhook handler only resolves on Plane completed state, records kind `plane` | `internal/controlplane/resolution.go` (POST /api/v1/webhooks/issues) |
| `planeClientFor` falls back to the sole Plane client regardless of job origin — `[INFERENCE]` wrong-tracker lookup possible in mixed setups | `internal/controlplane/server.go` |
| UI ticket links hardcode Plane host/workspace/prefixes | `internal/controlplane/web/src/run-links.js` |
| Artifacts, attempt history, workflow snapshots, SQLite v8 store, embedded React UI all present and reusable | `internal/artifacts/`, `internal/controlplane/` |

Related unmerged branches (coordination risk, see Risks): `factory/MACH-8-tracker-agnostic-issue-links…` (UI link tracking, 1 commit), `ui/linear-redesign` (56-file UI overhaul).

## Design

### 1. Data model — SQLite schema v9

Additive only (v8 → v9 migration must work on existing databases):

```sql
CREATE TABLE backlog_issues (
  id         TEXT PRIMARY KEY,          -- uuid
  project    TEXT NOT NULL,             -- repository/project key
  key        TEXT NOT NULL,             -- human key, <PROJECT>-<n>
  title      TEXT NOT NULL,
  spec       TEXT NOT NULL DEFAULT '',
  state      TEXT NOT NULL DEFAULT 'backlog',
  revision   INTEGER NOT NULL DEFAULT 1,-- bumped on every edit
  url        TEXT NOT NULL DEFAULT '',  -- optional external URL
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_backlog_issues_project_key ON backlog_issues(project, key);
CREATE INDEX idx_backlog_issues_project_state ON backlog_issues(project, state);

-- job_metadata gains:
--   source_kind      TEXT NOT NULL DEFAULT ''   -- 'plane' | 'linear' | 'backlog' | ''
--   backlog_issue_id TEXT                        -- links a started job to its native issue
```

Key allocation: per-project sequence `MAX(CAST(suffix AS INTEGER)) + 1` inside the create transaction (single-writer modernc SQLite, single connection — no new contention).

### 2. Provider identity

- `trigger_scheduler.admitIssue` stamps `source_kind` from the trigger's provider on every admitted job (plane/linear).
- `planeClientFor` (and webhook resolution) only use a Plane client when the job's `source_kind == "plane"` — removes the wrong-tracker fallback.
- No `native://` source URL is invented for native issues (protocol requires HTTP(S)); linkage is `backlog_issue_id` + display key.
- Board grouping gains the persisted identity instead of title-regex inference where a kind/id exists.

### 3. Native tracker — `internal/backlog` (new)

Entity: `Issue{ID, Project, Key, Title, Spec, State, Revision, URL, CreatedAt, UpdatedAt}`.

States: `backlog | todo | started | completed | cancelled` (Plane's semantic groups minus triage; completion ≠ cancellation).

Store methods: `CreateBacklogIssue`, `GetBacklogIssue`, `ListBacklogIssues(project, state, q)`, `UpdateBacklogIssue` (revision++), plus widened `MarkJobResolved` / `ResolveTicketsForIssue` kind validation (`pr`, `plane`, `linear`, `backlog`).

### 4. HTTP API

| Route | Behavior |
|---|---|
| `GET /api/v1/backlog?project=&state=&q=` | list issues |
| `POST /api/v1/backlog` `{project, title, spec?, url?}` | create; key auto-assigned; **no job enqueued** |
| `GET /api/v1/backlog/{id}` | issue detail |
| `PATCH /api/v1/backlog/{id}` `{title?, spec?, state?}` | edit; `revision++` |
| `POST /api/v1/backlog/{id}/start` `{repository, workflow?, command?, model?}` | creates a workflow/command job with the issue's **current spec pinned**; stamps `backlog_issue_id`; returns `{job_id}` |

Issue JSON: `{id, key, project, title, spec, state, revision, url, created_at, updated_at}`.

### 5. Product decisions (locked for this iteration)

1. **Backlog is always present** (built-in); external trackers remain opt-in per repo via existing `[issue_sources.*]` config. No new required config.
2. **Saving never enqueues.** Editing is separate from execution. `start` is the only path from issue → job.
3. **Start pins a spec snapshot.** Later issue edits never rewrite queued/running jobs from that issue. (Deliberately different from external-provider polling, where remote content is authoritative.)
4. **First successful start moves state → `started` (once); afterwards state is human-controlled.** PR merge records a resolution on the job (kind `backlog`) but never changes issue state.
5. **No auto-comments/delivery for native issues.** Provider delivery (comment/state) stays Plane/Linear-only.
6. **Linear reconciliation gets wired**: merged PR for a Linear-sourced job → `issueUpdate` state + comment, kind `linear` recorded. Resolved state name per repo: `[issue_sources.linear.<repo>] resolved_state` (default `"Done"`).
7. **No Plane code is copied.** Plane is AGPL-3.0-only; the repo is MIT. Domain concepts are reimplemented from the verified model. New files are original.

### 6. Linear client hardening (`internal/issues/linear.go`)

- `request()`: check HTTP status **and** GraphQL `errors` array; verify mutation `success`/entity presence.
- `IssuesWithLabel`: paginate via `pageInfo { hasNextPage, endCursor }`.
- Add `IssueState(ctx, key)` (state read) for reconciliation verification.
- Table tests against `httptest` fixtures.

### 7. UI (embedded React)

- New `Backlog` page: columns by state, composer, detail drawer (edit + Start-run dialog), project filter. Additive files only.
- `run-links.js`: prefer stored `source_kind`/identity; fall back to existing inference. Absorb the intent of unmerged `factory/MACH-8-tracker-agnostic-issue-links…`.
- Minimal edits to shared files (nav entry) to reduce conflict with `ui/linear-redesign`.

### 8. Docs

- README "What it adds": native backlog bullet + tracker choice.
- `docs/configuration.md`: backlog section + `resolved_state`.
- This plan file stays in `docs/plans/`.

## Phases

| Phase | Scope | Files | Assignee |
|---|---|---|---|
| P0 | Branch, baseline green | — | me |
| P1 | Schema v9, `internal/backlog`, store, API, start-run, `source_kind`, planeClientFor guard | `internal/controlplane/{store,task_inputs,server,trigger_scheduler,metadata}.go`, `internal/backlog/*` | agent (backend-core) |
| P2 | Linear hardening, resolution generalization, webhook kinds | `internal/issues/linear.go`, `internal/controlplane/{resolution,server}.go` | me (depends on P1 store kinds) |
| P3 | Backlog UI + run-links identity | `internal/controlplane/web/src/*` | agent (web), against the API contract above |
| P4 | Integration verification | — | me |
| P5 | Docs, commit, PR | README, docs/ | me |

P1 and P3 run concurrently (disjoint files); P2 follows P1.

## Verification & acceptance

Baseline: `just build`, `go test -race ./...`, web `npm test && npm run build` green on `main` before changes.

1. `POST /api/v1/backlog` creates an issue; `jobs` table unchanged (no enqueue).
2. `PATCH` bumps revision; a job already started from that issue keeps its pinned spec.
3. `start` creates a workflow job with the pinned spec and `backlog_issue_id` set; card groups by issue key on the runs board.
4. Native: merged PR → job resolved kind `backlog`; issue state unchanged.
5. Linear: fixture server test proves merged-PR path calls `issueUpdate` to `resolved_state` + comment, kind `linear` recorded.
6. Mixed Plane+Linear repos: a job from one provider can never be resolved against the other (source-kind guard, tested).
7. Migration from a v8 database opens and migrates cleanly.
8. Browser smoke: create → edit → start → board shows the job; backlog editor saves without enqueueing.
9. `just check` green (go vet, gofmt, go test -race, web tests/build).

## Risks

| Risk | Mitigation |
|---|---|
| `ui/linear-redesign` (56 files) conflicts with P3 | Keep UI additive; land nav entry only; rebase/port if it merges first. Confirm merge intent before P3. |
| `factory/MACH-8…` overlaps run-links | Absorb its intent; check it is not merged mid-flight. |
| v8→v9 migration breaks existing installs | Additive DDL only; migration test from v8 fixture. |
| Linear paths unverifiable live (no API key in this session) | `httptest` fixture tests; state limits honestly. |
| AGPL contamination | No copied code; original implementation; PR review notes provenance. |
| Scope creep toward a Plane clone | Out-of-scope list below is binding for this iteration. |

## Out of scope (this iteration)

Cycles/modules/pages, backlog attachments (artifacts stay run-owned), editing Plane/Linear issue bodies from OMM, multi-user auth, addon-repo extraction, spec revision history UI, buckets/storage backends.

## Goal

Ship the built-in native backlog so OhMyMachinist runs with **no external tracker**, while Plane and Linear remain fully supported per-repo choices with provider-neutral reconciliation — verified end-to-end per the acceptance list.
