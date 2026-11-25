# Checks, results and the evidence each phase owes

Read it before you record or read a check's result, judge another role's evidence, act on a red or missing CI check at the head, or find that no surface reaches the changed path.
The pull request body's `Checks` lines are in `skill://legion-worker/references/pr-body.md`.

## What a result is

- **A check** is a `check` block of the issue's Verification document:
  what it `serves`, its `surface` (`browser`, `cli` or `api`), its `run`, its `when` (`before_merge`, `after_merge` or `default_branch`) and its `negative_control`.
  The architect writes the checks; one carrying `ci_check` is CI's (`skill://legion-worker/references/ci-checks.md`).

- **A result** is one run of one check: `dispatch result --issue <issue> --check <block id> --role <your role> --verdict pass --commit <sha>`, with `--surface`, `--run`, `--observed` and `--negative-control`.
  These refuse a bare `-`: use `--run-file` and its kin, one reading stdin per call.
  Put the printed line into the body unchanged.

- The surface is production-like (the real-process harness, a sandbox repository, a real browser, a development stack, staging) and has the resource the change touches;
  `--run` or `--observed` links to the run.
  A unit or integration test is no result, except as the job a `ci_check` names.

- Record a result only after its commit is pushed (`400 RESULT_COMMIT_UNKNOWN`).

- A check that cannot fail is not a result.
  The negative control shows it failing on a deliberately wrong input.
  A documented procedure counts only when it ran: reading it is not a run.

- A `before_merge` result stands while its commit is the head of an open pull request Dispatch attributes to the check's issue (`packages/envoy/internal/dispatch/delivery/attribution.go`), or the final head of a merged one, and while no part of the check's block has changed.
  So after a push, record each `before_merge` check with no `ci_check` again.
  Beside a running fix, record at the pull request's head you read before you worked, never at a commit of your own branch: no pull request heads it, so a result there stands nowhere.

## The rules every phase's evidence follows

- The tester tries to break the changed surface (wrong and boundary inputs, the second and the concurrent run, the party it must refuse) and writes a failing test for each way it breaks that no existing test catches.
  Lint, types and the unit and integration suites are CI's: the tester lets CI do its job, boots and runs the app's real workflows like a user would, and reads no check run, re-runs no CI and waits on no CI, since the reviewer reads the head's checks before it approves.

- After a forward merge, the tester reruns each check the merge can reach (`skill://legion-worker/references/conflicts-and-rewrites.md`) and records each other check with no `ci_check` at the new head unrun, its `--observed` naming the earlier pass and why the merge misses it.
  A class search that finds an instance, or a parent's last round, takes a full test round.

## Never waiting on CI

- The implementer re-runs once a red check whose failure is not in this pull request's code, and so do the reviewer and the merger, each within its bounds: a red check that failed on its own is re-run once with `gh run rerun <run-id> --failed`, by the reviewer in `reviewing`, the implementer while it holds `implementing`, or the merger in `merging` at a head its own forward merge made, after it records the failure, and its settlement waited for as `skill://envoy` says, never by sleeping (where no settlement comes, the bound is the phase reminder or the daemon's backstop: it reads the counted set once then, and waits again on a re-run still queued or in progress).
  The merger re-runs only a failed `pull_request` check, records the failure in a `dispatch comment` on the issue, since it writes no handoff, and at any other head re-runs nothing.
  `skill://legion-worker/references/red-at-the-head.md` holds the rest of that rule: which red is the change's, which a pass in the role's own pod does not move; that a red is acted on in the head's own `pull_request` run once that run has completed, however the role learned of it; the test of whether the head's one re-run is already spent; and the record the role writes before it re-runs.
  A merge-group run is never re-run: it goes only through the merger's one requeue of a `failed_checks` removal.

- A required check missing at the head: read `gh pr view <n> --json mergeStateStatus` first.
  `DIRTY` means the head conflicts with its base, so no `pull_request` run starts and a body edit re-judges nothing: merge the base forward.
  A green `pull_request_target` run beside it proves nothing.

## When no surface reaches the changed path

Report it to the architect with `envoy_publish`, naming the missing surface and what it would have to do (a rig, a sandbox holding the resource, a credential, a command), and do not complete.
The architect has it built in this tree and resumes you.
A code path that first runs after the merge (a deploy workflow's inline step, a post-merge helper, a production-only resource) is an `after_merge` check instead, recorded after the merge by the role the project's `production_check` names (under `off` nothing records it), unless the implementer can run it against a development stack before then.
