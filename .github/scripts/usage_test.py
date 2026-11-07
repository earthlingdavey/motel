#!/usr/bin/env python3
"""The merge rule and the transcript step of usage.py, run by `make usage-check`: stdlib unittest."""
import contextlib
import io
import json
import os
import shutil
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import usage  # noqa: E402

KEYS, COLS = ["month", "model"], ["calls", "output"]


def rows(*quads):
    return [{"month": m, "model": k, "calls": str(c), "output": str(o)} for m, k, c, o in quads]


class MergeTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)

    def merge(self, old, fresh, recompute=False):
        path = os.path.join(self.dir.name, "a.csv")
        usage.write_csv(path, KEYS + COLS, old)
        with contextlib.redirect_stderr(io.StringIO()) as err:
            usage.merge([(path, KEYS, COLS, fresh)], recompute)
        return usage.read_csv(path), err.getvalue()

    def test_growth_replaces_the_month(self):
        got, err = self.merge(rows(("2026-09", "m", 1, 10)), rows(("2026-09", "m", 2, 20)))
        self.assertEqual(got, rows(("2026-09", "m", 2, 20)))
        self.assertEqual(err, "")

    def test_one_count_shrinking_keeps_the_month(self):
        got, err = self.merge(rows(("2026-09", "m", 1, 10)), rows(("2026-09", "m", 5, 9)))
        self.assertEqual(got, rows(("2026-09", "m", 1, 10)))
        self.assertIn("m shrank or vanished", err)

    def test_vanished_row_keeps_the_month(self):
        old = rows(("2026-09", "a", 1, 1), ("2026-09", "b", 1, 1))
        got, err = self.merge(old, rows(("2026-09", "a", 9, 9)))
        self.assertEqual(got, old)
        self.assertIn("b shrank or vanished", err)

    def test_recompute_accepts_a_shrink(self):
        got, err = self.merge(rows(("2026-09", "m", 5, 5)), rows(("2026-09", "m", 1, 1)), recompute=True)
        self.assertEqual(got, rows(("2026-09", "m", 1, 1)))
        self.assertEqual(err, "")

    def test_absent_month_stays_even_with_recompute(self):
        old = rows(("2026-08", "m", 1, 1), ("2026-09", "m", 1, 1))
        got, err = self.merge(old, rows(("2026-09", "m", 2, 2)), recompute=True)
        self.assertEqual(got, rows(("2026-08", "m", 1, 1), ("2026-09", "m", 2, 2)))
        self.assertIn("2026-08 is no longer in the source", err)

    def test_new_month_is_added_beside_the_old(self):
        got, err = self.merge(rows(("2026-09", "m", 1, 1)), rows(("2026-09", "m", 1, 1), ("2026-10", "m", 3, 3)))
        self.assertEqual(got, rows(("2026-09", "m", 1, 1), ("2026-10", "m", 3, 3)))
        self.assertEqual(err, "")

    def test_a_group_decides_each_month_together(self):
        """A deleted 600 s run replaced by two 60 s runs: runs grow while seconds shrink."""
        runs, jobs = os.path.join(self.dir.name, "runs.csv"), os.path.join(self.dir.name, "jobs.csv")
        rk, jk, jc = ["month", "workflow"], ["month", "workflow", "runner"], ["jobs", "seconds"]
        old_runs = [{"month": "2026-09", "workflow": "ci", "runs": "1"}]
        old_jobs = [{"month": "2026-09", "workflow": "ci", "runner": "x", "jobs": "1", "seconds": "600"}]
        new_runs = [{"month": "2026-09", "workflow": "ci", "runs": "2"}]
        new_jobs = [{"month": "2026-09", "workflow": "ci", "runner": "x", "jobs": "2", "seconds": "120"}]
        usage.write_csv(runs, rk + ["runs"], old_runs)
        usage.write_csv(jobs, jk + jc, old_jobs)
        group = [(runs, rk, ["runs"], new_runs), (jobs, jk, jc, new_jobs)]
        with contextlib.redirect_stderr(io.StringIO()) as err:
            usage.merge(group, False)
        self.assertEqual(usage.read_csv(runs), old_runs)  # grew, but kept with its jobs
        self.assertEqual(usage.read_csv(jobs), old_jobs)
        self.assertIn("runs.csv + ", err.getvalue())
        with contextlib.redirect_stderr(io.StringIO()):
            usage.merge(group, True)
        self.assertEqual(usage.read_csv(runs), new_runs)
        self.assertEqual(usage.read_csv(jobs), new_jobs)


class ClaudeTests(unittest.TestCase):
    """collect-claude: which project directories count, and whose file the counts land in."""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        cwd = os.getcwd()
        os.chdir(self.dir.name)
        self.addCleanup(os.chdir, cwd)
        self.root = os.path.join(self.dir.name, "projects")

    def transcript(self, project, calls):
        d = os.path.join(self.root, project)
        os.makedirs(d)
        with open(os.path.join(d, "s.jsonl"), "w") as f:
            for i in range(calls):
                f.write(json.dumps({"timestamp": "2026-09-01T00:00:00Z", "requestId": f"{project}{i}", "message": {
                    "id": f"{project}{i}", "model": "claude-x", "usage": {"input_tokens": 1, "output_tokens": 2}}}) + "\n")

    def test_match_takes_several_names(self):
        self.transcript("-x-motel", 1)
        self.transcript("-x-oldname", 2)
        self.transcript("-x-other", 4)
        for match in ("motel,oldname", "motel, oldname", ",oldname , motel,"):
            with contextlib.redirect_stdout(io.StringIO()) as out:
                got = usage.collect_claude(self.root, usage.match_names(match))
            self.assertEqual([(r["month"], r["model"], r["calls"]) for r in got], [("2026-09", "claude-x", "3")])
            self.assertIn("not counted: 1 other project directory (see --match;", out.getvalue())
        with contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()) as err:
            usage.collect_claude(self.root, ["motel", "-x-", "typo"])  # the default alone matching nothing is no news
        self.assertNotIn("not counted", out.getvalue())
        self.assertEqual(err.getvalue(), "warning: no project directory here has 'typo' in its name\n")
        for names in (["nothing"], []):
            with self.assertRaises(SystemExit) as cm:
                usage.collect_claude(self.root, names)
            if names:
                self.assertIn("; 3 other project directories (see --match;", str(cm.exception))
        os.makedirs(os.path.join(self.dir.name, "none"))
        with self.assertRaises(SystemExit) as cm:
            usage.collect_claude(os.path.join(self.dir.name, "none"), ["motel"])
        self.assertIn("0 other project directories (see --match; none at all means the mount is empty", str(cm.exception))

    def test_machine_names_a_public_file(self):
        self.assertEqual(usage.claude_path("laptop-1"), "usage/claude/laptop-1.csv")
        self.assertEqual(usage.label("laptop-1"), "laptop-1")
        for bad in ("", "Laptop", "a b", "a.b", "-a", "../x"):
            with self.assertRaises(SystemExit):
                usage.label(bad)

    def test_a_new_label_beside_old_files_warns(self):
        """A label the machine has not used before: the warning is the guard against a typo for one it has."""
        os.makedirs(usage.CLAUDE_DIR)
        with contextlib.redirect_stderr(io.StringIO()) as err:
            usage.check_machine("a")  # the first file: nothing to compare with
        self.assertEqual(err.getvalue(), "")
        usage.write_csv(usage.claude_path("a"), KEYS + COLS, [])
        with contextlib.redirect_stderr(io.StringIO()) as err:
            usage.check_machine("a")
        self.assertEqual(err.getvalue(), "")
        with contextlib.redirect_stderr(io.StringIO()) as err:
            usage.check_machine("a-1")
        self.assertIn("a-1 is a new machine (files so far: a)", err.getvalue())
        for stray in ("Not-A-Label.csv", "dir.csv"):  # summed, and no --machine could ever name it
            if stray.startswith("dir"):
                os.makedirs(os.path.join(usage.CLAUDE_DIR, stray))
            else:
                usage.write_csv(os.path.join(usage.CLAUDE_DIR, stray), KEYS + COLS, [])
            with self.assertRaises(SystemExit) as cm:
                usage.machines()
            self.assertIn(f"{stray}: not a machine file", str(cm.exception))
            shutil.rmtree(os.path.join(usage.CLAUDE_DIR, stray), ignore_errors=True)
            if os.path.exists(os.path.join(usage.CLAUDE_DIR, stray)):
                os.remove(os.path.join(usage.CLAUDE_DIR, stray))

    def test_the_default_is_always_counted(self):
        self.assertEqual(usage.match_list(""), ["motel"])
        self.assertEqual(usage.match_list("motel"), ["motel"])
        self.assertEqual(usage.match_list(" y, x ,motel"), ["motel", "y", "x"])
        self.assertEqual(usage.match_list("x,x,motel,x"), ["motel", "x"])  # once each, so one warning each
        for empty in (",", "   ", " , "):  # a flag that names nothing must not pass as "none given"
            with self.assertRaises(SystemExit) as cm:
                usage.match_list(empty)
            self.assertIn("names nothing", str(cm.exception))

    def test_an_identical_row_names_the_earlier_label(self):
        """The same machine run under a second label: its rows already sit in the first file."""
        os.makedirs(usage.CLAUDE_DIR)
        usage.write_csv(usage.claude_path("a"), KEYS + COLS, rows(("2026-09", "m", 1, 10), ("2026-10", "m", 2, 20)))

        def warned(machine, fresh):
            with contextlib.redirect_stderr(io.StringIO()) as err:
                usage.same_machine(machine, fresh)
            return err.getvalue()

        self.assertEqual(warned("a", rows(("2026-09", "m", 1, 10))), "")  # its own file
        self.assertEqual(warned("b", rows(("2026-09", "m", 1, 11))), "")  # another machine
        self.assertEqual(warned("b", []), "")
        got = warned("b", rows(("2026-09", "m", 1, 10), ("2026-10", "m", 3, 30)))
        self.assertIn("usage/claude/a.csv holds a row identical", got)
        # Both ways out undo what this run goes on to write: b's file.
        self.assertIn("`git mv -f usage/claude/a.csv usage/claude/b.csv` and run again", got)
        self.assertIn("`rm usage/claude/b.csv` and run with --machine a", got)
        usage.write_csv(usage.claude_path("b"), KEYS + COLS, rows(("2026-08", "m", 1, 1)))  # b is established:
        got = warned("b", rows(("2026-09", "m", 1, 10)))
        self.assertIn("summed twice; a single small row can also coincide", got)  # no advice to remove its file
        self.assertNotIn("rm ", got)
        usage.write_csv(os.path.join(usage.CLAUDE_DIR, "Bad.csv"), KEYS + COLS, [])
        with self.assertRaises(SystemExit) as cm:  # the setup message is not displaced by a stray file's
            usage.label("")
        self.assertIn("git config --global motel.usage-machine <label>` (make reads it; files so far: a, b)", str(cm.exception))
        self.assertIn("without --global, if your ~/.gitconfig is synced", str(cm.exception))
        self.assertEqual(usage.match_names(",oldname , motel,"), ["oldname", "motel"])

    def test_every_machine_file_is_read(self):
        os.makedirs(usage.CLAUDE_DIR)
        with contextlib.redirect_stderr(io.StringIO()):
            usage.write_csv(usage.claude_path("a"), KEYS + COLS, rows(("2026-09", "m", 1, 10)))
            usage.write_csv(usage.claude_path("b"), KEYS + COLS, rows(("2026-09", "m", 2, 20), ("2026-10", "m", 3, 30)))
        self.assertEqual(usage.machines(), ["a", "b"])
        self.assertEqual(usage.read_claude(),
                         rows(("2026-09", "m", 1, 10), ("2026-09", "m", 2, 20), ("2026-10", "m", 3, 30)))


class MainTests(unittest.TestCase):
    """collect-claude end to end: what a failed run leaves alone and a good one writes, in that order."""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        cwd = os.getcwd()
        os.chdir(self.dir.name)
        self.addCleanup(os.chdir, cwd)
        os.makedirs(usage.CLAUDE_DIR)
        os.makedirs("docs")
        repo = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
        shutil.copy(os.path.join(repo, usage.FACTORS), usage.FACTORS)  # the real one: render needs its whole shape
        with open(usage.COPILOT_CSV, "w") as f:
            f.write("month,ai_credits_usd,note\n")
        self.root = os.path.join(self.dir.name, "projects")
        os.makedirs(os.path.join(self.root, "-x-motel"))
        with open(os.path.join(self.root, "-x-motel", "s.jsonl"), "w") as f:
            f.write(json.dumps({"timestamp": "2026-09-01T00:00:00Z", "requestId": "r", "message": {
                "id": "m", "model": "claude-opus-5-5", "usage": {"input_tokens": 1, "output_tokens": 2}}}) + "\n")

    def run_main(self, *argv):
        self.addCleanup(setattr, sys, "argv", sys.argv)
        sys.argv = ["usage.py", "collect-claude", "--projects", self.root, *argv]
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()) as err:
            try:
                usage.main()
            except SystemExit as e:
                return str(e), err.getvalue()
        return None, err.getvalue()

    def test_a_failed_run_writes_nothing_and_a_good_one_everything(self):
        empty = os.path.join(self.dir.name, "empty")
        os.makedirs(empty)
        exit_, _ = self.run_main("--machine", "a", "--projects", empty)  # the last --projects wins
        self.assertIn("no project directory", exit_)
        self.assertFalse(os.path.exists(usage.claude_path("a")))
        self.assertFalse(os.path.exists(usage.FOOTPRINT))
        exit_, _ = self.run_main("--machine", "")
        self.assertIn("git config --global motel.usage-machine", exit_)
        exit_, err = self.run_main("--machine", "a", "--match=-x-")  # a value that starts with a hyphen, as make passes it
        self.assertIsNone(exit_, exit_)
        self.assertEqual(err, "")
        self.assertEqual([(r["month"], r["calls"]) for r in usage.read_csv(usage.claude_path("a"))], [("2026-09", "1")])
        with open(usage.FOOTPRINT) as f:
            self.assertIn("| 2026-09 | 1 |", f.read())
        shutil.copy(usage.claude_path("a"), usage.claude_path("c"))  # the same machine under a second label...
        exit_, err = self.run_main("--machine", "b", "--match", "")
        self.assertIsNone(exit_, exit_)
        self.assertIn("b is a new machine (files so far: a, c)", err)  # ...gets both warnings
        self.assertIn("usage/claude/a.csv holds a row identical", err)
        with open(usage.FOOTPRINT) as f:  # and the page is the sum of the machine files, not the last one's
            self.assertIn("| 2026-09 | 3 |", f.read())


class HandEditedFileTests(unittest.TestCase):
    """copilot.csv and factors.json are typed by hand; a bad edit must stop a run, not publish."""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        cwd = os.getcwd()
        os.chdir(self.dir.name)
        self.addCleanup(os.chdir, cwd)
        os.makedirs("usage")

    def copilot(self, body):
        with open(usage.COPILOT_CSV, "w") as f:
            f.write("month,ai_credits_usd,note\n" + body)
        return usage.read_copilot()

    def test_numeric_and_blank_credits(self):
        self.assertEqual(self.copilot("2026-09,12.5,first bill\n2026-10,,\n"), {"2026-09": 12.5, "2026-10": None})

    def test_duplicate_month_is_refused(self):
        for body in ("2026-09,10,\n2026-09,20,\n", "2026-09,10,\n2026-09,,\n"):
            with self.assertRaises(SystemExit):
                self.copilot(body)

    def test_bad_credit_values_are_refused(self):
        for v in ("abc", "-1", "nan", "inf", "$12"):
            with self.assertRaises(SystemExit):
                self.copilot(f"2026-09,{v},\n")

    def factors(self, text):
        with open(usage.FACTORS, "w") as f:
            f.write(text)
        return usage.load_factors()

    def test_factors_shape_is_checked(self):
        with self.assertRaises(SystemExit):
            self.factors("{not json")
        with self.assertRaises(SystemExit):
            self.factors('{"_sources": {}, "prices_usd_per_mtok": {}, "claude_gco2e_per_mtok": {}}')
        ok = '{"_sources": {}, "prices_usd_per_mtok": {}, "claude_gco2e_per_mtok": {}, "actions": {}}'
        self.assertEqual(self.factors(ok)["actions"], {})


if __name__ == "__main__":
    unittest.main()
