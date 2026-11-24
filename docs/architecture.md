# How motel fits together

Two pictures: what a request passes through inside moteld, and what a
deploy does from the push to the cutover. The picture of the whole
box, with what talks to it, is at the top of the
[README](../README.md). Each section ends with where its details are
written down.

Names are the ones [Your first deploy](first-deploy.md) uses: the app
`example` at `example.com`, and the box's deploy host,
`deploy.example.com`.

## Inside moteld

moteld is Caddy with motel's modules compiled in: one process, running
as the `motel` user, configured by one Caddyfile. Its modules are of
two kinds. Handlers stand in a site's route and see each request.
Caddy apps run beside the HTTP server and hold what the handlers ask
for.

```mermaid
flowchart TB
    net(["Ports 80 and 443: Caddy's<br/>automatic HTTPS,<br/>HTTP/1.1, 2 and 3"])

    subgraph appsite ["An app's site"]
        observe["observe:<br/>counts and times<br/>each response"]
        pb["hint_penaltybox:<br/>429 to a client<br/>in the penalty box"]
        cache["cache: a stored page<br/>is answered here"]
        rp["reverse_proxy<br/>dynamic liveswap example"]
        observe --> pb --> cache --> rp
    end

    subgraph deploysite ["The deploy host"]
        bw["box_webhook,<br/>the path / alone:<br/>a config push, its result"]
        lw["liveswap_webhook:<br/>POST /app deploys,<br/>rolls back; GET /app<br/>and /_motel/… read"]
        bw -- "every other path" --> lw
    end

    subgraph capps ["Caddy apps"]
        ls["liveswap:<br/>the deploy pipeline,<br/>the watchdog, each<br/>app's current socket"]
        mon["monitor (notify):<br/>the detectors, the alert<br/>engine, the metrics push"]
        ls -. "events,<br/>numbers" .-> mon
    end

    sock(["The running release,<br/>on its unix socket"])
    sysd(["The motel user's<br/>systemd manager:<br/>a unit per release"])
    applier(["The box applier,<br/>as root"])
    out(["Alertmanager and<br/>the metrics store,<br/>over unix sockets"])

    net -- "example.com" --> observe
    net -- "deploy.example.com" --> bw
    rp -- "the request" --> sock
    rp -. "which socket<br/>now?" .-> ls
    lw -- "token checked,<br/>action allowed" --> ls
    ls -- "D-Bus" --> sysd
    bw -- "the push,<br/>as a file" --> applier
    mon --> out
```

**An app's site.** Only `reverse_proxy` is required. `observe`,
`hint_penaltybox` and `cache` are each a line the operator adds, and
the order shown is the one a `route` block keeps: the observer first,
so that it counts what the other two answer, and the penalty box
before the cache, so that a boxed client gets no cached page. Outside
a `route` Caddy runs `cache` first
([penaltybox/README.md](../penaltybox/README.md), "Compatibility with
Souin"). The proxy holds no address for the app: for each request it
asks liveswap which socket the app's running release listens on. That
answer is the one thing a deploy changes, so a cutover needs no config
reload.

**The deploy host.** One site answers everything that is not a
visitor. `box_webhook` takes the path `/`, a config push from the
box's repository and the poll for its result, and passes every other
path on. `liveswap_webhook` takes the rest: a deploy or a rollback
from an app's CI, the status reads, and under `/_motel/` what a person
reads with the laptop client. Each request carries a token: an OIDC
token from CI, one a local key signed, or one a person's ssh key
signed. The handler checks the token first, and then whether its
holder may take that action on that app ([liveswap/access/DESIGN-access.md](../liveswap/access/DESIGN-access.md)).

**The Caddy apps.** `liveswap` owns the apps: it runs the deploy
pipeline, starts and stops each release as a transient unit on the
`motel` user's systemd manager, over D-Bus, and its watchdog restarts
a release that crashes or stays unhealthy. The units are systemd's,
not moteld's children, so they keep serving through a config reload
and a moteld restart. `monitor`, the notify module, takes liveswap's
events and each app's numbers, sends alerts to Alertmanager and pushes
the numbers to the metrics store, where the box has them set up.
`box`, not drawn, holds who may push the config and who may sign it.

**What moteld cannot do.** It runs unprivileged and cannot write its
own Caddyfile. A config push leaves `box_webhook` as a file in
`/var/lib/motel-box/in`, and the box applier, a root unit that a path
unit starts, checks the commit's signatures again, installs the file
and the secrets, and reloads moteld. The admin API is a unix socket,
`/run/motel/admin.sock`, with no TCP port, which no app can reach.

Details: [liveswap/DESIGN-liveswap.md](../liveswap/DESIGN-liveswap.md),
"Architecture"; [box/DESIGN-box.md](../box/DESIGN-box.md), "The trust
chain" and "Sequences", which draws a config push step by step;
[notify/DESIGN-notify.md](../notify/DESIGN-notify.md) and
[notify/DESIGN-metrics.md](../notify/DESIGN-metrics.md), "The shape".

## A deploy

A push to an app's `main`, as the example workflows do it on GitHub
Actions. `blog` is the app.

```mermaid
sequenceDiagram
    actor Dev as You
    participant GH as GitHub (Actions, releases)
    participant M as moteld (liveswap)
    participant N as New release
    participant O as Old release

    Note over O: serving every request
    Dev->>GH: git push to main
    GH->>GH: build app.tar.gz, upload it as a release asset
    GH->>GH: mint an OIDC token for this run (audience: the deploy host)
    GH->>M: POST /blog {url, version, sha256}, with the token
    M->>M: check the token against the app's deploy_trust
    M->>GH: downloading: fetch the asset (allowlisted host, size cap, sha256)
    M->>M: extracting: into releases/<version>/
    M->>M: preparing: pre-flight, then pre_start (migrations)
    M->>N: starting: a sandboxed systemd unit on a fresh unix socket
    loop soaking
        M->>N: GET /health, until healthy for the whole soak
    end
    M->>M: promoting: the atomic cutover
    Note over N: serving every new request
    Note over O: draining: requests in flight finish
    M->>O: stopping_old: SIGTERM to its cgroup, SIGKILL after grace
    M-->>GH: 200 OK
    Note over M,O: A failure before promoting leaves the old release serving. The webhook answers 422 or 5xx, and CI goes red.
```

The words before each colon are the deploy's phases, as the webhook's
answer and `motel status` name them. Three things the picture leaves
out:

- **Nothing secret is stored anywhere.** The token is minted for the
  one run and names the repository, branch and workflow it came from.
  The box holds only the `deploy_trust` block that says which of those
  may deploy the app.
- **The request stays open for the whole deploy**, so the workflow's
  step succeeds or fails with it.
- **After the cutover nothing reverts by itself.** The watchdog
  restarts the same release, and the releases before it stay on disk:
  `POST /blog?rollback=<version>` puts one back through the same
  phases, without the download and the migrations.

Details: [liveswap/README.md](../liveswap/README.md), "How a deploy
works", "Webhook API" and "Deploying from CI".
