# Dispatch store

`migrations/` holds every `<version>_<name>.up.sql` boot applies in version order, with the `<version>_<name>.census.sql` each one declares for the pre-deploy census.
`<version>` is one of the numbers the set grew up with, each of whose files keeps the number it landed with, and for every migration added since the UTC moment its author created it, `YYYYMMDDHHMMSS` (`date -u +%Y%m%d%H%M%S`):
two pull requests open at once cannot both claim one timestamp, while both would claim the next number.
This file sits beside that directory rather than in it because the directory is embedded with `all:` and `pgmigrate.Load` refuses a set holding a file named anything but `<version>_<name>.up.sql`, `.down.sql` or `.census.sql`, which would refuse every boot.

Boot applies every version `schema_migrations` lacks, in version order, so a fresh database applies the whole set in that order, and a migration that merges below one a database has already applied still runs there, once.
A deployed database applies a migration when the release carrying it deploys:
if T2's release deploys before T1 merges, that database applies T2 then T1 while a fresh one applies T1 then T2.
So a migration must not depend on its order relative to one written in a concurrent pull request, another branch open at the same time;
one that needs another's effect is written after that one has merged.
`max(version)`, the `schema_version` `/healthz` reports, is the highest version applied, not proof that every version below it is applied, which is why the auto-deployer's rollback guard compares the whole set of recorded versions.

An unmarked migration and its version record commit in one transaction.
A file whose first line is exactly `-- pgmigrate:non-transactional` must hold one non-unique `CREATE INDEX CONCURRENTLY IF NOT EXISTS` statement; it runs outside a transaction under the same migration key's session-level advisory lock through its separate version insert.
Both modes keep the five-second bound on each lock wait, and shared lock diagnostics.
Every wait for that key polls `pg_try_advisory_lock` with nothing open between two tries, because a blocking wait deadlocks with the concurrent index build that holds the key.
The concurrent mode drops an INVALID index of the migration's own name on its own table and builds it again, refuses an index of that name on another table with an explicit `DROP INDEX CONCURRENTLY` recovery command, and refuses a relation of that name that is no index by name, to be renamed or removed, instead of recording a skipped, incomplete build as success.

`schema_migrations` records, beside each applied migration's version, an identity, a file's name (`name`) and the sha256 of its whole text (`checksum`), and the sha256 of the text the runner applied (`applied_checksum`); the runner adds the columns to a table an earlier runner made.
The identity is the one the file's first replaces line names, the version's first file, or the file's own where it has none (`pgmigrate.Migration.RecordedIdentity`).
Before it applies anything, under the migration key, the runner judges every recorded row against this build's file of its version (`pgmigrate.Reconcile`), and it judges them all again before each migration it applies, since it gives the key back in between and another process may have recorded a version.
A row whose name is null, as every row an earlier runner wrote, is filled and never refused, and each fill is logged.
A row whose identity is the file's own or one its `-- replaces: <name> sha256:<hex>` lines name, and whose applied text, where it records one, is the file's own text or one its lines name, passes and is left as it is, so the release that recorded it still boots.
A version with no file is left as it is.
Any other row refuses the boot, and nothing applies:
a database that ran another branch's or an earlier commit's migration, or an edit of one, is reset (dropped, created empty and migrated by the server), and a file edited or renamed on purpose gets the replaces line the refusal names, after the ones it has.
`../../../cmd/dispatch/README.md` ("A recorded version under another file") has the refusal word for word.

**A migration must leave the release before it able to read and write the schema.**
A Dispatch release rolls, so the old build and the new build run against this schema at once, and nothing at boot refuses a schema newer than the binary knows.
A change is therefore expand-then-contract across two releases:
the expanding migration adds, with a default or a backfill, and the release that ships it writes both shapes, while the migration that drops, renames or narrows ships no earlier than the next release, once no running build reads the old shape.
No migration is an exception:
a change that cannot be written that way is split into releases that can, so no release stops every server process.
The same rule holds for everything else the two builds share (request bodies and routes, query values, socket frames, response members, stored documents, NATS stream and consumer settings and subjects, messages, and settings), and the two-build test checks it (`go test -tags twobuild ./cmd/dispatch`).
**Messages:** envelope and payload members are additive, and every consumer ignores a member it does not know;
a consumer ignores an event type or `kind` it does not know, and acks it;
a member or a kind is removed only after no running build reads it.
The decoders that make this true are the listener's (`cmd/listener/delivery.go`), the session plugins' (`packages/envoy-client/src/delivery.ts`, `renderInbound`), the Legion daemon's (`packages/daemon/internal/intake/decode.go`) and the delivery intakes' (`internal/dispatch/delivery/intake.go` and `jobs.go`).

`../../../cmd/dispatch/README.md` has the rule for each surface ("Two builds at once") and for migrations with their examples ("Database migrations"), the naming and the identifiers a set is refused for, and what the census is held to.

**A read at a past time binds each statement to the cut.**
Package `internal/dispatch/asof` reads Dispatch as it stood at a past time, the cut.
A route that answers `as_of` runs its present statements under a context that carries the cut (`asof.WithCut`).
Under such a context `Pool` binds each statement before it sends it (`asof.Bind`): a statement of `Query`, `QueryRow`, `Exec` or `SendBatch`, and each statement of a transaction that `Begin` or `BeginTx` opens and of its savepoints.
`Bind` puts the shadow of each shadowed table that the statement names in front of the statement, as a `WITH <table> AS NOT MATERIALIZED (…)` member of the same name.
PostgreSQL resolves the table's name to that member before the table, so the statement reads the table as it stood at the cut.
So a read at a cut runs the filters, the joins, the order and the pages of the read at the present.
A shadow answers the columns of its table at the cut (`asof.ShadowedTables`, `TestEveryShadowAnswersEveryColumnOfItsTable`).
It reads `events`, the append-only tables, `post_body_history`, `schema_migrations`, and the present rows of `issues`, `issue_links`, `issue_components`, `priority_tuples`, `projects`, `repo_projects`, `asks`, `comments`, `messages`, `comment_deliveries`, `message_deliveries` and `artifacts`.
It keeps a present row only where the row existed at the cut, and takes the row's values at the cut from the events where they record them.
The cut is a `timestamptz` literal in each shadow, written from the parsed time and never from the text of a request.
Each base table in a shadow is named with the schema `public`, so no shadow reads another shadow.
`Bind` refuses a statement that is not a read, and a string that holds more than one statement, so the pool sends nothing and returns the refusal (`*asof.RefusedError`).
A batch that begins its own transaction (`pgx.Batch.BeginTx`) queues a `begin`, which `Bind` refuses.
Under a cut the pool refuses each connection that it hands out and each copy, because a connection sends statements that nothing binds, and a copy writes (`RefusedAtCutError`).
`Acquire`, `AcquireFunc`, `Listener`, `Dedicated`, `CopyFrom` and a transaction's `CopyFrom` and `Prepare` return that error, a transaction's `Conn` and `LargeObjects` panic with it, and `AcquireAllIdle` takes no connection.
The separate pools that `Rooms`, `RoomAppends`, `Compaction` and `Horizon` hand out check no cut: a statement sent through one of them is sent as the caller wrote it, under a cut too.
Under a context with no cut, the pool sends each statement as the caller wrote it.
A table that a read at a cut reads with no shadow is in `asof.MachineryTables`, the state of Dispatch's own machinery read as it is now, or in `asof.CutReadTables`, read under a predicate of its own that holds the read to the cut, each with its reason.

**Eleven indexes on `events` serve the reads at a cut.**
`events` is larger than the 1 GiB that a migration may touch (`pgmigrate.CensusTableLimit`), so each index is a migration of its own, marked `-- pgmigrate:non-transactional`.
Each holds one `CREATE INDEX CONCURRENTLY IF NOT EXISTS`, which the census exempts from its size limit, and a `.census.sql` of `select 0`.
The build takes `SHARE UPDATE EXCLUSIVE` on `events`, which no ordinary read or write of `events` waits on.
Each header names the size of the index on a copy of production's event log and the read that it serves.
The predicate of each partial index is its shadow's list of types, word for word, because the planner uses a partial index only when the read's condition implies its predicate.
`asof.TestEachEventIndexNamesTheTypesItsShadowReads` compares the list of types of each index whose predicate is `type in (…)` or `type not in (…)` with its shadow's list: each index below but `events_created_at`, which has no predicate, and `events_repo_project` and `events_artifact_renamed`, whose predicate names one type.
`asof.TestTheShadowsUseTheirIndexes` reads the plan of a read through each shadow at a cut, and holds each of the eleven indexes to the read it serves.
A build that fails leaves an invalid index, which the planner does not use, and the next boot drops it and builds it again.
Reads at a cut stay correct until then, and slower.

- `20261120004315_events_issue_state`, `(issue_key, seq desc) include (created_at)` over `asof.IssueStateTypes`: the newest state of each issue at the cut (the `issues` shadow).

- `20261120004316_events_ask_state`, `((payload->>'id'), id desc) include (created_at)` over `asof.AskStateTypes`: the newest state of each ask at the cut (the `asks` shadow).

- `20261120004317_events_created_at`, `(created_at, id)`: the largest id of the events at the cut, at which a page of `GET /api/v1/events/log` at the cut ends (`asof.NewestEventIDStatement`).

- `20261120140001_events_issue_claim`, `(issue_key, seq desc) include (created_at)` over `asof.IssueClaimTypes`: the claim of an issue at the cut (the `issues` shadow).

- `20261120140002_events_issue_activity`, `(issue_key, created_at) include (seq)` over each type but `asof.IssueStateTypes`: the `updated_at` and the `last_seq` of an issue at the cut (the `issues` shadow).

- `20261120140003_events_comment_state`, `((payload->>'id'), id desc) include (created_at)` over `asof.CommentStateTypes`: the events of a comment at or before the cut and its first edit or delete after it (the `comments` shadow), and the creating event of an ask's reply (the `asks` and `ask_followers` shadows).

- `20261120140004_events_message_state`, the same key over the message types: the `edited_at` and the body of a message at the cut (the `messages` shadow).

- `20261120140005_events_repo_project`, `(created_at)` over `settings.repo_project.updated`: the mapping of each repository to a project at the cut (the `repo_projects` shadow).

- `20261120140006_events_ask_follower`, `((payload->>'ask_id'), id) include (created_at)` over `asof.AskFollowerTypes`: the follows and unfollows of an ask at the cut (the `ask_followers` shadow).

- `20261120140007_events_artifact_renamed`, `((payload->'artifact'->>'id'), id)` over `artifact.renamed`: the name of a document at the cut, the `previous_name` of its first rename after the cut (the `artifacts` shadow).

- `20261120140008_events_project_setting`, `(project_key, seq desc) include (created_at)` over the project setting types: the default assignee and the gated steps of a project at the cut (the `projects` shadow).

**The keyword table holds each past version of the searchable text.**
Migration `20261120021308_search_text_versions` creates two tables, with no foreign key.
`search_text_versions` holds one row for each version of the text that keyword search reads of an issue, a comment, an issue message or an ask, with the vector of that text.
A row is never updated: a new version is a new row.
`search_text_versions_fill` holds the one cursor row of the keyword fill (`internal/dispatch/textversions`): every event at or below its `event_id` has its rows.
The fill writes both tables from the event log while Dispatch serves, under the hold `search-text-versions-fill`.
It pauses while an answers read that its process serves takes longer than 800 ms (`../../../cmd/dispatch/README.md`, "Background loops").
No migration writes a row of either table.

`TextVersions.SQL()` is the relation of the version of each item that held at `TextVersions.Cut`, for a statement to join as `from <SQL()> v`.
A version holds at the cut when it started at or before the cut, and no later version of the same item in its owner's `seq` order started at or before the cut.
So an item has at most one version at a time, and none before its first version started.
The SQL writes the cut as a literal in UTC to the microsecond, from the time value and never from the text of a request.
It names each table with the schema `public`, so a common table expression of the statement that joins it cannot replace them.
Its columns, in this order, are a contract (`TestTheKeywordRelationAnswersTheContractColumns`): `kind`, `id`, `issue_key`, `project_key`, `event_id`, `event_seq`, `started_at`, `body`, `spec_artifact_id`, `spec_version`, `search` and `deleted_at`.
`deleted_at` is the post's present `deleted_at` on each version of a comment or a message, read from `public.comments` or `public.messages`, and null for an issue or an ask.
So a search at a cut hides a post deleted after the cut as soon as its delete commits, before the fill reads the delete.
`Store.TextVersionsAt(ctx, cut)` answers the relation only for a cut at or before the reach.
For a later cut it answers `*TextVersionsNotReachedError`, whose `Error()` names the reach and the cut.
With no cursor row, the error has `Reached` false and says that the fill has reached no time yet.
The same is true while the cursor row records rules older than `TextVersionsRules` (2).
The first rules passed the edit of an issue message and the delete of a comment or a message, and the fill reads those events again before the row records the rules that read them.
Call `TextVersionsAt` and `TextVersionsReach` with a context that carries no cut (`asof.WithoutCut`): they read the fill's own row and `pg_stat_activity` now.

The reach is the latest time T such that every event whose `created_at` is at or before T has its rows.
This includes an event that a transaction still open will commit.
`ComputeTextVersionsReach` computes the reach in a READ COMMITTED transaction, one statement at a time, in this order:

1. T0, `clock_timestamp()`, and whether the cursor is at most 5,000 events behind the newest event and not past it.
   When it is not, no value is computed and no later step runs.

2. A, the oldest `xact_start` of another client backend of this database that can write an event.
   A backend can write an event when its login is a superuser, when its login or a role it is a member of has `INSERT` on `public.events`, or when its login is a role that the query cannot resolve.

3. B, the oldest `created_at` of the events above the cursor, read in a later statement with its own snapshot, which checks the cursor against the newest event again.

4. The reach: the earliest of A, B and T0 less 1 s, less 1 µs.

A transaction open at step 2 is in A.
A transaction that commits after step 2 and before step 3 is in B.
A transaction that starts after step 2 started after T0.
The 1 s covers the moment between the start of a transaction and its report of `xact_start`.
No value is computed, and the stored reach stands, while a backend that can write an event is in a transaction whose start this role cannot read.
The same is true while the cursor is more than 5,000 events behind the newest event, and while it is past the newest event after a reset of the log that the fill has not found yet.
The fill stores the reach in each batch's transaction and never lowers it, and a reset of the event log sets it to null.
`Store.TextVersionsReach` computes the reach again and answers the later of the computed reach and the stored reach.
So a database whose fill is caught up and quiet is not refused a cut that the fill has reached.
