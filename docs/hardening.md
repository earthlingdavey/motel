# Hardening the box

[Your first deploy](first-deploy.md) leaves a working box with the
doors a fresh server has. This page closes them: SSH by key only, the
SSH port shut to everyone but you, and, if you want them, an
administrator who is not root, Debian's updates for its release —
security fixes and point releases — installed on their own, and seven
network protocols the kernel would otherwise load for any program
refused. It is recommended, and none of it is needed to deploy.

Nothing here changes how the box deploys or takes its config: app
deploys authenticate with the workflow's token, and config with a
signed push, and neither uses an account on the box. None of it
touches the running app.

Names are the ones from the first page, plus `alice`, the optional
administrator.

## 1. SSH by key only

As root on the box:

```sh
printf 'PermitRootLogin prohibit-password\nPasswordAuthentication no\n' > /etc/ssh/sshd_config.d/10-hardening.conf
sshd -t && systemctl reload ssh      # -t checks the config first: a bad one would lock you out
```

Keep that session open. In a second terminal on your laptop, log in
again — a new login is the only real test of the change:

```sh
ssh root@box.example.com echo ok     # prints ok: root's key still gets in
```

If it prints `ok`, you are done. If it says `Permission denied`, close
nothing: undo the change in the first terminal with `rm
/etc/ssh/sshd_config.d/10-hardening.conf && systemctl reload ssh`, and
find out why before trying again.

Nobody logs in with a password from here on: Debian allows one for
every account but root. Root
keeps logging in with its key — Debian's default; the line says so in
case the provider's image changed it — because root over SSH is the
way back [at 3am](after-first-deploy.md#at-3am), and a provider's web
console is a poor substitute. Treat root's key accordingly.

## 2. Shut the port you are not using

At the provider's firewall, allow **22 only from your own address**
(home, VPN). 80 and 443 stay open to the world; nothing else on the
box listens on a port, since every app is reached over a unix socket.
If your address changes, change the rule in the provider's firewall
settings, which do not need the box.

## 3. An administrator, if you want one

An account that is not root, for reading the logs: for a second
person, or to keep root's key for when it is needed. It reads
moteld's journal and every app's through the `adm` group, and that
is all it holds. It needs no `sudo`: the config changes only by signed
pushes from the box's repository, and so do the apps' secrets kept
there ([Secrets](secrets.md)).

From the laptop, put the administrator's SSH public key on the box —
yours, or the second person's:

```sh
scp alice.pub root@box.example.com:      # the administrator's public key
```

Then as root on the box:

```sh
adduser --disabled-password --comment "" alice      # no password: SSH keys only
install -d -m 0700 -o alice -g alice /home/alice/.ssh
install -m 0600 -o alice -g alice alice.pub /home/alice/.ssh/authorized_keys
usermod -aG adm alice
```

Check from the laptop:

```sh
ssh alice@box.example.com journalctl -u moteld -n 5   # the journal, through adm
```

What this grants: `adm` reads the whole system journal, not only
moteld's lines, so it is a trust grant in its own right. An app that
writes a secret to its own output puts it there too.

## 4. Debian's updates on their own, if you want them

Debian's `unattended-upgrades` installs Debian's updates for the box's
release — security fixes and point releases — by itself, once a day. Like step 3 it is optional, and nothing
in it stops or restarts an app.

What it does: normally between 06:00 and 07:00 in the box's time zone
(`systemctl list-timers apt-daily-upgrade.timer` shows the next run;
a run the box was off for happens after it boots)
it installs what Debian has published for the release the box runs,
from Debian's security archive and, at a point release, from the
release itself — and nothing from anywhere else. A daemon whose
package restarts it on upgrade runs the fix at once: `sshd` is one,
and open SSH sessions stay up, since the restart takes only the
listener. Everything started afterwards runs the fixed code: each app
at its next launch (a deploy, a rollback, a relaunch after a crash),
each SSH login, and the `ssh-keygen` a push to the box's repository
runs.

What it does not do: update moteld, whose `.deb` comes from no
repository ([Upgrading](upgrading.md) is how), or an app's runtime
that did not come from Debian, such as the Deno under
`/usr/local/bin` or the Node example's own executable
([Patching the host](upgrading.md#patching-the-host)). Nor does it reach what
a running app or daemon has already loaded, which waits for a
relaunch, or the running kernel, which waits for a reboot, both at a
time you choose; apps run on the box's own `/usr`, so a file an app
first reads after the upgrade is the new one. An app launched long ago
can thus be behind what the box has, and
[Patching the host](upgrading.md#patching-the-host) says how to give
it a launch. It never reboots the box: `Automatic-Reboot` is off
unless you turn it on, and this page does not. When a kernel update
lands it leaves the reboot to you and notes it: `cat
/run/reboot-required.pkgs` lists the packages waiting for one, and
the file is absent while none does.

As root on the box, first see whether it is already on, since a
provider's image may ship it:

```sh
apt-config dump APT::Periodic::Unattended-Upgrade    # "1": on; "0" or nothing: off
```

If it is off, install the package — on a box without it, this writes
`/etc/apt/apt.conf.d/20auto-upgrades`, which turns it on — and run
the check again:

```sh
apt install unattended-upgrades
```

If it still does not print `"1"`, the image installed the package
turned off, and installing again changes nothing, so write the file
the package writes when it is on:

```sh
printf 'APT::Periodic::Update-Package-Lists "1";\nAPT::Periodic::Unattended-Upgrade "1";\n' > /etc/apt/apt.conf.d/20auto-upgrades
```

In every case, check that it is on and what it would do:

```sh
apt-config dump APT::Periodic::Unattended-Upgrade    # "1"; anything else: a later file in /etc/apt/apt.conf.d/ turns it off (grep -r Periodic::Unattended-Upgrade /etc/apt/apt.conf.d/)
apt-config dump Unattended-Upgrade::Origins-Pattern    # three origin=Debian lines, nothing else
apt-config dump Unattended-Upgrade::Automatic-Reboot   # prints nothing, or "false": no automatic reboot
unattended-upgrade --dry-run -v    # what the next run would install; "No packages found" on a box that is up to date
```

Every run, this dry run included, writes what it did to
`/var/log/unattended-upgrades/unattended-upgrades.log`.

## 5. Kernel modules no app needs, if you want them

Every app runs with its network families cut to the four it needs
(the sandbox table in [liveswap/README.md](../liveswap/README.md#sandbox)),
but the kernel loads a protocol's module on the first socket that
names it, for any program, and under those four families three such
protocols are modules on Debian 13: SCTP, L2TP over IP and SMC. A
compromised app could open one and reach kernel code nothing on a web
server uses. This step tells the kernel's module loader to refuse
them, and with them DCCP, TIPC and RDS, which the CIS Debian benchmark
refuses on every server; those three load only by root's hand on
Debian, and a loaded TIPC takes configuration from any program. Like
steps 3 and 4 it is optional, and nothing in it stops or restarts an
app. It is the box's setting, not motel's: it holds for every
program on the box, which is why motel's package does not write it.
Skip it on a box that also runs an L2TP VPN server or an SCTP
workload, which a motel box does not.

As root on the box:

```sh
printf 'install %s /bin/false\n' sctp l2tp_ip l2tp_ip6 smc dccp tipc rds > /etc/modprobe.d/motel-protocols.conf
```

Check that the loader reads it, and that none of the seven is loaded
already — a loaded module stays until reboot:

```sh
modprobe -n -v sctp    # last line: install /bin/false (-n loads nothing)
lsmod | grep -Ew 'sctp|l2tp_ip|l2tp_ip6|smc|dccp|tipc|rds'    # prints nothing
```

What it does: `install <module> /bin/false` replaces loading the module
with a command that fails, so the kernel's autoload, a hand
`modprobe`, and a module that depends on one of the seven all stop,
with `Error running install command '/bin/false' for module <name>`
on the hand run. The way back is
`rm /etc/modprobe.d/motel-protocols.conf`; nothing else holds the
setting.

## 6. A ceiling for all the apps together, if you want one

Each app can be bounded on its own (`memory_max`, `tasks_max` and
`cpu_quota` in its block — [liveswap/README.md](../liveswap/README.md#resource-caps)),
and that is what keeps one app's burst from stalling another. What
that does not set is a ceiling for the apps *as a whole* against
moteld itself, which shares the box with them: moteld runs in
`system.slice` and every app under the `motel` user's slice, and
the two halves already split the CPU evenly under load, but nothing
bounds the apps' memory as a set. If you want that, set it on the
user's slice. Like the steps before it this is the box's setting, so
motel's package does not write it, and nothing in it stops or
restarts an app.

As root on the box, with the sizes that fit it (here a 4 GB box
keeping 1 GB back):

```sh
systemctl set-property user-$(id -u motel).slice MemoryMax=3G CPUQuota=300%
```

It applies at once to everything under the slice and persists across
reboots (systemd writes it under `/etc/systemd/system.control/`).
Check it:

```sh
systemctl show user-$(id -u motel).slice -p MemoryMax,CPUQuotaPerSecUSec
cat /sys/fs/cgroup/user.slice/user-$(id -u motel).slice/memory.max    # prints the bytes
```

Use `set-property`, not a drop-in file: on Debian 13's systemd 257 a
drop-in under `/etc/systemd/system/user-<uid>.slice.d/` followed by
`daemon-reload` shows the new value in `systemctl show` but does not
re-apply it to the running slice (measured; systemd 255 and 259 do),
so the ceiling would hold only from the next boot. The way back is
`systemctl set-property user-$(id -u motel).slice MemoryMax=infinity CPUQuota=`,
which lifts both at once; `systemctl revert user-$(id -u motel).slice`
then removes the files `set-property` left under `system.control`. Not
`revert` on its own: it removes the files but leaves the live ceiling
in the kernel until the next boot (measured, systemd 257).

What hitting the ceiling looks like: the kernel's OOM killer takes the
largest process under the slice — one app's, which its watchdog
restarts — and `journalctl -k` names it; the CPU quota only slows.
