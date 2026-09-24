---
title: "Small single-file changes through the jj-workspace + GitHub-PR Legion pipeline"
category: legion
tags:
  - jj-workspace
  - legion-pipeline
  - handoff-commit-ordering
  - head-sha-citation
  - review-rounds
date: 2026-09-24
status: active
module: legion-worker
related_issues:
  - "S392924187-1"
  - "sjawhar/legion-smoke#168"
---

# Small single-file changes through the jj-workspace + GitHub-PR Legion pipeline

A phase worker taking a small single-file change (here: appending marker lines to the
shared root `smoke.txt`) through the full Legion pipeline — plan → implement → test →
review rounds → retro — hits the same handful of jj-ordering and evidence pitfalls every
time. These are the reusable ones, grounded in PR #168 / S392924187-1.

## Keep the handoff commit *above* the implementation commit so the cited SHA is stable

Evidence lines cite the commit the change lands in. If the `implement: record handoff`
commit sits *below* the code commit, the code commit's SHA is recomputed every time the
handoff content or its ancestors change, so any SHA you cite is immediately stale — a
circular dependency (you cannot cite the tip inside a file that is part of the tip).

`jj split -m "<phase>: record handoff" .legion/<phase>.json` places the *selected* file
into a new commit in the **parent** slot and leaves the remaining changes as the working-copy
child. For a handoff split that inverts the desired order: it puts the handoff *below* the
code change and rewrites the code commit's SHA.

Two ways to get `impl → handoff(tip)`:

- **Preferred — never split.** Make the code change in `@`, `jj describe` it as the impl
  commit, `jj new` an empty child, `handoff_write` into that child (so it contains only
  `.legion/<phase>.json`), `jj describe` it as the handoff commit, then `jj new` once more
  for a clean empty working copy before setting the bookmark to the handoff commit. The impl
  commit's SHA never moves, so the `headSha` you cite (e.g. `2d456abb`) stays valid.
- **If you already split**, reorder with `jj rebase -r <handoff> -A <impl>` so the impl
  commit becomes the stable ancestor, then read the impl SHA and correct `headSha` in the
  handoff before pushing.

The pushed branch head (`4c9ce5b1`) is legitimately different from the cited implementation
SHA (`2d456abb`): cite the implementation commit for `headSha`, not the branch tip.

## Scope "no other file changed" to the product fileset

The PR diff always contains the `.legion/*` handoffs (`plan.json`, `implement.json`,
`test.json`). They are exempt process artifacts, not product changes — the same
`~(.legion | docs/solutions)` fileset the unchanged-diff fingerprint uses. When asserting
the change touches nothing else, name those paths as exempt rather than treating them as
violations; the product claim is about `smoke.txt` alone.

## Treat a shared file as append-only and idempotent

`smoke.txt` already carries other issues' blocks. Append exactly this issue's line(s) below
the existing content (round 3 lands immediately below round 2) and never rewrite or reorder
prior lines. `printf '<line>\n' >> file` after confirming the file ends in a newline keeps
the diff to a single added line per round.

## Run each review round as its own append → re-diff → re-test cycle

Every review round is a fresh `implementing` phase: append the one requested line, push it
forward, and re-verify on the **live PR surface** (`legion gh -- pr diff 168`,
`legion gh -- pr checks 168`) at the new head — never trust the prior round's result. Run
`legion threads resolve --pr <n> --repo <owner>/<repo>` before each corrective push and paste
its output into the PR body's Threads section, even when it reports `no unresolved threads`.

## Attach a concrete negative control per criterion

Prove the branch (not `main`) introduces the line:
`legion gh -- api repos/<owner>/<repo>/contents/<file> --jq '.content' | base64 -d | grep '<the new line>'`
on `main` exits 1 pre-merge. Reason that any unrelated product-file edit would enlarge the
diff and is therefore rejected.

## Retro: build only on the approved head; never re-push post-approval `.legion/` commits

Retro's only branch output is a `docs/solutions/` commit, and it must sit directly on the
reviewer's **approved head**. A tester/other-phase `.legion/` commit created *after* the
approval may be sitting locally above that head; building the retro commit on top of it would
drag process metadata into the pushed branch and void the approval (the merger's
`jj diff --from <approved-sha> --to <tip-sha> --summary` must show only `docs/solutions/`
changes). Create the retro commit with `jj new <approved-sha>` explicitly, not on whatever
`@` currently points at.
