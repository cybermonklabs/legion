-- This migration writes nine attribute nodes of the delivery_attempt supertag and their tuples.
-- Nothing reads an existing row of another table, and the only rows it can rewrite are those nine
-- nodes: no code outside a migration writes a node of kind `attribute`, and no earlier migration
-- writes one of these ids. The number of existing rows this migration would refuse or rewrite is
-- therefore 0.
select 0;
