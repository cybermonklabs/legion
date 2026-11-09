-- 20261109060737_drop_awaiting_slot_notice.up.sql -- the mark of a notice that can no longer be
-- written goes, with the trigger that kept it safe.
--
-- What this takes out: the column awaiting_slot_notice_at, the trigger
-- issues_carried_slot_notice_cleared and the function behind it, and the two clauses they were
-- added with inside issues_awaiting_shape. What is left is 0057_awaiting_person.up.sql's own
-- shape check, word for word.
--
-- Why the notice can no longer fire: an issue keeps its admission slot from the moment the daemon
-- starts it until it is done, leaves the workflow or waits only for its merge (record.KeepsSlot).
-- A root that hands its work to a person keeps the row it was started with through the whole
-- wait, so the pass that finds the wait answered resumes its architect on that row and asks the
-- cap for nothing. No root waits for a slot to resume, so nothing writes the mark, and the
-- architect reads where it stands in the line from `waitingLine` in its own record instead.
--
-- There is no down file: migrations here are forward-only. This one supersedes
-- 20261108055000_awaiting_slot_notice.up.sql rather than editing it, so a database that ran that
-- migration and a database that runs both reach the same shape, and the history of what the
-- deployment once held stays readable.
--
-- The order of the four statements is the order this change needs, for two reasons. A bare drop
-- of the column would take the whole shape check with it and say nothing: ALTER TABLE ... DROP
-- COLUMN drops a same-table check constraint that names the column automatically, and the
-- constraint names many columns besides this one. So it comes off by hand and goes back on, in
-- its 0057 form, once the column is gone. The trigger comes off before either, because the
-- dependency Postgres does track is the trigger on the table and not what the plpgsql body
-- reads: a function body naming a dropped column raises at the next write rather than at the
-- drop, so dropping the trigger and its function by hand is the whole of what removes them.
--
-- Rolling the image back is not free. Every release from legion-v19.45.0 on names
-- awaiting_slot_notice_at in its PutIssue and scans it in scanIssue, so pinning one of them
-- after this migration fails every issue read and every issue write until the column is back.
-- The statement that brings it back, and why nothing else of this migration need come with it,
-- are in docs/kubernetes.md under its own heading.
drop trigger issues_carried_slot_notice_cleared on issues;
drop function issues_carried_slot_notice_cleared();
alter table issues drop constraint issues_awaiting_shape;
alter table issues drop column awaiting_slot_notice_at;
alter table issues add constraint issues_awaiting_shape check (
  case when phase = 'awaiting_person'
    then (awaiting_ask is null) <> (awaiting_gate_version is null) and held_from is null
      and (awaiting_gate_version is null or awaiting_artifact is not null)
      and (awaiting_answered_by is null or awaiting_answered_at is not null)
      and (awaiting_answered_detail is null or awaiting_answered_by is not null)
    else awaiting_ask is null and awaiting_gate_version is null and awaiting_artifact is null
      and awaiting_answered_at is null and awaiting_answered_by is null and awaiting_answered_detail is null
      and awaiting_since_seq is null and awaiting_since is null
  end);
