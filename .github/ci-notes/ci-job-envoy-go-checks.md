# Notes for the envoy-go-checks job's workflow

The derivations behind the budgets of `.github/workflows/ci-job-envoy-go-checks.yaml` that point
into this file with `# why:` lines, as `.github/ci-notes/README.md` says. Each value's own
arithmetic stays on its line in the workflow file. The file holds two jobs since main's #5175
split envoy-go-checks in two, envoy-go-checks and envoy-go-build
(`.github/ci-notes/ci.md#two-jobs-that-call-one-file`).

## The two steps whose scripts the gate does not read

Two steps of this file start a download retried inside a script outside `.github/scripts/`,
which `.github/scripts/check-download-retry.sh` does not read, so it prices neither and each
budget is derived by hand (dispatch://INFRA-2899 owns the reach that would make it the check's).
Each budget is the script's retry arithmetic beside the step's measured duration, and the next
whole minute above that sum (the Budgets note of `.github/ci-budgets.md`). The durations are the
slowest of the 53 runs of each step in the 60 successful ci.yaml runs created from
2026-11-21T06:57Z to 08:12Z, when both steps were the one envoy-go-checks job's and each ran its
script itself.

- `Test loadtest/cluster/cluster.sh … and compare nats.yaml with a fresh render`, a step of the
  envoy-go-checks job, queues `packages/envoy/loadtest/cluster/cluster.test.sh` in the harness
  pool since main's #5176 (`.github/ci-notes/ci.md#the-harness-pool`): the step returns at once,
  the harness runs beside the job's others, and the step "Wait for the script harnesses and
  judge them" waits for it. The harness runs `packages/envoy/loadtest/cluster/render-nats.sh`
  twice. Its refused-chart case stops before
  any fetch, and its fresh render runs the helm the job's install-helm step put on PATH; only
  where that helm does not answer the version that render-nats.sh pins in `HELM` does it make the
  script's one retried fetch, `go install` of helm through `.github/scripts/ci-retry.sh` at the
  180 s module bound, 5 x 180 + 150 = 1050 s, which this budget prices as the harness's worst
  case; the chart is the vendored archive and the images are pinned from the committed render,
  so nothing else of it reaches the network. That step measured 373 s at most (job 136783645230
  of run 44883029327), when it still built helm, so 373 + 1050 = 1423 s -> 24 min, which the
  step hands the pool as the harness's own bound, `start --minutes 24`: the pool bounds a
  harness from its own start as a step's timeout-minutes bounded the step, and a timeout-minutes
  on a step that only queues would bound the queueing alone. The job's
  comment wrote 1950 s for this script until this change, three retried fetches, of which the
  chart index and the chart archive, 450 s each, stopped being fetches when main's #4272
  vendored the chart.
- `Pack envoy binaries`, a step of the envoy-go-build job, runs `scripts/go-third-party-notices.sh`
  once per release architecture. The script installs go-licenses through the same helper at the
  same bound, 1050 s, and only where `$tool` is not already executable; the tool's directory is
  named by the go-licenses version and the host's Go version, not by the GOOS or GOARCH the
  packages target, so the second call finds the first call's build and fetches nothing, and the
  step waits for one ladder. That step measured 37 s at most (the same job), so
  37 + 1050 = 1087 s -> 19 min.

Neither is its job's slowest retried step: setup-bun's 3416 s is, in both jobs, and the keys are
envoy-go-checks' 1800 + 3416 = 5216 s -> 87 min and envoy-go-build's 900 + 3416 = 4316 s -> 72 min,
each above its own budget for the cluster harness or the pack step.
