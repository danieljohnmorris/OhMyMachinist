# HTTP API (v1)

The control plane serves a versioned JSON API under `/api/v1`. Grok bots and
other machine clients drive Machinist through it; the web UI uses the same
routes with its same-origin CSRF defense.

All requests and responses are `application/json`. Errors are
`{"error": "message"}` with an appropriate status code.

## Authentication

- **`auth = "tokens"` (default).** Machine clients send
  `Authorization: Bearer <token>`. Tokens carry a scope, `read` or `write`;
  write endpoints reject read-scope tokens with `403`. Tokens are created and
  revoked from the Settings page or the CLI (`machinist tokens create`,
  `machinist tokens revoke`, `machinist tokens list`); the secret is shown
  once and only a SHA-256 hash is stored. Every change made with a token is
  audit-logged with the token's name (`GET /api/v1/audit-log`).
- **`auth = "none"` (opt-out).** No bearer token is required. Only allowed
  when the server binds a loopback or trusted Tailscale interface
  (`100.64.0.0/10`), and the server logs a startup warning. Binds to any
  other interface are rejected at startup.
- Browsers keep the existing same-origin defense: writes require the
  `X-Machinist-CSRF` header from `GET /api/v1/status`, a same-origin `Origin`,
  and browser Fetch Metadata (`Sec-Fetch-Site: same-origin`). Non-browser
  clients cannot write without a token: the CSRF token is served by the open
  status endpoint and `Origin` is self-reported, so Fetch Metadata is what
  separates real browsers from credential-less scripts (which must present a
  bearer token instead). Same-origin browser GETs (such as the token list)
  carry only the `X-Machinist-CSRF` header — browsers omit `Origin` there.
- The shared worker token (from `server.worker_token_file`) still
  authenticates worker endpoints and is accepted on write endpoints as a
  write-capable credential, audit-logged as `worker-token`.

## Endpoints

### Reading state (read or write scope)

| Method | Path | Description |
| --- | --- | --- |
| GET | `/api/v1/jobs?state=queued` | List jobs newest first with their runs. Optional `state` filter. |
| GET | `/api/v1/jobs/{id}` | One job: prompt, state, attempts and runs. |
| GET | `/api/v1/runs/{id}` | One run: command, executor, state, exit code, timings, final message, and the `skills` it loaded. |
| GET | `/api/v1/runs/{id}/events` | The run's transcript (event log). |
| GET | `/api/v1/jobs/{id}/artifacts` | Artifact files for a job. |
| GET | `/api/v1/artifacts/{id}/content` | Artifact content. |
| GET | `/api/v1/settings` | All stored settings key/value pairs. |
| GET | `/api/v1/factory` | Factory controls: dispatch state (running, paused, draining), the effective job cap and one slot per lane. |
| GET | `/api/v1/audit-log?limit=100` | Recent audit entries, newest first. |
| GET | `/api/v1/review-profiles` | Stored review profiles. |
| GET | `/api/v1/review-profile?project=X` | Resolved review profile for a project. |
| GET | `/api/v1/review-default-profile` | The global default profile name. |
| GET | `/api/v1/skills` | Installed skills with their versions and `used_by` references. |
| GET | `/api/v1/skills/{name}` | One installed skill with every version. |
| GET | `/api/v1/plane/intake` | Plane intake settings, shadow-mode decisions and imported script jobs. |
| GET | `/api/v1/trackers` | Default tracker and configured status for built-in, Plane and Linear. |
| GET | `/api/v1/import/preview?source=plane\|linear&project=NAME` | Bulk import plan. Without `project`: the source's remote projects (`projects`, each with `repository` when a configured `[issue_sources.*]` entry covers it). With `project`: the tickets that would be created (`key`, `title`, mapped `state`, `action` of `create`/`skip` and `reason` of `exists`/`completed`). Writes nothing. |
| GET | `/api/v1/tickets` | Built-in tickets, newest first, as `{"tickets": [...]}`. |
| GET | `/api/v1/tickets/{key}` | One built-in ticket with comments and its factory `job_id`. |

`GET /api/v1/status` (board state: jobs, workers, triggers, plus the CSRF
token) and `GET /api/v1/catalog` and `GET /api/v1/definitions` remain open so
the browser UI can bootstrap; they serve no secrets.

The run transcript is JSON Lines. `process.output` carries base64 command
output; coding agents that run in a JSON mode contribute typed agent events the
run page renders readably: `agent.message` (assistant text, markdown),
`agent.thinking` (a thinking summary, dimmed), `agent.tool_call` (`tool` name
and `tool_args` JSON) and `agent.tool_result` (`tool`, `message` truncated to
2 KB, `bytes` for the full size, `failed` when the tool reported an error).
File edits become `agent.file_edit` (`path`, `operation` of edit/write/create/
delete, `additions` and `deletions`, plus `tool_args` and a short result) and
subagents become `agent.subagent_start` (`subagent` name and a truncated task
summary) and `agent.subagent_end` (`status` and a short result).
Tool-call, tool-result and file-edit payloads are redacted for credential shapes
before they reach the log.

Each finished run also publishes a raw agent log artifact (`agent-output.log`)
under its run directory: the complete agent stdout with the same redaction,
capped at a configurable size and marked `[raw agent log truncated at N MB]`
when the cap is reached. It is downloadable from the run page Artifacts list for
every attempt, including failed, timed-out and cancelled runs.

Built-in ticket responses carry `key`, `title`, `description`, `repository`,
`state`, timestamps, optional `job_id`, and `comments`. The `approved` and
`in_progress` states cannot be assigned by PATCH: use approval and worker
dispatch. After approval, the repository cannot change and the ticket cannot
return to backlog/todo. `done` resolves linked terminal jobs. Invalid input
returns 400; unknown keys return 404; conflicting changes return 409.

### Changing state (write scope)

| Method | Path | Description |
| --- | --- | --- |
| POST | `/api/v1/jobs` | Queue a job. Body: `repository` plus either `command` or `workflow`; optional `title`, `spec`, `source_url`, `prompt`, `model`, `project`. Returns `{"id": "job_…"}`. |
| POST | `/api/v1/jobs/{id}/{action}` | Workflow actions: `cancel`, `retry` (requeue), `approve`, `request_changes`. Body may carry `run_id`, `feedback`, `previous_process_stopped`. |
| POST | `/api/v1/jobs/{id}/move-to-queue` | Stop a running or stalled job's current attempt and requeue it as the next attempt on the same card. Returns `{"job_id": "job_…"}` (the new attempt id); a repeated call returns the same id. The attempt is cancelled with the reason `moved to queue`, the lane's uncommitted work (tracked and untracked) is saved by the worker that held the lease and recorded on the cancelled attempt, and the new attempt carries the prior transcript and waits at the back of the queue. |
| DELETE | `/api/v1/jobs/{id}` | Delete a terminal job. |
| POST | `/api/v1/tickets` | Create a built-in draft. Body: `title`, optional `description`, `repository`, `project` (a configured `[tracker.projects]` name; unknown projects return 400). |
| POST | `/api/v1/import` | One-shot import of a configured Plane or Linear project into the built-in tickets table. Body: `source` (`plane`\|`linear`), `project` (remote project name), optional `dry_run` (plans without writing). Keys are the external ticket keys (ENG-123, MACH-5); completed or canceled states are skipped, started maps to `in_progress`, everything else to `backlog`. Idempotent: existing keys are skipped. Returns `created`/`skipped` counts and the per-ticket plan. |
| PATCH | `/api/v1/tickets/{key}` | Edit `title`, `description`, `repository`, or planning/completion `state`. `project` is fixed at creation. |
| DELETE | `/api/v1/tickets/{key}` | Delete an unapproved ticket; linked tickets return 409. |
| DELETE | `/api/v1/workers/{instance_id}` | Forget a registered worker. Unknown workers return 404; a worker running a job returns 409. |
| POST | `/api/v1/tickets/{key}/approve` | Atomically approve and queue work. Returns `{ticket, job_id, created}`; repeated approval returns the existing job. |
| POST | `/api/v1/tickets/{key}/comments` | Add a comment. Body: `{"body": "…"}`. Returns the ticket. |
| PUT | `/api/v1/settings/{key}` | Write one setting. Body: `{"value": "…"}`. The reserved `factory.*` keys are refused; use the factory endpoints below. |
| POST | `/api/v1/factory/stop` | Stop dispatching new jobs. Body: `{"mode": "drain"|"cancel", "reason": "…"}`. `drain` lets the running job finish; `cancel` stops it now. Workers stay connected. |
| POST | `/api/v1/factory/start` | Resume dispatch. |
| PUT | `/api/v1/factory/max-concurrent-jobs` | Set how many jobs run at once. Body: `{"value": 2}`. Clamped to the enabled connected lanes. |
| PUT | `/api/v1/factory/lanes/{name}` | Enable or disable one lane. Body: `{"enabled": false}`. A disabled lane stays connected but gets no new jobs. |
| GET | `/api/v1/tokens` | List tokens (write scope; browsers read with the CSRF header). |
| POST | `/api/v1/tokens` | Create a token. Body: `{"name": "…", "scope": "read"|"write"}`. Response carries the one-time `secret`. |
| DELETE | `/api/v1/tokens/{id}` | Revoke a token. |
| PUT | `/api/v1/review-profiles` | Save a library review profile. |
| PUT | `/api/v1/review-profile` | Save a project review profile. |
| PUT | `/api/v1/review-default-profile` | Set the global default profile name. |
| POST | `/api/v1/skills` | Install a skill. Body: `{"source": "<github url, directory or zip>"}`. Returns the install result. |
| POST | `/api/v1/skills/upload` | Install an uploaded skill. Multipart form with one `file` part holding a `.zip` archive or a bare `SKILL.md`. |
| POST | `/api/v1/skills/{name}/update` | Re-resolve the recorded source and add the new version. Body: `{"activate": true}` (optional). |
| POST | `/api/v1/skills/{name}/pin` | Set the active version. Body: `{"version": "…"}`. Returns the updated skill. |
| DELETE | `/api/v1/skills/{name}` | Remove a skill and every installed version. Add `?force=true` to remove a skill a shipped review profile still references. |

### Factory controls (write scope to change)

One panel's worth of operations, also exposed as MCP tools and the CLI
(`machinist pause`, `machinist resume`, `machinist job-cap`, `machinist lane`).
Every change is written to the audit log with who, when, the old and new value
and the reason.

- **Start/Stop.** `POST /api/v1/factory/stop` stops dispatching new jobs;
  `mode` chooses `drain` (the running job finishes) or `cancel` (the running job
  is stopped now). Workers stay connected while stopped, so `POST
  /api/v1/factory/start` resumes without reconnecting anything. The stopped
  state (with reason, who stopped it and when) is persisted, so it survives a
  control-plane restart.
- **Jobs at once.** `PUT /api/v1/factory/max-concurrent-jobs` applies live,
  without a restart, and clamps to the number of ready (connected and enabled)
  lanes.
- **Lanes.** `PUT /api/v1/factory/lanes/{name}` enables or disables one lane by
  worker name. A disabled lane stays connected but receives no new jobs.

`GET /api/v1/factory` reports the state, the effective cap, the ready/enabled
lane counts and one slot per lane (`busy` with its job, `idle`, `offline` or
`disabled`, plus the lane's repository checkouts). The same object is served as
`factory` on the open `GET /api/v1/status` so the web UI can render it without a
token. `server.max_concurrent_jobs` is the default cap used until the setting is
changed.

### Worker endpoints (worker token)

`POST /api/v1/workers/poll`, `POST /api/v1/runs/{id}/heartbeat`,
`POST /api/v1/runs/{id}/revoked`, `POST /api/v1/runs/{id}/complete`,
`POST /api/v1/runs/{id}/events`, `PUT /api/v1/runs/{id}/artifacts`. These are
the internal worker protocol and are not part of the documented bot API.
`POST /api/v1/runs/{id}/revoked` is how a worker reports where it saved the
lane's uncommitted work after its lease was revoked by a move to queue
(MACH-65); the location is recorded on the cancelled attempt.

## Example

```console
$ machinist tokens create --name grok-bot --scope write
machinist: created token token_6f21… (grok-bot, scope write); store the secret now, it is not shown again
mmt_9f2c…

$ curl -s -H "Authorization: Bearer mmt_9f2c…" http://127.0.0.1:7331/api/v1/jobs | jq '.jobs[0].id'
"job_27e06953ceeaf3f6"

$ curl -s -X POST -H "Authorization: Bearer mmt_9f2c…" \
    -H "Content-Type: application/json" \
    -d '{"repository":"my-project","workflow":"task-to-pr","title":"Fix login","spec":"…"}' \
    http://127.0.0.1:7331/api/v1/jobs
{"id":"job_6f21b0d0c5b7"}
```

## Configuration

```toml
[server]
listen = "127.0.0.1:7331"
database = "~/.machinist/server/machinist.db"
worker_token_file = "~/.machinist/server/worker.token"
# lease_grace = "3m"
# auth = "tokens"   # default; "none" opts out on loopback/Tailscale only
```

### Restart grace period

After a control-plane restart, a run lease that lapsed while the server was
down is kept for `server.lease_grace` (default 3m), measured from the server
start rather than the last heartbeat before the outage. While the grace
window is open the run is not redelivered to another lane: when the worker's
next heartbeat or poll arrives with the matching lease token, it re-attaches
and carries on, and a completion for the same lease is still accepted. Only
after the grace period with no heartbeat does the run go back to the queue
and a fresh lease (a restart also logs re-attaches and redeliveries). The
workflow no-replay rule is unchanged: an expired workflow job becomes
`interrupted`, not queued, and still needs `previous_process_stopped`
confirmation before a retry. Two to five minutes is a sensible range — a few
heartbeat intervals. Workers that really died during normal operation are
redelivered as before; the grace only covers the window right after a
restart.

Tokens live in the server database (outside any repository) and are never
written to config files or the repository. Keep them in your bot's
environment.

## MCP

The same API is exposed as MCP tools over streamable HTTP at `/mcp`; see
[docs/mcp.md](mcp.md).

## Skills

The skills endpoints manage the versioned skills library on disk
(`<data>/skills`, see the CLI `machinist skills`). An install resolves a GitHub
`SKILL.md`/directory URL, a local directory, or a zip archive, validates its
frontmatter and scans it for secrets before the version becomes active.

A skill entry looks like:

```json
{
  "name": "feature-dev:code-reviewer",
  "source": "https://github.com/owner/repo/tree/main/skills/reviewer",
  "active": "0123456789abcdef0123456789abcdef01234567",
  "versions": [
    {
      "version": "0123456789abcdef0123456789abcdef01234567",
      "resolved": "0123456789abcdef0123456789abcdef01234567",
      "installed_at": "2025-01-01T00:00:00Z",
      "scanned": true
    }
  ],
  "used_by": []
}
```

`used_by` lists the projects and review profiles that reference the skill: each
`skills.project.<project>` and `skills.default` setting that names it, plus the
shipped and stored review profiles whose reviewers reference it. It is always
present, and empty when nothing references the skill. Recorded sources and
manifests never contain credentials, so
no response returns a token or a token-bearing URL. Every install, update, pin
and removal is audit-logged with the token's name.

A removal is refused with `409` while a shipped review profile still references
the skill; pass `?force=true` to remove it anyway. The CLI `--force` flag and the
MCP `force` argument carry the same override.

A skill installed through `/api/v1/skills/upload` records its source as the
sentinel `upload:<name>` rather than the staging path, which the server deletes
once the version is written. Updating such a skill fails with `400` and
`skill "…" was installed from an upload; re-upload it to add a version`; upload
the archive or `SKILL.md` again to add a version and activate it. Re-uploading
content that matches an installed version adds no new version and makes that
version active.
