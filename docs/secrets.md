# Secrets

An app's secrets are commits, like the box's config. Each value is a
file of its own in the box's repository, encrypted on your laptop with
[age](https://age-encryption.org) to the box's own key and to every
reader's key: the signers' SSH keys, or an age key of a person's own.
The workflow carries the ciphertext to the box with the Caddyfile. The
box checks the commit's signatures, then decrypts the values with its
own key and writes one env file per app, which motel reads each time
the app launches. GitHub and the workflow only ever hold ciphertext,
and nothing is typed onto the box.

[After the first deploy](after-first-deploy.md#2-secrets-if-the-app-has-any)
walks through the first secret. This page is the reference: every task
after that, who can read what, and the risks that come with keeping
secrets in a repository. The box's side is specified in
[box/DESIGN-box.md](../box/DESIGN-box.md#secrets), "Secrets".

```
deploy.example.com/
  Caddyfile
  secrets/
    recipients            # who can read: the box's key first, then each reader's
    example/
      DATABASE_URL.age    # one value of the app example, encrypted to all of them
      STRIPE_KEY.age
```

The box writes the app's values to `/etc/motel/secrets/example.env`,
which only root and motel can read and no app's sandbox contains.
The app's block reads it with:

```
env_file /etc/motel/secrets/example.env
```

On the laptop you need `age` (`brew install age`, or `apt install
age`) besides git and `make`. The `make` and `git` commands below run
at the top of a checkout of the box's repository, where its Makefile
is, not in a box's directory. With more than one box in it, name the
one you mean: `BOX=deploy.example.com`.

## The box's key

The package makes the box's key when it is installed or upgraded on a
running system, and `motel init` makes it if it is missing. Root on
the box prints it:

```sh
ssh root@box.example.com motel box key
```

The output is the key alone, one `age1…` line, so `BOX_KEY=$(ssh
root@box.example.com motel box key)` passes it to a command. It is
public: the first line of `recipients` that is not a comment is this
key. The private half is `/etc/motel/age/identity`, readable by root
alone; it never leaves the box, and losing it loses nothing a reader
cannot encrypt again.

## Tasks

### Add a secret

The box's first secret also needs the box's key, so that `make secret`
can write `recipients`:

```sh
make secret APP=example VAR=DATABASE_URL BOX_KEY=$(ssh root@box.example.com motel box key)
```

After that, `BOX_KEY` is not needed:

```sh
make secret APP=example VAR=STRIPE_KEY
```

It asks for the value without showing it as you type, and takes the
line you type or paste exactly as it is, however long, up to the box's
cap. It reads the keys itself, since a terminal's own line editing
holds at most 1024 bytes on a Mac and drops the rest, so it takes no
edits either: a Backspace or an arrow key refuses the value (`… has a
control character in it: typing is not shown, so the prompt takes no
edits (Backspace, an arrow key); type or paste it again, or pipe it
in`), and you type or paste it again. A pipe works too, and is the way
to bring a value from a password manager's command line or the
clipboard without typing it, never via the disk or your shell's
history:

```sh
pbpaste | make secret APP=example VAR=STRIPE_KEY     # on Linux: xclip -o, or wl-paste
```

Either way the value goes straight into `age`; it is never written to
the laptop's disk. The value is taken exactly as given: spaces and
quotes are kept, and one trailing line break is dropped (the one
`echo` or a clipboard adds). A value with a line break inside it, a NUL
byte, or bytes that are not UTF-8 is refused, since an env file holds
one variable per line. An empty value is refused too: a variable that
is empty, or not secret, is an `env` line in the app's block instead.

`make secret` stages the file, says whether the app's block reads its
secrets yet, and prints the `env_file` line it needs if not. Commit
(signed) and push. The run names the variables the box added, changed
or removed, never their values.

A Deno app also needs `--allow-env=` in its block to name each
variable it reads.

### Change a value

The same command: `make secret APP=example VAR=STRIPE_KEY`. A push that
changes only secrets reloads nothing, and the running app keeps the
values it started with until its next launch ([When a value reaches
the app](#when-a-value-reaches-the-app)).

### See what there is

```sh
make secrets
```

lists each box, its readers, and each app's variable names. It needs no
key: names are in the clear, as file names. It also names a signer who
is not a reader yet.

### Read a value back

```sh
make secret-show APP=example VAR=STRIPE_KEY
```

prints the value on the terminal. It reads with the private key this
checkout signs with (`make signer` set it up), or with the one you name:
`IDENTITY=~/.ssh/id_ed25519`, or an age identity file. Only a reader
can read a value back.

### Remove one

```sh
git rm deploy.example.com/secrets/example/STRIPE_KEY.age
```

Commit and push. The box rewrites the app's env file without it. Removing
an app's last secret (its whole directory) removes its env file, and
the app's block must then lose its `env_file` line in the same commit.

### Bring in an env file

```sh
make secrets-import APP=example < example.env
```

sets one secret for each `KEY=VALUE` line, reading the file the way
motel reads an env file: `#` comments and blank lines skipped, an
optional `export ` dropped, one pair of quotes around a value taken off.
For a file that is already on the box, see [Move a hand-made env file
in](#move-a-hand-made-env-file-in).

### A notify channel's URL

A channel the box's `monitor` block names (notify/DESIGN-notify.md) is a
URL that is a credential by itself, so it is a secret too, of the box
rather than of an app: `APP=_box`, named `NOTIFY_` then capitals.

```sh
make secret APP=_box VAR=NOTIFY_OPS_URL
```

The box writes these to `/etc/motel/secrets/_box.env`, which notify
reads and no app is given, and reloads motel when they change: a
channel's new URL is in use once the push is applied. The block reads
each by name:

```
monitor {
    channel ops ntfy {secret.NOTIFY_OPS_URL}
    operator ops
}
```

A push whose `monitor` block names a secret `secrets/_box/` does not
hold is refused, as is a `_box/` value with no `monitor` block with a
channel to read it. A `NOTIFY_` variable among an app's own secrets never reaches the
app: motel withholds the prefix from every env file in
`/etc/motel/secrets/`, where the box writes an app's (an env file of
your own kept there is held to it too), says so in the journal at each
launch, and `make check` warns of it; name such a variable otherwise if
the app reads it.

### Let a new signer read them

`make signer`, run by the new person, also adds their key to every
box's `recipients` when age can encrypt to it ([A key age cannot
use](#a-key-age-cannot-use) when it cannot). Values already there were
encrypted before they were a reader, so someone who can read them
re-encrypts every value to the readers `recipients` now lists, once
for each box that has secrets:

```sh
make rekey BOX=deploy.example.com
```

(`BOX=` can be left out when the repository holds one box.) The values
pass through a pipe from one `age` to the other and never touch the
disk. Commit and push. Adding someone as a signer is the box README's
[Adding a signer](../examples/box/README.md#adding-a-signer).

### Remove a person

Their `signer` line out of each box's Caddyfile, signed by someone else
(a box refuses a file that drops the key that signed it), their key and
its comment out of each `recipients`, then `make rekey BOX=…` for each
box, commit, push.

That stops new ciphertext reaching them, and nothing more. They keep
whatever they could already read, which is every value encrypted to
their key in the repository's history, in any clone they have: age has
no revocation. **Change those values too**: a new password at the
database, a new key at the provider, then `make secret` for each and a
deploy of a new version of each app.

### A key age cannot use

age encrypts to `ssh-ed25519` keys and to `ssh-rsa` keys of 2048 bits
or more. It cannot use a hardware `sk-` key, or a key an agent holds
with no file beside it. (An ECDSA key, which Secure Enclave tools make,
is not a signer key either: a box refuses it, since its signatures have
two forms — make an ed25519 key to sign with.) A signer with one of
those reads with an age key of their own instead:

1. Make one: `age-keygen -o ~/.config/age/motel.txt` prints its
   public `age1…` line. For one held in hardware, `age-plugin-yubikey`
   or `age-plugin-se` makes it instead.
2. Put that line in each box's `recipients`, under a `#` line naming
   you, and commit it.
3. A reader runs `make rekey BOX=…` for each box, and pushes.
4. Read with `make secret-show APP=… VAR=… IDENTITY=~/.config/age/motel.txt`.

A plugin's key needs that plugin installed wherever someone encrypts to
it, so every reader who runs `make secret` or `make rekey` needs it too.

### A rebuilt box

A new box has a new key. Install motel and run `motel init` from
the box's directory, as the first deploy did; then, as a reader:

```sh
make rekey BOX=deploy.example.com BOX_KEY=$(ssh root@box.example.com motel box key)
```

encrypts every value again, to the new key in place of the old one.
Commit and push, **before the app's first deploy to the new box**:
`init` installs no secrets, so until this push applies the box has no
env file for the app, and a launch that reads a missing one fails.
Until then the workflow also stops before it sends anything, since the
values in the repository are encrypted to the old key, and names the
new one: `the box's key is <age1…>, but the first recipient in
deploy.example.com/secrets/recipients is <age1…>: a rebuilt box has a
new key. …` ([What is checked, and where](#what-is-checked-and-where)).

### A box cloned from a snapshot

A virtual machine made from a snapshot of a box holds a copy of that
box's key and of its env files, values and all. If the clone takes the
original's place and the original is gone, nothing needs doing. If it
becomes a second box:

1. Give it a key of its own, as root on it:
   `rm /etc/motel/age/identity && motel box key`.
2. Remove the env files of the apps it will not run, under
   `/etc/motel/secrets/`.
3. Set it up as any new box, with a directory of its own and
   `motel init` from it. Its first secret takes its new key:
   `make secret APP=… VAR=… BOX=<its host> BOX_KEY=…`.

### A lost laptop

The laptop's SSH key signed commits and read secrets, and the laptop
holds a clone of the repository: treat every value it could read as
known.

1. From another signer's laptop, remove the lost key as in [Remove a
   person](#remove-a-person), and push.
2. Change every value with `make secret`, and deploy a new version of
   each app.

If the lost key was the only signer, the way back is root on the box:
put a new key in the Caddyfile and run `motel init` again from the
box's directory, copied over (the box README's
[At 3am](../examples/box/README.md#at-3am)). If it was also the only
reader, nobody can run `make rekey`, and nothing needs it: remove the
lost key's line from each `recipients` (`make signer` with the new key
adds its own), then set every value anew with `make secret`, which
only encrypts, to the keys `recipients` lists. New values, since the
lost key still reads the old ones in the repository's history.

### Move a hand-made env file in

An env file made on the box by hand, say `/etc/motel/example.env`,
comes into the repository without passing through the laptop's disk:

```sh
ssh root@box.example.com cat /etc/motel/example.env | make secrets-import APP=example
```

(with `BOX_KEY=$(ssh root@box.example.com motel box key)` too, if it
is the box's first secret). Then:

1. Change the app's block to `env_file /etc/motel/secrets/example.env`,
   commit and push. The run names the variables as added.
2. Once that run is green, no launch reads the old file. Remove it, as
   root on the box: `rm /etc/motel/example.env`.

An env file made by hand keeps working where it is, for as long as you
like: `env_file` is a path, and the box writes only under
`/etc/motel/secrets/`. A file made there by hand, for an app the
repository has secrets for, is taken over and overwritten by the next
push.

## When a value reaches the app

motel reads an app's env file each time the app launches: a
deploy, a rollback, or a relaunch after a crash or a reboot. A push
never restarts a running app, and a push of apps' secrets alone reloads
nothing (the box's own, `_box/`, reload motel: [A notify channel's
URL](#a-notify-channels-url)). So a value that must change now — a leaked key, say — is
`make secret`, a push, and then a launch of the app. The quickest is a
rollback to the version that is running: in the app's repository,
Actions → deploy → **Run workflow**, with the running version as the
version to roll back to. The box relaunches that version from its
disk through the same health gate and cutover as a deploy, with no
build and no downtime, and the run's output ends with the app's status,
whose `last_deploy.note` reads `relaunched <version> (same artifact);
it reads its env file now`. A deploy of a new version launches it with
the new value too. Re-running the deploy's own workflow run also
relaunches it when the tarball it sends is byte for byte the one
running (the box compares the digests and fetches nothing), but the
template workflows rebuild the tarball on a re-run and the build
stamps it with the time, so for them that re-run is refused as a
different artifact under a version the box already has — use the
rollback. Either launch is a deploy: the app's in-memory state goes
with the old instance.

An env file edited or deleted on the box by hand is put back by the
next push, as a hand edit of the Caddyfile is: the repository is the
truth. The run says so, as a warning:

```
apply: example's env file on the box had been edited there since the last apply; this push replaced the edit with deploy.example.com/secrets/example/ at 9be0d44
```

## What is checked, and where

`make check`, which the workflow runs on every push and pull request,
prints a secrets count on each box's line:

```
deploy.example.com/Caddyfile → https://deploy.example.com/ (1 app, 1 signer, 2 secrets)
```

It refuses, before anything is merged, what the box would refuse; the
box refuses the same when a push arrives, and the run quotes the
reason. Each is in
[box/DESIGN-box.md](../box/DESIGN-box.md#which-checks-run-where),
"Which checks run where":

- anything in `secrets/` other than `recipients` and app directories
  of `<VAR>.age` files: a stray file, a link, a name outside the
  grammar;
- secrets for an app the Caddyfile does not declare;
- an app with secrets whose `env_file` is not
  `/etc/motel/secrets/<app>.env`, or one whose `env_file` is that
  path with no secrets;
- an app whose `env_file` is another app's file, or anywhere else in
  `/etc/motel/secrets`;
- a variable an `env` line in the app's block also sets (the `env` line
  would win, so the secret would never reach the app), and one named
  `SOCKET`, which motel sets itself;
- a value that is not an age file the box reads (an ASCII-armored one,
  say);
- values with no `recipients` file beside them, or a `recipients` whose
  first recipient is not the box's `age1…` key, or a line age does not
  encrypt to.

It warns about two names a case-insensitive disk would merge, such as
`DATABASE_URL.age` and `database_url.age` (the box keeps both, but a
Mac's checkout holds only one); about a reader's SSH key of a type age
skips, whose holder cannot read the secrets; and about a name that
starts with a dot, which it does not read and the box refuses if it is
committed.

The box alone checks what needs its key: that each value is encrypted
to it, decrypts whole, and is a value an env file can hold. The counts
and sizes have limits — how many apps have
secrets, how many each app has, how large an app's env file may be —
listed with their reasons in [Caps](../box/DESIGN-box.md#caps); `make
check` warns at three quarters of each, in the pull request, before a
push meets the limit.

Before it sends anything, the workflow compares the box's key with the
first line of `recipients`, and stops, with nothing sent, on:

- another key on the box: `the box's key is <age1…>, but the first
  recipient in <host>/secrets/recipients is <age1…>: a rebuilt box has
  a new key. Confirm it with ssh root@<host> motel box key, then
  make rekey BOX_KEY=<age1…>, commit and push; nothing was sent`
  ([A rebuilt box](#a-rebuilt-box));
- no key on the box: `the box names no key for secrets (no recipient in
  GET /): run motel box key as root on it; nothing was sent`;
- values with no recipients file: `<host>/secrets holds values but no
  recipients file names a key: make secret APP=… VAR=… BOX_KEY=$(ssh
  root@<host> motel box key) makes one; nothing was sent`.

## Who can read what

| Who | The values | How |
|---|---|---|
| Root on the box | Yes | The box's key, and the env files it writes |
| A reader: each key in `recipients` | Every value encrypted to that key, in all of the repository's history | `make secret-show`, or `age -d` on any clone |
| A signer | Yes, whatever `recipients` says | One signed commit can add a site that serves any file the `motel` user reads, env files included, so a signer's key is every app's secrets ([box README](../examples/box/README.md#secrets)). The template makes every signer whose key age can use a reader too |
| The `motel` user, and so a compromised motel | The env files | It reads them at each launch, as it always has; it cannot read the box's key |
| An app | Its own values, in its environment | motel passes them at launch; `/etc/motel` is in no app's sandbox |
| GitHub, the workflow, any token or app with read access to the repository, a fork | Never a value | Ciphertext only; see the next section |
| An administrator in the `adm` group ([Hardening](hardening.md#3-an-administrator-if-you-want-one)) | No, unless an app writes one to its own output | The journal only, which holds every app's output |

## What GitHub and CI see

Anyone who can read the repository learns which apps have secrets, what
each variable is called, how long each value is (to within two bytes:
age pads nothing), when each changed, and the readers' public keys and
the names in `recipients`' comments. A pull request shows which
variables it adds, changes or removes, as file names, and never what
they hold. That is the price of names in the clear: in return a
reviewer sees what a change touches, and `make check` can hold names
and sizes to the box's rules before anything is merged, with no key.

## The risks you take on

Stated in full in
[DESIGN-threat-model.md](../DESIGN-threat-model.md#residual-risks),
"Residual risks", and in its "Config webhook" section:

- **The ciphertext is kept for ever.** GitHub, every clone and every
  fork hold it. A reader's private key and a clone decrypt every value
  ever encrypted to that key, offline, and nothing on the box or at
  GitHub sees it happen. A signer's SSH key file is a reader's key, so
  guard it as you would the values; a passphrase on it helps. Removing
  a reader takes back nothing; new values do.
- **Names and lengths are public** to anyone who can read the
  repository.
- **No key involved is post-quantum.** The box's key is X25519, and
  age wraps an SSH key through X25519 or RSA. Ciphertext kept for ever
  is open to a quantum computer that does not exist yet. age's
  post-quantum keys cannot share a file with SSH-key readers; moving
  every reader to one is an open question in the design.
