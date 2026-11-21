#!/usr/bin/env python3
"""NOT TO MERGE. Prints each test's time from `go test -json` output.

usage: zz_times.py test.json <tree> <runner> <rep>

Lines, one fact each, easy to grep out of a job's log:
  PKG  <tree> <runner> <rep> <package> <seconds> <pass|fail>
  TEST <tree> <runner> <rep> <package> <test> <seconds> <pass|fail>
TEST lines are top-level tests only (no subtests), for the client's
screen packages, slowest first.
"""
import json
import sys

path, tree, runner, rep = sys.argv[1:5]
PREFIX = "github.com/smallhoursorg/motel/"
pkgs = {}
tests = {}
with open(path, encoding="utf-8", errors="replace") as f:
    for line in f:
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            e = json.loads(line)
        except ValueError:
            continue
        act = e.get("Action")
        if act not in ("pass", "fail"):
            continue
        pkg = e.get("Package", "").removeprefix(PREFIX)
        t = e.get("Test")
        if t is None:
            pkgs[pkg] = (e.get("Elapsed", 0.0), act)
        elif "/" not in t:
            tests[(pkg, t)] = (e.get("Elapsed", 0.0), act)

for pkg, (sec, act) in sorted(pkgs.items(), key=lambda kv: -kv[1][0]):
    print(f"PKG  {tree} {runner} {rep} {pkg} {sec:.1f} {act}")

for want in ("client/internal/tui", "client/internal/tui/door", "client/internal/tui/door/rowbreak", "client/internal/tui/model"):
    rows = [(sec, t, act) for (pkg, t), (sec, act) in tests.items() if pkg == want]
    rows.sort(reverse=True)
    total = sum(r[0] for r in rows)
    print(f"SUM  {tree} {runner} {rep} {want} {total:.1f} over {len(rows)} top-level tests")
    for sec, t, act in rows[:60]:
        print(f"TEST {tree} {runner} {rep} {want} {t} {sec:.2f} {act}")
