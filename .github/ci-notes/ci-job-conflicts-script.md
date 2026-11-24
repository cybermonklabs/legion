# Notes for the conflicts-script job's steps

The derivations of `.github/workflows/ci-job-conflicts-script.yaml`, which points into this file.
Each `# why:` line of that workflow names the section below that holds the derivation its value
rests on. A value's own arithmetic stays beside the value.

## The job's limit, held to the installs it runs

The job runs in merge groups, so its limit is part of the longest chain a merge group can wait on
before ci-ok judges it, which main's merge queue fails 180 minutes in (`ci.yaml`'s changes job says
what that chain is). With no `timeout-minutes` the job had GitHub's default of 360 minutes, which
alone would carry that chain past 180.

The limit must end after each wait on the network the job makes can fail by name: up to each step
that installs, the bounds of every step before it and the step's own installs at their worst case.
The job pulls no service or container image. It is held to the slowest green run read with every
install added at its worst, which counts each step before an install at no less than its slowest:

- 50 s: the slowest of the 27 green runs of the job among ci.yaml's 100 latest runs, read from the
  jobs API on 2026-11-24 (23 s to 50 s, median 29 s; job 138611723963 of pull-request run
  45436724420). Its checkout took 12 s, its frozen install 3 s and its lint 5 s of those.
- 425 s: the frozen `bun install`, through `.github/scripts/network-install.sh`'s bound for bun:
  three attempts of 120 s, each killed 15 s after its SIGTERM, with two 10 s pauses.

50 s + 425 s = 475 s, rounded up to 8 minutes.

A second sweep of the same window on 2026-11-24, later in the day, read 98 conflicts-script jobs
in ci.yaml's 100 latest runs, of which 20 were green with times: 27 s to 39 s, median 30 s, and
none over the 480 s this limit gives. The 50 s above is the slower of the two windows, so the
limit keeps it.

`./.github/actions/setup-bun` is left out, as in every other job's sum (`ci-job-pi-legion.md`): its
first source, oven-sh/setup-bun's own download, has no bound of this repository's to count.
