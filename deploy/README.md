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

`install.sh` is idempotent. Before it copies anything it checks the Machinist
binary: the path is `$MACHINIST_BIN` or `--binary <path>` (default
`/usr/local/bin/machinist`), it must exist and answer `machinist version`,
and `--install-binary <src>` copies a built binary there first; a binary
living outside `/usr/local/bin` is linked into it when running as root. It
then refuses to install from a tree that a gitleaks scan reports as
containing secrets — the scan runs from inside the repository with a relative
source, so its fingerprints match the relative ones in `.gitleaksignore`.

When `/etc/machinist` already holds configs (an existing factory), a
pre-flight loads every `config*.toml` / `worker*.toml` from its new location
and fails before any change unless every referenced `token_file`,
`worker_token_file`, `prompt_file` and `[repositories.*].path` exists there.
Relative paths resolve against the config file's directory, so a config
migrated from `~/.machinist` must be rewritten with absolute or `~/` paths
(what the relative form would miss is reported in the error).

It copies the factory scripts to `/usr/local/lib/machinist-factory/`, the
units to `/etc/systemd/system/`, the templates to `/etc/machinist/`, seeds
`config.toml`/`worker.toml` only when missing, runs `systemctl
daemon-reload`, restarts only the services whose unit files changed, and
records every managed file with its sha256 and the deployed git SHA in
`/etc/machinist/installed.json`.

Restart policy: non-worker units restart straight away when active. Worker
lanes drain by default — claims are paused with the factory control
(`machinist pause`, PR #54), each lane is restarted once it has no running
job, and claims are resumed again (also on error); a lane still busy when
`--drain-timeout` expires (default 600s) keeps running old code and is
reported as `PENDING`. `--restart-lanes=when-idle` restarts only currently
idle lanes, and `--no-restart` leaves every unit alone.

Drop-ins in a managed `<unit>.d/` directory that the repo does not ship are
reported as "local override (unmanaged drop-in)" so hand-made settings (for
example a `90-local.conf`) stay visible; they are never overwritten.

Overridable paths (for tests and unusual hosts): `MACHINIST_FACTORY_DIR`,
`MACHINIST_SYSTEMD_DIR`, `MACHINIST_CONFIG_DIR`, `MACHINIST_MANIFEST`,
`MACHINIST_BIN`, `MACHINIST_HOME` (the machinist user's home for `~/` path
resolution).

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
`/etc/machinist/machinist.env` (non-secret tooling values, including the
toolchain `PATH` for `gh`, `go`, `node` and version-manager shims — an
instance value, needed by the control plane and every worker); the tracked
templates are `env/plane.env.example` and `env/machinist.env.example`, with
placeholders only (`PLANE_API_KEY=changeme`). Copy a template to the real
name, fill it in, and keep it root-owned and mode 0600.

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
and the env files. It also reports, without failing, unmanaged drop-ins in
managed `<unit>.d/` directories ("local overrides") and running worker lanes
whose process started before the installed binary or unit file was updated
("stale workers" — a lane that missed an upgrade). On drift it writes a
health/status file (`~/.machinist/drift.json`) and a `machinist.drift` alert
listing the drifted files to the Factory Alerts spool
(`~/.machinist/alerts/pending/`), and exits 2. The
`machinist-drift-check.timer` runs it hourly.

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