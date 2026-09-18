---
title: "Implementer jj bookmark, push, and handoff mechanics"
category: legion
tags:
  - jj
  - bookmark
  - git-push
  - handoff-schema
  - head-sha
date: 2026-09-18
status: active
module: legion-worker
related_issues:
  - "SL200C-1"
  - "sjawhar/legion-smoke#83"
---

# Implementer jj bookmark, push, and handoff mechanics

Four mechanics that cost a retry each on SL200C-1. None is in the legion-worker skill's literal
commands.

## 1. `jj bookmark set legion/<KEY>` without `-r` targets the empty working copy

After `jj split -m "..." <paths>` the working copy `@` is a new, empty, undescribed commit and the
real commit is `@-`. The daemon provisions `legion/<KEY>` on `@`, so the skill's literal
`jj bookmark set legion/<KEY>` leaves it on the empty commit, and `jj git push` refuses a commit
with no description. Set the bookmark at `@-`:

```bash
cd -- "$LEGION_WORKSPACE" && \
  jj -R "$LEGION_WORKSPACE" bookmark set legion/<KEY> -r @- && \
  jj -R "$LEGION_WORKSPACE" git push --bookmark legion/<KEY>
```

On the first push the bookmark is moving from `@` to `@-`, which jj calls backwards and refuses
(`Refusing to move bookmark backwards or sideways`). Check `jj bookmark list --all-remotes
legion/<KEY>`: when no `@origin` line exists, `--allow-backwards` is safe because nothing remote
moves. Every later push (the `.legion/` deletion, retro) is a forward move and needs no flag.

## 2. `legion handoff write --phase implement` requires string fields in `proof[]`

The validator refused `proof.0.criterion: expected string, received undefined` and
`proof.0.negativeControl: expected string, received object`. Each proof entry needs
`criterion`, `command`, `observed`, `headSha`, and `negativeControl` as strings; extra fields such
as `surface`, `negativeControlCommand`, or `headShaNote` pass through undeclared. Write the payload
to `/tmp/<key>-implement.json` and pass `--data "$(cat /tmp/<key>-implement.json)"` so the
payload file is never snapshotted into the workspace.

## 3. The proof's head SHA cannot name the handoff commit

The handoff commit does not exist when the handoff is written, so `proof[].headSha` names the code
commit (the last commit that changes a product file) with a note saying so. After
`jj split -m "implement: record handoff" .legion/implement.json`, re-run the proof command at `@-`
(now the pull request head) and name that SHA in the PR body's `E2E (implementer)` line. On
SL200C-1 the code commit was `4f4a73b3` and the PR head `b680235962f5`; the reviewer and tester
both read the PR body, so the head there is the one that matters.

## 4. The `.legion/` deletion diff shows only the handoffs that existed at the reviewed head

The deletion commit removes every tracked `.legion/*.json` (`jj file list -r @ .legion` names
them; `.legion/messages/` is untracked). Comparing the reviewed head to the deletion head on GitHub
listed only `removed .legion/implement.json` and `removed .legion/plan.json`, because the tester's
and reviewer's handoffs were added and removed inside that range. That is the expected shape, not a
missing file. Quote the compare output and the PR's files-against-main list in the PR body's
Threads section so the reviewer can approve the new head by SHA without re-deriving it.
