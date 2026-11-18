-- 20261117200337_update_ask_marks_at_delivery.up.sql -- an update ask still queued at the deploy
-- is sent: the mark the earlier release wrote when it queued that ask goes (dispatch://INFRA-4109
-- R4, dispatch://INFRA-4131 R4).
--
-- What this changes: claims.update_tasked_at is cleared on every claim whose ask of the round it
-- records still waits in the outbox, an update_ask row (record.UpdateAsk) of that claim's issue,
-- role and round. Every other claim keeps the column, and nothing else of any row changes.
--
-- Why: the earlier release wrote update_tasked_at when its sweep queued a claim's ask, before the
-- ask was published (its askRound and Machine.PodUpdateAsked). This release writes it when the
-- publish reached a session (supervise's Machine.PublishUpdateAsk), and its row publishes nothing
-- to a claim that already holds the mark, which reads as an earlier ask of the round having reached
-- it. Read that way, a mark the earlier release wrote over an ask still queued at the deploy would
-- drop that ask unsent, and the round would wait for the claim's answer until its bound: the fault
-- this release fixes, at the moment a deploy opens rounds. Cleared, the row publishes when this
-- release runs it and marks the claim once that publish reached a session, and the sweep asks no
-- claim while its ask is queued (store's QueuedUpdateAsks), so the queued row stays the one ask.
--
-- What stays: a mark with no queued row. The earlier release's row of that ask has finished,
-- delivered or ended with no live holder, and nothing left in the database says which. Kept, a
-- delivered ask stays the only one its role gets; an ask that found no holder before the deploy is
-- not asked again, and its round ends at its bound as it did under the earlier release.
--
-- A row whose round the claim no longer records clears nothing: the round ended, or another
-- replaced it, and the row finishes without a publish.
--
-- There is no down file: migrations here are forward-only, and the column keeps its type and its
-- readers. The table holds the claims and the outbox of every project that shares the database,
-- so every daemon of one database moves to this release together, as 20261113110610 asks: a
-- daemon of the earlier release still running would mark the asks it queues after this ran, and
-- those asks would be dropped unsent by this release's rows.
update claims c
set update_tasked_at = null
where c.update_tasked_at is not null
  and exists (
    select 1 from outbox o
    where o.kind = 'update_ask'
      and o.issue = c.issue
      and o.payload ->> 'role' = c.role
      and o.payload ->> 'round' = c.update_round);
