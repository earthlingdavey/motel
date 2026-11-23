---
name: build-loop
description: How a feature or a ticket's fix is built in motel, from the ticket to a pull request the owner can merge. An Opus subagent builds it, reviews its own branch with /code-review and the triage skill, opens the pull request and answers Copilot's reviews; the coordinator checks the result and reports. Use it whenever the owner asks to build, add or implement something, hands tickets to subagents, or says "the usual loop", "self-review and triage" or "Opus subagents", and before telling the owner that a pull request is ready.
---

# Build loop

A change reaches the owner only when someone other than its author
has read it, every finding has gone through the `triage` rule, and
the reviewing has stopped at a cap. Two roles do it:

- **Coordinator:** the session the owner talks to. It plans, briefs,
  checks and reports. It writes no code: a fix made by the session
  that runs the loop has no reader but its author.
- **Builder:** an Opus subagent (`model: opus`), one per ticket, in a
  worktree of its own cut from a freshly fetched `upstream/main`. It
  builds, reviews its own work, opens the pull request and answers
  Copilot.

Caps, unless the owner names others: 4 self-review rounds, 3 Copilot
rounds at `balanced`, 1 final round on a model other than the
builder's.

## Before any code (coordinator)

1. Read what main already has, and ask the owner what other sessions
   are working on: that is out of scope.
2. Say what will be built. Where there is a real choice, give the
   options in plain words (what a person would see with each, and
   what each costs) and recommend one. Code starts when the owner
   has chosen.
3. One pull request for each independent change, each complete
   without the others.

## The brief (coordinator)

A builder knows only its brief. It carries the ticket, the owner's
choices, what is out of scope, the caps, how to push and to request
Copilot on this machine, and what to send back: the pull request and
its head, each round's triage table, what was not run, and anything
unchecked marked as unchecked. It tells the builder to read this
file whole, from its own worktree.

The builder does not hand back while CI or a review is pending: it
waits, answers the review, and reports with CI green on the head. It
stops to ask only for a choice the owner did not make, an accept
over the bar, or a change to who may deploy or sign in.

## Build and self-review (builder)

1. The change and its tests land together. A test for a fix fails
   without the fix: run it once that way.
2. Run what `AGENTS.md` says: the tests of the modules touched, and
   lint before every push. CI runs the e2e lanes.
3. A round is: commit, then `/code-review high <base>..<branch>`.
   `<base>` is the commit id `git merge-base upstream/main <branch>`
   prints, and `<branch>` the branch by name: a name such as
   `upstream/main` can move, and `HEAD` is the main checkout's.
4. Score every finding with the triage rule. Read
   `.claude/skills/triage/SKILL.md` whole, from the branch's own
   worktree, in the same turn: scores from an earlier reading have
   come out wrong. Fix what the rule says to fix, no larger than its
   score, and record the rest as the rule says.
5. Go round again while a round made fixes, up to the cap. Stop when
   a round finds nothing to fix. At the cap, what the rule says to
   fix is still fixed. When a round's findings are faults the last
   round's fixes brought in, the last section comes first.

## The pull request and Copilot (builder)

1. With tests and lint green, open an ordinary pull request, never a
   draft. A small fix is opened as soon as it is green, and reviewed
   while Copilot reads.
2. The description says what changes and why. It holds each round's
   triage table, the review bar in words (Copilot cannot read the
   triage skill), any one-way or wide flag, and "For the owner to
   confirm": each choice the builder made, with its alternative.
3. Request Copilot, naming the effort every time:

   ```
   gh api -X POST repos/smallhoursorg/motel/pulls/N/copilot-code-review -f review_effort=balanced
   ```

4. Score its findings as above. They can be in threads, in the
   review's body, or in plain comments. Fix, reply on each thread
   with the scores, and request again only after a fix. Stop when a
   review that has seen every fix leads to no fix, or at the cap.
5. If Copilot cannot review (its quota is spent), say so, and the
   coordinator tells the owner. With the owner's word a fresh
   subagent on another model stands in. It is given the range and
   what the change is meant to do, not the earlier findings, and it
   reviews without changing anything. The coordinator scores its
   findings and sends the builder the fixes, for as many rounds as
   the owner allows.

## Before saying it is ready (coordinator)

1. Check the builder's scores against the rule, read as above. A
   fact in a row comes from code someone read or a command someone
   ran.
2. If anything was pushed after the builder's last self-review round,
   run one more `/code-review high` over the builder's range, on a
   model other than the builder's (a fresh subagent, when the
   coordinator runs on the same one). Score the findings. Send the
   builder what fails, not a design for the fix, and read each
   fix's diff.
3. Run once what a person will see: the TUI's `--demo` in a real
   terminal, or the command itself.
4. On GitHub: CI green on the head commit, mergeable, not a draft,
   and the plain comments read.
5. Report the head, the rounds and what each found, every accept over
   the bar, the flags, what was not run, and what last read each
   part of the change. The owner merges.

## Changes after the rounds

- **Scope the owner adds** goes in a pull request of its own, or in
  commits of its own that get one self-review round and one more
  Copilot round before the pull request is called ready.
- **A merge of main whose conflict was resolved by hand** is a fix
  like any other: someone reads it.
- **A stacked pull request** is cut from its base's branch and names
  it in its first paragraph. Its `<base>` for a review is the
  merge-base with that branch. It merges its base in, never rebases.

## When a fix brings a fault in

Score the first finding again, the one the fix was for. If it was
never over the bar, take the fix back. If it was, fix once more. If
the next reading again finds faults from fixes, stop and ask the
owner, in plain words: go back to the last version a review read
whole, or go on. Going back is `git revert`, and a check that the
result equals that version plus what is kept. An accept over the bar
is the owner's to make.
