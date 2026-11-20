# Dispatch server (Go)

`cmd/dispatch` builds `envoy-dispatch`: the Dispatch API, document rooms, sign-in and the GitHub read proxy.
It also serves the web app's build, `packages/dispatch/web/dist`; the web app has its own guide, `packages/dispatch/AGENTS.md`.
`cmd/dispatch/README.md` has the operator detail: settings, migrations, rolling releases, routes and error codes.

## Run and test

Run from `packages/envoy`.

- Local Postgres: `./scripts/dev-postgres.sh` prints the `DATABASE_URL` to use.
- A seeded server with a person signed in: `bun run dispatch:local` from the repository root.
- Tests and vet:

  ```sh
  export DISPATCH_TEST_DATABASE_URL='<the URL ./scripts/dev-postgres.sh prints>'
  go test -timeout 20m ./internal/dispatch/api
  go test -timeout 20m $(go list ./internal/dispatch/... ./cmd/dispatch/... | grep -v '/internal/dispatch/api$')
  go vet ./...
  ```

  One test: `go test -timeout 20m -run TestName ./internal/dispatch/api`.
  `internal/dispatch/api` alone takes close to Go's ten-minute default deadline, so it runs on its own with 20 minutes.
- Three suites build only under a tag, and the commands above run none of them.
  A claim about two processes, or two builds, on one database needs evidence from them.
  Each needs `DISPATCH_TEST_DATABASE_URL`.

  ```sh
  go test -tags memory -count=1 -timeout 60m ./cmd/dispatch        # resident memory of real servers
  go test -tags multiprocess -count=1 -timeout 30m ./cmd/dispatch  # two servers, one database
  bash scripts/build-dispatch-build.sh --previous-release --out "$TMPDIR/dispatch-previous"
  bash scripts/build-dispatch-build.sh --commit edb69e0d7588ec037847182e74a04a343a0664c9 --out "$TMPDIR/dispatch-first-rollout"
  DISPATCH_PREVIOUS_BUILD="$TMPDIR/dispatch-previous" DISPATCH_FIRST_ROLLOUT_BUILD="$TMPDIR/dispatch-first-rollout" \
    go test -tags twobuild -count=1 -timeout 30m ./cmd/dispatch   # this build beside earlier ones
  ```

## Conventions

- A setting is a row of the table in `cmd/dispatch/settings.go`, never an `os.Getenv`.
  `TestEverySettingReachesItsReader` needs a case for a new row.
  A setting a release stops reading moves to `removedSettings`, and the server then refuses to boot while it is set.
  `envoy-dispatch settings` prints the table, and the docs site's configuration page is generated from it.
- A route is a row of `routes()` in `internal/dispatch/api/routes_table.go`.
  Its tests then name the generated files to write again: `testdata/routes.txt` and the OpenAPI document the web app's types come from.
  OAuth scopes go in `routeScopes`, and the routes a read-only token may call in `readScopeRoutes`.
  `GET /api/v1` and `envoy-dispatch routes` list every route.
- Subcommands (`census`, `settings`, `routes`, `rebuild-refs`, `backfill-files` and others) are in `cmd/dispatch/subcommands.go`.
- Two processes serve one database, and while a release rolls they are of two builds (README, "Running more than one process" and "Two builds at once"). So:
  - a route is removed only in the release after the one that stops calling it;
  - a migration expands in one release and contracts in the next (README, "Database migrations");
  - state a later request needs lives in Postgres or a signed cookie, never in one process's memory.
- Migrations are `internal/dispatch/store/migrations`, applied at boot.
  Before a deploy, `envoy-dispatch census` lists what a database has not applied (README, "Pre-deploy census").
- Triggers, not Go code, fill `artifacts.ref_key` and the `search` columns.
- `/healthz` is a readiness probe. A liveness probe on it restarts every server during a database failover.
  Its `commit` and `schema_version` say what a server runs.
- A deployment can run an older build than `main`, so a pull request whose behaviour needs a deployed server says so in its body.
- Test-only settings, never set in a deployment:
  `DISPATCH_TEST_HOOKS=1` mounts the `_test` routes the browser suite calls;
  `DISPATCH_DEV_SIGNIN=1` signs a person in with no identity provider, and boots only on loopback;
  `DISPATCH_IDENTITY=header:<Name>` takes a person's identity from a request header.
