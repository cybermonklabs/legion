# Notes for the pi-shared job's steps

The derivations of `.github/workflows/ci-job-pi-shared.yaml`, which points into this file. Each
`# why:` line of that workflow names the section below that holds the derivation its value rests
on. A value's own arithmetic stays beside the value.

## The job's limit, held to the installs it runs

The job's limit must end after each wait on the network the job makes can fail by name: up to each
step that installs, the bounds of every step before it (its own limit, or its installs where it
sets none) and the step's own installs at their worst case. The job pulls no service or container
image. It is held to the slowest green run read with every install added at its worst, which
counts each step before an install at no less than its slowest:

- 41 s: the slowest of the four green runs of 2026-11-24 read from the jobs API (37 s to 41 s,
  job 138209061390 of merge-group run 45323958245); its frozen install took 8 s and its install of
  jj 2 s of those.
- 425 s: the frozen `bun install`, through `.github/scripts/network-install.sh`'s bound for bun:
  three attempts of 120 s, each killed 15 s after its SIGTERM, with two 10 s pauses.
- 2,300 s: `.github/actions/install-jj`, at the worst case `.github/ci-notes/ci.md`'s "The worst
  case of the tool installs that read .github/scripts/ci-tools.sh" derives.

41 s + 425 s + 2,300 s = 2,766 s, rounded up to 47 minutes. The 10 minutes main gives the job end
before install-jj's worst case alone, so a stalled install there would end in GitHub's
cancellation, which names nothing.

`./.github/actions/setup-bun` is left out, as in every other job's sum (`ci-job-pi-legion.md`): its
first source, oven-sh/setup-bun's own download, has no bound of this repository's to count.
