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
| GET | `/api/v1/runs/{id}` | One run: command, executor, state, exit code, timings, final message. |
| GET | `/api/v1/runs/{id}/events` | The run's transcript (event log). |
| GET | `/api/v1/jobs/{id}/artifacts` | Artifact files for a job. |
| GET | `/api/v1/artifacts/{id}/content` | Artifact content. |
| GET | `/api/v1/settings` | All stored settings key/value pairs. |
| GET | `/api/v1/audit-log?limit=100` | Recent audit entries, newest first. |
| GET | `/api/v1/review-profiles` | Stored review profiles. |
| GET | `/api/v1/review-profile?project=X` | Resolved review profile for a project. |
| GET | `/api/v1/review-default-profile` | The global default profile name. |
| GET | `/api/v1/workflows?project=X` | A project's workflow config: `default_type`, `label_mappings`, and job types with their ordered stages. Includes `default: true` when the project has not saved its own. |
| GET | `/api/v1/job-types?project=X` | A project's job types and default type only. |

`GET /api/v1/jobs` and `GET /api/v1/jobs/{id}` attach a derived `stage` strip
(`{job_type, current, round, attention, stages:[{id,name,state,round}]}`) and a
`job_type` to every job, computed from the job state, the stage the executor
last reported and the ticket/PR resolution. A succeeded pull-request job waits
in UAT; it only reaches the finished stage when the PR merges or the Plane
ticket is Done.

`GET /api/v1/status` (board state: jobs, workers, triggers, plus the CSRF
token) and `GET /api/v1/catalog` and `GET /api/v1/definitions` remain open so
the browser UI can bootstrap; they serve no secrets.

The run transcript is JSON Lines. `process.output` carries base64 command
output; coding agents that run in a JSON mode contribute typed agent events the
run page renders readably: `agent.message` (assistant text, markdown),
`agent.thinking` (a thinking summary, dimmed), `agent.tool_call` (`tool` name
and `tool_args` JSON) and `agent.tool_result` (`tool`, `message` truncated to
2 KB, `bytes` for the full size, `failed` when the tool reported an error).
Tool-call and tool-result payloads are redacted for credential shapes before
they reach the log.

### Changing state (write scope)

| Method | Path | Description |
| --- | --- | --- |
| POST | `/api/v1/jobs` | Queue a job. Body: `repository` plus either `command` or `workflow`; optional `title`, `spec`, `source_url`, `prompt`, `model`, `project`. Returns `{"id": "job_…"}`. |
| POST | `/api/v1/jobs/{id}/{action}` | Workflow actions: `cancel`, `retry` (requeue), `approve`, `request_changes`. Body may carry `run_id`, `feedback`, `previous_process_stopped`. |
| DELETE | `/api/v1/jobs/{id}` | Delete a terminal job. |
| PUT | `/api/v1/settings/{key}` | Write one setting. Body: `{"value": "…"}`. |
| GET | `/api/v1/tokens` | List tokens (write scope; browsers read with the CSRF header). |
| POST | `/api/v1/tokens` | Create a token. Body: `{"name": "…", "scope": "read"|"write"}`. Response carries the one-time `secret`. |
| DELETE | `/api/v1/tokens/{id}` | Revoke a token. |
| PUT | `/api/v1/review-profiles` | Save a library review profile. |
| PUT | `/api/v1/review-profile` | Save a project review profile. |
| PUT | `/api/v1/review-default-profile` | Set the global default profile name. |
| PUT | `/api/v1/workflows?project=X` | Replace a project's workflow config. Body is the config object (`default_type`, `label_mappings`, `types`). Validated before saving and audit-logged as `workflows.write`. |
| PUT | `/api/v1/job-types?project=X` | Add or replace one job type. Body: `{"name": "…", "stages": [...]}`. |
| DELETE | `/api/v1/job-types?project=X&name=Y` | Delete one job type (the last type cannot be removed). |

A stage is `{"id","name","kind","agent","model","session","human_approval","review_profile"}`.
`kind` is one of `queued`, `planning`, `coding`, `review`, `uat`, `apply`,
`verify`, `approve`, `finished`; `session` is `continue` (same agent session)
or `fresh` (an independent one). Shipped templates: `web_pr` (Queued, Planning,
Coding, PR review, UAT, Finished) and `config_ops` (Queued, Planning, Apply,
Verify, Approve, Finished, no pull request), with `type:config → config_ops`
mapped by default. `POST /api/v1/jobs` accepts an optional `job_type`.

### Worker endpoints (worker token)

`POST /api/v1/workers/poll`, `POST /api/v1/runs/{id}/heartbeat`,
`POST /api/v1/runs/{id}/complete`, `POST /api/v1/runs/{id}/events`,
`PUT /api/v1/runs/{id}/artifacts`. These are the internal worker protocol and
are not part of the documented bot API.

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
# auth = "tokens"   # default; "none" opts out on loopback/Tailscale only
```

Tokens live in the server database (outside any repository) and are never
written to config files or the repository. Keep them in your bot's
environment.

## MCP

The same API is exposed as MCP tools over streamable HTTP at `/mcp`; see
[docs/mcp.md](mcp.md).

## Skills and review profiles

Review profile endpoints ship above. Skills are a stub until the shared
skills loader lands (see the review docs); no skills endpoints exist yet.
