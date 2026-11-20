# Notes for the daemon-go job's steps

The derivations of `.github/workflows/ci-job-daemon-go.yaml`, which points into this file. Each
`# why:` line of that workflow names the section below that holds the derivation its value rests
on. A value's own arithmetic stays beside the value.

## The job's limit, held to the installs it runs

The job's limit is the 70 minutes its comment derives from the go test steps' own bounds: each
step's slowest green run plus its own `-timeout`, so a hung test prints the goroutine dump that names
it, under 4,000 s in all. It must also leave room for every wait on the network the job makes, so
each one fails by name before GitHub's cancellation, which names nothing. Rule 22 of
`.github/scripts/check-ci-filters.sh` holds it to that: it sums, up to each step that installs, the
bounds of every step before it (its own limit, or its installs and its go tests' own `-timeout`
where it sets none), the runner's pulls and the step's own installs, and refuses a limit that ends
first. For this job that sum is 1,945 s, which 70 minutes (4,200 s) holds:

- 180 s: before the first step the runner pulls the Postgres service image with `docker pull`,
  which has no bound of its own and takes none from a workflow, so the job's limit is its one
  bound; the rule counts it at what a healthy pull takes (PULL_SECONDS, in
  `.github/scripts/ci_network_installs.py`).
- 490 s: the two apt commands of `.github/scripts/runner-tmux.sh`, 245 s each (three attempts of
  apt's 60 s, each killed 15 s after its SIGTERM, with two 10 s pauses). They run only on a runner
  image without tmux, which a later image can be, so they count.
- 1,275 s: the three NATS test images `.github/actions/pull-test-images` pulls, 425 s each.

The Postgres image `scripts/e2e/stage1-skeleton.sh` pulls (425 s) is left out, and the rule says
why: the pull stands in that script's `if [ -z "${LEGION_E2E_PG_DSN:-}" ]` branch, the devbox's
path, and this job's env sets LEGION_E2E_PG_DSN, so in this job the branch never runs. The rule
reads the branch's test from the script, so a job that dropped the variable would count the pull
again.
