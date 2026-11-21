# Notes for the envoy-go job's workflow

The derivations and the history of `.github/workflows/ci-job-envoy-go.yaml`, which points into
this file. Each `# why:` line of that workflow names the section below that holds the derivation
or the record its service or its step rests on, and the sections stand in the order their comment
blocks stood in the workflow file. A comment block of at most two lines stays where it is, beside
the value or the step it is about. So does a value's own arithmetic, the limits' table of the
job's timeout-minutes among it, and so does a comment inside a `run:` body, which is shell and
stays with its command: .github/scripts/ci_job_reads.py holds a step's `run:` text, the unit
step's comments and all, to the copy it keeps, and check-ci-filters.sh fails when the two differ.
In the text moved here, "this job" is the envoy-go job, "this file" is the workflow,
`.github/workflows/ci-job-envoy-go.yaml`, and "above" and "below" stand for that file's order:
the service or the step a section is about stands just below its `# why:` line there.

## The five settings on the postgres service's command line

The settings these tests need, as the server's own command line, so it starts with them
and no step has to restart it (jobs.<job_id>.services.<service_id>.command overrides the
image's CMD, and the runner passes it as the arguments after the image name in its
`docker create`). The read-back step below holds the server to every one of them.

This job's Postgres is a container that dies with it, so nothing it holds has to
survive a crash: fsync, full_page_writes and synchronous_commit are off, which is what
makes a forced checkpoint cheap. It is the top wait of the unit step otherwise - a
sample of pg_stat_activity beside it found IPC/CheckpointDone and IPC/CheckpointStart
outweighing every other wait event of an ACTIVE backend together by five times (run
42035294257), because `create database … strategy file_copy` forces two and a drop forces
one. The harness changes no server setting, so a developer's own Postgres keeps its
durability (dispatch://INFRA-1075).

The tests of this job run at the same time, each with a pool of its own, and the image's
default max_connections is 100, which several package binaries at once exhaust (`FATAL:
sorry, too many clients already`). 500 is five times that default, and no bound on what
the job can reach: eight package binaries (`go test -p 8`), each running up to twelve
tests at once (`-parallel 12`), each test store holding up to 16 connections
(storetest's testPoolSize), could ask for 1,536 at once, and 512 even at four tests a
binary, the runner's cores.

max_prepared_transactions is raised with it, from the image's default of 0, though no
test of this job prepares a transaction. The one test that does is the event log's floor
test, TestNoPreparedTransactionHoldsAnEventIDAndTheFloorTrailsOneAnyway, which only
envoy-go-race-api runs: it prepares a transaction holding the lock an insert into events
takes, which is the one writer pg_locks shows with no backend pid, and in CI it fails on
a server that can prepare none. This job carries the setting because every Postgres
service of this file starts on the same five settings, and
.github/scripts/postgres-service-settings.sh holds each of them to all five
(dispatch://INFRA-1075).

## What the unit step runs, and why its nine calls run at once

The step below runs the module's packages in the background and, beside them, eight other
calls: the plain run of what the race build cannot make, internal/pgmigrate, the routes
package, internal/dispatch/events, the shutdown feed test, the container-backed NATS
matrix, the integration suite and the agentloop vet. The package list runs 104 test
binaries, beside 37 packages with no test files (both runs of the pair at 8cfd2682, read
from the call's own -json). In the pair at bdffd7d2 (runs 43687973145 and 43691345741,
jobs 132732382594 and 132742525106) it took 519.5 s and 594.1 s from the first event of
its stream to the last, and every other call of this step had ended before it did. All nine
calls are started in the background at the step's head, the long ones longest first, so the
step costs the longest of them rather than their sum. The short calls follow the long ones,
and their order among themselves decides nothing: in run 2 the last of them ended at
07:50:49 while the package list ran until 07:58:31.
The race-impossible call runs here since the measurement dispatch://INFRA-985 asked for
was made; the comment on that call says what was measured and what it came back with,
and why the tests of it whose assertion is a wall-clock time run alone, in envoy-go-race
and envoy-go-checks, rather than here.
The shutdown feed test, internal/pgmigrate and internal/dispatch/events are each out of
the package list on a Postgres service of its own, for the reasons their own calls and
services give.

Each call but the vet writes a `go test -json` stream to a file of its own under
$RUNNER_TEMP/go-test-json - the natslive call's three go test commands write one between
them - which the upload step below publishes as one artifact per run attempt.
.github/scripts/go-test-times.py ranks the tests and packages of such a stream, and
compares two of them, which is how this job's time is measured (dispatch://INFRA-1075).
The stream is a file rather than the log because `go test -json` is verbose: the step
replays each call's package result lines to the log, and that call's whole stream only
when it failed, so a green log reads as it did and a red one carries every failing test's
output. -count=1 keeps the restored build cache from replaying a stored result as
`(cached)`. What a background call prints outside its stream cannot be left to interleave
with the others', so each writes that to a log of its own under $RUNNER_TEMP/go-test-logs,
replayed with its stream; the package list, which prints as it goes, writes no log of
its own. The step waits for all nine, replays one group per call, and exits with the
first non-zero status of the nine, each failing call named in an ::error:: of its own.

The API package used to run first and on its own in this job, because the per-test
database clone and drop of every other package queued on the service's one checkpointer
beside it; it runs in envoy-go-race-api now, and its xid window tests, the plain run the
race build cannot make of it, run alone in envoy-go-checks. The server above writes
nothing durable either way, so a clone's two forced checkpoints and its drop's one cost
little and that queue sets no step's time here any more.
-p 8 rather than go test's default of GOMAXPROCS, and -parallel 12 rather than its default
of GOMAXPROCS as well, because these tests wait far more than they compute: a profile of
the API package spends 2.19 per cent of its wall time on this process's CPU, and the
document service's spends 583 s of CPU over 1,867 s of wall. Four lanes on a four-core
runner would be four waits at a time, not four workers. The two numbers are bounded by
what the runner's own Postgres can serve beside them, which is why max_connections is
raised above, and by the race lanes' memory, which is why those jobs set their own
(envoy-go-race-docs). Twelve is measured rather than chosen: at twenty-four the step went
749 s to 1,105 s and ten tests of the document service and the outbox failed on their own
deadlines, the runner's four cores being the bound once the waits overlap (run
42035294257, dispatch://INFRA-1075). Both were measured while the API and documents
packages were still in this call; the race lane runs them now, so the call they bound is
a lighter one.

internal/dispatch/routes is NOT in that call: it is the routes call of this step, against
the second Postgres service, for the reason that service's own comment gives. It was in
this call for one revision of this change, and CI answered the question that put it here -
whether a disposable server's clones hold the safe horizon back long enough to matter -
by failing TestAnEventsSubscriptionFromTheWhoamiHeadReplaysWhatCommittedSince on its
events_start twice in three runs (42378432951 and 42424201523, passing in between). The
settings this service carries make a clone cheap; they do not make it free of a
transaction id, and the horizon waits on the oldest one open anywhere on the instance,
whatever it is doing (dispatch://INFRA-1075).

Everything the three race jobs run is left out of the package list, since they run it
under the detector, and so is internal/integration, which the integration call tests, and
internal/textcheck, whose tests are the text-check job's: that job runs them in a job
with no database and no Bun, and a change to only what they read must not start this
one. So are internal/dispatch/architecture/notes, internal/runnerlog,
internal/dispatch/specgraph, cmd/architecture-check and cmd/spec-check, which
envoy-go-checks tests on every push (NOTE_AND_SPEC_PACKAGES), and listenerwire and
internal/wirecheck, which envoy-go-checks tests in their place (LISTENERWIRE_PACKAGES):
listenerwire's clients test type-checks daemon packages, so a change to only those daemon
packages starts envoy-go-checks rather than this job. The pattern is built in
.github/scripts/ci_job_reads.py from the same lists those jobs' own steps are built from,
so what the list leaves out and what the other jobs run cannot drift apart. The
broker's packages, internal/cipher and everything under it, and its command, cmd/cipherd,
are left out too: the database-skips step below runs them with -json and fails on a failed
test as on a skipped one, so this step ran each of them a second time
(DATABASE_SKIPS_PKGS in ci_job_reads.py); that step in turn leaves out the race lane's
packages under the tree, the auth service's natscallout today (DATABASE_SKIPS_PATTERN).
What is left is the rest of the module less the routes package, internal/dispatch/events
and internal/pgmigrate, each of which has a call and a service of its own, and less the two
longest packages it held (the API's and the documents service's), which the lane runs
now. Its own -timeout is below, with the call. Its one -skip names seven tests: the
shutdown feed test, which the shutdown call runs on a service of its own, the two tests
of the web app's display-tree fixture, and the backend count's four tests of
cmd/cipher. Without `--check` TestWebDisplayFixture writes the committed fixture
rather than checking it, so a run here would ask nothing of it, and
TestTheWebFixtureFindsItsFilesFromAnyDirectory stats that same file, which would make it a
read of this job; envoy-go-checks runs both, the first with the check, and both texts are
built from the same list. So are the backend count's four tests of cmd/cipher,
which the count step below runs in this same job and env (BACKEND_COUNT_TESTS); the
package list is the one call of this step that runs cmd/cipher.
The long poles lead the list. `go test` starts the packages in the order its arguments
name, so a package that starts late ends the call late however little it is waiting for:
measured in run 42469495554, pmdoc started about 95 s into the step and ended it 91 s
after any other package finished, while every other long package was done 90 s earlier.
Leading with the five longest overlaps the longest one with the rest, which is what
longest-first scheduling buys. The five are internal/dispatch/pmdoc, cmd/dispatch,
cmd/cipher, cmd/dispatch-seed and internal/dispatch/outbox, which are the five
longest in both runs of the pair at cf069e0d (runs 43733795306 and 43737129665: pmdoc
525.2 s and 487.2 s, cmd/dispatch 521.0 s and 495.6 s, cmd/cipher, then cmd/agent-secrets,
411.2 s and 398.4 s, cmd/dispatch-seed 396.6 s and 388.1 s, outbox 391.1 s and 386.4 s),
ahead of cmd/cipherd, then cmd/broker, at 215.7 s and 211.3 s. Their order among themselves
is not a ranking: the two runs rank the first two the other way round, so what the list
leads with is the five, read back from the call's own -json (dispatch://INFRA-1075).
