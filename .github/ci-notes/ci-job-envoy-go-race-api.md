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
- **One test was 595 s of that**, `TestEveryReadRowWritesOnlyItsProofAndCachesAndAnswersAsItsWriter`,
  and it ran alone, so it blocked every other test of its own leg: the first three groups gave it
  a leg of its own, and in run 45717135535 that leg took 609.9 s beside 412.2 and 417.0 s.

#5634 then ran that test's rows on eight worlds at once, and it fell to 95 s. The groups were
dealt again from two runs of the whole package on this lane's runner at main 3ebc542c, which
carries #5634 (throwaway run
[45782644966](https://github.com/cybermonklabs/legion/actions/runs/45782644966), branch
ci/envoy-api-redeal-measure, deleted after it; jobs 139781577470 and 139781577002, `go test -json
-count=1 -race -timeout 70m`): the package took 1,043.1 and 999.0 s, its 2,900 tests summing
4,893 and 4,753 s, the 221 that run alone 473 and 457 s. Each test weighs the longer of its two
times, or that over 8 where it runs beside others, and the three groups model 357 s each. A test
that runs alone is still the floor of its leg, and the longest is now that one at 95 s, so three
legs are far from it.

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

The `packages/envoy` entry of `.github/go-legs.yaml` lists every package of the module in one place:
a leg of envoy-go, envoy-go-race, envoy-go-race-api or envoy-go-race-docs, or `elsewhere` with the
job that runs it; and it names under `unlisted` the plain job, envoy-go, whose last leg runs a
package no place names. So a pull request that adds an envoy package and no line here does not
fail these legs: every leg warns, and envoy-go's last leg runs the package, once. A pull request
that removes or renames a package the entry still names does fail them: `go_legs.py` refuses a
name `go list ./...` no longer prints, in every leg of every job that reads the entry, until the
line goes with the package.
