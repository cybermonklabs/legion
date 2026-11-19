# Getting the tool your work needs

Read it before you run a build or tests in your pod, read a shared database, reach for a tool the image may not carry, or measure Oh My Pi's binary, and when a test fails in your pod.

## What runs in your pod, and what runs on the pull request

- The issue's roles share one pod memory limit. In a gVisor pod, where the deployment runs the launcher's memory guard (it is on by default), the guard SIGKILLs near that limit a process that is not Legion's or an agent's, the one growing fastest first (`docs/kubernetes.md`, "What it ends"), usually your command, and tells your role why; a faster fill ends every role's turn. In a microVM pod the pod's own kernel stops such a command instead (an agent too, where the pod requests less than its limit), and your role is told the same way.
- In the pod: compile what you touched, run the tests you added or changed by name (`go test -run '<TestName>' ./internal/<package>/`, `bun test <files>`), build the program a check drives, and `go vet -tags e2e ./...`. For the race detector prefix `CGO_ENABLED=1`; the image sets it to 0.
- A whole module's test run (`go test ./...`), a whole Postgres-backed package, a browser suite run whole, and `make -j` or `lake build` go to CI on the pull request. Every other heavy run goes where your deployment's instructions say.

## The disk your pod works on

- One volume at `/legion` holds the clone, every workspace, `$TMPDIR` and the caches, shared by the issue's roles. Your home and `/tmp` are on the node's small disk, whose limit evicts the pod. Read free space with `df -h /legion`.
- A cache trimmer sweeps at 85 per cent used toward 70 (the defaults), oldest first. It deletes only `$TMPDIR/go-build<digits>` directories, `-a` and `-d` entries of `/legion/go/build`, directories directly under `/legion/scratch/cache` but `ms-playwright` and `legion-claude-code`, and directories directly under `/legion/scratch/bun/install/cache`. It skips anything open or changed in the last fifteen minutes, and deletes nothing a role put anywhere else.
- `$TMPDIR` is yours to clean: nothing else deletes what you leave there, and it fills the volume for every role. Delete the paths you made, each by its exact path.
- Chain a command that enters a scratch directory with `&&` (`cd -- "$TMPDIR/probe" && ./run.sh`): after a failed `cd`, `;` runs the command where the shell last stood, often the shared workspace.

## The database your tests use

- Never give a test, a migration or anything that creates or drops a database the URL in `OMP_SESSION_SQL_DSN_FILE`: it is the live sessions database every pod shares.
- Give `LEGION_TEST_PG_DSN`, `DISPATCH_TEST_DATABASE_URL` and `BROKER_TEST_DATABASE_URL` the value of `$LEGION_TEST_PG_DSN`, your pod's own server. Most suites make their own database per test. These migrate the database the URL names, so give each a database of your own: `packages/envoy/internal/broker/store` and `packages/envoy/internal/dispatch/{store,delivery,redeliver}`, the Dispatch browser suite's `DATABASE_URL` and the stage scripts' `LEGION_E2E_PG_DSN`. Make one with `createdb -h 127.0.0.1 -p 5433 -U legion_test "e2e_$(date +%s)"`.
- Empty a database by dropping your own schemas and objects, never `public`, which takes `vector` with it.
- Connection refused: the launcher runs `legion-testpg ensure` every 15 seconds, so wait and retry, and never start a second server. Still refused: report the error, the port and `tail -n 200 "$LEGION_TEST_PG_LOG"` as a fault of the pod. A connect with no `connection received` log line never reached the server: open fewer connections at once.
- `LEGION_TEST_PG_DSN` unset, with `legion-testpg` on `PATH`: `export LEGION_TESTPG_DIR=$XDG_DATA_HOME/testpg LEGION_TEST_PG_DSN='postgres://legion_test@127.0.0.1:5433/legion_test?sslmode=disable'`, then `legion-testpg ensure`.

## Reading a shared database

Read a database outside your pod through the read-only login whose URL is in the file `SESSION_READER_DSN_FILE` names. Never read it through the session URL the file `OMP_SESSION_SQL_DSN_FILE` names. Keep read-only inside one transaction (`BEGIN READ ONLY; … COMMIT;`, or `psql "$URL" --single-transaction -q -X -v ON_ERROR_STOP=1 -c 'SET TRANSACTION READ ONLY' -f <file>` with no `COMMIT` in the file): a session-wide setting stays on the pooled backend and fails the next client's writes. With no reader URL, report it as a fault of the deployment.

## What the image carries

- `command -v psql redis-cli sqlite3 nc dig unzip yq legion-testpg` shows which your digest has. `yq` is mikefarah's v4, not Debian's Python `yq`.
- `command -v gcloud bq az mise tmux busybox` shows the cloud CLIs and rig tools. `gsutil` is not on `PATH`; `az` is a wrapper that defaults `REQUESTS_CA_BUNDLE` to Debian's roots; run `busybox <applet>`, and download nothing with `busybox wget`, which checks no certificate.

## A tool the image lacks

1. Run it pinned through a runner the image carries: `uvx --from <package>@<version> <command>`, `bunx --package <name>@<version> <binary>`, `go run <module>@<version>`, or for a library `uv run --with <package>==<version> --no-project <program>`.
2. Take the package's name from the project's own documentation, never from a registry search: `bunx biome@0.3.3` runs a different package than `bunx --package @biomejs/biome@2.6.3 biome`.
3. Never vendor a binary, write a substitute or put an extraction on your own `PATH`. A tool no runner reaches is an image change: tell the architect the tool, why you need it and the runner forms you ruled out.
4. An image change carries the new name to every site an existing client's two names appear at (`git grep -n -e redis-cli -e redis-tools`), the smoke step of `packages/daemon/docker/worker.Dockerfile` included. Add it to the `toolchain` row of `packages/daemon/internal/capabilities/image.go` only when every deployment's image must carry it: the probe refuses an image without it.

## Before you measure Oh My Pi's binary

Read the source at the fork tag `.omp-pin` names (`git ls-remote --tags https://github.com/cybermonklabs/oh-my-pi.git 'v<version>'`), and compare `"$LEGION_OMP_PATH" --version` with the pin: the pod's binary can be another build. Say which build each figure came from.

## The tests your pod cannot run

Cite CI's run at the head for these and name each package the pod could not run. A failure not listed is yours to explain.

- Read what your work wrote, never a Kubernetes Job's phase: `Succeeded` means the container exited 0, and `Running` can mean zero pods under a full quota (`kubectl get pods -l batch.kubernetes.io/job-name=<job>`). A Job's harness detects the failure in the text its tool prints and exits non-zero itself, rather than passing a wrapper's exit code on, and writes a verdict that matters to durable storage as it is produced, not at the end: a node can vanish soon after its pod ends.
- gVisor lacks `SO_PEERPIDFD` and some terminal ioctls, so `cmd/agent-secrets`, `cmd/agent-secrets-helper` and `internal/broker/helper` of `packages/envoy` fail; `internal/dispatch/pmdoc`'s timing tests read `+Inf times`.
- Before you walk a path with `openat`, trust a tool's binary, set a stack limit, or report a duration measured in the pod: read `skill://legion-worker/references/gvisor.md`.
- No Docker: testcontainers panic. `DISPATCH_TEST_NO_CONTAINERS=1` skips `internal/dispatch/files`. For the daemon's NATS tests run one server per test binary and set both variables:

  ```sh
  go run github.com/nats-io/nats-server/v2@v2.15.1 -js -p 14222 -m 18222 -sd "$TMPDIR/nats" &
  export LEGION_TEST_NATS_URL=nats://127.0.0.1:14222 LEGION_TEST_NATS_MONITOR_URL=http://127.0.0.1:18222
  ```

  Each test's reset and leak check read the monitor port, so a test that leaves a client connected fails. Pick ports no other role holds.
- Never export `GOWORK=off` in `$LEGION_WORKSPACE`: the daemon's module resolves `../envoy` through `go.work`.
