-- 20261118164016_pull_request_attribution_moves.up.sql
--
-- The moves of a pull request's issue that wait for their events.
--
--   pull_request_attribution_moves  one row per pull request whose results row took another
--                                   issue, keyed by the canonical name of its repository and its
--                                   number. from_key is the issue the results row held before the
--                                   first move that is not appended yet, to_key the issue it holds
--                                   now, and source the source of that answer. A second move before
--                                   the events keeps from_key and replaces to_key, source and
--                                   queued_at.
--
-- Each reconcile pass writes the answer of the statement over the stored facts to every stored
-- row of a pull request, with no cap (delivery.SyncAttributions), and queues a row here for each
-- results row whose issue changes. The pass then appends the events of at most 500 queued rows,
-- the oldest first, and deletes them in the same transaction (delivery.AppendAttributionEvents).
-- So n changes need ceil(n / 500) passes for their events. A row whose two keys are equal again
-- appends nothing.
--
-- No stored row can fail a statement here: the table is new and empty. The statement writes the
-- catalog alone and locks no existing table. The release before names the table nowhere, so it
-- serves beside this one unchanged. Rolling back past this migration is an image rollback alone.
create table pull_request_attribution_moves (
  repo text not null,
  number integer not null,
  from_key text,
  to_key text,
  source text not null,
  queued_at timestamptz not null default clock_timestamp(),
  primary key (repo, number)
);
