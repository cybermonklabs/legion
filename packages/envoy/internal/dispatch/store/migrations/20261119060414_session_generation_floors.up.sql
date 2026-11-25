-- 20261119060414_session_generation_floors.up.sql — the generation an address's first session row
-- starts at after a Dispatch database is restored or made again alone.
--
-- The secrets broker refuses a machine-login approve whose session generation is below one a
-- revocation recorded at the address, and answers a revocation below the greatest it applied there
-- superseded. A Dispatch database made again, or restored to an earlier point, holds generations
-- below the broker's. An address with no user_sessions row had nothing to raise: the row cannot be
-- made by hand (user_sessions_login_fkey needs a people row, which only a sign-in makes), and the
-- first sign-in there made the row at generation 1 (end_people_sessions_on_new_account) — below the
-- broker's, so every approve at the address was refused 403 APPROVER_REPLACED and every revocation
-- answered superseded. After a database made again that is every address the broker holds a
-- generation for.
--
-- The reset after a Dispatch database restored or made again alone writes one deployment floor,
-- login `*`, above every generation the lost history can have issued: the greatest of the
-- microseconds at its raise, the broker's greatest recorded generation plus one, and every session
-- row and floor Dispatch holds plus one (running-dispatch.md, "Signing people in"). This file's
-- start_session_generation_at_floor runs before insert on user_sessions, which the sign-in's
-- trigger, PgSessionStore.EnsureSession and every other writer pass through, and starts the row at
-- its address's floor; 20261119183355 adds the deployment floor's arm, so a row starts at the
-- greater of the two. So no writer can make
-- the row below the deployment floor, whichever runs first and however long after the reset. A row
-- that already exists is raised by the reset's update, and a generation only rises
-- (user_sessions_binding_columns), so a floor never lowers one.
--
-- The floor is above the broker's generation, not merely its own number. At the broker's own number
-- every approve would be admitted (the broker refuses an approve whose point is *below* a
-- replacement's generation and supersedes a revocation *below* the greatest it applied), but every
-- cookie the replacement that left that generation issued would match the new row as well, a
-- signed-out one included. The time term keeps a later reset after a second restore above the first
-- one's floor. This table holds the deployment floor, the row `*`.
--
--   session_generation_floors.login        written once, at the insert.
--   session_generation_floors.generation   only rises: a later reset must never let the next row
--                                          start below an earlier floor.
--   session_generation_floors row          never deleted or truncated, for the same reason.
--
-- guard_session_generation_floors refuses every other write with SQLSTATE 23514 (check_violation).
-- Both functions name tables and functions without a schema, so the DO block below sets each one's
-- search_path to pg_catalog, the schema this migration runs in, then pg_temp, as 20261119021826
-- does for the clock guards: a temporary table named session_generation_floors would otherwise
-- decide where a session row starts. The migration makes an empty table, two functions and three
-- triggers, written into the catalogue alone, and reads, refuses and rewrites no row. The function
-- names are Dispatch's own: the broker's migrations may share this schema
-- (TestTheBrokerAndDispatchMigrationsCreateNoNameInCommon).
-- This header was edited in place after the migration identity check arrived, twice (round 26 of
-- its review, then these lines), and the two lines below name the texts before each edit, so a
-- database that applied either still boots.
-- replaces: 20261119060414_session_generation_floors.up.sql sha256:bd9d44ffd84309bec3bfd73529bdad12550331a175e246c383ff8e600429443d
-- replaces: 20261119060414_session_generation_floors.up.sql sha256:2b187b6569dd926846dceb59513c2d701385d6491fe8c95d6e5838ddd156540d
create table session_generation_floors (
  login text primary key check (login <> '' and login = lower(login)),
  generation bigint not null check (generation > 0)
);

create function guard_session_generation_floors() returns trigger language plpgsql as $$
begin
  if tg_op in ('DELETE', 'TRUNCATE') then
    raise exception 'session_generation_floors holds the floor an address''s session row starts at, written by a reset or a later writer, and its rows are never removed: a row removed lets the next sign-in start below that floor'
      using errcode = 'check_violation';
  end if;
  if new.login is distinct from old.login then
    raise exception 'session_generation_floors.login names the address a floor belongs to, and it never changes'
      using errcode = 'check_violation';
  end if;
  if new.generation < old.generation then
    raise exception 'session_generation_floors.generation is the floor an address''s session row starts at, written by a reset or a later writer, and it only rises'
      using errcode = 'check_violation';
  end if;
  return new;
end
$$;

create trigger session_generation_floors_guarded before update or delete on session_generation_floors
  for each row execute function guard_session_generation_floors();

create trigger session_generation_floors_not_truncated before truncate on session_generation_floors
  for each statement execute function guard_session_generation_floors();

create function start_session_generation_at_floor() returns trigger language plpgsql as $$
declare
  start_at bigint;
begin
  select generation into start_at from session_generation_floors where login = new.login;
  if start_at > new.generation then
    new.generation := start_at;
  end if;
  return new;
end
$$;

create trigger user_sessions_start_at_floor before insert on user_sessions
  for each row execute function start_session_generation_at_floor();

do $$
begin
  execute format('alter function %I.guard_session_generation_floors() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
  execute format('alter function %I.start_session_generation_at_floor() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
end
$$;
