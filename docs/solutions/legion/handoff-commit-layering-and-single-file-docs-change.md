---
title: "jj handoff-commit layering and single-file docs changes through the full Legion pipeline"
category: legion
tags:
  - jj
  - legion-worker
  - handoff
  - pr-diff-acceptance
  - review-rounds
date: 2026-09-25
status: active
module: legion-worker
related_issues:
  - "S391694762-1"
  - "sjawhar/legion-smoke#217"
---

# jj handoff-commit layering and single-file docs changes through the full Legion pipeline

A Stage 3 durable-workflow proof (Dispatch S391694762-1, PR #217) took a one-file docs
change — create `SMOKE.md` with one line, then append one line per review round — through
plan → implement → test → review → retro. The change itself was trivial; the friction was
entirely in the jj commit mechanics of recording handoffs and in the tooling around the push.
These are the reusable lessons.

## Keep the product change and the handoff in separate commits, handoff on top — build it that way, don't `jj split` it apart

The completion gate documents `jj split -m "<phase>: record handoff" .legion/<phase>.json`.
That works, but if you first wrote the handoff file into the *same* working-copy commit that
already holds your code change, `jj split` extracts the handoff into the **parent** commit
(below the code), leaving the layering inverted. Worse, a follow-up `jj rebase` to re-order can
hit `Concurrent modification detected, resolving automatically` on the shared clone's single
operation log and silently drop the just-created handoff commit.

The robust recipe that avoids all of it:

1. Make the product change in its own commit; `jj describe` it.
2. `jj new` an empty commit on top.
3. Write the handoff into that empty commit (`handoff_write`).
4. `jj describe -m "<phase>: record handoff"` and `jj bookmark set legion/<KEY> -r @`.

This yields product-commit-below, handoff-commit-on-top deterministically, with no `split` and
no `rebase`. It matches the merge-gate expectation that `.legion/*.json` rides above the product
commit and is stripped before merge.

## `handoff_complete` refuses while the handoff is the working-copy commit

After you `describe` the handoff commit, `@` *is* that commit, so its own addition shows in
`jj status` as `Working copy changes: A .legion/<phase>.json`. `legion handoff_complete` reads
that as "changes not committed" and refuses:

```
.legion/implement.json has changes in the working copy that are not committed
```

Fix: `jj new` once more so the working copy is an empty commit and the handoff sits at `@-`
(committed, not working-copy). Then `handoff_complete` succeeds. Do this on every phase that
writes a handoff.

## A handoff file cannot record the SHA of the commit that carries it

With handoff-on-top, `implement.json.headSha` is the **code/correction commit** (one below),
not the PR head — a file cannot contain the id of the commit that adds it, and editing a parent
to insert a descendant's SHA is circular. `test.json.headSha` legitimately equals the PR head
because it is written by an even-later commit. When reading a handoff, never assume
`headSha == PR head`; the branch tip is one handoff commit higher.

## One appended line per review round keeps AC1 to a single file across arbitrarily many rounds

When the plan pre-authorizes "one additional line per review round", append to the **same**
product file each round. This PR absorbed three `CHANGES_REQUESTED` rounds and stayed a
single-file diff (`SMOKE.md`, `@@ -0,0 +1,4 @@`, zero deletions) with no new issue. Verify each
round with `jj diff --summary` = `M SMOKE.md` (or `A SMOKE.md` on creation) before pushing.

## For a docs change with no repo skill, the PR diff is the acceptance surface — prove it as such

There was no agent skill for a markdown append, so the production-like surface is the live
GitHub PR diff. Prove the criterion by running the acceptance command itself
(`legion gh -- pr diff <n> --repo <owner>/<repo>`), quoting the exact hunk header and the lines,
plus a **negative control** that would fail the criterion (adding a second product file — here
the distinct lowercase `smoke.md` — makes the product-file count 2 and fails AC1). Record that
as both the `E2E (implementer)` and `E2E (tester)` lines; the tester should re-run the
implementer's exact command and keep a per-round proof array so the durable-workflow claim is
auditable across rounds.

## Case-sensitive twin guard

The repo already had a lowercase `smoke.md` from a prior issue; the target was uppercase
`SMOKE.md`. On a case-sensitive filesystem they are distinct paths. State the exact target path,
never edit the twin, and cite the twin in the negative control. `printf '%s\n' '<line>' >> SMOKE.md`
plus a `jj diff --summary` check is enough to keep the edit on the intended path.

## Pane tooling notes

- The suggested `bun -e '...' | legion handoff write` shortcut failed in the worker pane
  (`mise ERROR No version is set for shim: bun`). Fall back to the `legion` tool's
  `handoff_write` with the full JSON `data` object.
- Editing the PR body through a shell here-doc broke on backticks/parentheses in the body
  (`unterminated here document sequence`). Use `legion gh -- pr edit <n> --body-file <path>`,
  and write that body file **outside** `$LEGION_WORKSPACE` — writing it inside dirties the
  already-pushed working-copy (handoff) commit.
