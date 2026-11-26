-- 20261123020640_eval_days_rules.up.sql
--
-- The rule each deploy repository's delivery and CI day rows stand built under (dispatch://INFRA-978,
-- D1), where eval_days_settings (20261117100021) records one rule for every repository. A project
-- holds deploy sources (20261123010254), each repository the source of one delivery-on project at
-- most, so each repository's rows of the evals that read the deploy repository
-- (eval_days.deploy_repo) are built under its own source's rule, and a project's read folds the rows
-- of each of its source repositories.
--
-- eval_days_rules holds one row per repository that is a source the refresher last brought its rows
-- to (evals.syncSettings, evals.syncKept):
--   repo         the repository, spelled as its rows are keyed (eval_days.deploy_repo).
--   project_key  the delivery-on project whose source reads it (delivery.DeployRules): a deploy of
--              it that ships no pull request counts for this project and no other (INFRA-978 R12),
--              and this project's read selects its rows (evals.DeployReposIn), so a read answers
--              the selection the rows were built for, the old one whole or the new one whole.
--   production_job_names, production_apply_step_name, staging_e2e_job_name, staging_apply_job_name
--              the four fields of its rule a day row reads (evals.settingsMoves says which evals
--              each moves). A pass that reads another rule, or another project, for the repository
--              rewrites that repository's days the difference moves, and no other repository's, in
--              one transaction that records the new row (evals.rewriteSettings).
--   kept_since the start of the repository's history the rows stand built from (the UTC day of its
--              first deploy or PR-checks run, evals.keptRepo), null while it holds no delivery
--              run and so no row. A page that finds a repository's first runs, its runs gone, its
--              start moved, or the repository no source any more builds or deletes its days in
--              the transaction of its cursors (evals.syncKept, span.go).
--   updated_at when the row was last recorded.
--
-- eval_days_settings stays as it is: the release before this one reads and writes it, and the
-- release after this one drops it with delivery_settings (dispatch://INFRA-2918's pull request).
-- Its one row is copied to the repository it names, under the project 20261123010254 gave the
-- delivery settings to (the project whose default repository it is), spelled as the rows of the
-- repository are kept where kept_repos holds it, with its start. A row naming no repository, or one
-- that is the default repository of no project or of more than one, is not copied: the refresher's
-- first pass then finds no rule recorded and rebuilds the rows aside under the rules it reads.
create table eval_days_rules (
  repo text primary key check (repo <> ''),
  project_key text not null,
  production_job_names text[] not null,
  production_apply_step_name text,
  staging_e2e_job_name text,
  staging_apply_job_name text,
  kept_since date,
  updated_at timestamptz not null default now()
);

insert into eval_days_rules (repo, project_key, production_job_names, production_apply_step_name,
  staging_e2e_job_name, staging_apply_job_name, kept_since, updated_at)
select coalesce(kept.repo, e.deploy_repo), p.key, e.production_job_names, e.production_apply_step_name,
  e.staging_e2e_job_name, e.staging_apply_job_name, kept.since, e.updated_at
from eval_days_settings e
join projects p on p.default_repo = lower(e.deploy_repo)
left join lateral (
  select k.repo, k.since from unnest(e.kept_repos, e.kept_since) as k(repo, since)
  where lower(k.repo) = lower(e.deploy_repo)
  order by k.repo limit 1
) kept on true
where e.deploy_repo <> ''
  and (select count(*) from projects q where q.default_repo = lower(e.deploy_repo)) = 1;
