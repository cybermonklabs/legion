# The handoff, the completion, and a refused completion

Read it before you record the handoff your role prompt names or complete (step 5 of `skill://legion-worker`), when your phase ends, and when `handoff_complete` refuses your completion.

## The handoff

A handoff is never a commit and never a result: what you ran is a result you record against one of the issue's checks with `dispatch result`, and `skill://legion-worker/references/pr-body.md#what-a-result-is` defines a check and a result. To change a handoff already recorded, read it with `dispatch handoff-read`, edit the body, and record it whole again: each `dispatch handoff-write` adds a version, and `handoff_complete` reports the newest.

## Push any work, then complete

`handoff_complete` is your only way to end a phase: add no label, keep no status of your own and run no loop of your own, since the daemon owns the issue's state and the architect its decomposition and gates.

## When your phase ends

The workspace is read-only for you from the moment your phase ends, whether you ended it by completing or another role's move ended it while your turn ran: no commit, no `jj new`, no edit to a tracked file, in the turn still running when you learn it ended and in every turn after it, until a new task names your phase. Nothing stops that turn for you, so stopping is yours to do: drop the work in hand by restoring each tracked file you changed and did not commit (`jj -R "$LEGION_WORKSPACE" restore <paths>`), so `@` holds none of your edits for the next role. Two writes are the exception: that restore, and the forward merge a merge-forward message asks of the implementer.

## A refused completion

A refused completion is information, not a retry loop. Quote the answer verbatim to the architect, then act on its code:

- `HANDOFF_STALE_GENERATION` or `HANDOFF_NO_RUN`: push nothing more and end the turn without completing. When the task then arrives with its work and handoff done, call `handoff_complete` again in its turn; never redo the work or write a second handoff.
- `HANDOFF_STALE_ENTRY`: end the turn without completing, and report that work and that verdict no further; the daemon sends the new entry's task by itself, so ask no one for it. Do what it asks at the head it names, with a handoff of its own, never this completion's record, commit or verdict.
- `HANDOFF_HEAD_MERGED_FORWARD`: a merge forward the daemon asked for has landed above the head your completion names, or your completion names no head; nothing was recorded. Read the merge (`skill://legion-worker/references/conflicts-and-rewrites.md`), then complete again at the new head with a handoff that names it.
- `HANDOFF_NOT_CURRENT_PHASE`: report it; never complete again.
- `HANDOFF_NO_PULL_REQUEST` (the implementer's completion found no open pull request on `legion/<KEY>`, and none recorded merged): open it and complete again at the same commit; when the answer names a closed pull request, the architect decides the phase: tell it, and open or reopen nothing until it answers.
- `HANDOFF_ALREADY_RECORDED`: the first answer stands; if you did not see it, tell the architect.
