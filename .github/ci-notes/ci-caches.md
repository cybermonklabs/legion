# Notes for the cache filler's workflow

The derivations behind the budgets of `.github/workflows/ci-caches.yaml`, which point into this
file with `# why:` lines, as `.github/ci-notes/README.md` says. Each value's own arithmetic stays
on its line in the workflow file. The budgets of that file's other jobs are still derived in the
comments above their keys.

## The elan job's budget

The job's measured term is 1800 s, the 30 minutes it was written with: one toolchain download and
one build of the model on a miss, where a run that finds the key costs the checkout alone. The
Budgets note of `.github/ci-budgets.md` adds its single slowest retried step, install-elan's one
`ci_tools_fetch` of the elan release tarball, 1708 s (that note's third worst case), above the Lean
toolchain step's 750 s. 1800 + 1708 = 3508 s -> 59 min.

The Lean toolchain step is airlock-lean's, run here on the same miss that runs the build:
`elan toolchain install` of the toolchain lean-toolchain names, through
`.github/scripts/ci-retry.sh` at 120 s an attempt, so the build after it downloads nothing and the
entry saved holds the toolchain airlock-lean's build reads.
`.github/ci-notes/ci-job-airlock-lean.md#the-lean-toolchain-step-and-its-bound` derives that
bound from four measured installs, this job's own among them: job 134958116292 of run 44348716140,
the first fill on ubuntu-24.04-arm, where elan's download took 4.8 s and the whole install 11.6 s.
5 x 120 + 150 = 750 s -> 13 min.

## The bun job's budget

The bun job is the bun family's one saving job (the job's own comment says why there is one leg
per runner architecture). No run of it is measured yet, so its first term is built from what it
runs: a checkout of one commit, 68 s at most over the 1,032 checkouts of the 25 ci.yaml runs
created from 2026-11-21T07:46Z to 08:23Z (median 4 s), and `.github/actions/setup-bun`, 5 s at
most over the 482 calls in those runs that restored the entry. A miss adds the release asset of
51,794,595 bytes and the save of what it installed. 600 s holds that with room, and the Budgets
note of `.github/ci-budgets.md` adds the job's one retried step, setup-bun's two `ci_tools_fetch`
calls, 2 x 1708 = 3416 s (that note's terms). 600 + 3416 = 4016 s -> 67 min, above that step's own
57.

## The dispatch-builds job's budget

The job builds, on a miss of its key, each of the two builds ci.yaml's dispatch-two-builds job
restores, with the script and the bounds that job's build steps run
(`.github/ci-notes/ci-job-dispatch-two-builds.md#the-build-steps-budgets`): each build step is
1050 + 1050 + 116 = 2216 s -> 37 min.

No run of it is measured yet, so its first term is built from what it runs: the checkout of the
whole history, 96 s at most over the 753 checkouts of dispatch-two-builds that job's own comment
counts, and 68 s at most in the window the bun job's term above reads; setup-go and setup-bun, the
latter 5 s at most on a restore; and the two builds, 116 s at most each. That is under 400 s, and
900 s holds it with room. Its slowest retried step is setup-bun's two `ci_tools_fetch` calls,
3416 s, above each build's 2216 s. 900 + 3416 = 4316 s -> 72 min, above every step's own budget.

## The notices step whose script the gate does not read

`Write every Go release archive's third-party notices and cross-build the commands`, in the notices
job, runs `.github/scripts/go-release-notices.sh`, the script ci.yaml's lint job runs, and so waits
for the one go-licenses install of `scripts/go-third-party-notices.sh` that
`.github/ci-notes/ci-job-lint.md#the-notices-step-whose-script-the-gate-does-not-read` derives,
1050 s, which `.github/scripts/check-download-retry.sh` does not price. Beside it is the step's
measured duration on a miss, which holds the cold build of every release target: 674 s at most
over the 2 runs of the step in the 100 successful runs of this workflow created from
2026-11-19T12:04Z to 2026-11-21T09:04Z (job 136376790855 of run 44762031448; every other run found
the key and skipped it). 674 + 1050 = 1724 s -> 29 min, under the job's 127.
