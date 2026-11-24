# Notes for the envoy-go-race job's steps

The derivations of `.github/workflows/ci-job-envoy-go-race.yaml`, which points into this file.
Each `# why:` line of that workflow names the section below that holds the derivation its value
rests on. A value's own arithmetic stays beside the value.

## The job's limit, held to the downloads its forced-order steps run

Each run of `packages/envoy/scripts/forced-order.sh` downloads nats-server's module under
`.github/scripts/network-install.sh`'s bound for go: three attempts of 120 s, each killed 15 s
after its SIGTERM, with two 10 s pauses, 425 s before it fails by name. Three steps of this job
make that download: the script's own test (its skip-only case runs the script through to the
build; its four other cases refuse before the download) and the two forced-order steps. The
job's limit must end after the last of them can fail by name: up to it, the runner's pull, the
bounds of every step before it (its own limit, or its downloads and its go tests' worst case where
it sets none) and its own download. For this job that sum is 5,434 s, which 91 minutes (5,460 s)
hold:

- 240 s: before the first step the runner pulls the Postgres service image with `docker pull`,
  which has no bound of its own and takes none from a workflow, so the job's limit is its one
  bound; it counts at twice the slowest green pull, rounded up to the minute
  (`.github/ci-notes/ci.md`, "The runner's own pulls of a job's service and container images").
- 19 s: the step that sizes the Postgres service for the lane's Dispatch processes
  (`.github/scripts/pg-connection-ceiling.sh`), which sets no limit of its own, at its measured
  time: its cold build of the connections command and the service's restart took 19 s in job
  137890840698, a run of this job.
- 2,100 s: the race step, at the worst case the job's comment gives it: up to 5 minutes to compile
  the race binaries, then its -timeout of 30 minutes.
- 1,325 s: the test of forced-order.sh's refusals: its download, 425 s, the compile of its hooked
  build, up to 5 minutes as the comment gives a forced-order step, and the -timeout forced-order.sh
  gives go test, 10 minutes.
- 1,325 s: the forced-order step on the nats-server the module links, the same three parts.
- 425 s: the download of the forced-order step on nats-server v2.10.29.

240 s + 19 s + 2,100 s + 1,325 s + 1,325 s + 425 s = 5,434 s. The 80 minutes the job had end
before that sum, so a stalled download there would end in GitHub's cancellation, which names
nothing. The job's comment holds its other bound, its go test steps' own sum with the downloads at
their healthy speed, 71 minutes.
