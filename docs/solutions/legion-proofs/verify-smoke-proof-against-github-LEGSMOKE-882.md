---
title: "Verify a Legion smoke proof against GitHub's served bytes, not the local checkout"
category: legion-proofs
tags:
  - legion-smoke
  - proof-verification
  - fail-on-demand
  - review-threads
date: 2026-10-20
status: active
module: smoke
related_issues:
  - "LEGSMOKE-882"
  - "sjawhar/legion-smoke#710"
---

# Verify a Legion smoke proof against GitHub's served bytes, not the local checkout

Rules for every role that proves a `smoke/<KEY>.md` marker change in this repository.

1. Fix the marker's bytes before the first code push and check them whole: fetch the file at an
   explicit SHA with `gh api -H 'Accept: application/vnd.github.raw+json'
   'repos/sjawhar/legion-smoke/contents/smoke/<KEY>.md?ref=<sha>'`, then `cmp` it against the
   `printf` literal, `wc -c` the expected byte count, and confirm the last byte is LF, no CR, no
   BOM. A `wc -l`/`grep -c <KEY>` pair alone accepts CRLF, a BOM, trailing spaces and an
   unterminated second line.
2. Judge "the branch's first push" at the first code head (`commits?sha=legion/<KEY>&path=smoke/<KEY>.md
   | .[-1].sha`), never at the branch's literal first push: the planner's handoff-only push precedes
   it and carries no `smoke/` file. Record `head -1` there and re-compare it at every later head.
3. Read CI at the newest code head, not the branch tip. Handoff-only pushes (`.legion/<KEY>/` only)
   end their message with `skip-checks: true` and start zero workflow runs
   (`actions/runs?head_sha=<tip>` → `total_count: 0`); the verdict belongs to the code head below
   them, and `compare/<code-head>...<tip>` must list only `.legion/<KEY>/` paths.
4. `pulls/<N>/files` and `gh pr diff --name-only` are the base…head net diff: a path added and later
   deleted on the branch leaves no trace there. Read check-runs at the exact SHA
   (`commits/<sha>/check-runs`) to see what a red head carried.
5. When a `.fail-me` commit lands on the branch and `fail-on-demand`'s `gate` goes red, cure it with
   a deletion-only commit started from the remote tip (`jj git fetch`, then
   `jj new legion/<KEY>@origin`, `rm .fail-me`, `jj split -m … .fail-me`), pushed with no trailer.
   Never `gh run rerun`: the red is the file's, not flaky. A `.fail-me` on `main` cannot be cured
   from the branch without a net change outside the marker; report it instead.
6. Detect a review's one-line request mechanically: top-level comments (`in_reply_to_id == null`) on
   the marker's path whose author submitted a `CHANGES_REQUESTED` review and whose body contains
   `Legion proof review round`. Extract the fenced block's text with `tr -d '\r' | awk '/^```/{f=!f;
   next} f'`, require exactly one line, append it by byte copy (`cat >>`), and compute the expected
   size as `old + ${#L} + 1`. Never retype the line.
7. Once another role has written into the PR body, fetch the live body (`gh api
   repos/sjawhar/legion-smoke/pulls/<N> --jq .body`), edit only your own lines, and `PATCH` it back.
   Re-uploading a locally kept copy overwrites the reviewer's `Look at first` / `Not proven / risk`
   and the tester's `E2E (tester)` lines.
8. Reply on a review thread through `pulls/<N>/comments/<comment_id>/replies`; read a single comment
   through `pulls/comments/<comment_id>` (the `pulls/<N>/comments/<id>` form answers 404). Resolve
   with one `resolveReviewThread` GraphQL call per thread, after its reply.

## Evidence

LEGSMOKE-882 / sjawhar/legion-smoke#710 (2026-10-20). Marker `smoke/LEGSMOKE-882.md`: 56 bytes
at H1 `c971366f`, 95 bytes at `7cdf1f71` after the review's one-line request (`L` = 38 bytes,
extracted from review comment 4329699491). The driver's commit `f824408e` added `.fail-me`;
`gate` failed twice there (`check` passed); the deletion-only commit `b9acfdbe` from the remote
tip cured it, and `pulls/710/files` never listed `.fail-me`. Handoff-only heads `0fc3ee51`,
`e7c96f5d`, `b63b1791` started zero runs. Both review threads (`PRRT_kwDOUCdo886ugJY8`,
`PRRT_kwDOUCdo886ugiU8`) were answered once and resolved once each by the implementer's `gh`.
