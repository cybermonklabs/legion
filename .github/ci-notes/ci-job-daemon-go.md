# Notes for the daemon-go job's steps

The derivations and the history of `.github/workflows/ci-job-daemon-go.yaml`, which points into
this file. Each `# why:` line of that workflow names the section below that holds the derivation
or the record its job or step rests on. A value's own arithmetic stays beside the value.

## Why the race step is a job of its own

The workflow held two jobs, which ran at the same time: daemon-go-race ran the race step, and
daemon-go ran the plain step, the race-impossible step and the Stage 1 proof. The plain step's
package list is every package of the module less the race step's five (and the text checks), so
the two steps share no package and nothing orders them. Each now runs in legs
([below](#why-the-daemons-tests-run-in-legs)).

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
sets its time, beside the next three, each on a core of its own; plain leg 2 is the other 89
packages, about 880 s of package time over four cores, and the steps no leg splits (the
race-impossible tests, the forced-order tests and Stage 1, 57 s in that job). Race leg 1 is
internal/daemon alone; race leg 2 the other four, whose time is internal/launcher's (292 s in run
45601893438; internal/intake took 134 s there, its tests now beside each other). The longest leg
is then plain leg 1 or race leg 1, each about its setup plus its longest package, where the group
waited for daemon-go's 940 s.

The rules the legs keep:

- **Each package runs once, in one leg.** `.github/scripts/go_legs.py`, which each leg's go test
  step runs for its packages, compares the file with `go list ./...` first and fails the step,
  naming each, when a package is in no place, in two, or when a place names no package. It also
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
