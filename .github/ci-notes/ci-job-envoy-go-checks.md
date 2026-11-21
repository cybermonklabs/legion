# Notes for the envoy-go-checks job's workflow

The derivations behind the budgets of `.github/workflows/ci-job-envoy-go-checks.yaml` that point
into this file with `# why:` lines, as `.github/ci-notes/README.md` says. Each value's own
arithmetic stays on its line in the workflow file.

## The two steps whose scripts the gate does not read

Two steps of this job wait for a download retried inside a script outside `.github/scripts/`,
which `.github/scripts/check-download-retry.sh` does not read, so it prices neither step and each
budget is derived by hand (dispatch://INFRA-2899 owns the reach that would make it the check's).
Each budget is the script's retry arithmetic beside the step's measured duration, and the next
whole minute above that sum (the Budgets note of `.github/ci-budgets.md`). The durations are the
slowest of the 53 runs of each step in the 60 successful ci.yaml runs created from
2026-11-21T06:57Z to 08:12Z.

- `Test loadtest/cluster/cluster.sh … and compare nats.yaml with a fresh render` runs
  `packages/envoy/loadtest/cluster/cluster.test.sh`, which runs
  `packages/envoy/loadtest/cluster/render-nats.sh` twice. Its refused-chart case stops before
  any fetch, and its fresh render makes the script's one retried fetch, `go install` of helm
  through `.github/scripts/ci-retry.sh` at the 180 s module bound, 5 x 180 + 150 = 1050 s; the
  chart is the vendored archive and the images are pinned from the committed render, so nothing
  else of it reaches the network. That step measured 373 s at most (job 136783645230 of run
  44883029327), so 373 + 1050 = 1423 s -> 24 min. The job's comment wrote 1950 s for this
  script until this change, three retried fetches, of which the chart index and the chart
  archive, 450 s each, stopped being fetches when main's #4272 vendored the chart.
- `Pack envoy binaries` runs `scripts/go-third-party-notices.sh` once per release architecture.
  The script installs go-licenses through the same helper at the same bound, 1050 s, and only
  where `$tool` is not already executable; the tool's directory is named by the go-licenses
  version and the host's Go version, not by the GOOS or GOARCH the packages target, so the
  second call finds the first call's build and fetches nothing, and the step waits for one
  ladder. That step measured 37 s at most (the same job), so 37 + 1050 = 1087 s -> 19 min.

Neither is the job's slowest retried step: setup-bun's 3416 s is, and the job's key stays
1800 + 3416 = 5216 s -> 87 min, above both steps' own budgets.
