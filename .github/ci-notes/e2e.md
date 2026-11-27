# Notes for the E2E proofs workflow

The derivations behind the budgets of `.github/workflows/e2e.yaml` that point into this file with
`# why:` lines, as `.github/ci-notes/README.md` says. Each value's own arithmetic stays on its line
in the workflow file, or, for a value the proof job's matrix carries, in the comment of
`.github/e2e-scripts.json`.

## The proof step's own limit

`Run the proof` runs `bash "scripts/e2e/$SCRIPT"`, and twenty-one of the twenty-eight hosted scripts
source `scripts/e2e/lib/live-daemon-proof.sh`, whose `live_daemon_proof` installs nats-server with
`go install` through `.github/scripts/ci-retry.sh` at the 180 s module bound,
5 x 180 + 150 = 1050 s; `scripts/e2e/dispatch-session-settings.sh` retries the same install the
same way, and is no hosted row. With no limit of its own, the step was held by the job's alone,
which counts every step before it too, so a retry still running there could be cut part way, on
GitHub's bare timeout line, which names neither the step nor the install. So the step takes its
`timeout-minutes` from the row's `run_timeout_minutes`: the script's own measured duration with
headroom, plus those 1050 s where the script sources one of those two files, directly or through
a library it sources, written as the whole minute strictly above the sum (rule 8 of
`.github/scripts/check-download-retry.sh` says why equal is not enough). A script that sources
neither Go helper and carries no retry term of its own (every other hosted script except
shared-clone-kind.sh, which has its own three-download term below) gets its own measured figure
as its limit all the same, as the next whole minute above it, so a proof that hangs ends on its
own step.

Each row's measured figure is the one its `timeout_minutes` starts from (the comment of
`.github/e2e-scripts.json`), derived per row below:

- Sourcing `lib/live-daemon-proof.sh`, and calling `live_daemon_proof` once: behind-entry-fence.sh,
  child-adoption.sh, claim-compaction.sh, conflict-merger.sh, conflict-no-takeover.sh,
  experiment.sh, handoff-records.sh, late-completion.sh, merge-wait-readable.sh, owner-steps.sh,
  parent-waits.sh, plan-ratings.sh, provider-error-retry.sh, register-gate-basis.sh,
  review-range.sh, role-prompt-rule.sh, step-records.sh and sub-architect.sh at 25 minutes,
  1500 + 1050 = 2550 s -> 43 min,
  owner-steps-client.sh and look-names-its-check.sh at 30 minutes, 1800 + 1050 = 2850 s -> 48 min,
  and phase-entry-converge.sh at 20 minutes, 1200 + 1050 = 2250 s -> 38 min.
- Sourcing neither file, directly or through `lib/rig.sh`, through `lib/transcript.sh`, through
  `lib/run-verdict.sh`, through `lib/codex-host.sh`, through `lib/model-endpoint.sh` or through the
  libraries those source: dispatch-claimant-delivery.sh and dispatch-broker.sh, 1500 s -> 26 min;
  refusal-fallback-omp.sh, 1800 s -> 31 min; nats-callout-sessions.sh, 2100 s -> 36 min; and
  stage1-skeleton.sh and verification-results.sh, 1200 s -> 21 min.
- shared-clone-kind.sh, whose scripts/e2e/lib/kind-sandbox.sh retries its three downloads at the
  60 s a release asset or archive gets (the Retries note of .github/ci-budgets.md), kind v0.33.0
  (10.5 MB) and Agent Sandbox's `sandbox.yaml` (206 kB) from github.com and gVisor's tarball
  (76.4 MB) from storage.googleapis.com, each under a second from an issue pod on 2026-11-25: its
  75 minutes plus 3 x (5 x 60 + 150) = 1350 s, 4500 + 1350 = 5850 s -> 98 min. Its two ghcr.io
  reads, the pull token and the manifest of ghcr.io/cybermonklabs/legion-worker, read this
  repository's own package and stay bare, under exception (a) of
  .github/scripts/check-download-retry.sh, whose header names them (dispatch://INFRA-1545, round
  68).

The Go module download before the proof has retry arithmetic of 1050 s.
It is shorter than the slowest retried step in rows that select `jj`, `bun` or `apt`.
install-omp's fetch of the release archive, which a row whose `omp` is true runs after install-jj,
is 5 x 60 + 150 = 450 s, shorter still, so it sizes no row.
shared-clone-kind.sh's job limit is derived in this file's section "The Kind row's job limit".
Every row's job limit stays above its step's: `.github/scripts/select-e2e-proofs.sh` refuses one
that does not, so the pairs are read from `.github/e2e-scripts.json` and not copied here.
plan-ratings.sh's row had a `timeout_minutes` of 25, its measured figure alone, though its `jj` is
true, which put the job's limit under both install-jj's 65-minute step and this step's 43 minutes;
it is now the other 25-minute `jj` rows' 1500 + 3866 = 5366 s -> 90 min (dispatch://INFRA-1545).
Main's #4611 added owner-steps.sh and owner-steps-client.sh with their measured figures alone and
no `run_timeout_minutes`, which the selector refuses; their rows now carry 1500 + 3866 = 5366 s ->
90 min over 43 and 1800 + 3866 = 5666 s -> 95 min over 48 (dispatch://INFRA-1545, round 59), and
main's #4930's experiment.sh row, added the same way, now carries 1500 + 3866 = 5366 s -> 90 min
over 43 (dispatch://INFRA-1545), and main's #4392's dispatch-broker.sh row, added the same way
with its `jj` false and its `bun` true, now carries setup-bun's 1500 + 3416 = 4916 s -> 82 min
over 26 (dispatch://INFRA-1545, round 66). Main's #5463's
look-names-its-check.sh row, added with its measured 30 minutes alone and no
`run_timeout_minutes`, with its `jj` and `bun` true, now carries owner-steps-client.sh's
1800 + 3866 = 5666 s -> 95 min over 48 (dispatch://INFRA-1545, round 68). Main's #4592's
parent-waits.sh and register-gate-basis.sh rows, added with their measured 25 minutes alone and
no `run_timeout_minutes`, each with its `jj` true, now carry 1500 + 3866 = 5366 s -> 90 min over
43 (dispatch://INFRA-1545, round 71). Main's #5360 moved role-prompt-rule.sh, late-completion.sh,
claim-compaction.sh, provider-error-retry.sh and refusal-fallback-omp.sh from the `elsewhere` list
to `hosted`, each with its `omp` true, its measured figure alone and no `run_timeout_minutes`.
The first four, at 25 minutes with their `jj` and `bun` true, now carry 1500 + 3866 = 5366 s ->
90 min over 43. The second sum of .github/ci-budgets.md#the-budgets-note, the steps before
install-jj (the checkout, setup-bun and the workspace install) plus install-jj's 65 minutes,
3900 s, would be the larger only if those steps took more than 1466 s, and no hosted run of these
rows has measured them yet. refusal-fallback-omp.sh, at 30 minutes with its `jj` false and its
`bun` true, now carries setup-bun's 1800 + 3416 = 5216 s -> 87 min over 31; its second sum, the
checkout plus setup-bun's 57 minutes, 3420 s, would be the larger only if the checkout took more
than 1796 s (dispatch://INFRA-1545).

`.github/scripts/check-download-retry.sh` reads no script outside `.github/scripts/` and no number
out of an expression (A CEILING WRITTEN AS AN EXPRESSION, under its rule 8), so it prices neither
this step nor its rows, and every figure here is derived by hand. What holds them is
`.github/scripts/select-e2e-proofs.sh`, which refuses a row whose `run_timeout_minutes` is
missing, is no whole number, or is not below its `timeout_minutes`, since a job limit at or under
the step's own would cut the step before the step's limit could name it.

## The Kind row's job limit

shared-clone-kind.sh's row is `dispatch_only`, so no label run and no nightly makes a job of it,
and a `workflow_dispatch` that names it is the only run that does. Its `timeout_minutes` follows
the Budgets note, .github/ci-budgets.md#the-budgets-note. Its slowest retried step is the proof
step, whose 1350 s is above the module download's 1050 s, and the larger of that note's two sums is
the second: the time before the proof step plus the proof step's rounded limit, 98 minutes, 5880 s.

The time before the proof step is two figures, one observed and one a bound:

- 42 s, observed: the longest of four `workflow_dispatch` runs of `e2e.yaml` that ran this row,
  from the job's start to the start of its step `Run the proof`: 35 s in run 45875411054 (job
  140101289546), 34 s in run 45859154723 (job 140045527045), 42 s in run 45854354505 (job
  140028922572) and 39 s in run 45849489820 (job 140012075903). The workflow those runs ran had
  no retried module download, so they observe the other steps before the proof, the checkout,
  setup-go and the read of the service's settings among them, and none of them is a run of this
  branch's whole proof job.
- 1050 s, a bound: this branch's step "Download the Go modules this job builds",
  5 x 180 + 150 = 1050 s, which those runs did not have.

42 + 1050 + 5880 = 6972 s -> 117 min. The first sum is 42 + 4500 + 1350 = 5892 s -> 99 min:
the job's measured duration, its 42 s of setup and the script's 75 minutes, plus the proof
step's 1350 s. Rounding the larger sum leaves 48 s to spare. The 42 s is not a bound: with the
full module retry bound spent and more than 90 s spent by the other setup steps, the job can
stop the proof before its own deadline (dispatch://INFRA-1545, round 72).
