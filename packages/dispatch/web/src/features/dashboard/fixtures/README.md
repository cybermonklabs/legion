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
