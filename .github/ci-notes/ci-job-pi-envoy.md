# Notes for the pi-envoy job's steps

The derivations of `.github/workflows/ci-job-pi-envoy.yaml`, which points into this file. Each
`# why:` line of that workflow names the section below that holds the derivation its value rests
on. A value's own arithmetic stays beside the value.

## The job's limit, held to the installs it runs through network-install.sh

The job's limit must end after each wait on the network the job makes can fail by name: up to each
step that installs, the bounds of every step before it (its own limit, or its installs where it
sets none), the runner's pulls and the step's own installs. For this job that sum is 2,362 s, which
40 minutes (2,400 s) hold:

- 240 s: before the first step the runner pulls the NATS service image with `docker pull`, which
  has no bound of its own and takes none from a workflow, so the job's limit is its one bound; it
  counts at twice the slowest green pull, rounded up to the minute (`.github/ci-notes/ci.md`, "The
  runner's own pulls of a job's service and container images").
- 1,207 s: `.github/actions/install-omp`'s download of the pinned Oh My Pi, `curl --retry 3
  --retry-all-errors --max-time 300`: four attempts of 300 s, with curl's own pauses of 1, 2 and
  4 s between them, before the action fails by name.
- 490 s: the two apt commands of `.github/scripts/runner-tmux.sh`, 245 s each (three attempts of
  apt's 60 s, each killed 15 s after its SIGTERM, with two 10 s pauses). They run only on a runner
  image without tmux, which a later image can be, so they count.
- 425 s: the frozen `bun install`, three attempts of bun's 120 s, each killed 15 s after its
  SIGTERM, with two 10 s pauses.

The 15 minutes main gives the job end before that sum, so a stalled install there would end in
GitHub's cancellation, which names nothing. The job's comment holds its other bound: its steps'
own sum at their longest, 755 s.
