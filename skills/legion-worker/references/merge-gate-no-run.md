# A required item with no run at the head

Part of `skill://legion-worker/references/merge-gate.md`, "A required item with no run at the head". Read it when a required item is in neither list of the head (its check runs and its commit statuses), so it has no run.

Sort the item, in this order. Cases 2, 3, 4 and 6, and a head whose item the reopen did not start (case 5), end: tell the architect, do not approve, and wait with a `WAITING` line naming what you wait on.

Count the head's runs: `gh api repos/<o>/<r>/commits/<headRefOid>/check-runs --jq .total_count`.

1. Read `gh pr view <n> --repo <o>/<r> --json mergeable,mergeStateStatus` first, whatever the head's other runs show.
   - A head that conflicts with its base (`DIRTY`) has no merge ref, so GitHub starts no `pull_request` run for it, while a `pull_request_target` run, which GitHub builds from the base, can be green beside the missing item.
   - `DIRTY` takes a forward merge, which is the implementer's push, and never the remedy. The daemon asks the implementer for it while the issue keeps its phase: at once when a check the base requires has no green result on the head, the base requires a current branch, or the daemon could not read the base's rules, and otherwise once the issue enters `accepting`. It tells you the new head when the merge lands.
   - So a `DIRTY` head whose required checks are green on it is approved or not as any head is: an approval of it ends the round, and once the merge lands the daemon asks you to read the merge and approve the new head (*A merge of the base is reviewed for what it changed* in `skill://legion-worker/references/merge-gate.md`).
   - `UNKNOWN` is a state GitHub has not computed yet: read it again each minute for up to five minutes. A state still `UNKNOWN` after that, or a read that fails twice, is reported to the architect with the answer GitHub gave, as a refused read is: do not approve, and wait with a `WAITING` line naming the read.
   - A read that GitHub answers with a rate limit waits for the reset the answer names, and is then made again.
2. A fork head whose runs stand `action_required` waits on a maintainer. Never close and reopen it.
3. A check only a commit status reports, or a workflow another repository defines: nothing here starts it. Never close and reopen it.
4. A required workflow whose `paths` or `paths-ignore` filter, as the base holds it, skips every file of the change: a reopen sends the same event, so the head or the workflow must change. When the base dropped that filter after the pull request opened, the head is the remedy's case (case 5).
5. The remedy's case: a `BLOCKED` head, whatever its count, not from a fork (`--json isCrossRepository`), with no run `action_required`, where a workflow under `.github/workflows/` (as the merge commit holds it) has a job reporting the item whose `on:` runs it for `pull_request` with no `types` that leave out `reopened`, no `branches` filter that excludes the base and no path filter that skips the whole change, and neither list holds a run of it.
   - A job reports its `name:`, an expression there evaluated for each matrix entry, or else its key, with ` (<matrix values>)` for a matrix job. A job of a reusable workflow reports `<calling job> / <job>` and runs on the calling file's `on:`.
   - For `code_scanning`, the analysis jobs report under their own names and the `CodeQL` run is the code-scanning App's, made when an analysis uploads, so a head whose analysis jobs ran with no `CodeQL` run is no head this test matches.
   - Tell the architect why, close the pull request, wait a minute, and reopen it at the same head (a quicker reopen can have its run cancelled by the close's own run). Approve only once the item's run concluded green.
   - A head with more than one item with no run takes the remedy when it is not from a fork, has no run `action_required`, and at least one item meets the rest of the test: one close and reopen covers the whole head. Before you close, tell the architect about every other such item in the same message, by its case. Each of them is then waited for as its own case says, and none of them is a reason to reopen a second time.
   - A head with any item in case 2 or 3 takes no remedy for any of its items.
   - A head whose item still has no run once every run the reopen started has concluded (before then the item is missing only because its run has not started) is never reopened again: tell the architect, do not approve, and wait with a `WAITING` line naming what you wait on.
   - The remedy is for no other state: not a check that is running, not a check that is red, and not a head GitHub holds for a review the repository requires.
6. Anything else: tell the architect, do not approve, and wait.
