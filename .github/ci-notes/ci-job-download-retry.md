# Notes for the download-retry lanes' steps

The derivations and the history of `.github/workflows/ci-job-download-retry.yaml`, which points
into this file with `# why:` lines. A value's own arithmetic stays beside the value. What the two
jobs' rigs are and how they work is in `.github/ci-budgets.md`, "The download-retry job's rig" and
"The download-retry-images job's rig".

## Each image build's budget: the gate's price for that build

The `--budget` each report step of the download-retry-images job holds its build to is the price
the gate itself puts on that build: its measured build time plus the longest single ladder of the
Dockerfile it builds, the two terms the price comment of the key holding that build carries where
the image is built for real (`.github/scripts/check-download-retry.sh`, rule 10). Rule 10's
model is that a build runs its fetches one after another and pays a fault window once, through
the first fetch that meets it, so one ladder pays for the injected 60 s fault, whichever fetch
meets it first. The report checks the gate's promise and not a tighter one, and a build that
runs past the price its own key puts on it still fails.

None of those keys holds the probe, which is this rig's own: `report` measures a build from the
moment its proxy started, and the probe's two requests are made between the two, each bounded at
10 s by the rig's PROBE_BOUND. So each budget is the gate's price plus 20 s, as every site of the
download-retry job carries the probe's 20 s.

- The worker image: the key of `.github/workflows/worker-image.yaml`'s `build` job, which holds
  that build and no other fetch but the ci-ok report: 1800 s, the 30 minutes the job carried,
  which holds its build, plus the `go` stage's `go mod download` of
  packages/daemon/docker/worker.Dockerfile, 5 x 900 + 150 = 4650 s. That key names no measured
  build of its own, so its 1800 s is the measured term here too. 1800 + 4650 + 20 = 6470 s.
- The Envoy image: the key of `.github/workflows/ci-job-envoy-smoke.yaml`'s job, whose comment
  names this build's own 203 s (job 133743044741), plus the `bun install` and the `go mod
  download` of packages/envoy/docker/Dockerfile at 5 x 600 + 150 = 3150 s each.
  203 + 3150 + 20 = 3373 s.
- The held-port image: the same key, whose comment names this build's 11 s in the same job, plus
  the wrapper stage's `go mod download` of packages/envoy/infra/host/heldportwrapper/Dockerfile,
  5 x 600 + 150 = 3150 s. 11 + 3150 + 20 = 3181 s.
- The browser image: the key of `.github/workflows/browser-image.yaml`'s "Build and push" step:
  66 s, the slowest of the image's 33 cold builds measured in that job (job 128991520632 of run
  42565616054), plus the package set's `apt-get install` of .github/docker/browser.Dockerfile,
  5 x 300 + 150 = 1650 s, above the signing key's and the index read's 750 s each.
  66 + 1650 + 20 = 1736 s.

The Envoy and held-port arms take each build's own measured time, which envoy-smoke's key names,
rather than the 1200 s that key carries for its whole job: that term holds both builds and the
job's tests together, so charging it to each arm would count it twice, and the four arms with the
rest of this job would then come to 22996 s, above the 360 minutes GitHub gives a job on a hosted
runner.

Until job 136575882603 of the run at 95587c62 failed, these arms carried 2720, 920, 500 and 384 s,
each set by hand from one measured build and a margin. The browser's was 100 + 20 + 264 = 384 s:
the recovering fault lets the first attempt through no later than 100 s after the proxy starts,
the probe's 20 s, and four times the 66 s build, about the margin the Envoy image's 920 s kept
over its 239 s faulted build in job 133724372176. In job 136575882603 the browser arm took 915 s:
the injected fault on the PostgreSQL signing key recovered on its fourth attempt, then
azure.archive.ubuntu.com, outside the injected fault, served at 121 kB/s, so two `apt-get install`
attempts reached their 300 s bound and the third succeeded. The retry held; the budget had assumed
that every fetch after the fault meets a healthy host. That job's four arms took 540, 272, 130 and
915 s, each under the budget above.
