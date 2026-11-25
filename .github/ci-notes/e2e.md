# Notes for the E2E proofs workflow

The derivations behind the budgets of `.github/workflows/e2e.yaml` that point into this file with
`# why:` lines, as `.github/ci-notes/README.md` says. Each value's own arithmetic stays on its line
in the workflow file, or, for a value the proof job's matrix carries, in the comment of
`.github/e2e-scripts.json`.

## The proof step's own limit

`Run the proof` runs `bash "scripts/e2e/$SCRIPT"`, and fourteen of the nineteen hosted scripts
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
neither gets its own measured figure as its limit all the same, as the next whole minute above
it, so a proof that hangs ends on its own step.

The measured figure is the one each row's `timeout_minutes` adds its slowest retried step to (the
comment of `.github/e2e-scripts.json`): 25 minutes for every hosted script but five, 30 for
owner-steps-client.sh, 35 for nats-callout-sessions.sh, and 20 for phase-entry-converge.sh,
stage1-skeleton.sh and verification-results.sh.

- Sourcing `lib/live-daemon-proof.sh`, and calling `live_daemon_proof` once: behind-entry-fence.sh,
  child-adoption.sh, conflict-merger.sh, conflict-no-takeover.sh, experiment.sh,
  handoff-records.sh, merge-wait-readable.sh, owner-steps.sh, plan-ratings.sh, review-range.sh,
  step-records.sh and sub-architect.sh at 25 minutes, 1500 + 1050 = 2550 s -> 43 min,
  owner-steps-client.sh at 30 minutes, 1800 + 1050 = 2850 s -> 48 min, and
  phase-entry-converge.sh at 20 minutes, 1200 + 1050 = 2250 s -> 38 min.
- Sourcing neither file, directly or through `lib/rig.sh`, through `lib/codex-host.sh`, through
  `lib/model-endpoint.sh` or through the libraries those source: dispatch-claimant-delivery.sh
  and dispatch-broker.sh, 1500 s -> 26 min; nats-callout-sessions.sh, 2100 s -> 36 min; and
  stage1-skeleton.sh and verification-results.sh, 1200 s -> 21 min.

Those 1050 s are under each of the three retry figures a row's `timeout_minutes` carries, 3866,
3416 and 1400 s, so they size no job limit, and every row's job limit stays above its step's: 90
over 43, 95 over 48, 85 over 38, 82 over 26, 92 over 36, 44 over 21 and 77 over 21.
plan-ratings.sh's row had a `timeout_minutes` of 25, its measured figure alone, though its `jj` is
true, which put the job's limit under both install-jj's 65-minute step and this step's 43 minutes;
it is now the other 25-minute `jj` rows' 1500 + 3866 = 5366 s -> 90 min (dispatch://INFRA-1545).
Main's #4611 added owner-steps.sh and owner-steps-client.sh with their measured figures alone and
no `run_timeout_minutes`, which the selector refuses; their rows now carry 1500 + 3866 = 5366 s ->
90 min over 43 and 1800 + 3866 = 5666 s -> 95 min over 48 (dispatch://INFRA-1545, round 59), and
main's #4930's experiment.sh row, added the same way, now carries 1500 + 3866 = 5366 s -> 90 min
over 43 (dispatch://INFRA-1545), and main's #4392's dispatch-broker.sh row, added the same way
with its `jj` false and its `bun` true, now carries setup-bun's 1500 + 3416 = 4916 s -> 82 min
over 26 (dispatch://INFRA-1545, round 66).

`.github/scripts/check-download-retry.sh` reads no script outside `.github/scripts/` and no number
out of an expression (A CEILING WRITTEN AS AN EXPRESSION, under its rule 8), so it prices neither
this step nor its rows, and every figure here is derived by hand. What holds them is
`.github/scripts/select-e2e-proofs.sh`, which refuses a row whose `run_timeout_minutes` is
missing, is no whole number, or is not below its `timeout_minutes`, since a job limit at or under
the step's own would cut the step before the step's limit could name it.
