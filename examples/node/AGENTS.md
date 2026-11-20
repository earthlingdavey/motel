# Working on this app

It is deployed by [motel](https://github.com/smallhoursorg/motel),
which runs it under rules that do not show up when you run it locally.

- **Listen on the unix socket in `$SOCKET`** (`server.listen(socket)`).
  On the box there is no `PORT`, and nothing may listen on TCP.
  (Locally, with no `$SOCKET`, it serves 127.0.0.1:8000.)
- **`GET /health` answers 2xx once the app can serve.** A deploy only
  goes live after it has, continuously, for the soak period.
- **Handle SIGTERM**: finish in-flight requests, then exit. It is how
  the old version is stopped after a deploy.
- **Persistent data goes in `$DATA_DIR`** (the app's `shared/` dir on
  the box). The release directory is replaced on every deploy.
- **Migrations go in `migrate.js`.** It runs before the new version
  starts; a non-zero exit cancels the deploy and the old version keeps
  serving. It must be safe to run again on a database it has already
  migrated.
- **The app ships as one executable**, built by `npm run bundle` with
  Node's single-executable build: `server.js` and `migrate.js` are
  each embedded in a copy of the Node binary. Keep them dependency-
  free, or bundle dependencies into a single file first — `require`
  of a package from `node_modules` does not work inside the
  executable.
- **Build on the box's architecture.** The executable is the Node
  binary it was built with; the workflow's runner (the variable
  `MOTEL_RUNS_ON`, amd64 when unset) must match the box.
- **Only its own dirs exist in production.** The app's sandbox holds
  its release dir, `$DATA_DIR`, the directory `$SOCKET` is in, a
  private `/tmp` and the OS runtime under `/usr`. `/opt`, `/srv`, `/home` and host unix sockets (a local
  Postgres's, say) are absent, not merely unreadable.
- **Env vars come from the box**, not this repo: `motel.caddy` here
  documents what the code reads. A new variable is a change to the
  box's Caddyfile too; say so in the change's description.

Check a change with `npm run dev`, then `npm run bundle`.
