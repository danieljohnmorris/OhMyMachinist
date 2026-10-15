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

## Factory setup

The factory setup picks the tracker, the coding agent, the review agent and the
model each role uses. It is set per project in the UI, or as a global default in
`config.toml` under `[factory]`.

```toml
[factory.default]
tracker = "plane"
coding_agent = { kind = "omp", executor = "omp" }
review_agent = { kind = "codex", command = "codex exec --model {{model}}" }
models = { coding = { model = "glm-4.6", effort = "high", endpoint = "local", url = "http://spark:8080" } }

[factory.projects.my-project]
tracker = "linear"
models = { review = { model = "gpt-5", endpoint = "cloud" } }
```

`[factory.default]` applies to every project that has no setting of its own;
`[factory.projects.NAME]` applies to the worker repository `NAME`. Every field is
optional — an empty field means "unset", so that piece keeps whatever the trigger
or command configured.

### Vocabulary

- `tracker`: `plane` or `linear`. Both sit behind one adapter interface that does
  intake by label, comments, resolve-on-Done and ticket links. The project still
  needs its connection in [`[issue_sources.plane.NAME]`](#issue-sources) or
  `[issue_sources.linear.NAME]`, and the tracker key still comes from the
  environment (`PLANE_API_KEY`, `LINEAR_API_KEY`).
- `coding_agent`: `kind` is `omp`, `codex`, or `command` for a generic command
  template. `executor` names the worker executor that runs coding steps — that is
  the field that overrides the trigger's own agent, and the `coding` model must
  be an alias of that executor.
- `review_agent`: chosen separately from the coding agent — the same fields,
  except that `command` is the command string the built-in review runs. It takes
  `{{model}}`, `{{effort}}`, `{{endpoint}}` and `{{env.NAME}}` placeholders. It
  must name a `kind`, and `kind = "command"` must also carry a `command`; a spec
  that renders no command is rejected, since otherwise the project's review agent
  would be silently ignored.
- `models.<role>`: `coding`, `review`, or `non_coding`. Each holds `model`,
  `effort`, `endpoint` (`local` or `cloud`) and, for a local endpoint, the `url`
  of the server. A local endpoint requires a `url`.
- `{{env.NAME}}` placeholders resolve on the worker, from the worker's own
  environment, so secrets never enter `config.toml` or the control plane.

### Resolution order

First match wins:

1. the project's setting in Settings (stored in the control plane);
2. the instance-wide global default in Settings;
3. `[factory.projects.NAME]` in `config.toml`;
4. `[factory.default]` in `config.toml`;
5. built-in — empty, so the trigger or command keeps its own agent and model.

Per-project overrides live in the **Settings** page and are stored in the control
plane, so they survive a config reload and apply to new runs without editing the
file. Settings offers the resolved source, the supported adapters and roles, and
saves that are validated server-side.

### Worker executors, commands and model aliases

Agent executors, their command templates and their model aliases stay worker-owned
in `worker.toml`:

```toml
[executors.omp]
command = ["omp", "--model={{machinist.model}}", "-"]
models = { default = "glm-4.6", glm-4.6 = "glm-4.6" }
```

`coding_agent.executor` must name one of those executors, the model the `coding`
role selects must be an alias listed in that executor's `models` table, and the
executor's command must carry `{{machinist.model}}` — otherwise the run fails at
resolve time, with `executor "<name>" does not support model selection` or
`model "<name>" is not configured for executor "<name>"`.

The review agent is different: it renders a **command string**, which the worker
hands to the review harness as a shell-style command. Leave `command` empty to use
the adapter's default for `kind`, and note that the resolved review agent wins over
`MACHINIST_REVIEW_EXECUTOR` set in the worker's own environment:

| `kind` | Default review command |
| --- | --- |
| `omp` | `omp --model {{model}} -` |
| `codex` | `codex exec --model {{model}}` |
| `command` | none — `command` is required |

A local endpoint's `url` reaches the template as `{{endpoint}}`.

### Worked examples

**Local GLM via omp.** Plane for tickets, omp coding and reviewing, with a GLM
model on a local server:

```toml
[factory.default]
tracker = "plane"
coding_agent = { kind = "omp", executor = "omp" }

[factory.default.models.coding]
model = "glm-4.6"
effort = "high"
endpoint = "local"
url = "http://spark:8080"

[factory.default.models.review]
model = "glm-4.6"
effort = "high"
endpoint = "local"
url = "http://spark:8080"
```

`coding_agent.executor = "omp"` names the worker's `[executors.omp]`, whose
`models` table must list `glm-4.6` as an alias.

**Linear plus a cloud Codex review.** Linear for tickets, omp coding on the local
model, and a separate Codex CLI review agent on a cloud model:

```toml
[factory.default]
tracker = "linear"
coding_agent = { kind = "omp", executor = "omp" }
review_agent = { kind = "codex", command = "codex exec --model {{model}}" }

[factory.default.models.coding]
model = "glm-4.6"
endpoint = "local"
url = "http://spark:8080"

[factory.default.models.review]
model = "gpt-5"
effort = "high"
endpoint = "cloud"
```

The coding agent and the review agent differ here: coding runs omp with the local
model, review runs Codex with the cloud one, and `{{model}}` and `{{effort}}` in
the review command expand to the `review` role's values.

A secret a command template needs is written as a placeholder, not a value:

```toml
review_agent = { kind = "command", command = "my-reviewer --model {{model}} --effort {{effort}} --api-key {{env.MY_REVIEWER_API_KEY}}" }
```

`{{env.MY_REVIEWER_API_KEY}}` is replaced on the worker with that worker's
environment value, so the key never appears in this file, in the Settings page or
in the control plane.

### API

The Settings page reads and writes these through `/api/v1/factory-setting`
(GET, PUT, DELETE a project's setup), `/api/v1/factory-default` (GET, PUT the
global default) and `/api/v1/factory-catalog` (GET the supported trackers,
agents, roles and endpoints). See the [HTTP API reference](api.md#endpoints) for
request bodies and scopes.
