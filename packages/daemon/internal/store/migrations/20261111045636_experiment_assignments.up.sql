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
-- — so a key the daemon holds no record for writes nothing rather than raising, exactly as the
-- memory draw's own `where key = $1` did.
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
-- hand-over that settled the root at no draw (memory_carried), which is the pair the memory
-- draw's own condition refused to draw over. A root neither drew for nor
-- handed an answer to has nothing to record and takes no row, so it is drawn by this release's
-- own first open like any other new tree. memory_drawn_at is the instant and memory_carried the
-- mark, each verbatim; the experiment is the literal `memory`, the name
-- config.Project.ExperimentsDeclared gives the `memory` block, and the unit the literal `tree`,
-- the unit that block has always drawn for.
--
-- The variant is memory_group with one spelling changed: a row drawn under mode off is copied at
-- the empty variant rather than at the word `none` that column holds. That is the one answer the
-- two releases spell differently. The draw before this release answered a mode-off tree with the
-- word `none`, and this release's draw answers a mode-off experiment with the empty variant,
-- which is the generic rule every declared experiment needs: nothing says an experiment has an
-- arm named `none` to record such a draw under. Copied verbatim, one answer would stand in this
-- column under two spellings, one for the trees drawn before this deploy and another for those
-- drawn after, and every reader counting arms would have to know both.
--
-- What that costs: the old column told a tree drawn into no arm from a tree with no answer at all
-- by the group alone — `none` against the empty string, which is what the base's own hand-over
-- wrote for a tree it settled (coalesce(source.memory_group, '')) — and after this both read as
-- the empty variant. The distinction is not lost; it moves into the two columns beside it, which
-- this row carries and which api.AssignmentView publishes: mode is `off` for the drawn-under-off
-- row and the empty string for the settled one, and drawn_at is the instant of the draw for the
-- first and null for the second.
--
-- What it does not cost: the three memory fields of the state (internal/projection's memoryOf)
-- read nothing new. They are filled only from a row that records a draw — drawn_at set, which the
-- settled row has not — and every arm that is not the `memory` one, the empty arm among them,
-- is the group `none`. So a tree drawn under mode off answers the group `none`, the mode `off`
-- and its own instant here exactly as it answered them off the four columns before.
--
-- It alters no column and drops none. The four issues.memory_* columns stay in the schema exactly
-- as they stand and only stop being written, which is what makes the roll back below cost what it
-- costs rather than losing every tree drawn before this deploy as well.
insert into experiment_assignments (issue_key, experiment, unit, variant, mode, drawn_at, carried)
select key, 'memory', 'tree',
       case when memory_draw_mode = 'off' then '' else memory_group end,
       memory_draw_mode, memory_drawn_at, memory_carried
from issues
where memory_drawn_at is not null or memory_carried;

-- Rolling back onto a release before this one: legion-v19.57.3, which the deployment ran when this
-- was written, and whose tree the two lines cited here were read in. That release writes
-- issues.memory_* only where it creates a tree's lifecycle row (internal/admit/admit.go:1366,
-- `if lifecycle.Created`), and the lifecycle row of every tree drawn after this merge exists
-- already, so the rolled-back daemon creates none of them again: it re-draws nothing. It changes
-- arms all the same, which is what the roll back costs. That release decides a launch's memory
-- from issues.memory_group alone (its daemon.go's treeMemoryGroup, `issue.MemoryGroup ==
-- "memory"`), and this release writes no memory_* column, so a tree drawn into the `memory` arm
-- after the upgrade has an empty memory_group: its next role starts without the memory overlays
-- and without the memory instructions, and the tree runs the rest of its life in the other arm
-- than the one it was drawn into. The record goes with it: that tree's answer is in
-- experiment_assignments, which the rolled-back release names nowhere, so it reads as undrawn
-- there — its lifecycle row is never created again, so nothing ever writes the columns for it —
-- and the measurement job loses its exposure. The trees drawn before the merge are untouched:
-- their columns are the back-fill's source and still hold their draw, so they keep their arm and
-- read exactly as they did.
--
-- Rolling forward again costs the window. A tree the rolled-back release drew while it ran has
-- its answer in issues.memory_* alone, and the back-fill above runs once, at the first deploy of
-- this migration, and not a second time: this release's launchVariants finds no row for that tree
-- and runs it in no arm, so it too changes arm mid-life, and CarryAssignments settles the
-- children it hands over at no variant. A deploy that ran the roll back therefore copies those
-- trees over by hand before it reads the exposure again: the back-fill's own select, with
-- `on conflict (issue_key, experiment) do nothing` added, since the rows copied at the first
-- deploy stand.
--
-- There is no down file: the migrations here are forward-only, and nothing of this one need come
-- off for that roll back, which names neither the table nor its rows. Dropping the four
-- issues.memory_* columns is a later release's migration, once no release that reads them is one
-- an operator would roll back to.
