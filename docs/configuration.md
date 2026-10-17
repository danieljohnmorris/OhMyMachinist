# Configuration

Commands use an executor, optional prompt template, and timeout:

```toml
[commands.audit]
executor = "codex"
prompt_file = "prompts/audit.md"
timeout = "30m"

[commands.custom-workflow]
executor = "custom-workflow-script"
timeout = "2h"
```

Without `prompt_file`, the input prompt is sent unchanged. With a template, include
`{{machinist.prompt}}` for legacy command runs, or task variables such as
`{{task.spec}}` and `{{task.output_dir}}` for workflow tasks. Executors and repositories remain worker-owned:

```toml
[executors.custom-workflow-script]
command = ["./scripts/custom-workflow.sh"]

[repositories.my-project]
path = "/absolute/path/to/my-project"
```

Managed triggers select one command with `command = "audit"`. Model selection remains
available when the executor command includes `{{machinist.model}}`.

## Worker raw agent log

Each finished run uploads a raw agent log artifact for troubleshooting. Cap its
size in the worker config; the default is 64 MB:

```toml
data_directory = "~/.machinist/worker"
raw_log_max_bytes = 67108864
```

The cap bounds the uploaded file only; the transcript keeps its own typed events.
When the cap is reached the artifact ends with a `[raw agent log truncated at N MB]`
marker. The value must not be negative. The log is redacted with the same secret
patterns as the transcript.

## Task workflows

```toml
[workflows.deliver]
steps = ["build"]

[workflows.plan_then_build]
steps = ["plan", { command = "build", approval = "before" }]
```

Each named command must be configured. Workflow prompts can read `{{task.title}}`,
`{{task.source_url}}`, `{{task.spec}}`, and `{{task.output_dir}}`. The shared folder
is restored from the latest completed stage before execution. Keep temporary work
in `MACHINIST_SCRATCH_DIR`. [Storage settings](artifacts.md#storage) control file
limits and location. Configuration changes apply to new tasks; submitted tasks
keep their saved definitions.

## Artifact storage directory

Files are stored on the control-plane server’s disk, separately from the worker’s
scratch directory. Choose a persistent directory in the server config:

```toml
[storage.artifacts]
path = "/var/lib/machinist/artifacts"
```

The default is an `artifacts` directory beside the server database. Relative paths
resolve against the config file; `~` expands to the server user’s home. The server
creates the directory and must have permission to write there. Restart the server
after changing storage settings. When relocating existing storage, stop the
server and copy the entire artifact directory before changing the path; changing
the setting alone does not move saved files. Back up the database and files together.
See [storage settings](artifacts.md#storage) for size limits. Files are kept until their task is deleted.

## Failure alerts

When a run ends `failed` or `timed_out`, the self-healing pass owns alerting:
exactly one `fail-<run>.json` is spooled per run into a configured pending
directory for the external Factory Alerts watcher to pick up. The alert is
written when Machinist gives up on the run — retry budget exhausted, a spec or
secret-guard (or lane-dirty) cause needing a human, a healing error, workflow
jobs (healing never requeues those), and jobs with no tracker to comment on:

```toml
[alerts]
directory = "/var/lib/machinist/alerts"
```

The payload keeps the retired `fail.py` contract: `job_id`, `run_id`,
`repository`, `stage` and `command` (the stage that failed), configured
`timeout_millis`, `elapsed_millis`, `exit_code`, `reason` (`failed` or
`timed_out`), `classification` (the heal cause), `stderr_tail` (last 40
non-empty lines of the run error), an `inspect` command to read the run from
the server database, and `issue_key`/`issue_url` when the job came from a
tracker. A `healing` object carries the diagnosis: `cause`, `summary`, `fix`,
`attempts`, `budget`, `needs_human` and a `reason` saying why it was handed to
a human (or `retry_after` for scheduled retries).

**Dedupe rule.** Each file is written once per run (exclusive create) and
never duplicated, so a re-delivered completion neither rewrites the file nor
re-fires the webhook. Cancelled runs are operator actions and never alert. A
run that self-healing requeues produces no needs-human alert; with
`healing_alerts = true` it also spools a `healing-<run>.json` notice (same
payload, `needs_human: false`) so the watcher can follow the retry.

**Webhook.** Set `webhook_url` and every spooled alert — fail, healing and
health — is POSTed to it as JSON so the watcher can wake on push instead of
polling. The optional secret comes from `webhook_secret_env` or
`webhook_secret_file` and is sent, unlogged, in `webhook_header` (default
`X-Machinist-Alert-Token`). Delivery retries three times with a 10s timeout
per attempt; a final failure is logged and never fails the run completion or
the file write.

**Who writes what.** The server writes `fail-<run>.json`, the optional
`healing-<run>.json`, and `health-<name>.json` (one per configured endpoint,
overwritten until the endpoint recovers, then removed). The drift check
writes `drift-<timestamp>.json`. The Plane intake writes `pr-<KEY>-<job>.json`
when a run opens a pull request. The watcher should treat `needs_human: true`
fail alerts and unreachable-endpoint health alerts as pages, `healing-*.json`
as informational, and `pr-*.json` as a review prompt.

A command that runs past its `timeout` ends `timed_out` with exit code 124 and
alerts exactly like any other failure. Relative paths resolve against the
config file and `~` expands to the server user's home, like the artifact
storage path. When `[alerts]` is absent no files are written and no webhook
fires — `deploy/install.sh` and the drift check both warn about that config
(MACH-70), because failures then alert nowhere.

## Migration

The `agents` table was renamed to `commands`. Move `[agents.NAME]` to `[commands.NAME]`
and replace `--agent` with `--command`.

Legacy `[pipelines]` configuration is not supported. Use `[workflows.NAME]` with
an ordered `steps` list for stages tracked by Machinist, or use one script command
when the script should own its internal process. See [the task workflow guide](task-guide.md)
and [workflow configuration](workflows.md). Pre-command databases are
recreated once because this release intentionally consolidates the schema before active use.

## Built-in project management

The **Tickets** page uses the built-in tracker by default. Tickets and their
comments live in the control-plane SQLite database; no tracker service or API
key is required. Configure the command to run when an operator approves work:

```toml
[tracker]
default = "builtin" # builtin, plane, or linear; selects the Tickets view
key_prefix = "MM"
repository = "my-project"
command = "task-to-pr" # must name an existing [commands.task-to-pr]
# model = "optional-model-override"
```

Create a ticket, choose its repository, then use **Approve for factory**.
Approval atomically queues one job and stores its ticket link. Repeated approval,
including after a restart, returns the same job rather than submitting another.
A missing repository or command prevents approval without losing the draft.

Tickets start in `backlog`; `todo` is an optional planning state. Approval moves
them to `approved`, worker dispatch to `in_progress`, and successful execution to
`in_review`—successful execution alone is not delivery. Marking the ticket `done`
resolves its finished job, including when the ticket was marked done before the
worker finished. Completion posts feedback with the run link and any returned
PR URL. Drafts can be deleted; approved tickets retain their delivery history.

### Projects

Define `[tracker.projects.NAME]` tables to group built-in tickets. The name is the
key prefix (`NAME-1`, `NAME-2`, …) and must be 1–10 uppercase letters or digits
starting with a letter; lowercase names are normalised to uppercase. A project
supplies defaults for `repository`, `command` and `model`, falling back to
`[tracker]` when omitted. The project is chosen when the ticket is created and
cannot be changed afterwards, because the key derives from it. Approved jobs carry
the project so the runs board groups them with the same stack.

```toml
[tracker.projects.sparks]
repository = "sparks-repo"
command = "task-to-pr"
```

Projects can also be added and edited from the control plane's Projects view
(`#/projects`). Each entry is stored as a `tracker.projects.<NAME>` setting
holding the table body (`repository`, `command`, `model`); these settings
overlay the config file's tables the same way a config overlay does — the
setting wins per project, and the change applies on the next ticket without a
restart. Per-project skills live in the separate `skills.project.<NAME>`
setting and are edited on the same page.

The ticket stores the pull request URL reported by its run, and the ticket page
links to it. Tickets are flat: there are no sub-tickets. Jobs admitted from Plane
or Linear keep the issue's labels, shown under delivery details on the run.

Plane and Linear remain external trackers: create and edit their tickets in
those services, and configure the approved-label intake below. Selecting an
external tracker in Tickets does not migrate built-in tickets or credentials.

## Issue sources

Issue sources are configured on the server and use environment credentials. A
missing label defaults to `factory`. Project keys map to worker repository
names:

```toml
[issue_sources]
label = "factory"

[issue_sources.plane.my-project]
project_id = "00000000-0000-0000-0000-000000000000"

[issue_sources.linear.my-project]
project = "Example Project"

[issue_sources.triggers.my-project]
every = "5m"
source = "plane"
repository = "my-project"
command = "task-to-pr"
```

Set `source = "linear"` for a Linear trigger. For Plane, set `PLANE_URL`,
`PLANE_WORKSPACE`, and `PLANE_API_KEY`; for Linear, set `LINEAR_API_KEY`. Poll
jobs are deduplicated by issue URL or key while active.

Finished jobs whose ticket completes outside the factory move to the green
`resolved` state. Plane completed-group states and Linear completed-type
states are recognized; polling excludes completed and cancelled tickets so
leaving the approval label attached does not admit them again.

The control plane reconciles tickets at startup and roughly every ten minutes.
A merged pull request can resolve work by ticket key in its title or branch,
or by an issue link in its body. For external trackers, resolution moves the
ticket to Done and posts a comment. Built-in tickets are marked Done with a
comment and their linked finished jobs are resolved in one transaction.

Point issue webhooks at `POST /api/v1/webhooks/issues`. Plane uses HMAC-SHA256
in `X-Signature`; Linear uses `Linear-Signature` over the raw body and requires
a `webhookTimestamp` within five minutes of server time. The shared secret
comes from `issue_sources.webhook_secret` or
`MACHINIST_ISSUE_WEBHOOK_SECRET`. Native Plane intake uses the separate
`X-Plane-Signature` path described below.

## Plane intake

Machinist runs Plane intake itself: a webhook endpoint and a poll fallback that
stands down while webhooks are arriving. The same behaviour used to live in the
retired `plane-intake.py` (archived under `contrib/legacy/plane-intake/`). Every
value below is a default that an editable `plane.*` setting overrides, so an
empty `[plane]` table keeps working:

```toml
[plane]
url = "https://plane.danieljohnmorris.com"
workspace = "dan"
trigger_label = "factory"
approve_label = "approved"
command = "build-pr"
default_repo = ""            # unmapped projects get no job when empty
ui_url = "http://127.0.0.1:7331"
shadow = true                # evaluate and record without acting
auto_submit = true
start_comment = true
sync_since = "2026-10-10T10:48:00Z"
poll_every = "2m"
sync_every = "20s"
repositories = { p1 = "OhMyMachinist" }
identifiers = { p1 = "MACH" }
supervisors = { OMM = "Om", _default = "LocalAI" }
```

Secrets stay in the environment: `PLANE_API_KEY` for the REST API and
`PLANE_WEBHOOK_SECRET` (or `plane.webhook_secret`) for the HMAC signature on
`POST /api/v1/webhooks/issues`. The endpoint accepts CE v1 (`event = "issue"`,
action create/update) and v2 (`workitem.created`/`workitem.updated`) deliveries
in the `X-Plane-Signature` header, tolerating a re-serialised body.

The intake deduplicates per ticket: an edit to a ticket with a queued job
rewrites that job's spec, an edit while a job is running is ignored, and only a
ticket without active work queues a new job. Each job carries the ticket key,
its attempt chain, and the intake source, Plane delivery id and spec hash shown
on the run page.

Ticket state syncs back from runs: a start moves the ticket to `In Progress`
from backlog/unstarted and drops the `failed` label; a succeeded run with an
opened pull request moves it to `In Review`; a failed run adds the `failed`
label. Done and Cancelled tickets are never touched. A run that opens a pull
request writes a `pr-<KEY>-<job>.json` alert for the ticket's supervisor.

Editable settings (`plane.trigger_label`, `plane.approve_label`,
`plane.command`, `plane.default_repo`, `plane.ui_url`, `plane.auto_submit`,
`plane.start_comment`, `plane.shadow`, `plane.repositories`,
`plane.supervisors`, `plane.prompt_template`, …) are read on the next webhook,
poll or sync. Import the retired JSON state on first start with
`machinist plane poll` or by starting the server: `plane-repos.json`,
`plane-supervisors.json`, `plane-jobs/.seen.json`,
`plane-jobs/.state-sync.json` and `alerts/.plane-states.json` become database
rows. `GET /api/v1/plane/intake` serves the settings plus the shadow-mode
comparison report, and `machinist plane poll | sync | file F` runs one pass.
When the intake webhook is mounted, its `X-Plane-Delivery` header is recorded
on every job.
