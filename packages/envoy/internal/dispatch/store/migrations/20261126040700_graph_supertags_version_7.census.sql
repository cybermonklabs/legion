-- This migration sets the attributes of the seven block type supertag nodes (callout, ask,
-- requirement, plan_task, check, decision and reading) to what
-- 20261125163555_graph_supertags_decision_reading left on them, the version moved from 6 to 7,
-- and appends the `version` tuple that states each. Those rows are type nodes, and nothing but a
-- migration writes a type node: no route and no command writes one (cmd/dispatch's
-- requireNoRouteOrCommandWritesAType), and no migration between 20261125163555 and this one
-- writes one. So the seven nodes hold what 20261125163555 left, and the update rewrites only their
-- version. It reads no row of another table. The number of existing rows this migration would
-- refuse or rewrite is therefore 0.
select 0;
