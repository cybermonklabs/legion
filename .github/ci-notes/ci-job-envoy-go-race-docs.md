# Notes for the envoy-go-race-docs job's steps

The derivations and the history of `.github/workflows/ci-job-envoy-go-race-docs.yaml`, which points
into this file. Each `# why:` line of that workflow names the section below that holds the
derivation or the record its job or step rests on. A value's own arithmetic stays beside the value.

## Why the document service's tests run in three legs

`internal/dispatch/docs` is one package, and it is this lane's whole step: 1,430.0 s in the
throwaway run [45669988563](https://github.com/cybermonklabs/legion/actions/runs/45669988563)
(branch ci/envoy-race-split-measure, deleted after it), job 139402324250, on `ubuntu-24.04-arm`
with this job's Postgres service, its memory sampler, its cleanup detector and its command, with
`/proc/stat` every 5 s:

- **The step is not CPU-bound**: the four vCPUs were 52.8% busy, the 5-second intervals a median
  of 50.9% and a 90th percentile of 91.1%, 35 of 285 above 90%. `MemAvailable` never fell below
  8.97 GiB, and the sampler's own peak reading was 5,264,676 kB of the 8,388,608 kB limit.
- **The 1,121 tests' own times sum to 3,831 s** over that 1,430 s, so about 2.7 running at once:
  more slots means more runners.
- **The two longest tests are 491.6 s and 321.8 s**
  (`TestAMarginWriteMatchesTheWholeDocumentWrite` and
  `TestACaptureNeverReusesARenderingTheForkHasMovedPast`), so no split takes this lane below about
  500 s until those two are shorter.

Three legs, from the measured times: each models 444 s, and the longest test's 491.6 s is the
floor of the leg that holds it. A fourth leg would move nothing.

The groups are `.github/go-legs/packages/envoy/internal/dispatch/docs.yaml`, one group per leg, and
the rules they keep are in
[ci-job-daemon-go.md](ci-job-daemon-go.md#why-internaldaemons-tests-run-in-three-legs).

Two things this lane needs that the daemon's did not:

- its step runs the go command under `.github/scripts/race_memory.py`, so the leg's packages and
  `-run` reach it through `xargs env GODEBUG=… RACE_MEM_LIMIT_KB=… …`: `xargs` runs a program and
  not a shell, and `env` is what carries the two variables the step sets.
- the sampler reads the `-json` stream and writes its `Output` fields back out, so a run of this
  lane is not a `-json` stream a rebalance can read as one.
  `.github/scripts/go_legs_groups.py` reads such a run from its verbose lines instead
  (`--- PASS: <test> (<seconds>s)` and `=== PAUSE <test>`), which carry each test's own seconds
  and its pause.
