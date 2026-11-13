# liveswap

**Your reverse proxy is your deploy pipeline.** liveswap turns a
single Caddy server into a zero-downtime deploy orchestrator for the
apps it fronts — Node.js, Go, anything that can listen on a unix
socket. CI builds a tarball, POSTs a webhook with its URL, and Caddy
does the rest: download, migrate, start the new version on its own
socket, health-gate it, atomically cut traffic over, gracefully stop
the old one. No Kubernetes, no Nomad, no SSH keys in CI, no extra daemons.
One binary. Part of [motel](https://github.com/smallhoursorg/motel), from [smallhours](https://github.com/smallhoursorg).

```
git push → CI builds app.tar.gz → uploads it → curl webhook → Caddy hot-swaps it
```

## How a deploy works

```
POST /blog {url, version}
  │ downloading   stream the artifact (size-capped, https, token-gated)
  │ extracting    hardened tar extraction into releases/<version>/
  │ preparing     pre-flight (command and pre_start resolve inside the sandbox
  │               view, built for this machine — else 422), then pre_start
  │               (migrations) — non-zero exit aborts
  │ starting      spawn the app on a fresh unix socket, SOCKET injected
  │ soaking       GET /health until continuously healthy for `soak`
  │ promoting     atomic cutover — new requests hit the new version
  │ draining      wait `drain` for in-flight requests on the old one
  │ stopping_old  stop the old unit: SIGTERM its whole cgroup, SIGKILL after `grace`
  └ 200 OK        (any failure before "promoting" → old version never
                   stopped serving; the webhook returns 422 for a refusal
                   and 5xx for a failure, and CI goes red either way)
```

The diagram shows a **URL pull**; the same pipeline serves two more
sources (see [Webhook API](#webhook-api)): a **push** streams the
tarball in the request body (skips `downloading`), and a **rollback**
relaunches an on-disk release (skips `downloading`/`extracting`, and
`preparing` runs no `pre_start`). Versions are immutable — a deploy of an
existing version with other bytes is rejected; rollback is how you
relaunch one, the running version included, and a deploy of the
running version with the same artifact relaunches it too (see
[Relaunching the running version](#relaunching-the-running-version)).

The cutover is an atomic pointer swap inside a `reverse_proxy` dynamic
upstream source — no config reload, no socket juggling, and every
reverse_proxy feature (WebSockets, HTTP/2, streaming, retries) keeps
working. Rollback is one curl: `POST /<app>?rollback=<version>`.

If you know Nomad, the concept map is:

| Nomad | liveswap |
|---|---|
| `canary = 1` + `auto_promote` | start new → health gate → atomic cutover |
| `min_healthy_time` | `soak` |
| `auto_revert` | failures never promote; old version keeps serving |
| `artifact` stanza + deploy webhook | the webhook payload's `url` |
| `nomadService` template re-render + SIGUSR1 | `dynamic liveswap <app>` |

## Install

Build Caddy with the module:

```sh
xcaddy build --with github.com/smallhoursorg/motel/liveswap
```

Or take the prebuilt binary from
[motel](https://github.com/smallhoursorg/motel/releases), which
ships liveswap compiled in — on Debian 13 the `.deb` sets up everything
below for you.

**The server needs systemd.** liveswap runs your apps as transient
units on the service user's own systemd manager, so the box also needs
`libpam-systemd`, `dbus`, and `loginctl enable-linger <user>`; without that
manager, a config defining any app refuses to start. A container image
is not a deployment target for the same reason — there is no user
manager in one, and the per-app [sandbox](#sandbox) needs namespaces
most container hosts will not give you.

## Caddyfile

```caddyfile
{
	# Keep the admin API off TCP: every app can reach localhost. This
	# is the motel package's path (its unit creates /run/motel and
	# `systemctl reload` finds the socket here); a self-managed unit
	# needs RuntimeDirectory=motel, or another dir its user owns —
	# never one under the liveswap root or the sandbox's base view
	# (/usr, /etc/ssl/certs, ...), which an app's view would hold.
	admin unix//run/motel/admin.sock

	liveswap {
		# root /var/lib/liveswap                # where releases/state live (default)
		artifact_allowlist github.com/your-org/ # required: where artifacts may come from
		# allow_insecure_http                  # permit http:// artifact URLs (off by default)
		# allow_tcp_admin                      # test rigs only: let the admin API stay on TCP (off by default)

		# A deploy_trust block may also sit here, as the default for
		# apps that declare none of their own. Claims that name one
		# repo belong with the app they authorize, as below — a
		# per-app block REPLACES the global one, it does not add to it.

		app blog {                             # a Node.js app
			command node server.js             # runs with CWD = the release dir
			pre_start node migrate.js          # optional; non-zero exit aborts the deploy
			env_file /etc/motel/blog.env    # optional KEY=VALUE file
			env NODE_ENV production            # inline env, repeatable

			# Who may deploy this app — required (here or globally). A
			# deploy carries an `Authorization: Bearer <JWT>`; it is
			# authorized if any deploy_trust source verifies it. No
			# shared secret is ever stored on the box.
			deploy_trust github {                 # preset: GitHub Actions OIDC
				audience motel
				claim repository your-org/blog     # pin who may deploy
				claim ref        refs/heads/main
			}

			# Everything below is a default, shown for reference:
			# health_path       /health        # GET must return 2xx ("off" = liveness only)
			# health_interval   5s
			# health_timeout    2s
			# soak              15s             # continuous health required before cutover
			# deadline          5m              # abort if not healthy in time
			# drain             5s              # pause between cutover and SIGTERM
			# grace             10s             # SIGTERM → grace → SIGKILL
			# watchdog          on              # restart on crash / sustained health failure
			# watchdog_failures 3               # consecutive failed probes before a restart
			# watchdog_grace    30s             # post-start window where probe failures don't count
			# watchdog_restarts 5               # restart budget within watchdog_window
			# watchdog_window   10m             # sliding window for the budget
			# keep              5               # release dirs retained on disk
			# max_artifact_size 100MB
			# max_artifact_entries 100000
		}

		app api {                              # a Go app
			command ./server --config config.yaml
			env DATABASE_URL sqlite:{shared_dir}/api.db
			deploy_trust github {                 # its own repo, its own block
				audience motel
				claim repository your-org/api
				claim ref        refs/heads/main
			}
		}
	}
}

blog.example.com {
	reverse_proxy {
		dynamic liveswap blog
	}
}

api.example.com {
	reverse_proxy {
		dynamic liveswap api
	}
}

deploy.example.com {
	liveswap_webhook
}
```

To count an app's responses for the metrics store (the monitor
block's `metrics` line; experimental, notify/DESIGN-metrics.md), write
`observe <app>` first in its
site or route: per release, it counts each response's status class,
times it to its headers, and reads a cache's `Cache-Status` verdict, and
`GET /<app>` with the deploy token gains `metrics.requests`
(`?range=1h`, the default, or `?range=24h`). It keeps nothing of a
request, and costs about 0.4 µs on one (liveswap/DESIGN-liveswap.md,
"The response observer"). In a `route`, write it first; in a site with
no `route`, anywhere among the site's lines, since Caddy orders it
before `cache` and `hint_penaltybox` by itself. Taken out, the app's
numbers stop being pushed; put back, they start from zero.

```
blog.example.com {
	route {
		observe blog
		cache
		reverse_proxy {
			dynamic liveswap blog
		}
	}
}
```

Apps must listen on the unix socket at the injected `SOCKET` path —
one per instance, in a `run/<nonce>/` dir of its own; motel proxies to
it and health-probes it, and nothing listens on a TCP port. Their
environment is, lowest precedence first: an allowlisted slice of
Caddy's environment (`PATH`, `LANG`, `TZ`, `LC_*` — nothing else, so
supervisor credentials like ACME DNS tokens never reach apps) →
`HOME` set to the app's `shared/` → `env_file` → inline `env` →
injected `SOCKET`, all layered on the systemd user manager's own
defaults (`XDG_RUNTIME_DIR`, `INVOCATION_ID`, …). Two of those
defaults are **reserved** in a sandboxed unit and cannot be set by
`env` or `env_file`: `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS`
are unset after everything else, because they name the user manager's
own runtime directory and bus — the sockets a sandboxed app must not
hold. Setting either has no effect rather than an error. Keys must be valid
variable names (`[A-Za-z_][A-Za-z0-9_]*`) — systemd rejects anything
else, so config load does too. Four names are the shell's: `IFS`,
`OPTIND`, `PPID` and `PWD` are reset by the `/bin/sh` every unit runs
behind (below, "worker trees never outlive it"), so `env` and
`env_file` refuse them rather than pass a value that would not arrive. Anything more an app needs must be
passed explicitly via `env` or `env_file`. Apps get the user manager's
resource limits; each unit sets its open-files limit (soft and hard)
to the manager's ceiling, which the package raises to match
`motel.service` (1048576) via a `user@<uid>.service.d` drop-in —
effective from the manager's next start: an upgrade installs the
drop-in but deliberately does not restart a running manager (that
would stop every app), so the ceiling changes at the next boot or
manual restart.

### Listening on the socket

The socket is a path, so every server that can listen on a unix
socket works unchanged; locally you keep using a port. One branch
covers both:

```js
// Node / Express: server.listen() takes a path as well as a port.
server.listen(process.env.SOCKET ?? 3000);
```

A Node app that already passes `process.env.PORT` to `listen()` as it
is (`server.listen(process.env.PORT)`, or Express's
`app.listen(process.env.PORT || 3000)`) needs no change:
`env PORT {socket}` in its block sets `PORT` to the socket's path
([Placeholders](#placeholders)), and `listen()` binds a path it is
given; one that makes `PORT` a number first, as
`parseInt(process.env.PORT)` does, fails to listen.

```ts
// Deno: the `deno serve` CLI is port-only; call Deno.serve yourself.
const sock = Deno.env.get("SOCKET");
Deno.serve(sock ? { path: sock } : { port: 8000 }, handler);
```

```go
// Go
ln, err := net.Listen("unix", os.Getenv("SOCKET"))
if err != nil {
	log.Fatal(err)
}
log.Fatal(http.Serve(ln, mux))
```

Bun: `Bun.serve({ unix: process.env.SOCKET, fetch })`. Hono on Node:
`createAdaptorServer(app).listen(process.env.SOCKET)`. Python:
`gunicorn --bind unix:{socket}`, `uvicorn --uds {socket}`. Ruby: `puma
-b unix://{socket}`. Rust: axum/hyper on a `tokio::net::UnixListener`.
Where the path goes on the command line, as in those three, write the
`{socket}` placeholder (see [Placeholders](#placeholders)) in
`command`, not `$SOCKET`: `command` and `pre_start` run without a
shell and without systemd's `$VAR` substitution, so every word reaches
the app as written and `$SOCKET` arrives as those seven characters. A
command that needs a variable's value reads it from its environment.

Framework CLIs that only bind TCP (`next start`, Astro's node adapter)
need a small custom server that calls the framework's request handler
from an `http.Server` listening on the socket.

motel's health probe arrives as `GET <health_path>` with
`Host: localhost`; an app that allowlists hosts (Django's
`ALLOWED_HOSTS`, Rails' `config.hosts`) must accept it. To poke a
running app yourself, send the same request from a root shell, as the
motel user (`run/` is `0750 motel:motel`):
`runuser -u motel -- curl --unix-socket "$SOCKET" http://localhost/health`
(the path is `.socket` in the status JSON).

Bind the socket **once** per instance. At the first successful
connect motel hard-links the socket's inode into a directory of
its own (`<app>/proxy/`, outside the sandbox) and dials that from
then on — never the name under `run/<nonce>/`, which the app can write — so
an app that unlinks and re-binds its path is, as far as motel is
concerned, gone: its health probes fail and the watchdog restarts it.
(With `health_path off` the watchdog only watches the process, so a
re-bound app stays up and unroutable — another reason not to
re-bind.) A stale socket file is never your problem either: motel removes
it with the instance's run dir once the instance is retired, and
every launch gets a fresh path.

### Placeholders

`command`, `pre_start` and `env` values may use deploy-time
placeholders: `{version}`, `{socket}`, `{release_dir}`, `{shared_dir}`.
`{shared_dir}` (`<root>/<app>/shared/`) survives deploys — put SQLite
files and uploads there. Standard Caddy `{env.*}` placeholders are
resolved at config load.

### Options

| Option | Default | Meaning |
|---|---|---|
| `root` | `/var/lib/liveswap` | Releases, shared data, state per app. Plain characters: a space, a colon, a backslash, a quote or a control character in the path is refused, since a systemd unit file cannot carry one in a bind mount |
| `deploy_trust <preset> { … }` | — (required) | Who may deploy; global default or per app (see [Deploy authentication](#deploy-authentication-deploy_trust)) |
| `allow_insecure_http` | off | Permit plain-http artifact URLs |
| `allow_tcp_admin` | off | Test rigs only. Every app shares the box's network namespace, so an admin API on TCP (Caddy's default, `localhost:2019`) is every app's to reconfigure the server with; `motel validate` and `reload` refuse a config that runs apps with one, and `run` serves it saying so; so is an abstract socket (`unix/@name`, the network namespace's), or a unix socket under the liveswap root or the sandbox's base view, which an app's view holds, or at a relative path, which Caddy's working directory places where the gate cannot see; and a config given as stdin or a pipe, which the gate cannot read before Caddy reads it once, is refused unread. The starter keeps the API on `admin unix//run/motel/admin.sock`. This says the TCP listener is meant — a rig whose runner drives `/load` over the network — and the refusal stands down |
| `artifact_allowlist` | — (required) | Where artifacts may be fetched from. Entries are a host (`artifacts.corp`) or a host + path prefix (`github.com/your-org/`). An entry admits only the scheme's default port unless it declares one (`minio.corp:9000`) — no wildcards; the port picks which service on the host answers, so it belongs to the operator, not the payload. Pin the path on multi-tenant hosts — a bare `github.com` admits anyone's artifacts. Query strings are refused unless the entry declares the parameter names it vouches for: `gitlab.com/api/v4/projects/42/?job` allows `?job=build`, `bucket.s3.corp/releases/?X-Amz-*` allows the presigned-URL family (a trailing `*` declares a prefix; values are never re-encoded — signed queries pass through byte-identical — but every query byte must be a legal RFC 3986 query character, so percent-encode anything exotic). Closed by default because on some servers a query *name* can override path routing entirely (WordPress-style `?p=2`), which would defeat the path pin. A refused URL fails the deploy with a 422 naming the offending parameter. First hop only, by design (GitHub asset URLs redirect to S3); every hop must still be https unless `allow_insecure_http`. Apps may override |
| `command` | — (required) | argv to start the app, CWD = release dir |
| `pre_start` | — | Run-to-completion hook; failure aborts deploy |
| `env`, `env_file` | — | Extra environment |
| `health_path` | `/health` | 2xx = healthy; `off` = process-liveness only |
| `health_interval` / `health_timeout` | `5s` / `2s` | Probe cadence |
| `soak` | `15s` | Continuous health required before cutover. `0` is no soak: traffic moves on the first healthy probe. Refused with `health_path off`, where nothing is probed and the gate would look once, straight after the process starts: set a `health_path`, or a `soak` above 0 |
| `deadline` | `5m` | Bound on pre_start and, separately, on the health gate (soak included, so `soak` cannot exceed it). The gate is judged once per `health_interval`, so it can run over by one interval plus two `health_timeout`s (the probe before the last tick and the one on it); a soak that falls due on that first tick past the deadline still passes |
| `drain` | `5s` | In-flight grace after cutover, before SIGTERM |
| `grace` | `10s` | SIGTERM → SIGKILL window |
| `watchdog` | `on` | Continuous supervision: restart on crash or sustained health failure (`off` to disable) |
| `watchdog_failures` | `3` | Consecutive failed probes before a restart; one pass resets the count |
| `watchdog_grace` | `30s` | After every (re)start, probe failures don't count until this elapses; a crash always counts |
| `watchdog_restarts` | `5` | Restart budget within `watchdog_window`; crash and health restarts share it |
| `watchdog_window` | `10m` | Sliding window for the restart budget |
| `keep` | `5` | Release dirs retained (GC after success): the `keep` most recently deployed, a rollback counting as a deploy of its version. The version the deploy promoted is always one of them, and the version it replaced is the next |
| `deploy_log_lines` | `40` | How many of the app's last journal lines a failed deploy's response carries (see [Webhook API](#webhook-api)); `0` keeps the app's output on the box (no tail, and no health probe body). At most 1000, and 8 KiB whatever the count |
| `max_artifact_size` | `100MB` | Download cap; decompressed cap is 10× |
| `max_artifact_entries` | `100000` | Cap on the files, directories and links one artifact creates (implied parent directories included). The byte cap does not bound what extraction *consumes* — every object costs an inode and most a disk block — so this is what keeps one hostile artifact from filling the disk for everything else on the box. A CI-built artifact is thousands; a Next.js standalone output is ~5–20k |
| `backup { … }` | — | Declares what under the app's `shared/` is worth backing up: `sqlite` names database files, `files` names files and directories (`.` is all of `shared/`). See [Declaring backups](#declaring-backups) |
| `memory_max` | none | RAM for every unit of the app, e.g. `512MB` (at least `1MB`); at it the kernel kills the process that asked, the unit fails and the watchdog restarts the app. No swap under it. See [Resource caps](#resource-caps) |
| `tasks_max` | none (the user manager's own default, 15% of threads-max) | Threads and processes of every unit of the app; past it a fork or thread fails with EAGAIN. The unit's sleeping sibling (below, "worker trees never outlive it") is one of them, so the app itself gets one fewer and the least you can set is 2. Removing it on a reload puts the running unit back on the manager's default, where that default can be read; otherwise the unit keeps the cap until its next launch, and the journal says so |
| `cpu_quota` | none | CPU time for every unit of the app, in percent of one CPU: `200%` is two CPUs' worth, at most `100000%`; past it the app waits, nothing is killed |

Every duration, count and size above must be more than `0`, except `soak`
and `deploy_log_lines`, where `0` is a setting. A `0` elsewhere is
refused when the config loads, naming the option and its minimum;
leave an option out to get its default.

Both caps are cliffs an app can grow into, so each successful deploy
logs the artifact's entry count and decompressed size and, past **75%**
of either cap, warns naming the directive to raise; `GET /<app>` shows
the last deploy's figures as `artifact_entries` / `artifact_bytes`.
Entry names and link targets are also bounded at PATH_MAX (4096
bytes, under the release directory) and NAME_MAX (255 per component),
and the content entries *declare* is capped like the stream (a sparse
entry expands from no stream at all); not knobs — no real path or
artifact is close. The byte cap is for the whole artifact, not for
each file in it, so a single large executable is fine — the Node
example unpacks to about 300 MB in two binaries, and it is the
compressed tarball that has to fit `max_artifact_size`. A million tiny
files fit the byte budget comfortably; refusing those is the entry
cap's job.

### Declaring backups

```
app blog {
	command ./server
	backup {
		sqlite app.db
		files  uploads
	}
}
```

The block is a declaration: motel checks it when the config loads
— so `motel validate` refuses a bad one — and does nothing else
with it. Nothing in the server reads, copies or schedules anything
because of it.

Paths are relative to the app's `shared/` and take no placeholders
(`{shared_dir}/app.db` is written `app.db`). Each line takes one or
more paths and may repeat. A path is refused when it is empty, is
absolute, is not clean (`./`, `..`, a doubled or trailing `/`), reaches outside
`shared/`, holds a control character or is not UTF-8, is longer than
400 bytes (sqlite3 does not open a path past its own limit as itself),
or is declared twice. A `sqlite` path names one file,
never `.`.

A database may sit inside a `files` path — `files .` with `sqlite
app.db` is "everything, and this one is a database". Two `files`
paths may not contain one another, nothing may be declared under a
database's path, and a database's own `-wal`, `-shm` and `-journal`
are part of it: neither they nor anything under their names is
declared.

An app that declares a backup needs a literal `root`: with `root
{env.X}` the config is refused, naming the app. Without a `backup`
block, a placeholder `root` loads.

The Caddyfile's own `{$VAR}` is not caught: it is substituted in the
text wherever the file is adapted, before motel is given the
config, so the result depends on the environment of whoever adapts
it. Do not write `root` or a backup path with one in a config that
declares a backup.

Where a backup is declared matters as much as what. A backup run
(`motel-backup`) reads the Caddyfile inside a view that holds the
Caddyfile's own directory and nothing else, as an account that owns
nothing there: an app whose `app` line, `backup` block or `root` it
cannot read there is not in its plan, and is not backed up. So once
any app declares a backup, `motel validate` and `motel reload`
refuse a Caddyfile in which one of those was read from a file outside
its directory — an import, or the file a snippet is defined in — or
from a file others may not read, or under a directory others may not
enter — a link is followed to where it leads. A refused reload leaves
the running config serving. `motel run` is never refused for it — a
start that refused would take every site down — and logs the same
error instead. `motel validate` of any file but
`/etc/motel/Caddyfile` does not ask that file for its own mode or
the directories on its way: a candidate is not the file a run reads
(the box applier's step 6 validates a copy it staged `0600` in a
directory of its own), and the reload asks them of the file installed.
What a candidate imports is asked, and so is every directory from the
candidate's own down to it.

motel-backup reads one Caddyfile, `/etc/motel/Caddyfile`. Where
it is installed (`/usr/bin/motel-backup`), `motel reload` of any
other Caddyfile that declares a backup is refused as well: the server
would run what no backup run reads, and an app only in it would never
be backed up. The refusal names `motel-backup validate <file>` for a
look at what a backup run would make of it, and `/etc/motel/Caddyfile`
as where to put it (a signed push does). Where motel-backup is not
installed, it is a warning. `motel validate` of a candidate file —
step 6 validates a staged copy before the applier puts it in place — is
not asked; `motel run` logs it and serves.

The check adapts the Caddyfile in the environment the command was
started in, with each `--envfile` loaded first as Caddy's own
`validate` and `run` load it (a variable the environment already
holds is kept), so a `{$NAME}` set only there is judged as Caddy
will run it. The packaged service passes no `--envfile`, and
`motel-backup` refuses an import whose path depends on a variable.
### Resource caps

`memory_max`, `tasks_max` and `cpu_quota` bound every unit of the app —
the instance and its `pre_start` alike — as the unit's own
`MemoryMax=`, `TasksMax=` and `CPUQuota=`, which the sandbox's
read-only cgroupfs keeps the app from rewriting. None is set unless
you set it. What each one does when it is hit:

| Cap hit | What happens | What you see |
|---|---|---|
| `memory_max` | The kernel kills the process that asked for the memory — under a `sh -c` or `npm start` command that is a child — and the manager then stops the unit (`Result=oom-kill`); the watchdog restarts the app, on its budget. A `pre_start` that hits it fails the deploy | `journalctl -k` shows the kill — the kernel's line names the unit's cgroup and the process; the app's own journal (`journalctl -t motel-<app>`) shows nothing, a killed process prints no last line; the deploy's response or `watchdog.last_restart_cause` says `crash`, and the exit the runner records ends `; the kernel's OOM killer took a process of the unit (its memory_max, or the box's memory), and the unit was stopped for it` |
| `tasks_max` | A `fork` or thread creation fails with `EAGAIN`. A runtime limps on; a `sh -c` wrapped `command` dies on the first refused fork (dash exits 2), which the watchdog reads as a crash. Every unit starts as such a shell (the sibling), which is why a cap of 1 is refused at config time | the app's own error, in its journal |
| `cpu_quota` | The unit waits for the next 100 ms period. Nothing is killed | latency; `cpu.stat`'s `nr_throttled` in the unit's cgroup |

Sizing, measured 2026-10-22: an idle Node server has 7 threads (11 under
ten concurrent file reads, more with `UV_THREADPOOL_SIZE`), an idle
Deno server 8, so a `tasks_max` under a few dozen breaks a runtime
before it breaks a fork bomb. Without `tasks_max` a unit is already
bounded at the user manager's default, 15% of the kernel's
`threads-max` (2273 on a 2 GB box), and a fork bomb at that bound left
the box operable: motel answered, a deploy started within 2 s,
stopping the unit took 5 s. What it did do was stall the app's
**siblings** — a second app's requests ran to 10 s — because every
app's unit has the same CPU weight; `cpu_quota` on each app is what
bounds that. Caddy itself is not starved: `system.slice` and
`user.slice` split CPU 50/50 at the root. A leak without `memory_max`
is killed by the kernel's global OOM killer, the app first — a
user-manager unit runs at `oom_score_adj` 200 and motel.service at
0, which is systemd's default for user managers and their units, not
a setting of motel's — after the box has spent its free memory on
it.

A reload that changes a cap sets it on the running instance at once,
through the manager — no restart; `GET /<app>` shows the values in
effect as `caps` (`memory_max` in bytes, `tasks_max`, `cpu_quota` in
percent). A reload that removes one lifts it the same way. A
`memory_max` set below what the instance already holds is enforced
the same way too: the kernel reclaims what it can and kills the rest
(the row above), and the watchdog relaunches the app under the new
cap, so lower it to what the app is using, not to what it should be
using. Should the manager refuse the change, the journal says so and
the caps apply at the app's next launch.

A cap is only as real as the controller behind it: on a host whose
user manager does not delegate the memory, pids or cpu controller to
its units (some containers), systemd accepts and reports the cap and
the kernel bounds nothing. motel reads the unit's own
`cgroup.controllers` when it starts or reattaches, warns in the
journal, and lists such caps in status as `caps_unenforced` instead of
under `caps`. Debian 13 delegates all three (measured, as do Ubuntu
24.04/26.04 and Fedora 43), so on a supported box the field never
appears.

To bound every app at once — the whole `motel` user against Caddy —
set the caps on the user's slice instead, from the console:
`systemctl set-property user-$(id -u motel).slice MemoryMax=2G CPUQuota=300%`
applies at once and persists; a drop-in file plus `daemon-reload` is
**not** re-applied to the live slice on Debian 13's systemd 257
(measured; 255 and 259 do). It is the box's setting, not motel's,
so the package does not write it — see
[hardening step 6](../docs/hardening.md#6-a-ceiling-for-all-the-apps-together-if-you-want-one).

## Watchdog

Deploys and boot recovery start instances; the watchdog keeps them
running. It watches the current instance continuously and relaunches
the **same version** when the process exits, or when the health
endpoint (`health_path`, probed every `health_interval`) fails
`watchdog_failures` consecutive probes. With `health_path off` the
watchdog is liveness-only: crashes still restart, health does not
apply.

Restart pacing is deliberately not configurable: exponential backoff
from 1s, doubling to a 60s cap, with ±20% jitter; the backoff resets
only after the app has been continuously healthy for
`max(30s, watchdog_grace)`. Every restart re-arms `watchdog_grace`, so
a slow-booting app is not probed into a kill loop.

The watchdog **never gives up** on a failure a retry can fix. The restart budget is a rate
limiter, not a give-up point: after `watchdog_restarts` restarts
inside `watchdog_window` it *throttles* — logs an error, reports
`"state":"throttled"` in the status JSON, unroutes a dead instance so
the proxy fails cleanly instead of dialing a socket nothing is
listening on, waits for the
oldest restart to slide out of the window, and tries again. An app
taken down by a transient incident (a traffic flood, a dependency
outage) therefore comes back on its own once the incident ends, with
nobody at the keyboard; a persistent crash loop costs at most
`watchdog_restarts` restarts per `watchdog_window`, forever — which is
also the bound on anyone who can make your health endpoint fail. A
successful deploy is the fast path out of a throttle wait: it resets
the budget and backoff immediately. A long-running throttle loop means
the release itself is broken — watch `restarts_in_window` in the
status JSON, or have the monitor block report it as
`watchdog_throttled` ([docs/monitor.md](../docs/monitor.md); experimental). A deploy running over
a dead instance holds the restart until it ends (the state stays
`backoff` meanwhile): one that promotes replaces the instance, and
after one that fails the restart goes within about a second, without
waiting out the rest of its backoff — though a throttle still waits
for its budget.
Health probes never
follow redirects: a 3xx answer is simply "not 2xx".

A relaunch runs the deploy's pre-flight first, and a release it
refuses is not retried: one built for another machine (a backup
restored onto a box of the other architecture), or whose command is
no longer there or outside the sandbox view, would only die at exec
on every attempt (`exit status 127`, the shell's `not found` in the
journal). The watchdog then drops the instance —
nothing current, the proxy answers 5xx — logs the refusal once, parks
in `idle`, and status names it in `last_failure`; boot recovery
refuses such a release the same way, in the same field. A deploy of
another version (or a rollback to one), or a reload once the cause is
fixed, launches the app again; the refused version itself is still
on disk, so deploying it again is refused as already existing.

The status JSON's `watchdog` object reports `state`
(`grace|watching|backoff|throttled|disabled|idle`),
`consecutive_failures`, `restarts_in_window`, `last_restart_at`,
`last_restart_cause` (`crash|health`) and `last_failure` (the last
failed probe, or why nothing runs: a relaunch refused, a boot
recovery that gave up).
`restarts_in_window` is what the budget counts: it includes a restart
still waiting out its `backoff` or a deploy's end (dropped again if
that restart is called off), and a relaunch that failed to start, while
`last_restart_at` and `last_restart_cause` describe the last restart
that started an instance.

If your health endpoint can be slow under load, raise `health_timeout`
(probe timeouts count as failures). If it redirects — a `/health` →
`/health/` trailing slash is the classic — point `health_path` at the
final path, since a 3xx reads as unhealthy for the watchdog and the
deploy gate alike.

## Sandbox

Every instance runs inside systemd's own per-unit sandbox. There is
one sandbox, no setting that turns it off or widens it, and a host
either delivers it or motel does not start with an app configured:

| What every unit gets |
|---|
| User namespace (`PrivateUsers=`) |
| PID namespace (`PrivatePIDs=`) — supervisor, user manager and siblings **invisible and unsignalable** |
| Deny-by-default filesystem: the whole host filesystem replaced by an empty read-only tmpfs, only named paths bound back |
| motel's keys and sockets, other apps, and every path nothing named — **absent**, not merely unreadable |
| Private `/tmp`, minimal `/dev`, read-only cgroupfs, no capabilities, `@system-service` syscall filter less `io_uring_setup`, the native syscall ABI only, `AF_INET`/`AF_INET6`/`AF_UNIX`/`AF_NETLINK` only for `socket()` — the protocol modules under them (`sctp`, `l2tp_ip`, `l2tp_ip6`, `smc`) load for any program unless [hardening step 5](../docs/hardening.md#5-kernel-modules-no-app-needs-if-you-want-them) refuses them (#315) — no nested namespaces |

`PrivatePIDs=` needs systemd 257, which Debian 13 — the supported
host — ships, and the `.deb` refuses to install on anything older.
Where the namespaces cannot be had at all (a container, an
LXC VPS, a kernel built without user namespaces, an LSM that refuses
them) motel refuses to start, naming what the host lacks. It is
deliberately not a ladder, and there is deliberately no mode that runs
anyway: a weaker sandbox wearing the same name — or none at all — is
the "looks configured, quietly weaker" failure this design refuses.
An app with no sandbox would not be alone in paying for it: every app
runs as the motel user, so one bare app could read every sibling's
data and motel's own keys. That is why there is no per-app opt-out
either.

**An app sees its release dir, its `shared/`, its instance's
`run/<nonce>/`, a private `/tmp`, the OS runtime. Nothing else on the
host exists in its view.**

That is the whole guarantee. The view is built deny-by-default
(`TemporaryFileSystem=/:ro` plus explicit binds), so it is a policy
rather than a list of things to hide: `state.json`, `tmp/` (the upload
staging dir), other releases, other apps, `/var/lib/motel` (TLS
keys), `/run/motel` (admin socket), `/run/user/<uid>` (the manager
socket), `/etc/motel`, `/home`, `/opt`, `/srv` and every operator
`env_file` are absent — not present-but-unreadable — and no list has
to be kept current for that to stay true.

"The OS runtime" is a named set, not the host: `/usr` and its usrmerge
aliases (`/bin`, `/sbin`, `/lib*`), the certificate directories of the
TLS trust store, and the individual `/etc` entries needed for name and
user resolution, timezone and the dynamic linker. `sandboxBaseView` in
`sandbox.go` is the list, and it names entries rather than the trees
containing them: `/etc` is not bound whole (that would hand every app
every other app's `env_file`), and neither is `/etc/ssl`, which also
holds `/etc/ssl/private`. Every entry is optional, since no distro has
all of them, so inside a unit `ls /etc` shows however many this host
actually has — a dozen or so — and `ls /var/lib` shows exactly one,
the liveswap root.

The working directory is the release dir and `HOME` defaults to
`shared/`. Those two and the instance's `run/<nonce>/`, where it binds
its socket, are the writable binds (`BindPaths=`). `HOME` is applied
before `env_file` and inline `env`, so you can point it elsewhere —
inside the release dir, say. Point it somewhere the sandbox does not bind and
the app gets a `HOME` that does not exist inside its unit; liveswap
warns at every launch rather than refusing, since you asked for it.
The network namespace is shared by design — apps make outbound calls
and reach a same-box database over loopback — but nothing motel
runs listens on a port: each instance binds its own unix socket under
`run/<nonce>/`, which motel reaches at its real path, and a sibling's
socket is outside the view like everything else of the sibling's.

This closes the routes the threat model ranks first: reading the
supervisor's environment or walking the host through
`/proc/<pid>/root` (the **user** namespace is what closes them — the
kernel refuses `ptrace`-class access across user namespaces even for
the same uid), the admin socket, the TLS keys, sibling files. The
**PID** namespace adds the rest: process visibility and signals. Worth
keeping straight, because it is why a host that delivers neither is
refused rather than given something in between.

**What this costs you.** An app that reads something under `/opt`,
`/srv` or `/var/lib`, or whose runtime lives outside `/usr` (a
vendored Node, an `nvm`/`asdf` shim), cannot be reached at all —
those paths do not exist inside the unit. A deploy is where this
surfaces, and it has a fallback there: the health gate fails the new
version while the old one keeps serving. A command that is not in the
view is refused before the unit is even created — at the deploy's
pre-flight, as a 422 — with a message that says where the runtime has
to live, rather than failing at exec with the shell's `not found`
(`exit status 127`) in the journal. The same pre-flight
refuses an executable built for another machine (an x86-64 tarball
pushed to an arm64 box) naming the `runs-on` to build on, and a 32-bit
executable, which a unit cannot run: units allow the box's native
system-call ABI only (`SystemCallArchitectures=native`). Every
relaunch — boot recovery, a watchdog restart, a rollback — runs it
too, and stops at a refusal rather than retrying it (see
[Watchdog](#watchdog)).

**The host is measured at start.** Capability is probed by running a
throwaway unit and checking the namespaces and seccomp from inside
(`journalctl -t motel-sandbox-probe` shows what it saw). A host
that cannot deliver the sandbox fails the start — admin socket and
proxy included — with the probe's reason. That measurement is cached
per connection to the user manager, so a reload does not repeat it;
a refused host is measured again on the next start or reload, so
fixing the host does take effect. Restarting the user manager
re-measures either way.

**The sandbox is an availability dependency.** A host that stops being
able to deliver it will not start motel until the host is fixed,
and systemd does not retry a start motel refused — fix the host,
then `systemctl start motel`; there is no setting that runs apps
without one. That is deliberate —
the alternative is a supervisor that silently runs every app with no
isolation because the kernel changed its mind — so prove a box before
you restart into it. `motel validate` does not measure the host (it
never starts an app); a reload does, once: the verdict is held for the
life of motel's connection to the user manager, so the first start
or reload after motel dials the manager is the measurement and a
later reload reuses it. On a fresh box that first reload is the proof
you want, and a reload that cannot activate leaves the running config
serving where a restart does not. A host changed underneath a running
motel — a sysctl, an LSM policy load — is not re-measured until the
next restart, which is where it can refuse; there is deliberately no
uncached preflight, because the cache is what keeps a throwaway unit
off every reload.
Every launch is sandboxed the same way — a deploy, a crash relaunch,
boot recovery, a reattach after `systemctl restart motel` — so
there is nothing per instance to roll out and nothing to watch in
status: an app is running, or motel did not start.

**What a running instance's sandbox is fixed to.** A unit's view is
built when it starts and is never rebuilt under it — reloads
deliberately leave running apps alone — so a config change reaches an
app at its next launch (a deploy, a rollback, or a relaunch after a
crash, a sustained health failure, or a reboot), not before. That is the only
thing that ages now, and it fails safe: a secret belonging to an app
you add tomorrow is already absent from every unit running today,
because nothing ever bound it.

**Keep env files outside every app's view.** `/etc/motel` is the
documented location and no app may name it. The box writes each app's
secrets from its repository to `/etc/motel/secrets/<app>.env`
([docs/secrets.md](../docs/secrets.md)); a file made by hand elsewhere
under `/etc/motel` works the same way. motel refuses a config
in which one app's `env_file` sits inside another app's own dirs,
or anywhere in the OS base view (`/usr`, the named `/etc` entries) —
both would put one app's secrets in another app's sandbox, and the
second would put them in *every* app's. An `env_file` inside its own
app's `shared/` is a warning rather than an error: the app receives
those variables anyway, but under `shared/` it can also rewrite the
file and so choose its own next launch's environment.

motel reads the file itself, as the `motel` user, each time the
app launches (a deploy, a rollback, a relaunch after a crash or a
reboot); loading the config does not open it. So it must be readable
by that user and nobody else: the box writes its files 0640
`root:motel`, and a file made by hand needs the same, as root —
`install -m 0640 -o root -g motel -T /dev/null /etc/motel/blog.env`,
then edit it — and keep it in place: a missing or unreadable file
fails that launch.
A world-readable or world-writable file is warned about — at config
load (`motel validate` shows it when run as an account that can
reach the file, and says so when it cannot) and again at each launch,
from the file the launch actually read — never refused: the launch
still runs, and a mode changed between loads is not what takes an app
down on a relaunch. The owner and group are not checked; the
`install` line above sets them.

**The sandbox does not undo what an app already did.** It restricts
what an app can *reach*; it cannot un-copy. An app's `shared/`
survives every deploy and is bound writable into every new sandbox,
so whatever a compromised instance left there — a copied secret, a
payload the next version reads — is inside the view of every version
after it. If an app may have been compromised, deploying a fixed
version is not enough: clear its `shared/` and rotate anything it
could read.

**When an app breaks under the sandbox** the symptom is usually an
`ENOENT` for something that exists on the host: a database's unix
socket, a data directory outside the app's own, a runtime under
`/opt`. **There is no way to widen a view.** An app sees the OS base
view, its own release dir, its own `shared/` and its instance's
`run/<nonce>/`. That is the whole list, and it is fixed.

That is a deliberate limit, not an oversight. The one mechanism that
would widen a view is also the one that has to be correct against
symlinks, TOCTOU, cross-app containment and the base view all at once,
and getting it wrong hands one app another's secrets. It comes back
when a running app needs it and can be designed and reviewed on its
own; until then the answers below are the answers. There is no
per-app opt-out either: an app with no sandbox runs as the same user
as everything else and reaches all of it.

Practical consequences worth planning around:

- **Put persistent data in `shared/`.** It survives deploys, it is
  writable, and it is `$HOME` inside the unit. A SQLite file belongs
  there, not in `/var/lib/myapp`.
- **A same-box database is reached over TCP loopback**, which the
  shared network namespace allows; its unix socket is outside the
  view.
- **Install runtimes under `/usr`.** The base view binds it, so
  `/usr/local/bin/deno` or an apt-installed `node` is already inside
  every unit. A vendored runtime under `/opt`, or an `nvm`/`asdf` shim
  under a home directory, is not — ship it inside the release instead.

An app's own `releases/<version>`, `shared/` and `run/<nonce>/` must
**be** the directories they name. motel resolves them immediately
before the unit is created and refuses to launch if any points
somewhere else, because a symlink there cannot be told from one you
meant. No app can plant one: inside its unit it writes only those
three. A link in the app dir was put there from outside every
sandbox, and motel refuses it rather than guess. To put an app's
data on another disk, bind-mount it at the same path (`mount --bind
/mnt/blog-data /var/lib/liveswap/blog/shared`, or the equivalent
fstab entry) — a mount resolves to itself, so it is invisible to that
check, and an app cannot forge one. Being invisible, its source is not
checked either: mount only this app's own data there. A source that
is, contains or sits inside one another app uses (another app's
directory, one disk path mounted for two apps, a whole app dir mounted
at another's) gives both apps the same files, writable, and motel
does not refuse it (#308).

The app's directory itself, and the directories motel keeps in it
(`releases/`, `shared/`, `tmp/`, `run/`, `proxy/`, `deploys/`), must
not be links either, wherever motel writes or deletes there:
through a link (`blog -> shop`) they would be another app's. A deploy
into such a directory is refused (500, naming the path and the
remedy), and a socket prune, release GC or deploy record write logs
it and goes no further.

**Workloads the sandbox cannot host are not supported.** Anything
that creates its own namespaces (Chromium's sandbox under Puppeteer,
nested containers) or needs devices beyond the `/dev/null` class has
no place to run: there is no opt-out to put it in. That is a decision
to revisit when such an app exists, on its own terms, not a reason to
reopen the box for everyone.

**Hosts.** The sandbox is built on unprivileged user namespaces.
Debian 13's kernel permits them, which is why it is the supported
host and why the package ships no LSM policy of its own. Some kernels
refuse them — an LSM restriction on unprivileged user namespaces, or a
container or LXC host with them off entirely. motel does not work around
that: it probes, and a host that cannot deliver the namespaces refuses
to start rather than running apps bare. So does a host where systemd
skips the units' seccomp settings: a kernel without seccomp,
`SYSTEMD_SECCOMP=0` in the user manager's environment, or a container
whose own filter stops systemd installing one. The probe reads the
seccomp mode inside its unit and checks that `RestrictNamespaces=`
refuses it a user namespace. `journalctl -t motel-sandbox-probe`
says why it refused (`seccomp 0`, `unshare allowed`). The fix is the
host's: permit unprivileged user namespaces and seccomp for motel's
user manager, or move to Debian 13. There is no way to run there
without the sandbox. One case the probe cannot see: a container host
whose existing seccomp rules leave the kernel no room for the units'
system-call filter, which then does not load while the rest of the
sandbox does ([DESIGN-sandbox.md](DESIGN-sandbox.md), "What this does
NOT stop").

## Runtime permissions (Deno, Node)

The sandbox above is the **ceiling**: it is enforced by the kernel, it
is decided by your Caddyfile, and it survives a compromised artifact.
A runtime with its own permission model lets the app **narrow itself
further inside that ceiling** — and it can express things a mount
namespace cannot, most importantly *which network addresses the
process may reach*.

liveswap does not parse, synthesize or verify these flags. They are
just part of `command`, which is the point: `command` lives in your
Caddyfile on the box, so a deployed tarball cannot widen its own
permissions the way it could if they lived in the artifact.

A Deno app, with the placeholders from [Placeholders](#placeholders)
and a `main.ts` that calls `Deno.serve({ path: Deno.env.get("SOCKET") })`
(see [Listening on the socket](#listening-on-the-socket)):

```
app example {
    command deno run \
        --cached-only \
        --allow-net=unix:{socket} \
        --allow-read={release_dir},{shared_dir},{socket} \
        --allow-write={shared_dir},{socket} \
        --allow-env=SOCKET,DATABASE_URL \
        main.ts                          # {release_dir} read: for its own static files;
                                         # loading modules needs no read grant
    env DENO_DIR {release_dir}/.deno
    env DENO_NO_UPDATE_CHECK 1
    env DATABASE_URL {shared_dir}/app.db
    health_path /health
}
```

[examples/deno](../examples/deno) is a complete app built this way.
motel's e2e suite deploys it with its own flags, on the Deno
version pinned in `e2e/Dockerfile`, so a Deno release that changes
what serving needs fails CI rather than your deploy. The refusals
below were measured on Deno 2.9.6; re-check with
`deno run --help=full` if the box runs a different Deno.

**What that buys you:**

- **`--allow-net` names only the socket.** From Deno 2.9, a unix
  socket is a network address as well as a file: serving needs
  `--allow-net=unix:{socket}` plus read and write on it. That grants
  exactly that path, so every other address — any TCP host, loopback,
  any other socket — is refused with `NotCapable`, and a dependency
  that wakes up and tries to POST your secrets somewhere fails at the
  runtime boundary. Add `--allow-net=<host>` only for the hosts the
  app must call. (`--allow-net` is an **address allowlist, not a
  direction**: it does not distinguish listening from connecting.)
  Deno 2.8 needed only the read and write, and rejects the `unix:`
  form.
- **A `deno compile`d binary carries its own flags.** They are baked
  in at build time, so they come from the repo, not your Caddyfile: a
  compromised build can widen them. Keep `deno run` with the flags in
  `command` if you want the box to hold them.
- Without `--allow-read` the app cannot read `/etc/hosts`, and without
  `--allow-env` it cannot read its own environment — including the
  `SOCKET` liveswap injected. Name only what the app actually needs.
- Remote imports are governed by `--allow-import`, **not**
  `--allow-net`. Leave it off and add `--cached-only` so the serving
  process can never fetch a module. Vendor dependencies at build time
  and ship the populated cache **inside the tarball**, with `DENO_DIR`
  pointing into the release dir as above — the tarball is extracted
  there, so a cache shipped in it is the one Deno reads. Pointing
  `DENO_DIR` at `{shared_dir}` instead means the shipped cache is never
  consulted and a first `--cached-only` start fails; if you want the
  cache to survive deploys, warm it into `{shared_dir}` from
  `pre_start` and point `DENO_DIR` there.

**The runtime must be under `/usr`.** The sandbox base view
binds `/usr`, so a normal `/usr/local/bin/deno` (or an apt-installed
one) is already inside every unit. A runtime somewhere else —
`/opt/node/bin/node`, an nvm or asdf shim — is absent from the
sandbox and cannot be reached: ship it inside the release.

**What it does not buy you.** These flags are enforced *in-process* by
the runtime, so they hold exactly as long as the runtime does: `-A` /
`--allow-all`, `--allow-run` or `--allow-ffi` hand it all back, and a
bug in the runtime itself is outside their reach. That is why they are
the inner layer and not the only one — the user and PID namespaces and
the deny-by-default view are what still stand if the runtime is the
thing that breaks. Use both; do not trade one for the other.

**Health.** `Deno.serve` hands every path to your handler, so
`health_path` only works if it answers 2xx on it. If it does not, set
`health_path off` and the deploy gate falls back to "the process is
still alive after `soak`".

Node's `--permission` model layers the same way for the filesystem,
child processes and workers — it does not gate the network. The
principle is identical whatever the runtime: the ceiling is
liveswap's, the narrowing is the app's, and neither substitutes for
the other.

## Deploy authentication (`deploy_trust`)

Deploys are authenticated with a short-lived **JWT**, verified against
public material only — there is no shared secret on the box. Every app
must resolve to at least one `deploy_trust` source (globally or per
app), or config load fails. A request is authorized if **any** source
verifies its `Authorization: Bearer <JWT>` and **all** that source's
claim constraints match.

Presets:

- `deploy_trust github { audience <a>; claim … }` — GitHub Actions
  OIDC (`issuer https://token.actions.githubusercontent.com`, fixed).
- `deploy_trust gitlab { audience <a>; … }` — GitLab CI; add
  `issuer https://gitlab.example.com` for self-hosted.
- `deploy_trust oidc { issuer <url>; audience <a>; … }` — any OIDC
  provider (CircleCI, Buildkite, k8s, …).
- `deploy_trust local { public_key <path>; audience <a> }` — a key you
  control, for a laptop or a runner with no OIDC. Generate it with
  `motel deploy-keygen`, mint tokens with `motel deploy-token`.

[docs/outside-github.md](../docs/outside-github.md) walks through the
`gitlab`, `oidc` and `local` presets end to end: a key's two halves,
a job on another CI, and the token's format for minting it elsewhere.

Sub-directives: `audience` (required for OIDC — never trust an
unaudienced token; for `local`, the name of this box, see below),
`claim <name> <value>` (exact-match, repeatable — pin `repository`,
`ref`, `environment`, etc.; a claim named `@id` is refused, because
Caddy deletes that key from the config before motel reads it),
`subject` (sugar for `claim sub`),
`issuer` (oidc/gitlab), `public_key` and `max_token_ttl` (local).

A `local` token is a bearer credential for as long as it lives, and
nothing on the box revokes one short of removing its key. So a
`local` block accepts a token only while its lifetime, `exp - iat`, is
at most `max_token_ttl <duration>` (default `24h`): a token without
`iat`, or minted to live longer, is refused, and the journal's
`refused` names the directive. `motel deploy-token` mints for
`--ttl` (default `5m`) and warns above an hour. Mint one per deploy;
a job that cannot raises the bound on its block.

The OIDC presets also **require an identity claim** — one of
`repository`/`repository_id`/… (github), `project_path`/`project_id`/…
(gitlab), or `sub` (oidc). An audience alone is not identity: any
repo/project on the issuer can mint a token for any audience, so a
source with only an audience would authorize the whole issuer. Config
load fails without one.

Two shapes load but are wider than they read, and config load warns
about each (`motel validate` shows it). A `github` or `gitlab`
block that pins an identity but no branch accepts a token minted on
**any** branch the identity admits — one repository's, or every
repository's under a pinned owner or namespace — a workflow edited on
a feature branch deploys as readily as `main` — so pin the branch: `claim ref
refs/heads/main` on GitHub, `claim ref_path refs/heads/main` on
GitLab, where a bare `ref` is a name a branch and a tag can share and
so counts only beside a `claim ref_type` (`branch`, or `tag` to pin a
tag). What else counts as
that binding: `sha` (one commit), `workflow_ref` (GitHub, which
embeds the caller's ref), a `ref_protected` not pinned `false`, or a
`subject` in the provider's default form (which carries the ref). An
`environment` on its own — or GitLab's `environment_protected`, which
says who may deploy to one, not from where — is noted at info level
rather than warned about: it binds only as far as that environment
restricts its deployment branches on the provider side, which the box
cannot read; a job on any branch may declare one otherwise. A claim whose
placeholder resolved empty admits no token at all, and is warned
about as that. The generic `oidc` preset is not read: it requires
`sub`, whose shape is the issuer's. A global block that every app
overrides is not read either: it then backs only the unknown-app
path, through which nothing deploys. A `local` block without an
`audience` accepts a token minted for any box that trusts the same
key, so give each box its own audience and mint with `--audience` to
match.

`Authorization: Bearer` is the only accepted transport — Caddy redacts
it from access logs automatically.

### Multiple sources (teams)

`deploy_trust` is repeatable, and a request is authorized if **any**
block accepts it. That is how a team combines CI with per-developer
break-glass keys — think of the `local` blocks as an `authorized_keys`
list. This is the global options' `liveswap` block, not a whole file:

```caddyfile
liveswap {
	artifact_allowlist api.github.com/repos/your-org/

	# Everyday path: CI deploys on merge, no secret anywhere.
	deploy_trust github {
		audience blog
		claim repository your-org/blog
		claim ref        refs/heads/main
	}
	# Break-glass: each dev registers their own public key (they keep
	# the private half on their laptop). Revoke one by deleting its
	# block; no shared secret, no effect on the others. The audience
	# names this box (its deploy hostname), so a token minted for
	# another box that trusts the same key is refused here; mint with
	# `--audience blog.example.com --subject alice`.
	deploy_trust local {
		public_key /etc/motel/alice.pub
		audience blog.example.com
		subject alice
	}
	deploy_trust local {
		public_key /etc/motel/bob.pub
		audience blog.example.com
		subject bob
	}

	app blog {
		command node server.js
	}
}
```

A `github` block's `audience` is what the job must ask GitHub for. The
templates' `scripts/deploy.sh` asks for `motel`, so `audience blog`
above needs `MOTEL_AUDIENCE: blog` in the job's `env:`, beside
`MOTEL_URL`, so the rollback step gets it too, or the box answers
401.

Each dev runs `motel deploy-keygen` once and hands you the `.pub`
(public — safe to share); you add a block and reload. Use OIDC in CI
wherever the runner has it: a `local` key there stores a long-lived
private key in CI secrets — the thing OIDC exists to avoid. A runner
with no OIDC is what a key in CI is for
([docs/outside-github.md](../docs/outside-github.md)).

Every authorized deploy records **who authorized it**: the label of the
source that accepted the token, then what the token says about where
the deploy came from — `repository`, `ref` and `actor` for `github`;
`project_path`, `ref` and `user_login` for `gitlab`; `sub` for `oidc`
and `local`. A claim the token lacks is left out, and a value with a
space or a quote in it is quoted. The same string is `via` on the
`deploy authorized` log line and `deployed_by` in the status JSON and
in [deploy records](#deploy-records):

```
oidc:https://token.actions.githubusercontent.com repository=your-org/blog ref=refs/heads/main actor=alice
local:/etc/motel/alice.pub sub=alice
```

That is your audit trail. For a `local` block the key path says whose
key it was; `sub` is whatever the key's holder minted the token with
(`motel deploy-token --subject`), unless the block pins `subject
<name>`, which makes it a name the box checked.

A refused token is recorded too — in the journal only. The response
is the same flat 401 whatever the reason: anyone with a GitHub
repository can mint a token for audience `motel`, so an answer that
named the failed check would tell them which apps exist and what each
block pins. The `webhook auth failed` line carries `refused`: one
entry per source, in config order, each the source's label and what
it refused — the signature, `exp`, a local token's lifetime over
`max_token_ttl`, the audience, or a `claim` that did not match, with
the identity the token presented:

```
"refused":"oidc:https://token.actions.githubusercontent.com: claim \"repository\" mismatch, presented repository=your-org/other ref=refs/heads/main actor=alice; local:/etc/motel/alice.pub: go-jose/go-jose: error in cryptographic primitive"
```

A request with no bearer token says so instead, as does an app with
no source at all. The box failing to consult the issuer — its OIDC
discovery, or fetching its signing keys — lands here as well: still a
401 to the caller, with the network error in the journal. motel
holds one copy of each issuer's discovery and keys for every source
that trusts it — every app's, the global ones, the box's — per
`allow_insecure_http` setting (the box always fetches over https, so
with that option set on liveswap an issuer is held twice), and no
token waits on fetching them. When motel starts or reloads it runs
each issuer's discovery and fetches its keys again in the background;
on a reload the keys it already holds stay in use meanwhile, so a
reload refuses no token it would have accepted before, unless it adds
an issuer or changes liveswap's `allow_insecure_http` (a copy motel
holds nothing for yet). A token that arrives before an issuer's first
discovery and fetch have ended — at start, or after such a reload —
is refused with `oidc discovery for <issuer>: under way,
so a retry once that ends may go through` (or the key set's `which
they are being fetched for`) in `refused`. A discovery that failed is tried again by the
next reload, or by a token at most once a minute. After that the keys
are fetched only for a token signed under a key motel does not
hold yet, at most once a minute per copy, and that token is refused
at once rather than wait: with `the token is another issuer's, or is
under a new key, which they are being fetched for, so a retry once
that ends may go through` when it began a fetch, and with the last
fetch's error if that failed. (A token from an issuer the source does
not trust gets the same text: the signature is checked before the
issuer, so check the source's `issuer` first if a retry is refused
too.) So the first deploy after the issuer rotates its key is
refused, and its retry a few seconds later goes through — unless the
keys were fetched in the minute before, when the retry is refused too,
with `… fetched too soon for; a token from <time> on fetches them
again, as does systemctl reload motel now` in `refused`; after an
outage,
the first token past the minute is refused with the outage's error and
the next goes through. The templates' scripts (`scripts/deploy.sh`,
and the box repository's `bin/apply`) send a token the box refused
once more, 3 seconds later, so a rotation usually costs a run
three seconds, not a re-run. A bearer token longer than 8 KiB is
refused unread (`bearer token is <n> bytes, longer than 8192` in
`refused`); a GitHub or GitLab token is 1–2 KiB. A forged token under a key the
box does hold costs it no request to the issuer, which is also why an
issuer that replaces a key but keeps its kid is not picked up: tokens
under the new key are refused, with `signature does not match the key
held for kid "<kid>": the token is another issuer's, or, if the issuer
replaced that key, systemctl reload motel` in `refused`, until a
fetch for another kid or that
reload fetches the issuer's keys again. Each
entry's reason
is one line, cut at 300 bytes plus an ellipsis — an issuer's error
quotes what the token carried, and a presented identity can be
anything the issuer signed; the reason
comes before the identity, so the cut never takes it — after the
source's label, which is your config and is not cut. The line count
is bounded by the throttle below: past an address's ten failures a
minute, or the process's hundred, this line is not written; the
answer is the same 401 either way. A source the box
could not consult has a line of its own outside those budgets:
`webhook auth could not consult a trust source`, once a minute per
source while it lasts, whether the token was then refused or another
source accepted it — so an issuer outage, or a fallback source
quietly carrying every deploy through one, never leaves the journal
silent about it.

## Webhook API

`POST https://deploy.example.com/<app>`, with `Authorization: Bearer
<JWT>` (see `deploy_trust` above). There are three ways to supply the
release, all through the same endpoint and the same auth:

**1. Pull from a URL** (the default — a JSON body):

```json
{
  "url": "https://github.com/your-org/blog/releases/download/v1.4.2/blog.tar.gz",
  "version": "v1.4.2",
  "auth_header": "Bearer <token-for-private-assets>",
  "sha256": "<the tarball's digest, as sha256sum prints it>"
}
```

`auth_header` is optional and is sent verbatim as `Authorization` on
the artifact download. A redirect carries it only while each hop's
host is the URL's host or a subdomain of it; the first hop outside
that domain drops it for good, so GitHub's S3 redirect works. The URL
must pass `artifact_allowlist`.

`sha256` is optional too: the tarball's digest, 64 hex characters
(`sha256sum blog.tar.gz`). The box hashes the download as it streams
and refuses a mismatch with a `422` naming both digests, so a host
that serves other bytes than CI built cannot deploy them. Without it,
the download is trusted on the strength of the allowlist and the
token alone. The example workflows send it; a push takes no pin,
because its bytes are the request itself — the box hashes them as
they land, and that digest is the push's pin. The pin is kept as `sha256`
in the version's deploy record and in `last_deploy` (see
[Deploy records](#deploy-records)), so "are the bytes serving the ones
CI built?" can still be answered after the journal has rotated, for as
long as the record is kept — with the exceptions listed there (a pin
the filter redacts, and a rollback onto a re-deployed version).

**2. Push an uploaded tarball** — no artifact host needed. Stream the
`.tar.gz` as the request body with a gzip content type; the version is
a query parameter:

```sh
curl --fail-with-body -X POST -H "Authorization: Bearer $JWT" \
  -H "Content-Type: application/gzip" --data-binary @blog.tgz \
  "https://deploy.example.com/blog?version=v1.4.2"
```

The upload is capped at `max_artifact_size`. No `artifact_allowlist` is
consulted (there is no URL to pin — the bytes come straight from an
authenticated caller), and there is no SSRF surface on this path. This
is the path for deploying a local build directly (a laptop, an
air-gapped or egress-locked box) without hosting the artifact anywhere.

**3. Roll back to an on-disk release** — relaunch a version still on
disk (retained by `keep`), with no fetch or upload:

```sh
curl --fail-with-body -X POST -H "Authorization: Bearer $JWT" \
  "https://deploy.example.com/blog?rollback=v1.4.1"
```

Rollback runs the same blue/green pipeline (start, health-gate, cut
over), so it is zero-downtime too. A `422` is returned if that version
is no longer on disk. To see what you can roll back to, `GET /<app>`
returns `available_versions` — the on-disk releases, the running
version first, then most recently deployed first: the order the next
deploy's release GC keeps them in (`keep` of them after a successful
deploy) — and `deploys`, what each recorded version's latest deploy
came to (see [Deploy records](#deploy-records)). A version's deploy
time is its record's: the latest deploy or rollback of it. One whose
latest deploy failed (a rollback to it that failed) comes after every
one whose latest succeeded: it never served from that deploy, and
takes no place from a version that did. A release with no record —
deployed by a motel older than records, or its record could not
be written — sorts after every recorded one, ordered among those by
its directory's modification time (which the app running from it can
change), and `ordered_by_mtime` names it: it is the first release GC
prunes after the running one and the one before it, and a rollback
target while it is on disk. A release whose record is there but cannot be read is
listed the same way, and release GC holds it (neither deletes it nor
counts it toward `keep`) and says so in the journal until the record
can be read. During a version's first deploy, the version being
promoted has no record yet: until the deploy returns, the status lists
it first, as the running version, and names it in `ordered_by_mtime`.

**Versions are immutable.** A deploy (URL or push) never overwrites an
existing on-disk release, so a version you can roll back to can't be
silently replaced — re-deploying an existing version with other bytes
is a `422`. Use a new version, or roll back to relaunch an existing
one. (A deploy that *fails* before cutover is cleaned up, so that
version stays retriable — one a motel restart cuts short too, at
the next start.) The running version is the one exception,
relaunched rather than refused when the artifact is the same — see
[Relaunching the running version](#relaunching-the-running-version).

The response is synchronous:

| Code | Meaning |
|---|---|
| 200 | Deployed; body is the app's status JSON. Also every streamed deploy (below), whatever its outcome |
| 400 | The body could not be read, or is not valid JSON |
| 401 | Bad or missing token — or an unknown app, until the token authenticates against the global sources. The same flat answer whatever the reason and however often the address has failed; the journal's `webhook auth failed` line says why (above), within the throttle's budgets ([Secrets and logs](#secrets-and-logs)) |
| 404 | Unknown app, once authenticated against the global sources (unauthenticated, an unknown app is the 401 above) |
| 405 | A method other than `GET` or `POST` |
| 409 | A deploy is already running for this app (retry) |
| 413 | Pushed upload exceeded `max_artifact_size`, or a JSON body exceeded 64 KiB |
| 422 | Bad request — missing/invalid version, the running version asked for again without a matching digest (see [Relaunching the running version](#relaunching-the-running-version)), **version already exists** (versions are immutable — deploy a new version or roll back to relaunch it), a rollback target no longer on disk, a pre-flight refusal (the command or `pre_start` does not resolve inside the sandbox view, or is an executable built for another machine — the body names the box's architecture and the `runs-on` to build on — or a 32-bit one, which names the architecture to build for), or (URL path) an artifact url refused by `artifact_allowlist` (host, path, port, or an undeclared query parameter; the body names exactly what tripped and how the entry would declare it) |
| 5xx | Deploy failed — **the old version, if there was one, is still serving**; body says why. An artifact over `max_artifact_entries` or the decompressed byte cap fails here, before anything is written |

Because the response is synchronous through the whole pipeline, the
POST's wall time includes the health soak, the `drain` pause and the
old version's graceful stop — with defaults, a healthy deploy answers
in roughly soak + drain (~20s). Budget your CI step timeout for
`deadline` plus one `health_interval` and two `health_timeout`s (the
gate is judged once per interval, and the tick that completes the soak
still probes), plus drain and grace — and a second `deadline` when
`pre_start` is configured, since the pre_start step gets a full
`deadline` of its own before the health gate starts. Expect a
concurrent deploy to 409 until the first one finishes. A client that
hangs up (a cancelled job, a step timeout) does not stop a running
`pre_start`: a migration stopped midway would leave the app's data
half-changed, so it runs to its end, still bounded by `deadline`, and
the deploy then fails before any traffic moves. Until then a retry,
a rollback included, gets the 409. To end a migration that hangs
sooner than its `deadline`, stop its unit yourself; it is named
`motel-<app>.<version>.<nonce>.prestart.service`. For example, as root:
`runuser -u motel -- env XDG_RUNTIME_DIR=/run/user/$(id -u motel) systemctl --user stop 'motel-blog.*.prestart.service'`. The deploy
then fails as a failed `pre_start`.

A motel stop or restart (a crash, `systemctl restart motel`, an
upgrade) does not stop a running `pre_start` either: the manager runs
it on, bounded by `deadline` on its unit, and the motel that comes
back leaves it to finish. Until it has, that app takes no deploy (the
refusal names the migration) and no relaunch; the running version
serves on. The deploy itself ends with the motel that ran it (its
client sees the connection drop): the next start, or the next deploy,
removes the release it extracted and records it failed — `the deploy
was cut short: motel stopped or restarted during it` — so the same
version can be deployed again. Re-run the job.

**A failed deploy says why, from the app's side.** The 5xx body's
`status.last_deploy` carries the phase it stopped in, `phases` — each
phase the deploy passed through with how long it took — and, once a
launch happened, `detail`:

- `exit` — how the process ended, from systemd: `exit status 3`,
  `killed by signal 9 (killed)`. The `pre_start`'s when that failed;
  the app's when its start job failed or it died before becoming
  healthy.
- `probe` — the last health probe that completed with a non-2xx
  answer: its `status`, a redirect's `location`, and the first 512
  bytes of its `body`, which is where a framework says "Invalid
  HTTP_HOST header" or a migration says "no such table".
- `log_tail` — the last lines the `pre_start` and the app wrote to the
  journal since the deploy began, up to `deploy_log_lines` of them and
  8 KiB; `log_tail_truncated` when a cap dropped some, `log_tail_error`
  when the journal could not be read. journald stores a line it reads
  after its writer has gone — a child's last words, or any line while
  journald is behind — without the unit's name; the tail finds such
  lines by the unit's output stream, which journald names and only the
  unit's processes write to, once any line of that stream carries the
  unit's name or was written by the main process of the `pre_start`
  or app that failed. A unit none of whose lines does either leaves
  its lines out: a wrapper script that runs the app without `exec` and
  prints nothing itself, when the app prints its error and exits; a
  `pre_start` that succeeded, when journald read all of it late.
  `journalctl -t motel-<app>` on the box has every line.
  `journalctl` reads the tail; journald keeps a system user's output
  in the system journal, so the packaged unit gives motel's own
  process the `systemd-journal` group
  (`SupplementaryGroups=` in `motel.service` — the process, not
  the account, so the apps do not get it). A raw-binary install
  without that grant gets `log_tail_error` instead of a tail.

Every string in it passes the response filter first — `[redacted:KEY]`
where an `env_file` value was, and `redacted_env` naming the keys —
because the response lands in a CI log, and `deploy_log_lines 0`
keeps the app's output on the box entirely: no `log_tail`, and no
`probe.body` either (the probe's `status` and `location`, and `exit`,
are motel's own observations and stay). See
[Secrets and logs](#secrets-and-logs). A success carries `phases` and
no `detail`.

`GET /<app>` (same bearer token) returns status: phase, current
version, socket, pid, `command` — the argv the running instance was
actually launched with, read back from systemd, which is not
necessarily what the config says now (a reload does not restart a
running app, so an edited `command` applies at the next launch: a
deploy, a rollback, or a relaunch after a crash, a sustained health failure, or a reboot). It is the
*rendered* argv, not the configured text:
`command ./server {version}` reports as
`["/var/lib/liveswap/blog/releases/v1.4.2/server", "v1.4.2"]`, since
the unit runs an absolute resolved path with the placeholders already
substituted. Compare it against the release it should be running, not
against the directive. Anything you put in `command` is readable by
anyone who can read status, so keep secrets out of it: they belong in
`env_file`. The environment, inline `env` included, is not in status. Then: last deploy result (including `deployed_by`,
the `sha256` it was pinned to, and the artifact's `artifact_entries` /
`artifact_bytes` against the caps),
the watchdog's state (restart counts, last restart cause), and
`available_versions` — the on-disk releases you can roll back to,
the running version first, then most recently deployed first;
`ordered_by_mtime` names any placed by their directory's mtime
instead (see **3. Roll back** above).

The tarball's contents must sit at the archive root (`tar -czf
app.tar.gz -C dist .`), with versions matching `[A-Za-z0-9._-]{1,64}` and not starting with a dot.

**The bodies as Go types.** Every body this section describes is a
type in [`liveswap/wire`](wire/), which imports only the standard
library, so a client decodes them without linking Caddy: `Status`
(`GET /<app>`, and a deploy's 200), `Failure` (a failed deploy's 5xx:
`error`, and the `status` after it), `Error` (every other refusal,
`{"error": …}`), `DeployResult` (`last_deploy`, and
`GET /<app>?deploy=<version>`), `PhaseEvent` and `DoneEvent` (a
streamed deploy's lines, below), `Metrics` (the status's `metrics`,
over one of `MetricsRanges`) and `BoxStatus` (the box webhook's
`GET /`). The handlers marshal these types (a streamed line is the
filtered body with the event's fields appended); the order of their
keys is part of the shape, and `liveswap/testdata/wire` pins every body byte
for byte. A body the filter changed gains a top-level `redacted_env`
(see [Secrets and logs](#secrets-and-logs)), which only `DeployResult`
has a field for.

### Streaming the deploy

Send `Accept: application/x-ndjson` on any deploy or rollback POST and
the response is one JSON line per phase as the pipeline enters it,
then the body the single response would have carried, with two fields
added — `"event":"done"` and `http_status`, the code that response
would have had:

```
{"at":"2026-09-14T15:02:11Z","event":"phase","phase":"downloading"}
{"at":"2026-09-14T15:02:12Z","event":"phase","phase":"extracting"}
…
{"app":"blog","current_version":"v1.4.2",…,"event":"done","http_status":200}
```

The stream begins with the first phase, and from then on the status
line is `200` whatever happens — it went out before the outcome
existed — so a streaming client reads `http_status` from the last line
and nothing else: a failed deploy is `"http_status":500` with the
`error` and the old version's `status`, exactly as the single response
would have said. A request refused before any phase (401, 404, a 409
because a deploy is running, a 422 on the request or the version) has
nothing streamed yet and is the usual single response with its real
code. Every line passes the response filter. The examples'
`scripts/deploy.sh` streams, prints each line as it arrives, and exits
on the last line's code — or on the real one when nothing streamed.

### Relaunching the running version

A rollback to the version that is running, or a deploy of it that is
for the same artifact, relaunches it: the same pipeline from
`preparing` on, with no download or extraction and no `pre_start` — a
second instance of the installed release on a fresh socket, soaked,
promoted, the first drained and stopped. Zero downtime, as any deploy,
and how a changed `env_file` value or app definition reaches the app
without a new version (a reload never restarts a running app). "The
same artifact" is decided by digest, with nothing fetched: a URL
deploy's `sha256` pin, or the digest of a push's upload, must equal
the `sha256` in the version's deploy record. A request without a
digest, a version whose record has none (an unpinned pull, or a push
made by a motel older than this one, before pushes were hashed), or
a different digest is a `422` saying so and naming the way in — a
rollback to the running version needs no digest, since it is the same
release by definition. A URL deploy's `url` still has to pass the
scheme and `artifact_allowlist` checks a fetch gives it, though
nothing is fetched. The response is the status as for any deploy,
with `last_deploy.note` set: `relaunched v1.4.2 (same artifact); it
reads its env file now` (the second clause when the app has an
`env_file`). A relaunch is not a new deploy: the version's record
keeps the deploy that put it there, with its pin and times, and
`deploys` gains no entry. The cost is the cost of any deploy —
in-memory state goes — now also paid by an accidental re-run or a CI
retry that sends the same artifact again; and for the overlap the two
instances share the release dir (it is the working directory, bound
read-write), as two versions already share `HOME`, so an app that
keeps state in its release dir rather than under `HOME` may see its
two instances contend there.

### Deploy records

Every deploy writes its outcome — the same `last_deploy` object, with
its `phases` and, for a failure, its `detail` — to a record for that
version, and `GET /<app>?deploy=<version>` reads it back:

```sh
curl --fail-with-body -H "Authorization: Bearer $JWT" \
  "https://deploy.example.com/blog?deploy=v1.4.1"
```

The record is the latest deploy of that version (a rollback to it
replaces it, unless it is the version running: a relaunch keeps the
record of the deploy that put it there; a request refused before any
phase — a version already on disk, or the running one asked for with
another artifact or none to compare — writes none); a version never
deployed is a `404`. `GET /<app>` lists
every recorded version's outcome in `deploys`, newest first:
`version`, `status`, the failing `phase`, `deployed_by`, the `sha256`
the deploy was pinned to, and the times. `sha256` is what the
deployer asserted and the download was checked against, or the digest
of the bytes a push uploaded; absent for an unpinned pull (and for a
push made by a motel older than this one). A rollback copies the
pin from the record it replaces: a version's release cannot be
replaced while it is on disk, and its record is normally written
with it. Not always — a version whose release GC pruned can be
deployed again, and if that deploy left bytes on disk without
rewriting the record (the record write failed, or a cleanup did),
a rollback carries the old pin onto the new bytes. A pin equal to an
`env_file` value is redacted like any other, and a rollback then
records none. It is never a hash the box took of an unpinned
download. Records are kept for every version still on disk plus the
newest `keep` others (failed deploys, whose release is removed at
once, and versions release GC has pruned), so the set is bounded by
about twice `keep`. When a record was written is its version's deploy
time: the order of `available_versions` and of release GC. They are
written as the response filter left them
— a secret rotated later is not in an old record — and pass it again
when read, like every body. One consequence: a version name equal to
an `env_file` value is that value wherever it appears —
`current_version`, `available_versions`, `ordered_by_mtime`, `last_deploy`, `deploys`, a
record — and shows as `[redacted:KEY]`, and such a version's record
is only the envelope (version, outcome, times, the pin, and why); so do not
name versions (or apps) after `env_file` values; a release identifier
an app needs belongs in inline `env`. The outcome words — `succeeded`, `failed`,
the phase names — are never redacted where they stand as an outcome,
whatever `env_file` holds.

## Secrets and logs

What liveswap does for you:

- **Every webhook response is filtered before it leaves the box.** A
  response lands in a CI log — readable by everyone with read access
  to the repository, retained, public for a public repo — so nothing
  reaches it unfiltered. motel knows the app's secrets by value (it
  built the environment from `env_file`), so the first layer is exact:
  every `env_file` value of 8+ characters, as written, JSON-escaped,
  base64, hex or URL-escaped (and a URL value's password on its own),
  becomes `[redacted:KEY]`, and the response gains
  `"redacted_env": ["KEY", …]` naming what it contained. Then
  credentials with a recognisable shape (JWTs, bearer tokens, URL
  userinfo and query strings, provider key prefixes, PEM blocks,
  `…PASSWORD=`-style assignments) are replaced by name, and finally
  any 20+-character run that looks generated — base64 or hex alphabet,
  mixed, high entropy for its length — becomes `[masked, N chars]`
  (an assignment keeps its key: `BUILD_ID=[masked, 24 chars]`). The
  `env_file` values are replaced before the tail and the probe's body
  are cut to their caps, and the token a read's own bound cut in two
  is dropped, so a cut never leaves a fragment of one (a value with a
  space in it, cut by the read bound, is the one exception). The versions the
  status names and the app's own paths are exempt from the heuristics,
  so a git SHA used as a version survives — from the heuristics only:
  one equal to an `env_file` value is redacted like the value. Two
  consequences for what
  you put where: `env_file` is for secrets, and every value in it of
  8+ characters is scrubbed from responses, so a non-secret there (a
  path, a hostname) gets scrubbed too and belongs in inline `env`
  instead; and inline `env` values are never treated as secrets,
  because the Caddyfile lives in a repo. While an app's `env_file`
  cannot be read (a rewrite mid-flight, a mode not yet fixed), every
  response about that app is withheld, since its values are unknown
  to the filter; the body says so, and the next response after the
  file reads is normal. Past the first layer it is best effort, and
  the limits are in DESIGN-threat-model.md.
- Deploy logs record the artifact **host only**; download errors go
  through a redactor that drops credentials and query strings (where
  presigned-URL and token secrets live).
- Deploy auth stores no secret on the box: the config holds only an
  OIDC issuer + claim allowlist, or a public key. The deploy token
  arrives per request as `Authorization: Bearer`, which Caddy redacts
  from access logs automatically.
- The packaged systemd unit deliberately does **not** use `--environ`
  (unlike Caddy's dist unit), so ACME DNS tokens and any other
  supervisor secrets never land in the journal — journals get pasted
  into bug reports. The package smoke test asserts this.
- Failed webhook authentications are **throttled in the journal**.
  Nobody can guess a token — forging one needs a private key — so the
  throttle is not about guessing; it bounds what an unauthenticated
  flood can write to your journal. Per client address, 10 failures a
  minute are logged; further ones from that address are not, until the
  oldest failure ages out, and one line records that the address is
  throttled. Process-wide, 100 failures a minute
  are logged however many addresses a flood comes from, again with one
  line saying the budget is spent; that process budget governs every
  line about a refusal, so once it is spent an address is throttled
  silently. The throttle never changes the answer: every refusal is
  the same flat 401, so there is nothing for a client to wait out.
  The one line outside both budgets names a trust source
  the box could not consult, once a minute per source: that is the
  box's failure, bounded by your config, not by callers. The
  address is Caddy's `client_ip`
  (so `trusted_proxies` is honoured), IPv6 keyed by /64. The token is
  still verified for a throttled address: a valid one is admitted and
  clears the address, so sharing an address with a flood (a NAT, a CI
  egress pool, a proxy without `trusted_proxies`) never costs a
  deploy. Fixed, not configurable, and it survives config reloads.

What's yours to handle:

- Keep the **local signing key** (`deploy_trust local`) off the box —
  it belongs on the machine that mints tokens. The box needs only the
  `.pub`. (CI OIDC avoids a stored key entirely.)
- Your app's **stdout/stderr go to the journal** under the identifier
  `motel-<app>` (`journalctl -t motel-blog`, or by unit name —
  the status endpoint reports it). If your app prints its own secrets
  at startup, they end up in the journal — that one's on the app.
- **A failed deploy's response carries the app's last lines** (up to
  `deploy_log_lines`, 40 by default), the process's exit status and
  the last health probe's answer, after the response filter. The
  filter knows every `env_file` value and replaces it; what it does
  not know — a secret the app fetched at runtime and printed in a
  low-entropy form — is caught only by the heuristics. Two things
  follow. Keep secrets in `env_file`, where the filter can see them.
  And with a **public repository**, the Actions log is public: if
  your app's output is not something you would post, set
  `deploy_log_lines 0` and read the journal on the box instead.

## Deploying from CI

### GitHub Actions

No deploy secret to store — the job mints an OIDC token per run
(matching a `deploy_trust github { audience motel; claim repository
your-org/blog; claim ref refs/heads/main }` block on the box):

```yaml
permissions:
  id-token: write               # required to mint the OIDC token
  contents: write               # (for the release upload below)

steps:
- name: Build artifact
  run: |
    npm ci && npm run build
    tar -czf blog.tar.gz -C dist .

- name: Upload release
  id: release
  uses: softprops/action-gh-release@v2
  with:
    tag_name: build-${{ github.run_number }}
    files: blog.tar.gz

- name: Deploy
  env:
    ASSET_URL: ${{ fromJSON(steps.release.outputs.assets)[0].url }}
    GH_TOKEN: ${{ github.token }}
  run: |
    JWT=$(curl -fsS -H "Authorization: Bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
      "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=motel" | jq -er .value)
    echo "::add-mask::$JWT"
    SHA256=$(sha256sum blog.tar.gz | cut -d' ' -f1)
    curl --fail-with-body --max-time 900 -X POST \
      -H "Authorization: Bearer $JWT" \
      -d '{
        "url": "'"$ASSET_URL"'",
        "version": "build-${{ github.run_number }}",
        "auth_header": "token '"$GH_TOKEN"'",
        "sha256": "'"$SHA256"'"
      }' \
      https://deploy.example.com/blog
```

`ASSET_URL` is the uploaded asset's API URL
(`https://api.github.com/repos/your-org/blog/releases/assets/<id>`),
so the box needs `artifact_allowlist api.github.com/repos/your-org/`:
hosts match exactly, and `github.com/your-org/` does not admit it.
(`jq -er` fails the step if the mint returned nothing, instead of
sending an empty bearer and blaming `deploy_trust`.)

`SHA256` is the digest of the tarball this job built, taken from the
runner's own copy: the box refuses the asset unless it hashes to the
same, so nothing between the upload and the deploy can swap it.

(`auth_header` is a separate, artifact-download credential — the token
that reads a private release asset — not the deploy token. It is the
job's own token, which expires when the job ends.)

(For public repos, the plain `browser_download_url`
(`https://github.com/your-org/blog/releases/download/<tag>/blog.tar.gz`)
works with `artifact_allowlist github.com/your-org/` and needs no
`auth_header`.)

Mint and use the deploy token in the one step. Interpolating it into
a later step's `run:` script (`${{ steps.….outputs… }}`) prints it in
the job log, and until it expires anyone who reads the log can deploy;
if a later step must have it, pass it through `env:` and keep the
`::add-mask::`.

A re-run of a job that deployed sends the same version again. With
the same tarball — its digest equals the one on record — the box
relaunches the running version from its disk ([Relaunching the
running version](#relaunching-the-running-version)); with other bytes
under that version it is a `422` — versions are immutable — so a
re-run never deploys other bytes under a version the box has. The
example workflows rebuild the tarball on a re-run, and `tar -czf`
stamps it with the build time, so their re-run is the `422`; to
relaunch the running version from the Actions tab, **Run workflow**
with it as the rollback target. A URL or push deploy that fails has its extracted
release removed, so its version can be deployed again (a failed
rollback leaves its on-disk release alone) — unless the 5xx body says
`release <version> left
on disk` (the failed instance could not be confirmed stopped, so the
dir is kept rather than pulled from under a process that may still
run) or `cleanup of failed release` (removing it failed). Either way
that version stays a 422 until release GC — which runs after a
*successful* deploy and keeps the `keep` most recently deployed — drops it; use
a new version.

### GitLab CI

```yaml
deploy:
  stage: deploy
  id_tokens:
    MOTEL_JWT:               # verified by `deploy_trust gitlab { audience motel; claim project_path your-org/blog; claim ref_path refs/heads/main }`
      aud: motel
  script:
    - tar -czf blog.tar.gz -C dist .
    - |
      curl --fail-with-body --header "JOB-TOKEN: $CI_JOB_TOKEN" \
        --upload-file blog.tar.gz \
        "$CI_API_V4_URL/projects/$CI_PROJECT_ID/packages/generic/blog/$CI_COMMIT_SHORT_SHA/blog.tar.gz"
    - |
      SHA256=$(sha256sum blog.tar.gz | cut -d' ' -f1)
      curl --fail-with-body --max-time 900 -X POST \
        -H "Authorization: Bearer $MOTEL_JWT" \
        -d "{
          \"url\": \"$CI_API_V4_URL/projects/$CI_PROJECT_ID/packages/generic/blog/$CI_COMMIT_SHORT_SHA/blog.tar.gz\",
          \"version\": \"$CI_COMMIT_SHORT_SHA\",
          \"auth_header\": \"Bearer $DEPLOY_READ_TOKEN\",
          \"sha256\": \"$SHA256\"
        }" \
        https://deploy.example.com/blog
```

The same job with the template's `scripts/deploy.sh` pushing the
tarball in the request, no registry and no read token, is in
[docs/outside-github.md](../docs/outside-github.md#3-an-apps-deploy-from-another-runner).
On gitlab.com that URL needs `artifact_allowlist
gitlab.com/api/v4/projects/<project id>/` on the box. `SHA256` is the
tarball's digest from the job's own copy, which the box holds the
download to (see the pull payload above).
`DEPLOY_READ_TOKEN` is the artifact-download credential: a project
access token with `read_api`, stored as a masked CI/CD variable. The
job token is not a substitute: `auth_header` is sent as the
`Authorization` header, and GitLab takes `CI_JOB_TOKEN` in a
`JOB-TOKEN` header, as the upload line above shows.

`--fail-with-body` makes the CI job red exactly when the deploy fails
and prints the JSON `error` that says why (plain `--fail` hides it) —
and on failure the previous version, if there was one, never stopped serving. The body's
`status.last_deploy.detail` says why from the app's side — the exit
status, the last health probe's answer, the last lines the app and
its `pre_start` wrote (see [Webhook API](#webhook-api)); the whole
journal stays on the box, `journalctl -t motel-blog`.

`--max-time 900` is the budget [Webhook API](#webhook-api) gives, with
the defaults and a `pre_start`, plus the artifact's transfer: the
`pre_start`'s `deadline` (5m) + the health gate's `deadline` (5m) +
one `health_interval` and two `health_timeout`s (9s) + `drain` (5s) +
`grace` (10s) = 10m24s, which leaves 4m36s for the download. Raise it
with any of those settings. A curl that gives up before the cutover
fails the deploy, and the old version keeps serving. A `pre_start`
still running is not stopped: it runs to its end, and the deploy lock
is held until then, so a retry gets the 409. Stopping its `.prestart`
unit ends it sooner (see [Webhook API](#webhook-api)). A curl that
gives up after the cutover turns red a deploy the box completed.

## Server layout

```
/var/lib/liveswap/blog/
  releases/v1.4.2/        one dir per deployed version
  releases/v1.4.1/
  current -> releases/v1.4.2   (convenience symlink; state.json is truth)
  shared/                 persistent data, survives deploys (the app's HOME)
  state.json              current version, nonce and unit name
  deploying.json          a URL or push deploy in flight; gone when it ends
  alerts.json             a restart loop or refused relaunch reported to notify and not over; gone when none is
  deploys/v1.4.2.json     each version's latest deploy outcome (see Deploy records)
  tmp/                    download staging
```

Inside its sandbox an instance sees only `releases/<its version>/`
and `shared/` of this tree, and its own `run/<nonce>/`, where it binds
its socket (see [Sandbox](#sandbox)).

## Semantics and trade-offs (read this)

- **Apps are systemd units, not children of motel.** Each instance
  is a transient service under the motel user's own systemd manager
  (`user@<uid>.service`, kept alive by lingering — the package sets
  this up; self-managed installs need `loginctl enable-linger
  motel` and `libpam-systemd`, and motel refuses to start apps
  without that manager). Config reloads never touch them (deploy state
  lives outside the config, reference-counted across reloads — proven
  by an e2e scenario that reloads mid-traffic and asserts the app's
  PID is unchanged) — so an edited `command` or `env` does not apply
  to the running instance; it applies at the next launch (a deploy, a
  rollback, a relaunch after a crash, a sustained health failure, or a reboot), which is why status
  reports the `command` the instance is actually running — and
  neither do **motel restarts and upgrades**:
  on start, liveswap reattaches to the unit recorded in `state.json`
  and serves it without relaunching it (what visitors see while
  motel itself restarts: [Upgrading](../docs/upgrading.md));
  only if that unit is gone (reboot, or it died meanwhile) is the
  current version relaunched. Stopping motel
  therefore leaves apps running until the next start, and a motel
  that crashes is started again by systemd a second later and
  reattaches the same way (a start it refuses is not retried); removing
  the package stops them. Removing an app (or the whole `liveswap` block)
  via a **reload** stops its units, once any deploy of it already in
  flight has finished (the reload waits for it); if you instead edit the file and
  *restart* motel with the whole block gone, nothing is left to
  judge the old units and they keep running — decommission them
  explicitly, as root:
  `runuser -u motel -- env XDG_RUNTIME_DIR=/run/user/$(id -u motel) systemctl --user stop 'motel-*'`. Units are created with `Restart=no` — the
  watchdog is the only restarter of apps — and stopping a version kills its
  whole cgroup, so worker trees never outlive it. Each unit shows a
  `sleep infinity` beside the app in `systemctl --user status`: motel
  starts every app behind a shell that forks that sibling and becomes
  the app, because systemd 257 waits an app's `grace` on a stop only
  when its process is not alone in the cgroup (#380). To the app it is
  one child it did not spawn: it sleeps for the app's whole life, dies
  at the stop's SIGTERM with everything else in the cgroup, and the app
  gets one SIGCHLD then, as a prefork master does from a worker. It
  costs one task (`tasks_max` counts it) and about a megabyte, and adds
  nothing to the app's environment.
- **Changed app definitions apply at the app's next launch** — a
  deploy, a rollback, or a relaunch after a crash, a sustained health failure, or a reboot — never by
  restarting a running app mid-reload. To give an app one now, roll
  back to the running version ([Relaunching the running
  version](#relaunching-the-running-version)).
- **No post-promote *auto*-revert.** Once traffic cuts over, the deploy
  is done; if the new version misbehaves later, roll back explicitly
  with `?rollback=<version>` (its release dir is still on disk — that's
  what `keep` is for). Everything *before* promote is automatically
  contained. The
  watchdog restarts the *same* version on crash or sustained health
  failure — it never reverts to an older one.
- **One deploy at a time per app** — concurrent webhooks get 409, and
  CI retries are the queue. Different apps deploy in parallel.
- **Single node by design.** This is for the 1-server indie stack, not
  a cluster.
- **Linux with systemd only.** liveswap talks to the systemd user
  manager over D-Bus; there is no other process runner. Development on
  macOS happens in Docker (see below).
- Deploy tokens and artifact-URL query strings never appear in logs.

## Development

liveswap lives in the [motel](../) monorepo; the make targets at the
repo root cover it (no local Go toolchain needed — everything runs in
Docker):

```sh
make test              # unit tests, all modules (race + coverage)
make test-integration  # real Caddy + real systemd units via caddytest (privileged systemd container)
make e2e               # both module suites against the motel binary
make lint vet tidy
```

## License

Apache-2.0
