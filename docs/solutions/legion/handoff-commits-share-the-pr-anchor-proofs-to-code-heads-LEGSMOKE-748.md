---
title: "Handoff commits share the pull request with the code: anchor every proof, size and CI claim to a code head"
category: legion
tags:
  - legion-handoffs
  - first-push
  - pr-body
  - skip-checks
  - acceptance-criteria
date: 2026-10-13
status: active
module: legion-workflow
related_issues:
  - "LEGSMOKE-748"
  - "sjawhar/legion-smoke#569"
---

# Handoff commits share the pull request with the code: anchor every proof, size and CI claim to a code head

The phases' handoff files under `.legion/<issue>/` are committed on the issue branch, so they sit
in the pull request beside the code until retro removes them. Every claim about "the branch",
"the first push", "the diff" or "CI at the head" is skewed by them unless it is pinned to a code
head.

- Define "the first push" as the first commit whose diff touches the changed file, never the first
  push to the branch: the planner's handoff push comes first and holds no code. Record that SHA
  once in `implement.json` as `firstPushSha` and never rewrite it; keep a separate `codeSha` that
  moves to the newest code head every round.
- Compare bytes across rounds against `firstPushSha`, not against the previous head: `cmp` of the
  first line decoded from GitHub at the newest code head against the decode at `firstPushSha`
  proves earlier lines never drifted.
- Evaluate a "changed files are bounded" criterion on the net pull-request diff
  (`gh pr diff <n> --name-only` through an allow-list filter), never per commit, and let the
  allow-list name `.legion/<issue>/` during rounds and `docs/solutions/` after retro. Run the same
  allow-list as a pre-push guard:
  `jj diff --from 'fork_point(main@origin | @-)' --to @- --name-only`.
- Write the body's `Size:` line as the merge net (the code paths only) and say the
  `.legion/<issue>/` handoffs are excluded because retro removes them; never quote
  `gh pr view --json additions` at a head that still carries them.
- Name the newest code head in the `CI:` line. A handoff-only head carries `skip-checks: true` and
  gets no run of its own; its verdict is the code head's, carried by the daemon. Each code head
  gets two runs per workflow (`push` and `pull_request`): cite the `pull_request` run ids and note
  the `push` ones.
- Validate a handoff rewritten by in-place edits with `jq -e` before `jj split`: appending an
  entry to a JSON array by editing the file forgets the trailing comma on the previous entry.

## Evidence

- LEGSMOKE-748, PR sjawhar/legion-smoke#569. The planner's handoff push `437bdf87` preceded the
  code; `firstPushSha` is `d1e92f4d` (adds `smoke/LEGSMOKE-748.md`, 56 bytes), while `codeSha`
  moved to `52b0877c` (CI-red round) and then `c88797a1` (review round 1 appended the second
  line, 95 bytes). `cmp` of line 1 at `c88797a1` against `d1e92f4d`, both decoded from GitHub, was
  silent.
- Net diff at every round: `.legion/LEGSMOKE-748/*.json` plus the marker; the filter
  `grep -v -E '^(smoke/LEGSMOKE-748\.md|\.legion/LEGSMOKE-748/.+)$'` printed nothing. The foreign
  `.fail-me` (added at `ef7a4ae8`, deleted at `52b0877c`) appeared in no net diff.
- Handoff heads `cd4b3498`, `39b95cc3`, `2e38e3c1`: `gh run list --commit <sha>` printed `[]`.
  Code head `d1e92f4d`: runs 38416392222/38416392347 (`pull_request`) and 38416224090/38416224744
  (`push`), all success.
- Two in-place `implement.json` edits in this run each left the file unparseable until a comma
  was added; `jq -e` caught both before the commit.
