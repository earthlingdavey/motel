# The boxes' config, checked here and applied by the workflow on main.
# See README.md.
MOTELD ?= moteld
export MOTELD

.PHONY: check signer wip secret secrets secret-show rekey secrets-import

# Every box here is one motel will take, and a line for each. Needs a
# moteld binary: Linux only (the release tarball's, as
# .github/workflows/apply.yml installs it). On a Mac, the workflow runs
# it on every push and pull request.
check:
	bin/check

# Your SSH key into every box's signer list, and this checkout's commits
# signed with it:
#   make signer [KEY=~/.ssh/id_ed25519.pub] [PRINCIPAL=you@example.com]
signer:
	KEY="$(KEY)" PRINCIPAL="$(PRINCIPAL)" bin/signer

# A commit left unsigned, for config not yet meant to run: no box
# applies a history with an unsigned commit in it, whoever lands it on
# main (README.md, "The rules").
wip:
	git -c commit.gpgsign=false commit

# The apps' secrets, encrypted here with age to the box and its readers
# (README.md, Secrets; bin/secrets says more). Needs age.
#   make secret APP=example VAR=DATABASE_URL [BOX=deploy.example.com]
#     a box's first secret also takes BOX_KEY=$(ssh root@<your box> moteld box key)
#   make secrets [BOX=…]
#   make secret-show APP=example VAR=DATABASE_URL [IDENTITY=~/.ssh/id_ed25519]
#   make rekey [BOX=…] [BOX_KEY=…]
#   make secrets-import APP=example [BOX=…] < example.env
secret:
	APP="$(APP)" VAR="$(VAR)" BOX="$(BOX)" BOX_KEY="$(BOX_KEY)" bin/secrets set

secrets:
	BOX="$(BOX)" bin/secrets list

secret-show:
	APP="$(APP)" VAR="$(VAR)" BOX="$(BOX)" IDENTITY="$(IDENTITY)" bin/secrets show

rekey:
	BOX="$(BOX)" BOX_KEY="$(BOX_KEY)" IDENTITY="$(IDENTITY)" bin/secrets rekey

secrets-import:
	APP="$(APP)" BOX="$(BOX)" BOX_KEY="$(BOX_KEY)" bin/secrets import
