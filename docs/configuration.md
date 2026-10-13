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
resolved_state = "Done"

[issue_sources.triggers.my-project]
every = "5m"
source = "plane"
repository = "my-project"
command = "task-to-pr"
```

Set `source = "linear"` for a Linear trigger. For Plane, set `PLANE_URL`,
`PLANE_WORKSPACE`, and `PLANE_API_KEY`; for Linear, set `LINEAR_API_KEY`. Poll
jobs are deduplicated by issue URL or key while active. A Linear ticket is
resolved when it reaches the state named by `resolved_state` (default `Done`)
in that repository's `[issue_sources.linear...]` section; the merged-pull-
request path moves it there and posts one comment. Linear webhooks are not
consumed yet: reconciliation runs at startup and roughly every ten minutes.

## Built-in backlog

Repositories without an issue source can be driven from the built-in backlog:
a lightweight issue tracker stored in the control plane database, with states
`backlog`, `todo`, `started`, `completed` and `cancelled`. Creating and editing
backlog issues never enqueues work; **Start run** on an issue creates a
workflow or command job with the issue's current spec pinned to it, so later
edits never rewrite queued or running work. A backlog issue moves to `started`
once when its first run starts; from then on its state is yours to change,
and a merged pull request records a resolution on the job without completing
the issue. The backlog HTTP API is `GET/POST /api/v1/backlog`,
`GET/PATCH /api/v1/backlog/{id}` and `POST /api/v1/backlog/{id}/start`; the
web UI has a Backlog board with the same actions.

Finished jobs whose ticket completes outside the factory move to the green
`resolved` state. A card becomes resolved when its Plane ticket reaches a
completed-group state (Done), its Linear ticket reaches the configured
resolved state, or when a merged pull request references the ticket key in
its title or branch, or links the issue. The control plane reconciles tickets
at startup and then roughly every ten minutes; a merged pull request also
moves the Plane or Linear ticket to its resolved state and posts a comment.
Point a Plane webhook at `POST /api/v1/webhooks/issues` (HMAC-SHA256 in the
`X-Signature` header) to resolve cards immediately on state changes. The
shared secret comes from `issue_sources.webhook_secret` or the
`MACHINIST_ISSUE_WEBHOOK_SECRET` environment variable.
