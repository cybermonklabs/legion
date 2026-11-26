-- The migration creates node_search, a new table, and its indexes, and nothing else: it writes no
-- row, rewrites none and names no other table. The census names no table only a pending migration
-- creates, and node_search is one, so it counts nothing. The number of existing rows this
-- migration would refuse or rewrite is therefore 0.
select 0
