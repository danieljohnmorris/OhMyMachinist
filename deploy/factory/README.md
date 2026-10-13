# Factory scripts

Trusted post-agent tooling that runs outside the Codex sandbox on pipsqueek.

## deliver-pr.py

Branch + commit + push + `gh pr create` delivery. The secret-file guard refuses
real env files (`.env`, `.env.local`, `prod.env`, …) but allows example/template
env files (`.env.example`, `.env.sample`, `.env.template`, `.env.dist`,
`.env.defaults`, `example.env`, `sample.env`, `*.env.example`, `*.env.sample`);
staged added lines of those templates are still scanned for real-looking
secrets (see `tests/test_deliver_pr.py`).

Install:

    sudo install -m755 deploy/factory/deliver-pr.py /usr/local/lib/machinist-factory/deliver-pr.py

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

Per-project review instructions live in the control plane (`review_instructions`
table) and are edited per project in Settings. The review fetches them from
`MACHINIST_CONTROL_PLANE_URL` with `MACHINIST_CONTROL_PLANE_TOKEN` (exported by
the managed worker), or reads `MACHINIST_REVIEW_INSTRUCTIONS_FILE` directly.
Projects without stored instructions use the seeded default (Daniel's routing
rules: `src/content` markdown goes to the write-as-dan prose pass, everything
else to the three code reviewers).

