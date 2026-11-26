# Notes for the daemon-go job's steps

The derivations and the history of `.github/workflows/ci-job-daemon-go.yaml`, which points into
this file. Each `# why:` line of that workflow names the section below that holds the derivation
or the record its job or step rests on. A value's own arithmetic stays beside the value.

## Why the race step is a job of its own

The workflow held two jobs, which ran at the same time: daemon-go-race ran the race step, and
daemon-go ran the plain step, the stamped shape gate, the race-impossible step, the forced-order
step and the Stage 1 proof. The plain step's package list is every package of the module less the
race step's five (and the text checks), so the two steps share no package and nothing orders
them. Each now runs in legs ([below](#why-the-daemons-tests-run-in-legs)).

In one job their times added up. Over the 158 green daemon-go jobs of ci.yaml runs on 2026-11-22
from 13:46Z to 18:10Z (jobs API; the sample is the job list read at 18:57Z), the race step took
26.8 min at the median (36.3 at most) and the plain step 12.0 (15.0), after 1.4 min of setup, for
a job of 41.0 min. At the same time, the race job alone sets the wall time, and the second runner
costs the setup it repeats, about 1.4 min.

The split makes no test do less work. The race step's time is one package's: internal/daemon took
1,352 s of the step's 1,422 s in run 45048924209, and the four other packages of the step ended
inside it. That package's tests are where the rest of the time is.

## Why the daemon's tests run in legs

A go test step lasts as long as its longest package, or as its packages' sum over the runner's
four cores when that is longer, and most of this module's packages run their tests one after
another. go test starts packages in the order it is given them, and the plain step gave them in
import-path order, so one long package that came late set the step's time. In merge group
45553934471 (2026-11-25, 19.1 minutes, a group that ran only the daemon's jobs):

- daemon-go took 940 s, 809 s of it the plain step. internal/store ran 542 s and ended with the
  step, so it started 267 s in, behind the packages before it. The step's 91 packages summed
  2,269 s; after internal/store the longest were internal/runtime/sandbox 287 s,
  internal/workflow 281 s and cmd/legion 276 s (job 139002010987).
- daemon-go-race took 765 s, 667 s of it the race step: internal/daemon 629 s, internal/intake
  522 s and internal/launcher 311 s, each on a core of its own (job 139002011254).
- The jobs after them were lint at 331 s and pi-legion at 279 s, so the daemon's two jobs set the
  group's time.

So each step runs in legs: one job per leg, all at the same time, each over the packages
`.github/go-legs.yaml` lists for it, longest first. Plain leg 1 is internal/store, whose 542 s
sets its time, beside the next three, each on a core of its own; plain leg 2 is the other 87
packages, about 880 s of package time over four cores, and runs alone the steps no leg splits:
the race-impossible tests, the forced-order tests and Stage 1, 57 s in that job, and the shape
gate of internal/api stamped with main's merge order, added since (4 s with its stamp in job
139602068850). internal/daemon takes three race legs, a group of its tests each ([below](#why-internaldaemons-tests-run-in-three-legs));
race leg 4 is the other four packages, whose time is internal/launcher's (292 s in run
45601893438; internal/intake took 134 s there, its tests now beside each other). The longest leg
is then plain leg 1, internal/store's: 506 s of job in run 45678673783, against 463 s for the
longest race leg (a group of internal/daemon) and 385 s for race leg 4, where the merge group
waited 940 s for daemon-go.

The rules the legs keep:

- **Each package runs once, in one place.** A place is one leg, or, for a package split by test
  name as internal/daemon is in three race legs, one group of its tests in each of several legs
  of one job ([below](#why-internaldaemons-tests-run-in-three-legs)), every test in one group.
  `.github/scripts/go_legs.py`, which each leg's go test step runs for its packages, compares the
  file with `go list ./...` first and fails the step, naming each, when a package is in no place,
  in two, or when a place names no package. It also
  takes how many legs the job's matrix runs (`strategy.job-total`) and fails when the file's legs
  are not exactly those, since a leg the file has and the matrix does not launch runs in no job,
  and when a leg lists no package, since xargs would then run go test on the module's root. So a
  new package fails every leg until the file names it; `.github/scripts/go-legs.test.sh`, in the
  test job, holds those failures. A leg sees only its own job's legs, so a package filed under a
  job key no matrix job runs would pass every leg and run in none:
  `.github/scripts/ci_job_reads.py#hold_daemon_legs`, which the changes and ci-filters jobs import,
  refuses any key but daemon-go, daemon-go-race and `elsewhere`, and check-ci-filters.test.sh
  holds that.
- **Each leg runs in its step's mode.** A kind's legs are one matrix job with one step text, so
  -race, -skip and -timeout are the step's and no leg's own.
- **The old check-run names stay.** The jobs daemon-go and daemon-go-race wait for their kind's
  legs and pass only when every leg passed, so `daemon-go / daemon-go` and
  `daemon-go / daemon-go-race` mean what they meant to a person and to anything outside this run
  that reads a check run by name. ci-ok does not need them: it reads the result of
  `.github/workflows/ci.yaml#daemon-go`, which already combines every job of this file, legs
  included. Each leg also ends with the ci-ok report, so a failing leg posts a merge group's failing
  ci-ok without waiting for the others.
  The two jobs' error says they restate their legs' result, in the words
  `.github/scripts/runner-shutdown.sh#RUNNER_SHUTDOWN_RESTATED` holds, and that script passes over
  a check run carrying them: otherwise their own `exit code 1` would read every leg's runner
  shutdown as a failure of the change, and post an early failing ci-ok for it. ci-ok.test.sh holds
  both the reading and the two steps' words.
- **Rebalance from the legs' own times.** Each leg's log prints `ok <package> <seconds>s`. When
  a package's time moves, move packages between legs so no leg is longer than it has to be, and
  keep each leg's longest first.

What it costs:

- each leg repeats its setup, about 70 s for a plain leg and 100 s for a race leg, and a leg's
  runner sits partly idle when one package sets its time, so two legs a kind use more runner
  minutes than one job did: 1,705 runner seconds (29 billed minutes) in merge group 45553934471,
  1,939 and 2,135 s (37 and 39 billed minutes) in this change's runs 45586895835 and 45601893438;
- each of the two jobs that keep the old names is a second wait for a runner, after its kind's
  last leg and before ci-ok can start: in run 45601893438 `daemon-go / daemon-go-race` waited 79 s
  for a runner after the race legs ended at 11:23:39Z, then ran 6 s, and ci-ok started after it,
  so that run's daemon jobs ended 86 s after its last leg. That wait is the price of keeping the
  two names; dropping the two jobs and the restating marker would remove it and the names with it.

Standard runners on this public repository bill no money; the cost is runner concurrency and that
wait.

## Why internal/daemon's tests run in three legs

One leg of internal/daemon alone was the race job's time: 616 s with the Postgres service's crash
safety on, where the four other race packages took 290 s together. Measured on GitHub's own
runner, in the throwaway run
[45609991742](https://github.com/cybermonklabs/legion/actions/runs/45609991742) (branch
ci/daemon-race-cpu-scratch, deleted after it), each row a job of its own on ubuntu-24.04 with
this job's Postgres service, its images and its command, and the merge group's property count:

- **The step is not CPU-bound**, so a bigger runner would buy little. Over the whole step
  (job 139190706860, 615.9 s, package 611.6 s, 1,527 of the 1,530 listed tests run) `/proc/stat`
  every 5 s reads the four vCPUs **60.4% busy** — user 35.9%, system and interrupts 24.5%,
  iowait 6.5%, steal 0 — with the 5-second intervals at a median of 66.8% and a 90th percentile
  of 82.2%, and 3 of 122 above 90%.
- **Part of the wait was the service's disk.** The same commit and command with the service's
  crash safety off (job 139190707452, `.github/scripts/pg-crash-safety-off.sh`) took **458.7 s**,
  and iowait fell from 6.5% to 0.5%. That is #5515's change, now on main, and the 537 s this
  package takes in go-legs.yaml is a median with it.
- **The rest is test slots.** The tests' own times sum to 2,189 s over a 612 s package, a mean
  3.58 of the four slots `TestMain`'s `-parallel 4` gives, and the 65 tests that run alone sum
  69 s. More slots on the one runner did not help: at `-parallel 8` the package took 628.3 s
  (job 138480582657). So the lever is more runners, and the package is too long for one.

Three legs, not two, because the other race leg is the floor: it took 292 s of a 389 s job in
[45601893438](https://github.com/cybermonklabs/legion/actions/runs/45601893438) (job
139163705550). Splitting internal/daemon's 1,530 tests by name, as the same scratch run measured
them: two halves took 392.4 and 450.6 s (jobs 139190707040, 139190707062) and three thirds 198.0,
251.4 and 390.8 s (jobs 139190707083, 139190706503, 139190707056), each a round-robin over the
names rather than a balance. Two legs would leave internal/daemon setting the job's time; three,
balanced, model 278 s each at the crash-safety-on times they were measured with, and took
293.3, 259.6 and 218.3 s in run 45678673783, where the whole package had taken 641 s in one leg.

The groups are `.github/go-legs/packages/daemon/internal/daemon.yaml`, each group number to the
tests its leg runs, and the rules they keep are the legs' rules above, with two more:

- **Each test runs once, in one group, and a stale list does not stop a leg.** Before a leg runs,
  `.github/scripts/go_legs.py` lists the package's tests as that leg builds them
  (`go test -list .` under the step's own build flags, `-race` here, which decide which tests
  exist at all). It is an **error**, and the leg prints nothing, when a test is in two groups,
  when a group would run no test (the file gives it none, or every name it gives has gone from
  the package), when a group's pattern is over `PATTERN_LIMIT`, 100,000 bytes (Linux takes
  131,072 bytes in one argument, and the step's own words need room under it), when the groups
  the job's legs run and the groups the file has are not the same set, and when one leg runs a
  group beside another package, whose tests the group's `-run` would choose too: each of those
  would run a test twice, or run nothing. It is a
  **warning** when the list has merely gone stale against the source: a test no group names runs
  in `sorted(groups)[zlib.crc32(name) % len(groups)]`, the same group in every leg, and a group
  name the listing does not print matches nothing. That is deliberate. Over the seven days to
  2026-11-25, 61 of the 115 commits to main that touched `packages/daemon/internal/daemon/`
  changed its set of top-level tests — about nine pull requests a day — and a hand-edited list
  would have stopped each of them in three legs, after their setup, compile and listing, for
  nothing the exactly-once rule needs. `.github/scripts/go-legs.test.sh` holds each error, each
  warning and the placement, and holds the anchoring below by running the real `go test` the leg
  prints.
- **A group's `-run` is anchored**, `^(Name|Name|…)$`, so a test whose name begins with another's
  runs in its own leg only, and each test's subtests run with it. A leg that runs a group runs
  that package alone: a `-run` reaches every package of its `go test`, so a group beside another
  package would choose that package's tests too, and go_legs.py refuses it.

Rebalance with `.github/scripts/go_legs_groups.py <module> <package> <groups> <parallel> <list>
<source> <run>...`: it weighs each test by its measured time, or its time over the package's own
`-parallel` where it runs beside others, and deals the tests longest first into the groups. It
reads a `go test -json` stream, or the verbose lines of one where a wrapper printed them.

No leg's own run can feed it: this step runs `go test` without `-json`, and prints a package's
one `ok` line, not each test's time. A rebalance therefore needs a run made for it — a throwaway
`workflow_dispatch` of the lane's command with `-json`, or a build-cluster Job — on the runner
the legs run on, and the run it came from goes in the file's header, as the current one names
scratch run 45609991742. Rebalance when a leg's log shows the groups have drifted apart, and when
the warnings above name tests the placement rule put somewhere arbitrary.
