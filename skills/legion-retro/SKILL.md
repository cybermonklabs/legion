---
name: legion-retro
description: "Use when the Legion daemon starts you, the implementer, on `Phase: retro` after the task's last step passed."
---

# Legion retro

Retro records what the work taught, between the task's last step and the merge. It commits nothing, pushes nothing and writes no handoff: the merger merges the head the last step passed.

1. Read the issue, `dispatch results --issue <issue>`, the pull request, and each step's handoff. Where a `before_merge` check has no pass standing at the head, send the issue back with `request_backward_move` to the phase that owes the result, and name the check.
   A check that carries `ci_check` is CI's (`skill://legion-worker/references/ci-checks.md`), and no phase owes its result. A run of it that has not concluded is no failure: name it in step 4's `Runs still going` line and go on. A failed run is the merger's to judge: name it in step 4. A run that concluded `skipped`, `neutral` or `cancelled`, or a held-back job with no label, sends the issue back to implementing. Send a name that no run answers to the architect as a finding.
   With no reviewer step, list the unresolved bot threads and answer the ones you find, as `skill://legion-worker/references/review-threads.md` says.
   For a thread that needs a fix, call `report_defect` with the thread's URL in its `defect`.
   Reply `Reported as defect <id>: <one line>` on the thread, and resolve it.
   Call `report_defect` for each other defect that you find in the change, and go on with your retro.
   Do not send the issue back for it.
2. Record what the work taught as one `dispatch comment` on the issue. Each point gives the rule, its evidence (a file and symbol, or quoted text, or a commit of this pull request, and what went wrong), and the file the rule belongs in. Write no comment when no point qualifies. One comment holds every kept point, each under its own `###` heading:

   ```bash
   dispatch comment --issue '<KEY>' --body-file - <<'EOF'
   ## Retro learnings

   ### <the rule, in one line>

   <the rule, a few imperative lines>

   Evidence: <file and symbol, or text quoted from it, of this pull request, and what went wrong>.
   Belongs in: <the skill, prompt or guide>.
   Replaces: <the instruction and its path>, or nothing.
   EOF
   ```

3. With no reviewer step, record step 1's bot threads in the pull request body's `Threads` at the head you found, and bring its `Not proven / risk` up to date from the tester's handoff where the task has one, naming each check whose run has not concluded (`skill://legion-worker/references/pr-body.md`).
   Change no other line of the body: keep each other line, the `Checks` block included, as it was.
4. Post one Dispatch message on the issue, at most 32,000 characters:

   ```bash
   dispatch message --issue '<KEY>' --body-file - <<'EOF'
   ## Retro Complete

   **Learnings:** the comment of step 2 (`dispatch://<KEY>/comment/<id>`), or none: <why no point qualified>.

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

5. Complete with `handoff_complete`. In the same call send `plan_rating`, a whole number from 1 (the plan cost the work) to 5 (the plan fit the work), and `plan_reason`, one sentence of why. The daemon refuses a completion without them (`PLAN_RATING_REQUIRED`), with one alone, a rating off that scale or a reason blank or over 2,000 characters (`PLAN_RATING_INPUT`), and with both on any other completion (`PLAN_RATING_UNEXPECTED`).
