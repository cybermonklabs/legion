# Never waiting on CI

Read it when a check is red at the head of your pull request, a merge-group run fails, or a required check is missing at the head.

- A red check that failed on its own, its log (`gh run view <run-id> --log-failed`) showing no fault of the change, such as a lost runner or a network error, is re-run once with `gh run rerun <run-id> --failed`, and its settlement waited for as `skill://envoy`, *Wait for CI or a merge*, says (subscribe, then end the turn), never by sleeping. A red at the code is fixed. A red only GitHub can settle is GitHub's, which holds the merge for it under the base branch's own rules.
- A merge-group run is never re-run: the implementer finishes its round instead, and the pull request is tested again when it is queued again.
- A required check missing at the head: read `gh pr view <n> --json mergeStateStatus` first. `DIRTY` means the head conflicts with its base, so no `pull_request` run starts and a body edit re-judges nothing: merge the base forward. A green `pull_request_target` run beside it proves nothing.
