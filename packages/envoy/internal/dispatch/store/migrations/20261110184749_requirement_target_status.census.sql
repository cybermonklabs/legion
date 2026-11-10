-- 20261110184749 adds two nullable columns to document_blocks and two partial indexes over
-- them. It reads no row, writes no row and refuses none: every existing row carries null in
-- both columns, which is what "this block declares no target or status" means, and no read
-- before this change looks at either. There is nothing for a census to count, so it counts
-- nothing. What the pre-deploy report should be read for here is the size of document_blocks
-- and the holders of its locks: the alter takes ACCESS EXCLUSIVE for one catalogue write, and
-- each index build takes SHARE for one scan of the table, which blocks the block index's own
-- writes for that time.
select 0;
