-- 20261128030142_signal_refusals.up.sql
--
-- An automatic failure refused because its blamed deploy counted for other projects alone stays
-- refused (dispatch://INFRA-978/comment/d57e8d06-afb3-4456-9e3d-516923e2b706). Its write reads
-- whether the deploy counts for the row's project in its own transaction (signals'
-- deployCountsForRow), and a refusal writes no failure row. These tables record that refusal in
-- the same transaction, so no later write makes the row either: not even a derive of deploy rows
-- once a source move, a rule change or a default-repository move has made the deploy count for the
-- row's project. A deploy row gone since the write's lookup, and a deploy that counts for no
-- project (one being dropped), record no refusal: the signal stays unmatched, and a revert records
-- a hold of it instead, which is no refusal (revert_refusals, below).
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
-- revertFailures), and one per revert and blamed deploy held: a revert that writes no row for now,
-- because its own deploy finished before the deploy it blames, which restores no failure of that
-- deploy, or because the deploy it blames counts for no project. A revert that holds a refusal of
-- the deploy it blames writes no row and logs nothing on a later pass; one whose blame moves to
-- another deploy, one that counts for the repository's owner, still writes its row. A held revert
-- is no refusal (dispatch://INFRA-978/comment/91d5e75b-57eb-4b76-ba55-5a101fec2c32): every pass
-- judges it again, and the row records the judgement its hold was logged on, so a pass that judges
-- the same logs nothing and one that judges otherwise logs the new hold, or writes the revert's
-- row or its refusal, which deletes its holds. The columns:
--   repo, number   the delivery_reverts row of the revert, its key. The foreign key follows an
--                  update of the key, so the boot's rename pass, which rewrites
--                  delivery_reverts.repo, moves the refusal with its revert.
--   deploy_id      the production_events deploy row the revert blamed: one that counted for other
--                  projects than the repository's owner alone, for a refusal; for a hold, one that
--                  finished after the revert's own deploy or that counted for no project.
--   hold           null for a refusal; for a hold, why the revert writes no row: too_early (its
--                  own deploy finished before the deploy it blames) or no_project (the deploy it
--                  blames counts for no project).
--   restoring_deploy_id, restoring_finished_at   null for a refusal; for a hold, the revert's own
--                  deploy row and that row's finished_at, as the judgement read them, so a re-run
--                  that moves that deploy's finish, or a rule change that makes a later run the
--                  revert's ship run, is a new judgement.
--   refused_at     when the refusal, or the hold as it stands, was written.
--
-- check_fail_refusals_deploy, incident_refusals_deploy, revert_refusals_deploy and
-- revert_refusals_restoring index the foreign keys when a deploy row is deleted.
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
  repo                  text not null,
  number                integer not null,
  deploy_id             uuid not null references production_events (id) on delete cascade,
  hold                  text check (hold in ('too_early', 'no_project')),
  restoring_deploy_id   uuid references production_events (id) on delete cascade,
  restoring_finished_at timestamptz,
  refused_at            timestamptz not null default now(),
  primary key (repo, number, deploy_id),
  foreign key (repo, number) references delivery_reverts (repo, number) on update cascade on delete cascade,
  constraint revert_refusals_hold check (
    (hold is null) = (restoring_deploy_id is null) and (hold is null) = (restoring_finished_at is null)
  )
);
create index revert_refusals_deploy on revert_refusals (deploy_id);
create index revert_refusals_restoring on revert_refusals (restoring_deploy_id);
