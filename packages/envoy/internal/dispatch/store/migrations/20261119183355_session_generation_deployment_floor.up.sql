-- 20261119183355_session_generation_deployment_floor.up.sql — a new session row starts at the
-- deployment's floor as well as at its address's.
--
-- After a Dispatch database restored or made again alone, the runbook's reset
-- (docs/site/src/content/docs/dispatch/running-dispatch.md, "Signing people in") ends every
-- browser session by raising every address's session generation above every generation the lost
-- history can have issued. An address the restored database holds no session row for - one first
-- signed in after the backup, or every address of a database made again - has no row to raise, and
-- its next sign-in makes the row at 1 (end_people_sessions_on_new_account) or 0
-- (PgSessionStore.EnsureSession), where a cookie the lost history issued at that address may match
-- again: the cookie of another account that held the address after the backup among them. So the
-- reset writes its number as the deployment's floor too, the row of session_generation_floors
-- whose login is '*', and start_session_generation_at_floor, the trigger before every insert into
-- user_sessions (20261119060414), now starts a new row at the greatest of the row's own
-- generation, its address's floor and the deployment's.
--
-- '*' names no address: every address the table holds is a lowercase email or login, which never
-- is '*'. The table's guard (20261119060414) holds the deployment's floor as it holds an
-- address's: its login written once, its generation only raised, the row never deleted or
-- truncated. The trigger reads one more row by its key per insert into user_sessions.
--
-- A create or replace drops a function's own settings, so the DO block pins its search_path again,
-- to pg_catalog, the schema the tables are in, then pg_temp, as 20261119060414 did. The migration
-- replaces one function and alters its settings, written into the catalogue alone, and reads,
-- refuses and rewrites no row; the census exempts no DO block, since it cannot read what one runs,
-- but this one names no table, so nothing is measured or refused.
create or replace function start_session_generation_at_floor() returns trigger language plpgsql as $$
declare
  start_at bigint;
begin
  select max(generation) into start_at from session_generation_floors where login in (new.login, '*');
  if start_at > new.generation then
    new.generation := start_at;
  end if;
  return new;
end
$$;

do $$
begin
  execute format('alter function %I.start_session_generation_at_floor() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
end
$$;
