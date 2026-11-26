# Builds, tests and tools in your pod

Every role of your issue runs in one pod and shares its memory limit. Near the limit the launcher's memory guard stops the process that grows fastest and tells your role why. In the pod run a compile of what you touched, the tests you added or changed by name (`go test -run '<TestName>' ./internal/<package>/`, `bun test <files>`) and `go vet -tags e2e ./...`. A whole module's tests, a whole Postgres-backed package, a browser suite and a large build run in CI on the pull request, or where your deployment's instructions send heavy work.

Put large files under `$TMPDIR`, which is on the issue's volume; your home and `/tmp` are on a small disk. Delete only the paths you made.

## The database your tests use

`LEGION_TEST_PG_DSN` names your pod's own Postgres. Never give a test the URL in `OMP_SESSION_SQL_DSN_FILE`: it is the live sessions database. Make a database of your own for a suite that migrates the one its URL names: `createdb -h 127.0.0.1 -p 5433 -U legion_test "e2e_$(date +%s)"`.

Where `LEGION_TEST_PG_DSN` is unset and `legion-testpg` is on `PATH`, run `export LEGION_TESTPG_DIR=$XDG_DATA_HOME/testpg LEGION_TEST_PG_DSN='postgres://legion_test@127.0.0.1:5433/legion_test?sslmode=disable'`, then `legion-testpg ensure`. On a refused connection, wait and try again: the launcher runs `legion-testpg ensure` every 15 seconds, so never start a second server.

The pod has no Docker. For the daemon's NATS tests run one server per test binary and set both variables:

```sh
go run github.com/nats-io/nats-server/v2@v2.15.1 -js -p 14222 -m 18222 -sd "$TMPDIR/nats" &
export LEGION_TEST_NATS_URL=nats://127.0.0.1:14222 LEGION_TEST_NATS_MONITOR_URL=http://127.0.0.1:18222
```

## A tool the image lacks

Run it pinned through a runner the image carries: `uvx --from <package>@<version>`, `bunx --package <name>@<version>`, `go run <module>@<version>`. Never vendor a binary or put an extraction on your own `PATH`. A test harness may keep its own prefix under `$TMPDIR` and export its `PATH` only around its own commands. A tool that no runner reaches is a change of the image: tell your architect.
