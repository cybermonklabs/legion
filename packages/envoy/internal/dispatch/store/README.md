# Dispatch store

`migrations/` holds every `<version>_<name>.up.sql` boot applies in version order, with the `<version>_<name>.census.sql` each one declares for the pre-deploy census.
`<version>` is one of the numbers the set grew up with, each of whose files keeps the number it landed with, and for every migration added since the UTC moment its author created it, `YYYYMMDDHHMMSS` (`date -u +%Y%m%d%H%M%S`):
two pull requests open at once cannot both claim one timestamp, while both would claim the next number.
This file sits beside that directory rather than in it because the directory is embedded with `all:`, and both readers of it, `pgmigrate.LoadBuilt` at boot and `pgmigrate.Load` in the store's tests, refuse a set holding a file named anything but `<version>_<name>.up.sql`, `.down.sql` or `.census.sql`, which would refuse every boot.

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

**A table past the census limit still takes a new index.**
`pgmigrate.CensusTableLimit` is 1 GiB of `pg_total_relation_size`, and the census refuses an ordinary migration that touches a table above it, unless every statement of it that touches the table is a bounded write: an insert of at most `pgmigrate.BoundedWriteRows` (1,000) literal rows, or an update of at most that many rows by their primary key (`../../../cmd/dispatch/README.md`, "Pre-deploy census"), which is how a later migration seeds rows into `graph_tuples` or `graph_nodes`.
The concurrent mode above is the way through: `pgmigrate/census.go`'s `ConcurrentIndex` flag exempts a marked migration from the size limit by name, so a log that has grown past the limit is indexed by a migration whose whole body is one statement, in the shape `20261102233630_embeddings_window_owner.up.sql` uses:
a first line of exactly `-- pgmigrate:non-transactional`, then one `CREATE INDEX CONCURRENTLY IF NOT EXISTS`, non-unique and named.
`graph_tuples` and `graph_nodes` are unpartitioned for that reason, and a later index on either is one such migration.
An enabled DDL event trigger voids the exemption, and a UNIQUE build is refused, so a unique key on a table that size needs a stop-the-world migration instead.

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

**The keyword table holds each past version of the searchable text.**
Migration `20261120021308_search_text_versions` creates two tables, with no foreign key.
`search_text_versions` holds one row for each version of the text that keyword search reads of an issue, a comment, an issue message or an ask, with the vector of that text.
A new version is a new row, and a row is updated in one case alone: the fill's re-read, after a fill of a lower rules version wrote (`textversions`' `rereadBatch`), writes again a post's row whose cut body that version read from another source than this build reads first, with its body, its `body_from` and its vector.
`search_text_versions_fill` holds the one cursor row of the keyword fill (`internal/dispatch/textversions`): every event at or below its `event_id` has its rows.
Its `rules` is the lowest rules version that wrote the rows up to the cursor; a batch of a higher version that finds a lower one stored reads again what that version passed over before it writes past the cursor, and only that re-read raises the column.
The fill writes both tables from the event log while Dispatch serves, under the hold `search-text-versions-fill`.
It pauses while an answers read that its process serves takes longer than 800 ms (`../../../cmd/dispatch/README.md`, "Background loops").
No migration writes a row of either table.

`TextVersions.SQL()` is the relation of the version of each item that held at `TextVersions.Cut`, for a statement to join as `from <SQL()> v`.
A version holds at the cut when it started at or before the cut, and no later version of the same item in its owner's `seq` order started at or before the cut.
So an item has at most one version at a time, and none before its first version started.
The SQL writes the cut as a literal in UTC to the microsecond, from the time value and never from the text of a request.
It names each table with the schema `public`, so a common table expression of the statement that joins it cannot replace them.
Its columns, in this order, are a contract (`TestTheKeywordRelationAnswersTheContractColumns`): `kind`, `id`, `issue_key`, `project_key`, `event_id`, `event_seq`, `started_at`, `body`, `spec_artifact_id`, `spec_version`, `search` and `deleted_at`.
`deleted_at` is the post's own delete mark as the post stands now, `comments.deleted_at` or `messages.deleted_at`, on every version of a deleted comment or message at every cut, and null on every other version, so a reader leaves out or marks every version of a deleted post.
`Store.TextVersionsAt(ctx, cut)` answers the relation only for a cut at or before the reach.
For a later cut it answers `*TextVersionsNotReachedError`, whose `Error()` names the reach and the cut.
With no cursor row, the error has `Reached` false and says that the fill has reached no time yet.

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
