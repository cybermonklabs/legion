-- 20261128030142_signal_refusals.up.sql
--
-- An automatic failure refused because its blamed deploy counted for other projects alone stays
-- refused (dispatch://INFRA-978/comment/d57e8d06-afb3-4456-9e3d-516923e2b706). Its write reads
-- whether the deploy counts for the row's project in its own transaction (signals'
-- deployCountsForRow), and a refusal writes no failure row. These tables record that refusal in
-- the same transaction, so no later write makes the row either: not even a derive of deploy rows
-- once a source move, a rule change or a default-repository move has made the deploy count for the
-- row's project. A deploy row gone since the write's lookup records no refusal.
--
-- check_fail_refusals holds one row per failed after-merge check result refused so
-- (signals.RecordCheckFailure). A result that holds one is never written again: neither the result
-- route's write, nor the checks feed, nor a deploy row's match of the results that came before it
-- (signals' matchEarlyChecks). A result whose commit no deploy row held is not refused, and makes
-- its row when such a deploy row lands. The columns:
--   result_id   the check_results row; a result is refused once.
--   deploy_id   the production_events deploy row its first match blamed and that counted for other
--               projects than the result's alone.
--   refused_at  when the refusal was written.
--
-- incident_refusals holds one row per incident issue and blamed deploy refused so (signals'
-- recordIncident). An issue whose links blame a deploy it holds a refusal of writes no row on a
-- later event; a link that blames another deploy, one that counts for the issue's project, still
-- writes its row. The columns:
--   issue_key   the issue labelled incident.
--   deploy_id   the production_events deploy row its links blamed and that counted for other
--               projects than the issue's alone.
--   refused_at  when the refusal was written.
--
-- revert_refusals holds one row per merged revert and blamed deploy refused so (signals'
-- revertFailures). A revert that holds a refusal of the deploy it blames writes no row on a later
-- pass; one whose blame moves to another deploy, one that counts for the repository's owner, still
-- writes its row. The columns:
--   repo, number   the delivery_reverts row of the revert, its key. The foreign key follows an
--                  update of the key, so the boot's rename pass, which rewrites
--                  delivery_reverts.repo, moves the refusal with its revert.
--   deploy_id      the production_events deploy row the revert blamed and that counted for other
--                  projects than the repository's owner alone.
--   refused_at     when the refusal was written.
--
-- check_fail_refusals_deploy, incident_refusals_deploy and revert_refusals_deploy index the foreign
-- key when a deploy row is deleted.
--
-- The release before this one has no production event record, so no failure was refused before
-- this migration and there is nothing to backfill. The foreign keys take SHARE ROW EXCLUSIVE on
-- check_results, issues, delivery_reverts and production_events for the migration's short
-- transaction. No existing row is rewritten or refused, and the release before this one neither
-- reads nor writes the new tables. The census answers 0.
create table check_fail_refusals (
  result_id  uuid primary key references check_results (id) on delete cascade,
  deploy_id  uuid not null references production_events (id) on delete cascade,
  refused_at timestamptz not null default now()
);
create index check_fail_refusals_deploy on check_fail_refusals (deploy_id);

create table incident_refusals (
  issue_key  text not null references issues (key) on delete cascade,
  deploy_id  uuid not null references production_events (id) on delete cascade,
  refused_at timestamptz not null default now(),
  primary key (issue_key, deploy_id)
);
create index incident_refusals_deploy on incident_refusals (deploy_id);

create table revert_refusals (
  repo       text not null,
  number     integer not null,
  deploy_id  uuid not null references production_events (id) on delete cascade,
  refused_at timestamptz not null default now(),
  primary key (repo, number, deploy_id),
  foreign key (repo, number) references delivery_reverts (repo, number) on update cascade on delete cascade
);
create index revert_refusals_deploy on revert_refusals (deploy_id);
