# Notes for the envoy-go-checks job's steps

The derivations of `.github/workflows/ci-job-envoy-go-checks.yaml`, which points into this file.
Each `# why:` line of that workflow names the section below that holds the derivation its value
rests on. A value's own arithmetic stays beside the value.

## The job's limit, held to the installs it runs

The job's limit must end after each wait on the network the job makes can fail by name: up to each
step that installs, the bounds of every step before it (its own limit, or its installs where it
sets none) and the step's own installs at their worst case. The job pulls no service or container
image, and its test steps set no limit of their own, so it is held to the slowest green run read
with every install added at its worst, which counts each step before an install at no less than its
slowest:

- 1,328 s: the slowest green run. The tests' steps took 21.4 minutes at most, 1,284 s, over 695
  pull-request runs of 2026-11-22, which the job's comment cites, and the setup before them took
  44 s at most in the six green merge-group runs of 2026-11-24 read from the jobs API (job
  138214582900: the checkout, `./.github/actions/setup-bun`, the frozen install, `actions/setup-go`
  and the go-envoy restore). Those six took 861 s to 1,030 s whole (the slowest, job 138209054066
  of merge-group run 45323956275), so the older sample is the larger, and it is the one counted.
- 425 s: the frozen `bun install`, through `.github/scripts/network-install.sh`'s bound for bun:
  three attempts of 120 s, each killed 15 s after its SIGTERM, with two 10 s pauses.
- 848 s: `.github/actions/install-helm`, whose row reads get.helm.sh and then the copy in
  ghcr.io/cybermonklabs/legion/ci, at the worst case `.github/ci-notes/ci.md`'s "The worst case of
  the tool installs that read .github/scripts/ci-tools.sh" derives for such a row, whose
  17,455,249-byte tarball takes 120 s: 212 s, then 3 × 212 s.

1,328 s + 425 s + 848 s = 2,601 s, rounded up to 44 minutes. The 30 minutes main gives the job end
before that sum, so a stalled install there would end in GitHub's cancellation, which names
nothing.

`./.github/actions/setup-bun` and `actions/setup-go` with the go-envoy cache restore are left out,
as in every other job's sum (`ci-job-pi-legion.md`): neither has a bound of this repository's to
count on its first source.
