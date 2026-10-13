<p align="center">
  <img src=".github/assets/machinist-lockup.svg" width="520" alt="Machinist">
</p>

<p align="center">
  <strong>The open source AI software factory for agentic coders.</strong><br>
 Machinist is an open source software factory for repeatable and scalable AI coding workflows. 
</p>

<p align="center">
  <a href="https://machinist.sh">Website</a> ·
  <a href="docs/README.md">Documentation</a> ·
  <a href="docs/configuration.md">Configuration</a> ·
  <a href="examples/workflows/README.md">Workflow examples</a>
</p>

<p align="center">
  <img src=".github/site/technical-drawings.webp" width="100%" alt="Technical drawings of a milling machine, supervised coding-agent system, and precision linear assembly">
</p>

<p align="center"><sub>Machine section · supervised agent system · exploded assembly</sub></p>

Machinist is an open-source software factory implementation. It runs on your machine, keeps repository access and credentials local, and records the work from request to handoff. Commands can invoke Codex, Claude Code, another agent CLI, a test runner, a shell script, or repository-owned orchestration.

Please note: this is early access software and subject to change. 

## Why Machinist

- **One controlled entrypoint.** Workers expose named commands and repositories, never arbitrary shell text or machine-local paths.
- **Bring your own harness.** Use any executable that accepts a prompt on standard input.
- **Keep authority local.** Repositories, credentials, model aliases, and executor configuration stay on the worker.
- **Inspect every run.** Stream output, retain durable events and artifacts, and track terminal outcomes, duration, and reported token use.
- **Choose where to approve.** Pause before a step, review its files, and approve or request changes. Optional merge policies can automate narrowly defined low-risk changes.

<a id="quick-start"></a>

## Quick start

Build and initialize Machinist:

```sh
git clone https://github.com/owainlewis/machinist.git
cd machinist
mkdir -p ./bin && go build -o ./bin/machinist ./cmd/machinist
./bin/machinist init
```

`init` writes `~/.machinist/config.toml` with two commands, `task-to-pr` and `audit`.
Each is a prompt, an executor, and a timeout:

```toml
# ~/.machinist/config.toml
[commands.task-to-pr]
executor = "codex"
prompt_file = "prompts/task-to-pr.md" # optional
timeout = "120m"
```

Run it directly:

```sh
./bin/machinist run --command=task-to-pr --repo=/path/to/repo --prompt="Complete https://github.com/owner/repo/issues/42"
```

## How execution works

For a direct run, Machinist maps the configured command name to a fixed executable and uses the path supplied with `--repo` as the working directory. Managed submissions instead resolve an approved repository name from the worker configuration. In both cases, Machinist renders the prompt, sends it on standard input, streams stdout and stderr, and applies one overall timeout and cancellation. Exit code 0 succeeds; every non-zero exit code fails.

Inside each command, scripts are intentionally opaque. Their internal stages appear in logs, but Machinist does not infer their internal stages. A killed script restarts from the beginning unless the script owns checkpointing.

For explicit steps, [configure a workflow](docs/workflows.md):

```toml
[workflows.deliver]
steps = ["task-to-pr"]
```

Workflow tasks retain results and saved files, support approval and feedback, and keep earlier attempts in history. Each stage can read and write `{{task.output_dir}}`; Machinist restores and saves the shared folder between stages.

Start with [your first task workflow](docs/task-guide.md): create a task from an issue or spec, follow it on the board, and review the result. The [classify-then-merge example](examples/workflows/risk_delivery/README.md) adds independent risk assessment and a conservative merge policy.

## Go deeper

<a id="ohmymachinist"></a>

## OhMyMachinist

This fork adds two generic issue sources and delivery metadata:

- **Plane and Linear adapters.** Both can poll a configured label and submit a source-neutral issue. Plane uses `PLANE_URL`, `PLANE_WORKSPACE`, and `PLANE_API_KEY`; Linear uses `LINEAR_API_KEY`. Configure project-to-repository mappings and poll cadence in `config.toml`. HMAC-verified webhook intake is available as a library helper.
- **Job metadata.** Workflow steps may set `metadata` in their JSON step result. The job page shows optional issue, branch, pull request, preview, review verdict/summary, screenshots, status, and timestamps. Job titles use `KEY: Title`, and job lists expose the full title in a tooltip when clipped.
- **Generic delivery example.** `examples/factory/delivery.sh` has a dry-run mode and separate code/review stages. It never merges or deploys.

### Post-PR review

`deploy/factory/post_pr_review.py` reviews an existing pull request after delivery. It runs entirely from a **review profile** (MACH-32): lane checks for the languages in the diff, parallel reviewer passes routed by file glob, a docs pass, blocking policy, and the amend loop. It classifies check failures as new or pre-existing, posts one PR comment showing the resolved profile, skill versions and per-pass results, and sets the `machinist/review` commit status.

```sh
python3 deploy/factory/post_pr_review.py --repo <checkout> --pr <github-pr-url-or-owner/name#n> [--base <branch>] [--executor-cmd "<cmd ...>"] [--dry-run] [--json-out <path>] [--timeout-seconds <seconds>]
```

- Exit codes: `0` pass (and incomplete), `2` blocked by new check failures or blocking findings.
- Executor: set `MACHINIST_REVIEW_EXECUTOR` or pass `--executor-cmd`. The command must read a prompt on stdin and print the final answer. With neither set, reviewers and docs are skipped with an explicit note.
- Timeouts: default to 900 seconds per command; set `MACHINIST_REVIEW_TIMEOUT_SECONDS` or `--timeout-seconds`.
- Dry run: prints the comment body and planned commands without calling GitHub write APIs, pushing, or committing.

Wire it after delivery with:

```sh
python3 deploy/factory/post_pr_review.py --repo "$repo" --pr "$(jq -r .pr "$MACHINIST_OUTPUT_DIR/pr.json")"
```

Configure branch protection to require the `machinist/review` commit status so blocked pull requests cannot merge. The tool never merges, approves, or closes pull requests.

#### PR review profiles

A review profile is YAML (or markdown with YAML frontmatter) declaring the whole review:

- `scope`: `pin_head_sha` (findings tie to the head SHA; affected steps re-run if the head moves), `lanes_from` (`diff` or `repo`), `mode` (`review_only` or `amend` — amend allows pushing fixes to the PR branch).
- `checks.lanes[]`: each lane has a `when` glob list and ordered `commands`. A command is a string or `{run, if_exists, if_script, if_configured, optional, else, requires_authorization}`; `{changed}`, `{changed_py}`, `{changed_shell}` expand to the matched files. `checks.blocking` is `new_failures` (new vs base block; pre-existing is reported only) and `checks.missing_tool` is `blocked`, `skip`, or `pass` (blocked never counts as a pass). A command with `requires_authorization: deploy` is skipped and reported pending unless the run is started with `--authorized deploy` (or `MACHINIST_REVIEW_AUTHORIZED=deploy`).
- `reviewers[]`: named reviewer passes, each with `skills` (names from the installed skills library — e.g. `feature-dev:code-reviewer` — or an inline `prompt`), optional `model`/`effort`, `lanes` filters, `when` globs, `read_only`, `optional`, `trigger: on_request` (runs only when the PR carries the `machinist:adversarial` label), and `routing_only`.
- `routing`: `rules` map file globs to passes (mixed PRs get every matching pass; an unreadable file list falls back), plus the `fallback` reviewer set.
- `concurrency`: reviewer waves; a required pass is never dropped, only delayed.
- `reconcile`: `dedupe` and `verify_against_code` (findings outside the changed files are marked unverified).
- `docs`: `enabled`, `files`, `update_pr_description`.
- `blocking`: `severities` and `amend_rounds` (replaces `MACHINIST_REVIEW_AMEND_ROUNDS`; CLI/env still override).
- `output`: `pr_comment`, `commit_status` (default `machinist/review`), and `report` sections.

**Precedence.** The review resolves the profile in this order, first match wins:

1. `.machinist/review.yaml` (or `.machinist/review.md`) in the repository **at the PR base ref** — so a pull request cannot weaken its own review.
2. The project's setting in **Settings** (a library profile name or a full YAML body, plus optional extra instructions).
3. The instance-wide **default profile**: the Settings global default picker, else `review.default_profile` in the control-plane config, else the shipped `default` profile.
4. The shipped default profile bundled with the review step.

Shipped profiles live in [`profiles/review/`](profiles/review/) (`default`, and `dan-pr-review` ported from [pr-review-skill](https://github.com/danieljohnmorris/pr-review-skill/blob/main/SKILL.md) — set `default_profile = "dan-pr-review"` under `[review]` in the config, or pick it per project in Settings). A minimal worked example is at [`examples/review/basic.yaml`](examples/review/basic.yaml). The old free-text per-project instructions were migrated into each project's profile as extra instructions; nothing was lost.

**Skills.** Reviewer `skills` reference names in the installed skills library (`~/.agents/skills`, seeded from `deploy/factory/review/skills/`). Nothing is fetched at review time; if a required skill is missing the pass is reported INCOMPLETE, and an `optional` pass is skipped with a note. Install additional reviewer skills with `machinist review install-skill <github SKILL.md url>`; the shared skills loader (MACH-33) extends this stub with version pinning and caching.

**Settings editor.** Settings shows the resolved source, a library profile picker, an inline YAML editor, extra instructions, the global default picker, and a no-execution **preview**: paste changed-file paths and see the resolved profile source, matched lanes, checks, and reviewer passes. Saves are validated server-side and rejected with line-numbered errors (unknown keys, bad globs, unknown pass refs).

**Secrets.** Profiles and the PR comment never echo credentials: token-shaped strings in check and reviewer output are redacted before publication, the installer refuses payloads with secret findings, and CI runs a gitleaks scan on every PR. Secrets come only from the environment or a systemd `EnvironmentFile` outside git.

To follow upstream while keeping this fork:

```sh
git fetch upstream
git merge upstream/main
```

Configure sources with placeholder values, then replace only the local values:

```sh
export PLANE_URL=https://plane.example.com
export PLANE_WORKSPACE=
export PLANE_API_KEY=
export LINEAR_API_KEY=
```

| Guide | What it covers |
| --- | --- |
| [Documentation](docs/README.md) | Choose the right setup and operations guide |
| [Task workflow guide](docs/task-guide.md) | Set up planning, approval, shared files, and build |
| [Configuration](docs/configuration.md) | Commands, executors, workers, models, and repositories |
| [Development](docs/development.md) | Build, test, and work on Machinist locally |
| [VM deployment](docs/vm-deployment.md) | Run the control plane and worker as services |
| [Workflow examples](examples/workflows/README.md) | Prompt-driven and scripted workflows |

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) for development setup and pull-request expectations. Security issues should follow [SECURITY.md](SECURITY.md).

Machinist is released under the [MIT License](LICENSE).
