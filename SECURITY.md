# Security policy

motel terminates TLS for your sites, holds the keys, and supervises
your application processes. Its server, moteld, runs unprivileged —
`User=motel`, with `CAP_NET_BIND_SERVICE` its one capability and the
`systemd-journal` group for its own process, to read a failed app's
journal lines — but a compromise still reaches everything it serves,
so security reports are taken seriously and handled with priority.
The package also installs root units, each with a bounded set of
capabilities: the box applier (`motel-box-apply.service`), which
installs a signed config push, and the backups (`motel-backup.service`,
and its restore drill). `moteld monitor setup` adds root work with no
capability bound: the `chmod` line of Alertmanager's drop-in, and with
`--metrics` `motel-metrics-netns.service`, which makes the metrics
store's network namespace, and the `chmod` and `systemctl` lines of
the store's drop-in.

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting:

**[Report a vulnerability](https://github.com/smallhoursorg/motel/security/advisories/new)**

This opens a private draft advisory that only you and the maintainer
can see. Please do not post anything you believe is a security
problem in a public issue or discussion — a public report on a deploy
server puts every operator at risk before a fix exists.

What helps: the config that reproduces it (redact your secrets), the
version (`moteld version`; up to v0.3.1 it names only the Caddy it
was built with, so name your motel release too), and what an
attacker gains. Proof-of-concept payloads are welcome inside the
private advisory.

## What to expect

- Acknowledgement within a few days — this is a solo-maintained
  project, so "days" is honest rather than aspirational.
- If confirmed, a fix is developed in the private advisory fork,
  released, and the advisory published with credit to you (unless you
  prefer otherwise).

## Supported versions

Pre-1.0: only the latest release receives security fixes. Upgrading a
box on the `moteld` package (v0.8.0 and later) is a package install
and a service restart: deployed apps keep running through it, but new
visitors get errors while moteld restarts, about a tenth of a second,
or up to about 5 seconds while a slow request finishes ([docs/upgrading.md](docs/upgrading.md)). So a fix costs a
quiet moment, and little more. A box on v0.7.0 or earlier, whose
package was named `motel`, cannot upgrade in place: the move is a
remove and an install, which stops every app, and the first `moteld`
release's notes describe it.

## Scope notes for researchers

- Anything reachable from the webhook endpoint is squarely in scope.
  Deploys are authenticated by a verifiable token (`deploy_trust`:
  CI OIDC or a local public key), so there is no shared deploy secret
  on the box to steal; a forged or replayed token, or a way to deploy
  outside the configured trust claims, is in scope — as is any deploy
  reaching outside `artifact_allowlist` (see
  [liveswap/README.md](liveswap/README.md)).
- The webhook endpoint is not the only surface. Among the others: the
  config channel (`box_webhook`) and its root applier
  ([box/DESIGN-box.md](box/DESIGN-box.md)); people's reads (an app's
  status and deploy records, the box's `GET /` and the routes under
  `/_motel/`), their token and their grants
  ([liveswap/access/DESIGN-access.md](liveswap/access/DESIGN-access.md));
  the backups, `motel-backup`, whose units start as root
  ([backups/README.md](backups/README.md)); notify's and the metrics
  store's unix sockets
  ([notify/DESIGN-notify.md](notify/DESIGN-notify.md),
  [notify/DESIGN-metrics.md](notify/DESIGN-metrics.md)); and the
  laptop client, `motel` ([client/README.md](client/README.md)).
- Apps deployed by liveswap share one UID, and every one of them runs
  in its own user and PID namespace behind a deny-by-default
  filesystem view
  ([liveswap/DESIGN-sandbox.md](liveswap/DESIGN-sandbox.md)); there is
  no setting that runs an app without one. A way for one app to read
  another's files, environment, sockets or `/proc`, or to escape to
  the *supervisor or system*, is in scope. Known limits: an app
  reaches its **own** secrets and database by definition, and sibling
  apps share a network namespace, so one can connect to another's
  localhost port. The full attacker/asset/attack-path analysis is in
  [DESIGN-threat-model.md](DESIGN-threat-model.md).
- Reports against a host outside the supported matrix (Debian 13) are
  welcome through the same private form, and are triaged as bugs, not
  advisories: the sandbox is probe-gated, so a host that cannot
  deliver it refuses to start rather than claiming an isolation it
  does not have.
- Vulnerabilities in Caddy itself belong upstream:
  [caddyserver/caddy security policy](https://github.com/caddyserver/caddy/security/policy).
