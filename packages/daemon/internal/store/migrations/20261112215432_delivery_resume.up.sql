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
-- (workflow.ResumeTask). It is null for every other delivery.
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
-- from the notice's Review). A held task is narrowed at each send by asking the gate whether a
-- later approval superseded each decision it names (workflow.DecisionStands), and the
-- question is answered by Dispatch's order on both sides: the sequences when both carry one,
-- else Dispatch's times. It is null where Dispatch gave no sequence, for an answer on an ask, and
-- for every row kept before this migration; a daemon of an earlier release writes none.
--
-- issues.awaiting_answered_version is the version of the changes request that answered a root's
-- wait (record.Awaiting.AnsweredVersion), written beside the detail by the same answer, so the
-- resume asks whether the person's approval followed that review (workflow.DecisionStands) of a
-- number rather than of the detail, which is display text. awaiting_answered_version_at is the
-- answer's moment (awaiting_answered_at) the version was written with, and the version is read
-- only while the two are equal (record's issueColumns): a daemon of an earlier release names
-- neither column, so its write of a later answer leaves the old version standing beside a new
-- moment, and that answer reads as one with no version. Neither column is part of
-- issues_awaiting_shape, so that earlier daemon's write that clears a wait is not refused for
-- leaving them set.
--
-- A wait answered by a changes request before this migration is given its version here, read
-- once from the detail every release so far wrote for it (`version <n>: "<reason>"`, workflow's
-- changesDetail), so it reads as it did; no daemon parses the detail after this.
--
-- There is no down file: migrations here are forward-only.
alter table pending_task_deliveries add column resume jsonb;
alter table parked_decisions add column seq bigint;
alter table issues add column awaiting_answered_version integer,
  add column awaiting_answered_version_at timestamptz;
update issues
   set awaiting_answered_version = substring(awaiting_answered_detail from '^version ([0-9]+): ')::integer,
       awaiting_answered_version_at = awaiting_answered_at
 where awaiting_answered_by = 'changes_requested' and awaiting_answered_at is not null
   and awaiting_answered_detail ~ '^version [0-9]+: ';
