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

- A `before_merge` result stands at a compared open or merged head in that result's repository.
  The check needs a standing pass at every open head, across all its repositories.
  When none is open, only the final head of the last pull request to merge is required.
  An unordered merged set leaves the check not known.
  A result stands while no part of the check's block has changed.
  A pass and a fail at one required head read not known until one of them is retracted.
  Results at a head that is not required do not decide the check.
  After a push, record each `before_merge` check with no `ci_check` again at the new head.
  Run it again only when the change since its pass can affect what it drives; otherwise record its earlier pass at the new head with `--observed 'carried from <old sha>: <why the change cannot affect it>'`.

- An `after_merge` result counts only at a commit on the branch the issue's pull request merged into, or on the default branch.
  That commit must also be the merge of the work on that branch, or have it in its history.
  The merge of the work is the commit that brought the issue's pull request that merged last into that branch.
  Where work merged into an integration branch, the default branch uses that branch's newest recorded landing whose final head holds the work's merge.
  The newest counting result chooses the commit whose verdicts decide the check.
  A pass and a fail at that commit leave the check not known until one is retracted.

## The rules every phase's evidence follows

- The tester tries to break the changed surface (wrong and boundary inputs, the second and the concurrent run, the party it must refuse) and writes a failing test for each way it breaks that no existing test catches.
  Lint, types and the unit and integration suites are CI's: read on GitHub the ones the head ran, rerun no lane as evidence, and wait for none; a suite the repository holds back for its merge queue has no run at the head, and that is no gap.

- On a new head, the tester reruns each check the change since the head it last tested can reach (for a merge of the base, `skill://legion-worker/references/conflicts-and-rewrites.md#reading-a-merge-of-the-base`) and records each other check with no `ci_check` at the new head unrun, its `--observed` naming the earlier pass and why the merge misses it.

## Never waiting on CI

- The implementer re-runs once a red check whose failure is not in this pull request's code, with `gh run rerun <run-id> --failed`, and is woken by its settlement as `skill://envoy` says, never by sleeping.
  A merge-group run is never re-run.

- A required check missing at the head: read `gh pr view <n> --json mergeStateStatus` first.
  `DIRTY` means the head conflicts with its base, so no `pull_request` run starts and a body edit re-judges nothing: merge the base forward.
  A green `pull_request_target` run beside it proves nothing.

## When no surface reaches the changed path

Report it to the architect with `envoy_publish`, naming the missing surface and what it would have to do (a rig, a sandbox holding the resource, a credential, a command), and do not complete.
The architect has it built in this tree and resumes you.
A code path that first runs after the merge (a deploy workflow's inline step, a post-merge helper, a production-only resource) is an `after_merge` check instead, recorded after the merge by the role the project's `production_check` names (under `off` nothing records it), unless the implementer can run it against a development stack before then.
