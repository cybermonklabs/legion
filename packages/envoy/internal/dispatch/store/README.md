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

**A table past the census limit still takes a new index.**
`pgmigrate.CensusTableLimit` is 1 GiB of `pg_total_relation_size`, and the census refuses an ordinary migration that touches a table above it.
The concurrent mode above is the way through: `pgmigrate/census.go`'s `ConcurrentIndex` flag exempts a marked migration from the size limit by name, so a log that has grown past the limit is indexed by a migration whose whole body is one statement, in the shape `20261102233630_embeddings_window_owner.up.sql` uses:
a first line of exactly `-- pgmigrate:non-transactional`, then one `CREATE INDEX CONCURRENTLY IF NOT EXISTS`, non-unique and named.
`graph_tuples` and `graph_nodes` are unpartitioned for that reason, and a later index on either is one such migration:
`20261116172615_graph_nodes_proposed_index` is one, a partial index of the nodes at `proposed`, and `20261116232634_going_requests_ends_at_index` builds `going_requests_ends_at` the same way, because `going_requests` keeps every run of the command bar now and a plain build would hold off every question asked while it reads the table.
Each has a `.down.sql`, the `DROP INDEX CONCURRENTLY` an operator runs by hand outside a transaction; nothing depends on either index, so rolling back past one is an image rollback alone.
An enabled DDL event trigger voids the exemption, and a UNIQUE build is refused, so a unique key on a table that size needs a stop-the-world migration instead.

**A migration must leave the release before it able to read and write the schema.**
A Dispatch release rolls, so the old build and the new build run against this schema at once, and nothing at boot refuses a schema newer than the binary knows.
A change is therefore expand-then-contract across two releases:
the expanding migration adds, with a default or a backfill, and the release that ships it writes both shapes, while the migration that drops, renames or narrows ships no earlier than the next release, once no running build reads the old shape.
No migration is an exception:
a change that cannot be written that way is split into releases that can, so no release stops every server process.
The node and tuple store's migrations of lenses and runs of the command bar expand alone:
`20261116123354_lens_catalogue` and `20261116123355_lens_catalogue_further_kinds` write type nodes and catalogue nodes with their tuples, `20261116205055_interaction_nodes` writes the interaction node and `frame` values of every run recorded before it, `20261116081010_block_pointer_targets_present` and `20261117042519_block_moved_to_lens` restate a function that answers every call the release before makes as it did, and `20261117020856_graph_edges_asks_and_store_class` restates `graph_edges` with every arm the release before reads.
During a roll the release before records a run with no node, appends its frames with no `frame` value and no lock on the node, and deletes a run an hour old.
A frame of this release takes the run's interaction node row `for no key update` before the run's `going_requests` row, the order the erase of a run takes them in; a run with none gets its node from its row first, under that row, and no frame waits on a node's row while it holds the run's (`api/going.go`'s `lockGoingRun`).
The `going_events_terminal` index and the frame's `on conflict do nothing` settle a frame both releases write at one sequence, and `backfill-graph-store` mends what the release before wrote.
The same rule holds for everything else the two builds share (request bodies and routes, query values, socket frames, response members, stored documents, NATS stream and consumer settings and subjects, messages, and settings), and the two-build test checks it (`go test -tags twobuild ./cmd/dispatch`).
**Messages:** envelope and payload members are additive, and every consumer ignores a member it does not know;
a consumer ignores an event type or `kind` it does not know, and acks it;
a member or a kind is removed only after no running build reads it.
The decoders that make this true are the listener's (`cmd/listener/delivery.go`), the session plugins' (`packages/envoy-client/src/delivery.ts`, `renderInbound`), the Legion daemon's (`packages/daemon/internal/intake/decode.go`) and the delivery intakes' (`internal/dispatch/delivery/intake.go` and `jobs.go`).

`../../../cmd/dispatch/README.md` has the rule for each surface ("Two builds at once") and for migrations with their examples ("Database migrations"), the naming and the identifiers a set is refused for, and what the census is held to.
