---
name: goals
description: Write a goal (an agent's brief) or a result for the sprawl goal ledger. Use before spawn, create_goal, request_rework or report_result, in place of an issue tracker.
user-invocable: true
---

# Goals

The goal ledger (the event log, `SPRAWL_DB_DSN`) is the work record. There is no
separate issue tracker: a goal's text IS the brief, and the result closes it.

## Is it a goal?

Not if it takes fewer than about 5 commands. Do it yourself, or say it in one
`send_message`.

## A goal: three parts, about 600 characters at most

```
Goal: <one imperative sentence: the outcome, not the steps>
Files: <paths, functions, and every fact the agent would otherwise go look up:
        numbers, hosts, commands, decisions already made>
Done when: <checks someone else can run: a command and its expected output,
            a test name, a value>
Out of scope: <optional, one line>
```

**The facts go in the goal.** Never write "read DIRECTIVES.md", "see the REPORT",
"check the issue": every file you point at costs the agent context, and you have
already read it. If the facts don't fit in ~600 characters, the goal is too big:
split it.

Where it goes: `spawn({prompt})` for building work; `create_goal({text})` for
`research` / `bug_investigation`; `request_rework({reason})` uses the same three
parts, with Goal = what was wrong.

## A result: about 1k characters at most

Outcome (success / failure / blocked), then: what changed (commits, files), the
evidence that "Done when" holds (command plus trimmed output), and what is left
open. No story of how you got there. It goes in `report_result({summary})`; for
spawned work, write it to `.sprawl/agents/<you>/findings/result.md` and send your
parent a ≤300-character message pointing at it.

## One goal per agent

A dormant agent resumes the same Claude session, so its context only grows.
Give each new goal to a freshly spawned agent rather than messaging an old one
the next task; re-message only to fix the goal it is still working on. Retire
an agent once its goal is merged.

## Don't

- Don't polish goals: no labels, priorities or statuses. Write it, spawn, move on.
- Don't paste logs or diffs into a goal or result; name the file and the line.
