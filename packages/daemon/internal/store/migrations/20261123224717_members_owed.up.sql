-- 20261123224717_members_owed.up.sql — the mark that a re-admitted tree's members have still to
-- come back (dispatch://INFRA-5471).
--
-- issues.members_owed is true on a root whose re-admission has started a generation and whose tree
-- has not yet run in it: admission's readmit sets it, and the first moment the root stops being a
-- queued root clears it, by the root's promotion from the waiting line or by its design gate
-- opening first. At that moment the daemon brings back the members the re-admission left mid-phase,
-- and the members reopened at done, as the promotion always has (admit's startMidPhaseMembers and
-- reenterStrandedChildren), once in the generation. While the mark stands and the tree's lifecycle
-- is not open under the workflow (its cleanup reserved, as after a linger that ended), the gate's
-- opening leaves the root queued, so the promotion that opens the tree's next epoch brings the
-- members back (record's ran).
--
-- Every row takes the default, false, but the roots whose re-admission has still to bring their
-- members back: a root of its own tree standing in todo at admitted on a generation past the first,
-- holding no slot, released for no wait and with no linger, whose tree does not already run under
-- an ever-opened gate. A root the release before promoted stands in progress or holds the slot its
-- promotion granted, so it stays false, and the first pass of this release starts nothing for a
-- generation that already ran. A root whose gate opened before its promotion while its tree's
-- lifecycle stood open stays false as well: its tree already runs, and the members that tree left
-- suspended are the operator's to resume, as on the release before. A root whose gate opened while
-- its tree's lifecycle was not open is marked, and so queues again until its promotion.
--
-- The release before this one names each column of the table it writes (record's PutIssue) and
-- reads none of this one, so every row it inserts takes the default.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction that closes this header, its last
-- paragraph (migrations.WayBack), run with every daemon of the database stopped and before the
-- release before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1. issues holds the rows
-- of every project that shares the database, and a daemon of this release reads the column in
-- every read of a record (record's issueColumns), so one still running for another project after
-- the drop would fail every fact it applies.
--
-- The drop and the delete of this migration's row go together. The release before this one needs
-- neither to run, since it never names the column: the delete is what makes the version the store
-- reports, its highest recorded row (store.SchemaVersionTx), that release's own again. Both are
-- owed to the roll-forward. With the column dropped and the row deleted, store.apply finds no row
-- of this version and runs this file again, which adds the column again and marks the roots that
-- still wait as the paragraph above says; an add of a column that stands would fail.
--
-- begin;
-- alter table issues drop column if exists members_owed;
-- delete from schema_version where version = 20261123224717;
-- commit;
alter table issues add column members_owed boolean not null default false;

update issues set members_owed = true
 where key = tree and generation > 1 and status = 'todo' and phase = 'admitted' and linger_until is null
   and released_for_wait_at is null
   and not exists (select 1 from slots where slots.issue = issues.key)
   and not (exists (select 1 from tree_lifecycles lifecycle
                     where lifecycle.tree = issues.key and lifecycle.authority = 'workflow' and not lifecycle.cleanup_started)
            and exists (select 1 from design_gates gate where gate.issue = issues.key and gate.ever_opened));
