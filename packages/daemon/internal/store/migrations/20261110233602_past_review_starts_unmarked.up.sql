-- 20261110233602_past_review_starts_unmarked.up.sql — takes the admission-slot mark off every
-- queued start of a phase that holds no slot any more, so that the cutover leaves no row the
-- daemon will never run (INFRA-2560).
--
-- An issue holds one of the admission cap's slots from the moment the daemon starts it until its
-- work is past its review, and accepting, retro, merging and awaiting_merge hold none
-- (record.KeepsSlot). The release before this one kept a slot through the first three of those,
-- so workflow.StartOrOwe marked every start of a phase worker for a slot, a retro's and a
-- merger's among them, and the mark is what record.Store's outboxInOrder holds such a row by.
--
-- Without this statement such a row never runs again. outboxInOrder holds a queued start marked
-- for a slot unclaimable while the slots table names no row of its issue, and after this change
-- that issue never holds one: the first admission pass of this release releases the row the
-- issue held, and the rule grants none for a phase past the review. The row would stand in the
-- outbox for ever, its retro or its merger would never be started, and the tree would stop at
-- its retro with nothing left to move it.
--
-- Taking the mark off is the whole of the repair. The row keeps its id, its task, its phase and
-- its generation, so it is the start the transition wrote and nothing else, and it is claimable
-- at once, exactly as the start this release writes for those four phases is. The outbox's own
-- phase and generation fences still end a stale one, and record.validateOutboxPayload asks a
-- task and a phase of a marked row alone, so a payload this leaves unmarked passes it.
--
-- The statement names the four phases rather than every marked start, so a start of a phase that
-- still holds a slot keeps its mark and goes on waiting for the row its issue keeps.
--
-- Migrations here are forward-only and this one has no down file. Rolling the image back costs
-- these rows their wait: the earlier release reads a start with no mark as one claimable at
-- once, which is what it already does with every start that carries none.
update outbox set payload = payload - 'needsSlot'
where kind = 'supervise' and payload->>'op' = 'start'
  and payload->>'needsSlot' = 'true'
  and payload->>'phase' in ('accepting', 'retro', 'merging', 'awaiting_merge');
