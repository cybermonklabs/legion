# Dashboard's captured fixtures

Each file here is an answer a real server gave, saved with the command that read it.
The unit tables of `features/dashboard/` read these files and never a body written by hand.

## `handoffs.json`

The handoffs list of `GET /api/v1/issues/{key}/handoffs`, which `round-counts.ts` reads.
Read from production on INFRA-4097 at 2026-11-25T13:07Z:

```sh
printf 'Authorization: Bearer %s' "$(cat "$DISPATCH_TOKEN_FILE")" |
  curl -sS -H @- "$DISPATCH_URL/api/v1/issues/INFRA-4097/handoffs" -o handoffs-raw.json
```

The answer held 44 rows: `implement` 27, `plan` 2, `review` 4 and `test` 11.
This file keeps ten of them, in the answer's order, so that each phase is present: both `plan` rows, the first three `implement` rows, the first three `test` rows and the first two `review` rows.
Each row's `author.origin` is left out, because it names the deployment's machine and its paths; every other member is as the server wrote it.

## `serves-forms.txt`

The `serves` attributes that real Verification documents write, which the met tile matches to statements.
Read at 2026-11-25T13:08Z:

```sh
for K in INFRA-5702 INFRA-1196 INFRA-4097 INFRA-1150; do
  echo "# $K"
  dispatch doc-read --issue "$K" --artifact verification-md | grep -o -E 'serves="[^"]*"'
done
```

Every reference is a full `dispatch://<KEY>/spec#b-<id>`, of the issue's own spec or of an ancestor's.

## `legion-issue/`

The body of `GET /api/v1/issues/{key}/legion` for each case of the plan's phase table, which `phase-track.test.ts` and `role-words.test.ts` read.
Production does not serve the route yet, so each body is the answer of the server at this branch's head: the end-to-end harness's stack, started with `bun e2e/harness-up.ts` from `packages/dispatch` with `DISPATCH_E2E_PG_BIN=/usr/lib/postgresql/18/bin`, which builds the Go server of `packages/envoy` at that head and runs it on a Postgres of its own.
Captured at 2026-11-25T14:04:59Z.

Each case is one issue of the project `ACME` whose Legion state `seedLegionState` (`e2e/dashboard-seed.ts`) wrote as the sampler writes a read of the daemon that answered: the project's daemon setting, `legion_observations` rows for `phase` and for `claim.<role>` of each role, a `legion_reads` row, and the notification.
`configured-false` is captured first, before the daemon setting exists; every other case is configured.

| File | `phase` | Claims |
| --- | --- | --- |
| `planning.json` | `planning` | one role per claim state of `LEGION_CLAIM_STATES`, and `role-unlisted` in `parked` |
| `awaiting_person.json`, `held.json`, `integrating.json`, `awaiting_merge.json` | the file's name | `architect` working, `planner` idle |
| `another.json` | `parked`, which no list holds | the same |
| `admitted.json`, `implementing.json`, `testing.json`, `reviewing.json`, `accepting.json`, `retro.json`, `merging.json` | the file's name | the same |
| `done.json`, `unrecorded.json` | the file's name | the same |
| `state-null.json` | none written, so `state` is null | the same |
| `configured-false.json` | `planning`, under a project with no daemon setting | the same |

The commands, from `packages/dispatch`, with the harness's exports applied as its header says:

```sh
COOKIE=$(mktemp)
curl -sS -c "$COOKIE" -o /dev/null "http://127.0.0.1:$DISPATCH_E2E_PORT/auth/_dev/signin?login=alice"
# For each case, an issue of ACME and `seedLegionState(key, {configured, phase, roles})`, then:
curl -sS -b "$COOKIE" "http://127.0.0.1:$DISPATCH_E2E_PORT/api/v1/issues/<KEY>/legion" > legion-issue/<case>.json
```

Each file is the answer as the server wrote it, formatted by `bunx biome format --write`.
Every `read.ok` is true: no read of the sampler's own, which fails against the address nothing answers, came between a seed and its capture.

## The reads of one Dashboard seed

`issue.json`, `asks-open.json`, `decisions.json`, `changes.json`, `coverage.json`, `results.json` and `my-replies.json` are reads Dashboard makes (R8), of the one issue that `seedDashboard` (`e2e/dashboard-seed.ts`) seeds, as the server at this branch's head answered the person the seed signs in as, `alice`.
`tiles.test.ts` and `waiting-items.test.ts` read them.
The stack is the end-to-end harness's, started as for `legion-issue/` above, at 2026-11-25T22:21Z.

The seed's fake GitHub helper calls Playwright's `test.skip`, so the seed runs inside a Playwright test: one spec file outside the repository whose one test calls `resetDatabase()` (`e2e/seed.ts`) and then `seedDashboard(await mainHolds())`, run with `bunx playwright test --config <its own config>` under the harness's exports, at 2026-11-25T22:24:55Z.
The seed made the issue `ACME-1` on the path of a `main` without INFRA-1150 and INFRA-1253.

The reads, at 2026-11-25T22:25:12Z, each with `Dispatch-Trees: 1`, which the web client sends on every read (`api/client.ts`), so each body carries the display trees the page reads:

```sh
COOKIE=$(mktemp)
BASE="http://127.0.0.1:$DISPATCH_E2E_PORT"
curl -sS -c "$COOKIE" -o /dev/null "$BASE/auth/_dev/signin?login=alice"
get() { curl -sS -b "$COOKIE" -H 'Dispatch-Trees: 1' "$BASE$1"; }
get /api/v1/issues/ACME-1 > issue.json
get '/api/v1/issues/ACME-1/asks?state=open' > asks-open.json
get /api/v1/issues/ACME-1/decisions > decisions.json
get /api/v1/issues/ACME-1/coverage > coverage.json
get /api/v1/issues/ACME-1/results > results.json
get '/api/v1/me/replies?issue=ACME-1&state=unread' > my-replies.json
```

`changes.json` is the comments of the decisions document, the read Dashboard finds a Change in, after one Change.
At 2026-11-25T22:25:12Z that read answered `[]`.
At 2026-11-25T22:25:24Z the reader wrote a comment anchored on the second decision's words, as the decision card's Change writes one, and the read was made again:

```sh
DOC=<the decisions document's id: the seed's decisionsDocumentId>
curl -sS -b "$COOKIE" -H "Origin: $BASE" -H 'Content-Type: application/json' \
  --data "{\"anchor\":{\"artifact\":\"$DOC\",\"quote\":\"Chosen so the count and the list agree.\"},\"body\":\"Count the threads apart.\"}" \
  "$BASE/api/v1/issues/ACME-1/comments"
get "/api/v1/issues/ACME-1/comments?artifact=$DOC" > changes.json
```

Each file is the answer as the server wrote it, formatted by `bunx biome format --write`.
Where a row of the item table needs a case the seed does not hold (an approval by another person, a request on a plan), `waiting-items.test.ts` changes one member of a captured body.
Rows V1 and V2 give node states in the shape of dispatch://INFRA-1253's tree read (a node's `id`, `block_id` and `state`), which no server of this `main` answers.
