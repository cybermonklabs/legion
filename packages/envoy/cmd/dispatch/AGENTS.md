# Dispatch server (Go)

`cmd/dispatch` builds `envoy-dispatch`: the Dispatch API, document rooms, sign-in and the GitHub read proxy.
It also serves the web app's build, `packages/dispatch/web/dist`; the web app has its own guide, `packages/dispatch/AGENTS.md`.
`cmd/dispatch/README.md` has the commands and tests, and the operator detail: settings, migrations, rolling releases, routes and error codes.

## Rules

- A setting is a row of the table in `cmd/dispatch/settings.go`, never an `os.Getenv`.

- A route is a row of `routes()` in `internal/dispatch/api/routes_table.go`.

- Give every GET row a read class in `readClasses` (`internal/dispatch/api/read_classes.go`): a read, which a read-only token may call, or a GET that writes, with a `WriteNote` naming the write.
  Give it an `as_of` kind there too (`AsOf`, below).
  `register` refuses to mount a GET row with no class or no kind.

- Two processes serve one database, and while a release rolls they are of two builds (README, "Running more than one process" and "Two builds at once").
  Remove a route only in the release after the one that stops calling it, expand a migration in one release and contract it in the next (README, "Database migrations"), and keep state a later request needs in Postgres or a signed cookie, never in one process's memory.

- Leave `artifacts.ref_key` and the `search` columns to the triggers that fill them.

- A deployment can run an older build than `main`, so a pull request whose behaviour needs a deployed server says so in its body.

## Reads at a past time

`cmd/dispatch/README.md` ("Reads at a past time") has the whole rule: the parameter, the kinds, the headers, the members each route can leave out, the token that carries a cut, the codes and the error of a cut.

- A request sends `as_of`, an RFC 3339 time with an offset, and a route of kind answers gives what existed at that time, the cut.
  `asOfGate` (`internal/dispatch/api/server.go`) is the one reader of `as_of`, and no handler reads it.

- Each GET row has one kind (`readEntry.AsOf`).
  `asOfAnswers` answers `as_of` and the cut of a read-only token.
  `asOfNoHistory` answers the present only and refuses `as_of` with `400 AS_OF_NOT_SERVED`.
  Such a read refuses a read-only token that carries a cut with `403 AS_OF_TOKEN_NOT_SERVED`, and such a GET that writes refuses each read-only token with `403 READ_ONLY_TOKEN`.
  `asOfCaller` is `GET /api/v1/whoami` alone: it describes the caller now, names the cut of a token, and refuses `as_of` from every caller but a token that carries a cut, whose `as_of` it does not read.
  Every row that is not a GET row refuses `as_of` with `400 AS_OF_NOT_SERVED`.

- A token that carries a cut reads at its cut on each route of kind answers, whatever `as_of` the request sends.
  Every other read and `GET /ws/live` refuse it with `403 AS_OF_TOKEN_NOT_SERVED`, naming the cut.

- A route of kind answers runs its present statements under the cut, and the store binds each statement to the shadows of `internal/dispatch/asof` (`asof.Bind`).
  Such a route reads a table outside the shadows only where the table is a base table that a shadow reads, or has an entry in `asof.MachineryTables` or `asof.CutReadTables` with its reason: `TestAPastTimeStatementReadsNoShadowedTableDirectly` fails on any other table.
  A source that is not SQL (the live document room, the route roster, the Envoy listener, GitHub, the embedder) is read at a cut only through a branch of that route.

- An answer at a cut carries `Dispatch-As-Of: <cut>`.
  A member that the records at the cut cannot give is left out (`leaveOut`), never written as `null`, and named in `Dispatch-As-Of-Absent`.
  Add such a member to `asof.AbsentFields` with its reason, and to the list in `cmd/dispatch/README.md`: `TestTheGuideNamesTheMembersEachAnswersRouteCanLeaveOut` holds the list to the table.

- A filter that the records at a cut cannot evaluate is refused with `400 AS_OF_NOT_SERVED`, naming the filter, and is never read as the present.

- A route of kind answers declares `as_of` on its query struct (`routes_shapes.go`), but `GET /api/v1/search`, whose search at a cut goes to `GET /api/v1/search/results`.
  Its row says what it answers at a cut in `asOfAtACut` (`routes_table.go`): `TestEveryAnswersRouteDeclaresAsOfInTheOpenAPIDocument` fails on a route of kind answers that does not do both.

- A search at a cut that the keyword table's fill has not reached is refused with `409 AS_OF_SEARCH_NOT_FILLED`, never answered from the present text.
  Where the fill has reached a time, the refusal names it in its message and in `Dispatch-As-Of-Filled-Through`.
  Where the fill has reached no time yet, the refusal carries no such header.
