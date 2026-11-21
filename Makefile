# Everything runs in Docker — no host Go toolchain required.
# CI overrides COMPOSE to add docker-compose.ci.yml (bind-mounted caches).
COMPOSE ?= docker compose

# The workspace modules. NOTE: `./...` from the repo root matches only
# the root module — nested modules are excluded from a parent module's
# pattern even in workspace mode — so test/vet name each module's tree
# explicitly. These, RUN and the e2e lanes' splits and suites are set
# with ?=, so each is read from make's command line or the environment
# alike.
MODULES ?= . backups box client liveswap notify penaltybox
PKGS ?= ./... ./backups/... ./box/... ./client/... ./liveswap/... ./notify/... ./penaltybox/...

# One test by name: make test's `go test -run` pattern, as in
# make test PKGS=./penaltybox/... RUN=TestRetryAfterSeconds. Read with
# $(value), so a `$` in it (^TestX$/^sub$) reaches go test as typed,
# and its single quotes escaped for the shell.
RUN ?=

# MODULES, PKGS, RUN or an e2e lane's splits or suites left in the
# environment narrow a run as on the command line, but unseen, and a
# narrowed run passes: such a run ends by naming them. An empty
# MODULES or PKGS, or an empty list of splits or suites, is refused:
# it would run next to nothing.
# $(call narrowed_note,<the variables the target reads>).
env_narrowed = $(strip $(foreach v,$(1),$(if $(and $(filter environment,$(origin $(v))),$(value $(v))),$(v))))
narrowed_note = $(if $(call env_narrowed,$(1)),@echo "════ make $@: $(call env_narrowed,$(1)) from the environment: only what it names ran ════")

# Release version: the tag when present (release.yml passes it in),
# else a digit-leading dev placeholder that deb version rules accept.
VERSION ?= $(shell (git describe --tags --exact-match 2>/dev/null || echo v0.0.0-dev) | sed 's/^v//')

# Distro image for the package install smoke test (install-test).
DISTRO ?= debian:13

.PHONY: test help test-integration vet tidy tidy-check mod-download lint fuzz fuzz-list vulncheck pin-watch-check ci-changes-check secretscan usage usage-claude usage-check build build-client package clean-dist install-test e2e e2e-backup e2e-notify e2e-metrics e2e-warm soak e2e-logs clean

# -timeout 20m: a stopgap, client/internal/tui reaches go test's default 10 m on CI's x86 (#642).
test: ## unit tests, all modules (race + coverage); PKGS= some packages, RUN= a go test -run pattern
	$(if $(strip $(PKGS)),,$(error PKGS is empty: name packages, or leave it unset))
	$(COMPOSE) run --rm dev go test -race -cover -timeout 20m $(if $(value RUN),-run '$(subst ','\'',$(value RUN))') $(PKGS)
	$(call narrowed_note,PKGS RUN)

# The privileged systemd lanes (test-integration, e2e, install-test)
# need a cgroup-v2 Docker host: Docker Desktop, or Linux with systemd.
# Checked up front so a plain cgroup-v1 daemon fails with one clear
# line instead of an opaque systemd boot error.
define cgroup2_preflight
	@v=$$(docker info --format '{{.CgroupVersion}}' 2>/dev/null); \
	if [ "$$v" != "2" ]; then \
		echo "this target runs systemd inside a privileged container and needs a cgroup-v2 Docker host (got CgroupVersion=$${v:-unknown}): Docker Desktop, or Linux with systemd"; \
		exit 1; \
	fi
endef

# Runs inside the dev-systemd container (systemd as PID 1, root's user
# manager started by test/systemd/ready.sh): liveswap's runner creates
# real transient units, and the caddytest scenarios deploy a real app
# through them; the box applier asks the real systemctl. -p 1: the
# modules' caddytest suites (liveswap's and penaltybox's) all pin admin
# :2999 / http :9080, so their test binaries must not run in parallel;
# and backups' tests add users and run transient units one after
# another. Two groups, each -p 1, run at once: the caddytest pair, and
# backups, box and notify, which hold none of those ports, add no user
# liveswap reads, and reload only the system manager, not root's user
# manager, where liveswap's units run. Each group's output is held and
# printed after, a group at a time; go test's own -timeout (10 m a
# package) bounds a wedged one.
# -count=1: these tests hold what systemd, restic and sqlite3 do, and
# Go's test cache knows none of them — given the same test binary and
# flags it replays the last result, a pass included, after the program
# under it has changed [measured: a restic whose stuck-request default
# changed, its pin "(cached)" and passing]. A new Debian release is
# exactly such a change.
test-integration: ## real deploys through caddytest, under a real systemd user manager
	$(cgroup2_preflight)
	$(COMPOSE) up --build -d dev-systemd
	status=0; \
	$(COMPOSE) exec -T dev-systemd /bin/sh /src/test/systemd/ready.sh || status=1; \
	if [ $$status -eq 0 ]; then \
		$(COMPOSE) exec -T -e XDG_RUNTIME_DIR=/run/user/0 dev-systemd sh -c '\
			it="go test -count=1 -race -tags integration -v -run Integration -p 1"; \
			$$it ./liveswap/... ./penaltybox/... >/tmp/it-caddytest.log 2>&1 & a=$$!; \
			$$it ./backups/... ./box/... ./notify/... >/tmp/it-rest.log 2>&1 & b=$$!; \
			wait $$a; ra=$$?; wait $$b; rb=$$?; \
			echo "════ liveswap, penaltybox (the caddytest group) ════"; cat /tmp/it-caddytest.log; \
			echo "════ backups, box, notify ════"; cat /tmp/it-rest.log; \
			[ $$ra -eq 0 ] && [ $$rb -eq 0 ]' || status=1; \
	fi; \
	if [ $$status -ne 0 ]; then $(COMPOSE) exec -T dev-systemd journalctl --no-pager -n 100 || true; fi; \
	$(COMPOSE) rm -sf dev-systemd >/dev/null; \
	exit $$status

# Both tag sets: the integration-tagged files are not compiled by
# `make test` or a bare vet, so a signature change that breaks them
# stays invisible until the integration lane boots a whole systemd
# container to find out.
vet: ## go vet, both tag sets
	$(if $(strip $(PKGS)),,$(error PKGS is empty: name packages, or leave it unset))
	$(COMPOSE) run --rm dev go vet $(PKGS)
	$(COMPOSE) run --rm dev go vet -tags integration $(PKGS)
	$(call narrowed_note,PKGS)

tidy: ## go mod tidy in each of MODULES
	$(if $(strip $(MODULES)),,$(error MODULES is empty: name a module, or leave it unset))
	for m in $(MODULES); do $(COMPOSE) run --rm -w /src/$$m dev go mod tidy || exit 1; done
	$(call narrowed_note,MODULES)

# The PR-CI half of tidy: `go mod tidy -diff` writes nothing, prints
# the diff tidy would make and exits non-zero when there is one, so a
# missing go.sum line or a leftover require fails the PR that left it.
# It checks each module's go.mod/go.sum only: go.work is ignored and
# go.work.sum is not checked (that would be a separate check).
# Each module's output starts with its name (the diff says only
# current/go.mod), and a module that fails does not stop the others:
# CI checks every module in one job, and the run ends naming each that
# failed. Only exit 1 (the diff, a go error, or compose's own failure,
# such as an image it cannot pull: each module then fails in turn,
# each naming the cause) goes on to the next; any other status, an
# interrupt among them, stops the run there. The trap is for Ctrl-C
# under a shell that, like bash, would go on to the next module when
# the one it waits on handles the interrupt itself. The loop is not
# echoed: its headings say what runs, and an echoed copy would put
# "failed in:" in every log, green ones too.
tidy-check: ## fails if go mod tidy would change a module's go.mod/go.sum
	$(if $(strip $(MODULES)),,$(error MODULES is empty: name a module, or leave it unset))
	@failed=; trap 'exit 130' INT; \
	for m in $(MODULES); do \
		echo "════ tidy-check $$m ════"; \
		$(COMPOSE) run --rm -T -w /src/$$m dev go mod tidy -diff; rc=$$?; \
		case $$rc in 0) ;; 1) failed="$$failed $$m" ;; *) exit $$rc ;; esac; \
	done; \
	[ -z "$$failed" ] || { echo "════ make $@ failed in:$$failed ════"; exit 1; }
	$(call narrowed_note,MODULES)

# Every module's dependencies into Go's module cache, before a build
# needs them: a download from proxy.golang.org that breaks mid-stream
# is tried again here (three tries, 10 and 20 s apart), where vet,
# tidy-check or a lane's build failed on it [measured: seven CI jobs
# in the fortnight to 2026-11-21, and nothing tried a download again].
# CI runs it in every job that runs Go, right after the module cache
# is restored (.github/actions/go-cache says what is kept); on a
# machine whose cache is a volume that stays, nothing needs it.
#
# In each module with the workspace off, never once at the root: there
# `go mod download` takes every module of the whole graph, most of
# which no build reads, and rewrites go.work.sum [measured on a
# restored cache, run 44935635314: 8 to 13 s and about 4,500 requests
# to the proxy at the root; 0.5 to 0.9 s and 36 requests for all
# seven, a module at a time]. On a copy of go.mod and go.sum
# (-modfile), because the download writes into go.sum a checksum line
# it lacks, and tidy-check, which CI runs after this, must find the
# file as it was committed [measured 2026-11-21, Go 1.27.4: of a
# module's two lines dropped from penaltybox/go.sum, one was put
# back].
#
# Then the workspace's packages are loaded, once for each architecture
# CI runs on. With the workspace on, Go opens modules that no go.mod
# in the tree names, to rule out that a second module provides an
# imported package; the download above leaves them out, and the first
# build would fetch them [measured 2026-11-21 on an empty cache, Go
# 1.27.4: 191 modules, 1,084 MB, from the downloads; 7 more, 58 MB,
# from this load, the same 7 for amd64, arm64 and darwin and under
# the integration and e2e tags; after it go vet under both tag sets,
# the client's build and govulncheck's own build fetched nothing]. Loaded
# with -e and not tried again: a package of the tree that does not
# load is vet's to report, and here it would read as a download that
# failed; so a download that breaks in these few is left to the
# command that next needs the module, as before. If the load has to
# add a line to go.work.sum, it does, as the build after it would.
#
# Not here: what `go mod tidy` reads beyond all this, the tests of
# dependencies [55 modules, 67 MB, the same measurement]. tidy-check
# fetches those, and in CI its job is the one that saves the module
# cache, after it.
mod-download: ## each of MODULES' dependencies into Go's module cache; a download that breaks is tried three times
	$(if $(strip $(MODULES)),,$(error MODULES is empty: name a module, or leave it unset))
	@$(COMPOSE) run --rm -T dev sh -c '\
		t0=$$(date +%s); tmp=$$(mktemp -d) || exit 1; pkgs=; \
		for m in $(MODULES); do \
			pkgs="$$pkgs ./$$m/..."; \
			cp "$$m/go.mod" "$$m/go.sum" "$$tmp/" || exit 1; \
			n=1; \
			until GOWORK=off go mod download -C "$$m" -modfile="$$tmp/go.mod"; do \
				if [ $$n -ge 3 ]; then echo "════ make $@: go mod download failed in $$m, $$n times ════" >&2; exit 1; fi; \
				echo "════ make $@: go mod download failed in $$m (try $$n of 3); again in $$((n * 10)) s ════" >&2; \
				sleep $$((n * 10)); n=$$((n + 1)); \
			done; \
		done; \
		for arch in amd64 arm64; do GOARCH=$$arch go list -e -deps -test $$pkgs >/dev/null; done; \
		echo "════ make $@: $(MODULES), in $$(($$(date +%s) - t0)) s ════"'
	$(call narrowed_note,MODULES)

# Both tag sets, as for vet: a bare run never compiles the
# integration-tagged files, so neither the linters nor gofmt see them.
# A single `run.build-tags: [integration]` in .golangci.yml would not
# do: it drops the `!integration` goleak TestMain files instead.
# As tidy-check, each run is named, the loop is not echoed, and the
# others still run after one exits 1 (golangci-lint's issues, or
# compose's own failure); the end names each that failed, and any
# other status (the linter's own failure, an interrupt) stops the run
# there.
lint: ## golangci-lint (gofmt-gated), both tag sets; MODULES= the modules you touched
	$(if $(strip $(MODULES)),,$(error MODULES is empty: name a module, or leave it unset))
	@failed=; trap 'exit 130' INT; \
	for m in $(MODULES); do \
		for tags in "" integration; do \
			echo "════ lint $$m$${tags:+, $$tags tags} ════"; \
			$(COMPOSE) run --rm -w /src/$$m lint golangci-lint run $${tags:+--build-tags $$tags}; rc=$$?; \
			case $$rc in 0) ;; 1) failed="$$failed $$m$${tags:+($$tags)}" ;; *) exit $$rc ;; esac; \
		done; \
	done; \
	[ -z "$$failed" ] || { echo "════ make $@ failed in:$$failed ════"; exit 1; }
	$(call narrowed_note,MODULES)

# Real fuzzing of the untrusted-input surfaces (seed corpora already
# run inside `make test`). One target at a time — Go allows a single
# -fuzz pattern per invocation. The corpus accumulates in the
# gobuildcache volume across runs.
# FUZZTIME (default 2m) is read by the shell, never declared as a make
# variable: fuzz.yml passes a workflow_dispatch input through the
# environment, and a make variable would expand $(shell ...) in it.
FUZZ_MODULES = box client liveswap penaltybox

# Fuzz targets are discovered (`go test -list '^Fuzz'` in every package
# of the module — box keeps its proof core in a subpackage), never
# listed by hand, so a new target cannot be left out of the weekly run.
# fuzz-list is the deterministic half, run in every PR: it prints what
# fuzz will run, fails if a module has no targets, and fails if a
# committed testdata/fuzz/<Target> corpus (a crasher kept as a
# regression input) no longer has a target in its package to replay it
# — dead corpora test nothing. The seed corpora themselves run in
# `make test`.
fuzz-list: ## what fuzz will run, discovered per module
	@for m in $(FUZZ_MODULES); do \
		found=0; \
		pkgs=$$($(COMPOSE) run --rm -T -w /src/$$m dev go list -f '{{.Dir}}' ./...) || { echo "$$pkgs"; echo "go list failed in $$m"; exit 1; }; \
		for p in $$pkgs; do \
			rel=$${p#/src/}; \
			listed=$$($(COMPOSE) run --rm -T -w $$p dev go test -list '^Fuzz' .) || { echo "$$listed"; echo "go test -list failed in $$rel"; exit 1; }; \
			targets=$$(echo "$$listed" | grep '^Fuzz'); \
			for d in $$rel/testdata/fuzz/*/; do \
				[ -d "$$d" ] || continue; \
				n=$$(basename "$$d"); \
				echo "$$targets" | grep -qx "$$n" || { echo "$$d has no matching Fuzz target"; exit 1; }; \
			done; \
			[ -n "$$targets" ] || continue; \
			found=1; \
			for t in $$targets; do echo "$$rel $$t"; done; \
		done; \
		[ $$found -eq 1 ] || { echo "no fuzz targets found in $$m"; exit 1; }; \
	done

fuzz: ## fuzz the untrusted-input surfaces (FUZZTIME=2m per target, from the environment)
	for m in $(FUZZ_MODULES); do \
		pkgs=$$($(COMPOSE) run --rm -T -w /src/$$m dev go list -f '{{.Dir}}' ./...) || { echo "$$pkgs"; echo "go list failed in $$m"; exit 1; }; \
		for p in $$pkgs; do \
			listed=$$($(COMPOSE) run --rm -T -w $$p dev go test -list '^Fuzz' .) || { echo "$$listed"; echo "go test -list failed in $${p#/src/}"; exit 1; }; \
			targets=$$(echo "$$listed" | grep '^Fuzz'); \
			for t in $$targets; do \
				$(COMPOSE) run --rm -w $$p dev \
					go test -run '^$$' -fuzz "^$$t$$" -fuzztime "$${FUZZTIME:-2m}" . || exit 1; \
			done; \
		done; \
	done

# Known-vulnerability scan per module. govulncheck is a `tool`
# dependency in each module's go.mod — never compiled into the product,
# invisible to importers — so Dependabot bumps it like any require. A
# version literal here (`go run ...@vX`) would be a pin nothing
# watches: no ecosystem parses shell strings. The vuln database itself
# always updates regardless of tool version, which is why vulncheck.yml
# also runs this on a weekly cron. Accepted (unreachable, not yet
# fixable) advisories are alert_dismissals in .github/pin-watch.yml.
vulncheck: ## govulncheck, each of MODULES
	$(if $(strip $(MODULES)),,$(error MODULES is empty: name a module, or leave it unset))
	$(COMPOSE) run --rm dev sh .github/scripts/vulncheck.sh $(MODULES)
	$(call narrowed_note,MODULES)

# The manifest rules of pin-watch.py (.github/scripts/pin_watch_test.py),
# in the same python image as usage-check, which depends on it so CI's
# usage-check job runs both; the test stubs PyYAML, which the image
# lacks and the rules never read.
pin-watch-check: ## pin-watch.py's manifest rules
	$(COMPOSE) run --rm -T --user "$$(id -u):$$(id -g)" usage python3 -I -B .github/scripts/pin_watch_test.py

# The rule ci.yml's changes job skips the build-and-test jobs by
# (.github/scripts/ci_changes_test.py): in the same image and lane as
# pin-watch-check, since usage-check runs on every PR, skipped or not.
ci-changes-check: ## the rule by which CI skips its build-and-test jobs on a PR of markdown or the usage ledger alone
	$(COMPOSE) run --rm -T --user "$$(id -u):$$(id -g)" usage python3 -I -B .github/scripts/ci_changes_test.py

# Full-history secret scan — the same engine and image as CI's gitleaks
# gate (the image is pinned in docker-compose.yml, where Dependabot
# watches it), so local and CI can never drift.
#
# Inside the container the history is readable only when .git is a
# directory: a git worktree's .git is a file naming a gitdir outside
# the /src mount, and there gitleaks logs "not a git repository",
# scans 0 commits and exits 0 with "no leaks found" — a false pass. So
# a worktree gets --no-git and the scan covers the working tree
# instead: every file on disk (ignored build output included), not
# the branch's commits — those CI's git-mode run covers. A plain
# checkout (and CI's) is unchanged.
secretscan: GITLEAKS_FLAGS = $(shell test -d .git || echo --no-git)
secretscan: ## gitleaks: full history, or the working tree in a git worktree
	$(COMPOSE) run --rm gitleaks detect --source /src --redact -v $(GITLEAKS_FLAGS)

# motel's release, linked in as Caddy's CustomVersion. Caddy puts it
# in front of its full version (`moteld version`, `--version`,
# `environ`), which otherwise names only the Caddy built in. The short
# version, in Caddy's ACME User-Agent, metrics, tracing and FastCGI
# SERVER_SOFTWARE, stays Caddy's own.
GO_LDFLAGS = -s -w -X github.com/caddyserver/caddy/v2.CustomVersion=v$(VERSION)

# The public ledger of what building motel costs: Claude Code
# sessions, Actions runner minutes and Copilot credits as month-level
# facts in usage/ (CSVs), rendered to docs/footprint.md. Reads the
# transcripts under ~/.claude/projects (the compose service mounts them
# read-only) and the Actions API with your gh login — the token gh
# hands over carries the login's full scope, though public run data
# needs only its rate-limit identity. Run it monthly — GitHub keeps run
# records for 90 days — fill in usage/copilot.csv from GitHub billing
# (its note column is public too), and commit what changed. --user: the
# files it writes stay yours on a Linux host (the container is root).
# The sessions are spread over several machines and each sees only its
# own transcripts, so every machine writes its own file,
# usage/claude/$(USAGE_MACHINE).csv, and the page sums them. Name each
# machine once, on the host: `git config --global motel.usage-machine
# <label>` (a public label: the file name; per clone, without --global,
# if your ~/.gitconfig is synced between machines). USAGE_MATCH: project-
# directory names to count besides motel, comma-separated, for a
# checkout that kept an older name: `git config motel.usage-match
# <names>` (per checkout, or --global). usage-claude is the transcript
# half alone, for the other machines: no token, so no Actions API
# budget spent. Commit the machine's file with the page; when two
# machines' commits conflict in the page, take either side and run
# usage-check: it re-renders the page and fails until the re-rendered
# page is staged.
USAGE_REPO ?= smallhoursorg/motel
USAGE_MATCH ?= $(shell git config motel.usage-match)
USAGE_MACHINE ?= $(shell git config motel.usage-machine)
# Through the environment, not spliced into the recipe: a value with a
# quote or a `$` reaches the script as typed. --match= so a value that
# starts with a hyphen is a value, not an option.
export USAGE_MATCH USAGE_MACHINE
USAGE_FLAGS = --match="$$USAGE_MATCH" --machine="$$USAGE_MACHINE"
usage: ## what building this costs, into usage/ and docs/footprint.md; needs gh auth login
	GH_TOKEN=$$(gh auth token) $(COMPOSE) run --rm --user "$$(id -u):$$(id -g)" usage \
		python3 -I .github/scripts/usage.py collect-claude collect-actions render --repo $(USAGE_REPO) $(USAGE_FLAGS)
usage-claude: ## the Claude Code half of usage alone, no token
	$(COMPOSE) run --rm --user "$$(id -u):$$(id -g)" usage \
		python3 -I .github/scripts/usage.py collect-claude render $(USAGE_FLAGS)

# The PR-CI half of usage, like tidy-check: runs the merge rule's tests,
# then re-renders docs/footprint.md from the committed CSVs (no
# transcripts, no token) and fails when the page no longer matches them.
# It is also the only lane that runs the python image, so Dependabot's
# bumps of that pin are exercised.
usage-check: pin-watch-check ci-changes-check ## usage.py's tests, then fails if docs/footprint.md no longer matches usage/
	$(COMPOSE) run --rm -T --user "$$(id -u):$$(id -g)" usage python3 -I -B .github/scripts/usage_test.py
	$(COMPOSE) run --rm -T --user "$$(id -u):$$(id -g)" usage python3 -I .github/scripts/usage.py render
	git diff --exit-code -- docs/footprint.md

# Cross-compiles the product binary for the release targets. GOFLAGS is
# cleared so -buildvcs is back on: Go records the commit, and on a tag
# the release, in the build info `moteld build-info` prints (the dev
# service disables it for test cache friendliness). safe.directory is
# needed because /src is a bind mount owned by a different uid than the
# container's root.
# The trailing chmod is for Linux CI runners: the container runs as
# root, so without it the bind-mounted build/ is unwritable to the
# host user and package's cp/rm staging fails (macOS masks this). Of
# build/ itself and the four binaries, not a walk: build/ also holds
# the e2e lanes' directories, which a lane empties as it starts
# (e2e/bin.sh).
# The two arch builds run concurrently: cache-warm, wall time is
# mostly the two large links, and those overlap cleanly.
build: ## cross-compile moteld and motel-backup for linux amd64/arm64 into build/
	$(COMPOSE) run --rm -e GOFLAGS= -e CGO_ENABLED=0 dev sh -c '\
		git config --global --add safe.directory /src; \
		GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(GO_LDFLAGS)" -o build/moteld-linux-amd64 ./cmd/moteld & p1=$$!; \
		GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(GO_LDFLAGS)" -o build/moteld-linux-arm64 ./cmd/moteld & p2=$$!; \
		(cd backups && GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o ../build/motel-backup-linux-amd64 ./cmd/motel-backup) & p3=$$!; \
		(cd backups && GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o ../build/motel-backup-linux-arm64 ./cmd/motel-backup) & p4=$$!; \
		wait $$p1 || exit 1; wait $$p2 || exit 1; wait $$p3 || exit 1; wait $$p4 || exit 1; \
		if go version -m build/motel-backup-linux-amd64 build/motel-backup-linux-arm64 | grep -q -- "-tags=.*e2e"; then \
			echo "build/motel-backup-linux-*: built with -tags e2e, which shortens the setup clock for the e2e box: not a binary to ship"; exit 1; \
		fi; \
		chmod a+rwx build; chmod a+rwX build/moteld-linux-* build/motel-backup-linux-*'

# Builds the laptop client as a release must: in client/, with the
# workspace off (GOWORK=off), so the binary links exactly the versions
# client/go.mod records (TestWorkspaceAgrees holds the two builds
# equal), CGO off, -trimpath, and the release stamped where `motel
# version` reads it. It runs in the dev container, so by default it
# builds for Linux on the container's architecture; GOOS= and GOARCH= on
# the command line cross-build: make build-client GOOS=darwin GOARCH=arm64.
# package builds each of CLIENT_TARGETS this way, for the release.
# GOFLAGS is cleared as for build, so Go records the commit too.
build-client: ## the laptop client into build/; GOOS= GOARCH= cross-build
	$(COMPOSE) run --rm -e GOFLAGS= -e CGO_ENABLED=0 -e GOWORK=off $(if $(GOOS),-e GOOS=$(GOOS)) $(if $(GOARCH),-e GOARCH=$(GOARCH)) dev sh -c '\
		git config --global --add safe.directory /src; \
		os=$$(go env GOOS); arch=$$(go env GOARCH); \
		cd client && go build -trimpath -ldflags "-s -w -X main.version=v$(VERSION)" -o ../build/motel-$$os-$$arch ./cmd/motel && \
		chmod a+rwx ../build && chmod a+rwX ../build/motel-$$os-$$arch && echo build/motel-$$os-$$arch'

# The laptop client's release targets, each GOOS/GOARCH. package builds
# each with build-client and packs it as
# dist/motel_$(VERSION)_<os>_<arch>.tar.gz, with `motel` (0755) and
# LICENSE (0644) at the archive's root; the release checksums and
# attaches everything in dist/. A target is one more word here. The
# linux archives have the name the server's own archive had up to
# v0.7.0: a box repository made from that release's template, unless
# its repository variable pins an earlier release, installs this client
# as its checker from the latest release, and its workflow fails at the
# client's refusal of a server command (docs/upgrading.md). So does a
# raw-binary host's own script that still fetches that name (README.md,
# "Without the package").
CLIENT_TARGETS = darwin/arm64 linux/amd64 linux/arm64

# Builds .deb for both arches into dist/, then the client's archives
# (CLIENT_TARGETS above). .deb only, and no .apk
# planned: liveswap runs apps as transient systemd units with no
# fallback runner, so an Alpine/OpenRC package would install a moteld
# that serves but refuses any Caddyfile defining an app — see
# packaging/nfpm.yaml. The packages carry the systemd unit, the starter
# config (in /usr/share/motel) and the data dirs, and motel-backup
# with its two timers; postinstall creates the motel and
# motel-backup system users and installs the starter at
# /etc/motel/Caddyfile. Both arches are staged and packaged inside
# ONE nfpm container: staged on the host between two containers, a
# binary replaced in place kept its old size in Docker Desktop's
# bind-mount attribute cache, and nfpm wrote a tar header of the amd64
# size over the arm64 content ("missed writing 286720 bytes").
#
# clean-dist runs before build, so a run that fails anywhere leaves no
# .deb behind for install-test to certify in place of this one, and no
# client archive of an earlier run for a release to attach.
#
# The client's archives are packed in the dev container, not by the
# host's tar: GNU tar on every host, so a Mac's tar adds none of
# macOS's own metadata to them.
package: clean-dist build ## the .debs and the client's archives into dist/
	mkdir -p dist
	$(COMPOSE) run --rm --entrypoint sh -e NFPM_VERSION=$(VERSION) nfpm -c '\
		set -e; \
		for a in amd64 arm64; do \
			rm -f build/moteld build/motel-backup; \
			cp build/moteld-linux-$$a build/moteld; \
			cp build/motel-backup-linux-$$a build/motel-backup; \
			for f in deb; do \
				NFPM_ARCH=$$a nfpm package -f packaging/nfpm.yaml -p $$f -t dist/; \
			done; \
		done; \
		rm -f build/moteld build/motel-backup'
	for t in $(CLIENT_TARGETS); do \
		$(MAKE) --no-print-directory build-client GOOS=$${t%/*} GOARCH=$${t#*/} VERSION=$(VERSION) || exit 1; \
	done
	$(COMPOSE) run --rm dev sh -c '\
		set -e; \
		for t in $(CLIENT_TARGETS); do \
			os=$${t%/*}; arch=$${t#*/}; s=$$(mktemp -d); \
			cp build/motel-$$os-$$arch $$s/motel; cp LICENSE $$s/LICENSE; \
			chmod 0755 $$s/motel; chmod 0644 $$s/LICENSE; \
			tar -czf dist/motel_$(VERSION)_$${os}_$${arch}.tar.gz -C $$s motel LICENSE; \
			rm -rf $$s; \
		done'

clean-dist: ## remove dist/'s packages and archives
	rm -f dist/moteld_*.deb dist/motel_*.tar.gz

# Installs the freshly built .deb inside a systemd container (DISTRO
# picks the base image) and runs the staged smoke test: install, unit
# boot, a real liveswap deploy under the sandbox, the "Set it up" lines
# of docs/backups.md as an administrator (the page is mounted: the lines
# run are the lines written), reinstall, remove, purge.
# Needs dist/ populated first (make package). --privileged +
# --cgroupns=host with the cgroup mount is the reliable
# systemd-in-docker recipe on cgroup-v2 hosts (GitHub runners and
# Docker Desktop alike).
install-test: ## install dist/'s .deb under real systemd (DISTRO=debian:13); run package first
	$(cgroup2_preflight)
	docker build -t motel-install-test-$(subst :,-,$(DISTRO)) \
		--build-arg BASE_IMAGE=$(DISTRO) packaging/test
	docker rm -f motel-smoke >/dev/null 2>&1 || true
	docker run -d --name motel-smoke --privileged --cgroupns=host \
		--tmpfs /run --tmpfs /run/lock \
		-v /sys/fs/cgroup:/sys/fs/cgroup:rw \
		-v $(CURDIR)/dist:/dist:ro \
		-v $(CURDIR)/packaging/test/smoke.sh:/smoke.sh:ro \
		-v $(CURDIR)/liveswap/testdata/sandbox-view.sh:/sandbox-view.sh:ro \
		-v $(CURDIR)/e2e/backup/tty.sh:/tty.sh:ro \
		-v $(CURDIR)/docs/backups.md:/docs-backups.md:ro \
		motel-install-test-$(subst :,-,$(DISTRO))
	docker exec motel-smoke /bin/bash /smoke.sh; status=$$?; \
	if [ $$status -ne 0 ]; then \
		docker exec motel-smoke journalctl -u moteld --no-pager || true; \
	fi; \
	docker rm -f motel-smoke >/dev/null; \
	exit $$status

# The Go the e2e, e2e-notify and e2e-metrics images take (and soak's,
# the e2e stack's), built in the dev container right before each builds
# its images, as e2e-backup builds its box's: there Go's caches are the
# volumes `make build` and `make test` fill (in CI, ./.cache), where a
# build inside the image starts cold on any builder that kept no cache,
# CI's always. e2e/bin.sh says what it builds; build/<lane> is the
# images' `bin` context, which the target names to compose with
# E2E_LANE. After the cgroup check, so a host that cannot run the lane
# is told so before it compiles anything.
# $(call e2e_bin,<lane>[ notify-tests])
e2e_bin = $(COMPOSE) run --rm dev sh e2e/bin.sh $(1)

# The main suites run via the e2e-runner entrypoint. Then the systemd
# suite runs INSIDE the motel container (it needs systemctl,
# journalctl and the process tree): restart survival, moteld's own
# crashes (SIGKILL, exit 2, OOM) undone by the unit + reattach, a
# refused start staying down, cgroup teardown of a worker tree, crash
# cleanup, journal output. The recovery suite is the runner's view
# after all that: still serving, deploys still work. Last, the box
# apply suite runs inside the container: signed commits and secrets
# pushed through box_webhook to the root applier, with the box
# template's own commands, and the e2e config put back. Then the people
# suite, inside it too: people declared in a box's file read it with
# the client over the box's own TLS (e2e/box/people.sh says what it
# holds).
e2e: ## the liveswap, penaltybox, systemd, recovery, box and people suites against the shipped binary
	$(cgroup2_preflight)
	$(call e2e_bin,e2e)
	E2E_LANE=e2e $(COMPOSE) up --build -d e2e-motel e2e-upstream e2e-artifacts
	status=0; \
	$(COMPOSE) run --rm e2e-runner || status=1; \
	echo "════ systemd suite: restart survival, reattach, cgroup teardown ════"; \
	$(COMPOSE) exec -T e2e-motel /bin/sh /suite-systemd.sh || status=1; \
	echo "════ recovery suite: the runner's view after moteld's unclean death ════"; \
	$(COMPOSE) run --rm --entrypoint "/bin/sh /suite-recovery.sh" e2e-runner || status=1; \
	echo "════ box apply suite: signed pushes through box_webhook to the root applier ════"; \
	$(COMPOSE) exec -T e2e-motel /bin/sh /suite-box.sh || status=1; \
	echo "════ people suite: the client over the box's TLS, as a laptop reads it ════"; \
	$(COMPOSE) exec -T e2e-motel /bin/sh /suite-people.sh || status=1; \
	if [ $$status -ne 0 ]; then \
		$(COMPOSE) logs e2e-upstream e2e-artifacts; \
		$(COMPOSE) exec -T e2e-motel journalctl --no-pager -n 300 || true; \
	fi; \
	$(COMPOSE) down --remove-orphans; \
	exit $$status

# The notify suite: a box with Debian's Alertmanager and a stub receiver
# (e2e/notify/run.sh). Its own target, as the backup suite is: its waits
# are Alertmanager's and motel's timers, which no other lane should pay.
# Its splits (run.sh's SPLIT) run at once, a box each, sharing the sink,
# which answers each box with its own deliveries; their output is held
# and printed a split at a time, each ended at NOTIFY_LIMIT inside its
# box. The heartbeat split, five minutes of motel's hold and its resume
# (~9.5 min), is the lane's length: ci.yml names it with the other
# two, and a bare run leaves it out, and says so at its end.
# NOTIFY_SPLITS=heartbeat runs it alone; =all the whole suite on one
# box. The sink is metrics' too, its paths the same, so e2e-notify and
# e2e-metrics do not run at once on one host.
NOTIFY_SPLITS ?= core reload
NOTIFY_LIMIT ?= 1800
e2e-notify: ## the notify suite; NOTIFY_SPLITS= core reload (the default), heartbeat, or all
	$(cgroup2_preflight)
	$(if $(strip $(NOTIFY_SPLITS)),,$(error NOTIFY_SPLITS is empty: name a split, or all))
	$(call e2e_bin,e2e-notify)
	$(COMPOSE) rm -sf e2e-notify-box e2e-sink >/dev/null
	E2E_LANE=e2e-notify $(COMPOSE) up --build -d --scale e2e-notify-box=$(words $(NOTIFY_SPLITS)) e2e-notify-box e2e-sink e2e-artifacts
	logs=$$(mktemp -d); i=0; \
	for s in $(NOTIFY_SPLITS); do \
		i=$$((i + 1)); \
		( t0=$$(date +%s); $(COMPOSE) exec -T --index $$i -e SPLIT=$$s e2e-notify-box timeout $(NOTIFY_LIMIT) /bin/sh /suite-notify.sh >$$logs/$$i.log 2>&1; echo "$$? $$(($$(date +%s) - t0))" >$$logs/$$i.rc ) & \
	done; \
	wait; \
	status=0; i=0; \
	for s in $(NOTIFY_SPLITS); do \
		i=$$((i + 1)); \
		rc=; took=; read rc took <$$logs/$$i.rc || rc=unknown; \
		echo "════ notify split $$s (box $$i, $${took:-?}s) ════"; \
		cat $$logs/$$i.log; \
		if [ "$$rc" = 124 ]; then echo "════ notify split $$s ended at NOTIFY_LIMIT ($(NOTIFY_LIMIT)s) ════"; fi; \
		if [ "$$rc" != 0 ]; then \
			status=1; \
			echo "════ notify split $$s failed: box $$i's journal ════"; \
			$(COMPOSE) exec -T --index $$i e2e-notify-box journalctl --no-pager -n 200 -u moteld -u prometheus-alertmanager -u motel-am.socket || true; \
		fi; \
	done; \
	rm -rf $$logs; \
	$(COMPOSE) rm -sf e2e-notify-box e2e-sink >/dev/null; \
	$(if $(filter heartbeat all,$(NOTIFY_SPLITS)),,echo "════ heartbeat split not run (notify 15, motel's 5-minute hold, ~9.5 min): CI runs it; NOTIFY_SPLITS=heartbeat runs it here ════";) \
	exit $$status
	$(call narrowed_note,NOTIFY_SPLITS)

# The metrics suite: notify's box with Debian's victoria-metrics, and the
# same stub receiver; its own target and CI job, as notify's. Its parts
# are timer-bound, not CPU-bound (a minute's evaluation, a 75 s hold), so
# its splits (e2e/metrics/run.sh's SPLIT) run at once, a box each, as the
# backup suites do, with their output held and printed a split at a time.
# METRICS_SPLITS=rules runs one; METRICS_SPLITS=all is the whole suite on
# one box, in the order it ran in before the split. Each split is ended
# at METRICS_LIMIT inside its box, as a backup suite at SUITE_LIMIT, so a
# wedged one still prints what it got to. The boxes and the sink are
# removed first: a box count, or sink lines, left by an interrupted run.
# The sink is notify's too, its paths the same, so e2e-notify and
# e2e-metrics do not run at once on one host (a compose project a lane
# would let them).
METRICS_SPLITS ?= view vmalert store rules
METRICS_LIMIT ?= 900
e2e-metrics: ## the metrics suite; METRICS_SPLITS= view vmalert store rules (the default), or all
	$(cgroup2_preflight)
	$(if $(strip $(METRICS_SPLITS)),,$(error METRICS_SPLITS is empty: name a split, or all))
	$(call e2e_bin,e2e-metrics notify-tests)
	$(COMPOSE) rm -sf e2e-metrics-box e2e-sink >/dev/null
	E2E_LANE=e2e-metrics $(COMPOSE) up --build -d --scale e2e-metrics-box=$(words $(METRICS_SPLITS)) e2e-metrics-box e2e-sink e2e-artifacts
	logs=$$(mktemp -d); i=0; \
	for s in $(METRICS_SPLITS); do \
		i=$$((i + 1)); \
		( t0=$$(date +%s); $(COMPOSE) exec -T --index $$i -e SPLIT=$$s e2e-metrics-box timeout $(METRICS_LIMIT) /bin/sh /suite-metrics.sh >$$logs/$$i.log 2>&1; echo "$$? $$(($$(date +%s) - t0))" >$$logs/$$i.rc ) & \
	done; \
	wait; \
	status=0; i=0; \
	for s in $(METRICS_SPLITS); do \
		i=$$((i + 1)); \
		rc=; took=; read rc took <$$logs/$$i.rc || rc=unknown; \
		echo "════ metrics split $$s (box $$i, $${took:-?}s) ════"; \
		cat $$logs/$$i.log; \
		if [ "$$rc" = 124 ]; then echo "════ metrics split $$s ended at METRICS_LIMIT ($(METRICS_LIMIT)s) ════"; fi; \
		if [ "$$rc" != 0 ]; then \
			status=1; \
			echo "════ metrics split $$s failed: box $$i's journal ════"; \
			$(COMPOSE) exec -T --index $$i e2e-metrics-box journalctl --no-pager -n 200 -u moteld -u victoria-metrics -u motel-vmalert -u motel-vm.socket -u motel-vm.service -u motel-am-metrics.socket -u prometheus-alertmanager -u motel-metrics-netns || true; \
		fi; \
	done; \
	rm -rf $$logs; \
	$(COMPOSE) rm -sf e2e-metrics-box e2e-sink >/dev/null; \
	exit $$status
	$(call narrowed_note,METRICS_SPLITS)

# The backup suite: a box with systemd, Debian's restic and sqlite3, and
# an S3 server for the repository. Its own target and its own CI job —
# it shares nothing with the e2e stack, so run beside it, it costs its
# own length and not the e2e job's. It takes down only its own two
# services, so it can run beside `make e2e` on one host.
#
# Each suite has a box of its own, and they run at once: each seeds its
# own apps and writes its own credential file for a repository of its
# own on the one S3 server, so none depends on another's leftovers.
# Their output is held and printed a suite at a time, in order, with
# how long each took. Each suite is ended at SUITE_LIMIT, inside its
# box, so that a wedged one — nothing in a backup has a time limit —
# still prints what it got to, and its box's journal, well before the
# CI job's own limit ends everything with nothing printed.
#
# The box's two binaries are built first, in the dev container, for
# this host's arch: its Go caches are the volumes `make build` uses
# (in CI, ./.cache under actions/cache), where a build inside the
# image started cold every time. motel-backup is built with -tags
# e2e, which shortens setup's clock alone (backups/engine/
# setupclock_e2e.go): the package's binary is never built with it,
# and `make build` refuses one that was. build/ and the lane's
# directory are made writable to the host's user (the container is
# root), and nothing below them is touched: the binaries stay 0755,
# as the package installs them, and no walk of build/ meets another
# lane's directory as it is emptied (e2e/bin.sh).
BACKUP_SUITES ?= setup backup units status restore check
SUITE_LIMIT ?= 600
e2e-backup: ## the backup suites, a box each; BACKUP_SUITES= setup backup units status restore check (the default)
	$(cgroup2_preflight)
	$(if $(strip $(BACKUP_SUITES)),,$(error BACKUP_SUITES is empty: name a suite, or leave it unset))
	$(COMPOSE) run --rm -e CGO_ENABLED=0 dev sh -c '\
		mkdir -p build/e2e-backup && chmod a+rwx build build/e2e-backup || exit 1; \
		go build -trimpath -o build/e2e-backup/moteld ./cmd/moteld & p1=$$!; \
		(cd backups && go build -trimpath -tags e2e -o ../build/e2e-backup/motel-backup ./cmd/motel-backup) & p2=$$!; \
		wait $$p1 || exit 1; wait $$p2 || exit 1'
	$(COMPOSE) rm -sf e2e-backup-box e2e-s3 >/dev/null
	$(COMPOSE) up --build -d --scale e2e-backup-box=$(words $(BACKUP_SUITES)) e2e-s3 e2e-backup-box
	logs=$$(mktemp -d); i=0; \
	for s in $(BACKUP_SUITES); do \
		i=$$((i + 1)); \
		( t0=$$(date +%s); $(COMPOSE) exec -T --index $$i e2e-backup-box timeout $(SUITE_LIMIT) /bin/sh /suite-$$s.sh >$$logs/$$i.log 2>&1; echo "$$? $$(($$(date +%s) - t0))" >$$logs/$$i.rc ) & \
	done; \
	wait; \
	status=0; i=0; \
	for s in $(BACKUP_SUITES); do \
		i=$$((i + 1)); \
		rc=; took=; read rc took <$$logs/$$i.rc || rc=unknown; \
		echo "════ $$s suite (box $$i, $${took:-?}s) ════"; \
		cat $$logs/$$i.log; \
		if [ "$$rc" = 124 ]; then echo "════ $$s suite ended at SUITE_LIMIT ($(SUITE_LIMIT)s) ════"; fi; \
		if [ "$$rc" != 0 ]; then \
			status=1; \
			echo "════ $$s suite failed: box $$i's journal ════"; \
			$(COMPOSE) exec -T --index $$i e2e-backup-box journalctl --no-pager -n 200 || true; \
		fi; \
	done; \
	if [ $$status -ne 0 ]; then $(COMPOSE) logs --tail 300 e2e-s3; fi; \
	rm -rf $$logs; \
	$(COMPOSE) rm -sf e2e-backup-box e2e-s3 >/dev/null; \
	exit $$status
	$(call narrowed_note,BACKUP_SUITES)

# Every e2e lane's Go, built once and the binaries removed: it fills
# Go's caches and nothing else. For CI, whose four e2e jobs share one cache an
# architecture (ci.yml's e2e job says why one): a cache is saved by the
# first job to finish a run whose key missed, with only what that job
# compiled, so a job whose key missed builds every lane's first, and
# whichever saves, saves what all four use. The three lanes' binaries
# are the metrics lane's set (e2e/bin.sh, with notify's test binary);
# e2e-backup's own are moteld, compiled the same, and motel-backup with
# -tags e2e, built here with the flags its target above builds it with.
# On a machine whose caches are volumes that stay, nothing needs this.
e2e-warm: ## every e2e lane's Go, built to fill Go's caches (CI, when its cache key missed)
	$(call e2e_bin,e2e-warm notify-tests)
	$(COMPOSE) run --rm -e CGO_ENABLED=0 dev sh -c 'cd backups && go build -trimpath -tags e2e -o /dev/null ./cmd/motel-backup'
	rm -rf build/e2e-warm

# Leak-hunting soak against the product binary: deploy/reload/traffic
# churn, then goroutine/fd return-to-baseline assertions over the admin
# metrics endpoint. ~15-20 min at defaults; tune with SOAK_DEPLOYS,
# SOAK_RELOADS, SOAK_CLIENTS, SOAK_REQS. Runs per merge to main and
# weekly in CI (soak.yml); never in the PR path.
#
# On failure: moteld runs under systemd inside e2e-motel, so that
# container's console carries nothing from it and the journal is the
# evidence — same as the e2e target above, tail-bounded here because a
# soak pushes 15-20 minutes of churn through the stub servers.
soak: ## ~20 min leak hunt: deploy/reload churn, goroutine/fd assertions
	$(call e2e_bin,e2e)
	E2E_LANE=e2e $(COMPOSE) up --build -d e2e-motel e2e-upstream e2e-artifacts
	status=0; \
	$(COMPOSE) run --rm -e SOAK_DEPLOYS -e SOAK_RELOADS -e SOAK_CLIENTS -e SOAK_REQS \
		--entrypoint "/bin/sh /soak.sh" e2e-runner || status=1; \
	if [ $$status -ne 0 ]; then \
		$(COMPOSE) logs --tail 200 e2e-motel e2e-upstream e2e-artifacts; \
		$(COMPOSE) exec -T e2e-motel journalctl --no-pager -n 300 || true; \
	fi; \
	$(COMPOSE) down --remove-orphans; \
	exit $$status

e2e-logs: ## the e2e stack's logs and moteld's journal, while it is up
	$(COMPOSE) logs e2e-upstream e2e-artifacts
	$(COMPOSE) exec -T e2e-motel journalctl --no-pager -n 300

# --rmi local: the images this checkout built (no custom tag), one set
# per compose project; pulled images are kept.
clean: ## take the compose project down, with its volumes and the images it built
	$(COMPOSE) down -v --remove-orphans --rmi local

# Each target's `##` text, from its own line, in the order above. A
# target without one is not listed. Not the default goal: a bare make
# runs test.
help: ## this list
	@awk -F ':.*## ' '/^[a-z][a-z0-9-]*:.*## / { printf "  %-18s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
