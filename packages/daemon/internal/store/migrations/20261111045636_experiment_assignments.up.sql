-- 20261111045636_experiment_assignments.up.sql — one row for each unit the daemon drew an
-- experiment for, so a project declares as many experiments as it likes instead of the one
-- hard-coded memory coin (INFRA-2566).
--
-- What the four issues.memory_* columns are is one experiment spelled into the issues table: a
-- tree's root row carries the group it drew, the mode it was drawn under and the instant, and a
-- second experiment would need four more columns, a fifth a fourth set, and a project could never
-- declare one the daemon's schema had not been written for. A declared experiment
-- (config.Experiment, drawn by experiment.Draw) has a name of the operator's choosing, so the
-- record has to be a row keyed by that name rather than a column named after it.
--
-- The key is the unit the draw covers, which is why unit is on the row and not only in the
-- project's file: a tree experiment is recorded against the tree's root key, and an issue
-- experiment against each issue's own key, so one table holds both and a reader of an issue's
-- assignments asks for the issue's own rows and its root's. The column says which of the two a row
-- is, so a reader needs no second source to know what the key it is reading means, and the check
-- keeps the two words the loader takes (config.ExperimentUnit).
--
-- The row is written once and never updated, as the draw it records is made once: an experiment a
-- unit has been running under keeps its variant whatever the project's file says afterwards, and
-- the mode moving from random to off changes no unit already drawn. The primary key is what makes
-- that true in the schema, since every writer inserts under `on conflict (issue_key, experiment)
-- do nothing` (record.Store.PutAssignments, record.Store.CarryAssignments).
--
-- The three defaults are the shapes the record has to hold beside a plain draw. variant '' is no
-- variant: an experiment at mode off, which draws for no unit and is recorded as having drawn for
-- none, and the answer a hand-over settles a child at when the tree it left held no row for the
-- experiment. mode '' is such a hand-over too, where no draw was ever made and so no mode was ever
-- in force. drawn_at null is the same fact in the third column: nothing was drawn, so there is no
-- instant to record. carried is the hand-over's own mark, as issues.memory_carried is today: a row
-- a child took from the tree it left rather than drew for itself, which is why a draw never writes
-- over it and a measurement counts it against the tree it came from.
--
-- The foreign key is the issue record, cascading, because an assignment of an issue the record no
-- longer holds is of nothing: the row would name a key no reader could resolve and no measurement
-- could attribute. Both writers guard on that record themselves — their inserts select from issues
-- — so a key the daemon holds no record for writes nothing rather than raising, exactly as
-- record.Store.DrawMemory's `where key = $1` does today.
create table experiment_assignments (
    issue_key  text not null references issues(key) on delete cascade,
    experiment text not null,
    unit       text not null check (unit in ('tree', 'issue')),
    variant    text not null default '',   -- '' is no variant: mode off, or settled at a hand-over
    mode       text not null default '',   -- '' where a hand-over settled it and the old tree held no row
    drawn_at   timestamptz,                -- null where a hand-over settled it with no draw
    carried    boolean not null default false,
    primary key (issue_key, experiment));

-- The back-fill is the memory draw this release stops writing to issues and starts writing here.
-- Every tree the daemon has drawn for carries its answer on its root row, and the measurement job
-- reads assignments from the state API alone: without the copy, every tree drawn before this
-- deploy would read as having drawn nothing, and the memory experiment would lose its whole
-- exposure to date rather than just its new trees.
--
-- The rows it copies are the rows that hold an answer: a draw (memory_drawn_at is not null) or a
-- hand-over that settled the root at no draw (memory_carried), which is the pair
-- record.Store.DrawMemory's own condition refuses to draw over. A root neither drew for nor
-- handed an answer to has nothing to record and takes no row, so it is drawn by this release's
-- own first open like any other new tree. memory_group is the variant, memory_draw_mode the mode,
-- memory_drawn_at the instant and memory_carried the mark, each verbatim; the experiment is the
-- literal `memory`, the name config.Project.ExperimentsDeclared gives the `memory` block, and the
-- unit the literal `tree`, the unit that block has always drawn for.
--
-- It alters no column and drops none. The four issues.memory_* columns stay in the schema exactly
-- as they stand and only stop being written, which is what makes the roll back below cost what it
-- costs rather than losing every tree drawn before this deploy as well.
insert into experiment_assignments (issue_key, experiment, unit, variant, mode, drawn_at, carried)
select key, 'memory', 'tree', memory_group, memory_draw_mode, memory_drawn_at, memory_carried
from issues
where memory_drawn_at is not null or memory_carried;

-- Rolling back onto legion-v19.49.1, the release before this one. That release writes
-- issues.memory_* only where it creates a tree's lifecycle row (internal/admit/admit.go:1345-1349,
-- `if lifecycle.Created`), and the lifecycle row of every tree drawn after this merge exists
-- already, so the rolled-back daemon creates none of them again: it re-draws nothing, and no tree
-- changes arm. What it loses is the record. A tree drawn after the merge has its answer in
-- experiment_assignments and nothing in issues.memory_*, and the rolled-back release reads only
-- the latter, so that tree reads as undrawn there for the rest of its life — its lifecycle row is
-- never created again, so nothing ever writes the columns for it — and the measurement job loses
-- its exposure. The trees drawn before the merge are untouched and read exactly as they did.
--
-- There is no down file: the migrations here are forward-only, and nothing of this one need come
-- off for that roll back, which names neither the table nor its rows. Dropping the four
-- issues.memory_* columns is a later release's migration, once no release that reads them is one
-- an operator would roll back to.
