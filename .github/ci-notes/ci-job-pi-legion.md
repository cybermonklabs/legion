# Notes for the pi-legion job's steps

The derivations of `.github/workflows/ci-job-pi-legion.yaml`, which points into this file. Each
`# why:` line of that workflow names the section below that holds the derivation its value rests
on. A value's own arithmetic stays beside the value.

## The job's limit, held to the installs it runs

The job's limit must end after each wait on the network the job makes can fail by name: up to each
step that installs, the bounds of every step before it (its own limit, or its installs where it
sets none), the runner's pulls and the step's own installs. For this job that sum is 4,597 s, which
77 minutes (4,620 s) hold:

- 240 s: before the first step the runner pulls the NATS service image with `docker pull`, which
  has no bound of its own and takes none from a workflow, so the job's limit is its one bound; it
  counts at twice the slowest green pull, rounded up to the minute (`.github/ci-notes/ci.md`, "The
  runner's own pulls of a job's service and container images").
- 2,300 s: `.github/actions/install-jj`, at the worst case `.github/ci-notes/ci.md`'s "The worst
  case of the tool installs that read .github/scripts/ci-tools.sh" derives.
- 1,207 s: `.github/actions/install-omp`'s download of the pinned Oh My Pi, `curl --retry 3
  --retry-all-errors --max-time 300`: four attempts of 300 s, with curl's own pauses of 1, 2 and
  4 s between them, before the action fails by name.
- 425 s: the pull of the judge image `scripts/skill-scenarios/rig.sh` runs (its `judge_image`),
  three attempts of docker's 120 s, each killed 15 s after its SIGTERM, with two 10 s pauses. rig.sh
  pulls that image itself where the daemon lacks it, with three `docker pull`s that carry no bound
  of their own on a body that trickles, in the test step, so the job pulls it first in a step of
  its own and the test finds it in the daemon's store.
- 425 s: the frozen `bun install`, three attempts of bun's 120 s, each killed 15 s after its
  SIGTERM, with two 10 s pauses.

The 28 minutes main gives the job end before that sum, so a stalled install there would end in
GitHub's cancellation, which names nothing. The job's comment holds its other bound: its steps'
own sum at their longest, 396 s.

Neither `./.github/actions/setup-bun` nor `actions/setup-go` and the go-daemon cache restore is in
the sum, as in every other job's: neither runs an install through network-install.sh or a bound of
this repository's own.
