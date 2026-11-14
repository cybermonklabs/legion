# The merge gate: review, acceptance, retro, the merge, and the production check

Part of `skill://legion-worker`. Read it when you are the reviewer submitting a review or an
approval, the tester on a look of its closed issue's production check, or the merger merging
the pull request. Every path it cites is in cybermonklabs/legion.

The order, in full: the tester's evidence green → the reviewer's approval of the head → the
architect's acceptance, in `accepting` → retro → the merger's merge of the head it read, which
closes the issue → the production check after the close, which the project's `production_check`
setting gives to a role or takes away. Every handoff is a record on the Dispatch issue, never a
commit, so the head that merges carries none. Retro commits nothing (*Retro*, below), so the
approved head is the head that merges, except for the one commit of text fixes the loop after an
approval with open threads puts above it, which the reviewer reads and approves before the merge
(*After an approval with text threads open*, below); and no Legion check holds the head that
merges to the head the review approved in either case: a repository that wants its newest push
reviewed says so in its own rulesets. A conflict-forced merge goes back to the reviewer for a confirmation or a new round,
as the fingerprint decides (*The reviewer*, below, and
`skill://legion-worker/references/conflicts-and-rewrites.md`).
What holds a head the implementer pushed above the one the reviewer approved is this reference's
own carry rule and the architect's acceptance, not a path test.

## The reviewer

- The reviewer reads CI on GitHub and verifies the `Threads` facts and the issue's results there
  and with `dispatch results --issue <issue>` — never from a handoff — then, when the diff
  touches runtime code, runs
  `task(agent="thermonuclear-deep-review")` and
  `task(agent="thermonuclear-code-quality")` once at that head — the head the round reviews —
  waits for each one's result with `wait`, since each runs as a job of its own, and records the
  verdict from what those results say. A result that carries no report, or that names another
  place for it, is a failed run, not a verdict: run that agent again, or read its transcript, and
  record nothing from it until you have the findings text in front of you. Reading the agent's
  diff yourself and recording that in its place is the same as answering the rubric from a bare
  verdict — it is your own reading standing in for a review that did not happen, not the review
  of your own that the round always asks for. When you can do neither — the agent will not run
  again and its transcript holds no findings — that half of the round has no review: tell the
  architect, do not approve, and wait with a `WAITING` line naming what you wait on until it
  answers. The CI it reads is the head's own checks: no handoff is a
  commit, so every push to the branch is work whose head gets GitHub's workflow runs of its own.
  With `<o>/<r>`, `<n>`, `<base>` and `<headRefOid>` from
  `gh pr view <n> --json baseRefName,headRefOid`, two reads. The required set is the
  base branch's rulesets' required checks and its branch protection's, when it has any. Both
  branch endpoints take the base as a URL path, each segment escaped and each `/` kept (a raw `#`
  or `%` reads another branch), built in the same command from the name GitHub returns, never
  pasted or carried over from an earlier command:

  ```bash
  base=$(gh pr view <n> -R <o>/<r> --json baseRefName -q .baseRefName) && \
    base_path=$(jq -rn --arg b "$base" '$b | split("/") | map(@uri) | join("/")') && \
    echo rulesets: && gh api --paginate "repos/<o>/<r>/rules/branches/$base_path" --jq '.[] | select(.type=="required_status_checks") | .parameters.required_status_checks[].context' && \
    echo protection: && gh api "repos/<o>/<r>/branches/$base_path" --jq '.protection.required_status_checks.checks[].context'
  ```

  A read GitHub refuses stops the command there, and is reported to the architect, never guessed.
  The head's check-runs
  (`gh api repos/<o>/<r>/commits/<headRefOid>/check-runs --paginate --jq '.check_runs[] | [.name, .status, .conclusion] | @tsv'`):
  every required name present, `completed` and `success` (GitHub counts `neutral` and `skipped`
  as passing for a required check). A required check
  `queued` or `in_progress`: wait. One `completed` as `cancelled`, or absent: on a `pull_request`
  run, re-run that run's
  failed and cancelled jobs, once, then wait — never the whole run, which runs again every job that
  succeeded too. A merge-group run is never re-run: the implementer finishes its round instead,
  and the pull request is tested again when it is queued again.
  Find the run and job at the head: the head's runs
  (`gh api 'repos/<o>/<r>/actions/runs?head_sha=<headRefOid>&per_page=100' --jq '.workflow_runs[] | [.id, .name, .event, .status, .conclusion] | @tsv'`),
  then each candidate run's jobs
  (`gh api repos/<o>/<r>/actions/runs/<run-id>/jobs --jq '.jobs[] | [.id, .name, .status, .conclusion] | @tsv'`).
  A cancelled job of a pull-request run: `gh run rerun <run-id> --failed`, which GitHub applies to
  the run's failed and
  cancelled jobs and the jobs that depend on them, and to nothing that succeeded. Never
  `--job <job-id>` alone: under a routing workflow, re-running only the final job reads the
  cancelled results again and stays red. GitHub refuses a re-run while an attempt of that run is
  still in progress: wait for it. When no
  run at the head holds the job, or nothing will run it (a check only a commit status reports, a
  workflow another repository defines), tell the architect and do not approve.
  Approval is refused while any `before_merge` check of the issue's own Verification document has no pass standing at the head being reviewed: `REQUEST_CHANGES` naming each such check by its block id, and `dispatch results --issue <issue>` is the read that decides it, under the check's own line saying whether a pass stands and from which roles. That answer leaves out a pass at another commit, a pass recorded under an older wording of the check and a retracted pass, each of which the command still prints; a fail recorded after a pass leaves that pass standing until someone retracts it.
  A check carrying `ci_check` is CI's: the one result that `dispatch results` derives from the named run decides it (`skill://legion-worker/references/ci-checks.md`). While that run has not concluded, wait for it by its name, and do not approve or request changes for that check. Wait the same way where GitHub could not be read. When no run of that name answers after the head's checks settle, tell the architect as a finding and request no changes for it.
  An issue with no Verification document, or whose Verification document holds no `before_merge` check, is a gap you name in the review body, never a review that asks for changes: say which of the two it is, and tell the architect, who writes the checks. Your own findings and the checks that do exist still decide the review, and on an issue that has none the architect's acceptance is the gate.
  A result proves the head it was recorded at. A result a child recorded against a root's check proves nothing for the root, since it was recorded at the child's head, and a child never records the result that proves a root's check. A child that carries part of a root's requirement proves it with a check on the child's own Verification document whose `serves` names that requirement, at the child's own head. A root's own checks are proven at the root's own head, or, for an `after_merge` check, on the branch the root's pull request merged into or on the repository's default branch, in the root's last round.
  A passing result stays current across one push above the head you approved when that push changes no code that runs: read that push's own diff (`gh api repos/<o>/<r>/compare/<approved sha>...<new head>`), approve the new head, and say in that approval that you read the diff, that it changes no code that runs, the head each result ran at and the head you approve. The carry rests on the diff and on your statement of it, never on the push being called a text fix: where that push's diff changes code that runs every result is stale, as it is after any other push, so you tell the architect and the architect moves the issue to `testing` with `request_backward_move`, since a review submitted outside `reviewing` moves nothing.
  On a docs-only PR the `Thermo` line still records the simplify pass and the defects it
  reported, and its pair half reads `thermonuclear pair: not run (docs-only)`.
  Submit **one review per round**. A reviewer submits `REQUEST_CHANGES` only when a behavioural
  finding stands — one whose closing changes code that runs — and otherwise approves the head it
  read, named by SHA, leaving every text finding as the inline review thread it already writes.
  A push after your approval lands in one of two places. While GitHub has not yet reported the
  approved head mergeable the issue is still in `reviewing`, and the push leaves the approval
  standing for nothing, since any push may change code: the round goes stuck and your architect
  is told (`ApprovalStands` and `ApprovalSetAside` in
  `packages/daemon/internal/classify/decisions.go`, `stuckApprovedOtherCode` in
  `packages/daemon/internal/workflow/review.go`). The case "a push since the approved head, then
  an approval of the head" of `TestARoundItsApprovalCannotEndTellsTheArchitect` in
  `packages/daemon/internal/workflow/review_test.go` asserts the state that push leaves: with your
  completion in and your approval of a head a push has replaced, the issue stays in `reviewing`,
  the architect is told once that the round is stuck at the new head, and an approval of the new
  head moves the issue to `accepting` once GitHub reports it clean. Once GitHub
  has reported the approved head mergeable, the approval has already ended the round and a push
  finds the issue in `accepting`, which the case "the approval of the head, then a push: the
  round had already ended" of `TestAnApprovalStandsOnlyForTheHeadItNames` asserts. The message
  starting the text-fix loop goes at your approval either way: step 1 of *After an approval that
  leaves text threads open* in `skill://legion-worker/references/review-threads.md` is that rule,
  and says what each of the two asks of you. Carry every inline comment in that single
  call: `gh api --method POST repos/{owner}/{repo}/pulls/{number}/reviews --input body.json`
  with `commit_id`, `event` (`REQUEST_CHANGES` or `APPROVE`), `body` (with the
  Legion footer, which names the head the review read:
  `<!-- legion: {"session":"<session-id>","phase":"review","head":"<the head you reviewed>"} -->`),
  and a `comments[]` array of `{path, line, side, body}`, one entry per
  finding — never one `pr review` call per finding (each submission fires a `pr-review` wake).
  That `head` is the full 40-character SHA of the head whose diff and checks you read, which is
  the `commit_id` you submit with, and never the head GitHub shows at the moment you submit:
  read `gh pr view <n> --repo <owner>/<repo> --json headRefOid --jq .headRefOid` before you
  submit, and where it names another head a push landed under you, so review that head and
  submit at it rather than renaming the footer to a head you have not read.
  The daemon takes that head as the review's own word for the code it read, and cannot take GitHub's
  `commit_id` for the review instead, which GitHub rewrites onto the pull request's current head,
  so its record of a review can name a head the review never saw.
  A review whose footer names another head, or no head, decides no round:
  it is set aside, the round stays open and the
  architect is told the reason when the review's own webhook arrived, so the reviewer submits again at the head it read.
  A `COMMENT` decides nothing. After a
  forward merge, compute the fingerprint (*The unchanged-diff check* in
  `skill://legion-worker/references/conflicts-and-rewrites.md`) at the
  `commit_id` of your last submitted review and at the new head. Equal and that review was
  `APPROVE`: submit one more `APPROVE` naming the new head by SHA, its body naming both SHAs
  and the fingerprint — a confirmation, not a round; no thermo pass, no thread pass. Equal and
  that review was `COMMENT` or `REQUEST_CHANGES`: continue that round against the new head;
  nothing restarts. Different, or either run printed no fingerprint: a new round — thermo again,
  one review.
- Answer every thread you opened, and every thread a bot opened that is none of Legion's role
  Apps, as `skill://legion-worker/references/review-threads.md` says; the same reference says
  when every thread is settled enough to approve, and resolving one never gates your approval.
- **Only a behavioural finding sends the work back.** A finding is behavioural when closing it
  changes code that runs: a defect in the change's own code, a change to such code you ask for (a
  maintainability or duplication finding, a rename of an identifier, and a `CHANGES` verdict of
  the code-quality pass whose findings are in code that runs, among them), a missing or weak
  test, a `before_merge` check of the issue with no current passing result, an unmet requirement
  of the issue's spec, or a secret or a private name in public text. Every other finding is text:
  a document, a comment, a changelog fragment, a census line, a name inside prose. When every
  finding that stands is text, approve the head you read, by SHA, and say in the approval body
  that the open threads are the work the merge waits for. Each of them stays the inline review
  thread you already wrote. The implementer fixes the text and pushes it, and you resolve the
  thread and approve the head that fix landed on, so you write no commit of your own for one.
- **After an approval with text threads open.** The daemon starts no phase for the loop that
  closes those threads, so each step is carried by a message its own role sends, and the first is
  yours: at your approval, whatever phase the record shows, `envoy_publish` to the implementer's
  role topic naming the approved head by SHA and each open thread by its node id, since nothing
  else tells it its turn began. Step 1 of `skill://legion-worker/references/review-threads.md` is
  that rule, and says what follows when the implementer's push lands in `reviewing` and when it
  lands in `accepting`. The implementer fixes the text in its own live session, makes one push of
  one commit, replies on each thread, resolves none, and publishes the new head and its compare
  to your topic. You then resolve each thread you opened, one call per thread, approve the head
  that push made, by SHA, reading that head's checks as you read any head's, and `envoy_publish`
  to the architect's role topic naming that head: that message is the architect's gate and its
  acceptance is the one gate on the fix head (`skill://legion-worker/references/review-threads.md`
  gives the loop and the calls). A thread the fix cannot close, and a fix whose diff changes code
  that runs, take one route: tell the architect, which moves the issue with
  `request_backward_move`, to `implementing` for the thread and to `testing` for the stale
  results. Neither you nor the implementer moves the issue (step 4 of that reference says when a
  `REQUEST_CHANGES` records the finding and when you submit none).
- **A text finding stays text where a test pins the sentence it fixes.** A check that is green at
  the approved head is not a `before_merge` check of the issue with no current passing result, so
  it asks for no changes; a text fix that breaks such a pin is caught at the head the fix made,
  which you read before you approve it.

A reviewer's phase ends with its completion, not with its review. Every round records a handoff
and takes this order: read CI as above; write the handoff and record it with
`dispatch handoff-write`; submit the review of the head, by its SHA; then complete. A red at the
code is a `REQUEST_CHANGES` naming the check, never an approval. A request for changes does not
wait, since it stands whatever CI says and the issue leaves reviewing with it. The approval waits
on no CI verdict either: what the head's checks concluded decides nothing in the round, and a
settlement the daemon reads later — at that head or any above it — moves the
issue nowhere. The daemon names each check GitHub reported, with the conclusion GitHub gave it,
in one message to the tree's architect, the worker of the issue's current phase and the
implementer, and judges none of them: a red is the implementer's to fix or re-run, the
architect's to decide on, and GitHub's to hold the merge for. So the reviewer's own read before
it approves is the only read that decides anything. A review that names an older head reviews
code the pull request no longer has. The daemon moves the issue once two things are in — the
approval GitHub reports, and GitHub's own answer that the approved head is one the issue may
advance on (`clean`, `unstable` or `has_hooks`) — and your completion is no part of that: it
records your round's handoff, whether it comes before the approval or after the issue has moved
(`TestTheReviewEventDecidesAndTheCompletionRecordsItsFindings` in
`packages/daemon/internal/workflow/review_test.go`). An approved head GitHub answers `blocked`
for — a check the base requires still running or failed, or a required conversation resolution
with threads open — leaves the issue in `reviewing` until that answer comes clean.

## The architect's acceptance

- Between the reviewer's approval and retro the issue sits in `accepting` (Dispatch status
  `needs_review`), where no worker is started: the tree's architect reads the evidence (the
  tester's handoff, `dispatch results --issue <issue>` at the approved head and the reviewer's
  approval of the head by SHA; the daemon refuses the sign-off while an issue beneath it is still
  in the workflow, `SIGNOFF_CHILD_IN_WORKFLOW`, so a parent is accepted after its children, on
  the head of its last round, `skill://legion-architect`, section 4) and accepts with the
  `legion` tool's `sign_off`, which moves the issue to retro, or sends it back to `implementing`
  or `testing` with `request_backward_move` and a reason, which reaches the started worker's task
  as its `Reason:` line and counts a review round. The acceptance comment names each check with
  its result. An issue with no Verification document, or none holding a `before_merge` check,
  is a gap the tester and the reviewer named and neither refused, so that comment is its only
  gate: it names the gap and decides how each delivered item is proven at this head, never
  whether one stays unmet; checks it writes close the root's `criteria` step where the
  deployment gates it, since that step records the root's Verification document, and a child's
  is gated by nothing. It does
  not accept while the issue holds a finding ask no controller took (`findings.md`, "When no
  controller takes it"). Approval is gated by the `before_merge` checks of the issue's own
  Verification document at its head, so a child's result against a root's check proves nothing:
  a root's checks are proven at its own head, in its last round. No `after_merge` result is
  asked here: it is recorded after the close, where the project opens a check (*The production
  check after the close*, below).
  CI red at the head in `accepting` moves nothing, as it moves nothing anywhere: the daemon names
  the checks GitHub reported and leaves the decision to the roles it told. Read the head's checks
  on GitHub yourself before you accept, and send a red head back with `request_backward_move`.

## Retro

- **Retro commits nothing.** It reads the evidence, writes what the work taught as a
  `dispatch comment` on the issue for the skill librarian, brings the PR body up to date and
  completes (`skill://legion-retro` gives the steps), so the approved head is the head the merger
  merges. Nothing in Legion compares that head with the one the approval named either: a
  repository that wants a push made after the approval reviewed says so in its own rulesets (a
  stale review dismissed, a review required again), and GitHub holds the merge to them. A
  conflict merge pushed above the approved head still goes back to the reviewer for a
  confirmation or a new round, as the fingerprint decides
  (`skill://legion-worker/references/conflicts-and-rewrites.md`), because the branch's own
  procedure says so and not because any Legion check refuses the head. Retro removes nothing.
- **Retro brings the PR body's path-derived content up to date before it completes.** Whatever the
  repository's instructions derive from the pull request's changed paths (a checklist named for
  each class of path, read by a required check), retro recomputes for the whole diff at its head
  and writes into the live body before it completes (`skill://legion-retro`), since the merger
  reports a stale body rather than rewriting it. A body edit changes no commit, so the approval
  stands.

## The merger

- The merger resolves no review thread: the reviewer's approval already judged them, and the
  merge requires no thread state beyond it. It writes no file, commits nothing and pushes
  nothing.
- **The merge is the merger's own act, at the head it read.** It reads the pull request
  (`gh pr view <n> --json number,title,url,state,baseRefName,headRefOid,mergeStateStatus`), and
  every step below names that `headRefOid`:
  - *The message, before the merge.* GitHub's default squash message,
    `squash_merge_commit_message` of `COMMIT_MESSAGES`, joins every commit message of the pull
    request, which puts the tree's `Dispatch:` keys and its `Omp-Session:` trailers on the
    default branch. So the merger writes the message itself,
    `--subject '<pull request title> (#<n>)' --body ''`, on every merge, whatever the repository's
    setting says. It reads that setting first all the same
    (`gh api "repos/<o>/<r>" --jq '[.squash_merge_commit_title, .squash_merge_commit_message] | @tsv'`),
    because the merge-queue path below can pass no message.
  - *The merge.*
    `gh pr merge <n> --squash --match-head-commit <head> --subject '<pull request title> (#<n>)' --body ''`,
    as the implement App, so
    GitHub refuses the merge once the head has moved under it: merging at a named head is how bors
    and Prow's Tide merge only what they tested. On a base with a merge queue — a `merge_queue`
    rule among the base branch's rules — it enqueues the pull request instead, with GraphQL
    `enqueuePullRequest` and the same head as `expectedHeadOid`, because `gh pr merge` on such a
    base turns auto-merge on while a required check is pending, which arms the pull request rather
    than queueing it. `enqueuePullRequest` takes no message, so where that base's
    `squash_merge_commit_message` reads `COMMIT_MESSAGES` the queue would commit the branch's
    messages and nothing the merger passes can stop it: it enqueues nothing, completes nothing,
    and tells the architect, naming the setting and that only a person can change it to `PR_BODY`
    or `BLANK`. It never turns auto-merge on; the daemon submits no merge and changes no
    merge setting of any pull request.
  - *The watch.* On a merge-queue base it reads the pull request until GitHub merges it or takes
    it out of the queue, and acts on a removal: a head the queue took out over its own
    `merge_group` checks is work to fix, so the issue goes back; a person's dequeue is that
    person's decision, reported to the architect. A removal is never a merge.
  - *A failed merge is the merger's decision.* Wait for what GitHub waits for (a required check
    still running, a required review only a person can give) and merge again; merge again at the
    head it re-reads when a push moved it, or when GitHub refused while it was still computing
    mergeability; send the issue back with `request_backward_move` when the head itself must
    change (a conflict with the base, a check that failed); or tell the architect what no
    worker can do — a repository setting or an App permission only a person can change — with
    GitHub's own answer quoted. The repository's required reviews and checks decide what may
    merge: Legion bypasses none, and a review only a person can give is waited for.
- *The completion* is `handoff_complete` with the ordinary two-sentence `summary`, which names the
  pull request, the head merged and the merge commit. It records the merger's phase row and moves
  nothing: no Dispatch message, no publish, no phase. What moves the issue is the daemon's own
  reading of the merged pull request, which writes `done` — before the completion or after it,
  whichever comes second. The daemon refuses that completion only as it refuses any phase
  worker's: `HANDOFF_NOT_CURRENT_PHASE` (the issue left `merging`), `HANDOFF_STALE_GENERATION`,
  `HANDOFF_NO_RUN`, `HANDOFF_ALREADY_RECORDED` and `TREE_LINGERING`. It reads neither the head,
  nor its checks, nor the approval: every one of those reads is the merger's own, above.

## The production check after the close

- **The merge closes the issue; the production check is the work of the role the project names,
  after the close.** `projects.<KEY>.production_check` in the project's `legion.yaml` takes `off`
  or `tester`, and a project that names no key gets `tester`, so by default the tester works the
  check; the loader refuses every other value, naming the key and the value it read. Where the
  setting names the tester, the daemon reads that merge, writes `done` and hands the check task
  to the tester's live session. Where it is `off` nothing is opened at all — no record, no
  bound and no look — and the merged close is like any other done: every claim of the issue is
  retired and its pod released with the disk it ran on. A merged root still lingers its tree with
  every claim suspended, as before. On a root whose tree still runs, the merge is recorded and
  the root stays in `merging` on its merged pull request until the last other issue of its tree
  is done or parked, the check opening with that close. Its first line, which tells it from a
  phase task in a resumed session, is `Production check of <KEY>: <title>.`, naming the
  repository, the pull request, the merge commit, the look's number, its wake (`the merge`, for
  the first) and whether it is the last.
- **The check's record holds the role that works it, written when the check opened.** Changing the
  setting moves no check already open: a check keeps the role it opened with, and a check open
  before this release carries the implementer. Only that role may report on it, so a report from
  any other role is refused (`CHECK_ROLE_MISMATCH`). The check carries no phase and no handoff: a
  look ends with `handoff_complete` carrying a `verdict` and a `summary` and no commit, and the
  daemon refuses a report with no open check (`CHECK_NOT_OPEN`), on an ended look
  (`CHECK_LOOK_NOT_OPEN`) or with no verdict
  (`CHECK_VERDICT_REQUIRED`).
  One refusal is about the turn rather than the report: a completion sent from a turn that is not
  a look's own task is refused `CHECK_LOOK_NOT_SERVED` and records nothing, so report again in the
  turn of the look's task.
- **Each look does three things.** Do what the deployment instructions say to reach the deploy:
  when they name an agent or role for it, `envoy_publish` the merge commit to that role's topic
  with `expects_reply: false`, and await no reply. Read whether production carries the merge, and
  with it every requirement only the deploy can show, each as the requirement names it, since
  observing something else is never that requirement observed. Then
  report one of three outcomes:
  - `pass`, once you drove the changed path and those requirements in production through the
    user's own access path and
    recorded the result: `dispatch result` against the issue's `after_merge` check, at the merge
    commit, its `observed` ending `woken by <event>`, the printed line in the PR body's `Checks`
    block and one pull-request comment with the Legion footer. A closed issue takes a result for
    an `after_merge` or `default_branch` check, a comment and a message, and refuses every other
    write. A staging pass is not this: staging does not run every resource production does.
  - `pending`, when production does not carry the merge yet (a shared profile still holding the
    previous release, a daemon still running the previous commit, a deploy nobody has run), or a
    requirement's observation has not happened: the summary names what is missing and what you
    will look for next, `wake_at` names your next look (below), nothing is recorded, and the
    check's line in the body stays at
    `- Check <block id> (after_merge): not yet run`.
  - `fail`, for a requirement observed as not met, one still unobserved at the last look, or a
    defect: record the failing result with `dispatch result --verdict fail`, file no issue, and
    take the path `skill://legion-worker/references/findings.md` gives it — a clear fix to the
    controller topic, with its evidence comment on the controller's report issue; one needing a
    decision as a `dispatch ask` there, whose reference you publish to that topic. Either names
    this issue, the pull request, the merge commit, what you drove and what failed, and the
    result's `observed` and the summary name it (`dispatch://<REPORT KEY>/ask/<id>`).
- **Numbers the daemon holds are read with the `legion` tool.** Where the verdict rests on what a
  deployment's idle release has done, `read_release_records` answers a window of the daemon's own
  records of its releases, resumes and holds: its one argument `since` is the RFC 3339 instant
  the window starts at, and the tool's own description says what the answer carries. It reads
  them with this session's own claim, so there is no credential to fetch and none on disk.
- **You name your own next look, and nothing else wakes the check.** A `pending` on any look but
  the check's last carries `wake_at`, an RFC 3339 time: when you want to look again. Name a time
  you have a reason for — when the deploy you asked for should have run, when the next release
  lands — and say in the summary what you are waiting for, since that summary is what the next
  look's task tells you woke it. A `pending` with no `wake_at` on a look that is not the last is
  refused (`CHECK_WAKE_REQUIRED`): nothing is recorded, the look stays open, and the same report
  with a time is taken. A `wake_at` that no look follows is refused (`CHECK_WAKE_UNEXPECTED`): on
  a `pass`, on a `fail`, and on a `pending` on the last look, each of which ends the check. A time
  less than 60 seconds ahead becomes 60 seconds ahead, and a time at or after the check's own
  bound starts no look of its own, since the look at that bound already stands. No deploy run, no
  Dispatch `issue.deployed`, no Dispatch build and no daemon boot wakes a check: the daemon reads
  no deployment's production workflows at all, and your `wake_at` is the only thing that starts
  the look after this one.
- **Nothing waits in the session for a deploy.** A `pending` ends the look: the daemon stops the
  checking role's process, keeps its session and releases the issue's pod, whose Sandbox goes with
  its disk, and at the time you named a new pod comes back and your session resumes with the next
  look's task. So end the turn with the report: never poll, never wait on a reply, never ask for
  approval to deploy. Open a `dispatch ask` only where the instructions name nobody: the
  production gap and why it matters, then the required install or restart step, its risk, and
  outcome-named options; the look still ends `pending`.
- **The bounds.** A look is bounded at 2 hours, and a look nobody reported — its 2 hours, an
  operator's hold, or a claim that failed to start — starts the next look two hours later; on the
  last look it closes the check as not run. The check is bounded by the project's
  `production_check_hours` (336, 14 days, by default) from the close; the look that bound starts
  is the last, so told in its task
  (`the bound: production did not carry the merge within <N days>`). A `pass` or `fail` there
  closes the check as any look's would, and a requirement still unobserved there is that `fail`;
  only production not carrying the merge ends it `pending`, which carries no `wake_at` and
  closes the check as not run, written first on the pull request — its check line and a comment
  — because production did not carry the merge within the bound.
- **The record lives on the pull request and in the result.** Dispatch takes a comment, a message
  and a result for an `after_merge` or `default_branch` check on a closed issue, and refuses an
  ask there (`ISSUE_CLOSED`), so the deployment instructions' "on the pull request and the issue"
  is the pull request and that result, and a defect reaches Dispatch by the `fail` verdict's two
  paths alone — the message to the controller, which gives it to the tree that owns that code,
  and the ask on the controller's report issue. The architect accepted before the
  merge and is asked nothing after. `pass` or
  `fail` closes the check: your claim is retired and the issue's Sandbox goes with its pod and
  disk, for a root as for a child. Neither ending retires or suspends anything of an issue back in
  the workflow (a root reopened inside its linger): its claim and pod are the new run's.
