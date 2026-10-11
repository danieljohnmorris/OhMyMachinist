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
