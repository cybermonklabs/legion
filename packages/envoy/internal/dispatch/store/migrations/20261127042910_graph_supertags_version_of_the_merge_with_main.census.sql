-- This migration sets the five block type supertag nodes (callout, ask, requirement, plan_task
-- and check) to the attributes main's 20261125125701_graph_supertags_check_subject left on them,
-- the version moved from 6 to 12, and appends one version tuple for each. Those rows are type
-- nodes, and nothing but a migration writes a type node (cmd/dispatch's
-- requireNoRouteOrCommandWritesAType): main's 20261125125701 is the last migration below this one
-- that writes one, so the five nodes hold what it left, and each update rewrites only their
-- version. It inserts no node, so no insert can meet a row, and the tuple inserts are literal
-- rows that read no table. It refuses no row, and it rewrites no stored document, event or other
-- content row. The number of existing content rows refused or rewritten is 0.
select 0
