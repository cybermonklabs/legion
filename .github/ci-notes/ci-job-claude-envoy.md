# Notes for the claude-envoy job's steps

The derivations of `.github/workflows/ci-job-claude-envoy.yaml`, which points into this file.
Each `# why:` line of that workflow names the section below that holds the derivation its value
rests on. A value's own arithmetic stays beside the value.

## The job's limit, held to the installs it runs through network-install.sh

The slowest green run of this job in the 20 merge-group runs `.github/scripts/network-install.sh`'s
bounds were measured on (44633135919 to 44668680855) took 217 s, job 136018339483. The job runs
two installs through that script: Claude Code's `bun add` in `.github/actions/install-claude-code`
and the frozen install. Each can take 425 s on a stalled registry before the script fails it by
name: three attempts of bun's 120 s, each killed 15 s after its SIGTERM, with two 10 s pauses.
So the limit is 217 s + 850 s = 1,067 s, rounded up to 18 minutes. The ten minutes the job had
would end a stalled install in GitHub's cancellation, which names nothing, and rule 22 of
`.github/scripts/check-ci-filters.sh` refuses a job limit that ends before its wrapped installs
can fail by name.
