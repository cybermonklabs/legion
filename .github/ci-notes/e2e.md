# Notes for the E2E proofs workflow

The derivations behind the budgets of `.github/workflows/e2e.yaml` that point into this file with
`# why:` lines, as `.github/ci-notes/README.md` says. Each value's own arithmetic stays on its line
in the workflow file, or, for a value the proof job's matrix carries, in the comment of
`.github/e2e-scripts.json`.

## The proof step's own limit

`Run the proof` runs `bash "scripts/e2e/$SCRIPT"`, and fifteen of the twenty-one hosted scripts
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
comment of `.github/e2e-scripts.json`): 25 minutes for every hosted script but six, 30 for
owner-steps-client.sh and look-names-its-check.sh, 35 for nats-callout-sessions.sh, and 20 for
phase-entry-converge.sh, stage1-skeleton.sh and verification-results.sh.

- Sourcing `lib/live-daemon-proof.sh`, and calling `live_daemon_proof` once: behind-entry-fence.sh,
  child-adoption.sh, conflict-merger.sh, conflict-no-takeover.sh, experiment.sh,
  handoff-records.sh, merge-wait-readable.sh, owner-steps.sh, plan-ratings.sh, review-range.sh,
  step-records.sh and sub-architect.sh at 25 minutes, 1500 + 1050 = 2550 s -> 43 min,
  owner-steps-client.sh and look-names-its-check.sh at 30 minutes, 1800 + 1050 = 2850 s -> 48 min,
  and phase-entry-converge.sh at 20 minutes, 1200 + 1050 = 2250 s -> 38 min.
- Sourcing neither file, directly or through `lib/rig.sh`, through `lib/codex-host.sh`, through
  `lib/model-endpoint.sh` or through the libraries those source: dispatch-claimant-delivery.sh
  and dispatch-broker.sh, 1500 s -> 26 min; nats-callout-sessions.sh, 2100 s -> 36 min; and
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

Those 1050 s are under the retry figures a `jj`, a `bun` or an `apt` row's `timeout_minutes`
carries, 3866, 3416 and 1400 s, so they size those rows' job limits not at all. A row with none of
the three, shared-clone-kind.sh's, would carry the Go module download's 1050 s, which every row
runs, but its proof step's own 1350 s is more. So its job carries that step at its full limit and
the steps before it at their measured time: 38 to 82 s from the job's start to `Run the proof` over
the 22 jobs of e2e.yaml runs 45694873264, 45694862149, 45694670537 and 45715794138. That is
82 + 4500 + 1350 = 5932 s -> 99 min, above the step's 98.
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
over 26 (dispatch://INFRA-1545, round 66). Main's #5385's shared-clone-kind.sh row, added the
same way with 75 minutes, a `dispatch_only` row whose `jj`, `bun` and `apt` select none of those
three steps, now carries its proof step's three retried downloads and the steps before that step:
82 + 4500 + 1350 = 5932 s -> 99 min over 98 (dispatch://INFRA-1545, round 68). Main's #5463's
look-names-its-check.sh row, added with its measured 30 minutes alone and no
`run_timeout_minutes`, with its `jj` and `bun` true, now carries owner-steps-client.sh's
1800 + 3866 = 5666 s -> 95 min over 48 (dispatch://INFRA-1545, round 68).

`.github/scripts/check-download-retry.sh` reads no script outside `.github/scripts/` and no number
out of an expression (A CEILING WRITTEN AS AN EXPRESSION, under its rule 8), so it prices neither
this step nor its rows, and every figure here is derived by hand. What holds them is
`.github/scripts/select-e2e-proofs.sh`, which refuses a row whose `run_timeout_minutes` is
missing, is no whole number, or is not below its `timeout_minutes`, since a job limit at or under
the step's own would cut the step before the step's limit could name it.
