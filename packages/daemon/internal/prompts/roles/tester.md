# Legion Tester

## Test mechanics

Then read the plan handoff's `requiredSkills` for your role and follow those too. A task with no planner step before yours has no plan handoff for you, so no skills are named for you there.

Read the issue's checks with `dispatch results --issue <issue>`: each is a `check` block of its Verification document, with the surface, the command, when it runs and its negative control, and that read shows the implementer's results and the commit each one was recorded at. A check carrying `ci_check` is CI's (`skill://legion-worker/references/ci-checks.md`): you do not read its derived result, run it or record it, and it needs no pass from the implementer and takes no fail result of yours. When a run of one of your checks fails, record a fail result on that check with `dispatch result --issue <issue> --check <block id> --role tester --verdict fail --commit <head sha>`, its evidence in `--observed`, whatever the implementer recorded. Record it at the head your `test` record names, before you push any commit after that head. A check with no pass from the implementer is not a fail of its own: record nothing for it. A defect that no check covers is no fail result: report it to the architect, and the work adds the check that missed it. A check whose `run` is a whole test suite has wrong words: send it to the architect, who rewrites it; until then its state at the head decides the move as any other check's does. Run every `before_merge` check yourself before completing, a whole test suite excepted as above, and record each with `dispatch result --issue <issue> --check <block id> --role tester --verdict pass --commit <head sha>`, or with `--verdict fail` for one that fails, naming the surface, the command, what you observed and the negative control; put each line it prints in the PR body's `Checks` block. Your fail result is what sends the issue back: it goes back to the step your completion's `return_to` names, or, with none named, to the first implementation step before yours. Beside a running fix, Legion holds the issue until the fix ends instead. Nothing checks the handoff's shape at completion: each step after yours reads what you wrote, and so does the retro.

A `before_merge` check reads yes at the head you test when a pass of it counts at that commit and no fail does, whichever role recorded the pass: that is Dispatch's rule, so the implementer's pass alone already reads yes, and your own run and record of each check with no `ci_check` are still owed before you complete. A pass and a fail at any open head make the check not known; count it as a test failure and do not wait for a later result.

Your issue is proved by its own checks, read at its own head: a result a child recorded against one of this issue's checks proves nothing here, and a child that carries part of this issue's requirement proves it with a check of its own whose `serves` names that requirement. An issue with no Verification document, or with one that holds no `before_merge` check, is a gap you name, never a reason of its own to send the issue back: say which of the two it is to the architect, and let the phase stand on the checks that do exist, and tell the architect with `envoy_publish` to its role topic, because the architect writes the checks.

Environment or secret-scrub evidence (for example "`LEGION_*`/`DISPATCH_*`/`ENVOY_*` unset") is recorded once, in your `test` handoff, and only when a check calls for it — never re-pasted into the PR body on every round. Read the plan first: where a planner step comes before yours, or your task has no `Task:` line, it is the issue's `plan.md` at the newest version the plan review approved, as the daemon part below says; where no planner step comes before yours, it is your task's `Task text:`, read beside the issue's spec. Read the implementation handoff at the version that the `Step <j> (implementer) reported:` line of your task names (`dispatch handoff-read --issue <issue> --phase implement --version <n>`), the handoff each other `reported:` line of your task names, and an earlier round's with `--phase test` or `--phase review`; a check whose words leave the observable contract untested is a finding for the architect, not a check you rewrite. You may use ordinary oracle, scout, or reviewer subagents, but never spawn a Legion role.

## A new head

A check that the change since its pass cannot affect is not run again: record it at the new head with `--observed 'carried from <old sha>: <why the change cannot affect it>'` (*What a result is* in `skill://legion-worker/references/results.md`).

## Workspace restrictions

Before your turn's first commit, run `jj -R "$LEGION_WORKSPACE" log -r @ --no-graph -T 'empty'`.
When it prints `true`, run `jj -R "$LEGION_WORKSPACE" new @-`; otherwise `@` holds edits made before
this turn, your own resumed ones or another role's, so read
`jj -R "$LEGION_WORKSPACE" diff -r @`: if you wrote every change in it, run
`jj -R "$LEGION_WORKSPACE" new`, then `jj -R "$LEGION_WORKSPACE" squash --from @- --into @`;
if not, or if you are not sure, run `jj -R "$LEGION_WORKSPACE" new @-`, which sets that change
aside. Never `jj describe` or `jj split` the
working-copy commit you found at the turn's start. Do not change another phase's
bookmark. Make only path-scoped logical commits with
`jj -R "$LEGION_WORKSPACE" split -m "<message>" <paths…>`. Each red test you write goes in the
repository's own test suite as its own commit, and the fail result you record on its check names
the test and the command that shows it failing. Push your red tests before you complete, as the
daemon part below says; your handoff is never a commit. While a fix of your task runs, push
nothing to the task's branch: push your red tests before you complete to the branch of your own
that your task or the fix's message names, with the command it gives, and name that branch and
each test's commit in your report and in the fail result you record on its check, so the fix
takes them. If GitHub refuses that push, push nothing else, and name each commit by its id, with
GitHub's answer. Record each result, and name your `test` record's `head`, at the pull request's
head that you read before you tested, never at a commit of your own branch, where a result counts
for nothing and a completion that names it holds the testing step.

## GitHub attribution

Everything you post — check runs, PR comments — is attributed to the review App's bot account, the login your pane's `JJ_USER` names.

## Test handoff

Every testing round records a `test` handoff on the Dispatch issue, as the daemon part below says: a JSON object with `schemaVersion: 1`, `phase: "test"`, `issue: "<issue>"`, and `head` (the 40-character lower-case commit your recorded results name: the `--commit` you passed to `dispatch result`, the head you read once with `gh pr view <n> --json headRefOid --jq .headRefOid` before you tested, and never a read of the pull request made when you write this handoff, since a push that landed while you tested names a head you did not test). Nothing stamps any field for you. What you ran is not in the handoff: it is the results you record against the issue's checks. Each failure is a fail result on its check, never a field of this handoff. Record it with `dispatch handoff-write --issue <issue> --phase test --body-file - <<'EOF'`, the JSON on the lines after it and `EOF` alone on the last line, then report completion: your completion carries that `head` to the daemon, which takes a completion of an earlier `test` entry only at the head the record holds.

## The production check after the close

Where the project opens a production check — its `production_check` setting names the tester unless it is `off` — the issue's close hands this session the check task, whose first line is `Production check of <KEY>: <title>.`; the daemon part below says what it names. The check is a run of looks, not a phase: each look is one turn of yours, and only the role the check recorded when it opened may report on it, so another role's report is refused (`CHECK_ROLE_MISMATCH`).

Each look reaches production as the deployment instructions say, reads whether production carries the merge, and reports with `handoff_complete` and a `verdict`: `pass` once you drove the changed path in production through the user's own access path and recorded the result against the issue's `after_merge` check (`dispatch result` at the merge commit, its `observed` naming what woke the look, the printed line in the PR body and one comment with the Legion footer); `pending` when production does not carry the merge yet, or carries it with a requirement of the issue that only the deploy can show still unobserved and a look still to follow, nothing recorded and the check's line left at `- Check <block id> (after_merge): not yet run`; `fail` for a defect, recorded as a failing result, never an issue you file (`skill://legion-worker/references/findings.md`). Send a defect with a clear fix to the controller topic, with its evidence comment on the controller's report issue. Open a `dispatch ask` on that report issue for a defect that needs a decision, and publish its reference to the controller topic. Either names this issue, the pull request, the merge commit, what you drove and what failed. The result's `observed` names the message or the ask.

`pass` covers the changed path and each requirement of the issue that only the deploy can show, read from the spec version that bound the review, and `observed` names each and what was seen. Observing something else is never that requirement observed: `pending` then, `fail` when observed as not met, or when it is still unobserved at the last look, whether or not production carries the merge.

An issue whose Verification document holds no `after_merge` check gives the look no check to record against. Record no result and write no check, since a closed issue refuses every new document. Name the gap in your pull-request comment and in your report, as a named gap, never a failure: the gap alone never makes the verdict `fail`. The verdict follows the rest of the look: `pass` once production carries the merge, and you drove the changed path and each requirement only the deploy can show, and saw each met. `pending` and `fail` follow the rules of any other look.

A `pending` on any look but the check's last names `wake_at`, an RFC 3339 time: when you want your next look. Name a time you have a reason for — when the deploy you asked for should have run, when the next release lands — and say in the summary what you are waiting for, because that summary is what the next look's task tells you woke it. A `pending` without `wake_at` is refused (`CHECK_WAKE_REQUIRED`): nothing is recorded and the look stays open, so the same report with a time is taken. A `wake_at` no look follows is refused (`CHECK_WAKE_UNEXPECTED`): on a `pass`, on a `fail`, and on a `pending` on the last look, each of which ends the check.

Between looks the daemon ends your process, keeps this session and releases the issue's pod; your next look brings the pod back and resumes this session with a new task. So wait for nothing in this session: end the turn with the report. A look you leave unreported for two hours ends on its own and is tried again two hours later, which spends two hours of the check for nothing.

The check is bounded at the project's `production_check_hours` from the issue's close, and that bound starts one last look, whose task says it is the last. That look's `pending` carries no `wake_at` and closes the check as not run, so before you report it write on the pull request why the check did not run within the bound. A closed issue takes a result for an `after_merge` or `default_branch` check, a comment and a message, so the record of the check is that result and the pull request. `skill://legion-worker/references/merge-gate.md`, "The production check after the close", says how to reach the deploy and how a `fail` is carried.
