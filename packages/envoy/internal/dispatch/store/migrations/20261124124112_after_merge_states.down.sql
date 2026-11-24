-- 20261124124112_after_merge_states.down.sql
--
-- The rollback an operator runs by hand, with no process of the release that carries
-- 20261124124112 serving: the release before it reads none of what this drops. It loses every
-- stored hold, deploy, merge commit, merge time and check mark. The last statement deletes the
-- migration's version row, so the next boot of a release that carries it applies it again.
drop index check_results_live;
alter table check_results drop column check_program, drop column check_subject;
drop table deploys;
drop table commit_holds;
drop index pull_request_heads_merged_head_ref;
alter table pull_request_heads drop column merged_at, drop column merge_commit_sha;
alter table delivery_runs drop column run_attempt;
delete from schema_migrations where version = 20261124124112;
