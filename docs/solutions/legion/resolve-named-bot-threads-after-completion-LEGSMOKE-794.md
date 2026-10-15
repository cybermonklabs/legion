---
title: "A reviewer's accepted-bot-thread notice arrives after your phase completed; resolve exactly that thread with the workspace read-only"
category: legion
tags:
  - review-threads
  - envoy
  - implementer
  - reviewer
  - pr-body
date: 2026-10-15
status: active
module: pull-request review threads
related_issues:
  - "LEGSMOKE-794"
  - "sjawhar/legion-smoke#575"
---

# A reviewer's accepted-bot-thread notice arrives after your phase completed; resolve exactly that thread with the workspace read-only

Expect the reviewer's Envoy message naming an accepted bot thread by node id to reach you between assignments, while the issue is in `reviewing` and your workspace is read-only. Act on it in that turn: a GitHub write (`resolveReviewThread`, a PR body PATCH) mutates nothing in `$LEGION_WORKSPACE`, so the read-only rule does not block it, and the reviewer's approval is already written while the resolution waits on you.

Read the thread before the mutation: fetch its node (`gh api graphql -f query='{node(id:"<id>"){... on PullRequestReviewThread{isResolved path line comments(first:10){nodes{author{login} body}}}}}'`) and confirm two things — the root comment is the bot's and the reviewer's acceptance reply is present. Resolve only the ids the reviewer named, one mutation per id; re-query `reviewThreads` afterwards and quote the count.

Record it in the PR body's `Threads` section as `accepted by the reviewer — <its reason>; resolved at <current PR head>`, stamping the head GitHub reports at that moment (`pulls/<n> --jq .head.sha`) even when it is another role's handoff commit: the stamp names when the resolution happened, not a code head. Fetch the live body immediately before the edit and change only that section.

Confirm back on the reviewer's role topic with the thread id, the mutation result, and the new `Threads` count, so its handoff can record the resolution it could not perform itself.

## Evidence

Pull request `sjawhar/legion-smoke#575`. Review round 2 at head `69aae1ee` found `PRRT_kwDOUCdo886s8qQY` (`sjawhar-agent` bot, `smoke/LEGSMOKE-794.md:1`, "is this file change needed?"), replied `r4287775380` (not a defect: the spec asks for the file), and published its node id to `notifications.role.legion-legsmoke-legsmoke-794-implementer` (envoy id `2fce980c…`). The implementer, idle after its round-3 completion, read the node (root `4287764291` by the bot, acceptance by `legion-reviewer`), ran one `resolveReviewThread` → `isResolved: true`, re-queried `reviewThreads` → `totalCount 2`, both resolved, patched the live body's `Threads` line to `2 resolved, 0 unresolved` with the row stamped `69aae1ee`, and published the confirmation; `review.json` round 2 then recorded `namedToImplementer` and `gatesApproval: false`.
