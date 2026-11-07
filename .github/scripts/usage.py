#!/usr/bin/env python3
"""usage: the public ledger of what building motel costs.

Three sources, month-level CSVs under usage/, rendered to
docs/footprint.md:

  collect-claude   usage/claude/      Claude Code sessions, from the
                   <machine>.csv      transcripts Claude Code keeps under
                   ~/.claude/projects (mounted read-only at CLAUDE_PROJECTS
                   by the `usage` compose service): the project directories
                   whose name contains motel or one of the --match names
                   (comma-separated: extra names, for a checkout that
                   kept an older one), which is this repository, its
                   worktrees and its clones. One row per month and
                   model: API calls and tokens by kind. The sessions are
                   spread over several machines and each sees only its
                   own transcripts, so every machine writes its own
                   file, named by --machine (a public label), and render
                   sums the files. The label is set once per machine,
                   on the host: `git config --global motel.usage-machine
                   <label>`; make reads it, and motel.usage-match for
                   the extra names.
  collect-actions  usage/runs.csv     GitHub Actions, from the API for
                   usage/jobs.csv     --repo (GH_TOKEN required). runs.csv:
                   one row per month and workflow, the runs with at
                   least one timed job, each counted once however many
                   runner types its jobs used. jobs.csv: one row per
                   month, workflow and runner label: jobs, seconds,
                   billed minutes (each job rounded up to the minute).
  render           docs/footprint.md  from the CSVs (the machine files
                   summed), usage/copilot.csv
                   (AI credits typed in by hand from GitHub billing; its
                   note column is public too) and usage/factors.json
                   (prices, CO2 factors, runner watts, each with its
                   source).

The CSVs hold facts (tokens, seconds); cost and CO2 are recomputed from
factors.json at render time, so a price or factor change never rewrites
history by hand. Months are deliberately the finest grain: per-session
or per-day rows would publish working hours.

The steps collect first and write last, so a failure (no token, the API
down) leaves every file as it was. A run is one API call per workflow
run plus the listing, about 3,500 today, against the token's 5,000 an
hour: a second run inside the hour exhausts the budget and writes
nothing. A month already in a CSV is replaced by the fresh count unless
a count in one of its rows shrank or a row vanished — a deleted
transcript, Actions runs past GitHub's retention — then the committed
month stays and a warning says why; runs.csv and jobs.csv decide each
month together, so a run is never kept without its jobs. --recompute
replaces regardless (for a parser fix). A month the source no longer
has at all is kept either way; delete its rows by hand to drop it. The
rule holds file by file, so a run on one machine never lowers a month
or touches another machine's share; the page is the sum of the files
committed so far. Each machine commits its file and the page together;
when two such commits meet, take either side of the page and run `make
usage-check`: it re-renders the page from the merged files and fails
until that page is staged. One machine under two labels is summed
twice, so the label lives in the host's git config, once per machine,
where every worktree and clone reads it; a label with no file yet, or a
fresh row identical to one in another machine's file, warns. --match
adds names: the default is always counted. To relabel a machine, `git
mv` its file and set the config again.

`make usage` runs all three steps; `make usage-check` (CI) runs
usage_test.py, then re-renders from the committed CSVs and fails on a
difference. Stdlib only.

Accepted residuals (scored with .claude/skills/triage: severity,
occurrence, detection; fix size, risk):

- A month absent from the source stays under --recompute, so a parser
  change that moves every record out of a month leaves its old rows
  (S3 O5 D1 = 15; size 2, risk 2). Dropping it would wipe every month
  past the 90-day run window on each --recompute, a worse failure; the
  warning says to delete the rows by hand.
- A hand-edited file malformed beyond the up-front checks (a credits
  value the checks pass, a factors key below the top level) fails at
  render, after the CSVs were written (S2 O5 D1 = 10; size 3, risk 2).
  The error names the file; `make usage-check` re-renders the page once
  it is fixed. A full schema or a render-before-write is more code than
  a loud, recoverable failure buys.
- Two machines sharing one label, because ~/.gitconfig is synced
  between them, write one file: the merge rule keeps the larger
  machine's months and warns that the source shrank on the other's
  runs (S3 O2 D1 = 6; size 3, risk 2). The Makefile says to set the
  label per clone, without --global, when the global config is synced;
  a label kept beside the transcripts would be per machine by
  construction, and that mount is read-only and Claude Code's.
- A USAGE_MACHINE typed on the command line that is not the machine's
  configured label writes a second file, and the page sums the machine
  twice (S3 O5 D1 = 15; size 3, risk 2). The run warns, naming the
  existing labels and any row of theirs identical to a fresh one. That
  row check is a warning, not a stop, because two machines can produce
  one identical small row (a single title call) and a stop would leave
  no way on. A fingerprint committed beside each machine file would
  decide it exactly, and is a second file format for a case the warning
  already names.
"""

import argparse
import csv
import glob
import http.client
import json
import math
import os
import re
import sys
import tempfile
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime

CLAUDE_DIR = "usage/claude"
DEFAULT_MATCH = "motel"
RUNS_CSV = "usage/runs.csv"
JOBS_CSV = "usage/jobs.csv"
COPILOT_CSV = "usage/copilot.csv"
FACTORS = "usage/factors.json"
FOOTPRINT = "docs/footprint.md"

CLAUDE_COLS = ["calls", "input", "output", "cache_write_5m", "cache_write_1h", "cache_read"]
JOBS_COLS = ["jobs", "seconds", "billed_minutes"]
COPILOT_REVIEW_WORKFLOW = "copilot-pull-request-reviewer"
API_HOST = "api.github.com"
# Copilot's agent runners carry a per-environment UUID (ubuntu-cca-<uuid>): one family, not a row each.
UUID_SUFFIX = re.compile(r"-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
# A machine's label is a public file name: lowercase letters, digits and hyphens.
MACHINE = re.compile(r"[a-z0-9][a-z0-9-]*")


def warn(msg):
    print("warning: " + msg, file=sys.stderr)


def is_month(s):
    return (len(s) == 7 and s.isascii() and s[:4].isdigit() and s[4] == "-" and s[5:].isdigit()
            and 1 <= int(s[5:]) <= 12)


# ---- CSV ledger ----------------------------------------------------------


def read_csv(path):
    if not os.path.exists(path):
        return []
    with open(path, newline="") as f:
        return list(csv.DictReader(f))


def write_csv(path, header, rows):
    """Atomic, so a failure mid-write leaves the committed file as it was."""
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path) or ".", prefix=".usage-")
    try:
        with os.fdopen(fd, "w", newline="") as f:
            w = csv.DictWriter(f, fieldnames=header, lineterminator="\n")
            w.writeheader()
            w.writerows(rows)
        os.chmod(tmp, 0o644)
        os.replace(tmp, path)
    except BaseException:
        if os.path.exists(tmp):
            os.unlink(tmp)
        raise


def merge(group, recompute):
    """Month-level merge of fresh counts into the committed CSVs.

    group: [(path, keys, cols, fresh rows)]. Its files decide each month
    together: runs.csv and jobs.csv come from the same runs, so a month
    kept in one and replaced in the other would count runs without
    their jobs, or jobs without their runs.
    """
    def rows_of(rows, keys, m):
        return {tuple(r[k] for k in keys): r for r in rows if r["month"] == m}

    olds = [read_csv(path) for path, _, _, _ in group]
    names = " + ".join(path for path, _, _, _ in group)
    months = sorted({r["month"] for rows in olds for r in rows} | {r["month"] for _, _, _, fresh in group for r in fresh})
    outs = [[] for _ in group]
    for m in months:
        split = [(rows_of(old, keys, m), rows_of(fresh, keys, m), cols)
                 for (_, keys, cols, fresh), old in zip(group, olds)]
        absent = all(not n for _, n, _ in split)
        # Each count on its own: growth in one must not hide a loss in another.
        lost = [] if recompute or absent else [
            " ".join(k[1:]) for o, n, cols in split for k in o
            if k not in n or any(int(n[k][c]) < int(o[k][c]) for c in cols)]
        if absent:
            # Kept whatever the flags: the source's window is finite (90 days
            # of runs), so --recompute must not wipe the history before it.
            warn(f"{names}: {m} is no longer in the source; the committed rows stay "
                 "(delete them by hand to drop the month)")
        elif lost:
            warn(f"{names}: {m}: {', '.join(lost)} shrank or vanished, so the source lost data; "
                 "the committed month stays (--recompute overrides)")
        for out, (o, n, _) in zip(outs, split):
            keep = o if absent or lost else n
            out += [keep[k] for k in sorted(keep)]
    for (path, keys, cols, _), out in zip(group, outs):
        write_csv(path, keys + cols, out)


# ---- Claude Code transcripts --------------------------------------------


def tokens(v):
    return int(v or 0)


def claude_path(machine):
    return os.path.join(CLAUDE_DIR, machine + ".csv")


def stems():
    """(path, stem) for every usage/claude/*.csv, in order."""
    return [(p, os.path.basename(p)[:-len(".csv")]) for p in sorted(glob.glob(claude_path("*")))]


def machines():
    """The labels with a file; a file no label can name stops the run, or it would be summed and never reachable."""
    for path, m in stems():
        if not MACHINE.fullmatch(m) or not os.path.isfile(path):
            sys.exit(f"{path}: not a machine file (a label is lowercase letters, digits and hyphens); rename or remove it")
    return [m for _, m in stems()]


def so_far():
    """For a message on the way out: the labels there are, without a stray file's exit taking the message's place."""
    return "files so far: " + (", ".join(m for _, m in stems() if MACHINE.fullmatch(m)) or "none yet")


def label(machine):
    """A valid --machine: it names this machine's file and is a public label. A bad one stops the run."""
    if not machine:
        sys.exit("--machine is not set: each machine's sessions go in their own file, so name this one, once, "
                 f"on the host: `git config --global motel.usage-machine <label>` (make reads it; {so_far()}). "
                 "Set it per clone, without --global, if your ~/.gitconfig is synced between machines")
    if not MACHINE.fullmatch(machine):
        sys.exit(f"--machine {machine!r} must be lowercase letters, digits and hyphens: it names "
                 f"{CLAUDE_DIR}/<machine>.csv, a public file ({so_far()})")
    return machine


def check_machine(machine):
    """The label, checked before anything is read; one with no file yet is said so."""
    label(machine)
    have = machines()
    if have and machine not in have:
        # A label the machine has not used before: a typo for one it has is summed twice, and
        # same_machine catches that only while a committed row is still what this run counts.
        warn(f"{machine} is a new machine ({so_far()}); if this one already has a file, use that label")


def match_names(match):
    return [n.strip() for n in match.split(",") if n.strip()]


def match_list(match):
    """The names counted: the default and --match's extras; a --match that names nothing stops the run."""
    names = match_names(match)
    if match and not names:
        sys.exit(f"--match {match!r} names nothing: a name, or several comma-separated")
    return list(dict.fromkeys([DEFAULT_MATCH, *names]))


def same_machine(machine, fresh):
    """Warn when another label's file holds a row identical to a fresh one.

    Two machines all but never produce the same six counts for a month
    and model, so an identical row means this machine was run before
    under another label, and summing the two files would count it twice.
    A warning, not a stop: one small row (a single title call) can
    coincide, and a stop would leave no way on. The run goes on to write
    this label's file, so both ways out undo that.
    """
    this = claude_path(machine)
    for other in machines():
        if other != machine and any(r in fresh for r in read_csv(claude_path(other))):
            # The way out is for a label that is new; an established file is not to be removed on a coincidence.
            out = (f": either `git mv -f {claude_path(other)} {this}` and run again, or `rm {this}` and run with "
                   f"--machine {other}" if not os.path.exists(this) else "; a single small row can also coincide")
            warn(f"{claude_path(other)} holds a row identical to one counted here. If that is this machine under "
                 f"an earlier label, the two files are summed twice{out}")
            return


def read_claude():
    """Every machine's rows together; a (month, model) repeats across machines and render sums them."""
    return [r for m in machines() for r in read_csv(claude_path(m))]


def collect_claude(root, names):
    if not names:
        sys.exit("no project-directory names to count")
    if not os.path.isdir(root):
        sys.exit(f"{root} is not a directory: is ~/.claude/projects mounted? (docker-compose.yml, service usage)")
    projects = sorted(os.path.basename(d) for d in glob.glob(os.path.join(root, "*")) if os.path.isdir(d))
    dirs = [os.path.join(root, d) for d in projects if any(n in d for n in names)]
    # The others are counted, not named: they are this machine's other projects, and a run's
    # output gets pasted. The count is there so a checkout under an older name is seen left out.
    n_others = len(projects) - len(dirs)
    others = (f"{n_others} other project director{'y' if n_others == 1 else 'ies'} (see --match; none at all means "
              "the mount is empty: docker-compose.yml, service usage)")
    if not dirs:
        sys.exit(f"no project directory under {root} has {' or '.join(repr(n) for n in names)} in its name; {others}")
    print("claude: " + ", ".join(os.path.basename(d) for d in dirs))
    if len(dirs) < len(projects):
        print(f"claude: not counted: {others}")
    for n in names:
        if n != DEFAULT_MATCH and not any(n in d for d in projects):
            # A typo in --match is otherwise silent for good. The default is left out: a machine whose
            # checkout kept the older name has no motel directory, and that is what --match is for.
            warn(f"no project directory here has {n!r} in its name")
    msgs, files = {}, 0  # (message.id, requestId) -> [month, model, counts]
    for d in dirs:
        for path in glob.glob(os.path.join(d, "**", "*.jsonl"), recursive=True):
            if not os.path.isfile(path):
                continue
            files += 1
            with open(path, errors="replace") as f:
                for n, line in enumerate(f):
                    try:
                        rec = json.loads(line)
                    except ValueError:
                        continue
                    msg = rec.get("message")
                    if not isinstance(msg, dict):
                        continue
                    usage, ts, model = msg.get("usage"), rec.get("timestamp") or "", msg.get("model") or ""
                    # Synthetic records (model "<synthetic>") carry no usage worth counting.
                    if not usage or not model.startswith("claude"):
                        continue
                    if not is_month(ts[:7]):
                        sys.exit(f"{path}:{n + 1}: timestamp {ts!r} is not ISO 8601; nothing was written")
                    cc = usage.get("cache_creation") or {}
                    w5, w1 = cc.get("ephemeral_5m_input_tokens"), cc.get("ephemeral_1h_input_tokens")
                    if w5 is None and w1 is None:  # older transcripts: no TTL split, billed as 5m
                        w5, w1 = usage.get("cache_creation_input_tokens"), 0
                    counts = {
                        "input": tokens(usage.get("input_tokens")),
                        "output": tokens(usage.get("output_tokens")),
                        "cache_write_5m": tokens(w5),
                        "cache_write_1h": tokens(w1),
                        "cache_read": tokens(usage.get("cache_read_input_tokens")),
                    }
                    # Claude Code writes a streamed reply as one line per
                    # content block, all sharing the message id. The first
                    # line's output_tokens is a placeholder (a handful) and
                    # the last line's is the real count; the input and cache
                    # fields are the same on every line. One count per
                    # message, the largest value of each field.
                    key = (msg["id"], rec.get("requestId")) if msg.get("id") else (path, n)
                    seen = msgs.get(key)
                    if seen is None:
                        msgs[key] = [ts[:7], model, counts]
                    else:
                        for k, v in counts.items():
                            seen[2][k] = max(seen[2][k], v)
    rows = {}
    for month, model, counts in msgs.values():
        c = rows.setdefault((month, model), dict.fromkeys(CLAUDE_COLS, 0))
        c["calls"] += 1
        for k, v in counts.items():
            c[k] += v
    print(f"claude: {len(dirs)} project dirs, {files} transcript files, {len(msgs)} API calls")
    return [{"month": m, "model": model, **{k: str(v) for k, v in c.items()}} for (m, model), c in rows.items()]


# ---- GitHub Actions -------------------------------------------------------


_conn = threading.local()


def gh(path, token):
    """GET https://api.github.com{path} as JSON.

    One keep-alive connection per thread: a month's run is a few thousand
    calls, and Docker Desktop's network refuses connections when each
    call opens a new one. A dropped connection, a 5xx or a rate limit
    that names its wait is retried; anything else ends the run, and
    nothing has been written yet.
    """
    headers = {
        "Authorization": "Bearer " + token,
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "motel-usage",
    }
    for attempt in range(6):
        conn = getattr(_conn, "c", None)
        if conn is None:
            conn = _conn.c = http.client.HTTPSConnection(API_HOST, timeout=60)
        try:
            conn.request("GET", path, headers=headers)
            resp = conn.getresponse()
            body = resp.read()
        except (OSError, http.client.HTTPException) as e:
            conn.close()
            _conn.c = None
            if attempt == 5:
                sys.exit(f"GitHub API unreachable after 6 attempts ({e}) for {path}; nothing was written")
            warn(f"{e} for {path}; retrying")
            time.sleep(2 ** attempt)
            continue
        if resp.status == 200:
            try:
                return json.loads(body.decode())
            except ValueError:
                sys.exit(f"GitHub API sent a non-JSON body for {path}; nothing was written")
        if resp.status == 401:
            sys.exit("GitHub API 401: the token is bad or expired; `gh auth login` again. Nothing was written")
        retry_after = resp.getheader("Retry-After") or ""
        if resp.status in (403, 429) and retry_after.isdigit():
            # A secondary rate limit: GitHub says how long to wait.
            wait = min(int(retry_after), 300)
            warn(f"GitHub API {resp.status} for {path}: rate limited, waiting {wait}s")
            time.sleep(wait)
            continue
        if resp.status in (403, 429) and resp.getheader("X-RateLimit-Remaining") == "0":
            sys.exit("GitHub API rate limit exhausted; nothing was written. "
                     f"It resets at epoch {resp.getheader('X-RateLimit-Reset')}")
        if resp.status < 500 or attempt == 5:
            sys.exit(f"GitHub API {resp.status} for {path}: {body.decode(errors='replace')[:200]}")
        time.sleep(2 ** attempt)
    sys.exit(f"GitHub API gave up on {path}; nothing was written")


def paged(path, key, token):
    out, page = [], 1
    while True:
        batch = gh(f"{path}&per_page=100&page={page}", token).get(key, [])
        out += batch
        if len(batch) < 100:
            return out
        page += 1


def collect_actions(repo, token):
    # Keyed by id: a run created while paging shifts the page boundary
    # and would otherwise be listed twice.
    runs = {r["id"]: r for r in paged(f"/repos/{repo}/actions/runs?x=", "workflow_runs", token)}.values()

    def jobs(run):
        return run, paged(f"/repos/{repo}/actions/runs/{run['id']}/jobs?filter=all", "jobs", token)

    nruns, counts, njobs = {}, {}, 0
    with ThreadPoolExecutor(8) as ex:
        for run, js in ex.map(jobs, runs):
            month = run["created_at"][:7]
            # ".github/workflows/ci.yml" -> "ci"; GitHub's own are
            # "dynamic/agents/copilot-pull-request-reviewer" and the like.
            workflow = os.path.splitext(os.path.basename(run.get("path") or "?"))[0]
            timed = False
            for j in js:
                if not j.get("started_at") or not j.get("completed_at"):
                    continue
                sec = (datetime.fromisoformat(j["completed_at"]) - datetime.fromisoformat(j["started_at"])).total_seconds()
                if sec <= 0:
                    continue
                runner = ",".join(sorted(UUID_SUFFIX.sub("", label) for label in j.get("labels") or ["?"]))
                c = counts.setdefault((month, workflow, runner), dict.fromkeys(JOBS_COLS, 0))
                c["jobs"] += 1
                c["seconds"] += int(sec)
                c["billed_minutes"] += math.ceil(sec / 60)
                njobs += 1
                timed = True
            if timed:
                # One run however many runner types its jobs used. Kept at
                # this grain because the per-runner rows cannot give it
                # back: two single-runner runs look like one two-runner run.
                nruns[(month, workflow)] = nruns.get((month, workflow), 0) + 1
    print(f"actions: {len(runs)} runs, {njobs} timed jobs from the API")
    return ([{"month": m, "workflow": w, "runs": str(n)} for (m, w), n in nruns.items()],
            [{"month": m, "workflow": w, "runner": r, **{k: str(v) for k, v in c.items()}}
             for (m, w, r), c in counts.items()])


# ---- render ---------------------------------------------------------------


def family(model):
    for f in ("fable", "mythos", "opus", "sonnet", "haiku"):
        if f in model:
            return "fable" if f == "mythos" else f
    sys.exit(f"{model}: no model family (fable/opus/sonnet/haiku) in the name")


_warned = set()


def price(model, prices):
    p = prices["models"].get(model)
    if p is None and model[-8:].isdigit():  # dated snapshot: claude-haiku-4-5-20251001
        p = prices["models"].get(model[:-9])
    if p is None:
        if model not in _warned:
            _warned.add(model)
            warn(f"{FACTORS} has no price for {model}; using the {family(model)} family's, which is the current tier's")
        p = prices["families"][family(model)]
    return p


def claude_derived(r, factors):
    """(usd, gCO2e) for one row of a machine's claude file."""
    prices = factors["prices_usd_per_mtok"]
    p, mult = price(r["model"], prices), prices["cache_write_multiplier"]
    g = factors["claude_gco2e_per_mtok"]
    f = g["families"][family(r["model"])]
    inp, out = int(r["input"]), int(r["output"])
    w5, w1, cr = int(r["cache_write_5m"]), int(r["cache_write_1h"]), int(r["cache_read"])
    usd = (inp * p["input"] + out * p["output"] + cr * p["cache_read"]
           + w5 * p["input"] * mult["5m"] + w1 * p["input"] * mult["1h"]) / 1e6
    gco2 = ((inp + (w5 + w1) * g["cache_write_factor"] + cr * g["cache_read_factor"]) * f["input"]
            + out * f["output"]) / 1e6
    return usd, gco2


def runner_watts(label, a):
    spec = a["runners"].get(label, a["runners"]["default"])
    return (spec["vcpu"] * (a["vcpu_min_w"] + a["cpu_utilisation"] * (a["vcpu_max_w"] - a["vcpu_min_w"]))
            + spec["gb"] * a["gb_hour_kwh"] * 1000) * a["pue"]


def read_copilot():
    rows = read_csv(COPILOT_CSV)
    if rows and "ai_credits_usd" not in rows[0]:
        sys.exit(f"{COPILOT_CSV}: the header must have an ai_credits_usd column")
    out = {}
    for r in rows:
        m, v = r.get("month") or "", (r.get("ai_credits_usd") or "").strip()
        if not is_month(m):
            sys.exit(f"{COPILOT_CSV}: month {m!r} is not YYYY-MM")
        if m in out:
            sys.exit(f"{COPILOT_CSV}: {m} appears twice; one row per month")
        if not v:
            out[m] = None
            continue
        try:
            out[m] = float(v)
        except ValueError:
            sys.exit(f"{COPILOT_CSV}: {m}: ai_credits_usd {v!r} is not a number")
        if not v.isascii() or not math.isfinite(out[m]) or out[m] < 0:
            sys.exit(f"{COPILOT_CSV}: {m}: ai_credits_usd {v!r} must be a plain non-negative number")
    return out


def load_factors():
    """The hand-edited factors file, checked for shape so a bad edit stops a run before it writes."""
    try:
        with open(FACTORS) as f:
            factors = json.load(f)
    except ValueError as e:
        sys.exit(f"{FACTORS}: not valid JSON: {e}")
    for key in ("_sources", "prices_usd_per_mtok", "claude_gco2e_per_mtok", "actions"):
        if not isinstance(factors.get(key), dict):
            sys.exit(f"{FACTORS}: missing the {key} object")
    return factors


def fmt(x, d=0):
    return "—" if x is None else f"{x:,.{d}f}"


def table(header, rows):
    lines = ["| " + " | ".join(header) + " |",
             "|" + "|".join("---:" if i else "---" for i in range(len(header))) + "|"]
    lines += ["| " + " | ".join(r) + " |" for r in rows]
    return "\n".join(lines)


def render(factors):
    claude, runs, jobs, copilot = read_claude(), read_csv(RUNS_CSV), read_csv(JOBS_CSV), read_copilot()
    a, g = factors["actions"], factors["claude_gco2e_per_mtok"]
    grid_c = g["grid_gco2e_per_kwh"]
    months = sorted({r["month"] for r in claude + runs + jobs} | set(copilot))
    keys = ["calls", "out", "cr", "usd", "c_kwh", "c_kg", "min", "r_usd", "r_kwh", "r_kg", "reviews", "credits"]

    def month_stats(m):
        """A source with no rows for the month renders as "—", not 0."""
        s = dict.fromkeys(keys)
        s["credits"] = copilot.get(m)
        cl, ac = [r for r in claude if r["month"] == m], [r for r in jobs if r["month"] == m]
        if cl:
            s.update(calls=0, out=0.0, cr=0.0, usd=0.0, c_kwh=0.0, c_kg=0.0)
        for r in cl:
            usd, gco2 = claude_derived(r, factors)
            s["calls"] += int(r["calls"])
            s["out"] += int(r["output"]) / 1e6
            s["cr"] += int(r["cache_read"]) / 1e9
            s["usd"] += usd
            s["c_kg"] += gco2 / 1000
            s["c_kwh"] += gco2 / grid_c
        if ac:
            s.update(min=0, r_usd=0.0, r_kwh=0.0)
            s["reviews"] = sum(int(r["runs"]) for r in runs
                               if r["month"] == m and r["workflow"] == COPILOT_REVIEW_WORKFLOW)
        for r in ac:
            s["min"] += int(r["billed_minutes"])
            s["r_kwh"] += int(r["seconds"]) / 3600 * runner_watts(r["runner"], a) / 1000
            s["r_usd"] += int(r["billed_minutes"]) * a["usd_per_minute_private_repo"]
        if ac:
            s["r_kg"] = s["r_kwh"] * a["grid_gco2e_per_kwh"] / 1000
        return s

    stats = {m: month_stats(m) for m in months}

    def total(k):
        vals = [s[k] for s in stats.values() if s[k] is not None]
        return sum(vals) if vals else None

    tot = {k: total(k) for k in keys}

    def row(label, s):
        return [label, fmt(s["calls"]), fmt(s["out"], 1), fmt(s["cr"], 1), fmt(s["usd"]), fmt(s["c_kwh"]),
                fmt(s["c_kg"], 1), fmt(s["min"]), fmt(s["r_usd"]), fmt(s["r_kwh"], 1), fmt(s["r_kg"], 1),
                fmt(s["reviews"]), fmt(s["credits"], 2)]

    monthly = table(
        ["Month", "Claude calls", "Output Mtok", "Cache-read Gtok", "Claude USD (list)", "Claude kWh",
         "Claude kgCO2e", "Runner min", "Runner USD if private", "Runner kWh", "Runner kgCO2e",
         "Copilot reviews", "Copilot USD"],
        [row(m, stats[m]) for m in months] + [row("**Total**", tot)] if stats else [])

    by_model = {}
    for r in claude:
        c = by_model.setdefault(r["model"], dict.fromkeys(["calls", "in", "out", "w", "cr", "usd", "kg"], 0.0))
        usd, gco2 = claude_derived(r, factors)
        c["calls"] += int(r["calls"])
        c["in"] += int(r["input"]) / 1e6
        c["out"] += int(r["output"]) / 1e6
        c["w"] += (int(r["cache_write_5m"]) + int(r["cache_write_1h"])) / 1e6
        c["cr"] += int(r["cache_read"]) / 1e9
        c["usd"] += usd
        c["kg"] += gco2 / 1000
    models = table(
        ["Model", "Calls", "Input Mtok", "Output Mtok", "Cache-write Mtok", "Cache-read Gtok", "List USD", "kgCO2e"],
        [[m, fmt(c["calls"]), fmt(c["in"], 2), fmt(c["out"], 1), fmt(c["w"], 1), fmt(c["cr"], 2), fmt(c["usd"]),
          fmt(c["kg"], 1)] for m, c in sorted(by_model.items(), key=lambda kv: -kv[1]["usd"])])

    by_wf = {}
    for r in runs:
        by_wf.setdefault(r["workflow"], dict.fromkeys(["runs", "min", "kwh"], 0.0))["runs"] += int(r["runs"])
    for r in jobs:
        c = by_wf.setdefault(r["workflow"], dict.fromkeys(["runs", "min", "kwh"], 0.0))
        c["min"] += int(r["billed_minutes"])
        c["kwh"] += int(r["seconds"]) / 3600 * runner_watts(r["runner"], a) / 1000
    workflows = table(["Workflow", "Runs", "Billed minutes", "kWh"],
                      [[w, fmt(c["runs"]), fmt(c["min"]), fmt(c["kwh"], 1)]
                       for w, c in sorted(by_wf.items(), key=lambda kv: -kv[1]["min"])])

    src = factors["_sources"]
    sources = "\n".join(f"- **{label}.** {src[key]}" for key, label in [
        ("prices", "Prices"), ("claude_co2", "CO2 per token"), ("reference_prompt", "A published per-prompt figure"),
        ("transcripts", "Transcripts"), ("actions_retention", "Run retention"), ("actions", "Runners"),
        ("actions_grid", "Runner grid"), ("actions_price", "Runner price"), ("copilot", "Copilot credits")])
    spec = a["runners"]["default"]
    avg_wh = tot["c_kwh"] * 1000 / tot["calls"] if tot["calls"] else None
    ratio = avg_wh / g["reference_prompt_wh"] if avg_wh else None
    text = f"""# What building motel costs

Generated by `make usage` from [`usage/`](../usage): do not edit by hand.
The CSVs there are the facts (tokens, runner seconds, credits); every
derived number below is recomputed from
[`usage/factors.json`](../usage/factors.json) each time, so a price or
factor change updates the whole history at once.

Three things are counted: the Claude Code sessions that write the code
(tokens, priced at Anthropic's pay-as-you-go list prices and converted
to energy and CO2 with published per-token factors), the GitHub Actions
runner minutes that test and release it, and the Copilot AI credits that
review it. Not counted: the maintainer's laptop, web search and fetch
tool charges inside sessions, Copilot's own compute (GitHub publishes no
energy figures), network transfer, and the models' training.

## By month

{monthly}

A dash is a month the source has no rows for. Claude USD is what the
tokens would cost at list price, not what was paid (Claude Code runs on
a subscription); it is the comparable number. Runner minutes are free
for a public repository; "if private" is GitHub's list rate for them.
Every kWh and kgCO2e is an estimate, see below.

## By model

{models}

## By workflow

{workflows}

Runs are runs with at least one timed job, each counted once however
many runner types its jobs used.

## Where the numbers come from

- **Claude Code sessions.** Claude Code keeps a transcript of every
  session under `~/.claude/projects` on the machine it ran on; each API
  call's token counts are read from there, one count per message,
  taking the final line of a streamed reply (the first line's output
  count is a placeholder, which is how ccusage, checked against this on
  2026-10-16, comes out about a fifth low on output while agreeing on
  every other column). Only project directories named after this
  repository, its worktrees and clones are counted. The sessions are
  spread over several machines, each of which sees only its own
  transcripts, so each keeps its own file under
  [`usage/claude/`](../usage/claude) and the figures here are their sum.
  Claude Code purges transcripts after 30 days by
  default (Sources: transcripts), and the first machine on the ledger
  did until 2026-10-14: its sessions before 2026-09-13 are gone, so this
  part of the ledger starts there although the GitHub repository
  started on 2026-08-16.
  Machines so far: {", ".join(machines()) or "none"}.
- **Claude USD.** Anthropic's published prices per model (Sources:
  prices). Cache writes cost 1.25× (5-minute) or 2× (1-hour) the input
  price; cache reads the per-model rate.
- **Claude kWh and kgCO2e.** Per-token factors fitted to estimated
  Claude 3.7 Sonnet query energies and scaled per model family (Sources:
  CO2 per token). Cache writes count as input tokens, cache reads at
  {g['cache_read_factor']}× an input token; kWh is derived back from CO2 at the {grid_c}
  gCO2e/kWh those factors assume. Anthropic publishes no energy figures.
  By these factors the average call here comes out at {fmt(avg_wh, 1)} Wh;
  Google's published median for a Gemini Apps text prompt is
  {g['reference_prompt_wh']} Wh (Sources: a published per-prompt figure), {fmt(ratio)}× lower,
  though a chat prompt and a coding call with a large cached context
  are different work. Read these as an order of magnitude, not a
  measurement.
- **Runner minutes.** From the GitHub Actions API: every job of every
  run, each rounded up to the whole minute. GitHub deletes a public
  repository's runs after at most 90 days (Sources: run retention),
  which is why the ledger is committed monthly.
- **Runner kWh, kgCO2e and USD.** Every runner is taken as the
  {spec['vcpu']} vCPU / {spec['gb']} GB machine a public repository gets, at {int(a['cpu_utilisation'] * 100)}% CPU: {runner_watts('default', a):.1f} W
  with the data-centre overhead (Sources: runners), on a {a['grid_gco2e_per_kwh']} gCO2e/kWh
  grid (Sources: runner grid), priced at the 2-core x64 rate as a rough
  floor (Sources: runner price). Every job of this repository's own
  workflows that checks the tree out also logs its machine, Azure
  region and measured CPU time (the `runner-sample` action), from
  which these factors are recalibrated by hand; GitHub's own
  workflows (Copilot review, CodeQL, Dependabot) keep the assumed
  figures.
- **Copilot.** Reviews are the Copilot code-review workflow's runs.
  Credits are typed into `usage/copilot.csv` from GitHub billing each
  month, note column included, so both are public (Sources: Copilot
  credits).

## Sources

{sources}
"""
    with open(FOOTPRINT, "w") as f:
        f.write(text)
    print(f"render: {FOOTPRINT}, {len(months)} months")


# ---- main -----------------------------------------------------------------


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("steps", nargs="+", choices=["collect-claude", "collect-actions", "render"])
    ap.add_argument("--repo", help="owner/name for collect-actions (make passes USAGE_REPO)")
    ap.add_argument("--match", default="",
                    help=f"project-directory names to count besides {DEFAULT_MATCH}, comma-separated, for a checkout that "
                         "kept an older name (make reads git config motel.usage-match)")
    ap.add_argument("--machine", default="",
                    help=f"this machine's public label: its sessions go in {CLAUDE_DIR}/<machine>.csv (make passes "
                         "USAGE_MACHINE, from git config motel.usage-machine)")
    ap.add_argument("--projects", default=os.environ.get("CLAUDE_PROJECTS", "/claude-projects"),
                    help="the mounted ~/.claude/projects")
    ap.add_argument("--recompute", action="store_true", help="replace committed months even when a row shrank")
    args = ap.parse_args()
    # Both hand-edited files are checked before anything is written, not at render.
    read_copilot()
    factors = load_factors()
    token = os.environ.get("GH_TOKEN")
    if "collect-claude" in args.steps:
        machine, names = args.machine, match_list(args.match)
        check_machine(machine)
    if "collect-actions" in args.steps:
        if not token:
            sys.exit("GH_TOKEN is not set: `make usage` passes `gh auth token`; log in with `gh auth login`")
        if not args.repo:
            sys.exit("--repo owner/name is needed for collect-actions")
    # Collect everything before writing anything, then render whatever
    # was written: a failure leaves the ledger as it was, a success never
    # leaves the page behind its CSVs.
    groups = []
    if "collect-claude" in args.steps:
        fresh = collect_claude(args.projects, names)
        same_machine(machine, fresh)
        groups.append([(claude_path(machine), ["month", "model"], CLAUDE_COLS, fresh)])
    if "collect-actions" in args.steps:
        nruns, njobs = collect_actions(args.repo, token)
        groups.append([(RUNS_CSV, ["month", "workflow"], ["runs"], nruns),
                       (JOBS_CSV, ["month", "workflow", "runner"], JOBS_COLS, njobs)])
    if "collect-claude" in args.steps:
        os.makedirs(CLAUDE_DIR, exist_ok=True)  # here, not before collect-actions: nothing is written until all is collected
    for group in groups:
        merge(group, args.recompute)
    if groups or "render" in args.steps:
        render(factors)


if __name__ == "__main__":
    main()
