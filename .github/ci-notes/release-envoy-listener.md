# Notes for the Envoy listener's release workflow

The derivations behind the budgets of `.github/workflows/release-envoy-listener.yaml`, which point
into this file with `# why:` lines. That file reached the size limit
`.github/scripts/check-workflow-files.sh` holds every workflow file to once its downloads were
retried (dispatch://INFRA-1545), so the arithmetic of each budget stays on the line it sizes and
the reasoning behind it is here, as `.github/ci-notes/README.md` says.

## The jobs' budgets

Each job carries the budget it carried before dispatch://INFRA-1545 as its first term, plus the
arithmetic of its single slowest retried step, the rule of the Budgets note of
`.github/ci-budgets.md`.

- `targets`: 600 s, the measured 10 minutes this job carried, plus its slowest retried step
  alone: the leak-guard fetch of the release-push.sh steps, 6 call sites of one 450 s fetch =
  2700 s, above every other retried step there. 600 + 2700 = 3300 s -> 56 min, which carries the
  whole minute that note gives a step whose own arithmetic is a whole number of minutes (the
  tripwire's 46 min).
- `build`: 600 s, the measured 10 minutes this job carried, plus its slowest retried step alone:
  the setup-bun step's two `ci_tools_fetch` calls, 2 x 1708 = 3416 s, above the leak-guard fetch
  of the release-push.sh steps, 6 call sites of one 450 s fetch = 2700 s, and every other retried
  step there. 600 + 3416 = 4016 s -> 67 min.
- `docker`: 4800 s, the 80 minutes this job carried, which holds its builds (over both legs of
  this workflow's 15 runs from run 43834530574 to run 43983059481 the smoke build took 134 to
  234 s and the publish build 7 to 11 s), plus its slowest retried step alone: setup-bun's two
  `ci_tools_fetch` calls, 2 x 1708 = 3416 s. That is above the smoke image's build, priced at the
  longest single ladder of the Dockerfile it builds, since a build runs its fetches one after
  another and pays a fault window once, through the first fetch that meets it
  (`.github/scripts/check-download-retry.sh`, rule 10): the `bun install` and the `go mod
  download` of packages/envoy/docker/Dockerfile, at 5 x 600 + 150 = 3150 s each. The publish
  build after it reuses that build's layers from the same builder, so its RUN lines fetch nothing
  again. It is above `Check this job holds no credential (tripwire)` too, which runs
  `.github/scripts/release-push.sh` and so holds that script's leak-guard fetch at every one of
  its call sites, 6 call sites of one 450 s fetch = 2700 s, and above the Go module download and
  the workspace install at 5 x 180 + 150 = 1050 s each. 4800 + 3416 = 8216 s -> 137 min. The
  eight job keys a reviewer of dispatch://INFRA-1545 raised by hand did not hold this one: rule
  8's job-derivation clause found it, because it reads the step arithmetic the check computes
  rather than the figures a comment names (round 28).
- `release_build`: 600 s, the measured 10 minutes this job carried, plus its slowest retried step
  alone: the leak-guard fetch of the release-push.sh steps, 6 call sites of one 450 s fetch =
  2700 s, above every other retried step there. 600 + 2700 = 3300 s -> 56 min, which carries the
  whole minute the Budgets note gives a step whose own arithmetic is a whole number of minutes
  (46 min there).
- `release_publish`: 600 s, the measured 10 minutes this job carried, plus its slowest retried
  step alone: the leak-guard fetch of the release-push.sh steps, 12 call sites of one 450 s fetch
  = 5400 s, above every other retried step there. 600 + 5400 = 6000 s -> 101 min, which carries
  the whole minute that note gives a step whose own arithmetic is a whole number of minutes
  (91 min there).

A step that runs `.github/scripts/release-push.sh` carries 46 minutes, the whole minute above that
script's 6 x 450 = 2700 s, and one that runs it twice carries 91, the whole minute above
2 x 2700 = 5400 s.

## The retried install and module downloads

The workspace install of the `build` and `docker` jobs retries: five bounded attempts through
`.github/scripts/ci-retry.sh`, the last starting 150 s after the first. 180 s per attempt, the
bound every Bun install of this repository's CI carries (dispatch://INFRA-1545): this install
measured 20.70 s cold on a pod, against at most 11 s over 180 observations in the 20 most recent
successful ci.yaml push runs on main.

Each job that compiles Go downloads the modules it builds once, in a step of its own before the
first build, and retries that download, so the build itself reads the module cache and never the
proxy (dispatch://INFRA-1545, R1 and its D1: the retried unit is the download, never the build).
180 s per attempt: a cold `go mod download` of packages/envoy (an empty module cache, this
repository at this commit) measured 77 s on 2026-11-07, and the slowest cold run measured
anywhere in this repository is packages/envoy/infra's 113 s, so the bound is 1.6 times that
slowest measurement. It can be that tight where a tarball fetch's could not: an attempt the bound
kills leaves what it fetched in the module cache, and the next attempt resumes from there.

- `build`'s download is of the module its build compiles.
- `docker`'s is of the modules the smoke test compiles. The arm64 leg restores no module cache,
  so that leg's download is the cold one.
- `release_build`'s runs on the same condition as that job's builds, and is one call: every
  binary that job ships is a command of packages/envoy, and the cross-builds read that one module
  graph whatever GOOS and GOARCH they set. That job holds `contents: read` and no secret, so it is
  a build job like any other and the retry belongs in it; `.github/scripts/check-release-credentials.sh`
  pins the command set of the credentialed `release_publish`, not of this one. It restores no
  module cache, so the cold figure is the one every run pays.

## The build steps whose notices script the gate does not read

`Build envoy binary`, in `build`, and `Build agent-secrets binaries`, in `release_build`, each run
`scripts/go-third-party-notices.sh`, the first once and the second once for each archive it packs.
The script installs go-licenses through `.github/scripts/ci-retry.sh` at the 180 s module bound,
5 x 180 + 150 = 1050 s, and only where its `$tool` is not already executable; the tool's
directory is named by the go-licenses version and the host's Go version, not by the GOOS or
GOARCH the packages target, so every call after the first finds that build and fetches nothing,
and each step waits for one ladder. `.github/scripts/check-download-retry.sh` reads no script
outside `.github/scripts/`, so it prices none of that, and each budget is derived by hand
(dispatch://INFRA-2899 owns the reach that would make it the check's): the ladder beside the
step's measured duration, which holds its builds, over the 20 successful runs of this workflow
created from 2026-11-20T23:00Z to 2026-11-21T08:25Z.

- `Build envoy binary`: 54 s at most over its 40 runs, both legs (job 136709424404 of run
  44857506208, the arm64 leg). 54 + 1050 = 1104 s -> 19 min, under `build`'s 67.
- `Build agent-secrets binaries`: 172 s at most over its 12 runs (job 136751939257 of run
  44873151474; the other runs released nothing and skipped it). 172 + 1050 = 1222 s -> 21 min,
  under `release_build`'s 56.
