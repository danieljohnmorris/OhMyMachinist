# Legacy Plane intake (retired)

This directory archives the stand-alone Plane intake that Machinist replaced,
kept for reference only. Nothing here is installed or built.

- `plane-intake.py` — the 553-line stdlib Python service, run as
  `plane-intake.py serve | poll | sync | file F`.
- `machinist-plane-intake.service` — the webhook listener
  (`100.70.205.115:7340`, Tailscale-only).
- `machinist-plane-poll.service` and `machinist-plane-poll.timer` — the two
  minute poll fallback.
- `machinist-plane-poll.service.d/webhook-replaces-poll.conf` — the drop-in
  that skipped the poll once a webhook had been verified.

## Replacement

Plane intake now lives in the Machinist control plane:

| Legacy | Replacement |
| --- | --- |
| `plane-intake.py serve` | `machinist start` (or `machinist plane serve`), `POST /api/v1/webhooks/issues` with `X-Plane-Signature` |
| `plane-intake.py poll` | scheduler poll loop, quiet while webhooks arrive (`machinist plane poll` runs one pass) |
| `plane-intake.py sync` | scheduler state-sync loop every 20s (`machinist plane sync` runs one pass) |
| `plane-intake.py file F` | `machinist plane file F` |
| `PLANE_*` environment | `[plane]` in `config.toml` and the editable `plane.*` settings |
| `plane-jobs/.seen.json`, `.state-sync.json`, `alerts/.plane-states.json` | `plane_intake`, `plane_state_sync`, `plane_alert_state` tables (imported on first start) |
| `plane-repos.json`, `plane-supervisors.json` | `plane.repositories`, `plane.identifiers`, `plane.supervisors` settings (imported on first start) |

Behaviour is covered by `internal/plane` (pure rules) and
`internal/controlplane/plane_intake.go` (webhook, poll, approve bridge, state
sync, alerts, shadow mode). See `docs/configuration.md` for the settings.

## Cutover

1. Shadow mode is on by default (`plane.shadow`). Machinist records every
   decision it would take — the job it would create, the state-sync patch and
   the alerts — in `plane_shadow` without acting. The comparison report is
   served at `GET /api/v1/plane/intake`, which lists the shadow decisions
   beside the script jobs imported from each `plane-jobs/*/meta.json`.
2. Point the Plane webhook at Machinist and let both run for at least a day
   with zero unexplained diffs.
3. Set `plane.shadow` to `0`, then stop and disable
   `machinist-plane-intake.service` and `machinist-plane-poll.timer`.
4. Delete `/home/machinist/.local/bin/plane-intake.py` and the three unit
   files; this directory is the archive.