-- 20261123072431_task_copy_remove.up.sql — the outbox kind that removes the working copy of a task
-- that a later version of the plan removed (task_copy_remove, record.TaskCopyRemove).
--
-- A later version that removes a waiting or stopped task deletes its rows of the plan; the task's
-- working copy, the jj workspace on its branch beside the issue's own working copy, stays on the
-- daemon's host until a row of this kind, queued in the version's transaction, removes it. The
-- migration touches no table but the outbox's check.
--
-- The outbox check is one list, and main writes it whole on both sides of this version:
-- 20261122154643 (the basis seeds) below it and 20261126110259 (the merge holds change) above it.
-- So this migration writes no list of its own, as 20261121165047 and 20261122122826 write none: it
-- adds task_copy_remove to the list the database holds, whichever migration wrote it. On a database
-- main's release migrated, that list keeps every kind main's daemon queues, merge_holds_changed
-- among them; on a fresh database merge_holds_changed is still refused below 20261126110259, which
-- adds it (TestTheOutboxTakesTheMergeHoldsKindWith20261126110259). 20261126194217 writes the list
-- again above main's lists with this kind in it.
--
-- There is no down file. This directory keeps none, so the way back is the transaction that ends
-- this header, its last paragraph (migrations.WayBack), run with the daemon stopped, before the
-- earlier image is pinned, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1, before the way back
-- of any earlier migration. The release before decodes no task_copy_remove row, so the block
-- deletes each such row first, since the check written again without the kind refuses a row that
-- holds it; then takes the kind out of the list the database holds, which keeps every other kind,
-- main's among them; and deletes its version row, which is owed to the roll-forward, so that it
-- runs this file again. What the way back costs is the removal of each working copy such a row
-- named: the copy stays on the host until the issue's own workspace is removed.
--
-- begin;
-- delete from outbox where kind = 'task_copy_remove';
-- do $$
-- declare
--   kinds text[] := array(select k.m[1] from pg_constraint c,
--     regexp_matches(pg_get_constraintdef(c.oid), '''([^'']+)''::text', 'g') with ordinality k(m, n)
--     where c.conrelid = 'outbox'::regclass and c.conname = 'outbox_kind_check' order by k.n);
-- begin
--   alter table outbox drop constraint outbox_kind_check;
--   execute format('alter table outbox add constraint outbox_kind_check check (kind in (%s))',
--     (select string_agg(quote_literal(u.kind), ', ' order by u.n)
--       from unnest(array_remove(kinds, 'task_copy_remove')) with ordinality u(kind, n)));
-- end
-- $$;
-- delete from schema_version where version = 20261123072431;
-- commit;
do $$
declare
  kinds text[] := array(select k.m[1] from pg_constraint c,
    regexp_matches(pg_get_constraintdef(c.oid), '''([^'']+)''::text', 'g') with ordinality k(m, n)
    where c.conrelid = 'outbox'::regclass and c.conname = 'outbox_kind_check' order by k.n);
begin
  if cardinality(kinds) = 0 then
    raise exception 'outbox_kind_check names no kind, so this migration has no list to add task_copy_remove to';
  end if;
  alter table outbox drop constraint outbox_kind_check;
  execute format('alter table outbox add constraint outbox_kind_check check (kind in (%s))',
    (select string_agg(quote_literal(u.kind), ', ' order by u.n)
      from unnest(array_remove(kinds, 'task_copy_remove') || 'task_copy_remove'::text) with ordinality u(kind, n)));
end
$$;
