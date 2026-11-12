-- 20261112090430_architect_stalled_notices.up.sql — the mark that the project's controller has
-- been told a root's own architect has stalled (INFRA-2614).
--
-- The daemon's admission pass reads three arms over each root it holds a slot for, after it has
-- sent every role the work its phase entry names: the architect took nothing it was sent, the
-- architect's claim cannot be reached, and the root stands at admitted with no design-gate step
-- registered long after it was admitted. Each arm wakes the project's controller, which is the
-- one reader left — the architect is the role that would otherwise be told, and every arm is a
-- reading that it did not take its work, is not there, or has not moved.
--
-- The wake must be told once and stay told: every fact, tick, hold release, boot and entry sweep
-- runs that pass again, and a wake repeated on each of them would bury the controller's topic
-- under one root. The mark is therefore a row of its own, in the style of the children-first
-- marks (20261104004500), keyed by the issue, the tree it stands in, the generation of that
-- tree's run and the arm that read it.
--
-- The arm is in the key because each arm is a fact of its own: a root an arm has already woken
-- the controller for must still wake it for a second arm that reads it later, or one case would
-- hide another. The generation is the root's own run, so a re-admission tells the controller
-- anew, and the tree is in the key because a generation names a run within one tree alone: an
-- issue handed over as a root of its own runs at a generation of its own numbering (admit's
-- readmit), which can be the number the tree it left was at. Nothing deletes a row: the issues
-- table's own cascade does, when a record goes.
create table architect_stalled_notices (
    issue text not null references issues (key) on delete cascade,
    tree text not null,
    generation bigint not null check (generation >= 0),
    arm text not null,
    told_at timestamptz not null,
    primary key (issue, tree, generation, arm)
);
