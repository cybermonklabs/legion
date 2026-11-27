-- This migration inserts the type node plan_role, its attribute nodes plan_role/type and
-- plan_role/model and their tuples, and writes no other row: it updates no node, and it moves no
-- other block type's supertag node. Those three nodes are type nodes, and nothing but a migration
-- writes a type node: no route and no command writes one (cmd/dispatch's
-- requireNoRouteOrCommandWritesAType), and no other migration writes any of the three. So no node
-- plan_role, plan_role/type or plan_role/model exists for the inserts to meet, whether
-- 20261125125701_graph_supertags_check_subject has run before it or not. It reads no row of
-- another table. The number of existing rows this migration would refuse or rewrite is therefore 0.
select 0
