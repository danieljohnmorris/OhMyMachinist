# Factory scripts

Trusted post-agent tooling that runs outside the Codex sandbox on pipsqueek.

## deliver-pr.py

Branch + commit + push + `gh pr create` delivery. The secret-file guard refuses
real env files (`.env`, `.env.local`, `prod.env`, …) but allows example/template
env files (`.env.example`, `.env.sample`, `.env.template`, `.env.dist`,
`.env.defaults`, `example.env`, `sample.env`, `*.env.example`, `*.env.sample`);
staged added lines of those templates are still scanned for real-looking
secrets (see `tests/test_deliver_pr.py`).

Install (MACH-31: the review must be installed next to `deliver-pr.py` so it
runs as a plain script, with the skills available to the machinist user):

    sudo install -m755 deploy/factory/deliver-pr.py deploy/factory/post_pr_review.py /usr/local/lib/machinist-factory/
    sudo cp -R deploy/factory/review /usr/local/lib/machinist-factory/review
    sudo -u machinist deploy/factory/install-review-skills.sh

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
`--amend-rounds`, default `MACHINIST_REVIEW_AMEND_ROUNDS`, default 3. A PR still
blocking after the budget is exhausted exits `2` (BLOCKED) and `deliver-pr.py`
propagates it, so the run is not eligible for auto-merge.

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

