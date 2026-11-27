-- This migration inserts the attribute node plan_task/parent and its tuples, and sets the
-- attributes of the five block type supertag nodes (callout, ask, requirement, plan_task and
-- check) to what 20261125125701_graph_supertags_check_subject left on them, the version moved
-- from 6 to 7. Those rows are type nodes, and nothing but a migration writes a type node: no
-- route and no command writes one (cmd/dispatch's requireNoRouteOrCommandWritesAType), and no
-- migration between 20261125125701 and this one writes one. So the five nodes hold what that one
-- left, and the update rewrites only their version. No node plan_task/parent exists for the insert
-- to meet. It reads no row of another table. The number of existing rows this migration would
-- refuse or rewrite is therefore 0.
select 0
