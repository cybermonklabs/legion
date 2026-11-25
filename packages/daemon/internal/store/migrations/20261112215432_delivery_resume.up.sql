-- 20261112215432_delivery_resume.up.sql — a held task that resumes an answered root carries the
-- wait it answers, so its text is written at each send and not when it was queued
-- (dispatch://INFRA-3136 R2, dispatch://INFRA-3111).
--
-- resume is the wait the resume task of a root a person answered answers
-- (supervise.Delivery.Resume, record.Awaiting as JSON), with the id of the delivery it was written
-- for: {"delivery": "<delivery_id>", "wait": {...}}. The claim store writes it with the delivery
-- (store's putDelivery) from the outbox start that queued the task
-- (record.SuperviseRequest.Awaiting, the daemon's outbox start handler), and the daemon's rootTask
-- renders the task from it, beside the gate and the ask mirror as they stand at that send
-- (workflow.ResumeAwaitedRootTask). It is null for every other delivery.
--
-- The wait is carried rather than read from the issue at send because the issue no longer holds
-- it then: admission's resume of an answered root clears issues' awaiting columns in the
-- transaction that queues the task, and the in-place telling of a root its architect still runs
-- (workflow's tellInPlace) never writes them at all.
--
-- A delivery written before this migration has a null resume and is sent as the task text it
-- holds, as before. A daemon of an earlier release neither reads nor writes the column: it sends
-- the task text a row holds, which for a resume task this release queued and never sent is empty,
-- and its write of the claim's next delivery keeps the column's value, which is why the value
-- names its own delivery id — a value read under another delivery's id marks nothing.
--
-- parked_decisions.seq is Dispatch's sequence of a kept spec decision's review, beside at, which
-- for such a decision is now Dispatch's time of the review (record.ParkedDecision.At and Seq,
-- from the notice's Review). A held task is narrowed at each send by asking the gate whether each
-- decision it names is still the person's word (workflow.SupersedingApproval), and a changes
-- request's question is answered by Dispatch's order on both sides: the sequences when both carry
-- one, else Dispatch's times. It is null where Dispatch gave no sequence, for an answer on an ask,
-- and for every row kept before this migration.
--
-- parked_decisions.artifact is the Dispatch artifact id of the document a kept spec decision's
-- review is of (record.ParkedDecision.Artifact, from the notice's Artifact), which the
-- parked-decision controller notice that relays the decision carries too. A step's gate can move
-- to another document while the decision's reference stays the step's, and the approval of that
-- other document answers no review of the one the person reviewed, so the kept decision is asked
-- of its document as its relay is (workflow.SupersedingApproval). It is '' for an answer on an
-- ask, for a notice that carried no document, and for every row kept before this migration, and
-- such a row is asked of whichever document the gate holds (workflow's keptSuperseded).
--
-- parked_decisions.version is the version of the document a kept spec decision's review is of
-- (record.ParkedDecision.Version, from the notice's Version), which the parked-decision
-- controller notice that relays the decision carries too, so the kept decision is asked at the
-- version the person reviewed as its relay is (workflow.SupersedingApproval): an approval of a
-- later version is the person's word in the review's place, whichever of the two Dispatch marked
-- first. It is null for an answer on an ask, for a notice that carried no version, and for every
-- row kept before this migration, and such a row is asked at the version its gate holds approved.
--
-- A daemon of an earlier release names none of seq, artifact and version. A row it inserts
-- carries a null seq, the empty artifact and a null version. A decision it keeps over a row this
-- release kept (its upsert on issue and ref) is written beside that row's seq, artifact and
-- version, the earlier review's, so a rollback clears the three columns before that release is
-- pinned (docs/kubernetes.md, the update of parked_decisions under daemon API contract entry
-- 20261125181104).
--
-- issues.awaiting_answered_version, issues.awaiting_answered_artifact and
-- issues.awaiting_answered_step are the document the act that answered a root's wait was of, a
-- person's review or a newer version (record.Awaiting.AnsweredDocument), written beside the detail
-- by the same answer: the version of the review (the version a changes request reviewed or an
-- approval approved), so the resume asks whether that review is still the person's word
-- (workflow.SupersedingApproval) of a number rather than of the detail, which is display text; the
-- Dispatch artifact id of the document; and the design step whose gate row held that document at
-- the answer, so the resume asks that review of the gate of that step and names that document:
-- under a deployment that gates more than one step it need not be the step the resume speaks of, a
-- wait on a question records no document of its own, and a registration of another document on
-- that step afterwards leaves no row holding the document at all, so the step is recorded rather
-- than looked up. awaiting_answered_document_at is the answer's moment (awaiting_answered_at) the
-- three were written with, and the document is read only while the two moments are equal
-- (record's issueColumns): a daemon of an earlier release names none of the four columns, so its
-- write of a later answer leaves the old version, document and step standing beside a new moment,
-- and that answer reads as one with no document. None of the four is part of
-- issues_awaiting_shape, so that earlier daemon's write that clears a wait is not refused for
-- leaving them set.
--
-- A wait answered by a changes request before this migration is given its version here, read
-- once from the detail every release so far wrote for it (`version <n>: "<reason>"`, workflow's
-- changesDetail), with the answer's moment beside it (awaiting_answered_document_at), so it reads
-- as it did; no daemon parses the detail after this. A wait an approval answered before it is
-- given none, and its approval is asked at the version the gate holds approved. No wait is given
-- a document or a step: an answer before it is read as of the document the wait records and the
-- step of the row that holds it, and an answer of a question wait as of the step the resume speaks
-- of, as before.
--
-- There is no down file: migrations here are forward-only.
alter table pending_task_deliveries add column resume jsonb;
alter table parked_decisions add column seq bigint,
  add column artifact text not null default '',
  add column version integer;
alter table issues add column awaiting_answered_version integer,
  add column awaiting_answered_document_at timestamptz,
  add column awaiting_answered_artifact text,
  add column awaiting_answered_step text;
update issues
   set awaiting_answered_version = substring(awaiting_answered_detail from '^version ([0-9]+): ')::integer,
       awaiting_answered_document_at = awaiting_answered_at
 where awaiting_answered_by = 'changes_requested' and awaiting_answered_at is not null
   and awaiting_answered_detail ~ '^version [0-9]+: ';
