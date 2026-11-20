# Notes for the pi-envoy job's steps

The derivations of `.github/workflows/ci-job-pi-envoy.yaml`, which points into this file. Each
`# why:` line of that workflow names the section below that holds the derivation its value rests
on. A value's own arithmetic stays beside the value.

## The job's limit, held to the installs it runs through network-install.sh

Rule 22 of `.github/scripts/check-ci-filters.sh` refuses a job limit that ends before each wait on
the network the job makes can fail by name: it sums, up to each step that installs, the bounds of
every step before it (its own limit, or its installs where it sets none), the runner's pulls and the
step's own installs. For this job that sum is 1,095 s, which the 23 minutes (1,380 s) hold:

- 180 s: before the first step the runner pulls the NATS service image with `docker pull`, which
  has no bound of its own and takes none from a workflow, so the job's limit is its one bound; the
  rule counts it at what a healthy pull takes (PULL_SECONDS, in
  `.github/scripts/ci_network_installs.py`).
- 490 s: the two apt commands of `.github/scripts/runner-tmux.sh`, 245 s each (three attempts of
  apt's 60 s, each killed 15 s after its SIGTERM, with two 10 s pauses). They run only on a runner
  image without tmux, which a later image can be, so they count.
- 425 s: the frozen `bun install`, three attempts of bun's 120 s, each killed 15 s after its
  SIGTERM, with two 10 s pauses.

The ten minutes the job had before this change end before that sum, so a stalled install there
would end in GitHub's cancellation, which names nothing. The steps after the install set no bound
of their own; the slowest green run of the whole job in the 20 merge-group runs
`.github/scripts/network-install.sh`'s bounds were measured on (44633135919 to 44668680855) took
443 s, job 136032711558.
