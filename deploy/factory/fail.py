#!/usr/bin/env python3
"""Machinist failure hook + Plane notifier (trusted, root-owned; agents cannot edit).

  fail.py hook --rc N --stage S --stderr-file F [--brief B]
      Called by /usr/local/lib/machinist-factory/codex when a build-pr run fails
      (preflight / codex / deliver-pr / killed). Runs as machinist, NO network, NO keys.
      Writes ~/.machinist/alerts/inbox/<job_id>.json.
  fail.py notify
      Run by machinist-fail-notify.service (triggered by machinist-fail-notify.path on inbox/).
      Has PLANE_API_KEY via root-only EnvironmentFile. For each inbox alert: POST a comment
      on the linked Plane work item, optionally requeue (transient only, flag off by default),
      then move it to ~/.machinist/alerts/pending/ for Factory Alerts. It drains whatever is
      in the inbox when it starts, so an alert the path unit missed is still delivered; any
      file left behind for longer than FAIL_INBOX_WARN_S is logged as stale (MACH-62).
      An 'oom' alert (MACH-62 kernel OOM kill) is never requeued and its comment names the
      command and the job's peak memory.
  fail.py dry-run --run-id RUN
      Build the alert for an existing failed run from machinist.db and print it. No writes.
  fail.py sweep [--backfill-run RUN --to-pending]
      (2026-10-11) Run by machinist-fail-sweep.timer (every 1 min, User=machinist, no network).
      Machinist kills timed-out runs with SIGKILL to the whole process group (runner/process_unix.go),
      so the wrapper's TERM trap never fires and fail.py hook is never called. The sweep reads
      machinist.db for runs in state timed_out / failed that completed since the sweep was installed
      and have no alert yet, and writes alerts/inbox/<job_id>.json (stage "timeout", classification
      "timeout" for timed_out). notify then handles it exactly like a wrapper-written alert.
      --backfill-run RUN builds one alert for an older run; --to-pending writes it straight to
      alerts/pending/ with plane_comment "skipped: backfill" (no Plane comment, no requeue).
"""
import argparse, base64, glob, json, os, re, sqlite3, subprocess, sys, time, urllib.request

E = os.environ.get
HOME = E("HOME") or os.path.expanduser("~")
DB = E("MACHINIST_DB", f"{HOME}/.machinist/server/machinist.db")
JOBS_DIR = E("PLANE_JOBS_DIR", f"{HOME}/.machinist/plane-jobs")
ALERTS = E("FAIL_ALERTS_DIR", f"{HOME}/.machinist/alerts")
INBOX, PENDING, DONE = f"{ALERTS}/inbox", f"{ALERTS}/pending", f"{ALERTS}/done"
STATE = f"{ALERTS}/.state.json"
SWEEP_STATE = f"{ALERTS}/.sweep-state.json"
SWEEP_STATES = ("timed_out", "failed")  # cancelled = deliberate stop, never alerted
SWEEP_GRACE_S = int(E("FAIL_SWEEP_GRACE_S", "60"))  # let the wrapper hook win for ordinary fails
FAIL_INBOX_WARN_S = int(E("FAIL_INBOX_WARN_S", "300"))  # warn about inbox files older than this
PLANE_URL = E("PLANE_URL", "").rstrip("/")
WORKSPACE = E("PLANE_WORKSPACE", "")
COMMENT = E("FAIL_PLANE_COMMENT", "1") == "1"
REQUEUE_TRANSIENT = E("FAIL_REQUEUE_TRANSIENT", "0") == "1"
MACHINIST = E("MACHINIST_BIN", "/usr/local/bin/machinist")

PATTERNS = [  # first match wins; auth/allowlist are NEVER auto-requeued
    ("allowlist", re.compile(r"not in allowlist", re.I)),
    ("auth", re.compile(r"permission denied|\b40[13]\b|unauthori[sz]ed|forbidden|bad credentials|"
                        r"authentication failed|could not read username|write access|"
                        r"resource not accessible|gh auth login|publickey", re.I)),
    ("dirty-repo", re.compile(r"uncommitted changes", re.I)),
    ("timeout", re.compile(r"^machinist: (state=timed_out|command exceeded timeout)", re.I | re.M)),
    # MACH-62: a kernel OOM kill (cgroup memory.events oom_kill, the runner's own
    # "out of memory in <command>, peak N GB" text, or the kernel's report) is a
    # capacity problem, never a transient one, so it must not match below.
    ("oom", re.compile(r"out of memory|oom[_-]?kill|memory\.events|killed process", re.I)),
    ("transient", re.compile(r"timed? ?out|timeout|connection (reset|refused)|temporar|"
                             r"\b50[234]\b|rate limit|ECONNRESET|EOF occurred|killed by signal", re.I)),
]

def log(*a): print(time.strftime("%F %T"), *a, file=sys.stderr, flush=True)

def classify(text):
    for name, rx in PATTERNS:
        if rx.search(text): return name
    return "unknown"

def db_run(run_id):
    try:
        con = sqlite3.connect(f"file:{DB}?mode=ro", uri=True, timeout=10); con.row_factory = sqlite3.Row
        r = con.execute("select * from runs where id=?", (run_id,)).fetchone(); con.close(); return r
    except Exception as e:
        log("db read failed:", e); return None

def row_opt(r, key):
    """r[key] when the runs column exists, else None (older DBs predate MACH-62's
    error_class/peak_memory_bytes, and sqlite3.Row raises on a missing column)."""
    try: return r[key]
    except Exception: return None

def plane_meta(job_id, key=None):
    best = None
    for m in glob.glob(f"{JOBS_DIR}/*/meta.json"):
        try: meta = json.load(open(m))
        except Exception: continue
        meta["job_dir"] = os.path.dirname(m)
        if job_id and job_id in (meta.get("submit_out") or ""): return meta
        if key and meta.get("key") == key and (not best or meta.get("created", 0) > best.get("created", 0)):
            best = meta
    return best

def build_alert(run_id, rc, stage, tail, brief_text=""):
    r = db_run(run_id) if run_id else None
    job_id = r["job_id"] if r else None
    m = re.search(r"/browse/([A-Z][A-Z0-9]*-\d+)/", brief_text or "")
    meta = plane_meta(job_id, m and m.group(1))
    cls = classify("\n".join(tail))
    plane = None
    if meta:
        plane = {k: meta.get(k) for k in ("key", "issue_id", "project_id", "repo", "command", "job_dir")}
        plane["url"] = f"{PLANE_URL}/{WORKSPACE}/browse/{meta.get('key')}/" if PLANE_URL else ""
    return {
        "type": "machinist.run.failed", "detected_at": time.strftime("%FT%T%z"),
        "run_id": run_id, "job_id": job_id, "repository": E("MACHINIST_REPOSITORY") or (r and r["repository"]),
        "stage": stage, "exit_code": rc, "classification": cls,
        "auto_requeue_allowed": cls == "transient",
        # MACH-62: the runner records the OOM class and the job's peak in the run
        # row; None on older DBs that lack the columns.
        "command": row_opt(r, "command"),
        "error_class": row_opt(r, "error_class"),
        "peak_memory_bytes": row_opt(r, "peak_memory_bytes"),
        "stderr_tail": tail, "plane": plane,
        "inspect": f"sqlite3 -readonly {DB} \"select state,exit_code,error,result from runs where id='{run_id}'\"",
    }

def write_json(d, path):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = os.path.join(os.path.dirname(path), "." + os.path.basename(path) + ".tmp")
    json.dump(d, open(tmp, "w"), indent=1); os.replace(tmp, path)

def cmd_hook(a):
    try: tail = [l for l in open(a.stderr_file, errors="replace").read().splitlines() if l.strip()][-40:]
    except Exception: tail = []
    brief = ""
    try: brief = open(a.brief, errors="replace").read() if a.brief else ""
    except Exception: pass
    run_id = E("MACHINIST_RUN_ID") or f"norun-{int(time.time())}"
    alert = build_alert(run_id, a.rc, a.stage, tail, brief)
    write_json(alert, f"{INBOX}/{alert['job_id'] or run_id}.json")
    log(f"fail-hook: {alert['classification']} stage={a.stage} rc={a.rc} -> {INBOX}")

def oom_summary(alert):
    """'out of memory in <command>, peak N GB' -- the MACH-62 OOM wording, same as the
    runner's own failure text (runner/memory.go oomFailureMessage)."""
    cmd = (alert.get("plane") or {}).get("command") or alert.get("command") or "the job"
    text = f"out of memory in {cmd}"
    peak = alert.get("peak_memory_bytes")
    if peak:
        try: text += f", peak {int(peak) / (1024 ** 3):.1f} GB"
        except (TypeError, ValueError): pass
    return text

def plane_comment(alert):
    key = E("PLANE_API_KEY"); p = alert.get("plane")
    if not (COMMENT and key and PLANE_URL and WORKSPACE and p and p.get("issue_id")):
        return "skipped"
    last = "<br>".join(x.replace("&", "&amp;").replace("<", "&lt;") for x in alert["stderr_tail"][-8:])
    if alert["classification"] == "oom":
        # MACH-62: name the command and the peak so the ticket points at the job's
        # memory use; an OOM kill is a capacity problem, not a retryable one.
        what = oom_summary(alert)
        retry = ("Not auto-requeued: the job hit its memory cap; lower its memory use or raise the "
                 "worker [memory] limits (memory.high/max).")
    else:
        what = alert["classification"]
        retry = ("Transient: one automatic retry queued." if alert.get("requeue") else
                 "Not auto-requeued; needs a ticket/access fix, then re-label or re-submit.")
    html = (f"<p><b>Machinist run failed</b>: <b>{what}</b> at stage "
            f"<code>{alert['stage']}</code>, exit {alert['exit_code']}, job <code>{alert['job_id']}</code>.</p>"
            f"<pre>{last}</pre><p>{retry} Factory Alerts notified.</p>")
    url = f"{PLANE_URL}/api/v1/workspaces/{WORKSPACE}/projects/{p['project_id']}/issues/{p['issue_id']}/comments/"
    req = urllib.request.Request(url, data=json.dumps({"comment_html": html}).encode(), method="POST",
                                 headers={"X-API-Key": key, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r: return f"posted {r.status}"
    except Exception as e: return f"error: {e}"

def requeue(alert, st):
    p = alert.get("plane") or {}
    if not (REQUEUE_TRANSIENT and alert["classification"] == "transient" and p.get("job_dir")): return None
    if p.get("issue_id") in st.setdefault("requeued_issues", []): return "skipped: already retried once"
    prompt = open(f"{p['job_dir']}/prompt.md").read()
    r = subprocess.run([MACHINIST, "submit", "--command", p.get("command") or "build-pr", "--repo", p["repo"],
                        "--prompt", prompt], cwd=HOME, capture_output=True, text=True, timeout=60)
    st["requeued_issues"].append(p.get("issue_id"))
    return f"rc={r.returncode} {(r.stdout + r.stderr).strip()[-300:]}"

def inbox_stale(mtime, now, max_age):
    """Pure age check: an inbox file older than max_age seconds is stale (MACH-62)."""
    return mtime < now - max_age

def warn_stale_inbox():
    """Warn about alerts still sitting in the inbox: the path unit may not have fired
    (or a notify run died), and a stale alert means a ticket nobody commented on."""
    now = time.time()
    for f in sorted(glob.glob(f"{INBOX}/*.json")):
        try: mtime = os.path.getmtime(f)
        except OSError: continue
        if inbox_stale(mtime, now, FAIL_INBOX_WARN_S):
            log(f"notify: stale inbox alert {f} (older than {FAIL_INBOX_WARN_S}s); "
                f"machinist-fail-notify.path may not have run")

def cmd_notify(a):
    try: st = json.load(open(STATE))
    except Exception: st = {}
    # The glob below is the whole inbox, so the notifier drains whatever is queued when it
    # starts: a path unit that failed earlier (or a notify run that died mid-drain) loses
    # nothing, the next start picks the file up.
    for f in sorted(glob.glob(f"{INBOX}/*.json")):
        try: alert = json.load(open(f))
        except Exception as e: log("bad alert", f, e); continue
        alert["requeue"] = requeue(alert, st)
        alert["plane_comment"] = plane_comment(alert)
        write_json(alert, f"{PENDING}/{os.path.basename(f)}"); os.remove(f)
        log(f"notify: {os.path.basename(f)} class={alert['classification']} "
            f"plane={alert['plane_comment']} requeue={alert['requeue']}")
    write_json(st, STATE)
    warn_stale_inbox()

def run_stderr_tail(r):
    tail = []
    for line in (r["events"] or "").splitlines():
        try: ev = json.loads(line)
        except Exception: continue
        if ev.get("type") == "process.output" and ev.get("stream") == "stderr":
            d = ev.get("data", "")
            if ev.get("encoding") == "base64": d = base64.b64decode(d).decode("utf-8", "replace")
            tail += d.splitlines()
    return [l for l in tail if l.strip()][-40:]

def run_alert(r, stage=None):
    """Alert for a finished run straight from machinist.db (no wrapper involved)."""
    tail = run_stderr_tail(r)
    state, err = r["state"], (r["error"] or "")
    if state in ("timed_out", "failed"):  # make the cause visible + classifiable
        tail = (tail + [f"machinist: state={state} exit_code={r['exit_code']} {err}".rstrip()])[-40:]
    alert = build_alert(r["id"], r["exit_code"], stage or ("timeout" if state == "timed_out" else "run"),
                        tail, r["rendered_prompt"] or "")
    if not alert.get("repository"): alert["repository"] = r["repository"]
    alert.update({"run_state": state, "reason": state, "error": err, "source": "sweep",
                  "started_at": r["started_at"], "completed_at": r["completed_at"],
                  "duration_millis": r["duration_millis"], "worker_name": r["worker_name"],
                  "executor": r["executor"]})
    # MACH-62: re-read the OOM fields from the row the sweep just fetched (build_alert
    # already looked, this keeps the sweep's own row authoritative; None when absent).
    alert["error_class"] = row_opt(r, "error_class") or alert.get("error_class")
    alert["peak_memory_bytes"] = row_opt(r, "peak_memory_bytes") or alert.get("peak_memory_bytes")
    if not alert.get("command"): alert["command"] = row_opt(r, "command")
    if alert["plane"]: alert["plane_key"] = alert["plane"].get("key")
    return alert

def alert_exists(name):
    return any(os.path.exists(f"{d}/{name}") for d in (INBOX, PENDING, DONE))

def cmd_sweep(a):
    try: st = json.load(open(SWEEP_STATE))
    except Exception: st = {}
    seen = st.setdefault("runs", {})
    if a.backfill_run:
        r = db_run(a.backfill_run)
        if not r: sys.exit(f"run {a.backfill_run} not found")
        alert = run_alert(r); alert["source"] = "backfill"
        name = f"{alert['job_id'] or r['id']}.json"
        if alert_exists(name): sys.exit(f"alert {name} already exists; not backfilling")
        if a.to_pending:
            alert.update({"requeue": None, "plane_comment": "skipped: backfill"})
            write_json(alert, f"{PENDING}/{name}")
        else:
            write_json(alert, f"{INBOX}/{name}")
        seen[r["id"]] = {"alert": name, "at": time.strftime("%FT%T%z"), "backfill": True}
        write_json(st, SWEEP_STATE); log(f"sweep: backfilled {r['id']} -> {name}"); return
    if "since" not in st:  # first run: arm from now, never flood historical runs
        st["since"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        write_json(st, SWEEP_STATE); log("sweep: armed since", st["since"]); return
    cutoff = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - SWEEP_GRACE_S))
    try:
        con = sqlite3.connect(f"file:{DB}?mode=ro", uri=True, timeout=10); con.row_factory = sqlite3.Row
        rows = con.execute(f"select * from runs where state in ({','.join('?' * len(SWEEP_STATES))}) "
                           "and completed_at >= ? and completed_at <= ? order by completed_at",
                           (*SWEEP_STATES, st["since"], cutoff + "~")).fetchall(); con.close()
    except Exception as e:
        log("sweep: db read failed:", e); return
    n = 0
    for r in rows:
        if r["id"] in seen: continue
        name = f"{r['job_id'] or r['id']}.json"
        if alert_exists(name):  # wrapper hook already alerted this job
            seen[r["id"]] = {"alert": name, "at": time.strftime("%FT%T%z"), "by": "hook"}; continue
        alert = run_alert(r)
        write_json(alert, f"{INBOX}/{name}"); n += 1
        seen[r["id"]] = {"alert": name, "at": time.strftime("%FT%T%z"), "by": "sweep"}
        log(f"sweep: {r['state']} run={r['id']} job={r['job_id']} rc={r['exit_code']} -> {INBOX}/{name}")
    write_json(st, SWEEP_STATE)
    if n: log(f"sweep: wrote {n} alert(s)")

def cmd_dry(a):
    r = db_run(a.run_id)
    if not r: sys.exit(f"run {a.run_id} not found")
    alert = run_alert(r, "replay") if r["state"] in ("timed_out", "failed") else build_alert(
        a.run_id, r["exit_code"], "replay", run_stderr_tail(r))
    print(json.dumps(alert, indent=1))
    print(f"DRY: would write {INBOX}/{alert['job_id']}.json; plane comment="
          f"{'yes' if alert['plane'] else 'no ticket'}; requeue="
          f"{'eligible (flag ' + ('on' if REQUEUE_TRANSIENT else 'off') + ')' if alert['auto_requeue_allowed'] else 'never'}")

def main():
    ap = argparse.ArgumentParser(); sp = ap.add_subparsers(dest="cmd", required=True)
    h = sp.add_parser("hook"); h.add_argument("--rc", type=int, required=True); h.add_argument("--stage", required=True)
    h.add_argument("--stderr-file", required=True); h.add_argument("--brief")
    sp.add_parser("notify"); d = sp.add_parser("dry-run"); d.add_argument("--run-id", required=True)
    w = sp.add_parser("sweep"); w.add_argument("--backfill-run"); w.add_argument("--to-pending", action="store_true")
    a = ap.parse_args(); {"hook": cmd_hook, "notify": cmd_notify, "dry-run": cmd_dry, "sweep": cmd_sweep}[a.cmd](a)

# Guarded so the module can be imported (tests) without running the CLI.
if __name__ == "__main__":
    main()
