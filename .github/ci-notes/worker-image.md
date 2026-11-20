# Notes for the worker image's workflow

The derivations of `.github/workflows/worker-image.yaml`, which points into this file. Each
`# why:` line of that workflow names the section below that holds the derivation its value rests
on. A value's own arithmetic stays beside the value.

## The build's limit, held to the installs it runs

Every wait on the network of `packages/daemon/docker/worker.Dockerfile` runs through
`.github/scripts/network-install.sh`, bind-mounted for each RUN that installs, or is a curl with a
`--max-time` of its own; mise's installs end a stalled wait by themselves. Each wrapped install can
take three attempts cut at its kind's bound, each killed 15 s after its SIGTERM, with two 10 s
pauses, before it fails by name. BuildKit builds the stages one stage does not need from another
side by side, so what the build can spend on them is the longest chain of stages the last one needs,
which rule 22 of `.github/scripts/check-ci-filters.sh` counts: the plugin stage's chain, 2,485 s.
That is the builder's pull of the Bun image, 180 s, what a healthy pull takes (PULL_SECONDS, in
`.github/scripts/ci_network_installs.py`; the builder gives a FROM pull no bound of its own); the
plugin stage's apt update and its packages' download, 245 s each (60 s bounds; the install from that
download runs unwrapped, as every apt install here does, since unpacking is no network wait); its
install of the whole root workspace, 965 s (the bun-image kind's 300 s bound); and two more bun
installs, 425 s each (120 s). The build step's slowest green run below includes the unpacking.

The build step took 1,183 s at its slowest of 9,989 green runs on pull requests from 2026-10-27 to
2026-11-20, job 133793114356, and that job took 1,265 s, the slowest whole run of them. So the build
step's limit is 1,183 s + 2,485 s = 3,668 s, rounded up to 62 minutes, and the job's is 1,265 s +
2,485 s = 3,750 s, rounded up to 63 minutes. A shorter limit would end a stalled install in GitHub's
cancellation, which names nothing, and rule 22 refuses it.
