---
name: legion-retro
description: "Use when the Legion daemon starts you, the implementer, on `Phase: retro` after the architect accepted the reviewed head."
---

# Legion retro

Retro runs on the reviewed head, between the architect's acceptance and the merger's merge.
`<issue>` is your `LEGION_ISSUE`. Run every step, even on a mechanical change.

Retro commits nothing and pushes nothing: the merger merges the head the reviewer approved, and a
push would put a commit above it. Never rebase the branch or make another branch or bookmark.
What the work taught is a `dispatch comment` for the skill librarian; no role writes to
`docs/solutions/`.

## When retro stops

When a step fails (the body cannot be computed, read or written, or Dispatch refuses a write), do
not call `handoff_complete`: send the command and its output to the architect's topic on your
`Legion addressing` line. A defect you find in the work goes to the architect with its evidence,
and you hold your completion until it answers: while retro holds the phase,
`request_backward_move` is yours, and `handoff_complete` takes it away. End each turn of that hold
with a WAITING line naming the defect and the answer you wait on.

## Steps

1. **Read the evidence**: the issue, `dispatch results --issue <issue>`, the pull request, the
   tester's handoff (`dispatch handoff-read --issue <issue> --phase test`) and the review at the
   version your task's `Handoff:` line names (`--phase review --version <n>`). A pass must stand at
   the approved head for every `before_merge` check, and the body's `Checks` block carries each.
   - A check carrying `ci_check` is CI's: its derived result is the pass it needs
     (`skill://legion-worker/references/ci-checks.md`). A `ci_check` check has no pass standing at
     that head when its derived result is a fail, or its run concluded `skipped`, `neutral` or
     `cancelled`. Wait for a run that has not concluded, and read again where GitHub could not be
     read. Send a name that no run answers (`absent`) to the architect as a finding, since the
     check's words are wrong, and the same for a check whose `run` is a whole test suite. Neither
     wait nor that finding moves the issue back.
   - Any other check with no pass standing, or a check with no `ci_check` whose only evidence is a
     unit suite: `request_backward_move` with `to: implementing` for the implementer's result or a
     `ci_check` check, or `to: testing` for the tester's, its `reason` naming the check's block id.
2. **Get a second reading.** First list in your session the constraints the issue did not state,
   choices between two working options, abandoned approaches and reusable code. Then run
   `task(agent="oracle")` with the issue, the pull request body and the diff, asking for reusable
   learnings, and `wait` for its answer. Drop a point that cites no file, symbol, quoted text or
   commit of this pull request.
3. **Record the learnings** as one `dispatch comment` on the issue, per
   `skill://legion-retro/references/learnings.md`. When no point qualifies, write no comment and
   say so in step 5's message.
4. **Update the PR body**, per `skill://legion-retro/references/pr-body-content.md`, when the
   repository derives body lines from changed paths.
5. **Post one Dispatch message on the issue**, at most 32,000 characters (never a `gh issue`
   write):

   ```bash
   dispatch message --issue '<KEY>' --body-file - <<'EOF'
   ## Retro Complete

   **Learnings:** the comment of step 3 (`dispatch://<KEY>/comment/<id>`), or none: <why no point qualified>.

   **Key takeaways:**
   - <one to three reusable lessons>

   **Results read:** implementer <check and result>, tester <check and result>.

   **Production check:** <the command or user action that will show the change working in production>
   **Carried by:** <the deploy or restart that carries the merge there>
   **Run by:** <the agent or role your system prompt's deployment instructions name; when none, the person to ask>

   <!-- legion: {"session":"<session-id>","phase":"retro"} -->
   EOF
   ```

6. **Complete** with the `legion` tool's `handoff_complete`, its summary two sentences for the
   architect.

## Before you complete

- `jj -R "$LEGION_WORKSPACE" diff --from "legion/${LEGION_ISSUE:?}@origin" --to @ --name-only`
  lists nothing.
- Each body line you added or changed that says work was done names work this pull request did.
- The comment and the message are posted, and the pull request's head is the head you found.
