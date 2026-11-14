#!/usr/bin/env python3
"""The rule of ci-changes.py, run by `make ci-changes-check`: stdlib
unittest, from the repository root."""
import contextlib
import importlib.util
import io
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
spec = importlib.util.spec_from_file_location("ci_changes", os.path.join(HERE, "ci-changes.py"))
cc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cc)


def run(paths):
    out, err = io.StringIO(), io.StringIO()
    old, sys.stdin = sys.stdin, io.StringIO("".join(p + "\n" for p in paths))
    try:
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            cc.main()
    finally:
        sys.stdin = old
    return out.getvalue()


class Rule(unittest.TestCase):
    def test_light(self):
        for paths in (
            ["README.md"],
            ["docs/first-deploy.md", "liveswap/DESIGN-liveswap.md"],
            ["usage/runs.csv", "usage/claude/lima16.csv", "usage/factors.json", "docs/footprint.md"],
        ):
            with self.subTest(paths=paths):
                self.assertEqual(run(paths), "heavy=false\n")

    def test_heavy(self):
        for paths in (
            ["liveswap/app.go"],
            ["docs/first-deploy.md", "Makefile"],
            ["docs/backups.md"],
            ["box/DESIGN-box.md"],
            ["examples/node/README.md"],
            ["liveswap/testdata/notes.md"],
            [".github/workflows/ci.yml"],
            ["usage.md.go"],
            ["usage/broken_test.go"],  # code under usage/ is built by ./...
            ["usage/runs.csv", "usage/collect.py"],
            [],  # a diff that read nothing is a doubt, not a docs PR
        ):
            with self.subTest(paths=paths):
                self.assertEqual(run(paths), "heavy=true\n")

    def test_read_names_files_that_exist(self):
        # A renamed file leaves its old name here and its new one
        # unlisted, so the reader's file would turn light: fail instead.
        for path in sorted(cc.READ):
            with self.subTest(path=path):
                self.assertTrue(os.path.isfile(os.path.join(ROOT, path)), f"{path} is in READ but not in the tree")


if __name__ == "__main__":
    unittest.main()
