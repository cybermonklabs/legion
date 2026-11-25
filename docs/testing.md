# Testing

Rules for this repository's tests that the code under test does not show.

## The fixtures a test takes rather than copies

Each of these is one body per Go module, and a test that needs what it does calls it rather than writing the body again.
Each bullet says what the helper does not do, so a site that must keep its own body is not folded into it.

- A test that captures a log takes its buffer from the module's own: `packages/daemon/internal/testlog`'s `Buffer`, and `packages/envoy/internal/logtest`'s, which lives beside that package's default-logger swap and carries `Lines`, the logged lines holding a substring.
  `packages/envoy/internal/cmdtest`'s `LockedBuffer` and `storetest`'s `LockedLog` are both aliases of it, so either name reaches the same body.
  It is a mutex around a byte sink and nothing else: a type that needs a reader of its own embeds it and keeps that reader, and a writer that is no log sink — an `http.ResponseWriter`, a TCP relay, a writer that gates what it passes on — keeps its own body.

- A bounded wait is the module's `internal/testwait`.
  `Eventually` is the wait a test usually wants, at the package's own pace (`Poll`), failing with what it waited for: `packages/daemon`'s gives up 30 s before the binary's deadline and at no other moment, as `Until` and `Context` do (below), and `packages/envoy`'s after a minute or the binary's own deadline less a second; `WithinPolling` takes a bound and a pace of its own; `Holds` answers rather than fails, for a caller whose failure text is its own diagnostic dump or is built at give-up; and `Bounded` clamps a span of the caller's own to what is left of the binary's deadline less a spare of the caller's, for a wait that keeps a loop or a margin of its own, as the idle rig's do.
  `Remaining` is what is left of the binary's deadline less 30 s, for a hang guard that is not the test's subject, such as the acknowledgement window of a consumer whose window also bounds each store read a handling makes, and a wait for those acknowledgements (`packages/daemon/internal/intake`): a fixed bound there, a minute say, lets the machine decide whether a store read of 61 s is cut.
  `Receive`, in both modules, waits for one receive from a channel, such as the answer of a call the test started, and sets no span of its own: it gives up 30 s before the binary's deadline (the daemon module's, a quarter of a `-timeout` under 2 minutes, so its waits still wait under a short `-timeout`), writes why to stderr at once and then fails the test, so a call that waits for ever fails by name, even without `-v`, and a call that is merely slow passes; the launcher rig's waits for a role's connection and for its next frame are such waits.
  A row whose verdict is what a call answers, a refusal or a replaced file, waits for it with `Receive`.
  In both modules, `Until` waits until a reading holds and returns it, at `Poll`, and `Context` is a context for a call that takes one, or for a socket's deadline; `packages/envoy`'s `UntilPolling` is `Until` at a pace of the caller's. Like `Receive`, they end at the binary's own deadline less 30 s, the time the failing test's cleanups run in, and take no bound of their own, so a step that is only slow on a loaded machine fails nothing. Under a `-timeout` of 30 s or less that moment has passed before the test starts, so each of them, and the daemon module's `Eventually`, fails at once.
  The daemon module's `Eventually` and `Until` have no cap, and that has a cost: a wait whose fact never comes holds its test binary until 30 s before the deadline (in `daemon-go-race`, `-timeout 30m`, about 29.5 minutes), and every later `Eventually` or `Until` in that binary then begins past that moment, asks once and fails. Such a give-up line adds ", a moment that had passed before this wait began: the first give-up line in time order is the cause", so read a red package run from its first give-up line in time order: that one names the wait that never held, and the lines of tests that failed after it in 0.00 s are its side effects.
  A loop that is not a wait-for keeps its body: one that runs its check for a whole span rather than stopping at the first success, one that sends or serves on a tick, one that drives a stand-in process's own main loop, and one that retries a compare-and-set.

- A fifo that the code under test must replace without opening it for writing is planted with the daemon module's `internal/testfifo`.
  `Plant(t, path, perm)` makes the fifo and holds a reader on it until the test ends, so an open of it for writing returns at once rather than waiting for a reader, and `OpenedForWriting` answers what that reader was shown: POLLHUP once a writer opened and closed the fifo, POLLIN for bytes it wrote, a writer that holds it open, and nothing otherwise, with no clock.
  `Plant` first proves, on a scratch fifo of its own, that a held reader sees a writer's open and nothing before it, and fails the test by name on a kernel where it does not.
  It does not see an open for reading, which shows the reader nothing, and it does not keep a plain open for reading from waiting for a writer: a row whose code must refuse a fifo that it opens keeps the fifo bare and waits for the refusal with `testwait.Receive`.

- A fixture that runs git or jj takes its runner and its `init` argv from `packages/daemon/internal/fixturegit`.
  `Runner` holds the test and the child's whole environment — `Isolated(t, home)` is the usual one — and runs one command four ways: `Run` trimmed, `Raw` untrimmed, `Out` for standard output alone, and `Try` for a caller that keeps the error.
  `InitBare`, `InitWorkTree` and `JJInit` build the whole argv, and the semantic axes stay the call site's: bare or not, the initial branch, and colocation, with only the quiet spelling normalised.
  The runner chooses no binary — `argv[0]` does, so a fixture that runs a pinned git or jj passes that path itself.
  `packages/envoy` has no Go git or jj fixture runner, so this one is the daemon module's alone.

- A fake GitHub server is the module's `internal/githubtest`: `Server` for a fake its caller reaches by URL, `Serve` where the caller needs the server itself, and `Unexpected` as the default arm of a fake for which an unplanned request is a failure.
  `Spec` carries the opt-in bearer gate and the opt-in request recorder.
  The `switch`, its `default:` arm and the fake's own state stay the caller's, because they are what each test proves: a fake that reads the per-owner token the daemon minted must answer a request it then reads, and a `default:` that logs, that answers 400 or that is the happy path says something a shared arm would take away.
  A server that is no fake GitHub keeps its body, whatever it mentions: the daemon's own API, the issue-sandbox API, a fake Dispatch, a fake secrets broker, a reverse proxy in front of another fake, and a real `git http-backend` CGI behind a TLS proxy.
  A fake a test builds inline, in its own body rather than in a function of its own, is the same skeleton and takes the helper the same way; it is no shared copy, though, so nothing counts it, and most of the two modules' inline fakes still write it out.

- An in-test `go build` in `packages/envoy` goes through `internal/cmdtest`: `Build`, `BuildStamped`, or `BuildCommand` for a build that needs a directory, a target, flags or an environment of its own.
  One body holds `ENVOY_TEST_GO_OVERLAY`, the output and the failure; the overlay that variable names must be an absolute path, because a build whose `Dir` is another directory reads it from there.
  Each build is made once per test process and shared by every test that asks for the same one, in a directory `cmdtest.Main` makes and removes, so a package that builds a command runs its tests through it: `func TestMain(m *testing.M) { os.Exit(cmdtest.Main(m)) }`, or `cmdtest.Run` around another harness's `Main`.
  A `go build` that produces no binary, such as a compile check of a generated module, is not a build of this kind, and neither is one that takes no `testing.TB`: `infra/internal/pulumitest/pulumitest.go` builds the test binary's own plugin once per binary, under a `sync.Once` and removed by its `Main`, and returns an error rather than failing a test.

- A peer that is not there is the module's `internal/testport`: `Refused(t)` is a loopback address whose port the test holds bound, never listening, until it ends, so every dial to it is refused and no other program can bind the port, with or without `SO_REUSEADDR` or `SO_REUSEPORT`.
  It replaces the address of a server or listener the test closed: a freed port is any program's to take, and on a shared machine one took it and answered HTTP (dispatch://INFRA-3904).
  It holds no listening socket and hands nothing to a child: a process the test starts takes a socket the test holds (`cmdtest.HoldPort` and `Pass`, `DISPATCH_LISTEN_FD`, `ENVOY_LISTEN_FD`, `LEGION_LISTEN_FD`) or binds port 0 and names its port, never a port the test picked and let go of.
  A daemon a test boots in its own process (`packages/daemon/internal/daemon`) takes its API listener from a socket the test holds (`heldListen`, through `overrides.listen`), and a test whose daemons must listen on one port across a restart, as the controller sandbox rig's worker stream must, hands each boot a descriptor of a socket it holds for its whole run (`heldSocketListen`, through `overrides.listen` and `overrides.streamListen`).
  A port a test holds so that another bind of it fails is held at the address that bind names: a wildcard listener from `net.Listen` (`0.0.0.0` or `[::]`, which sets `SO_REUSEADDR`) keeps a bind of one address off its port under Linux but not under gVisor, whose netstack lets a second socket that sets `SO_REUSEADDR` bind that address beside it (`internal/testnats`'s `TestAServerThatLoggedAFatalErrorFailsItsStartEvenOnceReady` holds its server's profiler port at the server's own host for this reason).

- A listener a client dials — a test NATS server, a relay, proxy or fake in front of one — listens on `testport.ListenLoopback()` in `packages/envoy` and on `testnats.ListenLoopback()` in `packages/daemon`: a port the kernel chooses at a loopback address of its own, one of `127.0.0.0/8` chosen at random on Linux (`LoopbackIP`), and `127.0.0.1` elsewhere.
  A client a test leaves behind keeps dialing its server's address (a bus client reconnects forever), and the kernel or Docker gives a freed port to the next listener that asks, in the same test binary or in another one `go test` runs beside it; on `127.0.0.1` that listener's test takes the stranger's connections for its own.
  So does every server `packages/envoy/internal/testnats` and `packages/daemon/internal/testnats` start: an in-process server's relay and nats-server's own client, monitoring and route listeners, and a container's published client port (the daemon's containers' monitoring port too), so a leftover client cannot reach it, whichever port it gets.
  So do the `nats-server` processes `packages/envoy/infra/gcp`'s natslive tests run as the clustered hub's pods, the route tests' servers and rogues among them: each test's servers listen at one such address (`-a`), with their ports reserved there (`reservePortsOn`).
  Of those, the route check's staged hub (`clusteredhub_routecheck_fixture_live_test.go`) keeps each server's route port, and each peer's, on the `127.0.0.x` address the rows name it by, `127.0.0.1` being the pods' IP: the check takes a route connection from a pod IP as the hub's own, and a route from `127.0.0.1` reads as from `127.0.0.1` on any loopback (the Linux kernel's sources every loopback connection from `127.0.0.1` whatever `127/8` address it dials; gVisor's sources it from the address dialed). Their client and monitoring ports are at addresses of their own, and a test checks that a client at `127.0.0.1` reaches none of them (`requireNoStrayReaches`).
  A stray client can still dial such a route port: a reservation stops a second bind, not a dial. The check reads nothing of it: it skips a route connection from a pod IP, and counts a pod IP's refused route login only when the mesh is split (`infra/routes/log.go`, `routeConnections`). The route check's first row has a stray log in at each server's route port and requires the check to stay clean.
  A port another process binds, such as the host side of a port Docker publishes, is chosen by the kernel on that address (`testport.LoopbackPorts`): Docker asked for port 0 picks from its own record of what it has published there, and in envoy-infra-go it picked a port already held. The probe is released before the bind, so a start a taken port refuses (`testport.AddressInUse`) is tried again on a fresh address and port.
  Four listeners stay where they were, each for its own reason.
  `internal/testnats/cmd/natsd`, the standalone server of the scripts and the TypeScript stacks, stays on `127.0.0.1`: no Go test runs it.
  `internal/smoke`'s NATS container keeps Docker's default binding: envoy-smoke runs that binary alone on its runner, and its one client, the listener container, reaches it over the Docker network.
  `infra/core`'s `nats_monitor_test.go` server keeps the address its start script binds, which is what that test checks.
  `infra/host/heldportwrapper`'s test address accepts no connection, so a stranger changes nothing it checks.
  A test still closes every client it opens before its server, and registers the close as soon as the connect succeeds, so a setup that fails closes it too: `cmd/listener`'s `setupTestNATS` left ten tests' bus clients dialing for minutes after their tests had ended, and they reached the test servers of other packages' binaries.
  A container start tried again after a taken port refused it builds its container's options anew, a configuration file's reader among them: testcontainers reads a file's reader to its end as it creates the container, before Docker refuses the publish, so a retry handed the first attempt's reader copies an empty file in, and a server started from it enforces none of the users its configuration names.

- A test that a command line keeps the rules of a cobra command line takes `packages/envoy/clitest`, one body for both modules: `packages/daemon` can import it through its `replace` of `packages/envoy`, and it imports the standard library alone.
  `Conformance` runs the built binary, of any framework, and checks help on every command; a typo, an unknown flag at the root and on each command, an unknown shell for `completion` and an unknown topic for `help` that each exit 2, print the word they refuse, are no Go runtime crash and make no contact with its `Recorder`; a flag read after a positional; the completion script of each shell; and `__complete`.
  The package documentation is the whole list and the rule each check holds.
  It does not check what a command does with its arguments, its other exit codes or the shape of its tree, which each conversion's own tests hold, and it builds no binary: the caller hands it one.


## Bun tests

`bun test` runs every file of a package in one process, so what one file leaves behind is what the
next file reads: a directory, an environment variable, a port, a container, a cache. These rules are
how a file leaves nothing behind. `@legion/test-fixtures` holds the shared pieces, and nine
packages depend on it: cipher-client, claude-envoy, contracts, dispatch, envoy-client, pi-envoy,
pi-legion, pi-shared and proof-editor. The one Bun package that does not is the fixture package
itself, whose tests import its sources by relative path.

- Take a temporary directory from `@legion/test-fixtures/temporary-directories`, never from an
  `mkdtemp` with a cleanup of your own. One directory for a whole file or `describe` is
  `temporaryDirectory(prefix)`: call it at the test file's module scope, or in the body of a
  `describe`, never inside a test body. It reserves the path at the call and returns it, and
  registers the `beforeAll` that makes the directory and the `afterAll` that removes it where it is
  called. A directory per test is `temporaryDirectories(prefix)`, registered in those same two
  places, which returns a maker that makes a directory at each call and removes every one of them in
  its `afterEach`. Call that maker from a test body or a `beforeEach`, never from a `beforeAll`: it
  makes its directory at the call, and its `afterEach` removes it after the scope's first row, so
  one a `beforeAll` made is gone for every later row. A directory a whole file or `describe` shares
  is `temporaryDirectory`, which makes nothing until its hook runs. `bun:test` takes a hook's scope
  from the point of the call, and Bun 1.4.4 evaluates a module body and a `describe` body in three
  cases where it then runs no hook that body registered: a `bun test -t <pattern>` that selects no
  row of the file, one that selects no row of the `describe`, and `describe.skipIf(true)`. Nothing
  catches what such a run leaves: under `bun test` neither `process.once("exit")` nor `"beforeExit"`
  runs. So anything a file creates, sets or writes at module scope or in a `describe` body survives
  a filtered run, and a directory must be made where the hooks run. Write into a reserved directory
  from a test body, or from a hook registered below the reservation, since nothing is at the path
  until that `beforeAll` has run and Bun runs a scope's hooks in the order they were registered. A
  `describe` whose rows are each `.skip` runs both its `beforeAll` and its `afterAll`, so a
  directory reserved in its body is made and removed as usual; `describe.skipIf(true)` runs neither,
  so that directory is never made (`packages/test-fixtures/src/temporary-directories.test.ts` holds
  both, and `filtered-run.test.ts` runs a file under a filter that selects nothing and reads its
  `TMPDIR` afterwards). The fixture's own module body registers no hook, and cannot: Bun evaluates
  an imported module once per process, so a hook there would take the scope of whichever test file
  imported the fixture first, and every other file's directories would outlive their tests.
  `temporaryTmpdir(prefix)` (`@legion/test-fixtures/temporary-tmpdir`) is the same shape for a whole
  file's `TMPDIR`: it reserves one root, and makes it and points `TMPDIR` at it in a `beforeAll`
  whose `afterAll` puts the name back and removes the root. Anything staged under that root is
  staged in a hook too, since the name holds the caller's value until that `beforeAll` has run.
  Both forms take `root`, the directory the temporary one is made in, for a test that needs another
  filesystem, such as `/dev/shm` for a rename that must cross one. `memoryRoot({ need })` names
  `/dev/shm` where it is a tmpfs that allows exec, this process may write in and search it, and it
  has `need` bytes free (0 unless given), else `tmpdir()`: a suite that makes or removes many
  entries inside one row's or hook's time takes it as `root`, so a busy shared disk does not set
  that time. A suite that holds much there gives `need`: the most one run holds at once, times the
  runs that may share the host, because a tmpfs is often small (a Kubernetes pod's is 64 MiB),
  keeps what a stopped run never removed, and fails a write past its end with ENOSPC. tmpfs charges
  a page for each file that holds data, so a suite that removes files it has read holds less.
  Both remove a tree a test left read-only: the removal chmods `u+rwX` down the tree first,
  following no symlink out of it and leaving a file's own mode alone, because `rmSync` alone fails
  on the 0555 directories and 0444 files a Go module cache writes, and because a file in such a
  tree can be a hard link to one outside it.

- Put a variable back with `restoreEnvironment` or `captureEnvironment`
  (`@legion/test-fixtures/environment`), which set and delete the names themselves. A name that held
  no value is captured as unset and the restore deletes it again; setting it to the empty string
  instead would leave the next reader a value its caller never had. A file that loads a plugin entry
  captures the names that entry writes above the import that loads it:
  `packages/pi-envoy/extensions/envoy.ts` puts its own `bin/` first on `PATH`, sets `DISPATCH_HOST`
  and picks `ENVOY_CONFIG_FILE` when its module loads, and a capture below that import would put
  back what the entry wrote rather than what the caller had. Never restore by replacing the whole
  object (`process.env = { ...saved }`): that swaps the runtime's environment for a plain one, and
  after it `process.env !== Bun.env`, a write reaches neither `Bun.env` nor a child process the test
  spawns, and the names the test deleted stay deleted in the real environment. Under `--parallel`,
  which implies `--isolate`, the next file gets a fresh `process` and never sees the swap, so the
  damage shows only where it is hardest to read.

- `mock.module` at module scope outlives the file that wrote it: a filtered run that selects no row
  of the file still applies it, Bun 1.4.4 has no API that takes a module mock off again, and every
  later file of the process reads the mocked module (`packages/pi-shared/test/host-registry.ts` says
  what that costs the suites that mock the host package). Prefer a seam the code under test exposes,
  set in a `beforeAll` and cleared in the matching `afterAll`, as `setNatsConnectForTests` and
  `resetNatsConnectForTests` in `packages/pi-envoy/extensions/envoy.ts` are.

- A suite that runs jj or git takes its isolation from `versionControlEnvironment`
  (`@legion/test-fixtures/version-control-environment`): a home of the suite's own with a jj
  configuration file in it, no git configuration at all (`GIT_CONFIG_GLOBAL=/dev/null`,
  `GIT_CONFIG_NOSYSTEM=1`), no credential prompt, and the identity jj commits as. So neither the
  developer's `~/.jjconfig.toml`, nor their `~/.gitconfig`, nor the system gitconfig decides a
  result. A suite whose code under test starts jj with the process's own environment puts those
  names on `process.env` itself, and hands `versionControlNames` to `restoreEnvironment` so the next
  file finds its own again.

- Take a port from the kernel: bind port 0, read back the port the listener reports, and hold that
  listener from the moment it binds, so nothing else on the machine can take the port between the
  question and the row that asks it (`stubBroker` in
  `packages/envoy-client/src/__tests__/broker-stand-ins.ts`). Never write a port number into a
  test. The Dispatch e2e harness is the same rule at the scale of a run: a port variable left unset
  is port 0, one that is set is used as given, and two variables naming one port are refused where
  the ports resolve rather than by whichever listener loses the race (`harnessPort` and
  `harnessPorts` in `packages/dispatch/e2e/harness-ports.ts`).

- A row whose verdict rests on a connection's backpressure bounds what the connection may hold
  itself. On loopback the connection's buffers are the kernel's, and the TCP receive buffer grows to
  `net.ipv4.tcp_rmem`'s maximum: 4 MiB in a gVisor pod, 6 MiB on a Kubernetes node's Linux 6.12 and
  32 MiB by Linux 7.0's default, which CI's runner has. At 32 MiB a paused server let the
  connection take a whole 24 MiB upload, so the client saw no progress and cut it off.
  `socketDispatch` in `packages/envoy-client/src/__tests__/dispatch-keyed-writes.test.ts` reads a
  body only 1 MiB past what its server took.

- A suite that needs a program this host may not have skips with that program's name. Call
  `skipWithout("docker")` once, at the file's module scope, and hand the answer to every
  `describe.skipIf` and `test.skipIf` of the file: it prints one line naming the missing programs
  and the file whose tests did not run (`@legion/test-fixtures/host-tools`), and a call per test
  would print that line per test. A suite that needs a service does the same with the variable that
  names it, and makes a database of its own rather than reading the one it finds
  (`LEGION_TEST_PG_DSN`, `packages/dispatch/e2e/__tests__/psql-session.test.ts`). One that needs
  both builds the two halves into a single list and prints one line, rather than calling
  `skipWithout` beside a warning of its own, so a host that has neither reads one line naming both
  (`hostTool` with the variable, in that file and in `harness-stacks.test.ts`).

- A test under `scripts/e2e/lib` gates on a host program with `hostGate` from the same module
  instead, declaring its gated rows with the gate's own `test` and `describe`: outside CI each
  gated row skips as with `skipWithout`, naming the program, and under `GITHUB_ACTIONS` each fails
  by its own name, naming the program, since the job that runs those tests carries every program
  they need, and a skip there would be a green job with the proof gone (`skipOutsideCI` in
  `packages/daemon/internal/launcher/rolenamespace_test.go` is the same rule). Its `reason` takes a
  sentence of the caller's own (a library such as libpq), and its `never` names programs whose
  absence fails the rows on any host. A single row that never skips for a program calls
  `requireHostTools` in its body instead: the rows that run python3 do one or the other.

- A test that requires a command to have left in place a signal it was started ignoring, as
  `nohup` starts one ignoring SIGHUP and a shell's `&` job ignoring SIGINT, reads the command's
  `SigIgn` mask with `ignoresSignal` (`@legion/test-fixtures/held-process`): a listener for the
  signal clears the bit, so the read shows whether the command undid the ignore. gVisor's
  `/proc/<pid>/status` has no `SigIgn` line, and macOS has no `/proc`, so such a file calls
  `skipWithoutIgnoreMasks(file)` once, at module scope, as it would `skipWithout`, and hands the
  answer to the `test.skipIf` of each row that reads the mask or rests on what is read from it. On
  GitHub Actions on Linux it never skips: a runner there can read the mask, so one that cannot is
  broken, and the rows run and fail rather than pass in silence.

- A Go test of `packages/daemon` that reads a process's `SigIgn` or `SigCgt` mask from
  `/proc/<pid>/status` calls `testkernel.RequireSignalMasks(t, "SigIgn")` (or with `"SigCgt"`)
  first (`packages/daemon/internal/testkernel`). The probe reads the status of a shell that ignores
  SIGUSR1 and catches SIGUSR2, so it finds a line that is there but does not follow the process as
  well as one that is missing: the build cluster's gVisor has no mask line at all. It prints one
  `kernel probe:` line either way, as `requireKernel` in `packages/envoy/cmd/cipher` does, then
  skips the row where the kernel lacks the masks, and fails it under `GITHUB_ACTIONS`. A row whose
  claim a shell can show by behaviour makes that read in the test itself and the mask read in a
  subtest, so the test still proves its claim on gVisor: a non-interactive shell can set no trap
  for a signal it was started ignoring (`startsIgnoring` in
  `packages/daemon/internal/daemon/foreground_test.go`), and a shell started ignoring SIGHUP
  survives the hangup it sends itself (`ghHangupProbe` in `packages/daemon/cmd/legion/gh_test.go`).
  A row that cannot run because the test process itself was started with its signal ignored is a
  machine the row cannot run on too: it prints one `signal probe:` line either way, skips outside
  CI and fails under `GITHUB_ACTIONS`, whose runner starts `go test` with every signal at its
  default (`requireStartedAtDefault` in the same file). A skip there would leave `go test` green
  with nothing proved, since CI runs it without `-v`.

- Start a container with `docker run --rm` and register its removal the moment `docker run` returns,
  not once the whole start has succeeded: a start that throws half-way has already made the
  container (`startNats` in `scripts/e2e/lib/nats-stream.test.ts`). Remove it with `docker rm -f
  -v`, and give it the CPU and memory limits "Containers a test starts" below requires.

- Give a Bun process the test starts under a HOME of its own `BUN_RUNTIME_TRANSPILER_CACHE_PATH=0`,
  unless the row tests that cache. Where neither that variable nor `XDG_CACHE_HOME` names another
  place, Bun keeps its transpiler cache under HOME, so a child in an empty HOME writes a file and a
  rename for each module it caches (Bun caches only files above a size floor) before its own code
  runs: 282 for claude-envoy's channel server. Each is a directory-entry operation on the disk the
  HOME is on, and on a shared disk under load they made such a start take tens of seconds. With
  "0" the child transpiles as before and writes nothing (`NO_TRANSPILER_CACHE` in
  `packages/claude-envoy/tests/transpiler-cache.ts`).

- Two caches stay where their tool puts them, under the user's cache directory, and this is the one
  rule here that holds only under a condition. Oh My Pi's native modules, about 123 MiB a pin, are
  hardlinked into each test's fresh HOME from one copy per binary at
  `$XDG_CACHE_HOME/legion/test-omp-natives`, or `$HOME/.cache/legion/test-omp-natives` where
  `XDG_CACHE_HOME` is unset (`nativesCacheRoot` in `packages/pi-shared/test/omp-natives.ts`);
  `nativesCache` in `packages/daemon/internal/testbin/natives.go` computes that same path with
  `os.UserCacheDir` and `OMPHome` hardlinks out of it, so moving the Bun side alone would split a
  cache that exists to be shared by both languages and either fills for the other. The Go build and
  module caches of the `go run` in `runDaemonPane`
  (`packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts`) stay for the same reason: CI's
  `pi-legion` job restores `~/.cache/go-build` and `~/go/pkg/mod`, keyed
  `go-daemon-<runner os>-<runner arch>-go<version>`, before it runs that scenario
  (`.github/workflows/ci-job-pi-legion.yaml`). Those of `scripts/e2e/lib/held-port.test.ts` stay
  there too, although the `test` job that runs it restores no Go cache now that the daemon's Go
  suite is `daemon-go`'s. The condition is the rename: `fillNativesCache` in
  `packages/pi-shared/test/omp-natives.ts` renames a directory inside the cache root, and a
  directory rename under `$HOME` panics the gVisor sandbox of a Legion pod and kills every role in
  it. So the permission holds only while `XDG_CACHE_HOME` is honoured and the pod points it at its
  own volume, which Legion's runtime does for every role (`/legion/scratch/cache`). With
  `XDG_CACHE_HOME` unset in a pod, that cache is no longer a tool's cache but the pod's death.

- The real-binary harness's stand-in server (`serveStandin` in
  `packages/pi-shared/test/omp-harness.ts`, and claude-envoy's copy of it) takes two pieces of
  `@legion/test-fixtures`. It listens at `loopbackHost()` (`loopback-host`): on Linux an address
  of `127.0.0.0/8` chosen at random and never `127.0.0.1`, as the Go tests' listeners have
  (`LoopbackIP`, above), and `127.0.0.1` elsewhere, with its clients' URLs built from the address
  the server reports (`server.url`). A client of a test that has ended keeps dialing the address
  its own server had, and the kernel gives that port to the next listener on the same address: an
  OAuth client's form POST reached that stand-in on `127.0.0.1` and failed the row that was
  running. On Linux such a client no longer reaches it; elsewhere it still can. And it reads each
  POST body with `readStandinBody` (`standin-body`), which refuses a non-empty body that is no
  JSON object with a 400 naming the request, prints the same line, and leaves the request
  unrecorded, where a parse that throws out of `Bun.serve`'s `fetch` fails whichever test is
  running. A request with a JSON object body, an empty body or no body is recorded and answered as
  the case's own answer decides.

## A Bun test waits for its fact, and its command sets its one guard

- Wait for a fact through `@legion/test-fixtures/wait` (`until`, `settled`), and give the wait no
  duration. Each wait ends at its row's deadline: the newest mark of the row-deadline preload plus
  the command's `--timeout`, less a margin, so that the wait's own failure prints before Bun's
  timeout ends the row (`packages/test-fixtures/src/wait.ts`). A probe that throws ends the wait at
  once, so a probe that reads a report which makes its fact impossible throws. A wait under
  `jest.useFakeTimers()` refuses to start, because those timers hold `performance.now()` and every
  timer still until the test advances them. Outside `bun test`, a Bun driver takes its deadline
  from `LEGION_PROOF_DEADLINE_MS`, epoch milliseconds its script sets, and with neither a wait
  refuses to start.

- A command whose rows wait through it loads the row-deadline preload,
  `--preload @legion/test-fixtures/row-deadline`, as `packages/test-fixtures`' own `test` script
  does. The preload marks the start of each row, the end of each row's body, the end of each row
  and the start of a run, so each wait of a row ends before the row's guard, and so does a wait in
  a `beforeAll` or in a row's first `afterEach`. Without it a wait counts from its own start, and
  its failure says so. A hook that no mark directly precedes counts from an earlier mark, so its
  waits end earlier than its own clock would allow, never later
  (`packages/test-fixtures/src/row-deadline.ts`). So a row's second `afterEach` counts from the mark
  before its first, and the time the first took is gone from it.

- A command sets one guard, its `--timeout`. A row or a hook sets no timeout of its own, neither a
  third argument to `test` or `beforeAll` nor a `setDefaultTimeout`, because the shared wait reads
  the command's guard and not a row's. The exceptions are a row of `packages/dispatch/e2e/__tests__`
  that states its limit through `bounded-subprocess.ts`'s `rowLimit` or through
  `outside-path-reach-fixtures.ts`'s `row` and `probingRow`, and a hook or a row whose work is one
  measured download or one measured process run with no shared wait in it: such a hook or row
  takes a guard of its own, at least twice its slowest reading on a machine that runs the suite
  (CI's, or a dev box's), and the `"//"` note of its package's `package.json`, where the command's
  guards are written, gives that number with the readings and their runs. A file whose rows need a
  much larger guard than the rest runs in a second `bun test` command of the same script, with its
  own `--timeout` and the same preload. Tests written before this rule still set guards of their
  own (`git grep -n setDefaultTimeout -- '*.ts'` lists the files); each moves to its command's
  guard as it is carried to the shared wait.

## A test that looks for a process it started

A test, and each script library that a test runs, reads one process by its pid: its command line
from `/proc/<pid>/cmdline`, its parent, process group and session from `/proc/<pid>/stat`, and its
children from `/proc/<pid>/task/*/children`. `scripts/e2e/lib/process-tree.sh`, which
`scripts/e2e/lib/rig.sh` sources, has `proc_parent`, `proc_group`, `proc_children` and
`proc_describe`. Each of them asks `ps` or `pgrep` only on a host with no `/proc`, but for
`proc_children`, which also asks `pgrep -P` where a process's `task/<tid>/children` file is
missing (a kernel built without `CONFIG_PROC_CHILDREN`).

`pgrep` reads every process on the machine, and so does procps-ng's `ps` with `-p`: one
`ps -o args= -p PID` opened `/proc/<pid>/stat` 12,564 times on the dev box at 12,086 to 13,662
processes. There one `pgrep` or `ps -p` took 0.59 to 0.97 s at a load of 365 to 375, and a read of
one process's file took 1 to 3 ms. A `Bun.spawnSync` holds the test file's one event loop until its
child ends, so each such read also holds every timer of the file, Bun's timeout of the row
included.

When a test waits for a process that the code under test starts, the code tells the test that
process's pid. A command that a test hands to the code, such as a `!` token command or a stand-in,
writes its own pid (`echo $$ >FILE`) as its first act.

## The act rule of a Dispatch unit test

The preload of Dispatch's unit suite (`web/src/__tests__/setup.ts`) fails a row in which React
printed an act message, naming each message with its component and the frames that printed it
(`web/src/__tests__/act-warnings.ts`). Each `web/src/` path in this section is under
`packages/dispatch`.

- Wrap a state update the row causes in `act`, and one that a promise settles in
  `await act(async () => …)`.

- Under a held clock (`jest.useFakeTimers()`), hold the answer to the request a press sends, and
  give it inside `act` once the press is out. A press's own wait ends a few turns after its last
  event, so an answer given at once lands after that wait and updates the page outside `act`.

- A row that dispatches a DOM event itself (`window.dispatchEvent(new Event("scroll"))`, say)
  dispatches it inside `act` when a listener of a rendered component sets React state from it,
  even a state the event leaves as it was. React drops a set of the same value without scheduling
  it only while the component has no update left from an earlier render; when an update of that
  component lands just before the event, the set is scheduled outside `act` and the act rule fails
  the row. Under a lane's load that is rare, and the file alone does not show it:
  `web/src/features/conversation/ConversationTab.test.tsx`'s row "a scroll event measures the
  reader position in O(1) rect reads" failed 1 run in 20 that way.

- `web/src/__tests__/act-warnings.test.ts` runs the act rule's six fixtures,
  `web/src/__tests__/act-rule*.fixture.tsx`. Four draw React's act messages.
  `act-rule-quiet-restore.fixture.tsx` proves that the preload puts back a spy of
  `quietConsoleError()` that a row leaves in place. `act-rule-final-hook.fixture.tsx` proves that a
  row that the network report and the act check both fail fails with one report naming both, and
  that every reset and restore of the preload still runs after it, so the next row starts with
  nothing that row left.

## A Dispatch unit test runs in random order

`packages/dispatch/bunfig.toml` sets `[test] randomize = true`. Bun reads that file only from the
directory it starts in, and `bun run <script>` starts in the package's directory, so the file
reaches `bun run test`, `test:web`, `test:web:cold`, `test:harness` and `test:repeat` from any
directory, and with them CI's "Test dispatch dashboard" step and the weekly repeated run. It also
reaches a `bun test <file>` started in `packages/dispatch`, but not one started from the repository
root or from a subdirectory. Each `web/src/` and `e2e/` path below is under `packages/dispatch`,
and each command runs there.

- To reproduce a failing row, run its file alone with the seed its lane printed in its summary
  (` --seed=<n>`): `bun run test:web web/src/<file> --seed=<n>`,
  `bun run test:web:cold web/src/<file> --seed=<n>` for a row of the web-cold lane, or
  `bun run test:harness e2e/__tests__/<file> --seed=<n>`. Add `--rerun-each=<k>` for a row that
  first failed on run k, which the report of `bun run test:repeat` names. The order of the tests in
  a file depends only on the seed and on that file, so this repeats the order the row failed in. A
  row that failed from the lane's load, such as one that times a real process, or from another file
  that ran beside it, can pass alone: run it with the files that ran beside it, or under the same
  load.

- No flag of `bun test` turns random order off: `--randomize=false` is refused, and
  `--no-randomize` still shuffles. `bun --config=<file> test …` reads that file in place of
  `bunfig.toml`, so with a file that does not set `randomize` it runs in file order: use that to
  read a failure, never to make a row pass. A row is never pinned to an order or skipped to make it
  pass. It is fixed at its cause.

- Read the order a run used from its junit file (`--reporter=junit --reporter-outfile=<f>`), not
  from its console: Bun prints no `(pass)` lines where `CLAUDECODE` or `AGENT` is set.

- `bun run test:repeat --lane <web|web-cold|harness> [--reruns <k>] [--seed <n>] --out <dir>` runs
  one lane with every test run k times (`--rerun-each`), 20 when `--reruns` is left out, and writes
  a report of every row that failed, with how many of its runs failed, its seed and the command
  that runs it again, and of every failure that belongs to no row. A SIGTERM or a SIGINT stops it:
  it stops Bun and reports each test file a worker was in, with the run and the row it was in, by
  the row's place and name. One that comes after Bun has ended, while it writes its report, is
  named in the report beside Bun's results. A worker, or Bun's test process, killed from outside,
  as the out-of-memory killer kills one, is named the same way: each file it was in, with the run
  and the row. `.github/workflows/dispatch-unit-repeat.yaml` runs it weekly, each lane under three
  seeds, under a deadline that sends it that SIGTERM; the step `exec`s `timeout`, so the SIGINT
  that GitHub's runner sends a cancelled step, or one at its budget, reaches it too.

- A rerun of a file keeps every module the run before loaded, so a module-level store or cache
  keeps what that run left in it. Where a row needs such state empty, the module exports a reset and
  the preload (`web/src/__tests__/setup.ts`) runs it after each row (`resetRowReferenceReadings`,
  for the readings and key-list ids `row-references.ts` keeps, runs before each row too). State a
  module keeps across rows on purpose stays, such as the loaded Markdown engine
  (`markdown-engine.ts`). Without that reset, `row-references.test.ts` fails on a rerun: a row that
  counts readings finds texts read, or ids of lists of keys given, by rows that ran before it, and
  counts fewer reads than it expects.

- `bun run test:web:cold` is `test:web` with `web/src/__tests__/hold-lazy-loads.ts` added last to
  its preloads. That file registers its plugin in its `beforeAll`, where the test file's rows
  begin. From then on it holds back each load of every script module that the sources of `web/src`,
  tests aside, load with `import("<literal>")`, the `lazy()` pages and the Markdown engine among
  them, for `DISPATCH_COLD_HOLD_MS` milliseconds of real time, 1000 when unset. So every row that
  runs while such a module is held finds it unloaded, not only the row that random order puts first
  in its file. A load written another way, such as an `import()` of a computed specifier, and a
  style sheet are not held (the head of `hold-lazy-loads.ts` says how it lists them). A module the
  test file imports itself is loaded before its rows begin, unheld; a lazy load that the file's own
  `beforeAll` starts is held. A row that fails there and passes under `test:web` reads before it
  waits; a row that waits and still fails there ran out its wait behind a chain of holds, one lazy
  module loading the next, which a smaller `DISPATCH_COLD_HOLD_MS` tells apart. The weekly
  workflow runs it as the `web-cold` lane with `--reruns 1`: a rerun of a file keeps the modules
  its first run loaded, so only the first run is cold.

- `bun run test:web:late-reads` is `test:web` with `web/src/__tests__/hold-read-answers.ts` added
  last to its preloads. That file wraps the `queryFn` of every TanStack query, so each read's
  answer reaches its cache one turn of the real event loop after its request has answered, while
  what a page draws without a read comes at once. A row that waits for what the page draws at
  once, then reads what a read brings without waiting for it, fails there under every seed, where
  under `test:web` it fails only on a runner loaded enough to split the two commits:
  a row of `settings.test.tsx` read the sidebar's Settings link after the page's heading, and the
  sidebar draws its links only once its inbox, pinned issues and projects have answered. The wait
  is the real `setImmediate`, which a fake clock does not hold, and a cache a row seeds, a mutation
  and a write of the page's store run no `queryFn`, so none of them is held. A row that fails there
  and passes under `test:web` reads before it waits. Waiting fixes none of three other kinds of
  failure, which the head of `hold-read-answers.ts` names: a row that times a read's answer (the
  query client's own rows, a write's 401 path through the sign-in check), a row that ends while a
  read it never reads is out, whose late answer React reports outside `act`, and a page that
  never reaches the state its row waits for, which is a finding about the page. No workflow runs
  the lane: it is for a change to a test that reads what a read draws, and for a search of that
  class.

- A spy or a stub on a DOM method goes on the prototype that owns it, which `Object.hasOwn` tells:
  happy-dom's `Element.prototype`, not `HTMLElement.prototype`, owns `getBoundingClientRect`,
  `scrollTo` and `scrollIntoView`. A restore, Bun's `mockRestore` or an assignment, leaves an own
  copy on a prototype that only inherited the method, and that copy shadows every later spy on the
  owner.

- A `localStorage` method is replaced on the storage itself, as `onLocalStorage` in
  `web/src/features/project/IssuesTable.test.tsx` does, never on `Storage.prototype`: happy-dom
  copies the prototype's method onto the storage the first time it is read, and a spy on the
  prototype after that never reaches it.

- A row that reads a Markdown body or a lazily loaded part in the commit that mounts it first waits
  for it (`warmMarkdownRenderer()` from `web/src/features/refs/markdown-engine.ts`, or a `findBy*`
  query), because the earlier row that loaded it may run after it. A lazily loaded part is anything
  that renders from a module that `import()` or `lazy()` loads, not the Markdown engine alone. A
  row whose wait can end before such a load does warms the module before it mounts: a `waitFor` or
  `findBy*` on a held clock (`jest.useFakeTimers()`) spends its budget in fake time, and a load can
  outlast a `timeout` a row sets below the suite's budget (`web/src/__tests__/setup.ts`). The
  web-cold lane holds each lazy load 1 s, and the engine's loads come one after another, 2 to 3 s
  in all: rows of `reference-titles.test.tsx` and `document-references.test.tsx` that waited 2 s,
  and rows of `SearchPalette.test.tsx` on a held clock, failed there. A test file whose rows mount
  a lazily loaded part that no row reads, such as the shell's margin, imports that module at its
  top: a load that a row starts can arrive between two of that row's `act` scopes or in a later
  row, where React reports a suspended resource outside `act`
  (`web/src/app-sign-out.test.tsx`, `web/src/app.test.tsx`,
  `web/src/__tests__/responsive-shell.test.tsx` and `web/src/features/agents/AgentsPage.test.tsx`
  do).

- A harness test drops a database it made with `dropDatabase` from `e2e/psql.ts`, never with
  `drop database … with (force)`, which refuses a login that is not a superuser, as CI's is not,
  while an autovacuum worker is attached (the doc of `dropDatabase` says why).

## Unix sockets in Go tests

A unix socket path must be 107 bytes or shorter. Linux's `sun_path` holds 108 bytes with the
terminating zero, and Go refuses a longer path with `invalid argument` at `bind`, `listen` or
`dial`. A tmux server socket has the same limit (`Filename too long`). `t.TempDir()` puts the
test's name, cut to 64 bytes, in its path, under `GOTMPDIR` or else `TMPDIR`. With a long test name
and a socket under it, or under a directory made from it (an `XDG_RUNTIME_DIR`, `HOME` or state
directory a test sets to `t.TempDir()`), a temporary directory of about 20 bytes is enough to pass
the limit, as on a dev box whose `TMPDIR` is on a data disk.

- Take a test's socket directory from the module's `internal/testtmp`, the one helper every test
  that binds a socket calls (`packages/daemon/internal/testtmp`, and the same file in
  `packages/envoy`). `testtmp.Dir(t, longest)` is a fresh directory of the test's own, removed
  when the test ends, where `longest` is the longest path the test puts inside it, written
  relative to the directory (`"g9/.s.PGSQL.5432"`); `testtmp.Socket(t, name)` is the path of a
  socket named `name` in such a directory. `Dir` makes the directory under `TMPDIR` and measures
  what `longest` would come to there; only when that passes 107 bytes, or `TMPDIR` holds no
  directory at all, does it fall back to `/tmp`, and it says so with `t.Logf` when it does. Each
  module's `internal/testtmp` keeps a test that fails a new `os.TempDir()`, or a new
  `os.MkdirTemp` whose first argument is a string literal naming a directory, anywhere in that
  module's tests. It reads no other shape: a directory named by a variable or a constant, or an
  `os.CreateTemp`, is left to review.
- Two kinds of site keep a directory of their own on purpose.
  `packages/daemon/internal/shim/socketpath_test.go`'s `roleTempDir` builds a deliberately long
  one, as long as the `TMPDIR` a Legion role is told, so that its test proves sockets still bind
  where a role runs; a short directory would take the proof away. The once-per-binary
  directories a package's `TestMain` owns (the build directories of `internal/daemon`,
  `internal/stream` and `internal/runtime/tmux`, the jj configuration directory of
  `cmd/legion/main_test.go` and the home of `internal/workspace/workspace_test.go` in the daemon
  module; the sign-in secret directory of `internal/cipher/config` and the certificate directory
  of `infra/seed/main_test.go` in the envoy module) bind no socket, so they need no byte budget,
  and no `*testing.T` outlives them, so they keep `os.MkdirTemp("", …)` and a removal in that
  `TestMain`.
- A test changes a process variable, `TMPDIR` above all, only through `t.Setenv`, and a test
  that must unset one calls the module's `fixtureenv.Unset`, which goes through `t.Setenv`,
  never a bare `os.Setenv` or `os.Unsetenv`. `t.Setenv` puts back what the process held when the
  test ends, and Go refuses it in a parallel test or under a parallel parent, while a parallel
  test runs only beside other parallel tests, so no test can read a value another test changed.
  A bare call has neither fence.
- Before you call a test that binds a socket done, run it with a temporary directory of about 64
  bytes. A pass under `/tmp` does not show that the test works under a longer one. Set `GOTMPDIR`
  as well, since `t.TempDir()` reads it first:

  ```sh
  L=/tmp/a-deliberately-long-tmpdir-for-unix-socket-path-tests-0001 && mkdir -p "$L"
  TMPDIR="$L" GOTMPDIR="$L" go test ./<package>/... -count=1
  ```

## A test in `internal/dispatch/api`, `internal/dispatch/docs` or the daemon's `internal/intake` runs beside the others

Every top-level test of `packages/envoy/internal/dispatch/api`,
`packages/envoy/internal/dispatch/docs` and `packages/daemon/internal/intake` calls `t.Parallel()`
as its first statement, and each package's `TestMain` sets how many run at once: 8 in api, 6 in
docs, 4 in intake. Each test already has a database and a server or document service of its own
(`storetest.Open`; in intake `testpg.Clone` and a NATS server of `testnats`' pool, one per test that
holds one), so run alone it spends most of its time waiting on Postgres while the machine's other
cores sit idle.

A test stays serial when it, or a helper it calls, changes or measures something the whole test
process shares: the default logger (`logtest.SetDefault`, `slog.SetDefault`), the environment
(`t.Setenv`), a package variable, flags, the GC or `GOMAXPROCS`, heap statistics or goroutine
counts, or pmdoc's schema manifest, display tree version or block ID generator
(`pmdoc.UseManifestForTest`, `pmdoc.UseStagedBlockForTest`, `pmdoctest.UseBlockSchema`,
`pmdoc.SetDisplayTreeVersionForTest`, `pmdoc.SetBlockIDGenerator`). go test runs every serial
test before it starts the parallel ones, so such a test runs alone. A serial test whose reason is
not a call in its own body or a helper of the package's says why on its first line: a wall-clock
bound that missed under parallel load, or a write through another package's test hook.

A test that reads `pg_locks` or `pg_stat_activity` names its own database in the predicate
(`database = (select oid from pg_database where datname = current_database())`, or
`datname = current_database()`). The tests beside it hold the same tables, OIDs and lock keys in
databases cloned from the same template, so a predicate on a relation or a lock key alone counts
their waiters as its own.

A test beside seven others shares the runner's four cores with them, so a test that proves two
calls run at once, or that a request does not wait on something, by how many milliseconds it
took, fails on a correct product whenever the machine is busy. Prove it by the order of events
instead, with the test binary's deadline as the only bound (`testwait.Receive`): hold each call at
the stand-in until all of them have arrived, or hold the thing the request must not wait on until
after its response. A product bound the test does not mean to race takes a value past any run
through a field of the server that only a test sets, as `server.mentionLookupBound` and
`server.duplicateBound` do; a package variable would change it for the parallel tests beside it
(`comment_delivery_sender_test.go`, `TestMentionLookupsOfOneCommentRunAtOnce`).

A test in `internal/dispatch/docs`, and a helper it calls (`storetest`, `lockstep`, `docstest`),
registers its cleanups with `testcleanup.Add(t, f)`, not `t.Cleanup(f)`. testing keeps a finished
parallel test, and every closure its cleanups captured, until the package's last parallel test
ends, so a document service, a room of a 400 KB document or a peer that a cleanup shuts down stays
live for the rest of the run; `testcleanup.Add` lets go of it once the cleanup has run. Under the
race detector every live byte costs about three (the heap and the detector's shadow of it), and
envoy-go-race-docs fails its step when the binary passes 8 GB
(`.github/workflows/ci-job-envoy-go-race-docs.yaml`).

A docs test stays serial, and says so on its first line with what it measured, when run alone it
adds far more memory than the others: a few hundred megabytes or more of live heap after a
collection, about three times that under the race detector. Beside the package's other tests
such a test would take the binary toward the step's limit.

## Go's package deadline

`go test` panics a package that runs longer than ten minutes (`panic: test timed out after 10m0s`),
which reads as a failure of the change under test. `packages/envoy/internal/dispatch/api` alone
takes close to that, about 600 s on a workstation, so every command that runs that package, on its
own or in a pattern that includes it (`./...`, `./internal/dispatch/...`), passes `-timeout 20m`:

```sh
cd packages/envoy
go test -timeout 20m ./internal/dispatch/api
```

A Legion issue pod does not run that whole package: `skills/legion-worker/references/tools.md` says what the pod runs and where the rest goes.

CI runs that package under `-race` in `envoy-go-race-api`, the job that runs it by itself, which
gives it 70 minutes (`.github/workflows/ci-job-envoy-go-race-api.yaml`, which holds that job's
steps); `envoy-go` leaves it out of its unit step, and `text-checks` runs its fifteen source-guard
tests by name. `docs` runs under `-race` in `envoy-go-race-docs`, which gives it 60.
A command that runs one of its tests by name (`-run`) needs no deadline of its own. Run it alone
rather than beside the module's other Postgres-backed packages: together they share one database
service and the machine's CPU, and on a small machine `api` and `docs` then each overran the default
(`docs/solutions/testing/the-dispatch-servers-go-suites-in-an-issue-pod-run-api-and-docs-alone-with-a-timeout-and-read-the-c-collation-docker-and-bun-failures-as-the-environment-LEGION-752.md`).

Every package of `packages/daemon` runs in CI in exactly one leg that `.github/go-legs.yaml` names
for it (or under `elsewhere`, with the job that runs it). When you add, remove, rename or move a
daemon package, change its line there in the same change: every daemon-go leg fails, naming the
package, while `go list ./...` and the file disagree. Put a new package in the leg its measured
time fits, longest first; `.github/ci-notes/ci-job-daemon-go.md` says how the legs are balanced.

`packages/daemon/internal/daemon` is the daemon's slow, Postgres-backed package. CI runs it only
under the race detector, once per run, in the race leg `.github/go-legs.yaml` gives it, whose step
is "Test the concurrent packages under the race detector" (`.github/workflows/ci-job-daemon-go.yaml`,
job `daemon-go-race-legs`), beside the plain legs; the plain step ("Test the Go daemon") leaves it
out, and `go vet -tags e2e ./...` in the `typecheck` job still compiles it without the detector. The
figures below were measured while these steps were the `test` job's. On CI's runners, while the
plain step still ran it, it took at most 625 s there over the 49 green main `test` jobs of
2026-11-01 to 2026-11-03, and up to 733.581 s in the jobs whose race step timed out (job
125688799071). Under `-race` it took up
to 570 s over the 51 green `test` jobs of 2026-11-02 and 2026-11-03 that sjawhar/legion#2709
measured, on main and on pull requests, and ran go test's own ten minutes out on main at
job 125691048644 of run 41562777668; the race step gives it 30 minutes. On a busy machine it
takes longer still (908 s under `-race` on a shared devbox), so a command that runs the whole
package, alone or in a pattern that includes it, passes `-timeout 30m`:

```sh
cd packages/daemon
go test -race -timeout 30m ./internal/daemon/
```

A Legion issue pod does not run that whole package either: `skills/legion-worker/references/tools.md` says what the pod runs and where the rest goes.

## A cleanup's closure or argument holds nothing that reaches its object

A `runtime.AddCleanup` or `runtime.SetFinalizer` closure captures, and its argument carries, nothing that reaches the object the cleanup is attached to.
`runtime.AddCleanup` says what that costs: "If ptr is reachable from cleanup or arg, ptr will never be collected and the cleanup will never run."
The runtime keeps a cleanup's closure and its argument alive for as long as the object it is attached to, so either of them reaching that object makes the object immortal, and everything the object holds with it.
In `packages/envoy/internal/dispatch/docs` the object is a room's live document, and two closures held the service and one of its listings, each of which reaches every resident document through ygo's room table:
every room the tests loaded then stayed in memory with its replica and its trees, which is what took the `envoy-go-race-docs` job over its runner's memory (dispatch://INFRA-1085).

Hold the owner through a `weak.Pointer` instead, and do nothing when its `Value()` is nil, as that package's `keepInstance` (`roomcursor.go`) and `listResident` (`tree.go`) do: the cleanup drops its entry while the owner is alive, and an owner that is gone has no map left to drop it from.
Hold weakly what the argument would carry as well, and pass `struct{}{}`, which reaches nothing, as `keepInstance` does with the room load its cleanup once took as its argument.
A pointer to a field of the owner counts as the owner, since a field and the value holding it are one allocation.
`weak.Make` takes such an interior pointer and that weak pointer dies with the whole allocation, which is how `listResident` holds the listing it was given, `Service.instances` or `Service.replicas`.

Go's own detector finds a closure that reaches its object:

```sh
cd packages/envoy
GODEBUG=checkfinalizers=1 go test -count=1 -timeout 34m ./internal/dispatch/docs/
```

It prints `WARNING: LIKELY CLEANUP/FINALIZER ISSUES`, then one `is reachable from cleanup or cleanup argument` line per site naming that site's file and line, and ends the run with `fatal error: detected possible issues with cleanups and/or finalizers`.
The rule covers the cleanup's argument as well as its closure, so an argument that reaches the object is the same defect: the room load `keepInstance` passed reached a document of its own, which is the finding this job made on a head where the closure itself was already weak, and the comment on that cleanup names the path it took (dispatch://INFRA-1085).
A run that reports nothing is no proof on its own: the check runs at a garbage collection, so a short run can pass with the path present.
`GOGC=10` beside the detector makes a single test likelier to fire it, not certain, so repeat the run before you read a quiet one as a clean one: with this fix reversed, `TestALiveWritesAskBlockKeepsItsWriterWhenAnotherProcessSettlesIt` under `GOGC=10 GODEBUG=checkfinalizers=1` fired in 2 of 3 runs for the implementer and in 1 of 6 for the reviewer, and at the fixed head it was quiet in 9 of 9.
CI runs it in `envoy-go-race-docs` (`.github/workflows/ci-job-envoy-go-race-docs.yaml`), on the same line as the race detector, so one run of the package's tests serves both detectors, and on the package's line of `envoy-go`'s step "Test what the race build cannot run at all or at full size", which runs plain the tests the race build leaves out or shrinks.
The `envoy_go_race_docs` and `envoy_go` filters in `.github/ci-filters.yaml` select them.
Both are slow suites: a pull request runs them only with the label `full-ci` or that suite's `lane:` label, and the merge queue runs them on the tree the change lands as, which is where their result normally comes from (`.github/ci-notes/ci.md`, "Which lane a job runs in: the pull request or the merge queue").

The race run streams the test binary's own output (`go test -json`) and so carries a second check Go's detector does not make:
the package's `TestMain` prints `docs: <built> services built, <alive> still alive after <duration>` once the tests have run, and the line after it names each test that built a service still alive at the end of the run.
A test that builds a `*Service` registers a cleanup that shuts it down, with `testcleanup.Add`.
A service a test left running holds its rooms, and their documents, for the rest of the binary's run.
A service that was shut down is still alive at the end of the run when a callback of one of its stopped timers holds it.
`Timer.Stop` does not free the callback: the runtime keeps a stopped timer in the heap of the P that armed it until that P next clears its stopped timers or the timer's own time comes, and an idle P clears them only when it next runs, which depends on the machine's load.
A settlement timer's callback therefore holds the service and the room's state weakly (`scheduleSettleAfterLocked` in `packages/envoy/internal/dispatch/docs/service.go`): held strongly, a stopped one kept its shut-down service for up to an hour under a test's `settle = time.Hour`, and the check failed on a loaded machine in nearly every run (dispatch://INFRA-3904).

`packages/envoy/internal/dispatch/docs` is the only package of this repository that registers a cleanup today, and no repository-wide check holds that true:
`grep -rn "runtime.AddCleanup\|runtime.SetFinalizer" --include=*.go .` finds those two sites and nothing else.
A `runtime.AddCleanup` or `runtime.SetFinalizer` added in another package puts `GODEBUG=checkfinalizers=1` on the go test line that already runs that package's tests, in the same pull request, rather than a job that runs them again; `.github/scripts/ci_job_reads.py`'s `CLEANUP_DETECTOR_PACKAGES` names the packages whose lines carry it.

## A database a Go test creates

A Go test that creates a Postgres database writes `create database … strategy file_copy`, as
`packages/envoy/internal/dispatch/store/storetest`, the daemon's `internal/testpg` and Envoy's
`internal/testpg/testdb` do. Without
it, PostgreSQL 15 and later copy the source database through the write-ahead log, every page of it:
4.4 MB of log for an empty database made from `template1`, and 6.4 MB for a clone of Dispatch's
migrated template, which made one run of the `api` and `docs` packages write 16 GiB of log. A file
copy writes a few hundred bytes. `storetest`'s own test fails when a clone writes more than a
hundredth of its template's size, and each module's `internal/textcheck` test
`TestEveryCreatedDatabaseIsAFileCopy` fails on any line of the module's Go files that opens a
database's creation in a string without `strategy file_copy` on the same line.

In Envoy, a test that already holds a server's URL makes its database with
`internal/testpg/testdb`'s `DatabaseOn`, never `internal/testpg`'s: `testpg` links
testcontainers, whose package init walks every process on the machine before any test runs, and
on a box running some 26,000 processes that cost a test binary about 9.8 seconds of start
(`GODEBUG=inittrace=1`). `testpg.URL`, which may start a container, keeps that cost for the tests
that need it. The broker's `storetest` holds itself to it: `TestStoretestLinksNoContainerRuntime`
fails when its dependencies reach testcontainers.

A test that needs the schema takes a clone of a template its package migrated once, rather than
running the migrations itself: `packages/daemon/internal/testpg`'s `Clone` answers the clone's
URL, `ClonePool` an open `pgxpool` on it and `CloneStore` the opened store, each dropped when the
test ends, and the package's
`TestMain` is `testpg.Main("<prefix>", m.Run)`, which drops the template when the run ends and
composes with the `TestMain` a package already has
(`testpg.Main(prefix, func() int { return testnats.Main(m) })`).
`packages/envoy/internal/dispatch/store/storetest`'s `Open` is the same fixture for Dispatch's
schema.
`testdb.Database` is still there for a test that wants an empty database: `internal/record`'s own tests
apply part of the migration set by hand and assert what `Migrate` then returns, which a migrated
clone would not let them do.
`internal/store`'s own tests take that same `Database`, from the package
`packages/daemon/internal/testpg/testdb` that holds it: `internal/testpg` imports `internal/store`
to migrate the template its `Clone` copies, so a test binary of `internal/store` cannot import
`internal/testpg` back, and the half of the fixture that needs no schema sits in `testdb`, which
imports no package of the module except `internal/testwait`, which imports none, and so is open to
every package of it.
Each of those tests runs beside the others on databases and pools of its own, four at a time unless
the command line names a `-parallel` (`storeTestParallelism`). A pool writes its refusals, and
`Transact`'s lines about a commit over that `*Pool` itself, to its own log (`Pool.log`), so a test
reads its own lines there (`captureLog`) and never swaps the default logger. `Transact`'s lines
about a commit over any other `TxPool`, a test's wrapper of a `*Pool` included, go to the default
logger.
`internal/daemon`'s tests boot daemons, which migrate the database they are given: each test's
daemons write in a clone of the package's template (`testDSN`), so a boot there finds the schema
in place, and the tests run beside each other on databases of their own. So do `internal/workflow`'s,
each driving its engine over a clone of its own (`migratedPool`), four at a time unless the command
line names a `-parallel` (`workflowTestParallelism`).

Dispatch's store package cannot import `storetest`, which imports it, and its migration tests each
need the schema as it stood at some earlier version. Its own tests take
`openMigratedTestStore(t, version)` (`store_test.go`): a clone of a template migrated through that
version. A run keeps one template for each version its tests ask for, each built from the nearest
template below it, and the package's `TestMain` drops them all when the run ends. A test that
moves its clone further forward applies only the migrations past its version (`migrateAbove`),
not the whole history again.

A pgx pool a daemon test opens holds four connections, unless `LEGION_TEST_PG_DSN` sets a size of
its own (`pool_max_conns`), which it keeps. pgx sizes a pool with no size of its own at the greater
of 4 and the machine's CPU count, so on a large machine one test could ask for more connections
than PostgreSQL's default 100 allow, and pass only on a small one. `testdb.PoolBounded` gives a
URL the pool keys `LEGION_TEST_PG_DSN` sets, or `pool_max_conns=4` where it sets no size;
`testdb.Database`, `testpg`'s `Clone`, and the `testDSN` of `internal/daemon` and
`internal/store` answer such URLs. A URL that libpq, `pgx.Connect`, `psql`, a scratch Dispatch or
another child process reads takes `testdb.PlainDatabase` or `testdb.DatabaseURL` instead, which
hold none of pgxpool's keys even when `LEGION_TEST_PG_DSN` sets one: `pgx.Connect` sends such a
key to the server, which refuses it, `psql` refuses the URL, and Dispatch refuses a URL that sets
`pool_max_conns`.

A test that needs one of envoy's binaries takes it from
`packages/daemon/internal/testbuild`'s `EnvoyBinary`, which builds each command once per test
process and removes the directory when `testbuild.Main` ends the run.

A test changes no server setting (`fsync`, `full_page_writes`, `max_wal_size`): the server
`DISPATCH_TEST_DATABASE_URL` names can hold other databases, such as the local Dispatch's on
`packages/envoy/scripts/dev-postgres.sh`'s container.

## A Go test that opens a NATS client

The daemon's NATS tests share one server per test binary, and `packages/daemon/internal/testnats`
hands it to one test at a time (`URL`, `JetStream`). A test that wants a client of its own takes
it from `testnats.Connect(t, url)`, which names it after the test and closes it when the test
ends. A test that dials the shared server itself leaves a connection nobody named.

The fixture reads the server's client connections when a test takes it and again when that test
ends, so a connection that was not there at the start and is still there at the end fails the
test that left it, naming each one by the id the server gave it and the name its client carries
(`packages/daemon/internal/testnats/leakcheck.go`). A client the fixture opened it also closes,
so the next test still gets the server empty; one it never opened — a child process's, or a raw
`nats.Connect` — it records, and the next test to take the server fails at once naming the test
that left it, because nothing in this process can close another client's connection.

The daemon's own production clients are named too: `natsauth.Connect` takes the name as a
parameter rather than an option, so no call site can leave one empty, and an operator reading
`/connz` on the Envoy NATS sees which process holds each connection.

## A test that deletes a KV watcher's consumer

A KV watcher reads its bucket through an ordered consumer nats.go creates and keeps in memory, and a test that deletes that consumer, to stand for one the server lost, has a state that does not last.
nats.go puts a new consumer in its place once the lost one's idle heartbeats have been missing for a whole check: 5 s heartbeats, checked every 10 s, so nominally 5 to 20 s after the delete (nats.go v1.55.0 `js.go`, `orderedHeartbeatsInterval`, `hbcThresh`, `activityCheck`, `resetOrderedConsumer`); those are timers, so a loaded machine moves them later, never earlier.
A check that reads "the consumer is gone" some time after the delete therefore passes on a quick machine and fails on a loaded one: `TestTheListenerShutdownEndsItsKVWatchersBeforeTheDrain` once required the WARN a watcher's end writes for a consumer it finds gone, and failed when the listener's shutdown took 18 s to reach that end, which then deleted nats.go's replacement with nothing to warn of.
While the replacement's create is in flight, after the server made it and before the client read the answer, nats.go has not yet named it to the subscription, so a stop in that moment deletes nothing (`js.go` `deleteConsumer`); nats.go deletes the new consumer once the answer comes and it finds the subscription closed (`resetOrderedConsumer`), but in a goroutine nothing waits for, so a connection closed first leaves it for the server to remove once its interest is gone.
A replacement the server lists may still be in that moment in the client.

So such a test holds the state it means to test, rather than racing nats.go to it.
To test code that meets the consumer gone, delete it with `testnats.HoldConsumerLost` (`packages/envoy/internal/testnats/ordered_loss.go`): it sends the client, until the test ends, the idle heartbeat the server would, carrying the consumer's own sequences, so nats.go reads the subscription as alive and replaces nothing, however long the code under test takes; the client must have taken every message the consumer delivered, since a heartbeat naming one it has not taken reads as a gap (`js.go` `checkForSequenceMismatch`), and the bucket must take no write meanwhile.
`TestADrainEndsTheKVWatchersBeforeTheConnection` and the `consumers already gone` row of `TestTheListenerShutdownEndsItsKVWatchersBeforeTheDrain` do; with a plain delete and a 25 s stall before the end, the first failed 2 of 2, and with the hold it passed 2 of 2.
The heartbeats are the test's own timers, so a stall of a process past a whole check can still let nats.go replace the consumer. `ConsumerHold.Replaced` says whether it did, from the server's record of the creates (it follows `$JS.API.CONSUMER.CREATE.<stream>.>` from before the delete, so a stall loses none), and a test requires the loss's own outcome only where it did not, and only for that consumer. A create counts only where it is the lost consumer's own replacement: nats.go makes that one from the lost one's configuration, by start sequence at the sequence after the last it was delivered, with the same filter, so another consumer made on the stream by start sequence does not count.
To test code that meets nats.go's replacement, wait until the server lists it (it starts at the lost one's next sequence, `DeliverByStartSequencePolicy`; `awaitReplacedConsumer` in `packages/envoy/cmd/listener/drain_watchers_test.go`), and do not require the bucket empty afterwards, since the replacement may have been in flight.
Either way, read what the code says it did (`kvwatch.Watcher.End` logs `<name> watcher ended` once every stop it made has returned, whatever it found).
A NATS that falls silent for a few seconds loses the client's connection, and the code then meets none of the consumers the test arranged: a drain finds the connection reconnecting, or a reconnect makes every watcher again first. Read which from a record independent of the code under test: the bus's `envoy nats disconnected` and `envoy nats reconnected` lines, in order (`mainConnection` in the same file). Still lost at the drain (a disconnect with its error after the last reconnect), require only what the code says of a lost connection. A disconnect with no error is a close, which leaves a lost connection lost: nats.go reports one whenever the connection has a socket, and a reconnect to a frozen server has one, since the kernel still accepts the dial. Made again before the drain, require what any run requires, the end of each watcher and the buckets empty, and drop only the outcomes of the arrangement the reconnect replaced: a test that reads any earlier disconnect as "lost" lets a real leak after a reconnect pass.
A count of what is left reads a listing that reports its own failure (jetstream's `ListConsumers` or `ConsumerNames` and their `Err`), never nats.go's older listing, whose channel closes on a failed request as on an empty stream.

## A test that reads `GET /api/v1/references?since=` reads again

A since read answers only the edges at or below the event log's safe horizon (`getReferences` and
`sinceHorizon` in `packages/envoy/internal/dispatch/api/references.go`), and waits at most
`referencesSinceWait`, one second, for a horizon above the events committed before the read. Past
that wait it answers at the horizon it has and leaves the rest to a later read: the route defers,
it never loses an edge. Only an event write of Dispatch's own database holds the horizon back
(`packages/envoy/internal/dispatch/events/horizon.go`, "Costs"): a transaction in another database
on the server, or one of Dispatch's database that writes no event, holds nothing. In
`packages/envoy/internal/dispatch/api` the holders are the test's own event writes and those of
the server under test, which a loaded machine can keep open past that second.

So a read that answered within `referencesSinceWait` was not held back: what it listed is what it
owed, and a wrong answer fails the test at once. Only a read held past that wait is read again,
until the answer is whole (the wait of the next section, run as a watch in the test's own process,
`horizontest.NewWatchInProcess`). The helper is `awaitHorizonRead`, with `awaitSinceRead` for a
since read (`packages/envoy/internal/dispatch/api/references_test.go`); it takes the read as a
function, so each call site keeps its own reader. The event log route answers through the same
horizon (`sinceHorizon`), and its tests read it with the same helper (`events_log_test.go`).

A test that takes one read as final fails under an event write held past that second:
`TestAnAnsweredAsksPartEdgesAreListedSince` failed about one run in six on main, 7 of 40 measured
for dispatch://INFRA-592, and in this repository's own pod on 2026-11-02 it failed 15 of 40 runs
with a transaction held 2 s of every 2.5 s; under that same pulse, reading again, it passed 40 of
40. Those runs were measured while any transaction on the server held the horizon back.

Prove such a test with the horizon held: hold an event write of the test's own database open with
`holdEventWrite` (`packages/envoy/internal/dispatch/api/event_horizon_helpers_test.go`), and call
the `release` it returns on a timer longer than `referencesSinceWait` and shorter than
`horizontest.OwnHoldBound`, 5 s. `holdEventWrite` ends the hold only on that release, at the
test's first failure or at its end, never on a clock of its own. A transaction in another
database, such as a `psql` loop that runs `select pg_current_xact_id(), pg_sleep(2)`, no longer
holds the horizon, so it proves nothing. The re-read's wait (`horizontest.NewWatchInProcess`)
fails on a hold of the test's own database that has stood `OwnHoldBound`, as an own-database hold,
and names it; a hold past `referencesSinceWait` that the timer releases inside that bound is one
the re-read waits out.

## A test that waits for the event feed's first frame

The rule above bounds a *re-read*: the route answers at the horizon it has, so the test asks again.
A test waiting for the event feed's `events_start` has nothing to ask again.
That frame is the last thing a new subscription sends, not the first: `feedEvents` writes it only once `events.Horizon.Await` has returned
(`packages/envoy/internal/dispatch/api/live_events.go`, `event_stream_relay.go`), and every event write of Dispatch's database holds the horizon back until it ends, so the frame is as late as the longest event write open there: a test's own fixture, or the server's own writes on a loaded machine.
No fixed deadline bounds that, and a deadline raised to fit the worst run anyone has seen is the same flake on the day a write is held for a minute.

So no wait on the horizon has a clock of its own.
The wait is `horizontest` (`packages/envoy/internal/dispatch/horizontest`), an ordinary package so that the three packages whose tests wait on a horizon share it.
The event feed's frames are read on a goroutine that sets no read deadline, because a gorilla connection is corrupt after a read timeout.
In `api` the same wait covers every other wait behind the horizon: a live event, the relay's publishing and a subscription reaching it, the delivery hub's event feed and the deltas it drives, a persisted horizon, and a pass of the suggestion outcome sweep on a log that holds events, which reads only up to the horizon and awaits it on its first pass (`suggestion_outcomes_test.go`; the sweep's own settle is waited out first, on the database's clock, so the wait is on the horizon alone).
A sweep over an empty log has nothing above its horizon to wait for, so `sweep_runners_test.go` waits for its fact with `testwait` (no horizon wait, and no bound but the test binary's deadline), and the two-process sweep row in `cmd/dispatch` keeps its own bound (measured under a 45 s and a 75 s hold: both pass at once).

`cmd/dispatch`'s `waitEventsStart` (`live_events_wait_test.go`, which `openLiveEvents` calls) waits on what the Dispatch process says about itself (`horizontest.Wait`).
`events.StallWarning` is the message `Horizon.warnStall` logs, every `warnEvery`, while a pending observation is held back, naming the holder's kind, pid, database and query; the wait reads it out of the process's output (`horizontest.Lines`), and while the warning keeps arriving the wait goes on.
Count the warnings, never whether one was seen: a check that latches on the first warning waits for ever.
`cmd/dispatch`'s `multiprocess` row that waits on another process's event, on its own process's relay, polls the same way over that process's output (`pollBehindHorizon`).
Such a wait still ends in a named failure, rather than holding the run until the package's timeout; the bound is a signal rather than a clock.
It fails when no warning at all has arrived within a base bound, when no new warning has arrived within an extension window, when a warning names the test's own database for a holder that has held the horizon back at least `horizontest.OwnHoldBound` — a client of Dispatch's own database holding its readers back that long is the defect, not a stall to wait out — and a drain margin before the run's own deadline, so the failure prints rather than being cut off by the package timeout.
Silence alone does not fail it: the warnings come from a goroutine of the horizon's that a loaded machine can put off while an event write still holds the horizon, so at the base bound or the end of an extension window the wait reads the database's oldest event writer itself, with the query the diagnosis runs (`events.StallHolderSQL`), through a connection of its own (`horizontest.Dialed`), and fails only when no event write holds the horizon; a holder it finds counts as a warning.

`internal/dispatch/api` and `internal/dispatch/routes`, whose servers run in the test's own process, wait with `horizontest.WaitInProcess`, `PollInProcess`, `AwaitPersisted` and `NewWatchInProcess`, which read no warning.
Such a server's horizon logs to `slog.Default`, which every test of the process shares, and a test that changes the default logger cannot run beside the others (the rule above, "A test in `internal/dispatch/api` runs beside the others").
Nor does silence end such a wait: when the machine is merely slow to run the server, nothing holds the horizon — a relay the CPU has not run yet has no observation pending on an event write, so the horizon logs nothing and the database shows no event writer — and beside seven other tests that silence outlasted the base bound (dispatch://INFRA-3904): `TestEventStreamRelaySubscribeRacingTheLastCancelReceivesTheNextEvent`'s next event did not arrive in 3.251 s, the horizon having stopped warning 3 s before, and the replaced-log row of `live_events_horizon_test.go` failed at the base bound on a box at load 650-860.
So the wait reads the database's oldest event writer itself, through the server's own pool, from the base bound on, again an extension window after each holder it found, and no more often than every `horizontest.Poll`.
It ends only when what it waits for arrives, when a holder it read is a client of the test's own database that has held the horizon back at least `horizontest.OwnHoldBound`, or at the drain, where it fails naming what it waited for and the newest holder it read.
Under `go test -timeout 0` there is no drain, and such a wait has no end, as `testwait.Until`'s has none.

Each failure names the newest warning or holder, so the holder is in the output; the own-database failure also names that client's `application_name` and pid, because a transaction in that database can be anyone's — an operator's `psql`, another test's fixture — and not only Dispatch's own.
The base bound and the extension window are each three times a measurement taken under load or read from the server's own constants; `events/horizon.go`'s `horizonDiagnosisTimeout` is in the window between two warnings, because `warnStall` logs after its `pg_stat_activity` diagnosis.
The other two numbers are neither: `horizontest.Poll` (250 ms) is the rate at which a wait samples the warnings, and the most often a wait in the test's own process reads the holder, below a tenth of the extension window so it costs that window nothing, and `horizontest.Drain` (30 s) is what the wait leaves of the run's own deadline so that its failure prints instead of being cut off by the package timeout, which would name no test.
`horizontest.OwnHoldBound` (5 s) is three times a measurement of its own: sampling `pg_stat_activity` every 100 ms over 10 runs of `cmd/dispatch`'s `events_start` test under the load below, in this repository's pod on 2026-11-10, `nproc` 2, PostgreSQL 18.6, 202 samples caught a transaction holding a transaction id in a `dispatch_test_*` database, across 27 backends, all of them carrying no `application_name` — which is what Dispatch's own pools look like and what the test's own fixtures look like too, so the samples do not say whose the transactions were — and the worst age was 1.449 s.
The bound is read off the warning itself, so the wait needs no clock of its own: the shorter of the warning's `pending_for`, the age of the oldest pending observation, and the age of the holder it names, its `xact_start` against the warning's own time. `pending_for` alone is not the holder's: an earlier holder released since can have carried the oldest observation past the bound while the holder now named began a moment ago. A hold shorter than the bound extends the wait. A holder the wait read itself holds for its own age, on the database's clock.
Short thresholds are why the Horizon runs one diagnosis at a time: at a `warnEvery` below that diagnosis bound, several scans at once would queue for the two connections of the horizon's own pool, ahead of the observation the waiting reader needs.

The 2 s pulse above does not prove such a test: a hold shorter than the wait's own bound delays no `events_start` past it.
These waits were proved while a transaction in any database on the server held the horizon back, under a rolling hold in another database, a new 25 s transaction every second, held so that it was still open when the socket subscribed and not merely when `go test` started: the test builds its own binary inside its own clock (`cmdtest.Build`, from `startDispatchProcessWith`), which is a second on a warm build cache and minutes on a loaded machine, so a one-shot hold started before the command can be spent before the socket exists.
Measured in this repository's own pod on 2026-11-10, `nproc` 2, under 8 busy loops at four times `nproc`: under that rolling hold the old 10 s deadline failed all five runs it was measured over, and the wait on the signal passed 50 of 50.
Such a hold in another database delays nothing now. What a wait rides out is an event write of the server's own database shorter than `horizontest.OwnHoldBound`; one that stands longer fails the wait by design.

## A document test that waits for a room's appends

`settleCurrentGeneration` (`packages/envoy/internal/dispatch/docs/service_test.go`) waits for a room's live updates to reach its persistence and land, and then settles the room.
That wait is a queue.
A room's appends go through the service's room-appends pool of 8 connections, and a test whose subtests run in parallel drives one room per CPU on a single service.
So the wait is as long as that queue, which grows with the machine's CPU count, its load and the other tests running beside it.
A fixed 10 s clock on it failed `TestSetAskBlockTextRefusalOfEveryBrokenBodyLeadsToAValidDecision` (2026-11-16, 288 CPUs).
At the timeout all 8 connections were in use, with 1449 s of acquire waiting in total, and the same wait issued again returned at once.

`awaitRoomAppends` therefore waits on the room's idle signal and sets no span of its own, so the bound is a signal rather than a clock.
It fails when the appends have stopped: no append lands anywhere in the service, and the room's own counts do not change, for `appendStall`.
That bound is 16 s, three times the longest such stall measured: with it lifted to 10 min, the package as CI runs it (`GODEBUG=checkfinalizers=1 go test -v -count=1`, 288 CPUs, load 230 when it began, 2026-11-16) had 927 of these waits go more than 1 s with no append landing and the room's counts unchanged, 231 more than 3 s and 22 for 5 s or more, and the longest went 5.363 s.
The 9 s of `ShutdownDrainBudget` it was before, the time Shutdown gives the rooms' appends to land, was 1.7 times that stall.
It also fails 30 s before the run's own deadline, so that the failure names the wait.
Two scratch tests checked it, each with the 10 s clock and with this wait:

- 24 rooms whose appends were each held 4 s on their connection settled in about 12 s. The clock failed 8 of the rooms in each of 2 runs, and this wait passed 3 of 3 with the 9 s bound and 1 of 1 with the 16 s one.
- An append that another transaction's hold on the document's lock kept from landing failed both: the clock at 10 s, and this wait at 9.1 s with the 9 s bound and at 16.0 and 16.2 s with the 16 s one, naming the stall and the room's counts.

## A test that reads a model request's messages

Oh My Pi sends its date/cwd reminder (`<system-reminder>\nToday: …`) with a model request: ahead
of a user turn that is new since the session's previous request, or, when the local date or the
working directory has changed since then and the request holds no such turn, as a message of its
own after the request's newest message, where it stays in every later request. A run that crosses
midnight therefore sends requests that a run within one day does not, and a test that takes a
request's last message for the one that started its turn, or reads a message by its place or its
exact text, fails for the date alone. The stall watch's restart row did, in merge-group run
44963211024, whose case ran across midnight UTC: the turn ran, and its request ended with the
reminder.

- Read a stand-in's requests through `ownMessages` and `newestMessage`
  (`packages/pi-shared/test/omp-harness.ts`), which give the messages as the session sent them,
  without the reminder in either form, and tell a side turn with `isSelfCheck`, which reads the
  newest of them. The daemon's live proofs skip the reminder message with `isDateCwdReminder`
  (`packages/daemon/internal/daemon/claim_compaction_live_test.go`).
- A plugin's own context hook needs neither: it runs before the provider transform that adds the
  reminder (`agent-loop.ts` of the pinned Oh My Pi).
- Prove such a read with the date moved inside the host: start omp with `TZ=Etc/GMT+12`, and set
  `process.env.TZ = "Etc/GMT-14"` from an extension of the pane at the moment the row needs. The
  two zones are 26 hours apart, so the local date moves on whatever the time of day, and Bun
  applies a TZ set at run time.

## A writer a test holds

A test that needs a live socket's writer held, so that frames queue behind it while the test
sends the socket more frames, holds the writer itself: it builds its rig on a `heldWrites`
listener (`packages/envoy/internal/dispatch/api/live_socket_test.go`), calls `hold`, and waits
with `waitHeld` until a write waits at the gate. Small TCP buffers and a page that reads nothing
(`smallSendBuffers`, `smallReadDialer`) stall the writer only until the transport lets its write
through, and the transport decides when that is. Under gVisor, the runtime of Legion's worker
pods, a write blocked for a third of a second completed the moment the page sent its next frame.
`TestARepeatedSubReplacesAnotherKind` then waited for a queued frame the writer had already
written, and failed 8 of 300 runs. Those two helpers are for tests of a slow page itself, such as
one whose queue overflows.

## A fake HTTP server whose client gives up on requests

A fake `httptest` server whose handler fails the test on a request body it cannot read stops with `Shutdown` before `Close` when its client sends requests the test does not wait for, as the Cloud SQL connector does when it gives up on a refresh.
With `Close` alone, the stop can close a connection whose request headers the server has read but whose handler has not started: the handler then runs on the closed connection, reads the body as `use of closed network connection`, and fails the test for nothing the code under test did, as `TestTheWriterTokenFileIsReadAgainAtEachExchange` failed in CI's daemon-go (job 136636895793).
`stopServing` in `packages/daemon/internal/sessionproxy/connector_test.go` is that stop, and its comment says why it calls `Shutdown` before `Close` and where net/http checks Shutdown's mark; `TestStopServingRunsNoHandlerOnAConnectionItCloses` beside it fails with `Close` alone.
A fake whose handler fails the test on every request keeps `Close` alone, so a request whose headers the server has read by then still fails the test.

## A test that starts jj or git names a working directory

jj searches upward from the directory it runs in for the closest `.jj` and opens that repository, and it has no ceiling option.
`-R` names the workspace a command acts on, and the two that create a repository, `jj git clone` and `jj git init --colocate`, take no `-R` at all.
`-R` does not keep jj off the repository its working directory sits in: jj 0.45.1 loads that repository's configuration before it applies `-R`, and `jj -R <b> log` run inside a repository `a` that still held a legacy `.jj/repo/config.toml` migrated it, writing `a/.jj/repo/config-id`.
Every `-R` call site of `packages/daemon/internal/workspace` is safe because its `Dir` **is** its `-R` target, not because the flag fences jj in: `runCheckedOnClone`'s comment states that invariant, where the directory a command runs in is chosen.
A child process started with no working directory of its own inherits the test process's, which in a Legion issue pod is inside `$LEGION_WORKSPACE`, a jj workspace of the pod's real shared clone under `/legion/repos`.
So a test whose jj child names no directory opens and writes that clone, and the same test is green on a GitHub runner, whose checkout is git-only with no `.jj` anywhere above it (dispatch://INFRA-623).

- Set `Dir` on every `exec.Command` a test starts that runs `jj`, and on every one that runs `git`, to a directory the test made.
  A `--git-dir` is not enough on its own: it tells git which repository to act on and leaves the child standing wherever the test process stands, which is what the next reader of the command has to work out again.
  `Dir` is not enough on its own either, and this is the step the rule stopped short of: an inherited `GIT_DIR` names the repository git acts on whatever directory the child stands in, so it outranks `Dir` and `-C` alike, and a Legion issue pod hands every process such a block.
  So a test that starts git takes those variables out of the child's environment as well as placing it: `fixtureenv.Guard` in the package's `TestMain` for a child that inherits `os.Environ()`, or `fixtureenv.Isolated` for one given an environment of its own.

- The two probes that got this wrong were copies of one helper, now `testbin.RequireJJFilters` in `packages/daemon/internal/testbin`, each running `jj config list` with no `Dir`.
  A probe is as much a jj child as a fixture is.

- A test that drives `packages/daemon/internal/workspace` needs nothing of its own: `execRunner.Run` refuses a `Command` whose `Dir` is not absolute, and refuses a jj command that creates a repository when that directory, or any above it, holds a `.jj` directory (`refuseEnclosingWorkspace`).
  `TestProvisionLeavesTheEnclosingRepositoryUntouched` holds both on a runner, by provisioning from inside a decoy repository.

- A test that builds a repository of its own puts it under `t.TempDir()`, and `TMPDIR` must sit inside no checkout.
  Nothing refuses the fixtures' own `jj git init --colocate` (`runSetup`, `enclosingRepository`): it runs outside the runner, where `refuseEnclosingWorkspace` never sees it, so under a `TMPDIR` inside a checkout it would read that checkout's repository's configuration first, and migrate it when that repository still holds a legacy `.jj/repo/config.toml`.

## A test that starts jj or git inherits no pod's environment

A Legion issue pod hands every process its live workspace (`LEGION_WORKSPACE`), its git configuration (`GIT_CONFIG_*`), its GitHub credential, the deployment's own session store (`OMP_SESSION_SQL_DSN_FILE`, `OMP_SESSION_STORAGE`) and more.
A test that let git or jj inherit them read and pushed the pod's own work: the shim's rescue test, run with no `LEGION_WORKSPACE` of its own, pushed the pod's workspace to the real origin as `legion/WIDGETS-42-rescue-…` branches.

- A `packages/daemon` package whose tests run git or jj, as children or in-process through the code under test, calls `fixtureenv.MustGuard` in its `TestMain`, after any branch where the test binary runs as a helper child.
  MustGuard is Guard with a `TestMain`'s only answer to a failure: it prints it and ends the process with status 1, which `packages/daemon/internal/fixtureenv`'s own test holds, with the guarantee Guard gives every package (`fixtureenv.RequireGuarded` stays for the test that needs the check in its own body).
  Such a package is also a line of `guardingPackages` in `packages/daemon/internal/fixtureenv/guardingpackages_test.go`, which fails when a listed package's `TestMain` calls no `MustGuard`, calls it only inside a branch, runs the tests before it, or runs them under a branch whose condition does not say a helper-child marker is set (`os.Getenv(<name>) != ""`, or `if _, ok := os.LookupEnv(<name>); ok`), since every ordinary run of the package takes such a branch too.
  It also fails a `TestMain` that a build constraint keeps out of the default `go test` build, and a package of the module whose `TestMain` calls `MustGuard` while the list does not hold it, so adding a package to the guard adds it to the list.
  No test can see another package's process, so that list is the one check of every package's guard.
  Guard unsets every `LEGION_*`, `GIT_*`, `JJ_*`, `GH_*` and `GITHUB_*` variable, `SSH_AUTH_SOCK` and `SSH_ASKPASS`, and the three names that place an Oh My Pi session (`OMP_SESSION_SQL_DSN_FILE`, `OMP_SESSION_STORAGE` and `OMP_SESSION_SQL_DSN`, which carries the same URL by value), keeping the tests' own inputs (`LEGION_TEST_*`, `LEGION_E2E*`, `GITHUB_ACTIONS`, and the names the package passes), and sets `GIT_ALLOW_PROTOCOL=file`, which a repository's own `protocol.<name>.allow` cannot override.
  The session store a pod names holds the conversation of every agent of the deployment, so a test that opened it would read and write what those agents are reading and writing.
  Code under test that names its own transport, as provisioning's `GIT_ALLOW_PROTOCOL=https` does, still uses it.
  Guard also takes off the process's `PATH` every directory whose `git` is the image's git wrapper (`/opt/legion/bin/git`, or a link to it), which leads a pod's `PATH`: that git puts Legion's leak guard in front of every push, and that jj refuses a push whose git is any other, so a fixture's push to a local bare repository under the test's empty home was refused in a pod and passed on CI, whose `PATH` holds no such directory.
  It is the process's `PATH` that matters, because `exec.Command` and `exec.LookPath` look a bare name up on it whatever `PATH` the command's `Env` holds.
  Guard does the same under the `e2e` build tag, whose untagged tests are the same fixtures.
  A live proof of that build, which reaches a real cluster and GitHub on purpose, calls `fixtureenv.Live(t)` first in its entry point: that gives the test the process's own values back of everything Guard took out or set, until the test ends.

- A fixture command runs under `fixtureenv.Isolated(home)`: the same variables dropped, a home of the test's with its config and state homes (`XDG_CONFIG_HOME`, `XDG_STATE_HOME`) under it, and an identity.
  The state home is the test's because the leak guard caches public visibility answers there, and a test on the runner's would read answers other tests cached.
  A package whose code under test reads the process's home sets `fixtureenv.Home` on the process and gives its fixtures that same home (`packages/daemon/internal/workspace`'s `setupEnvironment`).
  Isolated keeps no test input either, so a fixture that needs one appends it after.

## Containers a test starts

Every container a test or a proof script starts carries a CPU limit and a memory limit sized to its
work, and is removed when the test stops, except kind's node containers (`scripts/e2e/README.md`
says why) and the Pulumi containers of `packages/envoy/infra/host/live_test.go`, whose released
`Listener` component has no CPU or memory inputs. Tests and the live proofs run on shared dev boxes
as well as on CI runners, and an unlimited server can take every core and all the memory of a host
that other work shares.

- In Go, set `Resources.NanoCPUs` and `Resources.Memory` in the container's host configuration:
  through `testcontainers.WithHostConfigModifier` for a `Run`, or as the request's
  `HostConfigModifier` for a `GenericContainer` (`packages/envoy/internal/testpg/testpg.go` is one).
  `WithHostConfigModifier` replaces any modifier an earlier option set, so a test that needs a host
  configuration of its own sets the limits in that same modifier.
- In a script, `docker run` takes `--cpus` and `--memory` before the image. A Postgres whose data
  directory is a `tmpfs` mount counts that data against its memory limit. `scripts/e2e/README.md`
  lists each proof's containers and their limits.
- Remove a container with its anonymous volumes: `docker rm -f -v`, since the Postgres images
  declare a `VOLUME` that a plain `docker rm -f` leaves on the host.

## A CI job that failed with no log at all

A job that stored no log did not hang: it lost its runner, and the one artifact that outlives the log says so.

GitHub stores a job's log when the runner finalises it.
A runner the service stops hearing from never finalises, so the logs endpoint answers `HTTP 404` with `<Code>BlobNotFound</Code>`, where a job that was cancelled, that failed and that passed each answers its log.
The check run's annotation is written by the service and not by the runner, so it is still there:

```sh
gh api repos/<owner>/<repo>/check-runs/<job id>/annotations
```

It answers one annotation at `annotation_level: failure`, in these words:
"The hosted runner lost communication with the server.
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

A message broker that gives at-least-once delivery may hand one message to a handler twice. NATS
JetStream is this repository's broker. Four facts decide when it hands one over a second time. The
comment on `ConsumerSpec.HandlerLimit` (`packages/daemon/internal/intake/consume.go`) is where the
mechanism is written whole, with the measurement behind each part.

- The intake consumer asks for one message at a time, so no pull is outstanding while a handler
  runs and nothing reaches the consumer then. A reconnect, which pulls again, is the one exception.
- The progress keeper prevents a second delivery while each of its reports reaches the server
  before the acknowledgement window lapses: each such report starts the window again.
- A late signal lets the message fall due again. A late report lets it fall due while the handler
  still runs. A late acknowledgement, which goes out only after the handler returns, lets it fall
  due after that return.
- A message that fell due is delivered again, and that redelivery races the acknowledgement or nak
  the handler's return sends. Either side can win. Each measurement is of one condition: with the
  progress keeper removed, the redelivery reached the consumer in 12 runs of 20; with a handler
  step that ignored its cancelled context and held the message past the window, the redelivery won
  all 15 runs on an idle machine and lost to the nak only under load. Neither is the rate under a
  keeper whose reports arrive in time, nor under a cut whose step honours its cancelled context
  while its reports and its nak all reach the server inside the window: the message then never
  falls due, and the nak alone decides when it comes back. A nak the server works first resets the
  redelivery timer, so the event comes one nak delay later instead.

Every counter a handling moves then moves a second time: a statement counter, a request counter, a
call counter in a fake. That second move is the broker's business and no part of what the code
promises, so a test asserts no exact count of such work. Assert the effect the handler leaves
behind instead: the record written, the fact applied once, the row a repeated handling does not
add. A repeat is then invisible to the test, because the handler's effect is the same either way.

Two kinds of row keep an exact count.

- A count is sound behind a window far above the handler's own work and above the trip its
  acknowledgement makes. The window is a hang guard then, and not a bet on timing: a redelivery
  the count would see needs the whole window to pass before that acknowledgement reaches the
  server. `TestAnEventTheDaemonCannotActOnIsAcknowledgedWithNoTransaction`
  (`packages/daemon/internal/intake/no_transaction_test.go`) sets `spec.AckWait` to
  `testwait.Remaining(t)`, what is left of the test binary's deadline less 30 s, over a
  handler whose own work is about `consumerSpec`'s 200 ms on a loaded runner. The margin is what
  this carve-out rests on: that work, and the acknowledgement's trip to the server, which the
  campaign of dispatch://INFRA-1113 measured at 516.8 ms at its largest, come to about 717 ms
  together, far below the window. The window is no fixed span, because the same window bounds the
  handler's store reads, and a fixed one, a minute say, cuts a read of 61 s and lets the machine
  decide the count. The progress keeper is no part of it. Its period is a third of the window
  (`spec.AckWait/3` in `consume.go`) and that handler acknowledges in about 200 ms, so the keeper
  never reports for that message at all. The carve-out ends where the margin closes, and a reader
  finds that out by measuring the handler's work and the acknowledgement's trip again, not by
  reading this sentence.
- A row whose subject is the broker's own delivery behaviour must count deliveries, because the
  count is what it proves. Such a row needs a window above the measured distribution of the first
  report of the progress keeper, the goroutine that reports a running handler in progress so that
  the server does not make its message due again. A window above the handler's own work is not
  enough. Until the keeper has reported once, the window alone stands between the held message and
  the server making it due again, and the count sees whatever repeat follows.
  `TestAFactHeldPastItsAcknowledgementWindowIsDeliveredOnceAndPreparedOnce` and its control
  `TestAFactHeldPastItsAcknowledgementWindowIsDeliveredTwiceWithNoProgressSignal`
  (`packages/daemon/internal/intake/ackprogress_test.go`) are of this kind.
  This carve-out rests on the row's subject being the broker's delivery behaviour: the count is
  the claim, so it needs the sized window whatever it is read from. A delivery count the daemon
  records is no way out of that, since such a count moves with the same late signals, and the
  daemon logs one: `delivered`, on `an intake fact outlived its acknowledgement window and was held
  in progress while it ran` and on `an intake fact was delivered again and dropped as already
  applied` beside it. The carve-out would end only where that behaviour can be seen through an
  effect that does not itself move with delivery timing. There is no such effect today, those two
  lines included.

The comment beside such a count names the window and the measurement that sized it. A later reader
changes the window, the handler or the keeper's period, and that comment is what tells the reader
which number the count rests on.

The rule is measured. `TestAnEventTheDaemonCannotActOnIsAcknowledgedWithNoTransaction` asserted an
exact running total of the statements that take the fact lock. It failed in CI with `after its
checks settlement the fact lock was taken 3 times, want 2: once for each event a record takes` (job
126264090148 of run 41723103641, 2026-11-03). `ApplyFact`
(`packages/daemon/internal/intake/facts.go`) takes the fact lock before `record.Store.MarkProcessed`
deduplicates the event, so a second handling of a message whose fact is already applied costs one
more lock statement. It writes no `processed_events` row and runs no handler. JetStream gives
at-least-once delivery and the intake gives exactly-once application. The row asserted the first.

The second handling happened because the progress keeper was starved. `keepInProgress`
(`packages/daemon/internal/intake/ackprogress.go`) reports the handler in progress on a
`time.Ticker` that ticks every third of the acknowledgement window. A ticker drops a tick its
goroutine was not scheduled for, and by its own documentation it sends only after each tick, so the
keeper reports nothing during the first period of a window (Go 1.27.3). Measured at a 200 ms window,
whose period is 66.7 ms: the largest interval from the keeper's start to its first report was
403.7 ms, which is 6.06 times the period, and the 99th percentile of that interval was 276.8 ms.
That late first report is the late signal above. At the 200 ms window the row failed 2 of 1000
runs in one campaign and 7 of about 2274 in another, which is 0.2 and 0.31 per cent. At the
committed one-minute window it passed 800 of 800.

Four issues record the same fault. dispatch://INFRA-456 was the daemon preparing one Dispatch issue
event's repository more than once under redelivery, which is why `keepInProgress` exists.
dispatch://INFRA-1471 was two rows of the intake counting work the broker repeats.
dispatch://INFRA-1530 built the keeper itself. dispatch://INFRA-1113 measured the mechanism above
and wrote this rule.
