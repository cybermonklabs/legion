---
title: "Prove a Legion pull request at pinned heads on GitHub"
category: legion
tags:
  - legion-phase-workers
  - proof-evidence
  - skip-checks
  - github-actions
  - review-threads
  - jj-hygiene
date: 2026-10-10
status: active
module: legion-workflow
related_issues:
  - "LEGSMOKE-610"
  - "sjawhar/legion-smoke#543"
---

# Prove a Legion pull request at pinned heads on GitHub

Rules for a phase worker (planner, implementer, tester, reviewer, merger) whose acceptance
criteria are judged on GitHub itself: the pull request's file list, a file's bytes at a commit,
and the Actions runs at that commit. Each rule is imperative; the run that taught it is under
Evidence.

## Address runs by the full SHA and scope them to the branch

- Pass `gh run list --commit` the full 40-character SHA. A short SHA returns `0` runs and no
  error, which reads as "nothing ran".
- Filter the list on `headBranch == <issue branch>` (or `main` at the merge commit). A run list at a
  SHA is not branch-scoped: the branch point carries every sibling branch's runs.
- Green means: the scoped list is non-empty, every run is `completed/success`, at least one run of
  each required workflow is present, none is `queued` or `in_progress`. Never test for a fixed
  count: a pull-request head has `push` and `pull_request` runs per workflow, a pre-PR head only
  `push`.
- Settle with `gh pr checks <n> --watch --fail-fast` in its own shell call, then re-read the live
  head and the scoped run list in a fresh call. The GitHub grant in a worker pane lives 60 seconds
  per call, so a long watch inside a subagent or `eval` 403s part-way.

## Identify a skip-checks head positively and carry its verdict with evidence

- A handoff commit whose message ends in `skip-checks: true` starts no workflow. Decide that from
  the commit itself (`commits/<sha>`: the trailer is present and the files are only
  `.legion/<issue>/`), never from the phase that made it: the trailer follows the handoff's
  `verdict` field, not the role, so a reviewer's `COMMENT`-round handoff runs CI and an
  `approved`-verdict handoff runs CI too.
- An empty scoped run list is "not judged", never "green". Walk `parents[0]` to the nearest
  commit J whose scoped list is green, record both SHAs and `jj diff --from J --to <head>
  --summary` showing only `.legion/<issue>/`, and state the verdict as carried from J.
- A skip-checks head is unmergeable on its own: with no run, the required status context is
  absent and GitHub reports `mergeable_state: blocked`. The head a human merges must be one CI ran
  on; retro's push is such a head.

## Turn a conditional acceptance line into a predicate pinned at the judged head

- When the spec says a file's shape depends on whether a review asked for something, define the
  predicate over GitHub objects and pin it to the head's committer date: for LEGSMOKE-610,
  `K(S)` = the number of top-level (`in_reply_to_id == null`) inline comments by the reviewer App
  on the file, inside a `CHANGES_REQUESTED` review, created before `S`'s committer date. Without
  the date pin, every head before the request retroactively fails once the request lands.
- Pin both shapes' bytes in the plan (size, line count, sha256) and prove the selected arm with
  `printf ... | cmp` against the raw contents read
  (`Accept: application/vnd.github.raw+json`, `?ref=<full sha>`).
- Prove the other arm as a negative control, plus a CRLF variant and a no-final-LF variant: each
  must exit 1 against the same bytes, otherwise the check cannot distinguish the shapes.
- Gate on the request's shape, then append the text the request states, extracted from the comment
  itself (`pulls/comments/<id>`, the text after its last `: `) and compared with `cmp`. The expected
  wording from earlier runs is a check on the extraction, not a condition for acting.

## Cure a foreign red head with a deletion-only child commit

- First read, in two calls, what the red head is: `contents/<trigger file>?ref=<sha>` and
  `commits/<sha>` (author, committer, parent, files). An author that is none of Legion's role Apps
  is an outside push; describe it as such and never as your own.
- Cure it with one child commit from `legion/<key>@origin` that deletes the trigger and nothing
  else. Never rewrite, rebase away, revert-by-rewrite or abandon the foreign commit: the fix is a
  child of it, pushed as a fast-forward.
- Do not re-run a workflow run that failed on its merits: it fails on the file legitimately. The
  cured head's own runs are the proof.
- The net file list after the cure (`compare <base>...<cured head>`) no longer shows the trigger,
  so a file-list acceptance line stays clean with the foreign commit still in history. Record the
  outside push in the handoff's `rounds[]` (kind `ci-red`, with author, message, files and the
  failing run ids) and the cure beside it; keep the superseded proofs in `rounds[]`, not `proof[]`.

## Write every proof as a tuple the next role re-runs verbatim

- `{criterion, surface, command, observed, headSha, negativeControl}`, each a full command with
  the full SHA inlined, so the tester re-runs it from its own scratch directory at the recorded
  head while the live head has already moved on.
- A proof the next role cannot re-run, or whose re-run disagrees, is a failure to report with a
  `failures[]` entry and `implementerProof.verdict: rejected`, never a gap for that role to fill.
- Rebuild an accumulating handoff from the file on disk (`bun -e` reading `.legion/<issue>/<phase>.json`
  and a patch file kept outside the workspace, piped to `legion handoff write --phase <phase>`),
  deleting `schemaVersion`, `phase`, `completed` and `issue` first: the command writes those itself
  and refuses them in the data.

## Stamp PR-body numbers with their SHA and rewrite a line a later fact contradicts

- Every number in the body (`Size`, run ids, byte counts) names the SHA it was read at. A stamped
  line is current for that SHA; the stamping commit itself changes the count, so stamp at the last
  code head and again at the head the merger judges.
- The `CI` line lists every head in the chain, the red one included, with its run ids and
  verdict; that is what lets a reader judge a red episode without opening a handoff.
- When a later measurement contradicts a line already in the body, rewrite that line. Two lines
  that disagree in an approved body are a false statement, not history.
- Publish the approved head's SHA where the next role reads it (the `APPROVE` body, the architect
  summary, READY): a handoff cannot contain the SHA of its own commit.

## Keep scratch bytes outside the jj workspace

- jj snapshots every file inside the workspace, and a file-list acceptance line is byte-literal.
  Every scratch file lives in `T=$(mktemp -d)` outside `$LEGION_WORKSPACE`; run `jj status`
  before each `jj split` and name exactly one path in it.
- `--jq .body` appends a newline to the pull-request body it prints; strip trailing blank lines on
  both sides before diffing a written body against its read-back, or the diff always shows one
  phantom line.

## Evidence

Run: Stage 4b proof tree 1 on `sjawhar/legion-smoke`, issue LEGSMOKE-610, pull request #543
(`smoke: add LEGSMOKE-610 marker file`), 2026-10-10. Base `aaf4c538`; approved head `eb053fc7`.

- Short SHA and scoping: `gh run list --commit aaf4c538` returned 0 runs; the unscoped list at the
  same full SHA returned 20 runs from 10 sibling branches (plan measurement M5), 23 head branches
  by the time the implementer ran its controls.
- Skip-checks heads: plan head `728a8818` (trailer true, only `plan.json`, scoped list `[]`,
  verdict carried from the branch point `aaf4c538`, runs 38027585236/38027585215); tester heads
  `96be3d1c` and `3a031fe9` likewise, carried from `954e2df1` and `1cc71d03`. The reviewer's round-1
  `COMMENT` handoff `60a9be14` had no trailer and ran both workflows green (ci 38031457315/38031459988,
  fail-on-demand 38031457297/38031459926), which the plan had predicted as skip-checks; at `3a031fe9`
  GitHub reported `mergeable_state: blocked`, at `1cc71d03` `clean`.
- Predicate K: at `60a9be14` (committer date 06:35:10Z) K=0 and the file was 56 bytes, sha256
  `ab3bdabc…`; comment 4236728635 was created 06:38:25Z; at `dc76284b` (06:39:31Z) K=1 and the file
  was 95 bytes, sha256 `7a26ff8d…`; the one-line arm, CRLF and no-final-LF variants each exited 1.
  The architect ruled (D1) that the request's own wording is appended, the literal being a check,
  not a condition.
- Foreign red head: `c375e875` by `sjawhar-agent[bot]` added `.fail-me` (25 bytes) on the
  implementer's `6a519ede`; fail-on-demand 38030233141/38030236738 failed, ci 38030233130/38030236977
  passed. Cure `3564646b` (`removed .fail-me` only, parent `c375e875`): four scoped runs green
  (38030343489/38030346252, 38030343568/38030346362), `.fail-me` 404, compare `aaf4c538...3564646b`
  lists no `.fail-me`. The failed runs were not re-run (architect ruling D7).
- Proof tuples: the tester re-ran the implementer's three commands at the recorded head `dc76284b`
  while the live head was `1cc71d03`; every observation matched.
- Body stamping: `Size` carried its SHA at every stamp; the CI line's statement that `60a9be14` was
  a skip-checks head was false against the tester's measurement and shipped into the approved
  body; retro corrected it at the merging head.
- Hygiene: every commit of the run touched exactly one path (`commits/<sha>.files` on GitHub); the
  lax item-1 filter printed exactly `added smoke/LEGSMOKE-610.md` at every head. The counter-case
  is PR #534 (LEGSMOKE-577), whose squash merge carried four `.legion/LEGSMOKE-577/*.json` files
  onto `main`.
