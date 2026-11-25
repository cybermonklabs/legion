-- 20261123154051_production_check_of_each_merge.up.sql — a production check for each merge of an
-- issue.
--
-- An issue whose plan merges the work of several tasks merges several times in one run, and each
-- merge opens its own check beside the others, so a check is keyed by its issue and the moment it
-- opened, which is already its identity (record.ProductionCheck.ID). generation is the issue's run
-- at the merge, which tells a check of the run that still holds the issue from a check of an
-- earlier run. task is the task whose merge the check checks, '' for an issue with no plan rows,
-- as the record reads a pull request's task (record.PullRequest.Task). Every row the upgrade
-- finds was written by a release that kept one check for each issue and ran no plan, and takes
-- the defaults: run 0 and no task.
--
-- A daemon of the release before reads the table by issue alone and writes it with
-- `on conflict (issue)`, which fails while the key is (issue, opened_at). So the way back keeps
-- the newest check of each issue, the one that release reads, and puts the key by issue back. An
-- older check of the issue still open is lost with its row; this lists each issue that holds one,
-- to run the way back after its older checks have closed:
-- select issue from production_checks where closed_at is null group by issue having count(*) > 1;
--
-- There is no down file. This directory keeps none, so the way back is the one transaction that
-- closes this header, its last paragraph (migrations.WayBack), run with the daemon stopped and
-- before the earlier image is pinned, through psql '<postgres_dsn>' -X -v ON_ERROR_STOP=1.
--
-- begin;
-- delete from production_checks p where exists (select 1 from production_checks q where q.issue = p.issue and q.opened_at > p.opened_at);
-- alter table production_checks drop constraint production_checks_pkey;
-- alter table production_checks add primary key (issue);
-- alter table production_checks drop column generation, drop column task;
-- delete from schema_version where version = 20261123154051;
-- commit;
alter table production_checks drop constraint production_checks_pkey;
alter table production_checks add primary key (issue, opened_at);
alter table production_checks add column generation bigint not null default 0;
alter table production_checks add column task text not null default '';
