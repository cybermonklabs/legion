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
A file whose first line is exactly `-- pgmigrate:non-transactional` must hold one non-unique `CREATE INDEX CONCURRENTLY IF NOT EXISTS` statement;
it runs outside a transaction under the same migration key's session-level advisory lock through its separate version insert.
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
The concurrent mode above is the way through:
`pgmigrate/census.go`'s `ConcurrentIndex` flag exempts a marked migration from the size limit by name, so a log that has grown past the limit is indexed by a migration whose whole body is one statement, in the shape `20261102233630_embeddings_window_owner.up.sql` uses:
a first line of exactly `-- pgmigrate:non-transactional`, then one `CREATE INDEX CONCURRENTLY IF NOT EXISTS`, non-unique and named.
`graph_tuples` and `graph_nodes` are unpartitioned for that reason, and a later index on either is one such migration.
An enabled DDL event trigger voids the exemption, and a UNIQUE build is refused, so a unique key on a table that size needs a stop-the-world migration instead.
An expression index keeps the values it computed when its immutable function is replaced, so a migration that redefines `canonical_login` (`20261117000831`) runs `REINDEX INDEX CONCURRENTLY agent_tokens_canonical_owner` (`20261119020200`) in the same release.

**A migration must leave the release before it able to read and write the schema.**
A Dispatch release rolls, so the old build and the new build run against this schema at once, and nothing at boot refuses a schema newer than the binary knows.
A change is therefore expand-then-contract across two releases:
the expanding migration adds, with a default or a backfill, and the release that ships it writes both shapes, while the migration that drops, renames or narrows ships no earlier than the next release, once no running build reads the old shape.
No migration is an exception: a change that cannot be written that way is split into releases that can, so no release stops every server process.
The same rule holds for everything else the two builds share (request bodies and routes, query values, socket frames, response members, stored documents, NATS stream and consumer settings and subjects, messages, and settings), and the two-build test checks it (`go test -tags twobuild ./cmd/dispatch`).
**Messages:** envelope and payload members are additive, and every consumer ignores a member it does not know;
a consumer ignores an event type or `kind` it does not know, and acks it; a member or a kind is removed only after no running build reads it.
The decoders that make this true are the listener's (`cmd/listener/delivery.go`), the session plugins' (`packages/envoy-client/src/delivery.ts`, `renderInbound`), the Legion daemon's (`packages/daemon/internal/intake/decode.go`) and the delivery intakes' (`internal/dispatch/delivery/intake.go` and `jobs.go`).

**Every column the account binding reads moves only as its writers move it** (migrations `20261118041916` and `20261118152422`, `TestEveryBindingColumnMovesOnlyAsItsWritersMoveIt`):
`people`, `user_sessions` and `agent_tokens` (`20261118041916`), and `broker_revocations` (`20261118152422`), the last because it is what ends a replaced account's credentials at the secrets broker.
The two guards that hold a stamp to the database's clock (`people.confirmed_at`, `broker_revocations.tried_at`) compare it with `greatest(now(), clock_timestamp())` (`20261118200651`), so a writer's own `now()` stamp passes after the clock steps backwards (`TestTheGuardsAcceptAWritersOwnStampAfterTheClockStepsBack`).
Both run under a `search_path` of their own, `pg_catalog`, the schema the runner made the tables in, then `pg_temp` (`20261119021826`, built from `current_schema()` as it runs), so a session's own schema or temporary table cannot stand in for their clock or their tables (`TestTheClockGuardsRunUnderTheirOwnSearchPath`, `TestATemporaryTableNamedPeopleDoesNotShadowTheGuardsRead`).
`people.sub_issuer` is the issuer of the account `people.sub` records: null for the deployment's sign-in issuer, and the outside origin for the subject a restricted member's first passkey enrollment makes (`PgPasskeys`, tables `outside_invitations`, `passkeys` and `passkey_ceremonies`).
The same guard moves it only with `sub`, and refuses it on a row with no `sub` (`20261120165538`, which pins the replaced function's `search_path` again);
a passkey's row holds no refresh token, so `people_sign_in_stands` holds a row of the sign-in issuer's account to the old pairing of refresh token and confirmation, and a passkey's row to holding no refresh token.
In the recovery below, `issuer` is the account's own: the sign-in issuer, or the outside origin for a passkey's account.
The guard is against a statement written by hand, or a mistaken one.
It refuses each transition no writer makes, so no such statement can make a credential act for an account that no longer holds its address.
A statement that makes a writer's own transition without the rest of what that writer does passes, since no guard can tell it from the writer's, and two such leave something to recover, the first of which the account binding's catch-up recovers by itself;
the last item is the broker's own clear, which recovers neither:

- **An account written into `people.sub` by itself**, rather than that account signing in, records none of the revocations a sign-in records; the trigger `people_account_binding_replacement` keeps the change (migration `20261121180429`), and once the account binding holds, the revocation loop's catch-up (`CatchUpAccountBinding`) does what that sign-in would have done.
  Say A held the address and B took it: writing A back is caught up by settling what is still owed for A and recording B's revocation naming A (`TestACatchUpOwesNothingToTheAccountThatHoldsTheAddressAgain` holds that a catch-up owes B's revocation alone, naming A, after A took the address back while the binding was off), which the loop pays, so the broker ends B's credentials and stops refusing A.
  Until then, while a build without the binding is in the fleet, B's credentials stay live at the broker, which serves them secrets without asking Dispatch, and the broker goes on refusing A.
  To recover at once, by hand, do on Dispatch's database what that sign-in would have done: first settle every revocation still owed for the account `people` records now (A), as a sign-in that takes an address back does (`settleReclaimedBrokerRevocationsSQL`), so the loop never pays one that would end what A holds; then record the revocation of the account the statement replaced (B, the `sub` it overwrote), naming A as its holder, exactly as a sign-in records one (`recordBrokerRevocationSQL`; an upsert where B was recorded before).
  The server's loop then pays it: the broker ends B's credentials and, since a revoke naming A as the holder deletes its record of A, stops refusing A.
  The block locks the address's `user_sessions` row first, so a sign-out at the same moment waits for it, rather than moving the generation between the statement's read of it and the guard's own check, which would refuse the statement:

  ```sh
  PGPASSWORD="$DISPATCH_PGPASSWORD" psql "$DISPATCH_PSQL_URL" -v ON_ERROR_STOP=1 -v email='<address>' -v issuer='https://accounts.google.com' -v subject='<the sub the statement overwrote>' <<'SQL'
  begin;
  select generation from user_sessions where login = :'email' for update;
  update broker_revocations set done_at = now() where email = :'email' and issuer = :'issuer' and subject = (select sub from people where people.email = :'email') and done_at is null;
  insert into broker_revocations (email, issuer, subject, holder_subject, recorded_at, session_generation) values (:'email', :'issuer', :'subject', (select sub from people where people.email = :'email'), clock_timestamp(), (select generation from user_sessions where user_sessions.login = :'email')) on conflict (email, issuer, subject) do update set generation = broker_revocations.generation + 1, holder_subject = excluded.holder_subject, recorded_at = excluded.recorded_at, session_generation = excluded.session_generation, done_at = null;
  commit;
  SQL
  ```

  `store.TestAnAccountWrittenBackByItselfIsRecoveredByRecordingTheSkippedRevocation` runs this block from this file and the runbook, and `store.TestAnAccountWrittenBackOverARevocationPaidBeforeIsRecordedAgain` runs it where the replaced account's revocation was recorded and paid before, through its conflict arm.
  A statement that records an account where the address recorded none overwrote no `sub`; the trigger keeps it as a first recording, which the catch-up revokes and owes for as the first sign-in would have.

- **A revocation settled by itself** (`done_at` written while the broker confirmed nothing) is what `envoy-dispatch clear-broker-revocation` does, and tells the broker nothing:
  the replaced account's credentials go on reading the secrets the broker serves without asking Dispatch,
  and the broker goes on refusing the account the address came back to, when it had recorded that account replaced, since only a revocation naming that account as the holder lifts the refusal.
  To have the server's loop pay it, which recovers both, record it again, naming the account the address records now as its holder, with the address's `user_sessions` row locked first, as above:

  ```sh
  PGPASSWORD="$DISPATCH_PGPASSWORD" psql "$DISPATCH_PSQL_URL" -v ON_ERROR_STOP=1 -v email='<address>' -v issuer='https://accounts.google.com' -v subject='<the replaced sub>' <<'SQL'
  begin;
  select generation from user_sessions where login = :'email' for update;
  update broker_revocations set generation = generation + 1, holder_subject = (select sub from people where people.email = broker_revocations.email), session_generation = (select generation from user_sessions where user_sessions.login = broker_revocations.email), done_at = null, recorded_at = clock_timestamp() where email = :'email' and issuer = :'issuer' and subject = :'subject';
  commit;
  SQL
  ```

  `store.TestARevocationSettledByItselfIsPaidOnceRecordedAgain` runs this block from this file and the runbook.

- **A refusal at the broker that the operator chooses to lift**, of an account the broker was told no longer holds the address, is cleared on the broker's database, after checking that `people.sub` records the account;
  it lifts that refusal and ends nothing, so it recovers neither write above.
  The broker refuses a delete from that table unless the deleting transaction first names the one row it deletes, by its own transaction id, in `broker.replaced_subject_delete`, as a JSON array of the transaction id, the address, the issuer and the `sub`, so the clear is one transaction that sets that marker for the row the operator chose to clear and then deletes it; a delete without the marker is refused.
  `$BROKER_PSQL_URL` is the broker's database URL with the pgx-only parameters a deployment sets on it (`pool_max_conns`) and its password taken out, and `$BROKER_PGPASSWORD` that password, which `psql` reads from `PGPASSWORD` so it is never an argument of a command; the runbook's "Signing people in" gives the one block that makes them, and Dispatch's `$DISPATCH_PSQL_URL` and `$DISPATCH_PGPASSWORD` the blocks above use, from `BROKER_DATABASE_URL` and `DATABASE_URL`:

  ```sh
  PGPASSWORD="$BROKER_PGPASSWORD" psql "$BROKER_PSQL_URL" -v ON_ERROR_STOP=1 -v person='<address>' -v issuer='https://accounts.google.com' -v subject='<sub>' <<'SQL'
  begin;
  select set_config('broker.replaced_subject_delete', jsonb_build_array(pg_current_xact_id()::text, :'person', :'issuer', :'subject')::text, true);
  delete from replaced_person_subjects where person = :'person' and issuer = :'issuer' and subject = :'subject';
  commit;
  SQL
  ```

  The broker's `store.TestAnOperatorsClearLetsAReplacedAccountTradeAgain` runs this block from this file and the runbook.

**Reset or restore Dispatch's database and the broker's together, never one alone**, or replaced accounts keep live broker credentials, for the reason the runbook gives (`docs/site/src/content/docs/dispatch/running-dispatch.md`, "Signing people in").
A revocation the broker answers `superseded` (`envoy_dispatch_broker_revocations_superseded_total`, and the revocation stays owed) means the address's `user_sessions.generation` is below the one the broker recorded and its replaced account's credentials were not ended: a Dispatch database restored or made again alone.
The runbook's own page for it (`docs/site/src/content/docs/dispatch/after-a-restore.md`) gives the recovery, a reset of the whole deployment rather than a pass over chosen addresses: in one transaction it writes the deployment's floor (`session_generation_floors`, the row `*`, which every session row made later starts at, migration `20261119183355`), raises every session row to it, revokes every personal token, settles every owed revocation and records its start (`deployment_resets`, migration `20261120144819`, whose marks every reader takes in the order of their `id`), the number above the broker's greatest generation and the microseconds since the epoch at the raise, so a second restore cannot bring back the first reset's cookies; then it ends every person's credential at the broker at once, lifts a refusal there only for the account the Workspace directory names as the address's holder, ends every person's credential again no sooner than an hour and ten minutes after its start, and records its end once that wait has passed and its read of the broker finds none made before the wait ended.
When it is the broker's database that was restored alone, nothing is owed; the runbook's other block records again every revocation settled at or after the broker's backup less an hour's margin for the two clocks and after the last reset's start, which is what tells the broker of those replacements a second time, owes again in the same transaction every `machine_login_revocations` row settled since that moment less the hour, after taking each member's approval lock as a removal does and with `recorded_at` set to its own moment (the pass then revokes every machine login the member holds, an approve in flight as the block ran included, and their approvals wait for it), and a broker database made again takes the same block with `-infinity`; the reset's blocks run again when the backup's moment less the same hour is before the last reset's end.
A revocation an earlier head of this branch recorded with no `session_generation` is refused by the broker on every call (`400 SESSION_GENERATION_REQUIRED`) and holds back no other row of its address (`payableBrokerRevocation`); the runbook's record-again gives it the address's generation.
Migration `20261118152422`'s header says such a row is sent with none and held to a time window: that is wrong, as `20261119021826`'s header says.

Strays of the columns the guard leaves free, because their writers move them both ways, widen at most what a person reads, never for whom a credential acts:
`people.member_kind` set to `full` on an outside account's row, or a `confirmed_at` kept fresh, lets that person's personal tokens read as a full member's until the hourly confirmation next runs for them, which only a cookie request or a sign-in starts.

`../../../cmd/dispatch/README.md` has the rule for each surface ("Two builds at once") and for migrations with their examples ("Database migrations"), the naming and the identifiers a set is refused for, and what the census is held to.

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
Its columns, in this order, are a contract (`TestTheKeywordRelationAnswersTheContractColumns`): `kind`, `id`, `issue_key`, `project_key`, `event_id`, `event_seq`, `started_at`, `body`, `spec_artifact_id`, `spec_version`, `search` and `deleted_at`, which is null in this build.
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
