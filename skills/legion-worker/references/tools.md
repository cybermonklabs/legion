# Getting the tool your work needs

Read it before you run a build or tests in your pod, read a shared database, reach for a tool the image may not carry, or measure Oh My Pi's binary, and when a test fails in your pod.

## What runs in your pod, and what runs on the pull request

- The issue's roles share one pod memory limit. In a gVisor pod, where the deployment runs the launcher's memory guard (it is on by default), the guard SIGKILLs near that limit the largest process that is not Legion's or an agent's, usually your command, and tells your role why; a faster fill ends every role's turn. In a microVM pod the pod's own kernel stops such a command instead (an agent too, where the pod requests less than its limit), and your role is told the same way.
- In the pod: compile what you touched, run the tests you added or changed by name (`go test -run '<TestName>' ./internal/<package>/`, `bun test <files>`), build the program a check drives, and `go vet -tags e2e ./...`. For the race detector prefix `CGO_ENABLED=1`; the image sets it to 0.
- A whole module's test run (`go test ./...`), a whole Postgres-backed package, a browser suite run whole, and `make -j` or `lake build` go to CI on the pull request. Any other whole test suite, or a build of a whole repository, goes where your deployment's instructions say.

## The disk your pod works on

- One volume at `/legion` holds the clone, every workspace, `$TMPDIR` and the caches, shared by the issue's roles. Your home and `/tmp` are on the node's small disk, whose limit evicts the pod. Read free space with `df -h /legion`.
- A cache trimmer sweeps at 85 per cent used toward 70 (the defaults), oldest first. It deletes only `$TMPDIR/go-build<digits>` directories, `-a` and `-d` entries of `/legion/go/build`, directories directly under `/legion/scratch/cache` but `ms-playwright` and `legion-claude-code`, and directories directly under `/legion/scratch/bun/install/cache`. It skips anything open or changed in the last fifteen minutes, and deletes nothing a role put anywhere else.
- `$TMPDIR` is yours to clean: nothing else deletes what you leave there, and it fills the volume for every role. Delete the paths you made, each by its exact path.
- Chain a command that enters a scratch directory with `&&` (`cd -- "$TMPDIR/probe" && ./run.sh`): after a failed `cd`, `;` runs the command where the shell last stood, often the shared workspace.

## The database your tests use

- Never give a test, a migration or anything that creates or drops a database the URL in `OMP_SESSION_SQL_DSN_FILE`: it is the live sessions database every pod shares.
- Give `LEGION_TEST_PG_DSN`, `DISPATCH_TEST_DATABASE_URL` and `BROKER_TEST_DATABASE_URL` the value of `$LEGION_TEST_PG_DSN`, your pod's own server. Most suites make their own database per test. These migrate the database the URL names, so give each a database of your own: `packages/envoy/internal/cipher/store` and `packages/envoy/internal/dispatch/{store,delivery,redeliver}`, the Dispatch browser suite's `DATABASE_URL` and the stage scripts' `LEGION_E2E_PG_DSN`. Make one with `createdb -h 127.0.0.1 -p 5433 -U legion_test "e2e_$(date +%s)"`.
- Empty a database by dropping your own schemas and objects, never `public`, which takes `vector` with it.
- A connect to your pod's server refused or failed, or `LEGION_TEST_PG_DSN` unset: read `skill://legion-worker/references/tools-testpg.md` before you retry, start anything or report.

## Reading a shared database

Read a database outside your pod through the read-only login whose URL is in the file `SESSION_READER_DSN_FILE` names. Never read it through the session URL the file `OMP_SESSION_SQL_DSN_FILE` names. Keep read-only inside one transaction (`BEGIN READ ONLY; … COMMIT;`, or `psql "$URL" --single-transaction -q -X -v ON_ERROR_STOP=1 -c 'SET TRANSACTION READ ONLY' -f <file>` with no `COMMIT` or `ROLLBACK` in the file, since one would end the read-only transaction early): a session-wide setting stays on the pooled backend and fails the next client's writes.

With no `SESSION_READER_DSN_FILE`, read nothing of a shared database through the session URL. Read your deployment's instructions for the route they give, and when they name none, report it as a fault of the deployment.

## What the image carries, and a tool it lacks

Read `skill://legion-worker/references/tools-image.md` before you reach for a tool the image may not carry, and before you install, download or vendor anything: it lists what the image carries, the pinned runner forms for the rest, and what an image change carries.

## Before you measure Oh My Pi's binary

Read the source at the fork tag `.omp-pin` names (`git ls-remote --tags https://github.com/cybermonklabs/oh-my-pi.git 'v<version>'`), and compare `"$LEGION_OMP_PATH" --version` with the pin: the pod's binary can be another build. Say which build each figure came from.

Search that source by the name of a construct or a setting, never by its value: one value is spelled in many places, and the code that sets it may spell it in none. If the module the binary names is absent at that tag, the binary is the ground truth: measure it, and say the source did not carry it.

## The tests your pod cannot run

Cite CI's run at the head for these and name each package the pod could not run. A failure not listed is yours to explain.

Check the result, never the request. A call's exit code, a reported limit and a job's phase each answer a different question than the one you asked, so where you set something, read it back and refuse on a mismatch, and where you read something, confirm the tool ran. An empty result is evidence only then: a missing binary, a refused permission or a truncated search gives you silence that reads as a clean answer, so check that the thing you counted had something to count.

- Read what your work wrote, never a Kubernetes Job's phase: `Succeeded` means the container exited 0, and `Running` can mean zero pods under a full quota (`kubectl get pods -l batch.kubernetes.io/job-name=<job>`). A Job's harness detects the failure in the text its tool prints and exits non-zero itself, rather than passing a wrapper's exit code on, and writes a verdict that matters to durable storage as it is produced, not at the end: a node can vanish soon after its pod ends.
- gVisor lacks `SO_PEERPIDFD` and some terminal ioctls, so `cmd/cipher`, `cmd/cipher-helper` and `internal/cipher/helper` of `packages/envoy` fail; `internal/dispatch/pmdoc`'s timing tests read `+Inf times`.
- Before you walk a path with `openat`, trust a tool's binary, set a stack limit, or report a duration measured in the pod: read `skill://legion-worker/references/gvisor.md`.
- No Docker: testcontainers panic. `DISPATCH_TEST_NO_CONTAINERS=1` skips `internal/dispatch/files`. For the daemon's NATS tests run one server per test binary and set both variables:

  ```sh
  go run github.com/nats-io/nats-server/v2@v2.15.1 -js -p 14222 -m 18222 -sd "$TMPDIR/nats" &
  export LEGION_TEST_NATS_URL=nats://127.0.0.1:14222 LEGION_TEST_NATS_MONITOR_URL=http://127.0.0.1:18222
  ```

  Each test's reset and leak check read the monitor port, so a test that leaves a client connected fails. Pick ports no sibling role holds, and keep the store directory off your home.
- Never export `GOWORK=off` in `$LEGION_WORKSPACE`: the daemon's module resolves `../envoy` through `go.work`.
- `internal/dispatch/pmdoc`'s browser-parity tests, which run `bun run` in its `gen/` directory, need `bun install` to have run for `@legion/proof-editor`.
