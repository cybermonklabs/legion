-- 20261120144819_deployment_resets.up.sql — the moments a reset of the whole deployment started and
-- ended, which the reset writes itself and the recovery after a broker restored alone reads.
--
-- After a Dispatch database restored or made again alone, the runbook's reset
-- (docs/site/src/content/docs/dispatch/running-dispatch.md, "Signing people in") settles every
-- owed broker revocation in its first transaction and ends every person's credential at the
-- broker. A broker restored alone later, to a backup from before or during that reset, brings back
-- what the reset ended, and the recovery must know when the reset started and when it ended:
-- record-again tells the broker again only what Dispatch settled after the reset's start (the
-- reset's own settlements are the ones it chose not to pay), and the reset's credentials and
-- directory blocks run again whenever the backup may predate the reset's end. The deployment's
-- floor (session_generation_floors, the row '*') cannot say when: its number is a time only while
-- the clock term of its greatest wins, and a broker generation or a floor above the clock, or a
-- clock set back between two resets, moves it.
--
-- So the reset writes its own two moments here, each with the transaction's own now(): a 'start'
-- row in the first block's transaction, so it is exactly the done_at of every revocation that
-- transaction settles, and an 'end' row in the block that closes the reset once its wait has passed
-- and its last read of the broker is clean. Every reader takes the newest mark by id, never by at,
-- since a clock set back between two resets would put the newer reset's moments before the older
-- one's. The table only grows:
--
--   deployment_resets.mark   'start' or 'end', written once, at the insert; an 'end' only while
--                            the newest mark by id is a 'start' at least an hour and ten minutes
--                            old, the close's own wait, so an end closes the open reset and no
--                            other, and never inside its wait.
--   deployment_resets.at     the inserting transaction's now(), written once, at the insert: a
--                            moment written by hand would move a bound the recovery reads.
--   deployment_resets row    never updated, deleted or truncated: a moment removed or moved lets
--                            record-again tell the broker of a settlement the reset chose not to
--                            pay, or skip the reset's blocks over a backup from before its end.
--
-- guard_deployment_resets refuses every other write with SQLSTATE 23514 (check_violation). It
-- reads this table without a schema, and the DO block below sets its search_path to pg_catalog,
-- the schema this migration runs in, then pg_temp, as 20261119060414 does for the floors' guard, so
-- neither a session's own table of that name nor its now() or operator can decide its comparison.
-- The migration makes an empty table, a function and two triggers, written into the catalogue
-- alone, and reads, refuses and rewrites no row. The names are Dispatch's own: the broker's
-- migrations may share this schema (TestTheBrokerAndDispatchMigrationsCreateNoNameInCommon).
create table deployment_resets (
  id bigint generated always as identity primary key,
  mark text not null check (mark in ('start', 'end')),
  at timestamptz not null default now()
);

create function guard_deployment_resets() returns trigger language plpgsql as $$
begin
  if tg_op <> 'INSERT' then
    raise exception 'deployment_resets holds the moments a reset started and ended, which the recovery after a broker restored alone reads, and its rows are never changed or removed'
      using errcode = 'check_violation';
  end if;
  if new.at is distinct from now() then
    raise exception 'deployment_resets.at is the moment of the reset''s own transaction, now(), never a moment written by hand'
      using errcode = 'check_violation';
  end if;
  if new.mark = 'end' and not exists (
    select 1 from (select mark, at from deployment_resets order by id desc limit 1) newest
    where newest.mark = 'start' and newest.at <= now() - interval '1 hour 10 minutes'
  ) then
    raise exception 'deployment_resets takes an end only for the open reset, whose start is the newest mark, once an hour and ten minutes have passed since that start'
      using errcode = 'check_violation';
  end if;
  return new;
end
$$;

create trigger deployment_resets_guarded before insert or update or delete on deployment_resets
  for each row execute function guard_deployment_resets();

create trigger deployment_resets_not_truncated before truncate on deployment_resets
  for each statement execute function guard_deployment_resets();

do $$
begin
  execute format('alter function %I.guard_deployment_resets() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
end
$$;
