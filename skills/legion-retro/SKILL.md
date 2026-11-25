---
name: legion-retro
description: "Use when the Legion daemon starts you, the implementer, on `Phase: retro` after the task's last step passed."
---

# Legion retro

Retro, between the task's last step and the merger's merge, reads the evidence, records what the work taught, rates the plan, and leaves the head as it found it.
The architect's acceptance precedes it only where the task names `acceptance:owner`.

**Done when:** before you complete:

- `jj -R "$LEGION_WORKSPACE" diff --from "legion/${LEGION_ISSUE:?}@origin" --to @ --name-only` lists nothing.
- Each body line you added or changed that says work was done names work this pull request did.
- The comment and the message are posted.
- The pull request's head is the head you found.
- Your `handoff_complete` carries `plan_rating` and `plan_reason`, or neither where your `legion` tool's `handoff_complete` takes neither.

Run every step, even on a mechanical change.

- **Commit nothing and push nothing.** The merger merges the branch's head.
  A push would put a commit above the head the last worker step passed.
  Outside a forward merge of the base, no Legion check compares the head that merges with a review's head.
- **Make no other branch or bookmark.**
- **Write no handoff.**
- **Change nothing in the workspace.**
- **Outside the workspace, write only** step 4's `mktemp -d` directory, the PR body, the Dispatch comment and message, and, with no reviewer step, the review-thread replies and resolutions of step 1.
- **Set no GitHub or Dispatch label:** Legion keeps phase state in the daemon.
- **Write nothing to `docs/solutions/`:** no role does.

## When retro stops

- **Do not call `handoff_complete` when a step fails** (the body cannot be computed, read or written, or Dispatch refuses a write).
  Send the command and its output to the architect's topic on your `Legion addressing` line.
- **Send a defect you find in the work, other than a bot thread that step 1 sends back, to the architect with its evidence.**
- **Hold your completion until the architect answers:** `handoff_complete` takes away `request_backward_move`, which is yours while retro holds the phase.
- **End each turn of that hold with a WAITING line** naming the defect and the answer you wait on.

## Steps

1. **Read the evidence**: the issue, `dispatch results --issue "$LEGION_ISSUE"`, the pull request, and the thread of each plan task in `plan.md`.
   Also read the tester's handoff (`dispatch handoff-read --issue "$LEGION_ISSUE" --phase test`) where it has a tester step.
   Read the GitHub review of the last worker step's head where it has a reviewer step.
   - **A pass must stand at the last worker step's head for every `before_merge` check.** The body's `Checks` block carries each.
   - **A check carrying `ci_check` is CI's:** its derived result is the pass it needs (`skill://legion-worker/references/ci-checks.md`).
   - **A `ci_check` check has no pass standing at that head** when its derived result is a fail, its run concluded `skipped`, `neutral` or `cancelled`, or it names a job held back without its label, whose run is `absent` or `skipped` until the label is on.

   | The check | Do |
   | --- | --- |
   | A `ci_check` run that has not concluded, or one GitHub could not be read for | It is neither a pass nor a fail yet: never wait for, watch or poll it. Name each run that has not concluded, with the time you read it, in step 5's `Runs still going` line, and go on. When CI settles, the daemon tells the architect and the worker of the phase the issue then stands in |
   | A `ci_check` check with any other name that no run answers (`absent`), and a check whose `run` is a whole test suite | Send it to the architect as a finding, since the check's words are wrong |
   | A `ci_check` check whose derived result is a fail | It is the merger's: the change passed every step of its task, and the merger decides at the merge whether the base or the change broke it. Name it in your retro message and complete |
   | Any other check with no pass standing, or a check with no `ci_check` whose only evidence is a unit suite | `request_backward_move` with `to: implementing` for the implementer's result, or, where the task has a tester step, `to: testing` for the tester's result. Name the check's block id in the reason |

   Neither a run still going nor that finding moves the issue back.
   - **With no reviewer step, list the unresolved bot threads and answer the ones you find**, as `skill://legion-worker/references/review-threads.md` says.
     For a thread that needs a fix, `request_backward_move` with `to: implementing`, the thread's URL in its `reason`.
2. **Get a second reading.**
   - **First, list in your session** the constraints the issue did not state, choices between two working options, abandoned approaches and reusable code.
   - **Then run `task(agent="oracle")`** with the issue, the pull request body and the diff, asking for reusable learnings, and `wait` for its answer.
   - **Drop a point that cites no file, symbol, quoted text or commit of this pull request.**
3. **Record the learnings** as one `dispatch comment` on the issue, per `skill://legion-retro/references/learnings.md`, which says what a point must carry for the skill librarian to use it.
   - **When no point qualifies, write no comment.** Say so in step 5's message.
4. **Update the PR body** per `skill://legion-retro/references/pr-body-content.md`: the lines the repository derives from changed paths, where it derives any, and, with no reviewer step, the `Threads` and `Not proven / risk` lines.
5. **Post one Dispatch message on the issue**, at most 32,000 characters:

   ```bash
   dispatch message --issue '<KEY>' --body-file - <<'EOF'
   ## Retro Complete

   **Learnings:** the comment of step 3 (`dispatch://<KEY>/comment/<id>`), or none: <why no point qualified>.

   **Key takeaways:**
   - <one to three reusable lessons>

   **Results read:** implementer <check and result>, tester <check and result, where the task has a tester step>.

   **Declined threads:** <with no reviewer step, each bot thread step 1 declined, by node id with its reason, or none>

   **Runs still going:** <each run whose check had not concluded when you read `dispatch results`, by its name with the time you read it, and that it may open more bot threads; or none>

   **Production check:** <the command or user action that will show the change working in production>
   **Carried by:** <the deploy or restart that carries the merge there>
   **Run by:** <the agent or role your system prompt's deployment instructions name; when none, the person to ask>
   EOF
   ```

   - **Never post it with a `gh issue` write.**
   - **Put no `<!-- legion: … -->` footer on a Dispatch comment or message.** Dispatch records the session that wrote it.
     Only a pull-request comment or review needs the footer.
6. **Complete** with the `legion` tool's `handoff_complete`.
   Make its summary two sentences for the architect.
   - **In the same call, rate the plan the issue ran:** its steps and the role that worked each, not the planner's `plan.md`.
   - **Send both fields below, or neither where your `legion` tool's `handoff_complete` takes neither field.**

   | Field | Value |
   | --- | --- |
   | `plan_rating` | A whole number from 1 (the plan cost the work) to 5 (the plan fit the work) |
   | `plan_reason` | One sentence of why, from step 1's evidence |

   | The daemon refuses a completion | Code |
   | --- | --- |
   | Without `plan_rating` and `plan_reason` | `PLAN_RATING_REQUIRED` |
   | With one alone, a rating off that scale, or a reason blank or over 2,000 characters | `PLAN_RATING_INPUT` |
   | With both, on any other completion | `PLAN_RATING_UNEXPECTED` |

   A refusal records nothing: complete again.
