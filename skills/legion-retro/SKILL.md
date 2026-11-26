---
name: legion-retro
description: "Use when the Legion daemon starts you, the implementer, on `Phase: retro` after the task's last step passed."
---

# Legion retro

Retro records what the work taught, between the task's last step and the merge. It commits nothing, pushes nothing and writes no handoff: the merger merges the head the last step passed.

1. Read the issue, `dispatch results --issue <issue>`, the pull request, and each step's handoff. Where a `before_merge` check has no pass standing at the head, send the issue back with `request_backward_move` to the phase that owes the result, and name the check.
2. Record what the work taught as one `dispatch comment` on the issue. Each point gives the rule, its evidence (a file and symbol, or quoted text, or a commit of this pull request, and what went wrong), and the file the rule belongs in. Write no comment when no point qualifies.
3. Update the pull request body where the repository requires lines for the paths the pull request changes.
4. Post one `dispatch message` on the issue: the comment's link, the results you read, and what will show the change working in production.
5. Complete with `handoff_complete`. In the same call send `plan_rating`, a whole number from 1 (the plan cost the work) to 5 (the plan fit the work), and `plan_reason`, one sentence of why. The daemon refuses a completion without them (`PLAN_RATING_REQUIRED`), with one alone, a rating off that scale or a reason blank or over 2,000 characters (`PLAN_RATING_INPUT`), and with both on any other completion (`PLAN_RATING_UNEXPECTED`).
