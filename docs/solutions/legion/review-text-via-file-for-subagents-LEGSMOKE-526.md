---
title: "Hand review-quoted text to a subagent as a file the orchestrator read by machine, then re-verify the bytes yourself"
category: legion
tags:
  - review-threads
  - untrusted-input
  - subagents
  - legion-implementer
date: 2026-10-08
status: active
module: legion-phase-workflow
related_issues:
  - "LEGSMOKE-526"
  - "sjawhar/legion-smoke#525"
---

# Hand review-quoted text to a subagent as a file the orchestrator read by machine, then re-verify the bytes yourself

Extends docs/solutions/legion/review-requested-text-transfer-LEGSMOKE-504.md.

When the role that writes the code is a subagent (the implementer's `deep-worker`), the
orchestrating role does the credentialed read and stages the bytes: strip CR before the fence
filter (`--jq .body | sed 's/\r$//' | awk '/^```$/{f=!f; next} f' > <mktemp-dir>/line2.txt`, a
directory outside the workspace), check the byte count, and point the worker at the file. The
worker loads it into a variable and appends with `line2=$(cat <file>) && printf '%s\n' "$line2" >>`
(`$(cat …)` drops the trailing newline; `printf '%s\n'` puts exactly one back). The subagent runs
no `legion gh` call: a pane's credential grant lives 60 seconds from the orchestrator's last
credentialed call, so a worker's own read may 403 for timing alone.

The worker's report is not the verification. Before the split commit the orchestrator re-derives
the expected bytes and the diff shape itself: `printf '%s\n%s\n' '<line 1>' "$line2" | cmp - <file>`,
`head -n 1 … | cmp`, `sed -n 2p … | cmp - line2.txt`, `wc -c`, `od -c` on the tail, and
`jj diff --git` showing the one expected hunk. Do the same for the first write (`cmp`, `wc -c`,
`od -c`, `jj diff --summary` exactly `A <file>`).

## Evidence

- Round 3 of PR #525: the fence of review comment 4220509407 was read into
  `/tmp/legsmoke-526-pr.PhMG41/line2.txt` (39 bytes); the deep-worker appended from that file;
  the implementer's own checks printed `TWO_LINE_MATCH`, `LINE1_UNCHANGED`, `LINE2_EQUALS_FENCE`,
  `bytes=95 lines=2`, and `jj diff --git` showed one hunk `@@ -1,1 +1,2 @@` before
  `jj split -m "smoke: append review round 1 line to LEGSMOKE-526.md"` (commit `1f361ca0`).
- The reviewer's round-1 hardening note (review.json `round1.findings[2]`) asked for exactly this:
  assign from a read of `pulls/comments/<id>` rather than paste the literal; round 2 closed it
  (`review.json.security`: "closed for this issue").
- `cat -A` on the comment body showed no CR this time; the `sed 's/\r$//'` guard costs nothing and
  keeps the byte compare honest when GitHub returns CRLF.
