# Notes for the ci-images job's steps

The derivations of `.github/workflows/ci-job-ci-images.yaml`, which points into this file. Each
`# why:` line of that workflow names the section below that holds the derivation its value rests
on. A value's own arithmetic stays beside the value.

## The job's limit, held to the crane calls of the image copy

`.github/scripts/mirror-ci-images.sh` runs every crane call through
`.github/scripts/network-install.sh`'s bound for crane: three attempts of 60 s, each killed 15 s
after its SIGTERM, with two 10 s pauses, 245 s before it fails by name. The job's limit must end
after the last of them can fail by name: up to it, the bounds of every step before it and the
calls themselves. The job has no service and no container, so the runner pulls nothing.

- 848 s: `.github/actions/install-crane`, at the worst case `.github/ci-notes/ci.md`'s "The worst
  case of the tool installs that read .github/scripts/ci-tools.sh" derives for an x86_64 runner.
- The copy step, by the rows of `.github/ci-images.yaml` (15 copied rows and 1 built row today).
  A built row makes no call. A copied row the package holds makes one, `crane digest` of its tag;
  one the package lacks makes three, that read, `crane copy` and `crane digest` of what it copied.
  The library/golang row makes one more, `crane config` of its Go. Which rows the package lacks is
  the package's state and not this tree's, so a row counts at the worst the tree allows, copied:
  3 × 245 s = 735 s, and the library/golang row 4 × 245 s = 980 s. The script runs the rows 4 at a
  time (PARALLEL in it, which says why 4), each row's calls one after another, and starts each row
  as an earlier one ends, in row order. So the rows end by the time ceil(rows / 4) rows have run one
  after another at the longest row's worst: ceil(15 / 4) × 980 s = 4 × 980 s = 3,920 s. (Rows k × 4
  + 1 to (k + 1) × 4 each start by the time k × 4 rows have ended, which is by k × 980 s, and each
  takes at most 980 s. The built row ends at once.)

848 s + 3,920 s = 4,768 s, which 80 minutes (4,800 s) hold. With the rows one after another the
same rows would come to 15 × 735 s + 245 s = 11,270 s. The 25 minutes main
gives the job end before even 2 × 980 s + 848 s, so a run whose calls each waited out
most of their attempts could be ended by GitHub's cancellation, which names nothing, before a
later call failed by name.

The steps after the copy are no part of the sum, since no install follows them: the package mode
of `.github/scripts/check-ci-images.sh` bounds each read it makes (30 s for each of its HTTP
requests, 120 s for its `crane config`), and `.github/scripts/mirror-ci-tools.sh` reads under the
rows' own `max_time`.
