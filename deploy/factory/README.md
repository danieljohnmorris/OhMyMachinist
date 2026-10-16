# Factory scripts

Trusted post-agent tooling that runs outside the coding-agent sandbox.

Install or upgrade every script here (and the systemd units, config and env
templates) with the idempotent installer from the repository root:

    sudo deploy/install.sh          # add --dry-run to preview

It copies these scripts to `/usr/local/lib/machinist-factory/`, refuses to
install from a tree with gitleaks findings, and records the deployed git SHA.
The install snippets below are only for a one-off manual copy.

## Scripts

- `deliver-pr.py` — branch + commit + push + `gh pr create` delivery.
- `post_pr_review.py` — the built-in PR review (checks, reviewers, docs pass,
  secret scan, `machinist/review` commit status).
- `omp-glm`, `omp-glm-pr`, `codex` — coding-executor wrappers: run the agent in
  the sandbox, then deliver the PR outside it.
- `fail.py` — failure hook and Plane notifier (`hook`, `notify`, `sweep`,
  `dry-run`) called by the wrapper and by the fail-notify/fail-sweep units.
- `lane-clean.sh` — dirty-lane guard run as `ExecStartPre` on the worker units.
- `restart-when-idle.sh` — restart a worker unit only when its lane is idle.
- `install-review-skills.sh` — install the review skills and pinned TruffleHog.

Instance values (allowed owners, git identity, omp model, Plane host) come from
the environment / `/etc/machinist/*.env`, never from these scripts.

## deliver-pr.py

Branch + commit + push + `gh pr create` delivery. The secret-file guard refuses
real env files (`.env`, `.env.local`, `prod.env`, …) but allows example/template
env files (`.env.example`, `.env.sample`, `.env.template`, `.env.dist`,
`.env.defaults`, `example.env`, `sample.env`, `*.env.example`, `*.env.sample`);
staged added lines of those templates are still scanned for real-looking
secrets (see `tests/test_deliver_pr.py`).

## omp executors (omp-glm, omp-glm-pr)

`omp-glm` is the sandboxed omp executor (`omp-glm <low|medium>`, prompt on
stdin) and `omp-glm-pr` wraps it with `deliver-pr.py` for coding jobs. Both are
trusted, root-owned and installed next to `deliver-pr.py`. `install-review-skills.sh`
also installs the pinned, checksum-verified TruffleHog binary the review uses for
its secret scan (`MACHINIST_TOOL_DIR` overrides the destination; default
`/usr/local/bin` when writable, else `~/.local/bin`). If `trufflehog` is already
on `PATH` the installer leaves it alone.

MACH-42: `omp-glm` runs omp in `--mode text` or `--mode json` from
`MACHINIST_OMP_MODE` (default `text`). `omp-glm-pr` sets `MACHINIST_OMP_MODE=json`
for its coding run only, so the worker receives the session as JSONL and renders
every assistant message, thinking block, tool call and tool result in the run
transcript. The built-in review calls `omp-glm` without that variable and keeps
parsing the reviewer's text findings.

Reviewers and the amend pass run under the executor from
`MACHINIST_REVIEW_EXECUTOR`; if that is unset, `deliver-pr.py` uses
`<install dir>/omp-glm medium` when `omp-glm` sits next to it (the same
sandboxed GLM coder the wrapper uses), and with no executor at all it logs the
fact and the review skips the reviewer passes. INCOMPLETE (crashed review, no
executor, timeout) never fails the delivery job: `post_pr_review.py` logs a
warning and exits 0 while still recording the `machinist/review` status and
JSON summary; only BLOCKED exits `2` and fails the delivery.

Run tests:

    python3 -m unittest discover -s deploy/factory/tests -t .

## Lane hygiene (MACH-59)

The review runs in the lane checkout, so an edit it leaves behind (the docs
pass, an amend round) would make the final `git switch` fail and then fail every
queued job behind it. `post_pr_review.py` commits wanted edits to the PR branch
and pushes them, or stashes them with a labelled stash (`machinist-review-wip-*`)
plus a patch outside the published run artifacts (scratch, else the Machinist
data dir), before it switches back.
`deliver-pr.py` does the same before its final switch. The worker runner
(`internal/runner`) auto-saves a dirty lane checkout to a labelled stash and a
patch before fetching, logs `lane.wip.saved` on the run, and alerts once; if the
stash itself fails the lane pauses instead of claiming the next queued job.

`restart-when-idle.sh <systemd-unit> [data-dir]` restarts a worker only when it
is idle. The worker writes `<data-dir>/lane.lock` for the whole run, including
publishing outputs and reporting the completion, so the script never restarts a
lane in the middle of a job's final steps. The data dir defaults to
`$MACHINIST_DATA_DIR`, then `~/.machinist/worker` (the worker's default
DataDirectory), so the script finds the lock the worker actually writes.

## post_pr_review.py (built-in PR review, MACH-26)

After `deliver-pr.py` opens a pull request it runs the built-in review from the
same directory (`post_pr_review.py`; `MACHINIST_REVIEW_SCRIPT` overrides the
path). Three passes: types/lint/build for the languages in the diff, parallel
reviewers (code review, silent-failure hunter, test analysis, plus
TypeScript/Rust extras), then a docs pass. Findings are posted as a PR comment
and the verdict is recorded as the `machinist/review` commit status; the JSON
summary lands in `$MACHINIST_OUTPUT_DIR/post-pr-review.json` and `pr.json`
carries `review_status`.

Blocking findings (new check failures, critical/bug reviewer findings) send the
change back to the executor to amend on the same branch, then the checks and
reviewers rerun. Each amend round counts against the retry budget:
`--amend-rounds`, default `MACHINIST_REVIEW_AMEND_ROUNDS`, default 3. A review
that still has blocking findings and cannot amend them (review-only scope, or a
verified credential) exits `2` (BLOCKED) and `deliver-pr.py` propagates it, so
the run is not eligible for auto-merge.

MACH-61: the review also has a wall-clock budget so it cannot consume the whole
job timeout. `blocking.max_minutes` (env `MACHINIST_REVIEW_MAX_MINUTES`, CLI
`--max-minutes`, default 30) caps the whole review stage, and the budget is
capped again to the job's remaining time (`MACHINIST_JOB_TIMEOUT_SECONDS` /
`MACHINIST_JOB_STARTED_EPOCH`, set by the worker), reserving
`MACHINIST_REVIEW_DELIVERY_RESERVE_SECONDS` (default 300s) for delivery. The
budget is checked before every round and every fix pass; a pass that would not
fit is not started. When the budget runs out the verdict is INCOMPLETE with the
reason "review budget exhausted" (rounds, minutes or job time) — never PASS and
never a job failure — and `deliver-pr.py` still finishes delivery: it commits and
pushes any leftover fix edits (or stashes them with a labelled stash plus a
`.patch` in `$MACHINIST_OUTPUT_DIR`), keeps the PR open, and leaves the lane
clean.

MACH-58: the review sends the pull request diff to the review model, so built
and generated files are filtered out first. The profile's `diff.exclude` globs
(default: `web/dist/**`, minified JS/CSS, `*.map`, lockfiles, generated code)
replace the shipped default list, and `diff.max_tokens` (env
`MACHINIST_REVIEW_DIFF_MAX_TOKENS`, default 200000) is the token budget for one
review prompt. The review comment's Notes list every excluded file. If the
filtered diff is still over budget the review switches mode and says so: it
chunks the diff per file (each reviewer pass runs once per chunk) or summarises
the files too large to include; the JSON summary carries a `diff` object with
the mode, token counts and excluded/summarised paths. An over-budget diff is
never sent anyway and never fails silently.

The review also runs `trufflehog git` over the pull request range
(`--since-commit=<base> --branch=<head>`) as a built-in secret scan. A verified
live credential (`--results=verified`) is a blocking finding that fails
`machinist/review` and prevents auto-merge; unverified/unknown detections are
listed in the review comment but do not block. Secret values are never echoed,
only TruffleHog's redacted form. A missing `trufflehog` binary or a failed scan
makes the review INCOMPLETE (it never silently passes).

Per-project review configuration lives in the control plane
(`project_review_settings` table) and is edited per project in Settings as a
review profile (MACH-32): a library profile name, an inline YAML profile, and
optional extra instructions. The review resolves the profile in order: a
`.machinist/review.yaml` at the PR base ref, then the control-plane project
setting (`MACHINIST_CONTROL_PLANE_URL` with `MACHINIST_CONTROL_PLANE_TOKEN`,
exported by the managed worker), then the instance default profile
(`review.default_profile` in the config), then the shipped `default` profile
in `profiles/review/`. Legacy free-text instructions were migrated into each
project's profile as extra instructions.

