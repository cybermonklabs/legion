-- 20261123041501_production_signals.up.sql
--
-- The bookkeeping of the loop that writes the record's GitHub deploy rows and its three automatic
-- failure signals (dispatch://INFRA-978, R6 and R7; prodevents/signals): where each of its feeds
-- stands, and the rule each deploy repository's GitHub deploy rows stand derived under.
--
-- production_signal_cursors holds one row per feed the loop reads, each a cursor of its own:
--   name        deploys (the delivery change log, delivery_changes: the run, job and pull request
--               writes that can make a deploy, read after they commit), checks and incidents (the
--               event log, events, read up to its safe horizon).
--   position    the seq of the last change, or the id of the last event, a committed page read.
--   at          for deploys, when the change at position was recorded (delivery.ChangeCursor.At),
--               which the lost-cursor rule reads (delivery.CursorLost); null for the event feeds.
--   updated_at  when a page last read its feed to its end (delivery.ChangeCursor.ReadAt) for
--               deploys, and when the page that moved it ran for the event feeds.
--
-- production_deploy_rules holds one row per deploy repository whose GitHub deploy rows the loop
-- derived from its stored runs:
--   repo        the repository, in lower case.
--   project_key the project whose source read it (delivery.DeployRules), the rows' project.
--   production_job_names, production_apply_step_name
--               the two fields of its rule that decide whether a run is a deploy
--               (delivery.ProductionApplies).
--   derived_at  when the rows were last derived under it.
-- A pass that reads another owner or another rule for a repository, or no row for it, derives
-- every GitHub deploy row of that repository again (a settings save, a moved default repository, or
-- the first pass of this release, which writes the rows of every stored run), and records the new
-- row once those rows are written, so a pass that fails part way derives the repository again
-- (signals.deriveMovedRules). A repository no rule reads any more loses its row and keeps its
-- deploy rows until a source reads it again.
--
-- Two new tables: nothing existing is read, locked or rewritten, and the release before this one
-- neither reads nor writes them, so the two builds roll together. The census answers 0.
create table production_signal_cursors (
  name       text primary key check (name in ('deploys', 'checks', 'incidents')),
  position   bigint not null default 0,
  at         timestamptz,
  updated_at timestamptz not null default now()
);

create table production_deploy_rules (
  repo                       text primary key check (repo <> '' and repo = lower(repo)),
  project_key                text not null,
  production_job_names       text[] not null,
  production_apply_step_name text,
  derived_at                 timestamptz not null default now()
);
