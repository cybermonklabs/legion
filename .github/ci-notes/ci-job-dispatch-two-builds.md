# Notes for the dispatch-two-builds job's workflow

The derivations behind the budgets of `.github/workflows/ci-job-dispatch-two-builds.yaml`, which
point into this file with `# why:` lines, as `.github/ci-notes/README.md` says. Each value's own
arithmetic stays on its line in the workflow file.

## The build steps' budgets

Each step that runs `packages/envoy/scripts/build-dispatch-build.sh --out` waits for that
script's two retried downloads: the build's workspace install, `bun install --frozen-lockfile` in
the commit's archive, and the Go modules and toolchain its server's build loads, `go list -deps
./cmd/dispatch` under `GOTOOLCHAIN=auto`. Each goes through `.github/scripts/ci-retry.sh` at the
180 s bound of its kind (the Retries note of `.github/ci-budgets.md`), so each is
5 x 180 + 150 = 1050 s. `.github/scripts/check-download-retry.sh` reads no script outside
`.github/scripts/`, so it prices neither, and the budget is derived by hand
(dispatch://INFRA-2899 owns the reach that would make it the check's).

Beside the two downloads the step holds the build itself, which the step's measured duration
gives: 116 s, the slowest of the 24 runs of `Build the previous build` in ci.yaml runs created from
2026-11-21T06:57Z to 08:12Z (job 136784988877 of run 44883794202), the same 116 s that job
132706600704 measured when the step was first budgeted. 1050 + 1050 + 116 = 2216 s, and the step
carries the next whole minute above, 37 minutes.

`Build the first-rollout build` takes the same budget. It runs the same script over an older
commit, and in every one of those 24 runs it took 1 s at most, because its pinned entry was
restored and the script found the build complete; a miss builds it as the previous build is built,
so it is held to that step's figure. It carried no budget of its own at ee618fd7 of
dispatch://INFRA-1545's branch, so a stalled download there ran until the job's ceiling cut it
rather than failing the step by name. `Build this tree as the previous build`, in the
`dispatch-two-builds-same` job, runs the same script over the tree and carries the same 37
minutes.

Both builds are restored and never saved here: `.github/workflows/ci-caches.yaml`'s
`dispatch-builds` job builds them and saves them on a push to main, under the keys these restores
read.
