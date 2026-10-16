<p align="center">
  <img src="https://github.com/danieljohnmorris/OhMyMachinist/blob/main/docs/assets/logo-dark-padded.png?raw=true" alt="oh my machinist">
</p>

<p align="center">
  <strong>Tracker tickets in, reviewed pull requests out.</strong>
</p>

<p align="center">
  <a href="https://github.com/danieljohnmorris/OhMyMachinist/releases"><img src="https://img.shields.io/github/v/release/danieljohnmorris/OhMyMachinist?include_prereleases&style=flat&colorA=222222&colorB=3FB950" alt="Release"></a>
  <a href="https://github.com/danieljohnmorris/OhMyMachinist/blob/main/LICENSE"><img src="https://img.shields.io/github/license/danieljohnmorris/OhMyMachinist?style=flat&colorA=222222&colorB=58A6FF" alt="License"></a>
  <a href="https://go.dev"><img src="https://img.shields.io/badge/Go-00ADD8?style=flat&colorA=222222&logo=go&logoColor=white" alt="Go"></a>
  <a href="https://react.dev"><img src="https://img.shields.io/badge/React-61DAFB?style=flat&colorA=222222&logo=react&logoColor=white" alt="React"></a>
  <a href="https://github.com/owainlewis/machinist"><img src="https://img.shields.io/badge/fork%20of-Machinist-E05735?style=flat&colorA=222222" alt="Fork of Machinist"></a>
</p>

<p align="center">
  Fork of <a href="https://github.com/owainlewis/machinist">Machinist</a> by <a href="https://github.com/owainlewis">@owainlewis</a>
</p>

OhMyMachinist is a fork of [Machinist](https://github.com/owainlewis/machinist), the open source AI software factory. It turns tracker tickets into reviewed pull requests on its own: label a ticket, and the factory picks it up, codes it, opens a PR, reviews it and closes the loop on the ticket.

It keeps the upstream control plane, worker model and executors. Everything below is what the fork adds on top. For the core docs, see [upstream Machinist](https://github.com/owainlewis/machinist#readme).

> [!NOTE]
> Early access software, run by one person against their own projects. Expect sharp edges.

## What it adds

**Intake**

- Plane intake by label poll or webhook, with project-to-repository mapping
- Built-in intake with an approve bridge, run-to-ticket state sync, supervisor alerts and a shadow mode for cutover
- One job per ticket (deduplicated), with Plane and PR links on every run
- Tickets resolve automatically when the ticket or its linked PR completes

**Delivery**

- Retries inject the previous attempt's transcript into the next coder prompt
- Job timeouts with failure alerts
- Lane checkouts are fetched fresh before each run; a dirty lane is stashed with a patch instead of failing the job

**Review**

- Built-in PR review after a pull request opens: language-aware checks, code and prose reviewers, a docs pass, one PR comment and a `machinist/review` commit status

**Web UI**

- Tasks board: one card per ticket with prior attempts, active and failed by default, Show succeeded, project and date filters that persist in the URL
- Run page: attempt picker, transcript tab, full-height transcript and an activity sidebar with state timings
- Transcript streams the whole agent session live: every assistant message, thinking summary, tool call and tool result (omp runs in `--mode json`), per attempt, followed by the delivery and review output
- Sidebar shows worker count and job concurrency, with Start/Stop and a Factory page for the job cap and lanes
- Factory controls: pause or resume dispatch (drain or cancel the running job), set how many jobs run at once, and enable or disable individual lanes, from the UI, the API, MCP or `machinist pause|resume`

## How it works

```
factory label  ->  intake  ->  coding agent in a lane  ->  pull request
               ->  built-in review  ->  merge and deploy  ->  ticket resolves
```

1. Add the `factory` label to a ticket.
2. Intake polls (or receives a webhook), dedupes, and queues one job for the mapped repository.
3. A worker runs your coding agent in a lane checkout, fetched fresh.
4. The agent pushes a branch and opens a PR.
5. The built-in review runs checks and reviewers, comments, and sets `machinist/review`.
6. The PR merges and deploys under your branch protection and merge policy.
7. The ticket is marked resolved in the tracker and in Machinist.

## Configuration today

**Coding agent.** Any executor that reads a prompt on stdin: omp, Codex, Claude Code, or your own command.
**Models.** Local or cloud, chosen per executor through model aliases.
**Lanes and concurrency.** Set how many jobs run at once and which repositories each worker may use.
**Review profile.** Choose the review executor and timeout with `MACHINIST_REVIEW_EXECUTOR` and `MACHINIST_REVIEW_TIMEOUT_SECONDS`.

A minimal setup, with placeholders:

```toml
# ~/.machinist/config.toml
[server]
listen = "127.0.0.1:7331"
max_concurrent_jobs = 2

[commands.task-to-pr]
executor = "omp"
timeout = "120m"
```

```toml
# ~/.machinist/worker/worker.toml
[executors.omp]
command = ["omp", "--model={{machinist.model}}", "-"]
models = { default = "<your-model>" }

[repositories.my-project]
path = "/absolute/path/to/my-project"
```

Secrets (tracker API keys, GitHub tokens) come from environment files kept out of git:

```sh
PLANE_URL=https://plane.example.com
PLANE_WORKSPACE=
PLANE_API_KEY=
```

## PR review profiles

A review profile is YAML (or markdown with YAML frontmatter) declaring the whole review:

- `scope`: `pin_head_sha` (findings tie to the head SHA; affected steps re-run if the head moves), `lanes_from` (`diff` or `repo`), `mode` (`review_only` or `amend` — amend allows pushing fixes to the PR branch).
- `checks.lanes[]`: each lane has a `when` glob list and ordered `commands`. A command is a string or `{run, if_exists, if_script, if_configured, optional, else, requires_authorization}`; `{changed}`, `{changed_py}`, `{changed_shell}` expand to the matched files. `checks.blocking` is `new_failures` (new vs base block; pre-existing is reported only) and `checks.missing_tool` is `blocked`, `skip`, or `pass` (blocked never counts as a pass). A command with `requires_authorization: deploy` is skipped and reported pending unless the run is started with `--authorized deploy` (or `MACHINIST_REVIEW_AUTHORIZED=deploy`).
- `reviewers[]`: named reviewer passes, each with `skills` (names from the installed skills library — e.g. `feature-dev:code-reviewer` — or an inline `prompt`), optional `model`/`effort`, `lanes` filters, `when` globs, `read_only`, `optional`, `trigger: on_request` (runs only when the PR carries the `machinist:adversarial` label), and `routing_only`.
- `routing`: `rules` map file globs to passes (mixed PRs get every matching pass; an unreadable file list falls back), plus the `fallback` reviewer set.
- `concurrency`: reviewer waves; a required pass is never dropped, only delayed.
- `reconcile`: `dedupe` and `verify_against_code` (findings outside the changed files are marked unverified).
- `docs`: `enabled`, `files`, `update_pr_description`.
- `blocking`: `severities`, `amend_rounds` (max review/fix rounds; `--amend-rounds` or `MACHINIST_REVIEW_AMEND_ROUNDS` override) and `max_minutes` (wall-clock budget in minutes for the whole review stage; `--max-minutes` or `MACHINIST_REVIEW_MAX_MINUTES` override; default 30).
- `output`: `pr_comment`, `commit_status` (default `machinist/review`), and `report` sections.

**Precedence.** The review resolves the profile in this order, first match wins:

1. `.machinist/review.yaml` (or `.machinist/review.md`) in the repository **at the PR base ref** — so a pull request cannot weaken its own review.
2. The project's setting in **Settings** (a library profile name or a full YAML body, plus optional extra instructions).
3. The instance-wide **default profile**: the Settings global default picker, else `review.default_profile` in the control-plane config, else the shipped `default` profile.
4. The shipped default profile bundled with the review step.

Shipped profiles live in [`profiles/review/`](profiles/review/) (`default`, and `dan-pr-review` ported from [pr-review-skill](https://github.com/danieljohnmorris/pr-review-skill/blob/main/SKILL.md) — set `default_profile = "dan-pr-review"` under `[review]` in the config, or pick it per project in Settings). A minimal worked example is at [`examples/review/basic.yaml`](examples/review/basic.yaml). The old free-text per-project instructions were migrated into each project's profile as extra instructions; nothing was lost.

**Skills.** Reviewer `skills` reference names in the versioned skills library at `~/.machinist/skills` (`MACHINIST_DATA_DIR` overrides the Machinist data directory). The library keeps every installed version at `<name>/<version>/` with a per-skill `manifest.json`; a review resolves the active version, then falls back to `~/.agents/skills`, the shipped `deploy/factory/review/skills/`, and `review/prompts/`. Nothing is fetched at review time; if a required skill is missing the pass is reported INCOMPLETE, and an `optional` pass is skipped with a note.

### Skills library

`machinist skills` owns one installed library with version history:

- `machinist skills install <github url | directory | zip>` — resolve a GitHub `blob`/`raw`/`tree` URL (optional `@ref`) to a commit SHA and fetch the whole skill directory, or install a local directory or zip. SKILL.md frontmatter must set `name` and `description`, and every new version is scanned with `gitleaks` before it becomes active (a missing `gitleaks` warns and records `scanned: false`).
- `machinist skills update <name> [--activate]` — re-resolve the recorded source and add a new version, keeping the old ones; the active version only moves with `--activate`.
- `machinist skills pin <name> <version>` — set the active version (rollback included).
- `machinist skills uninstall <name> [--force]` — remove a skill; refused while a shipped review profile still references it unless `--force`. Profiles that live in a repository or come from Settings are not consulted, so they can still point at a removed skill.
- `machinist skills list` — installed skills and their active version.

`machinist review install-skill <url>` is an alias for `skills install`. Private GitHub repositories read a token from `GITHUB_TOKEN`; it is never written to the manifest, the source URL, or logs.

### Skills configuration

A run loads an ordered list of installed skills, composed from three layers:

1. `.machinist/skills.yaml` in the repository, read at the base ref,
2. the project setting `skills.project.<project>`,
3. the global setting `skills.default`.

Layers compose in that order, de-duplicate by name, and a higher layer disables an inherited name with `enabled: false`. A `.machinist/skills/<name>/SKILL.md` bundled in the repository is a repo-scoped skill whose version is the repository commit, visible only to that project. Nothing is fetched at run time: a missing entry fails a run only when it is `required`, otherwise it is skipped with a warning. A minimal example is at [`examples/skills.yaml`](examples/skills.yaml).

```yaml
skills:
  - name: feature-dev:code-reviewer
  - name: rust-review@<version>
    applies_to: [review]
    rule: Prefer small, boring changes.
    required: true
  - name: typescript-jsdoc
    enabled: false
```

The loaded list (name, version, scope `global`/`repo`, injection method `native`/`prompt`, and warnings) is recorded on the run and returned with the job and run API responses; the run page shows it with any load warnings. The **Skills** page lists the installed library with its source, active version, install/update times and the projects and review profiles that reference it, and installs, updates, pins and uninstalls skills. Settings adds a per-project skills editor: an ordered list of names with `applies_to` and `required`, validated against the library, saved to the `skills.project.<project>` setting, with a **Resolve now** preview of the composed list and versions. omp executors get a per-run agent directory (`skills/<name>/SKILL.md` plus `RULES.md`) that `skill://<name>` resolves against; other executors get each SKILL.md appended to the prompt under a header naming its source and version. Review profile `skills:` and reviewer `skills` resolve through the same composed list and library.

**Settings editor.** Settings shows the resolved source, a library profile picker, an inline YAML editor, extra instructions, the global default picker, and a no-execution **preview**: paste changed-file paths and see the resolved profile source, matched lanes, checks, and reviewer passes. Saves are validated server-side and rejected with line-numbered errors (unknown keys, bad globs, unknown pass refs).

**Review budget.** The review runs inside the same job as the coding step. To keep it from consuming the whole job timeout it has its own budget: at most `blocking.amend_rounds` review/fix rounds and at most `blocking.max_minutes` wall-clock minutes for the whole review stage (defaults 3 and 30). Both are overridable by environment (`MACHINIST_REVIEW_AMEND_ROUNDS`, `MACHINIST_REVIEW_MAX_MINUTES`) and by CLI (`--amend-rounds`, `--max-minutes`). The budget is also capped to the enclosing job's remaining time (exposed by the runner as `MACHINIST_JOB_TIMEOUT_SECONDS`/`MACHINIST_JOB_STARTED_EPOCH`), reserving `MACHINIST_REVIEW_DELIVERY_RESERVE_SECONDS` (default 5 minutes) for delivery: a round or fix pass that would not fit is not started. When the budget runs out the verdict is INCOMPLETE with the reason "review budget exhausted" (rounds, minutes or job time), never PASS, and never a job failure — the delivery step still commits and pushes any fix edits (or stashes them with a labelled stash and a patch in the run artifacts), keeps the PR open, and leaves the lane clean.

**Secret scan.** The built-in review runs a pinned, checksum-verified [TruffleHog](https://github.com/trufflesecurity/trufflehog) over the pull request range (`trufflehog git --since-commit=<base> --branch=<head> --results=verified`). A **verified** (live) credential is a blocking finding: it fails the `machinist/review` status and prevents auto-merge. Unverified and unknown detections are reported in the review comment but never block, and secret values are printed only in TruffleHog's redacted form. Factory hosts install the binary with `deploy/factory/install-review-skills.sh` (`MACHINIST_TOOL_DIR` overrides the install location; the default is `/usr/local/bin` when writable, else `~/.local/bin`); if `trufflehog` is missing the review is reported INCOMPLETE rather than silently passing. The shipped profiles also carry a `trufflehog` reviewer skill so a review pass can load it by name.

**Secrets.** Profiles and the PR comment never echo credentials: token-shaped strings in check and reviewer output are redacted before publication, the installer refuses payloads with secret findings, and CI runs a gitleaks scan (and a TruffleHog scan for verified credentials) on every PR. Secrets come only from the environment or a systemd `EnvironmentFile` outside git.

## In progress

- **Pluggable trackers.** Plane today, Linear planned.
- **Per-project choice** of tracker, coding agent and models in Settings (MACH-37).

## Example setup

One way to run it, not a requirement: Plane for tickets, omp as the coding agent, and a local GLM model served on a pair of NVIDIA DGX Sparks. Swap any piece for Linear, Codex, Claude Code or a cloud model.

## Install

**From source**

```sh
git clone https://github.com/danieljohnmorris/OhMyMachinist.git
cd OhMyMachinist
just build
./bin/machinist init
```

`just build` builds the web UI and the `machinist` binary into `./bin`.

**Run**

```sh
just local            # control plane and worker together
just control-plane    # control plane only
just worker           # worker only
```

Then open the UI at `http://127.0.0.1:7331`.

**Post-PR review on its own**

```sh
python3 deploy/factory/post_pr_review.py --repo <checkout> --pr <owner/name#n> [--dry-run]
```

**Follow upstream**

```sh
git remote add upstream https://github.com/owainlewis/machinist.git
git fetch upstream && git merge upstream/main
```

## Self-hosting the factory

Everything the factory needs to run on a host lives in the repository under
[`deploy/`](deploy/README.md): the factory scripts, the systemd units and
drop-ins, a generic `config.toml`/`worker.toml` template and the `*.env.example`
templates. Install or upgrade with one idempotent command:

```sh
sudo deploy/install.sh          # add --dry-run to preview
```

The installer copies the scripts to `/usr/local/lib/machinist-factory/`, the
units to `/etc/systemd/system/`, runs `daemon-reload`, restarts only changed
services, records the deployed git SHA, and refuses to install from a tree with
gitleaks findings. Instance values live in `/etc/machinist/config.local.toml` /
`worker.local.toml`, merged on top of the tracked config (base first, overlay
wins); secrets come only from `/etc/machinist/*.env` systemd `EnvironmentFile`s
outside git. A periodic drift check diffs the installed files against the repo at
the deployed SHA and alerts on any hand-edited file. See
[`deploy/README.md`](deploy/README.md) for the merge order, units and the drift
check.

## Core docs

Commands, executors, workflows, workers and deployment are documented upstream and in [`docs/`](docs/README.md):

| Guide | What it covers |
| --- | --- |
| [Upstream Machinist](https://github.com/owainlewis/machinist#readme) | The core project |
| [Configuration](docs/configuration.md) | Commands, executors, workers, models, repositories |
| [HTTP API](docs/api.md) | Versioned `/api/v1` endpoints with scoped bearer tokens |
| [MCP server](docs/mcp.md) | MCP tools over streamable HTTP at `/mcp` |
| [Workflows](docs/workflows.md) | Multi-step workflows and approvals |
| [VM deployment](docs/vm-deployment.md) | Running the control plane and worker as services |
| [Self-hosting the factory](deploy/README.md) | Installer, units, config overlay, drift check |
| [Factory review](deploy/factory/README.md) | The built-in PR review |

## Licence

Released under the [MIT License](LICENSE), as upstream.
