# Self-hosting the factory

Everything the Machinist factory needs to run on a host lives here, so nothing
installed is hand-edited. Instance values (hosts, paths, projects, agents) and
secrets stay outside git.

```
deploy/
  install.sh          idempotent install/upgrade (also used by auto-deploy)
  drift-check.sh      diff installed files against the repo at the deployed SHA
  config/             generic config.toml / worker.toml templates
  env/                *.env.example templates for the systemd EnvironmentFiles
  systemd/            units and drop-in overrides
  factory/            trusted factory scripts (deliver-pr.py, review, wrappers, …)
  tests/              installer and drift-check tests
```

## Install and upgrade

```sh
sudo deploy/install.sh            # add --dry-run to preview
```

`install.sh` is idempotent. It refuses to install from a tree that a gitleaks
scan reports as containing secrets, copies the factory scripts to
`/usr/local/lib/machinist-factory/`, the units to `/etc/systemd/system/`, the
templates to `/etc/machinist/`, seeds `config.toml`/`worker.toml` only when
missing, runs `systemctl daemon-reload`, restarts only the services whose unit
files changed, and records every managed file with its sha256 and the deployed
git SHA in `/etc/machinist/installed.json`.

## Alert spool access for the watcher (MACH-72)

The alert watcher runs as a separate local user and reads and moves spooled
alerts (`fail-*`, `healing-*`, `health-*`, plane/PR/automerge notices) out of
`~/.machinist/alerts/pending` without root:

- **`[alerts] file_mode`** (default `"0640"`): the mode every spooled alert
  file is created with. World-writable and non-octal values are rejected at
  config load; the mode is applied explicitly so the process umask cannot
  narrow the group bits.
- **`[alerts] group`** (default `"machinist-alerts"`): `install.sh` creates the
  group if missing, adds the service user and any `[alerts] readers` to it,
  and creates the alerts directory plus its `pending` and `done`
  subdirectories as `service-user:group`, mode `2770` (setgid), with default
  ACLs giving the group `rwx`. Readers who must traverse the service user's
  home get only `--x` by ACL on the parent directories — never a world-wide
  chmod.
- **`/etc/tmpfiles.d/machinist-alerts.conf`**: `d` and `a+` rules written by
  `install.sh` (and recorded in the install manifest) so the setgid
  directories and default ACLs survive reboots; `install.sh` runs
  `systemd-tmpfiles --create` on it. The settings are read from
  `config.toml` / `config.local.toml`; the conf is rewritten on every
  install.
- **Stopgap retirement**: `install.sh` disables and removes the old root
  `machinist-alerts-perms.path`/`.service` chmod watcher; the drift check
  flags `alerts_access` drift if they are still installed.

`drift-check.sh` reports an `alerts_access` section: the group exists, the
spool directories have the expected owner, group, mode and setgid bit, the
default ACLs are present, the tmpfiles rule is installed, recent alert files
are group-readable per `file_mode`, and the stopgap units are gone. Any
mismatch is drift (exit 2, `machinist.drift` alert).

Overridable paths (for tests and unusual hosts): `MACHINIST_FACTORY_DIR`,
`MACHINIST_SYSTEMD_DIR`, `MACHINIST_CONFIG_DIR`, `MACHINIST_MANIFEST`,
`MACHINIST_SERVICE_USER`, `MACHINIST_SERVICE_HOME`, `MACHINIST_TMPFILES_DIR`.
`MACHINIST_ALERTS_PROVISION=1` runs the spool provisioning in privileged
contexts that are not uid 0 (containers); a normal `sudo deploy/install.sh`
does not need it.

The normal deploy and the MACH-24 auto-deploy both call `deploy/install.sh`, so
no file on the host is hand-edited.

## Config and the local overlay

`config.toml.example` and `worker.toml.example` are generic templates with no
instance values. When you set `--config /etc/machinist/config.toml`, Machinist
merges an instance overlay on top at load time:

1. the base file (`/etc/machinist/config.toml`, tracked template seeded once);
2. the overlay (`/etc/machinist/config.local.toml`, **not** in git), merged
   second and winning.

Tables merge key by key; every other value (including arrays) is replaced by the
overlay. The overlay is `$MACHINIST_CONFIG_OVERLAY` when set, else the sibling
`config.local.toml` next to the base file when it exists; worker.toml uses
`worker.local.toml` / `$MACHINIST_WORKER_CONFIG_OVERLAY`. Both overlays are
matched by `.gitignore` and are never read or diffed by the drift check.

## Secrets

Secrets come only from the environment or a systemd `EnvironmentFile` outside
git. The units read `/etc/machinist/plane.env` (Plane API key) and
`/etc/machinist/machinist.env` (non-secret tooling values); the tracked templates
are `env/plane.env.example` and `env/machinist.env.example`, with placeholders
only (`PLANE_API_KEY=changeme`). Copy a template to the real name, fill it in,
and keep it root-owned and mode 0600.

The installer runs gitleaks before copying anything and refuses to install from a
tree with findings; CI runs gitleaks over the working tree, the full git history
and `deploy/`, plus a TruffleHog verified-credential scan.

## Units

| Unit | Purpose |
| --- | --- |
| `machinist-control.service` | control plane (`machinist start --config /etc/machinist/config.toml`) |
| `machinist-worker.service` | single worker (`--config /etc/machinist/worker.toml`) |
| `machinist-worker@.service` | worker lane `%i` (`--config /etc/machinist/worker-%i.toml`) |
| `machinist-drift-check.{service,timer}` | periodic drift check |

Drop-ins under `<unit>.d/` carry the memory caps, the dirty-lane guard and the memory caps. Create `worker-<lane>.toml` (a copy of
`worker.toml` with a different name/data-directory) before enabling a lane.

## Drift check

```sh
/usr/local/lib/machinist-factory/drift-check.sh
```

It hashes every file in `/etc/machinist/installed.json` and compares it with the
repository at the deployed SHA (plus the recorded sha256), ignoring the overlay
and the env files. On drift it writes a health/status file
(`~/.machinist/drift.json`) and a `machinist.drift` alert listing the drifted
files to the Factory Alerts spool (`~/.machinist/alerts/pending/`), and exits 2.
The `machinist-drift-check.timer` runs it hourly. Both it and `install.sh`
warn when the server config has no `[alerts]` section (MACH-70): failed runs
then alert nowhere — no spool file, no webhook.

## Delivery reuse across restarts and retries (MACH-71)

`deliver-pr.py` derives the branch from the job's SPEC line, so a redelivered
run or a self-heal retry can find work that is already delivered. Before the
agent starts, `omp-glm-pr` looks up a branch already pushed for the job
(`deliver-pr.py --find-branch`) and checks it out; at delivery the script
reuses that branch and its open PR instead of opening a second one. If a push
to the job's own branch is rejected as non-fast-forward (another attempt got
there first), the script fetches, rebases onto the remote branch and pushes
again — it never fails the run on that rejection. When there is nothing new
to deliver and the job's PR is already open, the run finishes as already
delivered with `pr.json` linking the PR. Self-healing classifies a push
rejection on the job's own branch as a stale checkout (fetch and rebase),
never as infra, so it cannot schedule a from-scratch infra retry.

## Optional external dependencies

These are host-specific and deliberately not managed here:

- **A UI forwarder** (for example a Tailscale `socat` unit) if the UI must be
  reachable beyond loopback. The control plane binds `127.0.0.1:7331`.
- **A model shim** (for example a GLM/OpenAI-compatible bridge) if the executor
  targets a local model server.
- **omp/Spark hooks** such as a thermal gate or a busy gate in the host's omp
  configuration; they are generic omp tooling, not Machinist-specific.
- The retired `plane-intake.py` poll/webhook units (MACH-30 moves intake into
  Machinist's built-in intake).