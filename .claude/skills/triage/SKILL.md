---
name: triage
description: Decide whether a finding in motel gets fixed — from Copilot, /code-review, a test or the field. Scores the failure (severity × occurrence × detection) and the fix (size, risk), applies the rule, and records what is accepted in the subsystem's design doc.
---

# Triage: which findings get fixed

Without a rule every edge case looks mandatory, and each guard added
for a multi-fault case becomes a new branch for the next review to
probe. So every finding is scored on what the failure does and on what
its fix costs, and the rule below decides.

## 1. Find the subsystem's examples

The scales below are general. Each subsystem's design doc gives its
own examples for each level, and keeps its accepted residuals next to
the behaviour they describe. Read only the one the finding is in:

| Subsystem | Examples, accepted residuals, owed fixes |
|---|---|
| box | `box/DESIGN-box.md`, "Triage: which failures to defend" |
| packaging (`packaging/`, the `.deb`'s maintainer scripts) | `packaging/DESIGN-packaging.md`, "Triage" |
| liveswap | `liveswap/DESIGN-liveswap.md`, "Accepted residuals" and the **Owed** table after it (scored rows; no per-level examples yet, so the general scales) |
| backups (`backups/`, and `cmd/motel/backupcheck.go`, the validate and reload gate) | `backups/README.md`, "Accepted residuals" (scored rows; general scales) |
| notify (`notify/`, and liveswap's `Notifier` in `liveswap/lifecycle.go`) | `notify/DESIGN-notify.md`, "Triage": examples, "Accepted residuals" and "Owed" |
| usage ledger (`.github/scripts/usage.py`, `usage/`, `docs/footprint.md`) | the docstring of `usage.py`, "Accepted residuals" (scored rows; general scales) |

A subsystem with no examples yet is scored on the general scales
alone. Add its examples to its design doc when a finding first needs
them, not before.

## 2. Score the failure

Only a failure is scored. Two kinds of finding are not:

- **A suggestion that names no failure** (nothing is wrong, something
  could be nicer) is a feature. It is neither scored nor accepted; the
  owner weighs it as a feature.
- **A comment or design-doc sentence.** The scales measure what a
  running system does, and a comment neither occurs nor is detected.
  One that says something untrue about the code is corrected in the
  change that found it, with no row; imprecise wording stays as it is.
  Text a user acts on (a message, a tutorial step, a catalogue entry)
  is a tool's answer and is scored as one.

FMEA's three axes:

| Axis | 5 | 4 | 3 | 2 | 1 |
|---|---|---|---|---|---|
| **Severity** — the effect if it happens | silently breaks what the subsystem exists to guarantee | stuck until someone at the console fixes it | a wrong answer someone trusts: a verdict, or a doc or tool that leaves someone in a wrong state they believe is right | a wrong message; red where green was due | logging only |
| **Occurrence** — what must go wrong | normal operation | one common fault: a crash, a restart, a failed reload | one rare fault: `EIO`, a full disk, a clock step | two independent faults | three or more, or state only root can write found corrupted |
| **Detection** — how it surfaces | silently | — | a wrong but plausible answer | — | loudly, naming the cause |

Input an operator may write (a Caddyfile line, a path, a name) is not
a fault. A finding it triggers scores occurrence 5 however unlikely
the input, and detection carries it: a line dropped silently scores 5,
one refused and named scores 1. How likely the input is stays out of
the score, since scorers would each guess it differently.

A hostile motel uid (T5 in `DESIGN-threat-model.md`) is scored by
what it gains: an effect it could already have by other means scores
severity 1.

## 3. Score the fix

Two axes. Cost is never multiplied into severity × occurrence ×
detection: that product measures the failure, and these measure the
fix.

| Cost | 1 | 2 | 3 |
|---|---|---|---|
| **Size** — what the fix adds to the code that runs | tightens lines already there: no new branch, state or file | one new branch, or about ten lines | a new state, lock or file, or more |
| **Risk** — what the fix may break | the normal path behaves as before, and a new test proves the fix | anything between: the normal path changes under a test, new or existing, or it behaves as before with no new test proving the fix | the normal path changes, and no test covers it |

A fix to text a user acts on scores size 1, and risk 2 unless a test
checks the text.

## 4. Apply the rule

In order:

1. **Severity 5 is fixed**, whatever its occurrence.
2. **Single faults are fixed** (occurrence 3 or more): under any one
   fault the system must end correct, or red and named — the
   single-fault criterion of safety engineering.
3. Anything else is fixed if severity × occurrence × detection is 20
   or more, or if its fix scores 1 for both size and risk: a fix that
   cheap and that safe is made whatever the finding scores. Severity 1
   never clears by its product (1 × 5 × 5 is 25): only the cheap fix
   reaches it. Otherwise the finding is **accepted**.
4. **Stop** when the next guard costs more than it buys (ALARP): a fix
   that adds a branch for a multi-fault case is itself a finding to
   weigh, since every branch is one more state the next fault can
   reach. The cost axes are how that cost is weighed. Rules 1 and 2
   hold whatever a fix costs: the cost changes how they are fixed, not
   whether.

## 5. Report and record

- **Score before building.** An item from an issue's backlog is scored
  before a branch is cut. One that scores accept is closed on the
  issue with its scores, and recorded as below. A measurement a fix
  would rest on, such as memory or time, is taken before the fix.
- Give each finding one line: S, O, D, their product, size, risk, and
  the outcome — **fix**, **accept**, or **owed** (to be fixed, but not
  in this change).
- **Fix:** keep the fix as small as its score, and add the test that
  proves it.
- **Accept:** add a row to the subsystem's accepted-residuals table,
  with its scores and why. Severity 1 gets no row: the change's triage
  table holds it. Reply on the review thread with the same scores.
- **Owed:** add it to the subsystem's owed fixes, with its scores and
  where the fix will land.
- **Flag what the scores cannot see.** A fix or an owed fix gets a
  note on its line, for the person who merges, when either holds:
  - **One-way:** the way back does not undo it (the `.deb` a box
    upgraded from, `docs/upgrading.md`; an app's rollback to a release
    still on disk: the newest `keep`, five by default, and the running
    one), or nothing does: it deletes what nothing else holds, or
    publishes.
  - **Wide:** it changes code every app's requests or deploys run
    through, or reaches past the box (off-site backups, a workflow
    users copy).

  The note leaves the outcome as it is. A one-way fix waits for that
  person's go before it merges.
- When asking a reviewer such as Copilot, state the bar in the PR
  body: it cannot read this skill.
