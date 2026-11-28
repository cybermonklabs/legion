-- This migration writes two `supertag` nodes of the store and the tuples that state their base
-- attributes, and nothing else. Its ground is the node store's rule: "A supertag write ships as a
-- migration, in the same release as the binary that handles it. It passes the pre-apply census."
-- No route or command writes a node of that kind (requireNoRouteOrCommandWritesAType in
-- cmd/dispatch/supertag_nodes_test.go), and the seed's closed list names neither, so no row of
-- either exists for its inserts to meet. Each of its statements is an `insert … values` of literal
-- rows, so it reads no row of its own tables and no existing row of another table. It refuses no
-- row and rewrites no row a person or an agent wrote. The number of existing rows this migration
-- would refuse or rewrite is therefore 0.
select 0;
