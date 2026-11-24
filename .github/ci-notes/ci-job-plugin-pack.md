# Notes for the plugin-pack job's steps

The derivations of `.github/workflows/ci-job-plugin-pack.yaml`, which points into this file. Each
`# why:` line of that workflow names the section below that holds the derivation its value rests
on. A value's own arithmetic stays beside the value.

## The job's limit, held to the installs it runs and to its test step's deadline

The job's limit holds two sums, and is the larger of them.

Each wait on the network the job makes must fail by name before the limit: up to each step that
installs, the bounds of every step before it (its own limit, or its installs where it sets none)
and the step's own installs at their worst case. The job pulls no service or container image. It is
held to the slowest green run read with every install added at its worst, which counts each step
before an install at no less than its slowest:

- 216 s: the slowest of the nine green runs of 2026-11-24 read from the jobs API (167 s to 216 s,
  job 138188997494 of merge-group run 45317831962); its frozen install took 5 s, its install of
  Oh My Pi 6 s and its test step 69 s of those.
- 425 s: the frozen `bun install`, through `.github/scripts/network-install.sh`'s bound for bun:
  three attempts of 120 s, each killed 15 s after its SIGTERM, with two 10 s pauses.
- 1,207 s: `.github/actions/install-omp`'s download of the pinned Oh My Pi, `curl --retry 3
  --retry-all-errors --max-time 300`: four attempts of 300 s, with curl's own pauses of 1, 2 and
  4 s between them, before the action fails by name.

216 s + 425 s + 1,207 s = 1,848 s.

The test step's `-timeout 15m` must fire before the limit too, so a hung pack prints the goroutine
dump that names it: the slowest green run and the 15 minutes, 216 s + 900 s = 1,116 s. The test
step's own share of a green run is in the 216 s (69 s in that run, and 82 s at most of the nine,
job 138209061148, in a run of 177 s).

So the limit is 1,848 s, rounded up to 31 minutes. The 30 minutes main gives the job end before
that sum, so a stalled Oh My Pi download there would end in GitHub's cancellation, which names
nothing.

`./.github/actions/setup-bun` and `actions/setup-go` with the go-daemon cache restore are left out,
as in every other job's sum (`ci-job-pi-legion.md`): neither has a bound of this repository's to
count on its first source.
