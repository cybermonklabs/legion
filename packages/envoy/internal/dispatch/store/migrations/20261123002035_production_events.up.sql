-- 20261123002035_production_events.up.sql
--
-- One record of every deploy and every failed change of a project (dispatch://INFRA-978, R6 and
-- R7; root dispatch://INFRA-979, R9 and R13): what the three DORA failure measures count
-- (INFRA-978 R11). A deploy row is one deploy run of a deploy repository, written by the GitHub
-- adapter under the repository's owner project when that project's rule calls the run a deploy,
-- or one deploy a system posted to the project's route. A failure row is one failure signal: a
-- failed after-merge check, a merged revert, an incident issue, a posted failure, or a person's
-- flag; it names the deploy it blames.
--
-- The columns:
--   id               the row's own id.
--   project_key      the project the row was written under: a GitHub deploy's repository's owner,
--                    a posted row's route, a failure signal's project. Which projects a deploy
--                    counts for is the attribution's (dispatch://INFRA-2916, R15), read when the
--                    day rows are built, not this column.
--   kind             deploy or failure.
--   environment      the deployment environment, production unless a system posted another
--                    (OTel deployment.environment.name).
--   status           the system's own status word (OTel deployment.status), stored as sent.
--   source           which writer made the row: github (the adapter), api (a system's post),
--                    person (a person's flag), check_results, reverts, incident_label (the three
--                    automatic signals).
--   source_ref       the writer's own key for the row; one row per project and source_ref, so a
--                    signal sent twice writes one row.
--   started_at, finished_at   a deploy's run creation and its production job's completion; a
--                    deploy counts on its finished_at day. A failure leaves both null.
--   repo, commits, pull_requests, url   a deploy's repository, the commits it shipped (the run's
--                    head and each shipped pull request's merge commit), the pull requests it
--                    shipped as owner/repo#n, and the run's page; a failure's pull requests are
--                    the ones it blames.
--   actor            who or what wrote the row.
--   deploy_id        a failure's blamed deploy row, which may be another project's.
--   signal           a failure's signal: check_fail, revert, incident_issue, api, person.
--   detected_at      when the signal arrived.
--   state            counted or rejected; every signal counts when it arrives (R8). A reject
--                    keeps the row with state_reason, state_by and state_at, and a re-count
--                    writes its own reason, writer and time over them.
--   restored_at, restored_by, restored_by_deploy   when the failure was restored, who said so,
--                    and the deploy that restored it where one did (a revert's own deploy).
--
-- The indexes:
--   production_events_window, (project_key, kind, finished_at desc), is the read of a project's
--     deploys in a window, newest first (R6).
--   production_events_posted, (project_key, finished_at) over the deploys a system posted (source
--     api), is the read of a project's posted deploys, which the delivery day rows of a project
--     with an events source read.
--   production_events_commits, a GIN index of commits over the deploy rows, is the read of the
--     deploy whose commits hold a commit (prodevents.DeployForCommit), which every check_fail and
--     revert asks.
--   production_events_github_runs, (lower(repo), source_ref) over the GitHub adapter's deploy
--     rows, is the read of a repository's GitHub deploy rows and of the row of one of its runs.
--   production_events_failures_of, (deploy_id) over the rows that name a deploy, which are the
--     failure rows, is the read of the failures that blame one deploy, which every day row of the
--     three measures makes for each deploy it counts, and the check of the foreign key when a
--     deploy row is deleted.
--   production_events_restored_by, (restored_by_deploy) over the rows that name one, is the other
--     foreign key's check when a deploy row is deleted.
--   production_events_detected, (project_key, detected_at desc) over the failure rows, is the
--     route's read of a project's failures in a window.
--
-- A new table and seven indexes on it: nothing existing is read or rewritten, and the release
-- before this one neither reads nor writes it, so the two builds roll together. The census
-- answers 0.
create table production_events (
  id                 uuid primary key default gen_random_uuid(),
  project_key        text not null references projects (key) on delete cascade,
  kind               text not null check (kind in ('deploy', 'failure')),
  environment        text not null default 'production' check (environment <> ''),
  status             text not null default '',
  source             text not null check (source in ('github', 'api', 'person', 'check_results', 'reverts', 'incident_label')),
  source_ref         text not null check (source_ref <> ''),
  started_at         timestamptz,
  finished_at        timestamptz,
  repo               text not null default '',
  commits            text[] not null default '{}',
  pull_requests      text[] not null default '{}',
  url                text not null default '',
  actor              jsonb not null,
  created_at         timestamptz not null default now(),
  deploy_id          uuid references production_events (id),
  signal             text check (signal in ('check_fail', 'revert', 'incident_issue', 'api', 'person')),
  detected_at        timestamptz,
  state              text not null default 'counted' check (state in ('counted', 'rejected')),
  state_reason       text,
  state_by           jsonb,
  state_at           timestamptz,
  restored_at        timestamptz,
  restored_by        jsonb,
  restored_by_deploy uuid references production_events (id),
  -- What the writer says of the row: a person's or an agent's word on a failure, which the
  -- Evals tab's drill-down shows beside the pull requests it blames (dispatch://INFRA-2918, R14).
  note               text not null default '',
  unique (project_key, source_ref),
  constraint production_events_shape check (
    (kind = 'deploy' and finished_at is not null and deploy_id is null and signal is null
      and detected_at is null and restored_at is null and restored_by_deploy is null)
    or (kind = 'failure' and deploy_id is not null and signal is not null and detected_at is not null
      and finished_at is null)
  ),
  constraint production_events_state check (
    state = 'counted'
    or (state_reason is not null and state_reason <> '' and state_by is not null and state_at is not null)
  ),
  constraint production_events_restored check ((restored_at is null) = (restored_by is null))
);
create index production_events_window on production_events (project_key, kind, finished_at desc);
create index production_events_posted on production_events (project_key, finished_at)
  where kind = 'deploy' and source = 'api';
create index production_events_commits on production_events using gin (commits) where kind = 'deploy';
create index production_events_github_runs on production_events (lower(repo), source_ref)
  where kind = 'deploy' and source = 'github';
create index production_events_failures_of on production_events (deploy_id) where deploy_id is not null;
create index production_events_restored_by on production_events (restored_by_deploy)
  where restored_by_deploy is not null;
create index production_events_detected on production_events (project_key, detected_at desc) where kind = 'failure';
