---
name: legion-retro
description: "Use when Legion gives you a task's retro, as `Phase: retro` in your task or an `owner-step` notice of the retro, after the task's last step passed."
---

# Legion retro

Retro runs between the task's last step and before the merger's merge.
With no pull request, its end closes the issue.
The architect's acceptance precedes it only where the task names `acceptance:owner`.
`<issue>` is your `LEGION_ISSUE`.
The agent of the task's last step of the type implementation does it, as that step's role.
Where there is none, or it is the owner's, the owner does it and completes with `complete_step`, `step: 0`, where this skill says `handoff_complete`.
Where the retro is yours, what this skill sends the architect is yours to act on.
Run every step, even on a mechanical change.

For push and review rules, read `skill://legion-retro/references/pr-body-content.md`, "Merge and review".
Never rebase the branch or make another branch or bookmark.
Retro writes no handoff.
In the workspace retro itself changes nothing.
Outside it, retro writes only step 4's `mktemp -d` directory, the PR body, the Dispatch comment and message, and, with no reviewer step, the review-thread replies and resolutions of step 1.
It sets no GitHub or Dispatch label.
What the work taught is a `dispatch comment` for the skill librarian; no role writes to `docs/solutions/`.

## When retro stops

When a step fails (the body cannot be computed, read or written, or Dispatch refuses a write), do not call `handoff_complete`:
send the command and its output to the architect's topic on your `Legion addressing` line.
A defect you find in the work, other than a bot thread that step 1 sends back, goes to the architect with its evidence, and you hold your completion until it answers:
while retro holds the phase, `request_backward_move` is yours, and `handoff_complete` takes it away.
End each turn of that hold with a WAITING line naming the defect and the answer you wait on.

## Steps

1. **Read the evidence**: the issue, `dispatch results --issue <issue> --at <head>`, the pull request where there is one, the thread of each plan task in `plan.md`, and each handoff where the task has one: `dispatch handoff-read --issue <issue> --phase implement|test|review` reads the last, `--version <n>` an earlier one; your `Handoff:` line names the last worker step's.
   Read the GitHub review of the last step's head where there is a pull request and a reviewer step.
   Deployment roles write none; see `dispatch steps --issue <issue>`.
   A pass must stand at the head the last step passed for every `before_merge` check, and, with a pull request, the body's `Checks` block carries each.

   - A check carrying `ci_check` is CI's: its derived result is the pass it needs (`skill://legion-worker/references/ci-checks.md`).
     A `ci_check` check has no pass standing at that head when its derived result is a fail, its run concluded `skipped`, `neutral` or `cancelled`, or it names a job held back without its label, whose run is `absent` or `skipped` until the label is on.
     A run that has not concluded, or one GitHub could not be read for, is neither a pass nor a fail yet.
     Never wait for, watch or poll it, as your role prompt says of CI.
     Name each run not concluded, with the time you read it, in step 5's `Runs still going` line, and go on.
     When CI settles, the daemon tells the architect and the worker of the phase the issue then stands in.
     Send any other name no run answers (`absent`) to the architect as a finding: the check's words are wrong.
     Neither that run nor that finding moves the issue back.
     A `no_head` check can never pass: it is a defect, held as *When retro stops* says.

   - A `ci_check` check whose derived result is a fail is the merger's: the change passed every step of its task, and the merger decides at the merge whether the base or the change broke it.
     Name it in your retro message and complete.

   - A check whose `run` is a whole test suite has wrong words: send it to the architect as a finding, not the work back.

   - Any other check with no pass standing: `request_backward_move` it.
     Use `to: implementing` for the implementer's result or `to: testing` for the tester's, where the task has that step, else send it to the architect.
     Name the check's block id in the reason.

   - With no reviewer step and a pull request, list the unresolved bot threads and answer the ones you find, as `skill://legion-worker/references/review-threads.md` says.
     For a thread that needs a fix, `request_backward_move` with `to: implementing`, the thread's URL in its `reason`.

2. **Get a second reading.**
   First list in your session the constraints the issue did not state, choices between two working options, abandoned approaches and reusable code.
   Then run `task(agent="oracle")` with the issue and the PR body and diff, or, with no pull request, the steps' reports and results.
   Ask it for reusable learnings, and `wait` for its answer.
   Drop a point that cites no file, symbol, quoted text or commit of this pull request, or of the task's work where it has none.

3. **Record the learnings** as one `dispatch comment` on the issue, per `skill://legion-retro/references/learnings.md`.

4. **Update the PR body**, where the issue has a pull request, per `skill://legion-retro/references/pr-body-content.md`:
   the lines the repository derives from changed paths, where it derives any.
   With no reviewer step, record step 1's bot threads in `Threads` at the head you found, and bring `Not proven / risk` up to date from each tester step's handoff where the task has one, naming each check whose run has not concluded (`skill://legion-worker/references/pr-body.md`, "The brief for the human").

5. **Post one Dispatch message on the issue**, at most 32,000 characters (never a `gh issue` write):

   ```bash
   dispatch message --issue '<KEY>' --body-file - <<'EOF'
   ## Retro Complete

   **Learnings:** the comment of step 3 (`dispatch://<KEY>/comment/<id>`), or none: <why no point qualified>.

   **Key takeaways:**
   - <one to three reusable lessons>

   **Results read:** <each step that recorded one: its role, the check and the result>.

   **Declined threads:** <with no reviewer step, each bot thread step 1 declined, by node id with its reason, or none>

   **Runs still going:** <each run whose check had not concluded when you read `dispatch results`, by name with the time you read it, and that it may open more bot threads, or none>

   **Production check:** <the command or user action that will show the change working in production>
   **Carried by:** <the deploy or restart that carries the merge there>
   **Run by:** <the agent or role your system prompt's deployment instructions name; when none, the person to ask>
   EOF
   ```

   With no pull request, its last three lines read `none`.

6. **Complete** with the `legion` tool's `handoff_complete`, its summary two sentences for the architect.
   In the same call, rate the plan the issue ran (its steps and the role that worked each, not the planner's `plan.md`):
   send both `plan_rating` and `plan_reason`, or neither where your tool takes neither.
   `plan_rating` is a whole number from 1 (the plan cost the work) to 5 (the plan fit the work).
   `plan_reason` is one sentence of why, from step 1's evidence.
   The daemon refuses a completion without them where your tool takes them (`PLAN_RATING_REQUIRED`), with one alone, a rating off that scale or a reason blank or over 2,000 characters (`PLAN_RATING_INPUT`), and with both on any other completion (`PLAN_RATING_UNEXPECTED`).
   A refusal records nothing: complete again.

## Before you complete

- `jj -R "$LEGION_WORKSPACE" diff --from "legion/${LEGION_ISSUE:?}@origin" --to @ --name-only` lists nothing.

- The message is posted, and so is step 3's comment where a point qualified.
  The pull request's head, where there is one, is the head you found.
