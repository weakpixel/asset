---
name: implement
description: Use when the user selects a plan to implement; execute its steps, verify them, and track progress in the plan file.
argument-hint: [plan-slug or path]
disable-model-invocation: true
model: sonnet
---

# /implement: carry out a plan

Selected plan: $ARGUMENTS

## 1. Select the plan

Plans are in `plan/`, or in the plan folder the project instructions or project memory name. Resolve the plan in this order:
1. If `$ARGUMENTS` is a path or a slug, use `plan/<slug>.plan.md` (accept the slug with or without `.plan.md`).
2. Otherwise, if the file open or selected in the IDE is under `plan/`, use it.
3. Otherwise, if exactly one plan has `Status: ready` or `in-progress`, use it.
4. Otherwise, list the candidates with their status and ask the user to pick one.

Say which plan you selected before you start.

## 2. Check that it can start

- `Status: draft` or open questions: show the questions and ask the user to answer them (or to confirm that you should proceed with the recommended answers). Record the answers in **Decisions** and set the status to `ready`.
- `Status: done`: ask whether to reopen it or plan follow-up work with `/plan`.
- `Status: in-progress`: resume at the first unchecked step. First check that the checked steps really hold (their verify commands pass), because an earlier session may have stopped midway.

Load the skills listed under **Context → Skills**, and read the spec sections it links.

## 3. Execute

Set `Status: in-progress`. Then for each unchecked step, in order:
1. Do the step, following CLAUDE.md and the loaded skills.
2. Run its verify command or test.
3. Tick the box in the plan file only when the verification passes.

Rules:
- **Keep examples minimal.** An example is an `Example` function in an `example_test.go`, not a tour in the docs.
- **Stay within scope.** Don't do items under **Out of scope**. Note unrelated problems you find under **Implementation log → Follow-ups** instead of fixing them.
- **When the plan turns out to be wrong** (a step is impossible, the spec conflicts, a better approach appears): make small, obvious adjustments and log them as a deviation. For anything that changes scope, the public API or a recorded decision, stop and ask the user, then update the plan before continuing.
- **When blocked** (missing dependency, needs network, failing test you can't fix): set `Status: blocked`, log the reason and what you tried, and report to the user.
- Update the plan file as you go, not only at the end, so an interrupted session can resume.
- Don't commit unless the user asks.

## 4. Finish

- Run `make all` and check every **Acceptance criteria** box against real output.
- Make sure the decisions taken are written into `doc/` (the plan's Decisions say where).
- Append a dated entry to **Implementation log**: what was done, deviations, and follow-ups.
- Set `Status: done` only if every step and every acceptance criterion is checked. Otherwise leave it `in-progress` and say what's left.
- Report to the user:
  - what changed, with file links;
  - test and benchmark results (including any failures, with output);
  - deviations from the plan;
  - follow-ups.
