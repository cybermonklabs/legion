-- 20261123010254_project_delivery_settings.up.sql
--
-- Each project's own delivery settings (dispatch://INFRA-978, R1, R2), where delivery_settings
-- (0079) holds one row for the whole deployment. project_delivery_settings holds one row per
-- project: the switch, the population rule, and the failure sources with the day they are counted
-- from. project_deploy_sources holds the project's deploy sources, one row each: a github_workflow
-- source names the repository of its workflow, or none, and then reads the project's default
-- repository (projects.default_repo) and moves with it; an events source reads the deploys posted
-- to production_events. A source row holds its rule and the bookkeeping the intake and the
-- reconcile write for its repository. No target is a column: each is the target of the project's
-- eval row (R3). That a repository is the source of one delivery-on project at most depends on the
-- projects' default repositories, so the settings write checks it under a lock of each repository
-- (delivery.PutProjectSettings); within one project, two sources naming one repository are refused
-- here.
--
-- delivery_settings and eval_days_settings stay as they are: the release before this one reads them
-- `where singleton` and writes them `on conflict (singleton)`, and a migration leaves that release
-- able to read and write the schema (store/README.md, "Two builds at once"). The release after
-- this one drops them (dispatch://INFRA-2918's pull request). A save made through the release
-- before this one during the roll writes delivery_settings, which this release no longer reads;
-- the person saves again on this release's settings tab.
--
-- The one existing delivery_settings row is copied to the project whose default repository is its
-- deploy_repo, with delivery on, no failure source, and one github_workflow source that names no
-- repository and holds the row's rule and bookkeeping. The census counts a row that names the
-- default repository of no project or of more than one, which this copy could not place, so a
-- deployment holding one sets that project's default repository first.
--
-- A trigger on each table logs one `settings` change per write that moves a column other than the
-- bookkeeping, carrying the project key in `repo`, through delivery_change_log (as 0106 and
-- 20261120111107 do for the singleton), and announces it on dispatch_delivery_settings with the
-- project key, which wakes the settings cache. Every insert and update of a source announces its
-- repository's freshness.
create table project_delivery_settings (
  project_key text primary key references projects(key) on delete cascade,
  delivery boolean not null default false,
  population_authors text[] not null default '{}',
  excluded_repos text[] not null default '{}',
  failure_sources text[] not null default '{}',
  failures_measured_since date,
  updated_by jsonb not null,
  updated_at timestamptz not null default now(),
  constraint project_delivery_settings_failure_sources check (
    failure_sources <@ array['check_results', 'reverts', 'incident_label', 'events']::text[]
    and array_position(failure_sources, null) is null
  ),
  -- A failure source counts from a day (R9): no row of a failure measure exists before it.
  constraint project_delivery_settings_failures_since check (
    cardinality(failure_sources) = 0 or failures_measured_since is not null
  )
);

create table project_deploy_sources (
  id uuid primary key default gen_random_uuid(),
  project_key text not null references project_delivery_settings(project_key) on delete cascade,
  position integer not null check (position >= 0),
  kind text not null,
  -- null reads the project's default repository; checked as projects.default_repo is
  -- (20261119000230).
  repo text check (repo <> '' and repo = lower(repo)),
  deploy_workflow_path text,
  production_job_names text[],
  production_apply_step_name text,
  staging_e2e_job_name text,
  staging_apply_job_name text,
  pr_checks_workflow_path text,
  pr_checks_rollup_job_name text,
  last_event_at timestamptz,
  last_reconcile_at timestamptz,
  last_error text,
  unique (project_key, position),
  constraint project_deploy_sources_kind check (kind in ('github_workflow', 'events')),
  -- A github_workflow source reads its deploys from the workflow and its jobs, so it names them; an
  -- events source reads the record of production events and names none of them.
  constraint project_deploy_sources_workflow check (
    case kind
      when 'github_workflow' then coalesce(deploy_workflow_path, '') <> '' and production_job_names is not null
        and coalesce(pr_checks_workflow_path, '') <> ''
      else repo is null and deploy_workflow_path is null and production_job_names is null
        and production_apply_step_name is null and staging_e2e_job_name is null
        and staging_apply_job_name is null and pr_checks_workflow_path is null
        and pr_checks_rollup_job_name is null
    end
  ),
  -- As 0109 checks delivery_settings.production_job_names.
  constraint project_deploy_sources_production_job_names check (
    production_job_names is null
    or (cardinality(production_job_names) >= 1 and array_position(production_job_names, null) is null)
  )
);

-- A project's github_workflow sources read one repository each: two naming none both read the
-- default repository, so a null counts as one repository here.
create unique index project_deploy_sources_one_per_repo on project_deploy_sources (project_key, coalesce(repo, ''))
  where kind = 'github_workflow';

-- A project reads its posted deploys once.
create unique index project_deploy_sources_one_events on project_deploy_sources (project_key) where kind = 'events';

insert into project_delivery_settings (project_key, delivery, population_authors, excluded_repos, updated_by, updated_at)
select p.key, true, d.population_authors, d.excluded_repos, d.updated_by, d.updated_at
from delivery_settings d
join projects p on p.default_repo = lower(d.deploy_repo)
where (select count(*) from projects q where q.default_repo = lower(d.deploy_repo)) = 1;

insert into project_deploy_sources (
  project_key, position, kind, repo, deploy_workflow_path, production_job_names, production_apply_step_name,
  staging_e2e_job_name, staging_apply_job_name, pr_checks_workflow_path, pr_checks_rollup_job_name,
  last_event_at, last_reconcile_at, last_error
)
select s.project_key, 0, 'github_workflow', null, d.deploy_workflow_path, d.production_job_names,
  d.production_apply_step_name, d.staging_e2e_job_name, d.staging_apply_job_name, d.pr_checks_workflow_path,
  d.pr_checks_rollup_job_name, d.last_event_at, d.last_reconcile_at, d.last_error
from delivery_settings d
join projects p on p.default_repo = lower(d.deploy_repo)
join project_delivery_settings s on s.project_key = p.key;

create function project_delivery_settings_changed() returns trigger language plpgsql as $$
declare
  bookkeeping constant text[] := array['last_event_at', 'last_reconcile_at', 'last_error', 'updated_at', 'updated_by'];
  project text := coalesce(new.project_key, old.project_key);
  before_first date;
  after_first date;
  change_ref text;
begin
  if tg_op <> 'UPDATE' or (to_jsonb(old) - bookkeeping) is distinct from (to_jsonb(new) - bookkeeping) then
    if tg_table_name = 'project_delivery_settings' then
      if tg_op <> 'INSERT' and old.delivery and cardinality(old.failure_sources) > 0 then
        before_first := old.failures_measured_since;
      end if;
      if tg_op <> 'DELETE' and new.delivery and cardinality(new.failure_sources) > 0 then
        after_first := new.failures_measured_since;
      end if;
      if before_first is distinct from after_first then
        -- The eval refresher may need to announce a moved data_since even without day rows.
        change_ref := 'failure-first-day';
      end if;
    end if;
    perform delivery_change_log('settings', null, project, change_ref, null, null);
    perform pg_notify('dispatch_delivery_settings', project);
  end if;
  if tg_table_name = 'project_deploy_sources' and tg_op <> 'DELETE' then
    perform pg_notify('delivery_freshness', json_build_object(
      'project', new.project_key, 'repo', new.repo,
      'last_event_at', new.last_event_at, 'last_reconcile_at', new.last_reconcile_at,
      'last_error', left(new.last_error, 1000))::text);
  end if;
  return null;
end $$;

create trigger project_delivery_settings_changed after insert or update or delete on project_delivery_settings
  for each row execute function project_delivery_settings_changed();

create trigger project_deploy_sources_changed after insert or update or delete on project_deploy_sources
  for each row execute function project_delivery_settings_changed();
