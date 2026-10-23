---
title: "Read the approved head from GitHub, never from review.json"
category: legion
tags:
  - review
  - approved-head
  - handoff
  - merge-gate
date: 2026-10-23
status: active
module: merge-gate
related_issues:
  - "LEGSMOKE-1062"
  - "sjawhar/legion-smoke#764"
---

# Read the approved head from GitHub, never from review.json

The reviewer commits and pushes `.legion/<issue>/review.json` first and then approves the head
that push made, so the handoff is one commit below the SHA the approval names and can never carry
it. Every later reader of "the approved head" follows these rules.

- Take the approved SHA from the review itself:
  `gh api repos/<o>/<r>/pulls/<n>/reviews --jq '.[] | select(.state=="APPROVED") | .commit_id'`,
  and use that SHA as `--from` in the merger's `jj diff --from <approved-sha> --to <tip-sha>
  --summary`, as retro's base for "nothing above the approved head but my commits", and in the
  architect's acceptance. A SHA copied from `review.json` names the head one commit below the
  approval, so a range measured from it covers the review handoff commit too, a commit the
  approval already carries, and whatever it reports as "above the approved head" is off by that
  commit.
- In `review.json`, name a head for what it is. A field the reviewer fills before its push holds
  the head it read (`readHead`, or the round's code head); it is not the approved head, and a
  field called `reviewedHead` or `approvedHead` holding it misleads the merger. The approved SHA
  lives on GitHub only, and the approval body should name it, as this run's did.
- A reader who sees the two SHAs differ by exactly the review handoff commit
  (`compare/<review.json head>...<approval commit_id>` lists only `.legion/<issue>/review.json`)
  is looking at the normal shape, not a mismatch.

## Evidence

Pull request sjawhar/legion-smoke#764, branch `legion/LEGSMOKE-1062`, 2026-10-23:

- `.legion/LEGSMOKE-1062/review.json` line 7 recorded
  `"reviewedHead": "3621ee5b55ff6cb2dddd0af456995068fc4cdae3"` (the test handoff head, the branch
  tip when the reviewer read it). The review handoff commit pushed after that read is
  `69d1ea7c38848197094c26f2b2f802265aed98f9`.
- `gh api repos/sjawhar/legion-smoke/pulls/764/reviews` lists one review, id 5629925958,
  `APPROVED`, `commit_id` `69d1ea7c38848197094c26f2b2f802265aed98f9`; its body opens with
  `APPROVE head 69d1ea7c… (round 1; code head 0ac75fe9…, only .legion/ above it)`.
- Retro's commits (this note and the removal of `.legion/LEGSMOKE-1062/`) were therefore placed
  above `69d1ea7c`, the SHA GitHub holds, not above `3621ee5b`;
  `compare/3621ee5b...69d1ea7c` → `ahead`, 1 commit, files `[.legion/LEGSMOKE-1062/review.json]`,
  the normal one-commit shape.
- The fresh-eyes retro reader flagged the two SHAs as an inconsistency before reading the approval
  body; the rules above are what it would have needed.
