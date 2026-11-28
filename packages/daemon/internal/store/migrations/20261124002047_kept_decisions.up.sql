-- 20261124002047_kept_decisions.up.sql — a person's decision kept for a tree that does not run,
-- under the names of what it is.
--
-- The table of kept decisions takes the name kept_decisions, and with it its index, its primary
-- key, its two foreign keys and, on PostgreSQL 18 and later, the NOT NULL constraints that
-- PostgreSQL names after the table. The kind of the controller notice that relays such a decision
-- becomes kept-decision in each queued row. The processed-event source that keeps one decision to
-- one notice takes the same value, so that a spec decision whose notice the release before
-- relayed is not relayed again when its undelivered row runs after the upgrade. Each queued
-- tick's held reason for a member of a tree whose root is in backlog or icebox becomes
-- tree_in_backlog_or_icebox. Every other row and value stays as it is: a tick's other reasons,
-- and the source kept-told that marks a catch-up's telling.
--
-- Migration 20261101163106 keeps its file and its text. A new database runs it, and
-- internal/daemon's cutover test (cutover_merger_repair_test.go) holds its name to the list of
-- migrations that legion-v19.57.3 shipped. This file renames what that migration made.
--
-- The outbox rows must move with the names. This release decodes an outbox payload strictly and
-- refuses a controller notice of a kind it does not know (record.DecodeOutboxPayload), so a row
-- left in the old kind would fail at every attempt. The release before refuses the new kind in the
-- same way, which is why the way back moves the rows back too.
--
-- PostgreSQL 18 names a NOT NULL constraint after its table and column. PostgreSQL before 18
-- records NOT NULL in pg_attribute alone, so there the do block finds no row and renames nothing.
--
-- There is no down file. This directory keeps none — migrations here are forward-only and no
-- .down.sql file exists — so the way back is the one transaction that closes this header, its last
-- paragraph (migrations.WayBack), run with every daemon of the database stopped and before the
-- release before starts, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1. A daemon of the
-- release before that still runs on the database fails each read of the table after the rename,
-- and a daemon of this release fails each read of it after the way back, so no daemon of one
-- release runs on the database while the other release's names stand. docs/kubernetes.md carries
-- the same block under "Rolling back the release that renames the kept decisions", and
-- internal/textcheck's TestTheKeptDecisionsWayBackIsOneTextInTheHeaderAndTheGuide holds the two to
-- one text.
-- internal/store's TestTheKeptDecisionsTakeTheirNamesAndTheirWayBackGivesTheOldOnesAgain runs it.
--
-- The delete of this migration's row is owed to the roll forward. With the names moved back and
-- the row gone, store.apply finds no row of this version and runs this file again.
--
-- begin;
-- update outbox set payload = jsonb_set(payload, '{held}', (
--     select jsonb_agg(case when held->>'reason' = 'tree_in_backlog_or_icebox'
--       then jsonb_set(held, '{reason}', '"tree_parked"') else held end order by place)
--     from jsonb_array_elements(payload->'held') with ordinality as listed(held, place)))
--   where kind = 'controller_notice' and payload @? '$.held[*] ? (@.reason == "tree_in_backlog_or_icebox")';
-- update processed_events set source = 'parked-decision' where source = 'kept-decision';
-- update outbox set payload = jsonb_set(payload, '{kind}', '"parked-decision"')
--   where kind = 'controller_notice' and payload->>'kind' = 'kept-decision';
-- do $$
-- declare
--   named record;
-- begin
--   for named in select conname from pg_constraint
--     where conrelid = 'kept_decisions'::regclass and contype = 'n' and conname like 'kept\_decisions\_%'
--   loop
--     execute format('alter table kept_decisions rename constraint %I to %I',
--       named.conname, 'parked_decisions_' || substr(named.conname, length('kept_decisions_') + 1));
--   end loop;
-- end $$;
-- alter table kept_decisions rename constraint kept_decisions_claimed_by_fkey to parked_decisions_claimed_by_fkey;
-- alter table kept_decisions rename constraint kept_decisions_issue_fkey to parked_decisions_issue_fkey;
-- alter table kept_decisions rename constraint kept_decisions_pkey to parked_decisions_pkey;
-- alter index kept_decisions_issue rename to parked_decisions_issue;
-- alter table kept_decisions rename to parked_decisions;
-- delete from schema_version where version = 20261124002047;
-- commit;
alter table parked_decisions rename to kept_decisions;
alter index parked_decisions_issue rename to kept_decisions_issue;
alter table kept_decisions rename constraint parked_decisions_pkey to kept_decisions_pkey;
alter table kept_decisions rename constraint parked_decisions_issue_fkey to kept_decisions_issue_fkey;
alter table kept_decisions rename constraint parked_decisions_claimed_by_fkey to kept_decisions_claimed_by_fkey;
do $$
declare
  named record;
begin
  for named in select conname from pg_constraint
    where conrelid = 'kept_decisions'::regclass and contype = 'n' and conname like 'parked\_decisions\_%'
  loop
    execute format('alter table kept_decisions rename constraint %I to %I',
      named.conname, 'kept_decisions_' || substr(named.conname, length('parked_decisions_') + 1));
  end loop;
end $$;
update outbox set payload = jsonb_set(payload, '{kind}', '"kept-decision"')
  where kind = 'controller_notice' and payload->>'kind' = 'parked-decision';
update processed_events set source = 'kept-decision' where source = 'parked-decision';
update outbox set payload = jsonb_set(payload, '{held}', (
    select jsonb_agg(case when held->>'reason' = 'tree_parked'
      then jsonb_set(held, '{reason}', '"tree_in_backlog_or_icebox"') else held end order by place)
    from jsonb_array_elements(payload->'held') with ordinality as listed(held, place)))
  where kind = 'controller_notice' and payload @? '$.held[*] ? (@.reason == "tree_parked")';
