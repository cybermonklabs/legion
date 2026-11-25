# Your own steps and the retro

Part of `skill://legion-architect`, sections 5 and 6.
Read it when a step of your issue's plan has the key `owner`, and when its task has no step of the type implementation.
Read it also when a retro reports to you.

## The retro

- The retro is done by the agent of the task's last step of the type implementation, as that step's role.
  Where the task has no such step, or that step's key is `owner`, it is yours: you do it as `skill://legion-retro` says.

- A retro that reports a defect holds its completion until you say whether it moves the issue back.
  Only retro can do that in its phase.

- Retro's output is in `skill://legion-retro`, steps 3 to 5.
  When it reports that it cannot compute, read or write the pull request body, it is still in its phase.
  Answer it on its role topic from the deployment instructions, or open a `dispatch ask` naming the missing instruction.
  The architect that holds a child opens that ask on the child's own issue.

## Which steps are yours

- A step whose key is `owner`, of any role and at any place in the task.
  It is `implementer:owner`, `tester:owner`, `reviewer:owner`, `acceptance:owner`, or a role of the deployment or of the plan with the key.

- The retro of a task with no step of the type implementation, or whose last such step is yours.

## How Legion gives you a step

- Legion starts no agent for your step.
  It sends you one `owner-step` notice: its role is the step's role (none for the retro).
  Its phase is the phase the issue stands in, and its reason the task a worker of that step would get.

- The notice comes only while that step runs, once your slot's gh files hold the GitHub App of the step's role.
  That includes your own App, the review App: for a planner, tester or reviewer step, acceptance, and a retro with no implementation step.
  Where the task's last implementation step is yours, its retro waits for that step's App, the implement App, as the step did.
  Legion writes the slot first only for another App.
  While you do the step, your slot acts on GitHub as that App, so an implementer step opens its pull request as the implement App.
  A merge forward asked of you is the one exception, below.
  At the end of the step or of such a retro, Legion writes your own App back to your slot.
  It writes nothing where your next step, retro or merge forward of another App already runs.
  A notice that comes after the end waits for that write.

- While the step runs, Legion tells it to you again in the same words when you stay idle past the silence bound.
  It tells it again at a launch only where your session has not had it, as a fresh session after a lost one has not.
  A launch that resumes the session that had it is told nothing more.

- A retry of your claim at your step (`legion claims retry`) relaunches your session, and Legion tells you the step again.
  It starts no worker.
  While an `owner-step` of that run is still on its way to you, the retry tells you nothing more.

- Where the task's implementation step is yours, the daemon's ask to merge the base forward comes to you, in any phase before `merging`, where the merger is asked instead.
  That merge is the implementer's act, so your slot holds the implement App until the ask ends.
  It ends when its merge lands, or the pull request closes or merges.
  It also ends when the issue moves into its implementation step, leaves, is held, or its task stops.
  It ends too when the issue is set back to `todo` or adopted into another tree.
  A newer ask that reaches another agent ends it as well.
  Each end writes your own App back to your slot, as a step's end does.
  Where your tree lingers, nothing is written, and your next launch writes it.
  Until the ask ends, Legion tells you no step or retro of another App, such as your review step or your acceptance.
  Where you also work the step the issue stands at, the ask is the one merge-forward message you get.
  Your step then waits for the merge and goes on at the head the merge leaves.
  A review of a landed merge can be asked of you while you also work the step the issue stands at.
  That review is then the one merge-forward message you get, and you go on with your step at the new head.
  You get no landed message for a merge you pushed yourself.
  A `sign_off` or a `complete_step` is a call to the daemon, not to GitHub, so your slot's App does not change it.

## Reporting it

1. Do the work the task names, as a worker of that role would.
   Before you report a planning, implementing, testing or reviewing step, write its handoff record with `dispatch handoff-write`.

2. Report it with `legion({ op: "complete_step", issue: "<KEY>", step: <n>, summary: "<two sentences>" })`:
   `step` is its place in the task, 1 for the first step, and 0 for the retro.
   Add what that role's completion carries: the `verdict` `pass` or `fail` where the role reports one, and `return_to` with a `fail`.
   On the retro, add `plan_rating` and `plan_reason` (`skill://legion-retro`, step 6).

- At your acceptance step, `pass` is your sign-off, and `fail` sends the task back.
  It goes to `return_to`, or to its first step of the type implementation before the acceptance.
  A `fail` that names no `return_to` where no such step stands before it stops the task, and Legion tells you (`task-stopped`).
  `sign_off` and `request_backward_move` still work there.

- The daemon refuses, changing nothing, a report from any architect but the issue's own (`NOT_THE_OWNER`).
  It also refuses, changing nothing, a report of a step that does not run now with you as its agent (`OWNER_STEP_NOT_RUNNING`).
  That refusal names where the task stands and who works that place.
  It also refuses, changing nothing, a report that breaks that step's completion rules, with the codes `handoff_complete` gets.
  `handoff_complete` stays refused to you.

## Your review step

- `reviewer:owner` ends by your `complete_step`, with the `verdict` `pass` or `fail`, whether or not the issue has a pull request.
  Write its `review` handoff first, its `decision` `approved` for `pass` and `changes_requested` for `fail`.
  The tool sends that record's `head` as the commit you read, so you pass no commit.
  No review on GitHub decides it, yours included.
  Your `fail` sends the work back as a `request_backward_move` from that step does, and neither counts a review round.

- A review you leave on the pull request decides nothing of the step.
  It serves the repository's own merge rule, such as a required approving review, which you give yourself.
  After a forward merge where your reviewer step already ran (in the default steps, in `accepting`, `retro` or `merging`), the daemon asks you to approve the new head on GitHub.
  GitHub refuses an approval from the pull request's author, so that ask waits until your slot holds the review App.
  While your slot holds the implement App, it waits for the end of your retro or of the merge forward asked of you.
  A merger that enters `merging` while that approval is owed waits for it.

## A task with no pull request

- An issue with no pull request, such as one whose plan is `tester` alone, has no merge.
  Its retro's end closes the issue (`done`), no merger starts and no production check opens.

- Read its evidence from its steps' reports, its results (`dispatch results --issue <KEY> --at <head>`) and the working copy its task names, never from a pull request.
