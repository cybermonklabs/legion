---
title: "Implementer mechanics for multi-round one-file PRs"
category: legion
tags:
  - implementer
  - jj-bookmark
  - pr-body
  - review-rounds
  - ci
date: 2026-09-24
status: active
module: legion-worker
related_issues:
  - "S391565457-1"
  - "sjawhar/legion-smoke#154"
---

# Implementer mechanics for multi-round one-file PRs

Learned on S391565457-1 (PR #154): one new file, then three review rounds that each appended
one line. The product change was trivial; every snag was workflow mechanics.

## 1. First push: the provisioned bookmark sits on the empty working copy

The workspace is provisioned with `legion/<KEY>` pointing at `@` (the empty working copy).
After `jj split -m "<msg>" <path>`, the commit you want to push is `@-`, and
`jj bookmark set legion/<KEY> -r @-` fails with
`Refusing to move bookmark backwards or sideways`. On the first push only, use
`--allow-backwards`; it is safe because the bookmark has no remote yet
(`jj bookmark list legion/<KEY> --all` shows only `@git`). Later rounds move forward normally.

## 2. Prove each round's delta against the reviewed head

`legion gh -- api repos/<owner>/<repo>/compare/<reviewed-sha>...<new-code-sha> --jq '.files[].filename'`
shows exactly what the round changed. `<reviewed-sha>` is the `commit_id` of the latest
`CHANGES_REQUESTED` review (`pulls/<n>/reviews`), which differs every round. Expect the tester's
`.legion/test.json` in that delta: the tester cannot push, so its handoff commit rides your push.
That path is not scope creep; any non-`.legion/` path other than the target file is.

## 3. Rebuild the PR body from GitHub, never from a local draft

The tester edits `E2E (tester)` in the PR body between your rounds. Fetch the live body
(`legion gh -- pr view <n> --json body --jq .body > file`), replace only your own lines
(summary, `CI`, `Threads`, `E2E (implementer)`), and `pr edit --body-file`. Editing a draft you
saved earlier silently reverts the tester's line.

## 4. `legion gh` works only in bash, not in eval

Eval kernels do not inherit the pane's `LEGION_*` environment:
`legion gh` from Python `subprocess` fails with `LEGION_GH_PATH is not an absolute path`.
Run every credentialed command in `bash`, write its output to a file, and do text processing in
eval from that file.

## 5. Cite the CI run at the head you name

`pull_request`-event runs can lag the `push`-event runs by a minute or more; on round 1 only `push`
runs existed about 65 s after the push. List runs by branch with `headSha`
(`run list --branch legion/<KEY> --json databaseId,event,conclusion,headSha`) and cite a run whose
`headSha` equals the handoff head you pushed, not the code commit under it.
