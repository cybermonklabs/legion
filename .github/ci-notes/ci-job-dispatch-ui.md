# Notes for the dispatch-ui job's steps

The derivations of `.github/workflows/ci-job-dispatch-ui.yaml`, which points into this file.
Each `# why:` line of that workflow names the section below that holds the derivation its value
rests on. A value's own arithmetic stays beside the value.

## The job's limit, held to the installs it runs

The slowest green run of this job in the 20 merge-group runs `.github/scripts/network-install.sh`'s
bounds were measured on (44633135919 to 44668680855) took 308 s, job 136064256637 of run
44668680855; the 15 green runs of the job among them were read from the jobs API on 2026-11-23.
The job runs one install through that script, the frozen install, which can take 425 s on a stalled
registry before the script fails it by name: three attempts of bun's 120 s, each killed 15 s after
its SIGTERM, with two 10 s pauses. And `.github/actions/install-zsh` fetches each of its two pinned
files the Actions cache lacks through `ci_tools_curl`, which `.github/ci-notes/ci.md` ("The worst
case of the tool installs that read .github/scripts/ci-tools.sh") counts at 152 s a fetch with that
action's bound of 60 s. So the limit is 308 s + 425 s + 2 × 152 s = 1,037 s, rounded up to 18
minutes. The 15 minutes main gives the job would end a stalled install in GitHub's cancellation,
which names nothing, before the script failed it by name.
