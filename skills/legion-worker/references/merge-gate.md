# The merge gate: review and the production check

Read it when you review or approve, merge the pull request, or work a look of a closed issue's production check. Your role prompt gives the review submission, its footer, the thread rule, the CI read and the forward-merge confirmation; this page gives what it does not. The merger's steps are all in its role prompt.

## The reviewer

1. Read CI on GitHub and the issue's results with `dispatch results --issue <issue>`, never from a handoff.
2. When the diff the round reads (the whole change, or the difference from the commit your task names) touches runtime code, run `task(agent="thermonuclear-deep-review")` and `task(agent="thermonuclear-code-quality")` once at the head the round reviews, and `wait` for each result: each runs as a job of its own. Runtime code is every file but human-only documentation (`docs/`, READMEs, changelogs, `.changes/`): skills, prompt parts, agent definitions, `AGENTS.md`, workflows and deployment manifests are runtime code. Each agent reviews only the diff its task supplies, so name in each task the commit, the head and `git diff <sha> <head>` as the diff, or the whole change. The last two sentences of your task, `Last reviewed commit: <sha>.` and `Review entry: <n>.`, are the daemon's; your role prompt says how to read them.
3. A result that carries no report, or that names another place for it, is a failed run, not a verdict: run that agent again, or read its transcript, and record nothing from it until you have the findings text in front of you. Reading the agent's diff yourself and recording that in its place is the same as answering the rubric from a bare verdict. When you can do neither, that half of the round has no review: tell the architect, do not approve, and wait with a `WAITING` line naming what you wait on until it answers.
4. A `CHANGES` from either half is answered in the review: its findings are carried into a `REQUEST_CHANGES`, or each finding is recorded with why it does not stand. Your `APPROVE` needs both halves run at the reviewed head, or both `Thermo` rows reading `not run` because the diff the round reads touches no runtime code (the whole change, or the difference from the last reviewed commit to the head).

### What the base requires

Read the base's required checks with the base name escaped in the same command (a raw `#` or `%` reads another branch):

```bash
base=$(gh pr view <n> -R <o>/<r> --json baseRefName -q .baseRefName) && \
  base_path=$(jq -rn --arg b "$base" '$b | split("/") | map(@uri) | join("/")') && \
  echo rulesets: && gh api --paginate "repos/<o>/<r>/rules/branches/$base_path" --jq '.[] | select(.type=="required_status_checks") | .parameters.required_status_checks[].context' && \
  echo protection: && gh api "repos/<o>/<r>/branches/$base_path" --jq '.protection.required_status_checks.checks[].context' && \
  echo code_scanning: && gh api --paginate "repos/<o>/<r>/rules/branches/$base_path" --jq '.[] | select(.type=="code_scanning") | .parameters'
```

A `code_scanning` rule requires an analysis that no status-check list names. A read GitHub refuses leaves the required set unknown: report it to the architect, do not approve, and wait with a `WAITING` line naming the read. The architect has the read made with a credential that can make it, or asks a person (a child's own architect asks through the architect above it), and the review goes on from the answer.

### The head's checks

- Read the check runs and, every page, the commit statuses: `gh api --paginate 'repos/<o>/<r>/commits/<headRefOid>/status?per_page=100' --jq '.statuses[] | [.context, .state] | @tsv'`. Read the status list, never that endpoint's top-level `state`, which reads `pending` for a head with no status.
- The newest run of a name (latest `started_at`) decides, and the newest status of a context. `--paginate` runs `--jq` once per page, so choose over the merged list.
- A check run `queued`, `requested`, `waiting`, `pending` or `in_progress`, or a status `pending`, is running: wait, unapproved. GitHub refuses a re-run while an attempt of that run is still in progress: wait for it.
- A run waiting on an environment's approval, another App's check run, or a status left `pending` has no bound. Compare how long it has stood with how long the same name took at the base branch's head commit: `gh api "repos/<o>/<r>/branches/$base_path" --jq .commit.sha`, with `$base_path` built in the same command as above, then that commit's `check-runs` (`started_at` to `completed_at`) and `status` (`created_at` to `updated_at`). Once it has stood longer, or at once where that commit holds no finished one of that name, tell the architect, naming the run, do not approve, and wait with a `WAITING` line naming what you wait on.
- A check the base does not require holds no approval: you neither wait for it nor re-run it for the approval. Read its result all the same: a red one whose log shows the change breaks something is a finding; any other is named in the review body and decides nothing.
- The daemon's `checkRuns` count in the issue's `mergeWait` is only the runs a settlement applied: read GitHub before you act.

### A required item with no run at the head

A required item in either list is reported, not missing: running as above, or red, a check run concluded `failure`, `timed_out`, `cancelled` or `startup_failure`, or a status `failure` or `error`. Only a required item in neither list, the head's check runs and its commit statuses, has no run. Never approve a head while a required item has no run: GitHub will not merge it. Read `skill://legion-worker/references/merge-gate-no-run.md` when a required item has no run at the head: it sorts the item into its case, and only one case takes the close and reopen of the pull request.

### The issue's checks

- Approval is gated by the issue's own `before_merge` checks, as your role prompt says; a `ci_check` check as `skill://legion-worker/references/ci-checks.md` says. A check whose `run` is a whole test suite with no pass standing is a finding for the architect: wait for its rewrite, and request no changes for it. One that has a pass standing at the head is a finding for the architect too, and its pass decides the check, so the review goes on.

### A merge of the base is reviewed for what it changed

After a forward merge of the base, read the merge (`skill://legion-worker/references/conflicts-and-rewrites.md`) and this change's own commits since the head your last submitted review read, its Legion footer's `head`. This lists your session's reviews, oldest first, so the last line is your last review; no line, or `no head` on it, means you read the branch's whole diff:

```bash
gh api --paginate repos/<o>/<r>/pulls/<n>/reviews --jq '.[] | {id, state, submitted_at, footer: ([.body // "" | scan("<!-- legion: (\\{.*?\\}) -->")] | last | .[0]? // "" | try fromjson catch {})} | select(.footer.phase == "review" and .footer.session == "<your session id>") | [.id, .state, .submitted_at, (.footer.head // "no head")] | @tsv'
```

When the daemon's merge-forward message asks this while the issue stands past its review (`accepting`, `retro` or `merging`), tell the architect and the worker of the phase the issue stands in (the implementer in `retro`, the merger in `merging`; the architect alone in `accepting`) on their role topics what you decided. A request for changes there goes back through that role's `request_backward_move`.

## The production check after the close

Read `skill://legion-worker/references/merge-gate-production.md` when you work a look of a closed issue's production check.
