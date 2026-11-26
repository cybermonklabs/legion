# Notes for the envoy-go-race-api job's steps

The derivations and the history of `.github/workflows/ci-job-envoy-go-race-api.yaml`, which points
into this file. Each `# why:` line of that workflow names the section below that holds the
derivation or the record its job or step rests on. A value's own arithmetic stays beside the value.

## Why the API's tests run in three legs

`internal/dispatch/api` is one package, and it set this lane's time: 1,476.4 s of a 1,578.6 s step.
Measured on this lane's own runner, in the throwaway run
[45669988563](https://github.com/cybermonklabs/legion/actions/runs/45669988563) (branch
ci/envoy-race-split-measure, deleted after it) on `ubuntu-24.04-arm` with this job's Postgres
service and its command, `-json` for each test's own time and `/proc/stat` every 5 s (job
139402324284):

- **The step is not CPU-bound**, so a bigger runner would buy little: the four vCPUs were 47.1%
  busy over the step. It is not evenly busy either — the 5-second intervals read a median of
  34.6% and a 90th percentile of 97.6%, with 125 of 315 above 90% — because the package's 2,788
  tests run 8 at a time (`apiTestParallelism`) in bursts between waits on the service.
- **The tests' own times sum to 5,835 s**, a mean 3.95 running over the package's 1,476 s, and
  the 174 tests that run alone sum 847 s. So the lever is more test slots, which means more
  runners: a leg of its own for each group of its tests.
- **One test is 595 s of that**, `TestEveryReadRowWritesOnlyItsProofAndCachesAndAnswersAsItsWriter`,
  and it runs alone, so it blocks every other test of its own leg: it is the floor of whatever leg
  holds it, and three legs put it in a leg of its own. Below about 600 s this lane needs that
  test's own rows to run beside each other, which is the package owner's change and not this one.

Three legs, from the measured times: group 1 is that one test, and groups 2 and 3 model 438 s
each. A fourth leg would move nothing, since the floor stays 595 s. Measured: in run
45717135535 leg 1 ran that one test in 609.9 s and legs 2 and 3 took 412.2 and 417.0 s. Main
then added 76 API tests no group named; go_legs.py placed them, 27 of them in group 1, so in run
45735800190 leg 1 ran 28 tests in 631.3 s beside 513.4 and 417.1 s. The 76 are named in the
groups file now: the 27 that fell to group 1 are in group 3, the lightest, and the rest where they
were placed, so leg 1 is that one test again.

The groups are `.github/go-legs/packages/envoy/internal/dispatch/api.yaml`, one group per leg, and
the rules they keep are in
[ci-job-daemon-go.md](ci-job-daemon-go.md#why-internaldaemons-tests-run-in-three-legs): each test
in exactly one group, held by `.github/scripts/go_legs.py` before the leg runs; an anchored
`-run`; a leg that runs a group runs that package alone; and the old check-run name on a job that
waits for the legs.

What is new here, beside the daemon's: the step runs through
`packages/envoy/scripts/database-skips.sh`, which counts the run's skips. A leg runs a share of the
package, so that script takes the leg's `-run=^(…)$` as its own argument. An `EXPECTED_SKIPS` line
whose test the package still has and the pattern leaves out is another leg's and is not held in
this one; without that, every leg would report `database_skips=UNKNOWN` for the expected skips its
group does not hold. A line whose test the package no longer has is held in every leg, so a deleted
or renamed test's line still reads `UNKNOWN` (`packages/envoy/scripts/database-skips.test.sh`,
control 7).

The `packages/envoy` entry of `.github/go-legs.yaml` names only the two packages these legs and the
document service's run, with no `elsewhere`: it claims nothing about the module's other packages,
which run in jobs that do not read the file. So a pull request that adds or removes an envoy
package, which selects neither lane, cannot leave these legs failing in a later merge group.
