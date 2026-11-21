-- 20261119021826_clock_guards_search_path.up.sql — the two clock guards resolve their names under
-- a search_path of their own: pg_catalog, the schema Dispatch's tables are in, pg_temp.
--
-- guard_people_binding_columns and guard_broker_revocation_columns (20261118200651) name
-- clock_timestamp(), and the second people and user_sessions, without a schema, and a trigger
-- function runs under the session's search_path unless it sets its own. So a session could point
-- the guard at a clock or a table of its own: a schema of its own listed before pg_catalog, holding
-- a clock_timestamp() that runs ahead, lets a stamp later than the database's clock through; and a
-- temporary table named people, which Postgres searches before every schema of a path that does
-- not list pg_temp, lets a revocation name a holder the address does not record. This sets each
-- function's search_path to pg_catalog first, so no other schema's function takes the call, then the
-- schema the tables are in, then pg_temp, so a temporary table is found last, if at all.
--
-- The tables' schema is not always public: the runner applies each migration under the value RESET
-- gives search_path (pgmigrate.Pin), which a database's or a role's default can set to another
-- schema first, and the tables are created in current_schema() under it. So the setting is built
-- from current_schema() as this migration runs, quoted with %I, in a DO block; a fixed public would
-- make every write of broker_revocations fail with 42P01 where the tables are elsewhere. (Dispatch's
-- owner lock, 20261115200014, names the schema public, so a release with it keeps its tables there;
-- this setting does not rest on that.)
--
-- It is a migration of its own because 20261118200651 shipped without the setting, and the migrator
-- records a version with no checksum: a database that ran 20261118200651 takes this one. It alters
-- the two functions' settings alone, writes into the catalogue alone and reads, refuses and
-- rewrites no row; the census exempts no DO block, since it cannot read what one runs, but this one
-- names no table, so nothing is measured or refused, and it takes the exemptions of any migration
-- pending after it in the same deploy (TestCensusAnswersTheClockGuardsSearchPathMigration).
--
-- A correction, since 20261118152422 has shipped and is not edited: its header says that a
-- broker_revocations row recorded before it, which holds no session_generation, is sent with none
-- and that the broker then holds an approve against that replacement to a time window. That is
-- wrong. The broker requires a generation on every revoke (400 SESSION_GENERATION_REQUIRED), so
-- such a row is refused on every call and stays owed; it holds back no other row of its address
-- (store.payableBrokerRevocation pays it last and counts neither its owing nor its claim), and the
-- runbook's record-again gives it the address's generation, after which the loop pays it.
do $$
begin
  execute format('alter function %I.guard_people_binding_columns() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
  execute format('alter function %I.guard_broker_revocation_columns() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
end
$$;
