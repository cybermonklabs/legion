-- 0043_guards_search_path.up.sql — the guards of the broker's record of replaced accounts resolve
-- their names and operators under a search_path of their own: pg_catalog, the schema the broker's
-- tables are in, pg_temp.
--
-- A plpgsql trigger function with no setting of its own runs under the caller's search_path.
-- Postgres searches a schema the session lists before pg_catalog for a function or an operator,
-- and the session's temporary schema before every schema of a path that does not name pg_temp, and
-- making either needs no right a writer does not already hold (CREATE on the database, and the
-- TEMP right PUBLIC holds). So each of these resolves at run time something a session can own:
--
--   guard_replaced_person_subjects (0041) compares `<` for the generation and reads
--     current_setting, jsonb_build_array and pg_current_xact_id for the operator's delete marker.
--   guard_person_replacement_generations (0041) compares `<`, the one rule that keeps the applied
--     generation of an address from moving back, which 0041's own comment says "lets a late revoke
--     apply again".
--   refuse_recorded_account_change (0042) raises alone, and still resolves its own call.
--
-- With a session operator `<` on bigint that answers false, the generation the broker applied at
-- an address was lowered, and a revoke Dispatch recorded before the last replacement applied
-- again, ending what the account that holds the address now made. The setting is built from
-- current_schema() in a DO block, as Dispatch's 20261119021826 does, because the broker's tables
-- are not always in public: the runner creates them in the schema RESET leaves on search_path.
--
-- It alters three functions' settings, writes into the catalogue alone, and reads, refuses and
-- rewrites no row.
do $$
begin
  execute format('alter function %I.guard_replaced_person_subjects() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
  execute format('alter function %I.guard_person_replacement_generations() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
  execute format('alter function %I.refuse_recorded_account_change() set search_path = pg_catalog, %I, pg_temp', current_schema(), current_schema());
end
$$;
