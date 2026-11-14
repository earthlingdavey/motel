#!/usr/bin/env python3
"""Says whether a pull request needs ci.yml's build-and-test jobs.

Reads the changed paths, one per line, on stdin (ci.yml's `changes` job
feeds it `git diff --name-only --no-renames` between the PR's merge
commit and its first parent, the base branch), and prints the
`heavy=true|false` line that job writes to $GITHUB_OUTPUT.

heavy=false only when there are paths and every one is markdown, or a
usage ledger file (a .csv or .json under usage/), that no job reads: then lint, test, govulncheck, the e2e lanes, package
and install-test are skipped, and ci-ok accepts them skipped. gitleaks,
usage-check and dependency-review run on every PR whatever this says,
so usage/ and docs/footprint.md (which usage-check renders and diffs)
stay covered. Anything else, or any doubt, is heavy=true: an empty
list too, since a diff that went wrong reads as one. Pushes to main and
the release's workflow_call run everything without asking. A PR is
judged by the base branch's copy of this file, never its own, so an
edit to this rule cannot skip the checks of the PR that makes it; an
edit to ci.yml can (see the residuals).

No image build reads markdown (e2e/Dockerfile's `COPY . .` copies the
nested files, .dockerignore's `*.md` matching the root's alone, and
nothing in the build opens them), but tests and install-test do: READ lists those files, and examples/ is heavy whole,
since examples/templates_test.go checks the links of every .md under
it. A test or a lane that starts reading another markdown file adds it
here, or a PR that changes only that file merges with what reads it
unrun (the full run on main would catch it then, after the merge). The
readers, found with:
  git grep -nE '"[^" ]*\\.md"|\\.md:/|"\\.md"|go:embed.*\\.md' -- '*.go' Makefile e2e packaging

Accepted residuals (scored by the triage skill: severity, occurrence,
detection, their product; the fix's size and risk):

- A new reader not added to READ. A test or lane starts reading a
  markdown file and READ is not given it; a later PR changing only that
  file skips the reader, and main's full run goes red after the merge,
  naming the test. 2/2/1 = 4; a scan that finds readers is a new file
  of heuristics, size 3, risk 2. The grep above is the check by hand.
- ci-ok's list of gated jobs and the jobs' `if:` lines are two lists. A
  job gated without being added to ci-ok's list leaves ci-ok red on the
  first light PR, naming that job: 2/3/1 = 6, red and named, which the
  single-fault rule asks for. One list of the jobs that must always run
  instead would pass, as skipped, a job skipped for any other reason.
- The gate's own code is the PR's. A pull request runs the ci.yml it
  carries, so a slip in the classify step or in ci-ok's jq, made in the
  PR that changes them, can pass that PR with its jobs skipped: 5/2/3
  = 30 (two faults: the slip, and one that reads light; the run shows
  the jobs skipped and ci-ok says why). The same was true of ci-ok
  before the skip, and nothing inside a pull_request workflow can judge
  the workflow itself; a check from pull_request_target, which runs the
  base's workflows, could, at the cost of a workflow with the base's
  token running beside every PR. The step and the jq were proved by
  hand against made-up merge commits and needs when written.
"""
import sys

# The markdown some job reads, and the job:
READ = frozenset({
    "backups/README.md",   # backups/engine/lifecycle_invariants_test.go
    "box/DESIGN-box.md",   # box/units_test.go, box/failure_modes_test.go
    "docs/backups.md",     # install-test: smoke.sh runs its commands
    "docs/hardening.md",   # liveswap/sandbox_promises_test.go
})


def heavy(path):
    if path in READ or path.startswith("examples/"):
        return True
    # A fixture under testdata/ is read by its package's tests whatever
    # its extension.
    if "testdata" in path.split("/"):
        return True
    # The ledger alone: anything else under usage/, code included, is
    # built and tested as any other directory is.
    ledger = path.startswith("usage/") and path.endswith((".csv", ".json"))
    return not (path.endswith(".md") or ledger)


def main():
    paths = [line.strip() for line in sys.stdin if line.strip()]
    why = [p for p in paths if heavy(p)]
    if not paths:
        print("heavy: no changed paths read", file=sys.stderr)
    elif why:
        print(f"heavy: {why[0]}" + (f" and {len(why) - 1} more" if len(why) > 1 else ""), file=sys.stderr)
    else:
        print(f"light: {len(paths)} path(s), none read by a skipped job", file=sys.stderr)
    print(f"heavy={'true' if why or not paths else 'false'}")


if __name__ == "__main__":
    main()
