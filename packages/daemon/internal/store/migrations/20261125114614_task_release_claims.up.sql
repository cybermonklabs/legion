-- With both daemons stopped, reverse this migration before reversing task_agent_phases. Finish
-- in-flight pod releases first: the earlier daemon cannot identify the exact suspended claims.
-- The way back refuses a nonempty claim set rather than silently losing its cancellation work:
--
--   psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1 <<'SQL'
--   begin;
--   lock table issue_releases in access exclusive mode;
--   do $$
--   begin
--     if exists (select 1 from issue_releases where cardinality(suspended_claims) > 0) then
--       raise exception 'Finish in-flight pod releases before reversing task_release_claims';
--     end if;
--   end $$;
--   alter table issue_releases drop column suspended_claims;
--   delete from schema_version where version = 20261125114614;
--   commit;
--   SQL
--
lock table issue_releases in access exclusive mode;
do $$
begin
  if exists (select 1 from issue_releases where cardinality(suspended) > 0) then
    raise exception 'Finish in-flight pod releases and stop the earlier daemon before applying task_release_claims';
  end if;
end $$;
alter table issue_releases add column suspended_claims text[] not null default '{}';
