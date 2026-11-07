# DESIGN — threat model

This document describes the present: what is built, why, and what it
promises. History lives in git (`git log -- DESIGN-threat-model.md`);
the "History" section at the end holds only dated one-liners.
Amendments are for decisions still fresh or contested and get folded
into the body once they settle.

This document states what motel is defending, who the attacker is,
the concrete paths from an entry point to an asset, and how the shipped
isolation closes or bounds each one.
[liveswap/DESIGN-sandbox.md](liveswap/DESIGN-sandbox.md) is authoritative
for the sandbox's behaviour specification; this document places that
work in the wider attack surface rather than restating it.

Scope: a single Debian 13 box running `motel` (a Caddy distribution)
as the `motel` system user, supervising deployed apps as transient
systemd units under the motel user's own service manager via
liveswap's `systemdRunner` (liveswap/runner_systemd.go). Backups add
one place off the box: the bucket their repository lives in
("Backups"). Multi-node, Windows, and macOS-as-a-server are out of
scope by product design.

## Assets (what an attacker is after), ranked

1. **ACME DNS tokens** — in the supervisor env; issue/alter certs. The
   highest-value item on the box. The `/proc/<supervisor>/environ`
   route is closed by the non-dumpable supervisor and by the app-side
   namespaces ("The shared-UID rule"). Still on disk wherever Caddy
   persists config (`/var/lib/motel/caddy/autosave.json`) — a
   filesystem route, closed by the sandbox view.
2. **The backup repository's credential** —
   `/etc/motel-backup/repository.env`, root 0600 in a root 0755
   directory: the repository URL, its password and the storage key.
   With the three, restic reads every app's declared data and all its
   history, what an app has since deleted included, from any machine
   (docs/backups.md, "Before you start"); on B2 the key reads old
   versions too, so what a prune removed stays readable for the 180
   days the bucket keeps it. The password outlasts the box: restic
   changes a master key only through a new repository. Ranked under
   the ACME tokens and above the TLS keys because it is every app's
   data, past and present, readable from anywhere. The motel uid
   cannot read it: root reads the file, and the `motel-backup` uid
   holds the values while a job runs ("Backups"). It holds no env
   file: a backup holds what each app declares under its `shared/`.
3. **TLS private keys** — `/var/lib/motel/caddy/**`, mode `0750`
   owned by `motel`.
4. **Admin API socket** — `/run/motel/admin.sock` (the `admin`
   directive, packaging/Caddyfile); reconfigures the whole server.
   Gated on being the `motel` user, not on the network.
5. **Per-app secrets** — an app's own env vars / `env_file`
   (`/etc/motel/*.env`). Legitimately reachable by that app; the
   goal is to keep them from *siblings*. From accounts beyond the
   file's owner and group it is the file's mode that keeps them (0640
   root:motel as documented; root reads anything); config load and
   each launch warn when the mode admits everyone (`warnEnvFileModeOf`,
   liveswap/app.go). Owner and group are not checked. Where the box's
   config repository carries an app's secrets (box/DESIGN-box.md,
   "Secrets"), the env file is
   `/etc/motel/secrets/<app>.env`, written 0640 by the `box` applier
   into a setgid `root:motel` directory, and the values also exist
   encrypted in that repository's history, which the box's key for
   secrets (`/etc/motel/age/identity`, root 0600 in a 0700 root
   directory) and each reader's key decrypt.
6. **The box's configuration at rest** — `/etc/motel/Caddyfile`:
   which repository may deploy each app, each app's command and
   flags, which hosts are served, and who may change the file itself.
   Root-owned, 0644; written only by root — `motel init`, the `box`
   applier, the console ([box/DESIGN-box.md](box/DESIGN-box.md)), and
   the package's postinstall, which puts the starter there only where
   nothing stands or while the file is byte-identical to the starter
   it last wrote. Its record of that starter's digest
   (`/var/lib/motel-package/starter.sha256`, in a 0700 root
   directory) is root's alone, so the motel uid cannot steer an
   upgrade into overwriting the box's config. A
   writer here outlasts a restart, which is what makes it an asset
   distinct from the admin socket (whose changes do not).
7. **Sibling app data** — `/var/lib/liveswap/<app>/{releases,shared,state.json}`.
8. **System integrity** — root, persistence, other system services.
9. **Availability** — serving traffic and the deploy pipeline.

There is no deploy secret on the box. Deploys are authenticated by a
verified JWT (CI OIDC or a local public key); the verifier holds only
public material — see "Reducing the asset".

## Trust boundaries and entry points

Source is cited by symbol, not line: `Type.Method` or a package-level
name, with the file it lives in named on a passage's first citation —
later symbols are in that file unless another is named. Where a claim
rests on one statement inside a long function, the prose names it so it
can be found by search.

### Webhook endpoint — `liveswap/handler.go`

The first of two application-level authenticated entry points (the
other is `box_webhook`, "Config webhook" below). Auth is a
verified JWT in `Authorization: Bearer` (see "Reducing the asset"):
`deploy_trust` sources verify the token's signature and standard claims
against public material, then a claim allowlist. Auth happens **before**
existence is revealed: an unknown app name is verified against the
*global* trust sources, so its refusal is the same 401 as a known
app's (`Handler.ServeHTTP`, liveswap/handler.go). No token waits on
an issuer for it (#306): a token is checked against each source a
name resolves to in turn — an OIDC source checks its signature, then
its issuer, audience and expiry (go-oidc's order), then the claim
allowlist — and every check is local. Each issuer's discovery and keys
are held once for the process per `allow_insecure_http` setting (so at
most twice, when liveswap's differs from the box's https-only one),
shared by every source that trusts it over that client
(`issuerState`, liveswap/deploytrust/deploytrust.go), and fetched in
the background — at start and at each reload (`Warm`), then for a
token under a kid none is held for, at most once a minute per held
state, which that token does not wait on (`keySet`,
liveswap/deploytrust/jwks.go). The time to the 401 can still differ
by that local work, and that is the **residual**, accepted by the
owner's decision (S3 × O5 × D5 = 75, #306, 2026-10-20): each source a
name resolves to parses the token and checks its signature against a
key it holds under the token's kid (or each key it holds, for a token
naming none), whatever issuer and audience the token claims, where a
name with no source does neither. A token longer than `MaxTokenLen` (8 KiB) is refused before
any source parses it, which bounds the difference: measured on an
arm64 machine (2026-10-20), an OIDC source refuses a 2 KiB token
(about a real GitHub one) in 0.11 ms and a 16 KiB one in 0.33 ms,
where a 1 MiB one, which the header limit allows, took 16 ms. A
fraction of a millisecond per source is under network jitter for a
few samples, not for many; the asset is whether an app name exists,
which a domain or repository name often says anyway. Config load refuses an app
that resolves to zero trust sources (`App.Validate`,
liveswap/liveswap.go). Bearer is the only transport, which Caddy
redacts from access logs.

Properties that matter to the model:

- **GET and POST both sit behind the token.** GET returns full status,
  POST deploys, all else 405 — the method switch in
  `Handler.ServeHTTP` (liveswap/handler.go). The status endpoint is
  authenticated — not public.
- **What the webhook says back is filtered** (liveswap/redact.go,
  which states the filter's four rules; every body passes it —
  through `RespondJSON` in liveswap/handler.go, the record route's
  `respondFiltered`, the stream's lines, the record store's write).
  The
  response's audience is wider than its caller: the paved road prints
  it into a GitHub Actions log, readable by everyone with read access
  to the repository, retained, and public for a public repository.
  The caller is trusted to run code on the box; the log's readers are
  not. A failed deploy's body carries the app's own bytes on purpose
  (liveswap/journal.go, `deployDetail` in liveswap/app.go): the last
  lines its units wrote, read back with `journalctl` — the one
  external program motel runs, with argument lists of its own
  making — bounded by `deploy_log_lines` (default 40) and 8 KiB; the
  first 512 bytes of a failing health probe's body; and the exit status
  the runner recorded. A line is the units' when journald named one of
  them on it, or when it is in one of their output streams —
  journald's `_STREAM_ID`, which no client can set and only the unit's
  processes hold — learnt from the lines that name a unit and from
  those written under the app's identifier by the main process of the
  `pre_start` or app that failed, found by its pid (as the manager
  recorded it, or as the runner follows it while it runs), which
  another process could hold only once pids wrap. `deploy_log_lines 0`
  keeps the app's bytes — the tail and the probe body both — on the
  box; the exit status and the probe's status code are motel's
  observations and stay. Reading
  the journal is a grant: journald keeps a system user's output in the
  system journal, so the packaged unit puts motel's process in
  `systemd-journal` (`SupplementaryGroups=`, the process and not the
  account, so the apps under the user manager keep the account's
  groups and see no journal in their sandbox anyway). What that
  widens is what a compromised motel reads — every unit's lines —
  which the shipped units keep free of secrets (no `--environ`, the
  smoke test asserts it). Every body passes
  four layers before it is written. A streamed deploy (`Accept:
  application/x-ndjson`) writes two kinds of line, each through the
  same filter before it is written: phase lines, generated objects
  holding a phase name and a timestamp and nothing of the app's; and
  the last line, the single response's body filtered exactly as it
  would have been, with two fixed fields — `"event":"done"` and
  `http_status` — appended after the filter so that a body the filter
  withholds still ends the stream with them. First, exact: every `env_file`
  value of 8+ characters this process has rendered for a launch (or,
  after a restart, read from the file for the filter), in each form
  the filter recognises (as written, JSON-escaped, base64 standard and
  URL alphabets padded and not, hex, URL-escaped, and a URL value's
  password on its own), becomes `[redacted:KEY]`, and the body gains
  `redacted_env` naming the keys. motel is the party that knows
  these values, which is why the filter lives here and not in CI,
  where `::add-mask::` can only hide what the job itself knows.
  Second, an allowlist: the versions the status names and the app's
  own paths are exempt from the heuristics below — never from the
  first layer: a safe string equal to a known value is dropped,
  whoever named it (a request, the config, a name read off disk), so
  a version, an app name or a path spelled as an `env_file` value is
  redacted like the value. Third, shape rules for
  credentials with a recognisable form. Fourth, entropy: a run of 20+
  characters from the base64 or hex alphabet, mixed and with Shannon
  entropy above the class's bar — one that follows the run's length,
  since a short run cannot reach a fixed bar — is replaced by
  `[masked, N chars]`, never by a fingerprint. Markers are chosen so that none contains a
  known value (a key named for its own value gets a number in
  brackets), and the first layer runs once more over the finished
  bytes — any fallback included — so the promise holds for the bytes
  written, not only for the body as filtered; the safe strings are
  held out of the heuristic layers as spans, so a version shaped like
  a token survives them. The one text outside the promise is the
  report field's own name, `redacted_env`: it is fixed, in every such
  response, and so reveals nothing; its key names are filtered. The
  other is the outcome vocabulary — `succeeded`, `failed`, the phase
  names — where it stands as a `status`, `phase` or phase `name`: a
  known form that is a bare word (a value, or a URL value's password,
  spelled as one) is replaced everywhere but in such a pair, in every
  body, so a value spelled as one of the words rewrites no outcome.
  Every other form is replaced wherever it is; a body with fewer
  such pairs at the end than it had — a value that is part of a word
  was replaced inside it — is withheld; and the same word in `error`
  or `detail` is text, and filtered. A
  body the first layer would leave unparsable (a value made of JSON's
  own punctuation matching the structure rather than a string) is
  withheld, with the keys still reported; and while an app's
  `env_file` cannot be read, its values are unknown to the filter and
  every body about that app is withheld rather than sent through the
  heuristics alone. What the filter does **not** promise: an encoding not in
  the list, a value under 8 characters, a secret split across lines,
  a secret the app fetched at runtime and printed in a low-entropy
  form, an inline `env` value (not a secret by policy: the Caddyfile
  is in a repo), or a value the still-running instance was launched
  with before a motel restart if the file has changed since.
  Tested from both sides — a fuzz property that no known value or
  form survives and the JSON stays valid (`FuzzRedactor`), and a
  corpus that the diagnostics a reader needs do (`TestRedactorCorpus`)
  — because a filter with only the first test drifts toward blanking
  everything.
- **Auth failures are throttled in the journal**
  (liveswap/deploytrust/limiter.go). Token *forgery* is infeasible (no private
  key), so this is not a guessing oracle; what
  an unauthenticated caller can do with failures is make motel
  write a Warn per request. Two sliding windows bound that, in one
  lock scope so a burst of concurrent requests cannot each see room:
  per address, 10 failures a minute are logged, and further ones are
  not until the oldest ages out; process-wide, 100 failures a minute
  are logged however many addresses a flood comes from. The budgets
  decide what is logged, the once-per-window "budget spent" lines
  included, and nothing else: every refusal answers the same flat 401
  whatever they say, so a caller cannot see them and a client has no
  reason to retry a refused token. The logged `app` field is cut to
  what a real name could be. The address is Caddy's `client_ip`
  (`trusted_proxies` honoured), IPv6 keyed by /64; the table is bounded
  at 4096 addresses (drained ones swept at most once a window, then an
  arbitrary one dropped), and the global budget is what holds above
  that. The token is still verified for a throttled address and a
  valid one is admitted (and clears the address): a NAT, a CI egress
  pool, a proxy without `trusted_proxies` or a unix-socket listener all
  collapse clients onto one key, and the throttle must never be a
  deploy-denial primitive for whoever shares it. Verification cost is
  therefore not bounded by the throttle. Against a local key it is one
  signature check, comparable to the TLS handshake the request already
  paid; against an OIDC source it is the same, or none for a token
  under a kid the process holds no key for, which begins a fetch of
  the issuer's keys in the background, at most once a minute per
  issuer, and is refused without waiting on it (`keySet`,
  liveswap/deploytrust/jwks.go). A token longer than `MaxTokenLen` is
  refused before any parse. So the throttle is not what bounds CPU, and traffic does not set how often the box
  asks an issuer anything: a token makes it try an issuer's discovery,
  while that fails, and fetch its keys, each at most once a minute and
  one at a time (`issuerState`, liveswap/deploytrust/deploytrust.go).
  One limiter for the process, not per handler, so a
  reload hands out no fresh budget and a second mount does not double
  it. Body is capped at 64 KiB → 413
  (`maxPayloadBytes`, enforced in `Handler.deployURL`,
  liveswap/handler.go); `deployMu.TryLock()` → 409 serializes deploys
  (`managedApp.Deploy`, liveswap/app.go; the push path takes it before
  staging, in `Handler.deployPush`, liveswap/handler.go).
- **Path routing is `path.Base(path.Clean(...))`**, the first statement
  of `Handler.ServeHTTP` (liveswap/handler.go): `/anything/deep/myapp`
  targets `myapp`. A naive `path /deploy/*` site matcher does not
  constrain the app name; the operator's matcher is the only constraint.
- **Three request shapes** (`Handler.deploy` dispatches on them,
  liveswap/handler.go), all behind the same token: a JSON body pulls
  from a URL (below); a gzip body **pushes** the artifact itself
  (`POST /<app>?version=`, `Handler.deployPush`) — the bytes come straight
  from the authenticated caller, capped at `max_artifact_size`, staged
  under the app's `tmp/` and extracted like a download, and **no
  `artifact_allowlist` is consulted** because there is no URL to pin;
  `?rollback=` relaunches an on-disk release. The push path has no
  SSRF surface, but it means a token-holder can deploy arbitrary
  bytes with no artifact host in the loop: the allowlist confines
  *pulls*, and only the claim scope confines *who*.
- **Pull payload:** four fields only — `url`, `version`, `auth_header`,
  `sha256` (`deployPayload`, liveswap/app.go); unknown JSON silently
  ignored (no `DisallowUnknownFields`). `version` is
  `^[A-Za-z0-9_-][A-Za-z0-9._-]{0,63}$` (no leading dot, so never
  `.`/`..` or a release-GC bookkeeping name), double-sanitized before
  touching the filesystem (`versionRe`, `versionPathComponent` and
  `validVersion`, liveswap/names.go). `auth_header` is only
  control-char-checked (`parseDeployPayload`, liveswap/handler.go); its
  contents are attacker-chosen and forwarded to the allowlisted host.
  `sha256` is optional; when present it is exactly 64 hex characters,
  lowercased (`parseDeployPayload`), and the download must hash to it
  (below).
- **Response leaks (all post-auth):** the 500 path returns raw
  `err.Error()` plus the full status snapshot
  (`Handler.mapDeployResult`, liveswap/handler.go) — filesystem paths,
  tar entry names, the operator's allowlist echoed verbatim
  (`artifactAllowEntry.String` and `describeAllowlist`,
  liveswap/allowlist.go). The status snapshot exposes the app's
  **socket, PID and the argv its unit is running** — the last read back
  from the manager's `ExecStart` by `propExecStart`
  (liveswap/systemd_dbus.go), so a credential the operator passed as a
  command-line flag is echoed there — and watchdog cause/failure state
  (`managedApp.status`, liveswap/app.go; `watchdogState.statusSnapshot`,
  liveswap/watchdog.go). The environment is deliberately not in the
  snapshot: it is the place secrets are meant to go, and unlike the argv
  it answers no question about what is running. Artifact URLs *are*
  redacted before logs/errors (`redactURL`, liveswap/download.go), so
  query signatures do not leak.
- **Replay / downgrade is bounded.** The bearer JWT expires (`exp`) —
  an OIDC token at the expiry its issuer set, a local one at most its
  block's `max_token_ttl` after its `iat`, 24h unless the block sets
  one (`localVerifier.verify`, liveswap/deploytrust/deploytrust.go) —
  so a captured request is replayable only within that window.
  Versions are immutable: re-deploying an existing version (running or
  not) is refused 422, so replaying an *older* deploy's payload does not
  downgrade the app. A token-holder can still downgrade deliberately
  via `?rollback=<version>` — an explicit, audited operation
  (`deployed_by` + a `source:rollback` log), not a silent replay
  side-effect.

### Artifact fetching — `liveswap/download.go` + `allowlist.go`

The allowlist is **mandatory** — config load fails without one
(`App.Validate`, liveswap/liveswap.go); no any-origin mode. Pinning
(`artifactAllowEntry.pinnedURLString`, liveswap/allowlist.go) rebuilds
the outgoing URL so scheme is constant, host/port/path-prefix come from
*config bytes*, and only the path suffix + query come from the payload
— the request can never contribute host bytes, with two fail-closed
re-checks (port, then query) and a prefix-boundary guard, all three in
`pinnedURLString`. Canonicalization (`canonicalEscapedPath`) and query
gating (`artifactAllowEntry.vetQuery`) are thorough and
closed-by-default.

`https` only unless `allow_insecure_http` (`downloadArtifact`,
liveswap/download.go), re-enforced on **every redirect hop** by the
`CheckRedirect` closure in `newDownloadClient` — closing Go's default
https→http downgrade. `auth_header` is set on the first request only
(also `downloadArtifact`), and Go's client carries it across a redirect
only while the hop's host is the first hop's or a subdomain of it, on
any port (`shouldCopyHeaderOnRedirect` in net/http); from the first hop
outside that domain it is dropped for good. So the credential reaches
the allowlisted host and its subdomains, and nothing past a redirect
off them: GitHub's asset redirect to its storage host drops it. Size:
Content-Length pre-check plus streaming `LimitReader`, default 100 MB
(`downloadArtifact`; the default is set in `AppConfig.applyDefaults`,
liveswap/liveswap.go).

**Content binding is the deployer's choice.** A pull that carries
`sha256` is hashed as it streams (`downloadArtifact`), and a body that
hashes to anything else is refused as a `validationError` — a 422
naming the pinned and the served digests — with the staged file
removed; the cap is judged first, so a cut-short body reports the
cap, never a mismatch. The pin is the deployer's own hash of the
tarball it built, so with it a host can only withhold the artifact,
not substitute one. Without it the fetched bytes are trusted on the
allowlist and the token alone. The example workflows and the README's
CI recipes send it; a push (bytes from the authenticated caller) and a
rollback (no fetch) have nothing to pin.

**The documented, real gap:** the host allowlist governs the **first
hop only** — `CheckRedirect` deliberately does not re-check the host
per hop, because the GitHub→S3 redirect is load-bearing (the rationale
is on `newDownloadClient`, liveswap/download.go). So an allowlisted first
host can redirect the fetch to *any* https host — LAN, internal, an
https metadata endpoint. "https-only" is a partial SSRF barrier (it
stops plain-http metadata endpoints, not an attacker's https target).
Reaching it requires a valid deploy token **and** an allowlisted first hop.
Secondary: a malicious host can trickle bytes under the cap to hold the
per-app deploy lock open — DoS-of-deploys, not of serving. The stages
before the body are each bounded on their own (30 s connect, 10 s TLS
handshake, 30 s to response headers, set in `newDownloadClient`), so
the stall has to be in the body, which is bounded only by the caller
keeping its request open: the webhook request context carries no
deadline, so a CI job's own timeout is the bound on that trickle.

### Tar extraction — `liveswap/extract.go`

Two-pass (validate-all, then write — no partial residue,
`extractArchive`, liveswap/extract.go). Traversal via stdlib
`filepath.IsLocal`, which both passes reach through `safeRelPath`.
Symlink/hardlink targets must resolve inside the archive root
(`linkTargetStaysInside`). Modes: `Perm()|0600` strips
setuid/setgid/sticky structurally (`rootWriter.write`); dirs forced `0750`.
Devices/FIFOs rejected. Decompression bomb capped at
`max_artifact_size × 10` over the *decompressed* stream
(`decompressionRatioCap`, enforced by the `io.LimitedReader` in
`walkArchive`).

Containment is enforced twice, because the string checks above are
not the filesystem's. Once an earlier entry's symlink exists on disk,
a later name or link target that passes *through* it lands wherever
the link points, and a chain of links that are each inside as strings
(`l1 -> .`, `l1/x/y -> ../..`, `l1/x/y/z/w -> ../..`, then a regular
file or hardlink under the last) climbs out one directory per hop —
into the app dir, a sibling app, the supervisor's own state, or the
user manager's unit directory, as the `motel` uid. So the kernel,
not a model of it, judges what resolves where. The write pass goes
through an `os.Root` (`writeArchive`): every mkdir, open, symlink and
link is resolved by the Root's own walk, which refuses any hop that
leaves the staging dir — the chain above fails at its third hop, named
by entry, with nothing written outside. Then
`checkLinksResolveInside` walks the extracted tree and resolves every
symlink through the same Root (`Root.Stat`), refusing any whose
resolution leaves it: a `..` that climbs out through a link to `.`
(`a/..` is `.` as a string), a hard link to a symlink (that symlink's
relative target re-based to a new directory, which the walk sees as a
symlink), a target that leaves and comes back in by the staging dir's
own name (it would dangle once the tree is renamed into place), in
whichever order the archive listed them. Anything else the Root cannot
follow is refused too, a loop included: the Root gives up after 8
symlink hops where the kernel allows 40, so a link it calls
unresolvable may still resolve — outside — for a reader that is not
the Root (the price is that a chain of more than 8 inside links is
refused too, worded as a loop). A dangling target (a component that
does not exist, or a file where a directory was needed) is the one
thing accepted without resolving: the walk reports an escape ahead of
a gap, so everything up to the gap resolved inside, and what the gap
becomes later is the running app's business — its release is
writable, and an app that wants an outward link plants one, which is
why the supervisor trusts no link under an app dir (the one it
resolves, the app command, is refused unless it lands inside the
sandbox view). A
refusal leaves the partial tree in the staging dir, which the caller
removes as it does for any mid-write failure (`releaseFetcher.fetch`,
liveswap/download.go).
Links that resolve inside — a venv's `python -> python3 ->
python3.12`, a `.bin` link into a sibling directory — extract as
before. `TestWriteArchiveRootRefusesEscape` and
`TestWriteArchiveRootRefusesHardLinkFromOutside` exercise the write
pass with validation bypassed; `TestExtractRefusesLinksResolvingOutside`
the resolution check.

Residual items for the model:

- **Entry and name caps, independent of the byte budget.** The byte
  cap bounds the tar *stream*, not what extraction consumes: every
  entry costs an inode and most a 4 KB block, so 1 GB of 1-byte files
  is ~1M inodes and ~4 GB of blocks — a 20 GB ext4 root has ~1.3M
  inodes — enough to take the box to ENOSPC for ACME storage,
  `state.json`, the journal and every other app's next deploy, and it
  persists under `keep`. `max_artifact_entries` (default 100000; a
  CI-built artifact is thousands) counts the filesystem objects
  extraction creates — the parent directories an entry implies
  included, so one deep name cannot smuggle a thousand — and is
  rejected in the validate pass, so nothing is written. Names and link
  targets over PATH_MAX once joined under the release dir, or with a
  component over NAME_MAX, are refused the same way. The content the
  entries *declare* is capped like the stream: a GNU sparse entry's
  holes are synthesized by the reader from no stream bytes, so the
  stream cap alone would let one small entry write a disk of zeros.
  The byte budget is the *archive's*, not each entry's: a file may be
  nearly all of it, which is the shape of a single-executable artifact
  — `examples/node` ships two `node --build-sea` binaries, ~300 MB
  unpacked, under the 1 GB default. A per-entry ceiling would have to
  sit above the largest binary an app may ship, and there it bounds
  nothing the archive budget does not; the shape that costs more than
  its bytes is many small entries, and that is what
  `max_artifact_entries` refuses.
  A deploy reports the figures and warns past 75% of
  either cap. The archive is still read **twice**, so the byte cap
  permits 2× the CPU — bounded, and cheap next to the write pass.
- `+x` survives extraction — intended (the artifact ships the app
  binary), but it is the point where artifact bytes become code.

### Reverse proxy, admin socket, penaltybox

The starter config exposes only `:80` returning a static string; the
entire liveswap/webhook block ships commented out
(packaging/Caddyfile) — a fresh install has no deploy endpoint. Admin
is off TCP, on `unix//run/motel/admin.sock` (the `admin` directive,
same file), `RuntimeDirectoryMode=0750` owned by the service user
(packaging/motel.service) — so admin access is gated on *being the
`motel` user*. Every deployed app is that user, but a sandboxed app
cannot reach `/run/motel` at all (the path is not in its view), so
the gate holds against apps and only motel itself can connect. The
commented-out `deploy.example.com` example is a public TLS vhost with
`liveswap_webhook` behind `deploy_trust`; nothing rate-limits requests
in front of it, and the handler's own per-address auth-failure throttle
bounds only what it writes to the journal.

**penaltybox** is a response-phase rate-limit-hint enforcer; it touches
untrusted input only narrowly (the client key defaults to
`{http.vars.client_ip}`, deferring XFF trust to `trusted_proxies` — the
`Key` field of `Handler`, defaulted in `Handler.Provision`,
penaltybox/penaltybox.go). A misconfigured `trusted_proxies` turns
client bytes into store keys, bounded by `max_keys` (default 100 000,
idle-eviction). The origin's hint header is strict-parsed and fails
open. It is a minor surface: denial-of-protection under bad config, not
injection or exhaustion.

### Supervisor⇄app and app⇄app boundaries

Every app runs as `motel`, in its own user namespace and its own PID
namespace, with a deny-by-default filesystem view — see "The shared-UID
rule" and "The shipped mechanism". The network namespace is shared, by
design. The unit environment is the user manager's defaults
(`INVOCATION_ID`, …; `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS`
are unset because `/run/user` is not in the view) plus an allowlisted
slice of motel's (`PATH`, `LANG`, `TZ`, `LC_*` — `envAllowlist`,
liveswap/app.go; `HOME` is never inherited, `buildEnv` sets it to the
app's shared dir) — closing *direct*
inheritance of ACME tokens and any other supervisor secret
(`TestBuildEnvDoesNotLeakSupervisorSecrets`). The `/proc` route is
closed twice over (non-dumpable supervisor; cross-namespace refusal);
the filesystem routes are closed by absence.

**Where `env_file` values sit once the app runs.** motel reads the
file itself and hands the pairs to the manager as the transient unit's
inline `Environment=` property (`unitProperties`,
liveswap/systemd_dbus.go), never `EnvironmentFile=`. From then on the
`motel` uid — the trust domain that read the file — holds them by
four routes, and nothing else holds them by any; a fifth, a core dump,
is closed by the unit's limit — enforced by the kernel under a file
`core_pattern`, and under a piped one by the collector honouring it:

- **The unit's property**, for as long as the unit exists:
  `systemctl --user -M motel@ show -p Environment <unit>` (as that
  uid or as root; a bare `--user` targets the caller's own manager). A
  key that inline `env` or the injected `SOCKET` also sets is not
  here: `buildEnvFull` (liveswap/app.go) emits both and the manager
  keeps the last.
- **The transient unit file** the manager writes under
  `/run/user/<uid>/systemd/transient/`, which carries the un-merged
  `Environment=` line — overridden values included — until the unit
  is garbage-collected.
- **The running app's `/proc/<pid>/environ`.** The user-namespace
  closure measured on the spike ("The shared-UID rule", below) runs
  the other way, app to supervisor; this direction follows from the
  same `ptrace_may_access` rule, since the app's namespace is owned by
  the motel uid, which holds `CAP_SYS_PTRACE` in it. Not measured.
- **motel's own memory**, which keeps every value it has read — for
  a launch, or for the response filter, which reads the file without
  one (`managedApp.secrets` and `redactorFor`, liveswap/app.go) — for
  as long as the app stays configured, behind the non-dumpable floor.
- **Not a core dump.** App units run with `LimitCORE=0` on both the
  soft and the hard limit (`unitProperties`, liveswap/systemd_dbus.go),
  so a crash writes no dump of the app's memory, and the app cannot
  raise the limit back. Without it the manager's defaults apply, soft
  0 under an unlimited hard limit (measured on Debian 13, systemd
  257.13, 2026-10-13, #234): one `setrlimit` from a dump. Nothing
  makes an app non-dumpable (the floor under "A non-dumpable
  supervisor is the floor" is the supervisor's own); the dump itself
  is what is closed, since it would outlive the unit where the same
  uid and root could read it later. Who enforces the limit depends on
  the host's `core_pattern`: with a file pattern — Debian 13's
  default, `core`, in the crashing process's working directory, the
  release dir unless the app moved — the kernel writes nothing; with a
  pipe pattern the kernel hands the process to the helper whatever the
  limit, and it is systemd-coredump (not installed by default) that
  declines to store a dump when the limit is 0, its own "resource
  limits disable core dumping" rule. The cost is that no app can write
  a core dump for debugging; there is no per-app opt-in.

No account gains a read it did not already have: the file is
root:motel 0640 by the tutorial's install line (nothing in motel
enforces that), or, for a file the `box` applier writes, by the
applier's own mode and the setgid directory it writes into, `/run/user` — the manager's socket and the transient
file alike — is outside every view (`sandboxNeverReachable`,
liveswap/sandbox.go), and the PID namespace hides the process from
siblings. Stated because the manager's two copies are not obvious from
the file's mode. `LoadCredential=` delivery would take the values out
of the unit's properties and environment, not out of the uid's reach,
and what it closes under this sandbox is unmeasured: an open question,
not a plan.

### Config webhook — `box/`

The second authenticated entry point, and the only one whose outcome
is a root-owned file. [box/DESIGN-box.md](box/DESIGN-box.md) is
authoritative for its behaviour; this section places it. Status:
released in v0.3.0, and the secrets it carries (below) in v0.3.1.
On a box set up from the
template (`examples/box`; docs/first-deploy.md, step 3) root writes
the file once, with `motel init`, and the config repository's
signed pushes write it after that. A box with no `box` block has no
config webhook, and its file is root's to edit.

The operator's config repository is the only writer of
`/etc/motel/Caddyfile`. Its workflow mints an OIDC token and POSTs
a bundle — the Caddyfile, the raw commit object of `HEAD`, the tree
objects on the path to the file, and the first-parent chain back to
the commit the box runs — to `box_webhook`, a site directive beside
`liveswap_webhook`. Two processes stand between that request and the
file, and the boundary between them is the one that matters.

**The handler** (uid motel, in the serving process) verifies the
token against the `box` global option's `deploy_trust` through the
same entry point liveswap's handler uses — same 401, same budgets,
same `refused` line — so an unauthenticated caller learns nothing it
could not learn from a deploy webhook. It parses the bundle from
memory under fixed names and caps, binds the bundle's commit to the
token's `sha` claim, pre-checks the signature, runs `motel
validate` (and `motel-backup validate` where installed) as bounded
children with their output redacted, and drops the bundle as one
regular file for root. It answers by the first result it reads within
a bounded wait: 202 if that is `verified`, which precedes the install,
or the final result if root has already written it; 504 when it reads
none (a push of the commit the box already runs is answered
`no_change` at once). What that answer does not settle is polled,
because root's part can take minutes, and a proxy in front of the box
may cut a request held that long.

**The applier** (`motel-box-apply.service`, root, one shot, started
by a path unit watching the drop directory) trusts nothing the
handler did. It reads the bundle once into memory and never from disk
again; takes the signer list from the installed file's raw tokens
(never from the running configuration, which the motel uid
authors); requires the incoming file's `box_webhook` host to equal the
installed one and the bundle's path to equal the path `init` recorded
(the box's identity and which file is its: another box's equally
signed file, or another file for this host elsewhere in the same
tree, is refused); verifies the SSH signature with `ssh-keygen -Y` as uid
65534 against that list, on `HEAD` and on every first-parent commit
above the recorded baseline (an unsigned commit would otherwise ride
in under the next signed one); proves the file is the committed file
(commit, tree and blob hashes); requires first-parent descent from the
recorded baseline; refuses a file that drops the `box` block, every
signer, the `box_webhook` site or the key that signed it; and only
then writes, reloads through `systemctl reload motel`, and rolls
back — reloading again — on failure. It never runs Caddy's validate or
adapter: both execute module code on the input and expand `{$VAR}`
from root's environment.

What this closes. No account on the box but root is given a way to
change the config or write an env file: secrets ride this channel
(below), and an administrator is an account in `adm`, for the journal
alone. A leaked PAT, a
stolen GitHub session or an OAuth app with `contents:write` can push
to `main` and cannot produce the signature, so it cannot change the
box with anything a signer did not sign: its own commit is refused on
its own push and, because every commit on the chain is checked, cannot
be carried in by the next signed one (the merge button cannot either:
a squash is GitHub-GPG-signed, a rebase unsigned). What such a push
*can* do is stop every later push until it is rebased away — loud and
recoverable — and, the residual that remains, fast-forward onto `main`
a branch every commit of which a signer already signed: a signer's
own unmerged, signed configuration, which the box then applies because
it is exactly what the rule asks for. The box repository's README
therefore says that configuration not yet meant to run is committed
unsigned or lives in a fork; an unsigned branch is one the chain rule
refuses whoever lands it. A compromised action holding the
repository's OIDC token can present only commits a signer already
signed, cannot replay an older one — though replaying `HEAD` after a
console edit reinstalls HEAD's file and so reverts that edit, which is
the stated 3am contract — and cannot land a sibling
box's file.

What it changes for T5, honestly. The Caddyfile was root-owned before
`box` and the motel uid never wrote it, so "a supervisor RCE cannot
persist config" is not a gain — it held under `bin/push` too. The
delta is the other way: `box` adds a root process that consumes input
the supervisor controls — a tar reader, a commit and tree parser, a
path walk, an `ssh-keygen` invocation, lock and rename handling — and
what keeps T5 from reaching root through it is that the applier
re-verifies everything from the bytes, reads them once into memory,
parses under fixed names and small caps, runs `ssh-keygen` as uid
65534, trusts the handler for nothing but a well-formed bundle, and
refuses a Caddyfile with any `import` — an imported file is unsigned
bytes, and one under a motel-writable path would be T5's way to
make a change outlast a restart — including one spelled as a
placeholder: Caddy expands `{$NAME:default}` before it tokenizes, so
the applier refuses any `{$` at directive position, in a site address
or in the `box` block, and checks the file again after expanding
placeholders with an empty environment (box/DESIGN-box.md, "Reading
the signed file"). What the workflow is *told* remains the serving
process's word: the response and the result poll are served by
motel, so a T5 can lie to CI; it cannot change root's record or
root's file *itself*. What it can do through root is bounded by the
signers, not closed: the token-to-commit binding is the handler's
check, and root cannot repeat it (the token is not in the bundle, and
verifying one offline would mean a root unit fetching issuer keys or
trusting the handler's cache), so a supervisor RCE that drops a bundle
straight into `in/` can have root install any configuration a signer
signed that descends from the baseline and whose objects it holds —
`HEAD` again, which undoes a console edit, or a signer's signed but
unmerged branch. It cannot have root install anything a signer did not
sign; dropping an unsigned or self-authored file is refused outright.
Skipping the handler's validation, which root never repeats, is a
remaining T5 capability bounded by the signature, identity, proof and
descent checks: a signer-signed file that fails to load is rolled back,
one that loads but `motel-backup validate` would have refused is
installed. The box repository's
README rule that unmerged configuration stays unsigned is what keeps
that set to `main`'s own history.

What it does not close. `systemctl reload`'s exit status is the
motel uid's word; the `applied` phase trusts it, the file on disk
does not depend on it, and `systemctl restart` is the remedy. Root's
never-cut-the-branch guard checks presence, and the handler's
reachability check covers only what the box server's protocols,
listener wrappers, TLS policies and strict SNI check do to the
workflow's connection and what stands before `box_webhook` on `/`: a
`claim` typo passes both and strands the box until root fixes it on
the box.
The
handler's validate is a courtesy, not a boundary. Plain `crypto/sha1`
on the proof, not sha1dc. A signer's laptop is the operator; a
software-held signing key makes its compromise total for the box,
which a hardware-held (`sk-`) key reduces to "cannot sign unattended".

**Secrets through the same channel** (box/DESIGN-box.md, "Secrets").
Each value is an age file in the config
repository, `<host>/secrets/<app>/<VAR>.age`, encrypted on a laptop to
the box's own X25519 key and to readers — the signers' SSH keys, or an
age key of a person's own. The bundle carries the ciphertext; the
handler and root prove it against the signed commit as they prove the
Caddyfile; root alone decrypts it, after every signature on the chain
has verified, and writes `/etc/motel/secrets/<app>.env`. What
changes:

- **Nothing new for the motel uid or root.** T5 reads every env file
  now, as it reads every hand-made one, and cannot read the box's key
  (root 0600, in a 0700 root directory). It can answer the workflow's
  `GET /` with any key; the workflow only compares that answer with
  the repository's `recipients`, and the remedy it names, `make
  rekey`, is a person's after reading the key from root over SSH —
  and a substituted key would gain T5 only values it reads already.
  Root decrypts nothing a listed signer did not commit, so a T5 bundle
  dropped into `in/` cannot have root open hostile age input.
- **Nothing new for a signer**, either: a signed commit could already
  add a site that serves any file the motel uid reads, env files
  included (examples/box/README.md, "Secrets").
- **New: the values leave the box, encrypted.** GitHub, every clone and
  every fork hold them for ever. A reader's private key with read
  access to the repository decrypts every value encrypted to it, in
  all of history, offline, and nothing on the box or at GitHub sees it
  beyond the clone. The template makes every signer whose key age can
  use a reader (box/DESIGN-box.md, "Layout in the repository"), so a
  signer's software key now yields the values without a push. A
  reader removed from `recipients` keeps what was encrypted to them:
  age has no revocation, so a person leaving, or a leaked key, means
  new values, not only new ciphertext.
- **New: names and lengths are public to repository readers.** The
  layout puts each variable's name in a file name, and age pads
  nothing, so anyone with `contents:read` learns which apps have
  secrets, what they are called, how long each value is, and when it
  changed.
- **New: harvest now, decrypt later.** The box's key is X25519, and
  no reader key age takes from an SSH key is post-quantum either
  (`ssh-ed25519` is wrapped through X25519, `ssh-rsa` with RSA-OAEP);
  age's post-quantum hybrid keys cannot share a file with SSH-key
  recipients. Ciphertext kept for ever is
  open to a future quantum computer; the path is every reader on an
  `age1pq` key and one rekey, which protects only new ciphertext: the
  old stays in history, so a value that must stay secret is changed
  too (box/DESIGN-box.md, Open questions).

### Backups — `backups/`

`motel-backup` copies what each app's `backup` block declares — its
SQLite databases and the files under its `shared/` — into one restic
repository off the box every hour, and drills a restore of every app
each week. [backups/README.md](backups/README.md) is authoritative for
its behaviour and [docs/backups.md](docs/backups.md) for the bucket and
the operator's part; this section places them. Status: built on the
`backup` branch, released when it merges into `main`, which this
section gates (#172). It adds no entry point: it is a program of its
own, run by root's timers and root's hand, and the server's one use of
it is the box handler's `motel-backup validate`. What it adds is
asset 2, a root service that handles every app's data, and a bucket
whose rules the provider enforces.

**Who runs what** (backups/README.md, "Who runs as what", the one
table). The run is root, with `CAP_SYS_ADMIN`, `CAP_DAC_READ_SEARCH`,
`CAP_CHOWN` and `CAP_FOWNER` of root's and nothing else, in the host's
mount and PID namespaces, since the manager has to see its mounts. It
plans from `/etc/motel/Caddyfile` — the file, not the running
server — and does every step as a transient unit with a
deny-by-default view:

- what parses an app's bytes — `sqlite3`'s copy and check, a restore's
  check and install — runs as `motel`, in user and PID namespaces of
  its own, with no network and no credential, seeing one app;
- what holds the credential — restic, in every unit that runs it — runs
  as `motel-backup` and sees nothing of an app, or that app's
  declared paths read-only (the upload, with `CAP_DAC_READ_SEARCH`), or
  one empty directory (the fetch);
- the plan unit, which runs the adapter on the file, runs as
  `motel-backup` in namespaces of its own, with no network and no
  credential, and sees `/etc/motel` read-only;
- root itself opens an app's paths as `O_PATH` only, refusing a link
  anywhere on the way, binds what it opened, chowns a fetch to
  `motel` by name in a view of nothing else, and starts units. It
  reads the credential file only for which repository it names; the
  manager reads it, as root, for each restic unit.

**The bucket** (docs/backups.md, "Before you start"). Versioned, with
one rule: old versions are removed after 180 days. Two keys, the box's
and the off-box machine's, neither able to destroy a version: on B2 an
application key for the bucket alone with `listBuckets`, `listFiles`,
`readFiles` and `writeFiles`, never `deleteFiles`; on AWS S3 a user
whose policy denies `s3:DeleteObjectVersion` and every change to the
bucket. Nothing on the box forgets, prunes or repairs: retention runs
off the box, with the second key (backups/README.md, "Retention"). This
is option A, the owner's choice (backups/design/plan.md, PR 8),
and its claim is the guide's: nothing the box or the off-box machine
holds can destroy a backup, and a deletion or overwrite made with
either key can be undone for 180 days, if someone looks from off the
box at least once every 90.

**The principals.**

- **Root**, reached over SSH by key only (docs/hardening.md, step 1), is
  the trusted base. It runs `setup`, `restore`, `run`, `sweep`, the
  drill on demand, and every `systemctl` line that starts, stops,
  enables or disables a backup unit (docs/backups.md, "Who runs
  these"); the timers run `run` and the drill as root services
  (backups/README.md, "On a fresh box"). Each starts system units, and
  `restore --to` is root-equivalent (Residual risks), so no role short
  of root holds any of them: a two-tier sudoers would be a
  security-model change (the owner, F1 of the RC docs run,
  backups/design/plan.md). An administrator whose `sudo` is
  root's is root. `setup` is a person's: it shows the repository
  password on its terminal.
- **A signer of the box repository** chooses what is backed up. A
  `backup` block reaches `/etc/motel/Caddyfile` by a signed push, as
  the rest of the file does ("Config webhook"): the handler, as the
  motel uid, runs `motel-backup validate` on it after `motel
  validate` (box/validate.go), and the applier proves it and runs
  neither. On a box with no box repository, root edits the file by
  hand. Either way the run leans on no earlier check: the plan unit
  adapts the file with no environment, and the run validates every
  field of the plan again (backups/README.md, "A run", step 3, and "The
  Caddyfile is read without the server's environment"). A signer is the
  operator ("A signer's laptop is the operator").
- **The administrator** (optional; docs/hardening.md, step 3) is in
  `adm` alone, with no `sudo`. They read the journal — restic's stderr
  among it, which names an app's files — and run `motel-backup
  status` and `validate`, which need no privilege: the record,
  `/var/lib/motel-backup/status.json`, is world-readable and holds
  no secret (backups/README.md, "Status"). They choose nothing and
  cannot read the credential. An agent fits here, never in root's role.
- **The `motel-backup` uid** runs every restic unit, with the
  credential in its environment while the unit runs: whoever is that
  uid reads it there. It is a system account with no shell and no home,
  made by the package or by `setup` with the comment
  `made-by-motel`; any other account of that name is refused before
  any unit (backups/README.md, "What it refuses"). The server and the
  apps can neither read restic's environment nor signal it.
- **The storage provider** holds the repository, encrypted under its
  master key: without the password nothing reads it. It enforces the
  keys' limits, versioning and the rule.
- **The off-box machine**, the operator's own, holds the second key, for
  retention and for the look — `restic snapshots` and `restic check
  --read-data` (docs/backups.md, "Look from your own machine") — the one
  view of the backups the box does not author: whoever has the box can
  make its own `status` say anything. Somewhere off the box, too, the
  repository URL, the box's key and the password are kept, as `setup`
  asks: asset 2 whole.

**What each attacker gains.**

- **T1** reaches its own app's backup and nothing more. What it writes
  under its declared paths is what the next snapshot holds; earlier
  snapshots it cannot change, since it holds no key and nothing on the
  box forgets or prunes. It cannot aim the backup elsewhere: a declared
  path is the app's to replace with a link while it runs, so the run
  opens each one itself, refusing a link anywhere on the way, binds
  what it opened (`mount(2)` from `/proc/self/fd/<n>`), and refuses a
  `shared/` that is not the `motel` user's. Its bytes are parsed
  only by units that hold nothing: a FIFO in a database's place holds
  `sqlite3` five minutes at most (backups/README.md, "Databases"), and
  a restore puts a link back as a link, never followed, and no setuid
  bit. What it can spend is time and money: nothing bounds an upload,
  or a copy once begun, so an app that grows its data without limit
  holds the run lock — its siblings' backups wait, and `status` exits 1
  from the third hour — and grows the provider's bill. Its bytes become
  root's configuration only where root restores them `--to` such a
  directory (Residual risks).
- **T5** gains nothing ("What is unchanged", below).
- **T6.** A push credential or an OIDC holder lands only files a signer
  signed ("Config webhook"), and gains nothing more here. A signer
  chooses what is backed up: a signed file can add a declaration,
  narrow one, or drop them all. A dropped block is said once, by the
  run that finds it gone and by `status` until the next run, and only
  for an app the record held a snapshot of; a path dropped from a block
  still declared is not said (backups/README.md, "A run" and Accepted
  residuals). What is in the repository stays: the box cannot remove
  it, and the look sees the snapshots stop. A declaration stays inside
  its app's `shared/`, whoever signed it: the run refuses an absolute,
  unclean or escaping path, as `motel validate` does
  (liveswap/README.md, "Declaring backups"). The credential does not
  ride the channel: `setup` writes it at a terminal into
  `/etc/motel-backup`, outside the channel's `/etc/motel`
  (backups/README.md, "Setup"). A signer's software key is the box all
  the same ("Config webhook").
- **The administrator** gains what the journal and the record show —
  app names, data paths, snapshot ids, restic's words, an app's file
  names — and nothing to act with: every command that changes a backup
  is root's, and none they run reads the credential.
- **A stolen box key**, without the password, can list, read, overwrite
  and delete by name, and destroys no version: a delete by name only
  hides, and a version deleted by its id is refused. An attack with the
  box's key destroyed nothing through either of B2's APIs; the next run
  failed `exit 10`, and `status` exited 1 (measured through B2's S3
  endpoint, `v0.5.0-rc1`, 2026-11-02). A quieter attack, one pack
  overwritten, left the hourly runs `ok` and only the next drill saying
  so (measured; docs/backups.md, "After an attack"): the drill fetches
  each app's newest snapshot whole and checks one fifty-second of the
  data a week, and the look's `restic check --read-data` reads all of
  it. With the password as well — and the box's own file holds both —
  it reads every backup, on B2 the old versions too (`listFiles`,
  `readFiles`), and can write a snapshot the box's restore and drill
  take for its own: they choose by host, tag and time, which the writer
  sets (`newest`, backups/engine/engine.go); on a box with its record, a
  restore names the last snapshot a run ended `ok` on when the newest is
  another (backups/README.md, "A restore", step 2). The way back is a
  copy from a moment before its first write (docs/backups.md, "After an
  attack").
- **A stolen off-box key** has the same capabilities, and so the same
  gains. It is also the key that prunes, so its deletions look like a
  prune — and `prune` too only hides (measured, `v0.5.0-rc1`); the
  bucket's history, against when you last pruned, tells them apart
  (docs/backups.md, "After an attack").
- **A stolen provider account** — its login or its master key — can
  destroy everything, and no key on the box or the off-box machine is
  it: the guide gives the login a second factor, keeps its keys off the
  box, and has the master key regenerated once it has been typed to
  make the two keys (docs/backups.md, "Before you start"). Recovery from
  the other attackers needs no account key: a copy from the attacked
  bucket's history into a new bucket, made with a read-only key of the
  attacked bucket and a key of the new one, held the last backup and
  checked clean, and a box set up onto it was healthy (measured,
  `v0.5.0-rc1`). The answer to this attacker, repository rotation under
  a compliance Object Lock, is deferred (#169).

**What is unchanged.** The serving process gains nothing. Root reads
nothing it writes: the plan is root's file read by a unit of the backup
account, never the running server's word — "what gets backed up is not
for the process the internet talks to to say" (backups/README.md, "The
Caddyfile is read without the server's environment"); root opens app
data as `O_PATH`, a descriptor that cannot read; and what a unit under
the motel uid reports — a dump's result, a check's — is decoded
strictly against what was asked. The credential is never under its uid:
the file is root's, restic runs as `motel-backup`, and the staging
and restore directories are root's, 0700 (`open`,
backups/engine/engine.go), reached by a unit only through a bind the
manager makes. The motel uid holds every app's data already, so a
dump or a restore under it shows it nothing new; it can change what the
next snapshot holds, as T1 can of its own app, and no earlier one.
Skipping the handler's `motel-backup validate` installs a signed
file that the next run refuses, red and named ("Config webhook").
motel validates a declaration and does nothing else with it; the
supervisor still holds no grant.

**Measured on a real provider.** `v0.5.0-rc1` ran the release-candidate
checks on 2026-11-02 through B2's S3-compatible endpoint, the door the
box uses, with keys holding `listBuckets`, `listFiles`, `readFiles` and
`writeFiles` for one bucket (backups/release-candidate.md states each
check): a delete by name only hid, and a delete of a version by its id
was `AccessDenied` (check 3); restic left no lock, a lock held 30
minutes included (check 3), and `prune` only hid (check 5); the attack
and the recovery above (check 4); a read-only key was refused by
`setup` at once (check 6); and a rebuilt box with another uid and gid
for `motel` restored every file under the new ids (check 7). Not yet
read: whether B2's rule removes a marker hidden through the S3
endpoint, as it removed one hidden through its own API within two days
of a one-day rule
([RC-lifecycle-day2.out](backups/design/rc-runs/RC-lifecycle-day2.out),
`v0.3.0-rc1`) — read on 2026-11-04; it bears on how the rule ends the
180 days for what the box hides, not on what a key can destroy within
them; and the drill's egress on a bill (check 8), which bears on the
drill's cost, not on a boundary. `v0.3.0-rc1` ran most of the checks
through B2's own API, which the box no longer uses (2026-10-04 to
10-06; backups/design/rc-runs/). On AWS S3 and Hetzner nothing has
run, and every provider claim above is unverified there; on Hetzner
every key reaches every bucket of its project, so the keys' limits
would be a bucket policy, not worked out (docs/backups.md, "Before you
start").

What it does not close is under Residual risks: a stolen provider
account, root's reach after the fact, `restore --to`, the provider's
enforcement, a directory the box is joined to, and the account's name.

### Install-time — `packaging/postinstall.sh`

Runs as root at install; creates the `motel` user, chowns
`/var/lib/{motel,liveswap}`, enables linger so the motel user
manager runs without a session, and installs or replaces the starter
config at `/etc/motel/Caddyfile` only as asset 6 says: where nothing
stands, or while the file is byte-identical to the starter recorded in
root's own `/var/lib/motel-package`. Supply-chain attacks on motel's own
build land here, but they run in CI, not from a deployed app — out of
scope for the runtime model, in scope for release signing (roadmap).

## Attacker profiles (ranked by likelihood)

- **T1 — Poisoned dependency running *inside* a deployed app.** The
  primary threat, per the npm 2025 wave: a transitive dependency's
  runtime payload sweeps the filesystem for `.env`/tokens and
  exfiltrates. Already has code execution as `motel`. This is the
  attacker per-app isolation exists to stop.
- **T2 — Stolen deploy token / compromised CI identity.** A short-lived,
  claim-scoped token (or control of the CI identity that mints one). Can
  deploy arbitrary code to every app that accepts that trust source for
  the token's lifetime — by pushing a tarball directly, which passes no
  allowlist, or by pulling from a URL within `artifact_allowlist` — and
  roll back versions. Containment is the claim scope (and, for pulls,
  the allowlist), not the runtime; the sandbox bounds what the deployed
  code then reaches.
- **T3 — Malicious/compromised artifact host** within the allowlist
  pin. Controls status, redirect targets (any https host), timing —
  and the tarball bytes, unless the pull carries `sha256`, which the
  example workflows do: then other bytes are a 422, and what remains
  is withholding. The pin is kept in the version's deploy record, for
  as long as the record is, unless the filter would redact it (it
  equals or holds an `env_file` value, or those values could not be
  read whole); liveswap/README.md, Deploy records, has the exceptions. Faces `extract.go` and the first-hop SSRF gap.
- **T4 — Unauthenticated network attacker** on the public webhook/proxy.
  Faces the token gate — forgery needs a private key, so there is no
  guessing oracle; the realer wins are log-amplification (bounded by
  the auth-failure budgets: ten failures a minute per address and a
  hundred process-wide are logged, then silence; the answer is the
  flat 401 throughout), the CPU cost of JWT/JWKS verification (spent
  on every request — the budgets bound the journal, not the work),
  and any pre-auth proxy/Caddy surface.
- **T5 — RCE in the supervisor itself** (a Caddy or liveswap bug). Low
  probability, catastrophic: it *is* the `motel` user, so it already
  holds every asset short of root. No app-isolation design prevents
  this; the design question is only how much *worse* it can get (does it
  reach root?) — which is why the supervisor holds no grant (see "The
  shipped mechanism").
- **T6 — Compromised identity of the box's config repository.** Three
  distinct holders, in descending likelihood: a credential that can
  push to the repository (a leaked PAT, a stolen session, an OAuth or
  GitHub App with `contents:write`); a compromised Action in that
  repository, holding its OIDC token — or, where the `box` block trusts
  a `deploy_trust local` key, whoever holds that key, who mints the
  `sha` claim themselves and so needs no workflow and no `main`; a
  compromised signer's laptop.
  The asset is the configuration at rest — asset 6, the one that
  survives a restart. The first can push and cannot sign: what it
  writes is refused on every later push too, until removed, and what it
  can land is only a branch a signer fully signed already (the README's
  rule against signed unmerged config is the answer); the second can
  present only what a signer signed, in order, for this box, including
  `HEAD` again after a console edit; the third is the operator, bounded
  only by a hardware key's touch. Faces `box/` ("Config webhook").
  Once secrets ride the channel, the asset widens to asset 5's values:
  any of the three that can read the repository learns the secrets'
  names and lengths, and the third, or anyone holding a reader's key
  file, decrypts them. A signer also chooses what is backed up
  ("Backups").

For **T2**: deploy-arbitrary-code (a push is contained by nothing but
the claim scope; a pull additionally by the allowlist); deliberate
rollback. For **T3**: archive-borne CPU (bounded by the byte and
entry caps; inode exhaustion is closed by the entry cap; a link chain
out of the staging dir is refused by the `os.Root` write pass and the
post-write symlink resolution check); first-hop→any-https SSRF. For **T4**:
log-amplification, bounded by the auth-failure throttle (online
*token forgery* is infeasible).
For **T5**: total, by definition — the containment question is
root-vs-not-root, and `box` adds a root process fed by the supervisor
to that question (the "Config webhook" section says what keeps it
closed); backups add root services that read nothing it writes
("Backups"). For **T6**: a push credential gets a red run and a stalled
`main` until its commit is removed, or a signer's own signed branch
landed early; an OIDC holder gets a replay of `HEAD` (a `no_change`,
or the undoing of a console edit), one bundle in `in/` at a time with
no age-out and one in root's hands, and the handler's validate on a
signer's file; a signer's software key gets the box, and — with secrets
in the repository — every value encrypted to it, offline.

## The shared-UID rule

Everything liveswap touches runs as one UID: motel, the motel
user's `systemd --user` manager, and every app. The kernel gates
`/proc/<pid>/{environ,root,cwd,mem,fd,maps}` on
`ptrace_may_access(PTRACE_MODE_READ)`, which any same-UID caller passes
while the target is dumpable — regardless of the caller's mount
namespace or seccomp filter, **but not across user namespaces**: after
the uid and dumpable checks the LSM hook runs, and commoncap's
`cap_ptrace_access_check` refuses a caller whose `user_ns` differs
from the target's unless the caller holds `CAP_SYS_PTRACE` *in the
target's namespace*, which an app in a child namespace never does.
(Measured on the 2026-08-30 spike — bare: `/proc/<manager>/root` open;
`PrivateUsers=yes` alone: denied.) Three consequences, and they decide
the mechanism:

1. **No mount sandbox without a namespace the ptrace check honours.**
   A sandboxed app that can `ptrace`-read any same-UID PID outside its
   sandbox opens `/proc/<that-pid>/root/…` and walks the host
   filesystem. The user manager (host root, same UID, always running)
   is a permanent such target; every sibling app is another.
   `ProtectSystem=strict`, `InaccessiblePaths=`, any bind-mounted view
   are all void without one. `ProtectProc=invisible` does not
   substitute: `hidepid` hides other *users'* processes, and there are
   no other users here. Two namespaces do, and they close different
   things:
   - a **user namespace** (`PrivateUsers=yes`) closes the `/proc`
     reads — `root`, `environ`, `cwd`, `mem`, `fd` — of every process
     outside it, so the mount restrictions hold. Signals still
     deliver: `kill` checks uids, not namespaces.
   - a **PID namespace** (`PrivatePIDs=yes`) makes those processes
     invisible and unsignalable, and gives the unit an in-namespace
     init.

   Every app unit gets both. There is no lesser configuration and no
   opt-out: a host that cannot deliver both namespaces fails the start
   rather than running any app bare (`unitFor` refuses a spec without
   a sandbox). A unit motel did not start with them — a stale one
   from a development build, or another same-uid process's — is
   refused at reattach and replaced by a sandboxed relaunch, so "every
   unit" holds for adopted units too. That check is for an honest
   stale unit; a same-uid process that starts units on purpose can
   give one any property set, and is the trust domain, not a boundary
   the check could hold.

   **Corollary: an opt-out is impossible on a shared uid.** A per-app
   "sandbox off" reads as per-app; it is not — a bare app reads every
   sibling's data and motel's keys. The rule that makes the user
   namespace load-bearing is the rule that makes any app outside a
   namespace every app's problem.

2. **A non-dumpable supervisor is the floor.**
   `prctl(PR_SET_DUMPABLE, 0)` makes motel's `/proc` entries require
   `CAP_SYS_PTRACE`, which apps never hold — the unit's
   `CapabilityBoundingSet=` is empty and `NoNewPrivileges=` stops an
   `execve` from acquiring any — with or without user namespaces.
   `liveswap/harden` is a leaf package
   whose `init` runs right after `syscall`'s — before `os`, `fmt`,
   Caddy and every package depending on them; only
   `syscall`-closure-only leaves that sort earlier can precede it, and
   those cannot touch `/proc` (measured with `GODEBUG=inittrace=1`:
   the 17th of 460 initializers on motel, two such leaves ahead of
   it; `TestInitRunsBeforeOS` pins `syscall < harden < os` in any
   binary that runs it) — so any binary importing liveswap (motel or
   an xcaddy build) is non-dumpable before `main`; a failure is fatal.
   Pinned by a unit test and by the real-systemd e2e suite (scenario
   11). It closes the `/proc/<supervisor>/environ` and
   `/proc/<supervisor>/root` routes only; TLS keys on disk, the admin
   socket and sibling files need the mount namespace.

   *Residual, recorded because `liveswap/harden` cites it:* app units
   outlive supervisor restarts, so a same-UID app could in principle
   race the interval between `execve` and that `init` — well under a
   millisecond — and read the new supervisor's environment. On Debian
   13 that is a *read* race: Yama's default
   `kernel.yama.ptrace_scope=1` forbids a non-descendant from
   `PTRACE_ATTACH`/`PTRACE_SEIZE` at any time (Yama gates
   `PTRACE_MODE_ATTACH`, which the dumpable flag does not govern), so
   an app cannot seize the supervisor in the window; a host set to
   `ptrace_scope=0` is outside this model. The window is closed from
   the app's side by the namespaces above — an app in its own user
   namespace cannot read the supervisor's `/proc` at all, and one in
   its own PID namespace cannot see the supervisor's PID — and since
   every app is sandboxed it stands for nothing on a running box.

   *The capability probe runs under the shared uid.* It starts a real
   transient unit, bounded at 30 s, so any process holding that uid can
   interfere with it. A failed probe fails the whole server start, so
   this is an availability attack on the supervisor, not a way to
   weaken an app: interference cannot produce a running motel with
   a lesser sandbox. A probe made to fail, or to time out twice, keeps
   motel down until an operator starts it — a refused start exits 1,
   which the unit never retries; a start stretched past the unit's
   `TimeoutStartSec` is restarted by systemd, which repeats the
   exposure rather than ending it. Either way the bound is the same
   trust domain. The verdict is cached per manager connection
   (`userManagerClient.cachedSandboxCapability`,
   liveswap/systemd_dbus.go), which narrows the window, and a failed
   verdict is deliberately NOT cached, so interference costs the next
   start rather than pinning a verdict until the manager
   restarts. The remedy is to remove the interfering process, which
   runs as the motel uid and is therefore either motel's own app
   or something already in the trust domain.

3. **Resource caps need a read-only cgroupfs inside the sandbox.** The
   cgroup subtree under `user@<uid>.service` is delegated to — owned
   by — the motel UID, so `MemoryMax=`/`TasksMax=` on a user-manager
   unit is a limit the app can rewrite in `/sys/fs/cgroup` (or escape
   by migrating its PIDs) unless that tree is read-only in its view.
   Runtimes only ever *read* it. `ProtectControlGroups=` is set on
   every unit, so the caps an operator sets (`memory_max`, `tasks_max`,
   `cpu_quota`, per app; DESIGN-liveswap.md "Resource caps") hold:
   measured from inside the unit, the value reads back and the write
   is EROFS.

## The shipped mechanism

systemd's own per-unit sandboxing on the user-manager runner, issued as
transient-unit properties by a supervisor that holds no grant.

**Why the user manager and not the system one.** polkit is blind to
unit *properties*, so a `StartTransientUnit` grant on the *system*
manager that the supervisor can shape is root-equivalent (a unit can
name `User=root`, any `ExecStart=`) — a naive systemd runner would
make T5 *worse*. motel instead talks to the motel user's own
manager over its private socket (`/run/user/<uid>/systemd/private`):
no session bus, no polkit, no grant of any kind. It can create units
only as itself, under `NoNewPrivileges`, so a supervisor RCE gains
nothing it did not already have (T5 unchanged). What only the system
manager can buy — per-app `User=`, `IPAddressDeny=` egress filtering —
stays future work and, if wanted, must arrive as a root-owned template
or a minimal privileged helper, never as a system-manager transient
grant.

**The property set**, on every unit — `unitProperties` renders the
lifecycle properties and appends `sandboxProperties` for the rest, and
`userManagerClient.StartTransientUnit` adds `LimitNOFILE=`
(liveswap/systemd_dbus.go). The capability probe's unit (below) is a
`oneshot` run from `/`, with no `BindPaths=`: it binds nothing of an
app's. No unit carries an `ExecStopPost=` or any other `Exec*=`
beside its `ExecStartEx`: the manager spawns one process per unit
(#311; "Lifecycle" below).

- Namespaces: `PrivateUsers=yes`, `PrivatePIDs=yes`, `PrivateTmp=yes`,
  `PrivateDevices=yes`, `PrivateIPC=yes` (its own SysV IPC, POSIX
  message queues and `/dev/mqueue`); `RestrictNamespaces=` (empty set
  — an app cannot create further namespaces). `/dev/shm`, which
  `PrivateDevices=` binds from the host and no IPC namespace covers,
  is a tmpfs of the unit's own (`TemporaryFileSystem=`, `size=50%` as
  the kernel gives the host's; `memory_max` is the practical cap;
  #311).
- View: `TemporaryFileSystem=/:ro` replaces the whole filesystem with
  an empty read-only tmpfs; `BindReadOnlyPaths=` puts back a named OS
  base view and `BindPaths=` the release being started, the app's
  `shared/` and the instance's own `run/<nonce>/`. Nothing else exists
  inside. There is no `InaccessiblePaths=` and no
  `ProtectSystem=` because there is nothing left for either to act on
  — an unnamed path is absent, not merely unreadable. The view is
  deny-by-default, so nothing is derived from the running
  configuration and nothing ages: a secret declared tomorrow is absent
  from a unit started yesterday for the same reason every other path
  is.
- Kernel and privilege surface: `ProtectControlGroups=`,
  `ProtectKernelTunables=`, `ProtectKernelModules=`,
  `ProtectKernelLogs=`, `RestrictRealtime=`, `RestrictSUIDSGID=`,
  `LockPersonality=`, `NoNewPrivileges=`, `CapabilityBoundingSet=`
  (empty), `SystemCallFilter=@system-service` less `io_uring_setup`,
  with `SystemCallErrorNumber=EPERM`, `SystemCallArchitectures=native`,
  `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK`
  (netlink for `getifaddrs()`, which Node and Go frameworks call at
  startup; the socket itself is not read-only, but changing a link, an
  address or a route needs `CAP_NET_ADMIN`, which nothing in the unit
  holds). The family list filters `socket(2)` alone. Two routes around
  it are closed: an io_uring ring's `IORING_OP_SOCKET` (kernel 5.19+)
  opens a socket of any family without that call, so no unit can set
  up a ring; and on amd64 the x86-32 ABI's `socketcall()` is not
  filtered at all, so no unit runs that ABI (the pre-flight refuses a
  32-bit executable). `socketpair(2)` gives no socket outside
  `AF_UNIX`: only `AF_TIPC` implements it besides in 6.12, and `tipc`
  has no autoload alias, so it loads only by root's hand, which
  docs/hardening.md step 5 refuses as well where taken. The list
  filters no protocol number, and the kernel loads a protocol's module
  on the first `socket()` naming it, so `sctp`, `l2tp_ip`, `l2tp_ip6`
  and `smc` are an app's to open unless the operator took that step,
  which makes them unavailable to every program on the box (with
  `dccp`, `tipc` and `rds` against a root-loaded module); motel
  ships no host policy. Open by default; the modules a netlink socket
  or a `socketpair()` loads are in Residual risks (#315).
- Lifecycle: `Restart=no` (the liveswap watchdog is the sole
  restarter of apps; motel.service itself is restarted by systemd
  after a crash, never after a refused start), `KillMode=control-group`,
  `TimeoutStopSec=` (the app's `grace`), no `ExecStopPost=` (the
  instance's socket goes with its run dir when motel retires it,
  `socketRef.retire`; on a user manager a second process of a unit
  cannot join its IPC namespace, #311),
  `LimitCORE=0` on both limits (no core dump of an app; "Where
  `env_file` values sit", above), `LimitNOFILE=` on both limits at the
  user manager's `DefaultLimitNOFILE` when the manager reports one (the
  package raises it with a `user@<uid>.service.d` drop-in),
  `UnsetEnvironment=XDG_RUNTIME_DIR DBUS_SESSION_BUS_ADDRESS`.
- What it runs: `Type=simple` (`oneshot` for a `pre_start`),
  `ExecStart=`, `WorkingDirectory=` (the release dir), `Environment=`,
  `Description=`, `SyslogIdentifier=motel-<app>`,
  `StandardOutput=journal`, `StandardError=journal`.

**The probe stays.** At `App.Start` — every config activation —
motel starts a real transient unit with this property set and reads
back from inside it that both namespaces engaged and that the unit's
own seccomp set is in force (`Seccomp: 2`, and a user namespace
`RestrictNamespaces=` refuses it). The verdict is
cached against the manager connection's generation
(`userManagerClient.cachedSandboxCapability`): later activations on
the same connection read the cache, and a redial (a manager restart,
or motel's own) starts a new generation that the next `Start`
measures afresh. A unit launched between a redial and that next
`Start` — a watchdog relaunch after the manager came back — is not
preceded by a probe; if the host can no longer deliver the namespaces
it fails `226/NAMESPACE` and the watchdog reports it, never a bare
app. The probe is not a
proxy for the systemd version — it is what catches a container, an LXC
VPS or a kernel built without user namespaces, all of which can present
a supported manager version and still refuse the unit. CI sets
`kernel.apparmor_restrict_unprivileged_userns=0` on GitHub's Ubuntu
kernel to make the runner behave like a Debian host, so no lane
exercises a restricted kernel; the probe is the only thing standing
between such a host and a silent loss of isolation. Deleting it would
turn a measurement into a claim.

**What it does not do**, by design: the network namespace is shared
(egress open); there are no per-app UIDs; resource caps are the
operator's to set (`memory_max`, `tasks_max`, `cpu_quota` per app;
none by default). Each is in "Residual risks".

## Reducing the asset (deploy auth)

The two mitigation axes are orthogonal: *isolate the runtime* (above)
and *shrink the prize*. A symmetric, long-lived deploy secret would be
bad on two counts: the verifier would store the same value it checks
(so the prize physically sits in the supervisor's env), and theft would
be permanent. motel uses asymmetric verification — the box holds
only public material:

- **OIDC (CI, primary):** the box verifies a per-run token against the
  provider's public JWKS and a claim allowlist (`deploy_trust github |
  gitlab | oidc`). Nothing high-value on the box, nothing stored in CI;
  the token is minted per run, short-lived, and scoped to the claims
  the block pins. An identity claim (`repository`, `project_path`,
  `sub`, …) is required at config load; a branch binding (`ref` —
  `ref_path` on GitLab — `sha`, `workflow_ref`, a `ref_protected` not
  pinned false, or a default-form `subject`, which carries one) is the
  operator's to add, and a `github` or `gitlab` block some app
  inherits or names without one is warned about at load
  (`warnUnboundTrust`, liveswap/deploytrust.go; an `environment` alone
  is noted, since it binds only as far as the provider's
  deployment-branch rule for it; the generic `oidc` preset requires
  `sub` and is not read): any ref of the pinned identity — any branch a
  collaborator can push, with its workflow edited to deploy — would
  otherwise do.
- **Local key (no OIDC: a laptop, or a runner without it):** the box
  trusts a public key (`deploy_trust local`); the operator mints tokens with the private
  half (`motel deploy-token`). The signing key never touches the
  box. The block's `audience` names the box a token is for; without
  one — warned about at load — a token minted for any box that trusts
  the same key is accepted.

Implementation: `liveswap/deploytrust/` (signatures checked by the
vetted `go-jose`, never hand-rolled; an OIDC token's standard claims by
`go-oidc`, a local one's by go-jose's `ValidateWithLeeway`; then
deploytrust's own checks, `localVerifier.verify` requiring a local
token's `exp` and `iat` and holding it to `max_token_ttl`, and
`matchClaims` holding either kind to the block's claim allowlist; the
issuer's key set is fetched and cached by deploytrust's own `keySet`,
liveswap/deploytrust/jwks.go, which bounds the refetch). Effects on
the model:

- **No deploy credential on the box.** `/proc/<supervisor>/environ`
  would yield ACME tokens, not a deploy key; the top asset is ACME
  tokens.
- **T2 is bounded:** a stolen token is short-lived and claim-scoped, not
  a permanent deploy-anything key. The compromise surface is "the CI
  identity that mints tokens", not "a secret on the box". A `local`
  key held in a runner's secret store is the exception: for an app,
  whoever steals it deploys until its public half is removed from the
  box. For the box's config, signatures still bound what it can land.
- **T4 has no online guessing:** forging a token needs the issuer's or
  operator's private key; there is nothing to brute-force. The
  per-address failure throttle bounds the log-amplification vector.
- **Replay is bounded** by the token's `exp`: for an OIDC token, the
  one its issuer set; for a local one, at most its block's
  `max_token_ttl` after its `iat` (24h unless the block sets one). The
  box refuses a local token minted to live longer, or without an `iat`,
  and `motel deploy-token` warns above an hour.

What it does **not** do: ACME tokens and TLS keys still live on the box,
so the sandbox is still required. Auth design shrinks the prize; it
does not isolate the runtime.

## Residual risks

- **An app's own secrets and its own database are reachable by
  definition** — not a bug, stated so operators do not expect otherwise.
- **Install-time supply chain** (motel's own build) is a CI concern;
  release signing is the roadmap answer, not a runtime control.
- **T5 is contained, not prevented** — a supervisor RCE holds every
  asset short of root. It does not reach root because the supervisor
  holds no grant; keep it that way. The `box` applier is a root unit,
  and the rule it holds is the same one from the other side: root
  consumes nothing from the serving process except a bundle it
  re-verifies from scratch, and the exit status of `systemctl reload`.
  The backup run is another root service, under the same rule: it
  reads nothing the serving process writes, and what its units under
  the motel uid report is decoded strictly against what was asked
  ("Backups").
- **The reload's exit status is the motel uid's word.** The `box`
  applier marks a push `applied` on it. A compromised supervisor can
  say "loaded" and keep serving the old configuration until a restart;
  the file on disk is root's regardless, and `systemctl restart
  motel` is the remedy. Reading the admin API from root to check
  would be the same trust in another form, and was rejected.
- **Root's never-cut-the-branch guard is presence; reachability is the
  handler's.** A pushed Caddyfile that keeps a `box` block, a signer,
  the `box_webhook` site and the signing key passes root's guard. The
  handler also refuses one whose adaptation would answer a push to `/`
  before `box_webhook`, or whose server would refuse the workflow's
  connection first (its protocols, a listener wrapper, the host's TLS
  connection policy, its strict SNI check), moves where that server
  listens (`https_port`, a `bind`: the door compare), or sets a request
  limit — the `servers` option's or a `timeouts` directive's — past
  what a push can meet, a floor or the rate's ceiling (box/DESIGN-box.md, step
  6) — a check a
  supervisor RCE skips with the rest of the handler, and one `motel
  init` does not run. Neither catches a mistyped `claim`, a host
  without a certificate, or a key no laptop holds, any of which strands the box. The remedy is root on the box: `motel init`
  again, or a hand edit the next signed push overwrites.
- **A signer's software-held key is the box.** Nothing on the box
  distinguishes a signature made by the operator from one made by
  whoever holds the operator's key file. A hardware-held key (`sk-`
  types, which the `signer` grammar accepts) reduces a laptop
  compromise to "cannot sign without a touch". Stated so operators
  choose knowingly.
- **Secrets in the config repository are ciphertext for ever**
  (box/DESIGN-box.md, "Secrets"). A reader's key file and a
  clone decrypt every value ever encrypted to that key, offline and
  unseen; removing a
  reader does not take back what was encrypted to them, so the remedy
  for a departure or a leak is new values. Names and lengths are
  public to anyone who can read the repository. No key involved is post-quantum
  (X25519, and RSA for `ssh-rsa` readers). Each is the price of a repository-held secret that
  no CI and no service ever sees in plaintext.
- **A stolen provider account destroys every backup** — the limit of
  the bucket's design (option A; docs/backups.md, "Before you start").
  Nothing the box or the off-box machine holds can destroy a backup,
  and a deletion or overwrite made with either key can be undone for
  180 days if someone looks from off the box at least once every 90,
  which leaves another 90 to recover; whoever has the box can make its
  own `status` say anything, so the look is what counts. The account is
  outside that: its login, with a second factor, and its keys, never on
  the box, are the operator's to keep. Repository rotation under a
  compliance Object Lock is the deferred answer (#169).
- **After root on the box, the repository password and master key are
  the attacker's.** Root had the credential file: the password and,
  through the key files, the master key that encrypts every backup.
  New storage keys stop the attacker writing; they do not unknow the
  password, and restic changes a master key only by moving the data
  into a new repository (`restic copy`) or starting one
  (docs/backups.md, "After an attack"). Anyone who later reads the
  bucket with them reads the backups.
- **`restore --to` is root-equivalent for whoever chooses the
  directory.** It writes where the name leads; the rule that every
  directory on the way is root's and writable by no one else keeps
  others from planting a link, not root's own directories out of
  reach. Restored into `/etc/systemd/system/<unit>.d`, an app's files
  became that unit's settings at the next `daemon-reload` (measured;
  backups/README.md, "A restore"). So it is never to be granted through
  `sudo`.
- **The bucket's protection is the provider's to enforce.** That no key
  can destroy a version, that a delete by name only hides, and that the
  rule keeps old versions 180 days are the provider's behaviour:
  measured through B2's S3 endpoint (`v0.5.0-rc1`) and on MinIO,
  unverified on AWS S3 and Hetzner, and on Hetzner, where every key
  reaches every bucket of its project, not worked out. Whether B2's
  rule removes a marker hidden through its S3 endpoint is read on
  2026-11-04.
- **A directory the box is joined to is trusted as root is** (the
  owner, 2026-09-27; backups/README.md, "What it refuses"). Its
  administrator can give any account of its own the backup account's
  uid, and whoever holds that uid is the restic process and reads the
  credential from its environment. No check on the box stops that.
- **The `motel-backup` account is trusted by name and mark.** Every
  `setup`, run, restore and drill asks that the account of that name be
  the one in `/etc/passwd`, resolved line for line, with motel's
  comment `made-by-motel`, and refuses any other, a directory's
  included (backups/README.md, "What it refuses"). Past that it is
  trusted: what root does to it afterwards — a shell, a password, a
  group — is root's, and whoever then logs in as it reads a running
  restic's environment. Requiring a locked shell and no real home as
  well was deferred by the owner (2026-09-26,
  backups/design/plan.md).
- **Sibling localhost ports** — closed for the contract: nothing
  motel runs listens on a port. Each instance binds a unix socket
  in a directory of its own (`<root>/<app>/run/<nonce>/app.sock`, the
  only `run/` entry in that instance's view), and a sibling's socket is
  outside the view like the rest of the sibling.
  What remains is an app that opens a loopback listener of its own
  accord: that port is reachable by siblings, by the app's own choice.
- **Kernel modules an app can load** — open, accepted by decision
  (owner, 2026-10-22; S3 × O3 × D5 = 45; liveswap/DESIGN-sandbox.md,
  "Module-backed protocols"). Two halves. On a box without
  docs/hardening.md step 5, a `socket()` under `AF_INET`/`AF_INET6`
  with protocol 132, 115 or 256 loads `sctp`, `l2tp_ip`/`l2tp_ip6` or
  `smc` and gives the app a live socket into it: the step is the
  operator's, because a modprobe.d file is the whole box's setting and
  motel's package carries no host policy. On every box, the modules
  an app loads through the families it has. `AF_NETLINK`
  is admitted for `getifaddrs()`, and a `socket(AF_NETLINK, SOCK_RAW,
  n)` loads the module for protocol `n` as any user and hands the app
  a live netlink socket into it (`nfnetlink`,
  `xfrm_user`, `crypto_user`, `ib_core`, `scsi_transport_iscsi` on
  Debian 13), as a generic-netlink `GETFAMILY` by name loads any
  module with a family alias; `socketpair()` loads any aliased
  family's module and runs its create path, then fails with no socket.
  A compromised app reaches those modules' init and request parsers
  and the few requests they answer unprivileged: `crypto_user`'s
  `GETALG` report, `sock_diag`'s socket dumps; `nfnetlink` and
  `xfrm_user` refuse it at their first check. systemd has no filter
  for a protocol argument; blocking `nfnetlink` breaks nftables; a
  longer list in the hardening step (the family modules,
  `crypto_user`) is possible and not decided.
- **A syscall filter the kernel refused** — accepted by decision (user,
  2026-10-19; S5 × O5 × D5 = 125, scores to fix). On a host whose user
  manager already runs under a long outer seccomp chain, the kernel can
  refuse the unit's `SystemCallFilter=` over its 32768-instruction
  budget, and systemd starts the unit without it, with a debug line.
  The unit's other filters still load, so the capability probe's
  checks pass. The app then has its namespaces, view and empty
  capability set, but not the allow list or the `io_uring_setup` deny
  (liveswap/DESIGN-sandbox.md, "What this does NOT stop").
- **The socket name is app-writable, and `connect(2)` follows
  symlinks.** The instance's `run/<nonce>/` is bound writable, so an
  app can replace its socket with a symlink to a socket outside its
  view — motel's admin socket, a sibling's — and a proxy that
  dialled the *name*
  would connect through it in motel's namespace: a confused deputy
  handing the admin API to the world. motel therefore never dials
  the name. The first successful connect opens the file
  `O_PATH|O_NOFOLLOW` (a symlink comes back as the symlink and is
  refused; only `S_IFSOCK` passes) and hard-links that inode, via the
  descriptor's `/proc/self/fd` magic link so no second lookup of the
  name happens, to `<app>/proxy/<nonce>.sock` — a directory no app's
  view contains — and every dial from then on, proxy and prober, goes
  to the pinned name (`socketRef`, liveswap/socket.go). What the app
  does to its own name afterwards is irrelevant. The pinned name is
  unique per instance and never reused — the nonce is 64 random bits,
  so a repeat takes on the order of 2^32 launches of one app — and
  retiring an instance simply unlinks it, so a request that obtained
  the address before the cutover and dials after it reaches nobody —
  never another app (a descriptor number would be recycled; a nonce
  is not). A reattach
  applies the same check before adopting a record and unlinks the
  candidate it pinned if the manager says definitively that the unit
  is not running. Each instance binds in a `run/<nonce>/` of its own,
  so the instance a deploy replaces — running alongside, possibly the
  compromised one the deploy exists to replace — cannot reach the new
  socket's name before it is pinned. Pinned by
  `TestGetUpstreamsRefusesASymlinkUnderTheSocketName`,
  `TestSocketRefPinsTheSocketAndRefusesASymlink` and the reattach
  cases in `TestEnsureRunningRefusesARecordWhoseSocketItCannotDial`.
- **Network egress** is open for every app. A runtime whose permission
  model gates the network (Deno's `--allow-net`, which the socket
  contract lets an app narrow to `unix:{socket}` alone) can close it
  from inside — see
  liveswap/README.md "Runtime permissions"; Node's `--permission`
  model does not cover network I/O. The runtime-agnostic answer is
  `PrivateNetwork=` on the unit — a path socket crosses network
  namespaces, so the contract already allows it — as a per-app opt-in
  when an app that wants no network exists (it would also cut the app
  off from any database): #57.
- **Resource exhaustion** — a runaway app stalls its *siblings*: under
  a fork bomb a second app's requests ran to 10 s while motel, in
  `system.slice`, kept a 5–9 ms median (the root splits CPU evenly
  between the two slices), a leak is taken by the global OOM killer
  (the app first: user-manager units run at `oom_score_adj` 200 and
  motel.service at 0 — systemd's defaults, which motel does not
  set and no test holds) and forks
  are bounded by the user manager's default `TasksMax=` (measured;
  DESIGN-liveswap.md "Resource caps"). Per-app `memory_max`,
  `tasks_max` and `cpu_quota` bound an app the operator sets them on,
  and `ProtectControlGroups=` keeps the app from rewriting them; none
  is set by default, so an unbounded app is still the default, and a
  ceiling for the apps as a whole is the operator's slice setting
  (docs/hardening.md step 6).
- **`state.json` must stay outside any writable sandbox view**
  (liveswap/state.go is read on relaunch for the version and the
  unit, and checked before either is used: a version that is not one,
  a unit that is not this app's, a nonce that is not one — refused,
  never a path). It is written and read by the functions the deploy
  records are (liveswap/ownfile.go): a fresh temp file renamed over
  the name, and a read that follows no link at the name, does not
  block on a FIFO and refuses anything but a regular file of a state
  file's size — at recovery, a permanent error naming the file, never
  a reset. Unlike the records' directory (recordsDir), the app dir
  above it is not checked by the read; the launch path checks it
  (resolveBindSources), and every save follows a launch.
  Normative and shipped: only the release
  being started, `shared/` and the OS base view are bound into the
  unit — the app dir root, `state.json`, `tmp/` (the upload staging
  dir: a running instance must not be able to rewrite the next
  version's tarball), `deploys/` (each version's recorded deploy
  outcome; `TestDeployRecordsAreOutsideTheSandboxView`) and the
  other releases do not exist inside.
- **The deploy record store is a trust boundary in both directions**
  (liveswap/deploys.go states its four rules; every function there
  and both filters hold them). A record is written once, through the
  filter, into a directory verified as the app's own — so no known
  secret is on disk in it (a version equal to one is the exception,
  and is already the record's file name and the release directory's),
  and a secret rotated later is not in an old record for the new
  filter to miss. Anything read back is text
  from disk: served only through the filter, trusted for nothing
  else unless it proves itself a record (a regular file, a valid
  version name, an object naming that version). One that does orders
  the releases: its write time is its version's deploy time, which
  release GC and `available_versions` follow, since a release dir's
  own mtime is the running app's to set (liveswap/state.go, "Release
  order"); a release with no record sorts after every recorded one,
  by that mtime among its kind, and is named in the status
  (`ordered_by_mtime`). Nothing from disk
  reaches a response except through the filter as body text; a name
  read off disk — a release directory's, a record's, `state.json`'s —
  is a safe string like any other and, like any other, never one
  equal to a known value (nothing on disk says who chose a name);
  and nothing is appended to a body after the
  filter's final pass but what the filter's own second rule names.
  Nothing there
  follows a link, ancestors included, and the store never blocks a
  deploy or a status.
  `sandboxSpecFor` in liveswap/sandbox.go is the single place that
  list is built; `TestSandboxSpecFor` and
  `TestSandboxViewIsExactlyWhatIsNamed` pin it — the latter asserts the
  rendered set of bind destinations IS the view, so an accidental
  widening fails there.
- **Log amplification is bounded, not closed.** The auth-failure
  throttle caps what the webhook writes to the journal (about 110
  lines a minute from any number of sources); an operator-enabled
  access log records every request regardless, and the verification
  CPU per request is not throttled (see the webhook section for why).

## History

Dated one-liners; the full text of each is in git.

- 2026-08 — Threat model written as a decision record comparing three
  isolation stacks (A: UID + hardening + AppArmor; B: bubblewrap +
  delegated cgroups; C: root-owned systemd template). B was recommended.
- 2026-08-28 (#29, `465e846`) — Shared deploy secret (`LIVESWAP_SECRET`,
  `X-Liveswap-Secret` header) replaced by `deploy_trust` (OIDC + local
  key). The former top asset left the box.
- 2026-08-29 (#34, `17f1a04`) — Apps run as transient units on the
  motel user's own manager: neither B nor C. Restart-survival
  (`Reattach`) shipped with it.
- 2026-08-30 (#38, `6a080d0`) — Non-dumpable supervisor; the shared-UID
  rule recorded, corrected by the spike measurement that a user
  namespace *does* close cross-process `/proc` reads. That measurement
  made systemd's own namespaces the mechanism.
- 2026-08-30 (#40 branch; merged 2026-09-02 as `6e8dfa2`) — Sandbox
  built: two probe-gated tiers (*filesystem* on systemd < 256, *full*
  ≥ 256), deny-by-default view (replacing `ProtectSystem=strict` + a
  derived `InaccessiblePaths=` set that could go stale — the "view is a
  policy" residual). Ubuntu needed an AppArmor profile attached by path
  to a user-manager wrapper.
- 2026-09-01 (`3446a53` on the #40 branch; tier code removed in #47
  `ce97f37`) — Debian 13 only; one tier; AppArmor profile and wrapper
  deleted; CI sets `apparmor_restrict_unprivileged_userns=0`.
- 2026-09-03 (#48 `5e36764`) — Probe measured once per manager
  connection; failure never cached.
- 2026-09-03 (#49 `20e61c1`) — `sandbox auto`/`require` collapsed to
  `on|off`.
- 2026-09-04 (#51 `24ff8e9`) — `sandbox off`, the recorded tier and the
  status field removed; the runner refuses a spec without a sandbox;
  reattach verifies the live unit's namespaces. Closed by construction:
  the bind-source TOCTOU race (no attacker-controlled app runs outside
  a sandbox; motel and the user manager still share the uid, and
  are the trust domain), the app-writable recorded tier (no record), and
  "what a bare app leaves behind in `shared/`" (nothing runs bare). The
  pre-#40 "reachable today" attack-path table described bare apps and
  is history with them.
- 2026-09-06 — App contract moved from `127.0.0.1:$PORT` to a
  per-instance unix socket in `<app>/run/`; the sibling-port residual
  closed for every runtime, compiled apps included.
- 2026-09-07 — The two owed non-isolation items closed: webhook auth
  failures throttled in the journal, per address and process-wide (T4
  log amplification bounded; deploys never refused), and
  `max_artifact_entries` plus PATH_MAX/NAME_MAX name caps in
  extraction (T3 inode exhaustion closed), with a 75% warning so a
  growing app sees either cap coming.
- 2026-09-16 (#106) — Optional `sha256` on a pull deploy, checked on
  the download stream; T3 no longer chooses the bytes of a pinned
  pull. Actor and attribution claims in `deployed_by` (#111) landed
  the day before.
- 2026-10-09 — `box` designed (box/DESIGN-box.md): the config
  repository becomes the only writer of `/etc/motel/Caddyfile`,
  through an OIDC-authenticated webhook and a root applier that
  verifies an SSH commit signature, the file's membership in the
  commit, and descent from the commit the box runs. The configuration
  at rest joins the asset list (5); T6, the compromised config-repo
  identity, joins the profiles; the `MOTEL_CONFIG` sudoers grant
  leaves the model when the applier ships, the secrets grant when
  secrets ride the same channel.
- 2026-10-11 — `box` built: the handler (step 6 reachability
  included), the root applier, `motel init` and the template that
  applies by signed push (box/DESIGN-box.md, History). `bin/push` and the
  `MOTEL_CONFIG` grant are gone from the template; the tutorial sets
  up the config repository on day 0, an administrator is optional and
  holds only the secrets grant, and root keeps key-only SSH as the way
  back.
- 2026-10-12 — Secrets through the config channel designed
  (box/DESIGN-box.md, "Secrets"): age files in the repository, root
  decrypts after every proof; new facts stated — a reader's key and a
  clone decrypt, offline, every value encrypted to it; names and
  lengths are public to repository readers; no key is post-quantum.
- 2026-10-13 — The template's secrets grant is gone (box PR 7e2):
  secrets are commits in the config repository, and an administrator
  is an account in `adm`, for the journal alone.
- 2026-10-16 (#234) — App units run with `LimitCORE=0` on both limits:
  a crash writes no dump of the app's memory, env values included, so
  the fifth route for `env_file` values is closed — by the kernel
  under a file `core_pattern` (Debian's default), by the collector's
  own limit check under a piped one. No app can write a core dump for
  debugging.
- 2026-10-18 (#298) — The syscall filter takes `io_uring_setup` out of
  `@system-service` and app units run the native ABI only, closing two
  routes to a socket of a family `RestrictAddressFamilies=` leaves out;
  `socketpair(2)` is the one left (#315). Sibling IPC joined the
  residual risks (#311).
- 2026-10-22 (#315) — `socketpair(2)` gives no socket outside `AF_UNIX`
  on Debian 13 (`tipc`, the one other family implementing it, has no
  autoload alias). The module-backed protocols under
  `AF_INET`/`AF_INET6` (`sctp`, `l2tp_ip`, `l2tp_ip6`, `smc`) are the
  operator's to make unavailable (docs/hardening.md step 5; a file
  shipped by the `.deb` was built and declined), open by default; they
  and the modules a netlink socket or a `socketpair()` loads joined the
  residual risks, accepted by decision.
- 2026-10-23 (#311) — Sibling IPC closed: `PrivateIPC=yes` and a
  `/dev/shm` tmpfs of each unit's own, at the host's 50%
  (liveswap/DESIGN-sandbox.md, 2026-10-23). The Sibling IPC residual
  row is gone. Every unit now has one `Exec*=`: the `ExecStopPost=`
  that removed the instance's socket is gone, since on a user manager
  a second process the manager spawns cannot join the unit's IPC
  namespace; the probe checks the IPC namespace and `/dev/shm` from
  inside the unit, as it does the other namespaces.
- 2026-11-02 (#172) — Backups joined the model ("Backups"): the
  repository credential is asset 2, and the assets after it move down
  one; the principals, what each attacker gains, and six residual
  risks, the stolen provider account first (#169), with what
  `v0.5.0-rc1` measured through B2's S3 endpoint.
