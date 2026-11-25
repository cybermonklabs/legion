# The merge gate: review and the production check

## The reviewer

1. Read CI on GitHub and the issue's results with `dispatch results --issue <issue>`, never from a handoff.
2. When the diff the round reads touches runtime code (the whole change, or the difference from the commit your task names), run `task(agent="thermonuclear-deep-review")` and `task(agent="thermonuclear-code-quality")` once at the head the round reviews, and `wait` for each result: each runs as a job of its own.
   Each agent reviews only the diff its task supplies: name in each task the commit, the head and `git diff <sha> <head>`, or the whole change, or, for a merge of the base, the merge's resolution (`git show --remerge-diff <merge>`) and this change's own commits since that commit.
3. A result that carries no report, or that names another place for it, is a failed run, not a verdict: run that agent again, or read its transcript.
   Reading the agent's diff yourself and recording that in its place is the same as answering the rubric from a bare verdict.
   When you can do neither, that half of the round has no review: tell the architect, do not approve, and wait with a `WAITING` line naming what you wait on until it answers.
4. A `CHANGES` from either half is answered in the review: its findings are carried into a `REQUEST_CHANGES`, or each finding is recorded with why it does not stand.
   Your `APPROVE` needs both halves run at the reviewed head, or both `Thermo` rows reading `not run` because the diff the round reads touches no runtime code, or no `Thermo` line on a docs-only pull request.

### What the base requires

Read the base's required checks, the base name escaped in the same command (a raw `#` or `%` reads another branch):

```bash
base=$(gh pr view <n> -R <o>/<r> --json baseRefName -q .baseRefName) && \
  base_path=$(jq -rn --arg b "$base" '$b | split("/") | map(@uri) | join("/")') && \
  echo rulesets: && gh api --paginate "repos/<o>/<r>/rules/branches/$base_path" --jq '.[] | select(.type=="required_status_checks") | .parameters.required_status_checks[].context' && \
  echo protection: && gh api "repos/<o>/<r>/branches/$base_path" --jq '.protection.required_status_checks.checks[].context' && \
  echo code_scanning: && gh api --paginate "repos/<o>/<r>/rules/branches/$base_path" --jq '.[] | select(.type=="code_scanning") | .parameters'
```

A `code_scanning` rule requires an analysis that no status-check list names.
A read GitHub refuses leaves the required set unknown: report it to the architect, do not approve, and wait with a `WAITING` line naming the read.

### The head's checks

- Read the check runs and, every page, the commit statuses:
  `gh api --paginate 'repos/<o>/<r>/commits/<headRefOid>/status?per_page=100' --jq '.statuses[] | [.context, .state] | @tsv'`.
  Read the list, never the endpoint's top-level `state`, which reads `pending` for a head with no status.

- The newest run of a name (latest `started_at`) decides, and the newest status of a context.
  `--paginate` runs `--jq` per page, so choose over the merged list.

- A required check run `queued`, `requested`, `waiting`, `pending` or `in_progress`, or such a status `pending`: wait, unapproved.
  A run waiting on an environment's approval, another App's check run, or a status left `pending` has no bound: once it outlasts its usual CI time, tell the architect and wait.

- A check the base does not require holds no approval: a red one is a finding if its log shows the change breaks something, else name it in the review body.

- The `checkRuns` of the issue's `mergeWait` counts only the runs a settlement applied: read GitHub before you act.

### A required item with no run at the head

Never approve a head while a required item has no run; sort it in this order.
Cases 2, 3, 4 and 6 end: tell the architect, do not approve, and wait with a `WAITING` line naming what you wait on.

1. Read `gh pr view <n> --repo <o>/<r> --json mergeable,mergeStateStatus` first.
   `DIRTY` takes a forward merge, the implementer's before `merging` and the merger's in it: the daemon asks for it and names the new head.
   `UNKNOWN`: read each minute for up to five minutes, then report it to the architect as a refused read.
2. A fork head whose runs stand `action_required` waits on a maintainer.
   Never close and reopen it.
3. A check only a commit status reports, or a workflow another repository defines: nothing here starts it.
   Never close and reopen it.
4. A required workflow whose `paths` or `paths-ignore` filter, as the base holds it, skips every file of the change:
   a reopen sends the same event, so the head or the workflow must change.
5. The remedy's case: a `BLOCKED` head, not from a fork (`--json isCrossRepository`), with no run `action_required`, where a workflow under `.github/workflows/` (as the merge commit holds it) has a job reporting the item whose `on:` runs it for `pull_request` with no `types` that leave out `reopened`, no `branches` filter that excludes the base and no path filter that skips the whole change, and neither list holds a run of it.
   A job reports its `name:` or key, ` (<matrix values>)` after it for a matrix job, `<calling job> / <job>` for a reusable workflow's;
   for `code_scanning`, the analysis jobs report under their own names and the `CodeQL` run is the code-scanning App's.
   Tell the architect why, close the pull request, wait a minute, and reopen it at the same head (the close's own run can cancel a quicker reopen's).
   Approve only once the item's run concluded green.
   One close and reopen covers every item of the head: name the others to the architect in the same message.
   A head with any item in case 2 or 3 takes no remedy, and an item with no run once the reopen's runs concluded is never reopened.
6. Anything else.

### The issue's checks

- Approval is gated by the issue's own `before_merge` checks, as your role prompt says;
  a `ci_check` check as `skill://legion-worker/references/ci-checks.md` says.
  With no earlier tester step, the implementer's evidence for each is what you read.
  A check whose `run` is a whole test suite with no pass standing is a finding for the architect: wait for its rewrite, and request no changes for it.

### A merge of the base is reviewed for what it changed

After a forward merge, read the merge and this change's own commits since your last submitted review's footer `head` (`skill://legion-worker/references/conflicts-and-rewrites.md#reading-a-merge-of-the-base`, which lists your reviews).

## The production check after the close

- Reach the deploy as the deployment instructions say.
  When they name an agent or a role, `envoy_publish` the merge commit to its topic with `expects_reply: false` and await no reply.
  Open a `dispatch ask` only where the instructions name nobody; the look still ends `pending`.

- A staging pass is no production pass: staging may lack resources production runs.

- Report each look's verdict as your role prompt says.
  A `fail` follows `skill://legion-worker/references/findings.md`: file no issue.
