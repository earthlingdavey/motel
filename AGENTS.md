# Notes for coding agents

## Testing locally

CI is the gate: every pull request that changes code runs every lane,
on x86 and arm64.
Locally, run only what covers your change, and let CI run the rest.
Everything runs in Docker through the Makefile; no host Go is needed.

- **Unit tests:** `make test PKGS=./notify/...` for the module you
  changed; `make test` for all of them (~1 min warm); one test by
  name, `make test PKGS=./notify/... RUN=TestName` (a `go test -run`
  pattern, as typed: a plain `$`, not make's `$$`).
- **Lint, before every push:** `make lint MODULES="notify"`, naming
  each module you touched. It runs both tag sets.
- **Integration:** `make test-integration` (~2.5 min) only when your
  change touches what those tests drive for real: systemd units,
  restic, sqlite3, the box applier.
- **e2e:** run the lane for the code you changed, and in it only the
  split or suite you need, named on make's command line as below or in
  the environment. `e2e/notify/run.sh` and `e2e/metrics/run.sh` list
  their splits. The Makefile lists the rest: `BACKUP_SUITES`, a script
  each in `e2e/backup/`, and the `e2e` target's suites, which run
  together.

| Lane | One part | A bare run |
|---|---|---|
| `make e2e-notify` | `make e2e-notify NOTIFY_SPLITS=core`; or `reload`, `heartbeat` | core and reload (~4 min). heartbeat is notify 15, motel's 5-minute hold (~9.5 min): name it only when you change the hold, `heartbeatHoldAfter` and the code that holds and resumes the heartbeat in `notify/channelfailed.go`. CI always runs it. |
| `make e2e-metrics` | `make e2e-metrics METRICS_SPLITS=view`; or `vmalert`, `store`, `rules` | all four (~5 min) |
| `make e2e-backup` | `make e2e-backup BACKUP_SUITES=setup`; or `backup`, `units`, `status`, `restore`, `check` | all six |
| `make e2e` | | the whole lane |

`e2e-notify` and `e2e-metrics` share the receiver: do not run them at
once on one host.
