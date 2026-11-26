-- Run by hand if a rollback removes the statistics object; no runner applies a rollback script.
-- Nothing depends on this object, and dropping it leaves the planner multiplying the three kind
-- equalities on graph_tuples as independent.
drop statistics if exists graph_tuples_kinds;
