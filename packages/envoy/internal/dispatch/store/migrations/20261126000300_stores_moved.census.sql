-- 20261126000300 creates an empty table, the move's record, and writes the type nodes of the
-- three kinds the move brings into the store: the supertag nodes `priority`, `step` and `plan`,
-- their attribute nodes and the tuples that state each value. No route or command writes a node of
-- either kind (requireNoRouteOrCommandWritesAType in cmd/dispatch/supertag_nodes_test.go) and no
-- earlier migration writes one of these ids, so it writes beside no row another writer wrote. Each
-- of its writes is an `insert … values` of literal rows, the one into graph_nodes with `on conflict
-- do nothing`, which keeps a node row a database already holds as it is: it reads no stored row of
-- any table but the key a conflict probes, and refuses and rewrites none. The number of existing
-- rows this migration would refuse or rewrite is therefore 0.
select 0;
