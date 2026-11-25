# Dispatch server (Go)

`cmd/dispatch` builds `envoy-dispatch`: the Dispatch API, document rooms, sign-in and the GitHub read proxy.
It also serves the web app's build, `packages/dispatch/web/dist`; the web app has its own guide, `packages/dispatch/AGENTS.md`.
`cmd/dispatch/README.md` has the commands and tests, and the operator detail: settings, migrations, rolling releases, routes and error codes.

## Rules

- A setting is a row of the table in `cmd/dispatch/settings.go`, never an `os.Getenv`.

- A route is a file of its own in `internal/dispatch/api`, `route_<path>+<method>.go`, holding one `declareRoute(routeDecl{...})` that states every fact of the route (`routeDecl` in `routes_table.go`); `internal/dispatch/routefile` names the file. A branch made before routes moved into files adopts the layout with `go run ./cmd/dispatch-route-adopt -C <that worktree>`, run from `packages/envoy` of a checkout that holds the command, on a clean tree: it makes the merge of main itself (`origin/main`, or `main@origin` under jj, unless it is given a revision), and refuses a tree that is mid-merge or has changes, naming the commands that return it (`git merge --abort`; under jj `jj abandon @`, then `jj edit` of the merge's first parent).

- Two processes serve one database, and while a release rolls they are of two builds (README, "Running more than one process" and "Two builds at once").
  Remove a route only in the release after the one that stops calling it, expand a migration in one release and contract it in the next (README, "Database migrations"), and keep state a later request needs in Postgres or a signed cookie, never in one process's memory.

- Leave `artifacts.ref_key` and the `search` columns to the triggers that fill them.

- A deployment can run an older build than `main`, so a pull request whose behaviour needs a deployed server says so in its body.
