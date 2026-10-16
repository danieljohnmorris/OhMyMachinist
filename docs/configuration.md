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

When a run ends `failed` or `timed_out`, the server writes one pending fail alert
JSON file per run to a configured spool directory for an external alert poller or
notifier to pick up:

```toml
[alerts]
directory = "/var/lib/machinist/alerts"
```

The payload records the job id, run id, command (stage) name, configured timeout,
elapsed time, exit code, the issue key and URL when the job came from a tracker,
and a `reason` of `failed` or `timed_out` — so a notifier can report "timed out
after 2h" instead of a generic failure. A command that runs past its `timeout`
ends `timed_out` with exit code 124 and alerts exactly like any other failure;
each file is written once per run and never duplicated. Cancelled runs do not
alert. Relative paths resolve against the config file and `~` expands to the
server user's home, like the artifact storage path. When `[alerts]` is absent,
no files are written.

## Migration

The `agents` table was renamed to `commands`. Move `[agents.NAME]` to `[commands.NAME]`
and replace `--agent` with `--command`.

Legacy `[pipelines]` configuration is not supported. Use `[workflows.NAME]` with
an ordered `steps` list for stages tracked by Machinist, or use one script command
when the script should own its internal process. See [the task workflow guide](task-guide.md)
and [workflow configuration](workflows.md). Pre-command databases are
recreated once because this release intentionally consolidates the schema before active use.

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
`resolved` state. A card becomes resolved when its Plane ticket reaches a
completed-group state (Done), or when a merged pull request references the
ticket key in its title or branch, or links the Plane issue. The control plane
reconciles tickets at startup and then roughly every ten minutes; a merged
pull request also moves the Plane ticket to Done and posts a comment. Point a
Plane webhook at `POST /api/v1/webhooks/issues` (HMAC-SHA256 in the
`X-Signature` header) to resolve cards immediately on state changes. The
shared secret comes from `issue_sources.webhook_secret` or the
`MACHINIST_ISSUE_WEBHOOK_SECRET` environment variable.

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
