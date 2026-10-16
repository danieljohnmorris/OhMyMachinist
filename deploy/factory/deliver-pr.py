#!/usr/bin/env python3
"""Trusted post-agent delivery: branch + commit + push (SSH) + gh pr create.
Runs outside the Codex sandbox. Output goes to stderr (Machinist job log).
Exit 0 = PR opened/reused, 0 with note = nothing to deliver, 3 = delivery failed.

Install (MACH-17): sudo install -m755 deploy/factory/deliver-pr.py /usr/local/lib/machinist-factory/deliver-pr.py

Fork mode (2026-10-10): if the repo has git config machinist.prBase=<remote> (e.g. "upstream"),
origin is OUR fork (push target) and <remote> is the third-party upstream (never pushed).
  machinist.upstreamPr=manual (default): push branch to fork, open a REVIEW PR inside the fork
     (fork main synced to upstream main first); log upstream compare URL. Upstream is not touched.
  machinist.upstreamPr=draft: also open a DRAFT PR fork:branch -> upstream (only after the fork owner opts in)."""
import argparse, fnmatch, json, os, re, subprocess, sys, time

def _owners(var):
    return set(filter(None, (value.strip() for value in os.environ.get(var, "").split(","))))

# Owners the delivery step may PUSH to. Instance values: set MACHINIST_ALLOWED_OWNERS
# (comma-separated) in the factory env file. Never in git.
ALLOWED_OWNERS = _owners("MACHINIST_ALLOWED_OWNERS")
# Third-party owners a PR may target from a fork; never pushed to.
UPSTREAM_PR_OWNERS = _owners("MACHINIST_UPSTREAM_PR_OWNERS")
# Owners whose branches use a plain title slug (no factory/, no KEY-).
PLAIN_BRANCH_OWNERS = _owners("MACHINIST_PLAIN_BRANCH_OWNERS")
# Commit identity for the factory; override per instance.
GIT_NAME = os.environ.get("MACHINIST_GIT_NAME", "Machinist Factory")
GIT_EMAIL = os.environ.get("MACHINIST_GIT_EMAIL", "machinist@localhost")
MAX_FILES = 300
SECRET_GLOBS = [".env", ".env.*", "*.env", "*.pem", "*.key", "*.p12", "*.pfx", "id_rsa*", "id_ed25519*",
                "id_ecdsa*", "*.keystore", "credentials*.json", ".npmrc", ".netrc", "*secret*"]
# Example/template env files pass the secret guard (checked before SECRET_GLOBS);
# their staged added lines are still scanned for real-looking secrets (defence in depth).
ENV_TEMPLATE_GLOBS = [".env.example", ".env.sample", ".env.template", ".env.dist", ".env.defaults",
                      "example.env", "sample.env", "*.env.example", "*.env.sample"]
NOHOOKS = ["-c", "core.hooksPath=/dev/null"]
# PR title/body rules (delivery contract; model PR: <owner>/<repo>#179):
#  - title: short plain description, no ticket-id prefix ("OMM-1:"), no colon chains
#  - body: one lead sentence + What it adds / What is not implemented / What was not tested / Tested
#  - no Machinist boilerplate, no pasted spec, no run ids
# The coder writes PR_FILE in the repo root (first line "# Title"); we read it, delete it, never commit it.
PR_FILE = ".machinist-pr.md"
PR_HEADINGS = ["What it adds", "What is not implemented", "What was not tested", "Tested"]
TICKET_PREFIX = re.compile(r"^\s*\[?[A-Z][A-Z0-9]+-\d+\]?\s*[:\-\u2013\u2014]\s*")
BOILERPLATE = re.compile(r"(?im)^.*(automated pr from machinist|not merged\. review before merging|machinist run run_|sandboxed codex).*\n?")

ENV_LINE_RE = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*)=(.*)$")
# Common real-secret token shapes (checked against the value only).
SECRET_VALUE_RE = re.compile(r"ghp_|gho_|github_pat_|sk-|xox[bp]-|AKIA|-----BEGIN")
# Key names whose non-empty, non-placeholder value is treated as a real secret.
SENSITIVE_KEY_RE = re.compile(r"TOKEN|SECRET|PASSWORD|API_KEY", re.I)


def is_env_template(path):
    base = os.path.basename(path).lower()
    return any(fnmatch.fnmatch(base, p) for p in ENV_TEMPLATE_GLOBS)


def is_placeholder_value(value):
    v = value.strip().strip("\"'")
    if not v:
        return True
    low = v.lower()
    if low in {"changeme", "change_me", "change-me"} or re.fullmatch(r"x+", low):
        return True
    if low.startswith("your") or (v.startswith("<") and v.endswith(">")):
        return True
    if re.fullmatch(r"\$\{[^}]*\}", v):
        return True
    return False


def template_diff_has_secret(diff_text):
    """True if the unified diff adds a non-comment KEY=value line holding a real-looking secret."""
    for line in diff_text.splitlines():
        if not line.startswith("+") or line.startswith("+++"):
            continue
        content = line[1:].strip()
        if not content or content.startswith("#"):
            continue
        m = ENV_LINE_RE.match(content)
        if not m:
            continue
        key, value = m.group(1), m.group(2)
        if is_placeholder_value(value):
            continue
        if SECRET_VALUE_RE.search(value) or SENSITIVE_KEY_RE.search(key):
            return True
    return False


def secret_like(files, diff_text_by_file=None):
    """Return the subset of files that look like secret files.

    diff_text_by_file maps path -> staged unified diff text (git diff --cached -U0);
    used to scan allowlisted env templates for real-looking secrets. Pure function
    so the guard is testable without git.
    """
    diff_text_by_file = diff_text_by_file or {}
    bad = []
    for f in files:
        base = os.path.basename(f).lower()
        if is_env_template(f):
            diff_text = diff_text_by_file.get(f)
            if diff_text and template_diff_has_secret(diff_text):
                bad.append(f)
        elif any(fnmatch.fnmatch(base, p) for p in SECRET_GLOBS):
            bad.append(f)
    return bad


def clean_title(t):
    t = TICKET_PREFIX.sub("", (t or "").strip().lstrip("#").strip()).strip().rstrip(".")
    if t.count(":") > 1:
        t = t.split(":")[-1].strip() or t
    return t[:72].strip()

def read_pr_file(repo):
    path = os.path.join(repo, PR_FILE)
    if not os.path.isfile(path): return None, None
    try: text = open(path, errors="replace").read()
    finally: os.unlink(path)
    lines = text.strip().splitlines()
    if not lines: return None, None
    title = clean_title(lines[0]) if lines[0].lstrip().startswith("#") else None
    body = BOILERPLATE.sub("", "\n".join(lines[1:] if title else lines)).strip()
    return title, (body or None)

def fallback_body(title, files):
    adds = "\n".join(f"- `{f}`" for f in files[:40]) + ("\n- ..." if len(files) > 40 else "")
    return (f"{title}.\n\n**What it adds**\n{adds}\n\n**What is not implemented**\n- Not reported.\n\n"
            f"**What was not tested**\n- Not reported.\n\n**Tested**\n- Not reported.\n")

def log(*a): print("deliver-pr:", *a, file=sys.stderr, flush=True)

REVIEW_VERDICTS = {0: "PASS", 2: "BLOCKED", 3: "INCOMPLETE", 4: "ERROR"}
# MACH-63: every verdict publishes a machinist/review status; PASS=success,
# BLOCKED=failure, INCOMPLETE/ERROR=error (the reason goes in the description).
REVIEW_STATUS_STATES = {"PASS": "success", "BLOCKED": "failure", "INCOMPLETE": "error", "ERROR": "error"}

def review_verdict(json_path, returncode):
    """Resolve the built-in review verdict (MACH-63).

    The JSON verdict (``status``/``reason`` written by post_pr_review.py) wins;
    the exit code is the fallback. Only an explicit PASS is a pass: a missing or
    unreadable verdict, an unknown exit code, INCOMPLETE and ERROR all stay
    non-PASS, so a review that did not run is never reported as PASS. Returns a
    dict with status, reason, exit_code, passed and failure (BLOCKED fails the
    delivery job; INCOMPLETE/ERROR do not, MACH-31)."""
    status = None
    reason = ""
    if json_path and os.path.isfile(json_path):
        try:
            with open(json_path, errors="replace") as handle:
                payload = json.load(handle) or {}
            candidate = str(payload.get("status") or "").strip().upper()
            reason = str(payload.get("reason") or "").strip()
            if candidate in REVIEW_STATUS_STATES:
                status = candidate
            else:
                status = "ERROR"
                reason = reason or f"review wrote an unknown verdict: {candidate or '(none)'}"
        except (OSError, ValueError, AttributeError) as error:
            status = "ERROR"
            reason = f"cannot read the review verdict: {error}"
    elif json_path:
        # A JSON verdict was requested but not written. Fall back to the exit
        # code, but 0 is not a pass without the verdict file (MACH-63).
        mapped = REVIEW_VERDICTS.get(returncode)
        status = mapped if mapped and mapped != "PASS" else "ERROR"
        reason = f"review wrote no verdict (exit code {returncode})"
    if status is None:
        mapped = REVIEW_VERDICTS.get(returncode)
        status = mapped if mapped else "ERROR"
        if mapped is None:
            reason = f"unknown review exit code {returncode}"
    return {
        "status": status,
        "reason": reason,
        "exit_code": returncode,
        "passed": status == "PASS",
        "failure": status == "BLOCKED",
    }

def publish_review_status(full, pr, status, reason, repo):
    """Publish the machinist/review commit status on the PR head (MACH-63).

    post_pr_review.py posts its own status when it runs; this publishes the
    verdict again from the delivery side so the PR never shows a mismatch: a
    review that produced no usable verdict (bad arguments, a crash, a missing
    JSON file, an unknown exit code) still gets a non-PASS status posted here.
    Never raises: a status we cannot post is only a warning."""
    if not pr:
        return
    state = REVIEW_STATUS_STATES.get(status, "error")
    description = (f"Machinist review {status}" + (f": {reason}" if reason else ""))[:140]
    sha = run("gh", "pr", "view", pr, "--json", "headRefOid", "--jq", ".headRefOid", check=False, cwd=repo)
    if not sha:
        log("warn: could not resolve the PR head to publish the review status")
        return
    r = subprocess.run(
        ["gh", "api", f"repos/{full}/statuses/{sha}", "-f", f"state={state}",
         "-f", "context=machinist/review", "-f", f"description={description}"],
        cwd=repo, capture_output=True, text=True,
    )
    if r.returncode != 0:
        log(f"warn: could not publish the review status: {(r.stderr or r.stdout).strip()[-300:]}")

def run_review(repo, pr_url):
    """Run the built-in review (types/lint/build, parallel reviewers, docs) and
    return its verdict.

    Returns a dict from review_verdict(), or None when the review script is not
    installed (nothing ran). Only an explicit PASS is a pass; the delivery job
    fails on BLOCKED only — INCOMPLETE and ERROR never fail it (MACH-31) but are
    never reported as PASS either (MACH-63)."""
    script = review_script_path()
    if not os.path.isfile(script):
        log(f"review skipped: {script} not found")
        return None
    command = [sys.executable, script, "--repo", repo, "--pr", pr_url]
    executor = review_executor_cmd()
    if executor:
        command += ["--executor-cmd", executor]
    else:
        log("no review executor available (set MACHINIST_REVIEW_EXECUTOR); reviewers will be skipped")
    out = os.environ.get("MACHINIST_OUTPUT_DIR")
    json_path = None
    if out:
        json_path = os.path.join(out, "post-pr-review.json")
        command += ["--json-out", json_path]
    log("built-in review starting")
    completed = subprocess.run(command, cwd=repo)
    verdict = review_verdict(json_path, completed.returncode)
    detail = f": {verdict['reason']}" if verdict["reason"] else ""
    log(f"built-in review verdict: {verdict['status']}{detail}")
    if verdict["status"] != "PASS":
        log(f"review {verdict['status']}: the delivery job continues for INCOMPLETE/ERROR and fails only for BLOCKED")
    return verdict

def review_executor_cmd():
    """Executor for the built-in review (MACH-31): MACHINIST_REVIEW_EXECUTOR,
    else omp-glm next to this script (the same sandboxed GLM coder the wrapper
    uses), else None (the review logs it and skips reviewers)."""
    env = os.environ.get("MACHINIST_REVIEW_EXECUTOR", "").strip()
    if env:
        return env
    omp = os.path.join(os.path.dirname(os.path.abspath(__file__)), "omp-glm")
    if os.access(omp, os.X_OK):
        return f"{omp} medium"
    return None

def review_script_path():
    """The built-in review step (MACH-26) lives next to this script once
    installed; MACHINIST_REVIEW_SCRIPT overrides the location."""
    return os.environ.get("MACHINIST_REVIEW_SCRIPT") or os.path.join(
        os.path.dirname(os.path.abspath(__file__)), "post_pr_review.py"
    )

def review_status(out_dir):
    if not out_dir:
        return None
    try:
        return json.load(open(os.path.join(out_dir, "post-pr-review.json"))).get("status")
    except (OSError, ValueError, AttributeError):
        return None

def _commit_or_stash_review_edits(repo, branch, run_id):
    """A review that stopped (budget exhausted, timeout, crash) may leave its
    last fix edits uncommitted in the lane. Commit and push them; if that is
    not possible (secret-like files, no remote), stash them under a labelled
    stash and write a patch into the run artifacts, MACH-61."""
    g = lambda *c, **k: run("git", *NOHOOKS, *c, cwd=repo, **k)
    if not g("status", "--porcelain", check=False):
        return
    label = f"machinist-review-budget-{run_id or 'manual'}"
    before = g("rev-parse", "HEAD", check=False).strip()
    try:
        run("git", *NOHOOKS, "add", "-A", cwd=repo)
        files = [f for f in g("diff", "--cached", "--name-only").splitlines() if f]
        bad = secret_like(files)
        if bad:
            raise RuntimeError(f"secret-like files {bad}")
        run("git", *NOHOOKS, "-c", f"user.name={GIT_NAME}",
            "-c", f"user.email={GIT_EMAIL}",
            "commit", "--no-verify", "-q", "-m", f"Review fixes ({label})", cwd=repo)
        run("git", *NOHOOKS, "push", "--no-verify", "origin", f"HEAD:refs/heads/{branch}", cwd=repo)
        log(f"review lane: committed and pushed leftover review fixes ({label})")
        return
    except RuntimeError as error:
        # Undo the local commit if one landed so the edits can be stashed whole.
        if before:
            run("git", *NOHOOKS, "reset", "--soft", before, cwd=repo, check=False)
        else:
            run("git", *NOHOOKS, "reset", "-q", cwd=repo, check=False)
        log(f"review lane: could not commit+push leftover edits ({error}); stashing")
    g("stash", "push", "-u", "-m", label, check=False)
    out = os.environ.get("MACHINIST_OUTPUT_DIR")
    if out:
        patch = os.path.join(out, f"{label}.patch")
        try:
            text = g("stash", "show", "-p", "--include-untracked", "stash@{0}", check=False)
            os.makedirs(out, exist_ok=True)
            with open(patch, "w") as handle:
                handle.write(text)
            log(f"review lane: stashed leftover review fixes ({label}); patch at {patch}")
        except OSError as io_error:
            log(f"review lane: could not write patch {patch}: {io_error}")
    else:
        log(f"review lane: stashed leftover review fixes ({label})")

def finalize_after_review(repo, branch, pr, run_id):
    """Whatever verdict the review returned, delivery must still finish: leave
    the lane clean (commit+push or stash any leftover edits) and the PR open
    (reopen if it went missing), MACH-61."""
    _commit_or_stash_review_edits(repo, branch, run_id)
    if pr:
        state = run("gh", "pr", "view", pr, "--json", "state", "--jq", ".state", check=False)
        if state.strip().upper() != "OPEN":
            run("gh", "pr", "reopen", pr, check=False)

def run(*cmd, check=True, cwd=None):
    r = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True)
    if check and r.returncode != 0:
        raise RuntimeError(f"{' '.join(cmd[:4])}... failed ({r.returncode}): {(r.stderr or r.stdout).strip()[-800:]}")
    return r.stdout.strip()

def slug(s, n=40):
    s = re.sub(r"[^a-z0-9]+", "-", s.lower()).strip("-")
    return s[:n].strip("-") or "change"

def wip_scratch_dir():
    """Where a saved lane-WIP patch goes: never the published run artifacts.

    MACHINIST_OUTPUT_DIR is the run's published task folder (uploaded and shown
    to the user), and the stash path deliberately covers untracked files a
    commit refuses (an untracked .env, say), so a patch there would publish the
    very secret the stash exists to keep out. Prefer the unpublishable scratch
    dir, then the Machinist data dir.
    """
    scratch = os.environ.get("MACHINIST_SCRATCH_DIR", "").strip()
    if scratch:
        return scratch
    data_dir = os.environ.get("MACHINIST_DATA_DIR", "").strip()
    base = data_dir or os.path.join(os.path.expanduser("~"), ".machinist")
    return os.path.join(base, "review-wip")

def save_wip(repo):
    """Stash a dirty lane so the final branch switch cannot fail (MACH-59).

    The built-in review runs in this checkout; if it leaves edits behind the
    switch at the end of delivery would fail and strand the job. Stash them
    with a labelled stash plus a patch outside the published run artifacts,
    then switch on a clean tree. Never raises: a delivery must not fail because
    of this.
    """
    def git(*c):
        return subprocess.run(["git", *NOHOOKS, *c], cwd=repo, capture_output=True, text=True)
    try:
        status = git("status", "--porcelain")
        if status.returncode != 0:
            log(f"warn: could not read lane status: {(status.stderr or status.stdout).strip()}")
            return
        if not status.stdout.strip():
            return
        label = "machinist-deliver-wip-" + time.strftime("%Y%m%d-%H%M%S")
        stash = git("stash", "push", "--include-untracked", "-m", label)
        if stash.returncode != 0:
            # Do not read stash@{0}: a failed push leaves any pre-existing stash
            # at the top, and labelling its patch with this run's id would claim
            # we saved WIP we did not.
            log(f"warn: could not stash lane WIP: {(stash.stderr or stash.stdout).strip()}")
            return
        patch = git("stash", "show", "--patch", "--include-untracked", "stash@{0}").stdout.strip()
        out = wip_scratch_dir()
        if patch:
            os.makedirs(out, exist_ok=True)
            path = os.path.join(out, label + ".patch")
            with open(path, "w") as handle:
                handle.write(patch + "\n")
            log(f"saved lane WIP to stash {label} and {path}")
        else:
            log(f"saved lane WIP to stash {label}")
    except OSError as e:
        log(f"warn: could not save lane WIP: {e}")

def gh_repo(url):
    m = re.search(r"github\.com[:/]([^/]+)/([^/]+?)(?:\.git)?$", url)
    if not m: raise RuntimeError(f"not a GitHub repo: {url}")
    return m.group(1), m.group(2)

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", required=True); ap.add_argument("--brief", required=True)
    ap.add_argument("--start-branch", default="")
    a = ap.parse_args()
    repo = a.repo
    g = lambda *c, **k: run("git", *NOHOOKS, *c, cwd=repo, **k)
    brief = open(a.brief, errors="replace").read()

    owner, name = gh_repo(g("remote", "get-url", "origin"))
    if owner not in ALLOWED_OWNERS: raise RuntimeError(f"push owner {owner} not in allowlist")
    full = f"{owner}/{name}"
    try: base = g("symbolic-ref", "--short", "refs/remotes/origin/HEAD").split("/", 1)[1]
    except RuntimeError: base = "main"
    up_remote = run("git", "config", "machinist.prBase", cwd=repo, check=False)
    up_mode = run("git", "config", "machinist.upstreamPr", cwd=repo, check=False) or "manual"
    up_full = up_base = None
    if up_remote:
        uo, un = gh_repo(g("remote", "get-url", up_remote))
        if uo not in UPSTREAM_PR_OWNERS | ALLOWED_OWNERS: raise RuntimeError(f"upstream owner {uo} not allowed")
        if uo == owner: raise RuntimeError("prBase remote equals origin; fork mode misconfigured")
        up_full = f"{uo}/{un}"
        try: up_base = g("symbolic-ref", "--short", f"refs/remotes/{up_remote}/HEAD").split("/", 1)[1]
        except RuntimeError: up_base = "main"
        log(f"fork mode: push -> {full}, upstream {up_full} ({up_mode})")

    pr_file_title, pr_file_body = read_pr_file(repo)
    if not g("status", "--porcelain"):
        log("no changes from agent; nothing to deliver"); return 0

    sm = re.search(r"--- SPEC: (\S+) (.*?) ---", brief)
    key, title = (sm.group(1), sm.group(2).strip()) if sm else ("", "")
    if not title:
        title = next((l.strip() for l in brief.splitlines() if l.strip()), "Machinist change")[:72]
    run_id = os.environ.get("MACHINIST_RUN_ID", "manual")
    pr_title = pr_file_title or clean_title(title) or "Machinist change"
    if PLAIN_BRANCH_OWNERS & {owner, (up_full or "").split("/")[0]}:
        # Plain-branch owners: plain kebab-case from the PR title; no factory/, no ticket key, no run id
        base_b = slug(pr_title, 50)
        branch, n = base_b, 2
        while (run("git", "rev-parse", "--verify", "-q", f"refs/heads/{branch}", cwd=repo, check=False)
               or run("git", "ls-remote", "--heads", "origin", branch, cwd=repo, check=False)):
            branch, n = f"{base_b}-{n}", n + 1
    else:
        branch = f"factory/{(key + '-') if key else ''}{slug(title)}-{run_id[-6:]}"

    cur = g("rev-parse", "--abbrev-ref", "HEAD")
    if cur != branch: g("switch", "-c", branch)
    g("add", "-A")
    run("git", *NOHOOKS, "rm", "-q", "--cached", "--ignore-unmatch", PR_FILE, cwd=repo, check=False)
    files = [f for f in g("diff", "--cached", "--name-only").splitlines() if f]
    diff_by_file = {f: g("diff", "--cached", "-U0", "--", f) for f in files if is_env_template(f)}
    bad = secret_like(files, diff_by_file)
    if bad or len(files) > MAX_FILES:
        g("reset", "-q")
        raise RuntimeError(f"refusing to commit: {'secret-like files ' + str(bad) if bad else str(len(files)) + ' files > ' + str(MAX_FILES)}; left on branch {branch}")
    msg = f"{pr_title}\n\nMachinist run {run_id}" + (f"\nRefs {key}" if key else "")
    env_id = ["-c", "user.name=" + (run("git", "config", "user.name", cwd=repo, check=False) or GIT_NAME),
              "-c", "user.email=" + (run("git", "config", "user.email", cwd=repo, check=False) or GIT_EMAIL)]
    run("git", *NOHOOKS, *env_id, "commit", "--no-verify", "-q", "-m", msg, cwd=repo)
    sha = g("rev-parse", "--short", "HEAD")
    if os.environ.get("DELIVER_PR_DRY") == "1":
        log(f"DRY: would push {branch} @ {sha} to {full} and open PR base={base} upstream={up_full} mode={up_mode} files={files}"); return 0
    g("push", "--no-verify", "-u", "origin", branch)
    log(f"pushed {branch} @ {sha} to {full}")

    spec = re.search(r"--- SPEC:.*?---\n(.*?)--- END SPEC ---", brief, re.S)
    pr_body = pr_file_body or fallback_body(pr_title, files)
    if not pr_file_body: log(f"warn: coder wrote no {PR_FILE}; using minimal fallback body")
    def body(note):
        return pr_body  # rules: no boilerplate/notes in the PR body; notes go to the job log
    def open_pr(repo_full, pr_base, head, note, extra=()):
        existing = run("gh", "pr", "list", "--repo", repo_full, "--head", head.split(":")[-1], "--state", "open",
                       "--json", "url", "--jq", ".[0].url", check=False)
        if existing: return existing
        r = subprocess.run(["gh", "pr", "create", "--repo", repo_full, "--base", pr_base, "--head", head,
                            "--title", pr_title, "--body", body(note), *extra], cwd=repo, capture_output=True, text=True)
        if r.returncode != 0:
            raise RuntimeError(f"branch {branch} pushed but gh pr create on {repo_full} failed: {r.stderr.strip()[-500:]}. "
                               f"Open manually: https://github.com/{repo_full}/compare/{pr_base}...{head}?expand=1")
        return r.stdout.strip().splitlines()[-1]

    result = {"branch": branch, "commit": sha}

    def finish(rc):
        out = os.environ.get("MACHINIST_OUTPUT_DIR")
        if out:
            # run_review already recorded the verdict (status/reason); fall back
            # to reading the JSON only when no review ran at all.
            if "review_status" not in result:
                result["review_status"] = review_status(out)
            json.dump(result, open(os.path.join(out, "pr.json"), "w"))
        if a.start_branch and a.start_branch != "HEAD":
            save_wip(repo)
            g("switch", "-q", a.start_branch)
        return rc

    def handle_review(pr):
        """Run the review, publish its status, record the verdict (MACH-63).

        Returns the delivery exit code when BLOCKED (0 otherwise): only BLOCKED
        fails the delivery; INCOMPLETE and ERROR are recorded and never a PASS."""
        verdict = run_review(repo, pr)
        if verdict is not None:
            result["review_status"] = verdict["status"]
            if verdict["reason"]:
                result["review_reason"] = verdict["reason"]
            publish_review_status(full, pr, verdict["status"], verdict["reason"], repo)
            if verdict["status"] in ("INCOMPLETE", "ERROR"):
                # Recorded in the run events (the job result) as well as pr.json.
                log(f"review {verdict['status']}: {verdict['reason']}")
        if verdict is not None and verdict["failure"]:
            return 2
        return 0

    if up_full:
        up_compare = f"https://github.com/{up_full}/compare/{up_base}...{owner}:{branch}?expand=1"
        result["upstream_compare"] = up_compare
        # keep fork base == upstream base so the review diff is just our change (ff-only; never force)
        r = subprocess.run(["git", *NOHOOKS, "push", "--no-verify", "origin", f"refs/remotes/{up_remote}/{up_base}:refs/heads/{base}"],
                           cwd=repo, capture_output=True, text=True)
        if r.returncode != 0: log(f"warn: could not sync fork {base} to {up_remote}/{up_base}: {r.stderr.strip()[-200:]}")
        pr = open_pr(full, base, branch,
                     f"Review PR inside the fork. NOT sent upstream.\nAfter the fork owner approves, open upstream PR: {up_compare}")
        result["pr"] = pr
        result["review_pr"] = pr
        review_rc = handle_review(pr)
        finalize_after_review(repo, branch, pr, run_id)
        if review_rc:
            log("review did not pass; upstream PR not opened")
            return finish(review_rc)
        if up_mode == "draft":
            result["upstream_pr"] = open_pr(up_full, up_base, f"{owner}:{branch}",
                                            "Draft PR from fork. Not ready to merge.", extra=("--draft",))
            log(f"upstream draft PR: {result['upstream_pr']}")
        else:
            log(f"upstream NOT touched; after approval: gh pr create --repo {up_full} --base {up_base} --head {owner}:{branch}")
            log(f"upstream compare: {up_compare}")
    else:
        pr = open_pr(full, base, branch, "Not merged. Review before merging.")
        result["pr"] = pr
        review_rc = handle_review(pr)
        finalize_after_review(repo, branch, pr, run_id)
        if review_rc:
            return finish(review_rc)
    log(f"PR: {pr}")
    return finish(0)

if __name__ == "__main__":
    try: sys.exit(main())
    except Exception as e:
        log("FAILED:", e); sys.exit(3)
