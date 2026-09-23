---
title: "Substring-safe append markers and stable-proof handoff commits in Legion phase work"
category: legion
tags:
  - legion-workflow
  - jj-workspace
  - acceptance-criteria
  - implementer-proof
  - correction-rounds
date: 2026-09-23
status: active
module: legion-smoke
related_issues:
  - "S393466070-1"
  - "sjawhar/legion-smoke#112"
---

# Substring-safe append markers and stable-proof handoff commits

Reusable lessons from S393466070-1 / PR #112: a one-line README marker taken through the full
Legion phase workflow (plan → implement → test → review) across three scripted
`CHANGES_REQUESTED` correction rounds, on a shared jj workspace.

## 1. A substring-count acceptance is brittle — add context away from the marker

The acceptance was `git grep -c "Legion Stage 3 smoke marker" README.md` == 1, with the marker as
the file's final line. `grep -c` counts *lines containing the substring*, not exact-line matches,
so any line that merely contains `Legion Stage 3 smoke marker` inflates the count and fails the
gate.

- Commit the marker line exactly **once** and never modify it afterward (introduced at `ec405fb5`,
  untouched through every later commit).
- Every correction must add context on a **separate line** whose text deliberately omits the
  marker substring. Across rounds the traceability line evolved
  `Legion smoke check: S393466070-1.` → `Legion smoke check (Stage 3 proof): S393466070-1.` →
  `Legion smoke check (Stage 3 proof): S393466070-1 (PR #112).` — none contains
  `Legion Stage 3 smoke marker`, so the count stayed 1.
- Verify both facts every round, not just the count: `git grep -c "<marker>"` **and**
  `tail -n1` (the marker must remain the *final* line). A negative control (grep a wrong string →
  0) proves the check discriminates.

Footgun: a future edit that reuses the marker phrasing for a heading or a near-duplicate line
silently breaks the gate. Prefer an anchored acceptance (exact-line / regex `^...$`) over a bare
substring count when the file will keep growing.

## 2. Keep the proof's head SHA on a stable product commit, not the handoff commit

An implement handoff (`.legion/implement.json`) records a `proof` array whose `headSha` cites where
the behavior was verified. The handoff file **cannot contain the SHA of its own commit** (writing
the SHA changes the commit). Resolve the circularity by making the product change its **own commit**
and citing that stable SHA:

- Land the product edit as a commit (e.g. `ec405fb5`, and each correction `10c30932`, `e0e56606`,
  `01863ca9`). Read its `commit_id` — it is now stable.
- Create the handoff as a **child** commit with `jj -R "$WS" new -m "implement: record handoff"`,
  then `legion handoff write --phase implement`, citing the product commit's SHA as `proof.headSha`.
  `jj new` (endorsed by the worker skill's jj-safety rules) keeps the product commit's SHA fixed;
  `jj split` would rewrite it because the working copy briefly holds both changes.
- The PR body's `E2E` line can cite the branch head (what the reviewer re-runs), while the handoff
  proof cites the product commit — both are honest and the product commit is an ancestor of the tip.

## 3. `legion handoff complete` needs the handoff committed with a clean working copy

`legion handoff complete` refuses while `.legion/<phase>.json` shows as an uncommitted working-copy
change. After the handoff commit, create an empty working copy on top
(`jj -R "$WS" new`) and set the bookmark to `@-` (the handoff commit). Then the file lives in a
committed ancestor and `@` is clean.

## 4. Keep helper files out of the product diff

A PR-body draft written under `.legion/` (or anywhere tracked) leaks into the working-copy diff and
would be pushed. Keep scratch files **outside** `$LEGION_WORKSPACE` (e.g. `/tmp`) so the product
diff stays to the one intended file. Verify with `jj -R "$WS" diff --from main@origin --to <head>
--summary` before every push — only the intended product file plus the allowed `.legion/*` handoffs
should appear.

## 5. Correction rounds with no inline threads

The scripted reviews were top-level `CHANGES_REQUESTED` with **no inline review threads**. An
implementer can only resolve threads (via `legion threads resolve`), never a bare review, so each
round: make a minimal acceptance-preserving one-file edit, run
`legion threads resolve --pr <n> --repo <o>/<r>` (it printed `no unresolved threads` here), push,
refresh the PR body, and finish with `legion handoff complete` — a push alone does not finish a
round. When a change request names no concrete finding, refine your *own* prior correction rather
than accumulate near-duplicate lines (each of which risks matching a substring acceptance).

## Note observed at retro

The reviewer approved head `f6da1ff1`, which still carried the `.legion/*.json` handoffs — the
separate `.legion/` deletion step did not run before retro in this scripted flow. Retro adds only
this `docs/solutions/` commit above the approved head; the approval stands because the commit
changes nothing but documentation.
