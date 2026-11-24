# Notes for the claude-envoy job's steps

The derivations of `.github/workflows/ci-job-claude-envoy.yaml`, which points into this file.
Each `# why:` line of that workflow names the section below that holds the derivation its value
rests on. A value's own arithmetic stays beside the value.

## The job's limit, held to the installs it runs through network-install.sh

The slowest green run of this job in the 20 merge-group runs `.github/scripts/network-install.sh`'s
bounds were measured on (44633135919 to 44668680855) took 217 s, job 136018339483. The job runs
three installs through that script: Claude Code's `bun add` in `.github/actions/install-claude-code`,
the frozen install, and the pull of the NATS image the reply-grant row starts (docker's bound is
120 s too). Each can take 425 s on a stalled registry before the script fails it by name: three
attempts of 120 s, each killed 15 s after its SIGTERM, with two 10 s pauses. And
`.github/actions/install-zsh` fetches each of its two pinned files the Actions cache lacks through
`ci_tools_curl`, which `.github/ci-notes/ci.md` ("The worst case of the tool installs that read
.github/scripts/ci-tools.sh") counts at 152 s a fetch with that action's bound of 60 s. So the
limit is 217 s + 3 × 425 s + 2 × 152 s = 1,796 s, rounded up to 30 minutes. The ten minutes main
gives the job would end a stalled install in GitHub's cancellation, which names nothing, before
the script failed it by name.
