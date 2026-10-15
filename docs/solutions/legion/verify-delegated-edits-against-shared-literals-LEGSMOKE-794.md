---
title: "Give a delegated worker the reference bytes you will verify against, then verify its diff yourself before you split"
category: legion
tags:
  - deep-worker
  - delegation
  - jj
  - implementer
date: 2026-10-15
status: active
module: implementer orchestration
related_issues:
  - "LEGSMOKE-794"
  - "sjawhar/legion-smoke#575"
---

# Give a delegated worker the reference bytes you will verify against, then verify its diff yourself before you split

Build the reference files first (`printf '%s\n' '<line>' > /tmp/l1`) and hand the worker acceptance checks that `cmp` the target against those exact paths. Your own re-check is then the same command on the same reference, so a pass cannot be an artefact of two different readings of the spec.

Bound the worker's task to one file and name every forbidden action: no `jj new`/`split`/`describe`/`commit`/`abandon`, no `git`, no push, no GitHub write; tell it `jj status` is the one allowed jj command. A worker that snapshots the working copy by running `jj status` has mutated nothing; a worker that runs `jj new` has moved the change you were about to split.

Before `jj split`, run `jj -R "$LEGION_WORKSPACE" diff --git` and read the whole hunk list: it must touch only the file in scope, and the hunk must be the shape you asked for (`@@ -0,0 +1,1 @@` for a new one-line file, `@@ -1,1 +1,2 @@` for an append, a one-file `deleted file mode` for a removal). Then `cmp` the file against the reference yourself. Only then split, with the file path as the split's argument so the commit holds nothing else.

Delegate the removal of a foreign file the same way: the worker runs `rm <path>` and reports `jj status` showing only `D <path>`; you confirm the diff is a single deletion before committing the forward cure.

## Evidence

Pull request `sjawhar/legion-smoke#575`. Three delegated edits, each verified before its commit: `d736b951` (worker created `smoke/LEGSMOKE-794.md` with `printf`; orchestrator re-ran `wc -c` 56, `tail -c1 | od` → ` 0a`, `cmp … /tmp/l1` exit 0, diff one `new file` hunk), `6faa4f8d` (worker ran `rm .fail-me`; diff showed only `deleted file mode 100644 .fail-me`, marker still `cmp` exit 0 against `/tmp/l1`), `ba4777f0` (worker appended L2 with `printf >>`; diff one hunk `@@ -1,1 +1,2 @@`, `head -c 56 | cmp - /tmp/l1` and `tail -c +57 | cmp - /tmp/l2` both exit 0, whole file `cmp` against `/tmp/final` exit 0). Each worker's acceptance list named the same `/tmp/l1`, `/tmp/l2`, `/tmp/final` the orchestrator had built, and each report's numbers matched the re-run.
