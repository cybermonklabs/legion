# Testing

Rules for this repository's tests that the code under test does not show.

This file uses semantic line breaks: one sentence per line, and a long sentence may break after a clause.
A blank line separates list items.
Add a new item next to the item it belongs with, never at the end of a list unless it belongs there, so two pull requests that add items or change different sentences do not conflict.
`bun scripts/semantic-line-breaks.ts docs/testing.md` checks the layout, and `--write` fixes it.

## The design of this repository's tests

A test is deterministic only when the test controls every input that decides its result.
Four such inputs can come from the machine and not from the test:
the wall clock, the progress of background work, a server or a database that another test binary also uses, and the process environment.
A test that takes one of them from the machine measures the machine's load, and a loaded machine fails it.
An issue pod gives six agent roles 2 CPUs, and CI runs some Go jobs under the race detector, so such a test fails there, and its failure reads as a defect of the change under test.
Gerard Meszaros, *xUnit Test Patterns*, names this smell "Erratic Test", with the causes "Interacting Tests" and "Test Run War".
Of the 161 flaky tests whose cause Luo, Hariri, Eloussi and Marinov classified ("An Empirical Analysis of Flaky Tests", FSE 2014), asynchronous waits caused 74 (45%), concurrency 32 (20%) and test order dependency 19 (12%).

So each test owns what decides its result, by the six rules below.
Each rule gives its reason, its standard source and the helpers an author takes for it.
The sections after this one give the detail of each helper.
dispatch://INFRA-3149 records the failures that these rules remove.

### R1: a test owns every server and database that it uses

A fixture gives a test only a server or a database that the fixture starts or clones for that test, or for that test binary alone.
No fixture gives a test the address of state that another test binary can use.

The reason: two test binaries on one server delete each other's streams and see each other's clients as leaks.
In an issue pod, with a second binary on the daemon's former shared NATS server, 9 of 30 runs of `TestCapturedArtifactVersionAndApprovalFlowThroughConsume` failed, and with one binary at a time none of 30 failed (dispatch://INFRA-3273).

The source: Meszaros gives the "Fresh Fixture" in place of the "Shared Fixture".
Martin Fowler, "Eradicating Non-Determinism in Tests" (https://martinfowler.com/articles/nonDeterminism.html), gives each test its own state.

The helpers:

- NATS in `packages/daemon`: `internal/testnats`'s `URL` or `JetStream`, which start a nats-server with JetStream in the test process for each test ("A Go test that opens a NATS client" below).

- NATS in `packages/envoy`: `internal/testnats`'s `URL`, which gives each test a JetStream account of its own on a server of the test binary's own.

- PostgreSQL in `packages/daemon`: `internal/testpg`'s `Clone`, `ClonePool` and `CloneStore`, which copy a migrated template, and `internal/testpg/testdb`'s `Database`, an empty database ("A database a Go test creates" below).
  A package whose daemons migrate the database they are given takes one database for its run:
  `testdb.RunDatabaseMain` in its `TestMain`, and `testdb.RunDatabase` in each test.

- PostgreSQL in `packages/envoy`: Dispatch's `internal/dispatch/store/storetest` (`Open`), the broker's `internal/broker/store/storetest`, and `internal/testpg/testdb` (`Server.Database`, and `Template`, which the two `storetest` packages clone).

- Only those fixtures read a database URL from the environment (`LEGION_TEST_PG_DSN`, `DISPATCH_TEST_DATABASE_URL` and `BROKER_TEST_DATABASE_URL`).
  They give a test a database that they made, and never the database that the URL names.
  A gate that must fail rather than skip without a server asks `testdb.Available`, which answers whether a server is there and not where.
  A test that starts a child process, such as `cmd/dispatch` or the broker, gives the child the URL of the test's own database.

- A shell proof that writes PostgreSQL takes a database of its own for its run.
  It takes one through `exec_run_database` in `scripts/e2e/lib/loopback-dsn.sh`, which hands the server the run's own database in that server's environment and prints it nowhere, or through a Go test binary whose fixture makes the run's database, as the driver of `scripts/e2e/lib/live-daemon-proof.sh` does through `testdb.RunDatabaseMain`.

One exception holds no shared state.
The `-omp` suites share one NATS server, which `LEGION_TEST_NATS_URL` names.
That server runs without JetStream, so no suite can leave a stream on it, and each relay subject carries its session's own id.
`skills/legion-worker/references/tools.md` starts it so in a pod, and CI's NATS services run so.

The opt-in measurement lane, `packages/envoy/internal/dispatch/benchtime`, reads `DISPATCH_BENCH_DATABASE_URL`.
That variable names a seeded database on purpose, and CI never sets it.

### R2: the shared wait holds no number of its own

A test waits for what it cannot make happen itself through the shared wait, and gives the wait no duration.
The wait has one bound, and the test command sets it (R5):

- In Go, the bound is the test binary's deadline less testwait's margin, and under `-timeout 0` the wait has no end.

- In Bun, the bound is the row's deadline, which the marks of the row-deadline preload set (`@legion/test-fixtures/row-deadline`).

- In a shell proof, the bound is the proof's guard less its margin.

The margin is the time a test's cleanups take after its wait gives up and before Go's own deadline, Bun's timeout or the proof's guard ends the run.
It is 30 s in both Go modules (`margin` in `packages/daemon/internal/testwait` and in `packages/envoy/internal/testwait`), `max(1000 ms, guard / 20)` in Bun (`packages/test-fixtures/src/deadline.ts`), and 30 s in a shell proof (`proof_margin_seconds` in `scripts/e2e/lib/rig.sh`).
The Go margin is measured: at 61b66deb, in builds Jobs under 8 busy loops on 2 CPUs, three runs of each rig with the heaviest cleanups and a planted wait that never holds, from the wait's line to the test's `--- FAIL` line.
The daemon's in-process daemon took at most 0.394 s (`TestTheCutoverStartsTheMergersTheOldBuildLeftStopped`, Job margin-cutover-8) and its idle rig 0.587 s (`TestIdleReleaseAndWake`, margin-idle-8).
Envoy's `cmd/dispatch` two-process pair took at most 1.116 s (margin-mp-8), `internal/dispatch/docs`'s two processes 0.302 s (margin-docs-8) and a broker fleet of four pods 0.209 s (margin-broker-8).
Every reading is far below half of 30 s, so both modules keep 30 s; measure again before lowering it.

A fact that the product writes after the test's last action is waited for, and never read once.
A log line that the product writes is such a fact.
When a wait gives up, its failure names the fact it waited for and, where the condition can say it, the last state it read.
The pace of a poll is the helper's own, and no test sets it.
A stand-in for a production component keeps that component's own bound and names the setting it copies, as `clusterRoutedRuntime` in `packages/daemon/internal/daemon` keeps the timeout of the cluster's readiness probe.

To prove that something does not happen, take the first of these that reaches the code:

1. Read the state that would make it happen.

2. Under `testing/synctest`, where the code does no real network I/O, call `synctest.Wait`, then read.

3. Wait for the effect of a later action: send a marker after the thing, wait for the marker's effect, then read.

Only where none of the three reaches the code does a test hold a span.
It holds the span through one named function, and the span's `why` says which of the three does not reach the code, and why.

The reason: a number that a test chooses is too long to end a wait, or too short for a loaded machine.
The Go project removed a per-test timeout in favour of the binary's deadline for this reason (CL 504495):
"Any constant timeout we choose will either be too long to ever trigger, or not long enough to handle rare cases where the system is overloaded or just slow."
The cost is that a wait for a fact that never comes holds its binary to the deadline, where Go's panic prints the stack of every goroutine.
A condition that reads a report which makes its fact impossible, such as a process that exited, fails the test at once.

The source: Steve Freeman and Nat Pryce, *Growing Object-Oriented Software, Guided by Tests*, chapter 27, "Testing Asynchronous Code".
A test waits for an observable effect, by listening for an event or by sampling state, succeeds as soon as the effect appears, and times out only to detect failure.
The same chapter proves that an action had no effect by the effect of a later action ("Testing That an Action Has No Effect").
Fowler says never to use a bare sleep.

The helpers:

- Go, in each module: `internal/testwait`, whose package comment says the rest.
  `Eventually` waits until a condition holds.
  `Until` waits until a reading holds, and returns that reading.
  `Receive` waits for a receive from a channel.
  `Next` waits for the next matching item of a `Stream`, such as a line of a captured log, and reads the stream in order.
  `Context` gives a call that takes a context the same bound.
  `InMain` is the wait of a `TestMain`, which has no `t`.
  `Span` and `Quiet` are the named spans, and each refuses an empty `why`.

- Bun: `@legion/test-fixtures/wait`.
  `until` waits until a probe returns a truthy value, and `settled` waits until a promise settles.
  `span` and `quiet` are the named spans.
  `timeLeft` gives the milliseconds left before the deadline, at most the longest delay that a timer takes.
  Each function refuses an argument past its own, so a planted duration fails.

- A shell proof: `wait_for` in `scripts/e2e/lib/rig.sh`, which gives up before the guard that `proof_guard` set.

### R3: the code under test tells a test what happened

The code reports the facts that its tests wait for, so a test has an event to wait on and no reason to poll.
The daemon reports through one typed observer, `overrides.observe` (`packages/daemon/internal/daemon/observe.go`).
It is nil in production, and the daemon calls it with no lock held, so a slow test cannot hold the daemon.
It reports these:

- `observedReady`: the daemon is ready to serve.

- `observedAdmissionPass`: an admission pass committed.

- `observedPass`: a pass of a periodic loop ended.

- `observedOutboxSettled`: the run of an outbox row settled.

- `observedStranded`: the boot's report of the claims that nothing will wake finished.

A test of `internal/daemon` hooks an observer (`observing`, in `observer_test.go`) before the work it waits for begins.
It then waits on that observer's stream (`ready`, `pass`, `admissionPasses`, `settled`, `stranded`).
It does not poll `/healthz`, a row count of the outbox or a log to learn that one of these happened.
A test whose subject is the `/healthz` answer itself still reads it.
Envoy's delivery hub reports what it did through its own `observe` (`deliveryHubNote`, `packages/envoy/internal/dispatch/api/delivery_live_hub.go`).

The reason: code that does not say when its work is done gives its test two choices.
The test waits a number of seconds, or it reads once and can read before the product wrote the fact.
Both fail under load.

The source: Freeman and Pryce, chapter 27: a test waits for an observable effect, by listening for an event or by sampling state.
A report is the event.

### R4: time in the test process belongs to the test

No test passes or fails because it read a clock.
So no verdict reads any of these:

- elapsed time

- processor time

- a ratio of two measured times

- a count of events that a real clock paced

- a moment taken from the real clock before the test runs and judged later against another clock

A bound on work is a count of that work.
A timing promise across a process boundary is checked by the order of the events it promises, or by the program's own record of when it acted.
A service level that the product promises is measured in the opt-in measurement lane, `packages/envoy/internal/dispatch/benchtime`, whose `Timing` skips a test unless `DISPATCH_BENCH_TIMING` is set.
A person runs that lane on a quiet machine, and CI never runs it.

A product timer whose behaviour a test asserts takes the module's one clock, `internal/clock`'s `Clock`, through a field that defaults to `clock.Real()`.
The test gives it a `clocktest.Fake`: `New`, or `Held` for times that a database compares with its own `now()`.
The test moves the fake with `Advance` or `Set`, and moves it past a deadline only once the code has armed that deadline (`Armed`, `AwaitArmed`).
A package that needs only some of the clock's methods declares a narrowing of `Clock`, and keeps no clock of its own.
Code with no real network I/O runs its timing tests under `testing/synctest`, as the tests of `packages/envoy/internal/dispatch/duty`'s `WokenLoop` do.
A Bun test drives Bun's fake timers (`jest.useFakeTimers()`) where the code runs in the test's own process.

A test that needs a periodic pass of code in its own process triggers that pass, or advances the clock that schedules it.
It never shortens the interval of the pass.
In `packages/daemon/internal/daemon`, `overrides.passes` holds one trigger for each periodic loop (`passTriggers`, in `passes.go`).
A test hooks them with `triggering` (`passes_test.go`), and its `pass` runs one pass of a loop and waits for that pass's `observedPass`.
A child process that a test starts keeps its own clock, so its intervals may be shortened through its settings, as the `DISPATCH_TEST_*` settings of `cmd/dispatch` and the `ENVOY_*` settings of Oh My Pi do.
The test of a child waits for the facts that the child reports, and no verdict depends on the interval.
A time that a server records itself, such as the moment JetStream stores a message, is the server's clock, and no clock of the test or of the code reaches it.
A row whose input is a message older than a window lets real time age the message before anything is delivered, says so beside the sleep, and reads no verdict from that time, as `soak` in `startEnvelopeDeliveryThrough` does (`packages/envoy/cmd/listener/unplaceable_delivery_acceptance_test.go`).
Such a sleep is no wait and no span: a loaded machine only makes the message older.

The reason: a verdict on measured time measures the machine.
A budget of 1 s of processor time in `internal/dispatch/pmdoc` read 1.07 s and 1.23 s beside other work, and a `pi-envoy` row read 2,210.99 ms against a bound of 2,000 ms.

The sources:

- Fowler says to wrap the system clock.

- Freeman and Pryce, chapter 27, make timers and schedulers external, so that a test fires them ("Externalize Event Sources").

- `java.time.Clock`: "Best practice for applications is to pass a Clock into any method that requires the current instant and time-zone".
  The same design is in .NET's `TimeProvider` and in `k8s.io/utils/clock`.

- Go's `testing/synctest` (https://go.dev/blog/synctest) gives a test virtual time, and a `Wait` that returns when every goroutine of the test is durably blocked.
  A goroutine blocked on network I/O is not durably blocked, so a test over a real PostgreSQL or NATS drives the fake clock instead.

- Bun fakes `setTimeout`, `Date` and `performance.now` on the globals of the test process (https://bun.com/docs/test/dates-times), so a child process keeps its own real clock.

- *Software Engineering at Google*, chapter 11: the test runner enforces what a test of each size may do, and a small test may not sleep.
  Chapter 14 advises tunable and lower delays in a larger test.
  This repository lowers a delay only in a child process, whose clock the test cannot reach.

### R5: each test command has one hang guard, and the guard is never a verdict

Each test command sets the guard of its runner in one place:

- a package's `test` script (`bun test --timeout`)

- a Go command's `-timeout`, in `.github/workflows/ci.yaml` or in this file

- a shell proof's `proof_guard` (`scripts/e2e/lib/rig.sh`)

- a CI job's `timeout-minutes`

Set each guard to at least twice the longest correct run that it bounds, read on the machines that run it.
Write that reading beside the number.
No helper sets a timeout of its own.
A row that needs more than its package's guard keeps its own guard in one place, with the reading that shows it.
Where the sum of a CI job passes the merge queue's check timeout of 180 minutes, split the job or cut the cost of the suite.

A role or a person that runs part of a suite passes the flags that the suite's own command passes.
A bare runner command takes the default guard of that runner: ten minutes for each binary of `go test`, and 5,000 ms for each row and hook of `bun test`.

The reason: the guard stops a hang and prints what was running, and Go's guard prints the stack of every goroutine.
A guard below a correct run fails a correct change.
The `beforeAll` hooks of `packages/pi-legion/scripts/skill-scenarios/score.test.ts` ran for 5,013.68 ms and 5,030.91 ms against Bun's default of 5,000 ms beside other work, and the file passed alone in 5.4 s to 5.7 s.

The source: *Software Engineering at Google*, chapter 11: the test runner enforces what a test of each size may do.
Go's CL 504495 leaves a hang to the binary's deadline.

### R6: a test builds the environment of each process that it starts

A fixture gives each process that it starts the environment that the fixture builds, and not the environment of the machine.
A fixture ends each process of its rig that can act on the fixture's own children before the fixture runs them.

The helpers:

- In `packages/daemon`, a package whose tests run git or jj calls `fixtureenv.MustGuard` in its `TestMain`, and a fixture command runs under `fixtureenv.Isolated(home)` ("A test that starts jj or git inherits no pod's environment" below).

- A Bun suite that runs jj or git takes `versionControlEnvironment` from `@legion/test-fixtures/version-control-environment`.

- The launcher's bundle fixture (`packages/daemon/internal/launcher/workspace_bundle_test.go`) gives each child `fixtureenv.Isolated(home)` and a stand-in leak check (`leakguardtest.Write(…, leakguardtest.Passes)`).
  Its `restoreInto` ends the rig's launcher before the fixture's own jj and git run, and `launcher.Run` stops its reaper and waits for it before it returns.

The reason: a Legion pod puts a git wrapper first on `PATH`, and that wrapper's pre-push hook needs a leak check that the pod's `PATH` can lack.
When the test binary is PID 1 of its own PID namespace, the launcher runs a reaper that collects every exited child of the process.
In that shape, 9 of 12 runs of the launcher's bundle cases failed, 8 of them with `waitid: no child processes` (dispatch://INFRA-3273).

The source: Meszaros's "Fresh Fixture", and *Software Engineering at Google*, chapter 14, on the hermeticity of a larger test.

## The fixtures a test takes rather than copies

Each of these is one body per Go module, and a test that needs what it does calls it rather than writing the body again.
Each bullet says what the helper does not do, so a site that must keep its own body is not folded into it.

- A test that captures a log takes its buffer from the module's own:
  `packages/daemon/internal/testlog`'s `Buffer`, and `packages/envoy/internal/logtest`'s, which lives beside that package's default-logger swap and carries `Lines`, the logged lines holding a substring.
  `packages/envoy/internal/cmdtest`'s `LockedBuffer` and `storetest`'s `LockedLog` are both aliases of it, so either name reaches the same body.
  It is a mutex around a byte sink and nothing else:
  a type that needs a reader of its own embeds it and keeps that reader, and a writer that is no log sink
  — an `http.ResponseWriter`, a TCP relay, a writer that gates what it passes on — keeps its own body.

- The shared wait is the module's `internal/testwait` (R2 above, and the package comment).
  A loop that is not a wait for a fact keeps its body:
  one that sends or serves on a tick, one that drives a stand-in process's own main loop, and one that retries a compare-and-set.

- A fixture that runs git or jj takes its runner and its `init` argv from `packages/daemon/internal/fixturegit`.
  `Runner` holds the test and the child's whole environment
  — `Isolated(t, home)` is the usual one — and runs one command four ways:
  `Run` trimmed, `Raw` untrimmed, `Out` for standard output alone, and `Try` for a caller that keeps the error.
  `InitBare`, `InitWorkTree` and `JJInit` build the whole argv, and the semantic axes stay the call site's:
  bare or not, the initial branch, and colocation, with only the quiet spelling normalised.
  The runner chooses no binary — `argv[0]` does, so a fixture that runs a pinned git or jj passes that path itself.
  `packages/envoy` has no Go git or jj fixture runner, so this one is the daemon module's alone.

- A fake GitHub server is the module's `internal/githubtest`:
  `Server` for a fake its caller reaches by URL, `Serve` where the caller needs the server itself, and `Unexpected` as the default arm of a fake for which an unplanned request is a failure.
  `Spec` carries the opt-in bearer gate and the opt-in request recorder.
  The `switch`, its `default:` arm and the fake's own state stay the caller's, because they are what each test proves:
  a fake that reads the per-owner token the daemon minted must answer a request it then reads, and a `default:` that logs, that answers 400 or that is the happy path says something a shared arm would take away.
  A server that is no fake GitHub keeps its body, whatever it mentions:
  the daemon's own API, the issue-sandbox API, a fake Dispatch, a fake secrets broker, a reverse proxy in front of another fake, and a real `git http-backend` CGI behind a TLS proxy.
  A fake a test builds inline, in its own body rather than in a function of its own, is the same skeleton and takes the helper the same way;
  it is no shared copy, though, so nothing counts it, and most of the two modules' inline fakes still write it out.

- An in-test `go build` in `packages/envoy` goes through `internal/cmdtest`:
  `Build`, `BuildStamped`, or `BuildCommand` for a build that needs a directory, a target, flags or an environment of its own.
  One body holds `ENVOY_TEST_GO_OVERLAY`, the output under the test's own temporary directory and the failure;
  the overlay that variable names must be an absolute path, because a build whose `Dir` is another directory reads it from there.
  A `go build` that produces no binary, such as a compile check of a generated module, is not a build of this kind, and neither is one that takes no `testing.TB`:
  `infra/internal/pulumitest/pulumitest.go` builds the test binary's own plugin once per binary, under a `sync.Once` and removed by its `Main`, and returns an error rather than failing a test.

- A peer that is not there is the module's `internal/testport`: `Refused(t)` is a loopback address whose port the test holds bound, never listening, until it ends, so every dial to it is refused and no other program can bind the port, with or without `SO_REUSEADDR` or `SO_REUSEPORT`.
  It replaces the address of a server or listener the test closed: a freed port is any program's to take, and on a shared machine one took it and answered HTTP (dispatch://INFRA-3904).
  It holds no listening socket and hands nothing to a child: a process the test starts takes a socket the test holds (`cmdtest.HoldPort` and `Pass`, `DISPATCH_LISTEN_FD`, `ENVOY_LISTEN_FD`, `LEGION_LISTEN_FD`) or binds port 0 and names its port, never a port the test picked and let go of.

- A test that a command line keeps the rules of a cobra command line takes `packages/envoy/clitest`, one body for both modules: `packages/daemon` can import it through its `replace` of `packages/envoy`, and it imports the standard library alone.
  `Conformance` runs the built binary, of any framework, and checks help on every command; a typo, an unknown flag at the root and on each command, an unknown shell for `completion` and an unknown topic for `help` that each exit 2, print the word they refuse, are no Go runtime crash and make no contact with its `Recorder`; a flag read after a positional; the completion script of each shell; and `__complete`.
  The package documentation is the whole list and the rule each check holds.
  It does not check what a command does with its arguments, its other exit codes or the shape of its tree, which each conversion's own tests hold, and it builds no binary: the caller hands it one.


## Bun tests

`bun test` runs every file of a package in one process, so what one file leaves behind is what the next file reads:
a directory, an environment variable, a port, a container, a cache.
`@legion/test-fixtures` holds the shared pieces, and nine packages depend on it:
broker-client, claude-envoy, contracts, dispatch, envoy-client, pi-envoy, pi-legion, pi-shared and proof-editor.
The one Bun package that does not is the fixture package itself, whose tests import its sources by relative path.

A Bun test waits and is guarded as R2 and R5 above say:

- Wait for a fact through `@legion/test-fixtures/wait` (`until`, `settled`), and give the wait no duration.
  Each wait ends at its row's deadline: the newest mark of the row-deadline preload plus the command's `--timeout`, less a margin, so that the wait's own failure prints before Bun's timeout ends the row (`packages/test-fixtures/src/wait.ts`).
  A probe that throws ends the wait at once, so a probe that reads a report which makes its fact impossible throws.
  A wait under `jest.useFakeTimers()` refuses to start, because those timers hold `performance.now()` and every timer still until the test advances them.
  Outside `bun test`, a proof's Bun driver takes its deadline from `LEGION_PROOF_DEADLINE_MS`, which `proof_guard` exports, and with neither a wait refuses to start.

- Every `bun test` command loads the row-deadline preload, `--preload @legion/test-fixtures/row-deadline`.
  It marks the start of each row, the end of each row's body, the end of each row and the start of a run, so each wait of a row ends before the row's guard, and so does a wait in a `beforeAll` or in a row's first `afterEach`.
  A hook that no mark directly precedes counts from an earlier mark, so its waits end earlier than its own clock would allow, never later (`packages/test-fixtures/src/row-deadline.ts`).
  So a row's second `afterEach` counts from the mark before its first, and the time the first took is gone from it.

- A command sets one guard, its `--timeout`.
  A row or a hook sets no timeout of its own, neither a third argument to `test` or `beforeAll` nor a `setDefaultTimeout`, because the shared wait reads the command's guard and not a row's.
  The one exception R5 allows is a hook or a row whose work is one measured download or one measured process run with no shared wait in it, as the judge-omp `beforeAll` of `packages/pi-legion/scripts/skill-scenarios/rig.test.ts` pulls the judges' image and each of its rows that runs a judge holds that judge's `docker run`:
  such a hook or row takes a guard of its own, at least twice its slowest reading on a machine that runs the suite (CI's, or a dev box's), and the `"//"` note of its package's `package.json`, where the command's guards are written, gives that number with the readings and their runs.
  A file whose rows need a much larger guard than the rest runs in a second `bun test` command of the same script, with its own `--timeout` and the same preload, as `packages/pi-envoy`'s `-omp` files do.

These rules are how a file leaves nothing behind:

- Take a temporary directory from `@legion/test-fixtures/temporary-directories`, never from an `mkdtemp` with a cleanup of your own.
  One directory for a whole file or `describe` is `temporaryDirectory(prefix)`:
  call it at the test file's module scope, or in the body of a `describe`, never inside a test body.
  It reserves the path at the call and returns it, and registers the `beforeAll` that makes the directory and the `afterAll` that removes it where it is called.
  A directory per test is `temporaryDirectories(prefix)`, registered in those same two places, which returns a maker that makes a directory at each call and removes every one of them in its `afterEach`.
  Call that maker from a test body or a `beforeEach`, never from a `beforeAll`:
  it makes its directory at the call, and its `afterEach` removes it after the scope's first row, so one a `beforeAll` made is gone for every later row.
  A directory a whole file or `describe` shares is `temporaryDirectory`, which makes nothing until its hook runs.
  `bun:test` takes a hook's scope from the point of the call, and Bun 1.4.4 evaluates a module body and a `describe` body in three cases where it then runs no hook that body registered:
  a `bun test -t <pattern>` that selects no row of the file, one that selects no row of the `describe`, and `describe.skipIf(true)`.
  Nothing catches what such a run leaves: under `bun test` neither `process.once("exit")` nor `"beforeExit"` runs.
  So anything a file creates, sets or writes at module scope or in a `describe` body survives a filtered run, and a directory must be made where the hooks run.
  Write into a reserved directory from a test body, or from a hook registered below the reservation, since nothing is at the path until that `beforeAll` has run and Bun runs a scope's hooks in the order they were registered.
  A `describe` whose rows are each `.skip` runs both its `beforeAll` and its `afterAll`, so a directory reserved in its body is made and removed as usual;
  `describe.skipIf(true)` runs neither, so that directory is never made (`packages/test-fixtures/src/temporary-directories.test.ts` holds both, and `filtered-run.test.ts` runs a file under a filter that selects nothing and reads its `TMPDIR` afterwards).
  The fixture's own module body registers no hook, and cannot:
  Bun evaluates an imported module once per process, so a hook there would take the scope of whichever test file imported the fixture first, and every other file's directories would outlive their tests.
  `temporaryTmpdir(prefix)` (`@legion/test-fixtures/temporary-tmpdir`) is the same shape for a whole file's `TMPDIR`:
  it reserves one root, and makes it and points `TMPDIR` at it in a `beforeAll` whose `afterAll` puts the name back and removes the root.
  Anything staged under that root is staged in a hook too, since the name holds the caller's value until that `beforeAll` has run.

- `temporaryDirectory` and `temporaryDirectories` take `root`, the directory the temporary one is made in, for a test that needs another filesystem, such as `/dev/shm` for a rename that must cross one.
  A file whose one hook or row makes or removes hundreds of entries takes `root: memoryRoot()`, the same module's `/dev/shm` where it is a writable tmpfs that allows exec and `tmpdir()` otherwise:
  on the dev box's shared disk each entry waits for metadata and I/O that every session of the box shares, 0.2 ms to 6 ms, and `score.test.ts`'s 5,004 entries took 1,122 ms to 12,608 ms there and 49 ms to 65 ms on `/dev/shm` (dispatch://INFRA-4311).
  Both remove a tree a test left read-only:
  the removal chmods `u+rwX` down the tree first, following no symlink out of it and leaving a file's own mode alone, because `rmSync` alone fails on the 0555 directories and 0444 files a Go module cache writes, and because a file in such a tree can be a hard link to one outside it.

- Put a variable back with `restoreEnvironment` or `captureEnvironment` (`@legion/test-fixtures/environment`), which set and delete the names themselves.
  A name that held no value is captured as unset and the restore deletes it again;
  setting it to the empty string instead would leave the next reader a value its caller never had.
  A file that loads a plugin entry captures the names that entry writes above the import that loads it:
  `packages/pi-envoy/extensions/envoy.ts` puts its own `bin/` first on `PATH`, sets `DISPATCH_HOST` and picks `ENVOY_CONFIG_FILE` when its module loads, and a capture below that import would put back what the entry wrote rather than what the caller had.
  Never restore by replacing the whole object (`process.env = { ...saved }`):
  that swaps the runtime's environment for a plain one, and after it `process.env !== Bun.env`, a write reaches neither `Bun.env` nor a child process the test spawns, and the names the test deleted stay deleted in the real environment.
  Under `--parallel`, which implies `--isolate`, the next file gets a fresh `process` and never sees the swap, so the damage shows only where it is hardest to read.

- `mock.module` at module scope outlives the file that wrote it:
  a filtered run that selects no row of the file still applies it, Bun 1.4.4 has no API that takes a module mock off again, and every later file of the process reads the mocked module (`packages/pi-shared/test/host-registry.ts` says what that costs the suites that mock the host package).
  Prefer a seam the code under test exposes, set in a `beforeAll` and cleared in the matching `afterAll`, as `setNatsConnectForTests` and `resetNatsConnectForTests` in `packages/pi-envoy/extensions/envoy.ts` are.

- A suite that runs jj or git takes its isolation from `versionControlEnvironment` (`@legion/test-fixtures/version-control-environment`):
  a home of the suite's own with a jj configuration file in it, no git configuration at all (`GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`), no credential prompt, and the identity jj commits as.
  So neither the developer's `~/.jjconfig.toml`, nor their `~/.gitconfig`, nor the system gitconfig decides a result.
  A suite whose code under test starts jj with the process's own environment puts those names on `process.env` itself, and hands `versionControlNames` to `restoreEnvironment` so the next file finds its own again.

- A test that starts a process under a HOME or an `XDG_CACHE_HOME` of the test's own puts `...bunCacheOff` (`@legion/test-fixtures/bun-child-environment`) first in that process's environment;
  a test that moves its own process's HOME also runs `Object.assign(process.env, bunCacheOff)` and puts the key back with HOME;
  and a Go test puts `BUN_RUNTIME_TRANSPILER_CACHE_PATH=0` beside its `HOME=` or `XDG_CACHE_HOME=`.
  Bun keeps its runtime transpiler cache under HOME (`$HOME/.bun/install/cache/@t@`, or `$XDG_CACHE_HOME/bun/@t@`), and a HOME a test made is empty, so each Bun child writes each module larger than 4 KB it transpiles into the test's directory, and the test's removal deletes it again:
  149 files for one start of the dispatch entry, 3,083 in one run of `askgate-omp.test.ts`.
  No row checks that work, and stalling it on purpose reproduces the two-child AskGate row's time-out.
  Bun's own advice is to turn the cache off on a filesystem that does not last (https://bun.com/docs/runtime/environment-variables).
  The key goes to a process that is not Bun too (a shell, `python3`, `claude`), since a Bun it starts inherits it, and a HOME that does not exist (`/nonexistent-home-for-envoy-tests`) takes it as any other does.
  `bun scripts/bun-cache-census.ts -- <command>` runs a command under `strace` and names each Bun process it starts under a HOME or an `XDG_CACHE_HOME` of its own without `BUN_RUNTIME_TRANSPILER_CACHE_PATH=0`, and each cache file written outside the runner's own cache.
  It is a measuring tool, run by hand over one test file at a time (`bun scripts/bun-cache-census.ts -- bun test <file>`), and no CI job runs it or its test:
  CI's runner image has no `strace`, and a suite run under it slows until rows fail on time.

- Take a port from the kernel: bind port 0, read back the port the listener reports, and hold that listener from the moment it binds, so nothing else on the machine can take the port between the question and the row that asks it (`stubBroker` in `packages/envoy-client/src/__tests__/broker-credential.test.ts`).
  Never write a port number into a test.
  The Dispatch e2e harness is the same rule at the scale of a run:
  a port variable left unset is port 0, one that is set is used as given, and two variables naming one port are refused where the ports resolve rather than by whichever listener loses the race (`harnessPort` and `harnessPorts` in `packages/dispatch/e2e/harness-ports.ts`).

- A suite that needs a program this host may not have skips with that program's name.
  Call `skipWithout("docker")` once, at the file's module scope, and hand the answer to every `describe.skipIf` and `test.skipIf` of the file:
  it prints one line naming the missing programs and the file whose tests did not run (`@legion/test-fixtures/host-tools`), and a call per test would print that line per test.
  A suite that needs a service does the same with the variable that names it, and makes a database of its own rather than reading the one it finds (`LEGION_TEST_PG_DSN`, `packages/dispatch/e2e/__tests__/psql-session.test.ts`).
  One that needs both builds the two halves into a single list and prints one line, rather than calling `skipWithout` beside a warning of its own, so a host that has neither reads one line naming both (`hostTool` with the variable, in that file and in `harness-stacks.test.ts`).

- Start a container with `docker run --rm` and register its removal the moment `docker run` returns, not once the whole start has succeeded:
  a start that throws half-way has already made the container (`startNats` in `scripts/e2e/lib/nats-stream.test.ts`).
  Remove it with `docker rm -f
  -v`, and give it the CPU and memory limits "Containers a test starts" below requires.

- Two caches stay where their tool puts them, under the user's cache directory, and this is the one rule here that holds only under a condition.
  Oh My Pi's native modules, about 116 MiB a pin, are hardlinked into each test's fresh HOME from one copy per binary at `$XDG_CACHE_HOME/legion/test-omp-natives`, or `$HOME/.cache/legion/test-omp-natives` where `XDG_CACHE_HOME` is unset (`nativesCacheRoot` in `packages/pi-shared/test/omp-natives.ts`);
  `nativesCache` in `packages/daemon/internal/testbin/natives.go` computes that same path with `os.UserCacheDir` and `OMPHome` hardlinks out of it, so moving the Bun side alone would split a cache that exists to be shared by both languages and either fills for the other.
  The Go build and module caches of the `go run` in `runDaemonPane` (`packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts`) stay for the same reason:
  CI's `pi-legion` job restores `~/.cache/go-build` and `~/go/pkg/mod`, keyed `go-daemon-<runner os>-go<version>`, before it runs that scenario (`.github/workflows/ci-job-pi-legion.yaml`).
  Those of `scripts/e2e/lib/held-port.test.ts` stay there too, although the `test` job that runs it restores no Go cache now that the daemon's Go suite is `daemon-go`'s.
  The condition is the rename: `fillNativesCache` in `packages/pi-shared/test/omp-natives.ts` renames a directory inside the cache root, and a directory rename under `$HOME` panics the gVisor sandbox of a Legion pod and kills every role in it.
  So the permission holds only while `XDG_CACHE_HOME` is honoured and the pod points it at its own volume, which Legion's runtime does for every role (`/legion/scratch/cache`).
  With `XDG_CACHE_HOME` unset in a pod, that cache is no longer a tool's cache but the pod's death.

## Unix sockets in Go tests

A unix socket path must be 107 bytes or shorter.
Linux's `sun_path` holds 108 bytes with the terminating zero, and Go refuses a longer path with `invalid argument` at `bind`, `listen` or `dial`.
A tmux server socket has the same limit (`Filename too long`).
`t.TempDir()` puts the test's name, cut to 64 bytes, in its path, under `GOTMPDIR` or else `TMPDIR`.
With a long test name and a socket under it, or under a directory made from it (an `XDG_RUNTIME_DIR`, `HOME` or state directory a test sets to `t.TempDir()`), a temporary directory of about 20 bytes is enough to pass the limit, as on a dev box whose `TMPDIR` is on a data disk.

- Take a test's socket directory from the module's `internal/testtmp`, the one helper every test that binds a socket calls (`packages/daemon/internal/testtmp`, and the same file in `packages/envoy`).
  `testtmp.Dir(t, longest)` is a fresh directory of the test's own, removed when the test ends, where `longest` is the longest path the test puts inside it, written relative to the directory (`"g9/.s.PGSQL.5432"`);
  `testtmp.Socket(t, name)` is the path of a socket named `name` in such a directory.
  `Dir` makes the directory under `TMPDIR` and measures what `longest` would come to there;
  only when that passes 107 bytes, or `TMPDIR` holds no directory at all, does it fall back to `/tmp`, and it says so with `t.Logf` when it does.
  Each module's `internal/testtmp` keeps a test that fails a new `os.TempDir()`, or a new `os.MkdirTemp` whose first argument is a string literal naming a directory, anywhere in that module's tests.
  It reads no other shape: a directory named by a variable or a constant, or an `os.CreateTemp`, is left to review.

- Two kinds of site keep a directory of their own on purpose.
  `packages/daemon/internal/shim/socketpath_test.go`'s `roleTempDir` builds a deliberately long one, as long as the `TMPDIR` a Legion role is told, so that its test proves sockets still bind where a role runs;
  a short directory would take the proof away.
  The once-per-binary directories a package's `TestMain` owns (the build directories of `internal/daemon`, `internal/stream` and `internal/runtime/tmux`, the jj configuration directory of `cmd/legion/main_test.go` and the home of `internal/workspace/workspace_test.go` in the daemon module;
  the sign-in secret directory of `internal/broker/config` and the certificate directory of `infra/seed/main_test.go` in the envoy module) bind no socket, so they need no byte budget, and no `*testing.T` outlives them, so they keep `os.MkdirTemp("", …)` and a removal in that `TestMain`.

- A test changes a process variable, `TMPDIR` above all, only through `t.Setenv`, and a test that must unset one calls the module's `fixtureenv.Unset`, which goes through `t.Setenv`, never a bare `os.Setenv` or `os.Unsetenv`.
  `t.Setenv` puts back what the process held when the test ends, and Go refuses it in a parallel test or under a parallel parent, while a parallel test runs only beside other parallel tests, so no test can read a value another test changed.
  A bare call has neither fence.

- Before you call a test that binds a socket done, run it with a temporary directory of about 64 bytes.
  A pass under `/tmp` does not show that the test works under a longer one.
  Set `GOTMPDIR` as well, since `t.TempDir()` reads it first:

  ```sh
  L=/tmp/a-deliberately-long-tmpdir-for-unix-socket-path-tests-0001 && mkdir -p "$L"
  TMPDIR="$L" GOTMPDIR="$L" go test ./<package>/... -count=1
  ```

## Go's package deadline

`go test` panics a package that runs longer than its `-timeout`, ten minutes by default (`panic: test timed out after 10m0s`), which reads as a failure of the change under test.
Each Go command of a job of `.github/workflows/ci.yaml`, in the `ci-job-<id>.yaml` file that holds that job's steps, passes a `-timeout` of at least twice its slowest package over the job's 20 newest green merge-group runs (for `daemon-go`'s two Go steps, the 15 of the `test` job, which carried them, since main's memory guard landed), and the step's comment there gives that reading (R5).
The guards and their readings, each read up to the date its item names or up to 2026-11-15 where it names none, were:

- `daemon-go`, "Test the Go daemon": 15 minutes, against `internal/store`'s 342.597 s; the race step: 30 minutes, against `internal/daemon`'s 833.358 s under `-race`; both over the 15 green merge-group runs of the `test` job, which carried both steps until #2945, from 2026-11-15T20:47Z, when main's memory guard (#4021) landed, to 2026-11-16T05:39Z.

- `envoy-go`: 20 minutes for the unit step, against `cmd/broker`'s 477.995 s; 10 minutes for each plain run of what the race build cannot run, against `internal/dispatch/docs`'s 49.044 s; 10 and 5 minutes for natslive's `internal/testnats` and `internal/bus` (3.538 s and 5.029 s); 300 s for the integration step (7.683 s).

- `envoy-go-race`: 30 minutes, against `internal/dispatch/store`'s 664.278 s.
  `envoy-go-race-api`: 60 minutes, against `internal/dispatch/api`'s 1508.156 s and its step's 1643 s, up to 2026-11-17T10:27Z.
  `envoy-go-race-docs`: 65 minutes, against `internal/dispatch/docs`'s 1881.151 s.
  `envoy-go-docs-finalizers`: 45 minutes, against `internal/dispatch/docs`'s 1261.698 s.

- `dispatch-memory`: 70 minutes, against 1963.339 s; `dispatch-multiprocess`: 25 minutes, against 718.638 s; `dispatch-two-builds`: 30 minutes, against 575.933 s over its 8 green merge-group runs; `broker-multiprocess`: 20 minutes, against 60.731 s; `listener-server-change`: 15 minutes, against 152.28 s.

- `envoy-infra-go`: 10 minutes for the unit step, against `gcp`'s 28.059 s, and 30 for each natslive call, against `core`'s 124.22 s on a restored cache, since a cold cache installs four tools first.
  `envoy-smoke`: 5 minutes for the smoke test (16.133 s) and 15 for the live tests (145.791 s).
  `envoy-go-checks` and `airlock-lean`: 10 minutes for each command, against at most 121.054 s, the two-minute fuzz.

- `text-checks` and `plugin-pack`, jobs since #2945, read over their own green merge-group runs from it to 2026-11-16T17:33Z (19 and 16 runs whose logs GitHub served): 2 minutes for each module's text checks, against `packages/envoy/infra`'s 31.588 s; 1 minute for the API's source guards (10.660 s) and for the supervise table's two pins (5.547 s); and 15 minutes for the plugin pack, against `internal/daemon`'s 63.227 s.

Each `bun test` step of a job of `ci.yaml` that runs no package script passes the row-deadline preload and a `--timeout` of at least twice the slowest row or hook its command ran under 8 busy loops on 2 CPUs in a builds Job at 61b66deb, and the step's comment there gives that reading (R5):

- `test`: claude-envoy's `tests/build-inputs.test.ts` 10,000 ms, against 2,992.78 ms; `scripts/third-party-notices.test.ts` 5,000 ms, against 396.72 ms; `scripts/semantic-line-breaks.test.ts` 5,000 ms, against 806.66 ms; `scripts/priorities-seed.test.ts` 5,000 ms, against 797.60 ms.
  `scripts/dispatch` 15,000 ms, against 5,190.57 ms; `scripts/approval-eval` 5,000 ms, against 196.24 ms.
  `scripts/semantic-line-breaks.guides.test.ts` 15,000 ms, against 7,111.27 ms, read in a builds Job at `c8a44f97`.

- `test`, `bun test scripts/e2e/lib`: 60,000 ms, against `held-port.test.ts`'s `beforeAll`, which builds the real envoy-listener, 21,991.12 ms on a warm Go cache, and the slowest row's 13,106.99 ms.
  Before dispatch://INFRA-3149 the step passed no `--timeout` and ran on Bun's default of 5,000 ms, which killed `scripts/e2e/lib/smoke-cleanup.test.ts`'s `smoke-main-clean` teardown row at 8.3 s in merge-group run 43708116445 (#4046's queue run, at `e21be234`).
  Over 60 `test` jobs on 2026-11-15, 08:14Z to 11:16Z, that file's 600 rows took 111 ms at the median, 241 ms at the 95th percentile and 8.3 s at the slowest, that row of run 43708116445.
  In a 2-CPU pod its slowest row took 4.5 s beside 8 busy loops and 7.4 s beside 16, and 20 of 20 runs of each passed under a 30 s limit while 5 of 20 failed at Bun's 5 s default (measured for dispatch://INFRA-3621).
  The guard stays 60,000 ms, which the hook's 21,991.12 ms sets.

- `conflicts-script`: `scripts/conflicts` 5,000 ms, against 1,986.02 ms.

- `sessions-script`: `scripts/sessions` 5,000 ms, against its `beforeAll`'s 2,197.48 ms.

- `envoy-session-stall`: envoy-client's `nats-stall.test.ts` 120,000 ms, against a row's 50,109.26 ms and its `beforeAll`'s 25,685.20 ms; `test/session-stall.ts` 180,000 ms, against 60,810.22 ms; pi-envoy's NATS path freeze 90,000 ms, against 30,499.69 ms; claude-envoy's channel stall 120,000 ms, against 53,001.47 ms.
  The two suites of the real CLI against a real Dispatch take 120,000 ms each, against their `beforeAll`s, which build and start that server: 52,389.38 ms for the design-gate advice and 45,890.03 ms for the moved-approval advice.

A command a person or an issue pod runs takes its reading from the machines that run it, which are slower than CI's.
`packages/envoy/internal/dispatch/api` took up to 1508.156 s under `-race` in `envoy-go-race-api`, and a plain run is faster (about 600 s on a workstation), so every command that runs that package, on its own or in a pattern that includes it (`./...`, `./internal/dispatch/...`), passes `-timeout 60m`:

```sh
cd packages/envoy
go test -timeout 60m ./internal/dispatch/api
```

A Legion issue pod does not run that whole package: `skills/legion-worker/references/tools.md` says what the pod runs and where the rest goes.

CI runs the whole package only under `-race`, alone, in `envoy-go-race-api` (`.github/workflows/ci-job-envoy-go-race-api.yaml`, which holds that job's steps);
`envoy-go` leaves it out of its unit step, and `text-checks` runs its fifteen source-guard tests by name.
A command that runs one of its tests by name (`-run`) needs no deadline of its own.
Run it alone rather than beside the module's other Postgres-backed packages:
together they share one database service and the machine's CPU, and on a small machine `api` and `docs` then each overran the default (`docs/solutions/testing/the-dispatch-servers-go-suites-in-an-issue-pod-run-api-and-docs-alone-with-a-timeout-and-read-the-c-collation-docker-and-bun-failures-as-the-environment-LEGION-752.md`).

`packages/daemon/internal/daemon` is the daemon's slow, Postgres-backed package.
CI's `daemon-go` job (`.github/workflows/ci-job-daemon-go.yaml`) runs it only under the race detector, in its race step, once per job;
the plain step ("Test the Go daemon") leaves it out, and `go vet -tags e2e ./...` in the `typecheck` job still compiles it without the detector.
Under `-race` it took up to 833.358 s over the race step's 15 green merge-group runs from 2026-11-15T20:47Z to 2026-11-16T05:39Z, while that step was the `test` job's, and 908 s on a shared devbox, so a command that runs the whole package, alone or in a pattern that includes it, passes `-timeout 35m`:

```sh
cd packages/daemon
go test -race -timeout 35m ./internal/daemon/
```

A Legion issue pod does not run that whole package either: `skills/legion-worker/references/tools.md` says what the pod runs and where the rest goes.

## A cleanup's closure or argument holds nothing that reaches its object

A `runtime.AddCleanup` or `runtime.SetFinalizer` closure captures, and its argument carries, nothing that reaches the object the cleanup is attached to.
`runtime.AddCleanup` says what that costs: "If ptr is reachable from cleanup or arg, ptr will never be collected and the cleanup will never run."
The runtime keeps a cleanup's closure and its argument alive for as long as the object it is attached to, so either of them reaching that object makes the object immortal, and everything the object holds with it.
In `packages/envoy/internal/dispatch/docs` the object is a room's live document, and two closures held the service and one of its listings, each of which reaches every resident document through ygo's room table:
every room the tests loaded then stayed in memory with its replica and its trees, which is what took the `envoy-go-race-docs` job over its runner's memory (dispatch://INFRA-1085).

Hold the owner through a `weak.Pointer` instead, and do nothing when its `Value()` is nil, as that package's `keepInstance` (`roomcursor.go`) and `listResident` (`tree.go`) do:
the cleanup drops its entry while the owner is alive, and an owner that is gone has no map left to drop it from.
Hold weakly what the argument would carry as well, and pass `struct{}{}`, which reaches nothing, as `keepInstance` does with the room load its cleanup once took as its argument.
A pointer to a field of the owner counts as the owner, since a field and the value holding it are one allocation.
`weak.Make` takes such an interior pointer and that weak pointer dies with the whole allocation, which is how `listResident` holds the listing it was given, `Service.instances` or `Service.replicas`.

Go's own detector finds a closure that reaches its object:

```sh
cd packages/envoy
GODEBUG=checkfinalizers=1 go test -count=1 -timeout 60m ./internal/dispatch/docs/
```

It prints `WARNING: LIKELY CLEANUP/FINALIZER ISSUES`, then one `is reachable from cleanup or cleanup argument` line per site naming that site's file and line, and ends the run with `fatal error: detected possible issues with cleanups and/or finalizers`.
The rule covers the cleanup's argument as well as its closure, so an argument that reaches the object is the same defect:
the room load `keepInstance` passed reached a document of its own, which is the finding this job made on a head where the closure itself was already weak, and the comment on that cleanup names the path it took (dispatch://INFRA-1085).
A run that reports nothing is no proof on its own: the check runs at a garbage collection, so a short run can pass with the path present.
`GOGC=10` beside the detector makes a single test likelier to fire it, not certain, so repeat the run before you read a quiet one as a clean one:
with this fix reversed, `TestALiveWritesAskBlockKeepsItsWriterWhenAnotherProcessSettlesIt` under `GOGC=10 GODEBUG=checkfinalizers=1` fired in 2 of 3 runs for the implementer and in 1 of 6 for the reviewer, and at the fixed head it was quiet in 9 of 9.
CI runs that command in `envoy-go-docs-finalizers` (`.github/workflows/ci-job-envoy-go-docs-finalizers.yaml`, which holds that job's steps), which the `envoy_go_race_docs` filter in `.github/workflows/ci.yaml` selects, with `-timeout 45m` against the job's slowest green run of 1261.698 s.
The command above gives 60 minutes because an issue pod's run of the package took 1690 s (recorded on dispatch://INFRA-3149).
It is one of the nine slow suites: a pull request runs it only with the label `full-ci`, and the merge queue runs it on the tree the change lands as, which is where its result normally comes from.

That job runs the command with `-v` for a second check Go's detector does not make:
the package's `TestMain` prints `docs: <built> services built, <alive> still alive after <duration>` once the tests have run, and the line after it names each test that built a service still alive at the end of the run.
A test that builds a `*Service` gives it a `t.Cleanup` that shuts it down.
A service a test left running holds its rooms, and their documents, for the rest of the binary's run.
A service that was shut down is still alive at the end of the run when a callback of one of its stopped timers holds it.
`Timer.Stop` does not free the callback: the runtime keeps a stopped timer in the heap of the P that armed it until that P next clears its stopped timers or the timer's own time comes, and an idle P clears them only when it next runs, which depends on the machine's load.
A settlement timer's callback therefore holds the service and the room's state weakly (`scheduleSettleAfterLocked` in `packages/envoy/internal/dispatch/docs/service.go`): held strongly, a stopped one kept its shut-down service for up to an hour under a test's `settle = time.Hour`, and the check failed on a loaded machine in nearly every run (dispatch://INFRA-3904).

`packages/envoy/internal/dispatch/docs` is the only package of this repository that registers a cleanup today, and no repository-wide check holds that true:
`grep -rn "runtime.AddCleanup\|runtime.SetFinalizer" --include=*.go .` finds those two sites and nothing else.
A `runtime.AddCleanup` or `runtime.SetFinalizer` added in another package comes with a detector job of its own, in the same pull request.

## A database a Go test creates

A Go test that creates a Postgres database writes `create database … strategy file_copy`, as the daemon's `internal/testpg` and Envoy's `internal/testpg/testdb` do, the latter for every clone Dispatch's and the broker's `storetest` give out.
Without it, PostgreSQL 15 and later copy the source database through the write-ahead log, every page of it:
4.4 MB of log for an empty database made from `template1`, and 6.4 MB for a clone of Dispatch's migrated template, which made one run of the `api` and `docs` packages write 16 GiB of log.
A file copy writes a few hundred bytes.
`internal/testpg/testdb`'s own test fails when a clone writes more than a hundredth of its template's size, and each module's `internal/textcheck` test `TestEveryCreatedDatabaseIsAFileCopy` fails on any line of the module's Go files that opens a database's creation in a string without `strategy file_copy` on the same line.

In Envoy, everything that makes a database on a server a variable or a URL names is in `internal/testpg/testdb` (`Server` and its `Database`, `DatabaseOn`, and `Template`, which both `storetest` packages clone), never in `internal/testpg`:
`testpg` links testcontainers, whose package init walks every process on the machine before any test runs, and on a box running some 26,000 processes that cost a test binary about 9.8 seconds of start (`GODEBUG=inittrace=1`).
`testpg.URL`, which may start a container, keeps that cost for the tests that need it.
The broker's `storetest` holds itself to it: `TestStoretestLinksNoContainerRuntime` fails when its dependencies reach testcontainers.

A test that needs the schema takes a clone of a template its package migrated once, rather than running the migrations itself:
`packages/daemon/internal/testpg`'s `Clone` answers the clone's URL, `ClonePool` an open `pgxpool` on it and `CloneStore` the opened store, each dropped when the test ends, and the package's `TestMain` is `testpg.Main("<prefix>", m.Run)`, which drops the template when the run ends and composes with the `TestMain` a package already has (`testpg.Main(prefix, func() int { return testnats.Main(m) })`).
`packages/envoy/internal/dispatch/store/storetest`'s `Open` is the same fixture for Dispatch's schema.
`testdb.Database` is still there for a test that wants an empty database:
`internal/record`'s own tests apply part of the migration set by hand and assert what `Migrate` then returns, which a migrated clone would not let them do.
`internal/store`'s own tests take that same `Database`, from the leaf package `packages/daemon/internal/testpg/testdb` that holds it:
`internal/testpg` imports `internal/store` to migrate the template its `Clone` copies, so a test binary of `internal/store` cannot import `internal/testpg` back, and the half of the fixture that needs no schema sits in `testdb`, which imports nothing of the module and so is open to every package of it.
A package whose daemons migrate the database they are given, rather than taking one each, takes one database for its whole run instead:
`testdb.RunDatabaseMain` from its `TestMain`, and `testdb.RunDatabase` in each test (`internal/daemon`).
A test of that package that needs rows no other test of the run can see takes a clone of its own (`testpg.Clone`).

A test that needs one of envoy's binaries takes it from `packages/daemon/internal/testbuild`'s `EnvoyBinary`, which builds each command once per test process and removes the directory when `testbuild.Main` ends the run.

In Envoy, every connect a test database's URL leads to waits for as long as the test binary's deadline allows, and no less:
`testdb.Server` sets the URL's `connect_timeout` to the whole seconds left until `t.Deadline()` (`testdb.WithConnectDeadline`), in place of pgx's two minutes, so a Postgres that answers a connect late on a loaded machine slows the test rather than failing it (dispatch://INFRA-4308).
A child process a test starts on such a database takes the same bound (`cmd/dispatch`'s `databaseURLOf`).
Envoy's one drop of a test database, `dropDatabase` in `internal/testpg/testdb`, asks again while Postgres answers SQLSTATE 55006, which a forced drop answers while a backend it terminated has not exited yet, until the binary's deadline.

A test changes no server setting (`fsync`, `full_page_writes`, `max_wal_size`):
the server `DISPATCH_TEST_DATABASE_URL` names can hold other databases, such as the local Dispatch's on `packages/envoy/scripts/dev-postgres.sh`'s container.

## A Go test that opens a NATS client

Each daemon Go test that takes NATS through `packages/daemon/internal/testnats` gets a nats-server with JetStream of its own, in the test process.
The test's first `URL` or `JetStream` call starts it, a later call in the same test gets the same server, and a subtest gets a server of its own.
The server is the nats-server the test binary links in (`github.com/nats-io/nats-server/v2` v2.15.1, which `packages/daemon/go.mod` requires and the hub runs), on loopback at a port it picks, with its store under the test's own temporary directory.
So the daemon's Go tests need no container runtime and no NATS server named in the environment, and no test reads `LEGION_TEST_NATS_URL`.
The server holds nothing of another test's, so a test creates the streams it needs with `testnats.CreateStream`.
The server stops when the test ends, and `testnats.Main`, which the package's `TestMain` returns, fails the run when a server outlives its test.
The server `URL` starts admits only a client that carries a token of the test's own.
The URL carries the token as its user (`nats://<token>@127.0.0.1:<port>`), so every client that dials the URL carries it: the fixture's, the daemon's, a child process's and a raw `nats.Connect`.
A client of another process can reach the server, because the kernel may give the server a port that the other process still dials.
The server refuses such a client at `CONNECT`, so it never holds the test's leak check open (dispatch://INFRA-3149).
`StartNkeyAuthorized` and `StartNkeyAuthorizedUsers` start in-process servers that admit only nkey users.
Two tests run a container, because their subject is a server the test binary does not link:
`TestDaemonPermissionsOnRealServers`, whose subject is the published NATS images (`testnats.StartImage`, in `image.go`), and `TestOpenConsumersRefusesAServerThatSkipsAFilter` (`packages/daemon/internal/intake`), whose subject is the daemon's refusal of nats:2.10, a server that can skip a message on a consumer filtering on a wildcard and a literal at the same token (`testnats.StartOpenImage` with `testnats.SkippingImage`).
Where no container runtime answers, those tests and the tests of the image helpers skip, and on GitHub Actions they fail.
Envoy's tests have a rig of their own, `packages/envoy/internal/testnats`, which gives each test a JetStream account on a server of the test binary's own.
The two are copies, because Go's internal rule keeps one module from importing the internal packages of the other.

A test that wants a client of its own takes it from `testnats.Connect(t, url)`, which names it after the test and closes it when the test ends.
A test that dials the server itself leaves a connection nobody named.

A test proves that a subscription of its own received nothing by reading what the subscription holds (`NextMsg(0)`), once two things hold.
First, the code that would publish has returned, and its publish reached the server before it returned: a JetStream publish waits for the stream's acknowledgement, and `bus.Client`'s core publish flushes (`publishConfirmed`).
Second, a round trip on the subscription's own connection follows, a flush or any request, and the server answers it only after it has written that subscription every message it took before.
A handler that a subscription runs may still be running when its effect shows, so the test first sends a marker behind the message through the same subscription and waits for the marker's effect (`keptBehind` in `packages/envoy/cmd/listener/confirmed_test.go`).
A publish made on a goroutine that nothing waits for, such as the listener's refusal exception (`publishRefusalException`), is ordered by none of this.

The end of the test's server is its leak check (`packages/daemon/internal/testnats/leakcheck.go`).
The server held no client when it started, so each client it holds at the end is one that the test, or the code under it, left open.
A client that `Connect` opened and that is still connected to the server was given another test, or a stand-in for a test whose cleanups do not run.
The check fails the test at once for it, naming it by the id the server gave it and the name it carries, and closes it.
The check waits through the shared wait for any other client (the daemon's own, a child process's, a raw `nats.Connect`) to go, because a server drops a socket on a clock of its own.
A client still open when that wait gives up fails the test, by its id and its name.

The daemon's own production clients are named too:
`natsauth.Connect` takes the name as a parameter rather than an option, so no call site can leave one empty, and an operator reading `/connz` on the Envoy NATS sees which process holds each connection.

## A test that reads `GET /api/v1/references?since=` holds the route's wait open

A since read answers only the edges at or below the event log's safe horizon (`getReferences` and `sinceHorizon` in `packages/envoy/internal/dispatch/api/references.go`).
It waits at most `referencesSinceWait`, one second on the server's clock, for a horizon above the events committed before the read.
Past that wait it answers at the horizon it has and leaves the rest to a later read: the route defers, and it never loses an edge.
The horizon is instance-wide, so any transaction that holds a transaction id on the Postgres instance holds it back.
In `packages/envoy/internal/dispatch/api` the holder can be the test process itself:
`storetest.Open`'s clone, a `create database … template … strategy file_copy`, and its `drop database … with (force)` (`Template` in `packages/envoy/internal/testpg/testdb/template.go`, which `packages/envoy/internal/dispatch/store/storetest/storetest.go` clones from).

So the test's server runs on a `clocktest.Held` fake that the test never moves, and the route's wait never ends on its own (R4).
The read then waits until the horizon passes every event committed before it, so what it lists is every edge it owes.
One read is final, and a wrong answer fails the test at once: a missing edge, a duplicate or a wrongly stamped one.
The helper is `awaitSinceRead` (`packages/envoy/internal/dispatch/api/references_test.go`), which takes the read as a function, so each call site keeps its own reader.
A test whose server runs on the real clock instead, such as the event log route's (`events_log_test.go`, which answers through the same horizon, `sinceHorizon`) and the two references rows that prove the real wait (`…OnAQuietServer`, `…WithholdsEveryMentionAboveAnOpenLowerEvent`), reads again for as long as the horizon says it is held back, through `awaitHorizonRead`, which waits as the next section's wait does (`horizontest.NewWatch(...).Poll`).

A test that took one read as final on the real clock failed under a transaction someone else held:
`TestAnAnsweredAsksPartEdgesAreListedSince` failed about one run in six on main, 7 of 40 measured for dispatch://INFRA-592, and in this repository's own pod on 2026-11-02 it failed 15 of 40 runs with a transaction held 2 s of every 2.5 s.

Prove such a test under a loop that holds the state, as `docs/solutions/testing/code-that-waits-on-instance-wide-database-state-is-tested-under-a-loop-that-holds-that-state-pr2392.md` prescribes:

```sh
while true; do
  psql "$LEGION_TEST_PG_DSN" -Atc 'select pg_current_xact_id(), pg_sleep(2)'
  sleep 0.5
done
```

The holder must release each transaction.
One that never commits holds the horizon back for good, so no read under it answers, and the test cannot pass.

## A test that waits for the event feed's first frame

The rule above holds the route's wait open through the server's clock.
A test of `cmd/dispatch` runs Dispatch as a child process, whose clock the test cannot reach, and it waits for the event feed's `events_start`.
That frame is the last thing a new subscription sends, not the first:
`feedEvents` writes it only once `events.Horizon.Await` has returned (`packages/envoy/internal/dispatch/api/live_events.go`, `event_stream_relay.go`), and the horizon's proof is instance-wide, so the frame is as late as the longest write transaction open anywhere on the Postgres instance.
No fixed deadline bounds that, and a deadline raised to fit the worst run anyone has seen is the same flake on the day someone holds a transaction for a minute.

So the test waits through the shared wait, and reads what the server says about itself.
`events.StallWarning` is the message `Horizon.warnStall` logs, every `warnEvery`, while a pending observation is held back, naming the holder's kind, pid, database and query.
The wait is `horizontest` (`packages/envoy/internal/dispatch/horizontest`), an ordinary package so that the three packages whose tests wait on the horizon share it: `cmd/dispatch`'s `waitEventsStart` (`live_events_wait_test.go`, which `openLiveEvents` calls) reads the warnings out of the Dispatch process's output (`horizontest.Lines`), and `internal/dispatch/api` and `internal/dispatch/routes`, whose servers run in the test's own process, read them as records (`horizontest.Listen`, `WaitInProcess`, `PollInProcess`), so `pending_for` and `database` are typed values there and are not parsed.
The event feed's frames are read on a goroutine that sets no read deadline, because a gorilla connection is corrupt after a read timeout.
The wait goes through `testwait` with no bound of its own, so a hold of any length only delays it, and only the test binary's deadline ends it.
When it ends there, its last read is the horizon's own account of itself (`Watch.String`): how many stall warnings the horizon logged while the test waited, none included, and the newest of them, which names the holder's kind, pid, database and query from the horizon's own diagnosis.
Count the warnings, never whether one was seen: a check that latches on the first warning reads every later hold as the first.
`Listen` hands every record on to the handler it replaced, and when that is slog's own default handler it writes as that handler does through a logger of its own: slog's handler writes through the log package, and a record handed to it while `slog.SetDefault` has the log package pointed back at the new handler deadlocks every goroutine that logs (`TestListenOverSlogsOwnDefaultHandlerPassesEveryRecordOn`).

The wait ends in a named failure, and at no moment of its own.
It fails at once when a warning names the test's own database for a holder that has held the horizon back at least `horizontest.OwnHoldBound`:
a client of Dispatch's own database that holds its readers back that long is the defect, not a stall to wait out.
That failure also names the client's `application_name` and pid out of the warning, because a transaction in that database can be anyone's (an operator's `psql`, another test's fixture) and not only Dispatch's own.
Otherwise the wait fails only where every shared wait does, and prints the horizon's account.
The rules this wait had before, of 3 s, "no warning yet" and "no new warning", failed merge-group run 42731347664:
a foreign transaction held the horizon while load delayed the first warning past them.

`horizontest.OwnHoldBound` (5 s) is three times a measurement of its own:
sampling `pg_stat_activity` every 100 ms over 10 runs of that test under the load below, in this repository's pod on 2026-11-10, `nproc` 2, PostgreSQL 18.6, 202 samples caught a transaction holding a transaction id in a `dispatch_test_*` database, across 27 backends, all of them carrying no `application_name` — which is what Dispatch's own pools look like and what the test's own fixtures look like too, so the samples do not say whose the transactions were — and the worst age was 1.449 s.
The bound is read off the warning itself, so the wait reads no clock of its own (R4): the shorter of the warning's `pending_for`, the age of the oldest pending observation, and the age of the holder it names, its `xact_start` against the warning's own time.
`pending_for` alone is not the holder's: a foreign holder released since can have carried the oldest observation past the bound while the holder now named is a transaction of the test's own database that began a moment ago.
A hold shorter than the bound extends the wait as a foreign hold does.
Short thresholds are why the Horizon runs one diagnosis at a time:
at a `warnEvery` below that diagnosis bound, several scans at once would queue for the two connections of the horizon's own pool, ahead of the observation the waiting reader needs.

Prove such a test with a transaction that is still open when the socket subscribes, not only when `go test` starts.
The test builds its own binary first (`cmdtest.Build`, from `startDispatchProcessWith`), which takes a second on a warm build cache and minutes on a loaded machine, so a one-shot hold started before the command can end before the socket exists.
A rolling hold, a new 25 s transaction every second, does not depend on that, and is the recipe to use.
Measured in this repository's own pod on 2026-11-10, `nproc` 2, under 8 busy loops at four times `nproc`:
under a rolling hold, a former 10 s deadline of this wait failed all five runs it was measured over, and a wait on the server's warnings passed 50 of 50.

## A document test that waits for a room's appends

`settleCurrentGeneration` (`packages/envoy/internal/dispatch/docs/service_test.go`) waits for a room's live updates to reach its persistence and land, and then settles the room.
That wait is a queue.
A room's appends go through the service's room-appends pool of 8 connections, and a test whose subtests run in parallel drives one room per CPU on a single service.
So the wait is as long as that queue, which grows with the machine's CPU count, its load and the other tests running beside it.
A fixed 10 s clock on it failed `TestSetAskBlockTextRefusalOfEveryBrokenBodyLeadsToAValidDecision` (2026-11-16, 288 CPUs).
At the timeout all 8 connections were in use, with 1449 s of acquire waiting in total, and the same wait issued again returned at once.

So `settleCurrentGeneration` waits on the room's own signals, its pending updates reaching persistence and its appends becoming durable (`waitForPendingUpdates`, `waitForDurableAppends`), under the shared wait's context (`testwait.Context`), and sets no span of its own.
A queue of any length only delays it, and only the test binary's deadline ends it, naming the wait.
An append that another transaction's hold on the document's lock keeps from landing for good fails it there; a held append that lands late passes.

## A writer a test holds

A test that needs a live socket's writer held, so that frames queue behind it while the test sends the socket more frames, holds the writer itself:
it builds its rig on a `heldWrites` listener (`packages/envoy/internal/dispatch/api/live_socket_test.go`), calls `hold`, and waits with `waitHeld` until a write waits at the gate.
Small TCP buffers and a page that reads nothing (`smallSendBuffers`, `smallReadDialer`) stall the writer only until the transport lets its write through, and the transport decides when that is.
Under gVisor, the runtime of Legion's worker pods, a write blocked for a third of a second completed the moment the page sent its next frame.
`TestARepeatedSubReplacesAnotherKind` then waited for a queued frame the writer had already written, and failed 8 of 300 runs.
Those two helpers are for tests of a slow page itself, such as one whose queue overflows.

## A test that starts jj or git names a working directory

jj searches upward from the directory it runs in for the closest `.jj` and opens that repository, and it has no ceiling option.
`-R` names the workspace a command acts on, and the two that create a repository, `jj git clone` and `jj git init --colocate`, take no `-R` at all.
`-R` does not keep jj off the repository its working directory sits in:
jj 0.45.1 loads that repository's configuration before it applies `-R`, and `jj -R <b> log` run inside a repository `a` that still held a legacy `.jj/repo/config.toml` migrated it, writing `a/.jj/repo/config-id`.
Every `-R` call site of `packages/daemon/internal/workspace` is safe because its `Dir` **is** its `-R` target, not because the flag fences jj in:
`runCheckedOnClone`'s comment states that invariant, where the directory a command runs in is chosen.
A child process started with no working directory of its own inherits the test process's, which in a Legion issue pod is inside `$LEGION_WORKSPACE`, a jj workspace of the pod's real shared clone under `/legion/repos`.
So a test whose jj child names no directory opens and writes that clone, and the same test is green on a GitHub runner, whose checkout is git-only with no `.jj` anywhere above it (dispatch://INFRA-623).

- Set `Dir` on every `exec.Command` a test starts that runs `jj`, and on every one that runs `git`, to a directory the test made.
  A `--git-dir` is not enough on its own: it tells git which repository to act on and leaves the child standing wherever the test process stands, which is what the next reader of the command has to work out again.
  `Dir` is not enough on its own either, and this is the step the rule stopped short of:
  an inherited `GIT_DIR` names the repository git acts on whatever directory the child stands in, so it outranks `Dir` and `-C` alike, and a Legion issue pod hands every process such a block.
  So a test that starts git takes those variables out of the child's environment as well as placing it:
  `fixtureenv.Guard` in the package's `TestMain` for a child that inherits `os.Environ()`, or `fixtureenv.Isolated` for one given an environment of its own.

- The two probes that got this wrong were copies of one helper, now `testbin.RequireJJFilters` in `packages/daemon/internal/testbin`, each running `jj config list` with no `Dir`.
  A probe is as much a jj child as a fixture is.

- A test that drives `packages/daemon/internal/workspace` needs nothing of its own:
  `execRunner.Run` refuses a `Command` whose `Dir` is not absolute, and refuses a jj command that creates a repository when that directory, or any above it, holds a `.jj` directory (`refuseEnclosingWorkspace`).
  `TestProvisionLeavesTheEnclosingRepositoryUntouched` holds both on a runner, by provisioning from inside a decoy repository.

- A test that builds a repository of its own puts it under `t.TempDir()`, and `TMPDIR` must sit inside no checkout.
  Nothing refuses the fixtures' own `jj git init --colocate` (`runSetup`, `enclosingRepository`):
  it runs outside the runner, where `refuseEnclosingWorkspace` never sees it, so under a `TMPDIR` inside a checkout it would read that checkout's repository's configuration first, and migrate it when that repository still holds a legacy `.jj/repo/config.toml`.

## A test that starts jj or git inherits no pod's environment

A Legion issue pod hands every process its live workspace (`LEGION_WORKSPACE`), its git configuration (`GIT_CONFIG_*`), its GitHub credential, the deployment's own session store (`OMP_SESSION_SQL_DSN_FILE`, `OMP_SESSION_STORAGE`) and more.
A test that let git or jj inherit them read and pushed the pod's own work:
the shim's rescue test, run with no `LEGION_WORKSPACE` of its own, pushed the pod's workspace to the real origin as `legion/WIDGETS-42-rescue-…` branches.

- A `packages/daemon` package whose tests run git or jj, as children or in-process through the code under test, calls `fixtureenv.MustGuard` in its `TestMain`, after any branch where the test binary runs as a helper child.
  MustGuard is Guard with a `TestMain`'s only answer to a failure:
  it prints it and ends the process with status 1, which `packages/daemon/internal/fixtureenv`'s own test holds, with the guarantee Guard gives every package (`fixtureenv.RequireGuarded` stays for the test that needs the check in its own body).
  Such a package is also a line of `guardingPackages` in `packages/daemon/internal/fixtureenv/guardingpackages_test.go`, which fails when a listed package's `TestMain` calls no `MustGuard`, calls it only inside a branch, runs the tests before it, or runs them under a branch whose condition does not say a helper-child marker is set (`os.Getenv(<name>) != ""`, or `if _, ok := os.LookupEnv(<name>); ok`), since every ordinary run of the package takes such a branch too.
  It also fails a `TestMain` that a build constraint keeps out of the default `go test` build, and a package of the module whose `TestMain` calls `MustGuard` while the list does not hold it, so adding a package to the guard adds it to the list.
  No test can see another package's process, so that list is the one check of every package's guard.
  Guard unsets every `LEGION_*`, `GIT_*`, `JJ_*`, `GH_*` and `GITHUB_*` variable, `SSH_AUTH_SOCK` and `SSH_ASKPASS`, and the three names that place an Oh My Pi session (`OMP_SESSION_SQL_DSN_FILE`, `OMP_SESSION_STORAGE` and `OMP_SESSION_SQL_DSN`, which carries the same URL by value), keeping the tests' own inputs (`LEGION_TEST_*`, `LEGION_E2E*`, `GITHUB_ACTIONS`, and the names the package passes), and sets `GIT_ALLOW_PROTOCOL=file`, which a repository's own `protocol.<name>.allow` cannot override.
  The session store a pod names holds the conversation of every agent of the deployment, so a test that opened it would read and write what those agents are reading and writing.
  Code under test that names its own transport, as provisioning's `GIT_ALLOW_PROTOCOL=https` does, still uses it.
  Guard also takes off the process's `PATH` every directory whose `git` is the image's git wrapper (`/opt/legion/bin/git`, or a link to it), which leads a pod's `PATH`: that git puts Legion's leak guard in front of every push, and that jj refuses a push whose git is any other, so a fixture's push to a local bare repository under the test's empty home was refused in a pod and passed on CI, whose `PATH` holds no such directory.
  It is the process's `PATH` that matters, because `exec.Command` and `exec.LookPath` look a bare name up on it whatever `PATH` the command's `Env` holds.
  Guard does the same under the `e2e` build tag, whose untagged tests are the same fixtures.
  A live proof of that build, which reaches a real cluster and GitHub on purpose, calls `fixtureenv.Live(t)` first in its entry point:
  that gives the test the process's own values back of everything Guard took out or set, until the test ends.

- A fixture command runs under `fixtureenv.Isolated(home)`: the same variables dropped, a home of the test's with its config and state homes (`XDG_CONFIG_HOME`, `XDG_STATE_HOME`) under it, and an identity.
  The state home is the test's because the leak guard caches public visibility answers there, and a test on the runner's would read answers other tests cached.
  A package whose code under test reads the process's home sets `fixtureenv.Home` on the process and gives its fixtures that same home (`packages/daemon/internal/workspace`'s `setupEnvironment`).
  Isolated keeps no test input either, so a fixture that needs one appends it after.

## Containers a test starts

Every container a test or a proof script starts carries a CPU limit and a memory limit sized to its work, and is removed when the test stops, except kind's node containers (`scripts/e2e/README.md` says why) and the Pulumi containers of `packages/envoy/infra/host/live_test.go`, whose released `Listener` component has no CPU or memory inputs.
Tests and the live proofs run on shared dev boxes as well as on CI runners, and an unlimited server can take every core and all the memory of a host that other work shares.

- In Go, set `Resources.NanoCPUs` and `Resources.Memory` in the container's host configuration:
  through `testcontainers.WithHostConfigModifier` for a `Run`, or as the request's `HostConfigModifier` for a `GenericContainer` (`packages/envoy/internal/testpg/testpg.go` is one).
  `WithHostConfigModifier` replaces any modifier an earlier option set, so a test that needs a host configuration of its own sets the limits in that same modifier.

- In a script, `docker run` takes `--cpus` and `--memory` before the image.
  A Postgres whose data directory is a `tmpfs` mount counts that data against its memory limit.
  `scripts/e2e/README.md` lists each proof's containers and their limits.

- Remove a container with its anonymous volumes: `docker rm -f -v`, since the Postgres images declare a `VOLUME` that a plain `docker rm -f` leaves on the host.

## A container a test starts publishes no port

A test or a live proof never lets Docker choose a host port for a container: no `-p` or `--publish` that names no host port, no `-P`, and no testcontainers-go request on Docker's bridge network, which publishes every port the image declares when the request names none.
Docker chooses such a port with an allocator of its own: the port after its last choice in the kernel's ephemeral range (`/proc/sys/net/ipv4/ip_local_port_range`), which it binds without asking the kernel whether another program holds it.
Every `connect()` and every bind to port 0 on the machine takes its port from the same range, so on a busy machine other programs hold ports Docker has not counted.
Docker tries the next port when its bind fails, ten times in all, and then fails the container's start with `failed to bind host port … address already in use`.
On one dev box, from 2026-11-16 to 2026-11-17, 32 container starts failed that way, and their tests failed for a cause outside the code under test (dispatch://INFRA-4327).

A server gets its port in one of three ways, by what the server can do:

- A server that binds port 0 and logs the port runs on the host's network (`--network host`), where Docker publishes nothing and the kernel chooses the port at the server's own bind.
  NATS runs with `-a 127.0.0.1 -p -1`, and `-m -1` where the test reads its monitoring, and the test reads `Listening for client connections on 127.0.0.1:<port>` and `Starting http monitor on 127.0.0.1:<port>` from its log.
  `packages/envoy/internal/testnats`'s `StartImageConfig`, `packages/daemon/internal/testnats`'s `StartImage` and `StartOpenImage`, the envoy smoke test (whose listener takes `PORT=0` and logs `envoy-listener listening`) and `scripts/e2e/lib/containers.sh`'s `start_nats_container` and `nats_container_ready` start their servers so.

- Postgres, which cannot take port 0, listens on a unix socket alone: `-c listen_addresses= -c unix_socket_directories=/var/run/postgresql,/sockets`, with a directory of the run's own mounted at `/sockets`, mode 0777 for the image's user, and short enough for the 107 bytes of a socket path ("Unix sockets in Go tests" above).
  Its address is `postgres://<user>:<password>@/<database>?host=<directory>`, which psql, pgx, the daemon's `postgres_dsn` and Dispatch under `DISPATCH_DEV_SIGNIN=1` all take; a WHATWG URL reader (`new URL` in Bun) refuses userinfo before an empty host, so an address such a reader parses names the user and password in the query instead (`postgres:///<database>?host=<directory>&user=<user>&password=<password>`).
  The test waits for the second `database system is ready to accept connections` line, since the image's entrypoint first runs a server on the same socket to make the database, and stops it.
  `packages/envoy/internal/testpg` (on the host's network, where testcontainers publishes nothing) and `containers.sh`'s `start_postgres_container` and `postgres_container_ready` start their servers so.

- A server with fixed ports of its own runs on Docker's default bridge network, in a network namespace of its own where no other program holds those ports, and publishes nothing.
  The test dials it at its container address (`docker inspect`'s `.NetworkSettings.Networks.bridge.IPAddress`), as `packages/envoy/internal/tests3` dials SeaweedFS, or reaches it through `docker exec` alone, as the task health check of `packages/envoy/infra/core/nats_monitor_healthcheck_live_test.go` does, whose subject is the container's own loopback.
  testcontainers-go publishes the image's declared ports on that network, so these start with the docker CLI; in `packages/envoy` the container-runtime guard of `internal/testnats/nats_test.go` admits that in the rigs' own packages alone, and its walk leaves out the `infra` module.

testcontainers-go's reaper, Ryuk, publishes a port Docker chooses too: every CI job that runs testcontainers sets `TESTCONTAINERS_RYUK_DISABLED`, and so does a developer who runs those tests on a busy machine.

## A CI job that failed with no log at all

A job that stored no log did not hang: it lost its runner, and the one artifact that outlives the log says so.

GitHub stores a job's log when the runner finalises it.
A runner the service stops hearing from never finalises, so the logs endpoint answers `HTTP 404` with `<Code>BlobNotFound</Code>`, where a job that was cancelled, that failed and that passed each answers its log.
The check run's annotation is written by the service and not by the runner, so it is still there:

```sh
gh api repos/<owner>/<repo>/check-runs/<job id>/annotations
```

It answers one annotation at `annotation_level: failure`, in these words: "The hosted runner lost communication with the server.
Anything in your workflow that terminates the runner process, starves it for CPU/Memory, or blocks its network access can cause this error."

Read the job record beside it (`gh api repos/<owner>/<repo>/actions/jobs/<job id>`).
A lost runner leaves one step `in_progress` with a null conclusion and every step after it `pending`, so the last line of the step record is the last second anything was reported.
Compare `created_at` with `completed_at` and with the job's own `timeout-minutes`:
a job its ceiling ended stops at the ceiling, and one that stopped before it was ended by something else.

Those three causes are what GitHub can say, and none of them is a reading of your job.
A job that carries `.github/scripts/disk_watch.py` through its steps prints the free disk, the memory the kernel reports available and the one-minute load of every second it watched, which is what tells the second cause from the other two on the next run that does store a log.
`tool-fallback` and `tool-fallback-arm` carry it from before their first faulted install to after their last, and `envoy-smoke` carries it around its live-tests step alone.
It says nothing about the run that was lost: the series goes to the log, and that run has none (dispatch://INFRA-2605).

## A test asserts no count of work a broker may repeat

A message broker that gives at-least-once delivery may hand one message to a handler twice.
NATS JetStream is this repository's broker.
Four facts decide when it hands one over a second time.
The comment on `ConsumerSpec.HandlerLimit` (`packages/daemon/internal/intake/consume.go`) is where the mechanism is written whole, with the measurement behind each part.

- The intake consumer asks for one message at a time, so no pull is outstanding while a handler runs and nothing reaches the consumer then.
  A reconnect, which pulls again, is the one exception.

- The progress keeper prevents a second delivery while each of its reports reaches the server before the acknowledgement window lapses:
  each such report starts the window again.

- A late signal lets the message fall due again.
  A late report lets it fall due while the handler still runs.
  A late acknowledgement, which goes out only after the handler returns, lets it fall due after that return.

- A message that fell due is delivered again, and that redelivery races the acknowledgement or nak the handler's return sends.
  Either side can win.
  Each measurement is of one condition: with the progress keeper removed, the redelivery reached the consumer in 12 runs of 20;
  with a handler step that ignored its cancelled context and held the message past the window, the redelivery won all 15 runs on an idle machine and lost to the nak only under load.
  Neither is the rate under a keeper whose reports arrive in time, nor under a cut whose step honours its cancelled context while its reports and its nak all reach the server inside the window:
  the message then never falls due, and the nak alone decides when it comes back.
  A nak the server works first resets the redelivery timer, so the event comes one nak delay later instead.

Every counter a handling moves then moves a second time: a statement counter, a request counter, a call counter in a fake.
That second move is the broker's business and no part of what the code promises, so a test asserts no exact count of such work.
Assert the effect the handler leaves behind instead: the record written, the fact applied once, the row a repeated handling does not add.
A repeat is then invisible to the test, because the handler's effect is the same either way.

Two kinds of row keep an exact count.

- A count is sound behind a window far above the handler's own work and above the trip its acknowledgement makes.
  The window is a hang guard then, and not a bet on timing:
  a redelivery the count would see needs the whole window to pass before that acknowledgement reaches the server.
  `TestAnEventTheDaemonCannotActOnIsAcknowledgedWithNoTransaction` (`packages/daemon/internal/intake/no_transaction_test.go`) sets `spec.AckWait = time.Minute` over a handler whose own work is about `consumerSpec`'s 200 ms on a loaded runner.
  The margin is what this carve-out rests on:
  that work, and the acknowledgement's trip to the server, which the campaign of dispatch://INFRA-1113 measured at 516.8 ms at its largest, come to about 717 ms together, a factor of about 80 below a minute.
  The progress keeper is no part of it.
  At a minute its period is 20 s (`spec.AckWait/3` in `consume.go`) and that handler acknowledges in about 200 ms, so the keeper never reports for that message at all.
  The carve-out ends where the margin closes, and a reader finds that out by measuring the handler's work and the acknowledgement's trip again, not by reading this sentence.

- A row whose subject is the broker's own delivery behaviour must count deliveries, because the count is what it proves.
  Such a row needs a window above the measured distribution of the first report of the progress keeper, the goroutine that reports a running handler in progress so that the server does not make its message due again.
  A window above the handler's own work is not enough.
  Until the keeper has reported once, the window alone stands between the held message and the server making it due again, and the count sees whatever repeat follows.
  `TestAFactHeldPastItsAcknowledgementWindowIsDeliveredOnceAndPreparedOnce` and its control `TestAFactHeldPastItsAcknowledgementWindowIsDeliveredTwiceWithNoProgressSignal` (`packages/daemon/internal/intake/ackprogress_test.go`) are of this kind.
  This carve-out rests on the row's subject being the broker's delivery behaviour:
  the count is the claim, so it needs the sized window whatever it is read from.
  A delivery count the daemon records is no way out of that, since such a count moves with the same late signals, and the daemon logs one:
  `delivered`, on `an intake fact outlived its acknowledgement window and was held
  in progress while it ran` and on `an intake fact was delivered again and dropped as already
  applied` beside it.
  The carve-out would end only where that behaviour can be seen through an effect that does not itself move with delivery timing.
  There is no such effect today, those two lines included.

The comment beside such a count names the window and the measurement that sized it.
A later reader changes the window, the handler or the keeper's period, and that comment is what tells the reader which number the count rests on.

The rule is measured.
`TestAnEventTheDaemonCannotActOnIsAcknowledgedWithNoTransaction` asserted an exact running total of the statements that take the fact lock.
It failed in CI with `after its
checks settlement the fact lock was taken 3 times, want 2: once for each event a record takes` (job 126264090148 of run 41723103641, 2026-11-03).
`ApplyFact` (`packages/daemon/internal/intake/facts.go`) takes the fact lock before `record.Store.MarkProcessed` deduplicates the event, so a second handling of a message whose fact is already applied costs one more lock statement.
It writes no `processed_events` row and runs no handler.
JetStream gives at-least-once delivery and the intake gives exactly-once application.
The row asserted the first.

The second handling happened because the progress keeper was starved.
`keepInProgress` (`packages/daemon/internal/intake/ackprogress.go`) reports the handler in progress on a `time.Ticker` that ticks every third of the acknowledgement window.
A ticker drops a tick its goroutine was not scheduled for, and by its own documentation it sends only after each tick, so the keeper reports nothing during the first period of a window (Go 1.27.3).
Measured at a 200 ms window, whose period is 66.7 ms:
the largest interval from the keeper's start to its first report was 403.7 ms, which is 6.06 times the period, and the 99th percentile of that interval was 276.8 ms.
That late first report is the late signal above.
At the 200 ms window the row failed 2 of 1000 runs in one campaign and 7 of about 2274 in another, which is 0.2 and 0.31 per cent.
At the committed one-minute window it passed 800 of 800.

Four issues record the same fault.
dispatch://INFRA-456 was the daemon preparing one Dispatch issue event's repository more than once under redelivery, which is why `keepInProgress` exists.
dispatch://INFRA-1471 was two rows of the intake counting work the broker repeats.
dispatch://INFRA-1530 built the keeper itself.
dispatch://INFRA-1113 measured the mechanism above and wrote this rule.

## Pages under `docs/solutions/` that this design makes untrue

Each page below was true when it was written.
Each holds a rule or a fact that the design above makes untrue, named here with what is true now.
The rest of each page is not changed by this design.
A page that is not in this list can still name a shape that this design removed, so read a page's claims against the code before you follow them.

- `testing/the-daemons-nats-tests-run-in-a-docker-less-pod-against-a-local-nats-server-named-by-two-variables-INFRA-388.md` tells a reader to run the daemon's NATS tests one package at a time against a local nats-server that two variables name, and says that `testnats.URL` resets that shared server for each test.
  Now each test gets a server of its own in the test process, and no variable names one ("A Go test that opens a NATS client").

- `testing/pgvector-from-the-pgdg-deb-lets-the-pods-embedded-postgres-run-the-scratch-dispatch-tests-INFRA-252.md` tells a reader to point the two NATS variables at a local `nats-server -js`.
  Now the daemon's tests need no NATS server.

- `testing/the-pod-only-skip-list-is-re-derived-against-a-main-extract-and-the-daemon-package-needs-a-longer-timeout-INFRA-225.md` says to run one NATS-backed `go test` process at a time for each server, because a second process's reset deletes the first one's streams.
  Now no two test binaries share a NATS server (R1).

- `testing/a-nats-stream-deleted-twice-in-quick-succession-comes-back-with-its-messages.md` says that the daemon's `testnats.URL` keeps one account and deletes every stream on the shared server before it gives the server to a test.
  Now each test's server is new and empty.

- `testing/the-envoy-in-process-nats-rig-what-it-guarantees-and-the-three-places-it-does-not-INFRA-250.md`, section 3, says that the daemon's `testnats` is container-backed and that its `URL` refuses a second take by the test or by one of its subtests.
  Now the daemon's server runs in the test process, a second call in a test gets the same server, and a subtest gets its own.

- `testing/go-test-in-a-worker-pod-leaves-gowork-to-go-work-and-names-the-packages-the-pod-cannot-run-LEGION-630.md` lists `internal/daemon`, `internal/intake`, `internal/natsauth` and `internal/testnats` as packages that fail in a pod with `panic: rootless Docker not found`.
  Now their NATS tests start no container:
  only `TestDaemonPermissionsOnRealServers` and the tests of `StartImage`'s helpers need one, and off GitHub Actions they skip where none answers.

- `testing/a-many-package-go-lane-reads-package-level-fail-lines-uncapped-environmental-test-failures-flood-a-head-window-LEGION-629.md` takes `internal/intake`, `internal/natsauth` and `internal/testnats` out of a pod's run as packages the pod cannot run, after the LEGION-630 page.
  Now their NATS tests run in a pod.

- `testing/a-loaded-devbox-stretches-every-wall-clock-budget-in-the-envoy-and-pi-envoy-suites.md` tells a reader to start a NATS container with no deadline, as the daemon's `workflowNATS` does.
  Now the daemon's NATS tests start no container.

- `testing/prove-a-nats-grant-in-a-docker-less-pod-by-rendering-it-from-the-branchs-own-code-INFRA-117.md` and `testing/a-pod-drives-the-rendered-configuration-itself-and-a-plans-ci-only-claim-is-checked-against-docs-solutions-INFRA-127.md` say that every suite built on `internal/testnats` needs Docker, so only CI runs it.
  Now the `testnats` of each module runs its servers in the test process.

- `testing/a-live-proof-that-switches-what-a-base-requires-answers-every-read-of-the-required-checks-pass-INFRA-272.md` says that `testwait.Eventually` gives up after 1 minute, and tells a reader to poll with a loop of their own bounded at 3 minutes.
  Now `Eventually` ends only at the binary's deadline less testwait's margin, and no test keeps a loop with a bound of its own (R2).

- `testing/a-live-proofs-read-of-a-board-lane-effect-waits-on-that-lane-a-wait-on-the-lifecycle-lane-orders-nothing-there-INFRA-229.md` tells a reader to hold a claim that something did not happen on the board lane over a settle window.
  Now a test proves an absence from state, from `synctest.Wait` or from the effect of a later action, and holds a span only through `Span` or `Quiet`, with its why (R2).

- `testing/a-test-of-a-read-that-defers-under-shared-state-retries-only-what-the-routes-own-bound-admits-pr2688.md` tells a reader to time each `?since=` read and to read again only a read that spent the route's bound.
  Now the test's server runs on a held fake clock, the route's wait never ends on its own, one read is final, and no verdict reads a clock (R4, and "A test that reads `GET /api/v1/references?since=` holds the route's wait open").

- `testing/three-reds-one-shape-a-flaky-upper-bound-is-repaired-by-the-statistic-or-the-observable-pr2641.md` says that a bound which is the only thing left in a test is a hang guard, which stands far from the work's own duration and inside the step's `-timeout`.
  Now a test keeps no bound of its own, and its command's guard is the one hang guard (R5).

- `testing/a-wait-whose-one-poll-scales-with-the-host-needs-a-poll-floor-beside-its-deadline-pr2163.md` says that `until_true` serves every other proof.
  Now a proof that a workflow runs sets its guard with `proof_guard` and waits through `wait_for`, and `until_true` serves only the proofs that a person runs (`scripts/e2e/lib/rig.sh`).

- `envoy/a-respelling-search-reads-each-distinct-writing-back-once-and-a-linearity-gate-takes-no-clock-floor-pr2373.md` says that `cputest.Measure` solves a coarse clock in `growthVerdict`.
  Now `internal/cputest` is gone, and `growthVerdict` counts the parse's work and reads no clock (R4).

- `envoy/two-pull-requests-take-the-same-migration-number.md` says that `openTestStore` migrates the database that `DISPATCH_TEST_DATABASE_URL` names in place.
  Now each test of the Dispatch store takes an empty database of its own (`openEmptyTestStore`), and no test migrates the database that the URL names (R1).
