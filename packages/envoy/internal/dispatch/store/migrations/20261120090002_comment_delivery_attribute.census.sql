-- This migration writes one attribute node of the comment supertag and its tuples. Nothing reads an
-- existing row of another table, and the only row it can rewrite is that node: no code outside a
-- migration writes a node of kind `attribute`, and no earlier migration writes this id. The number
-- of existing rows this migration would refuse or rewrite is therefore 0.
select 0;
