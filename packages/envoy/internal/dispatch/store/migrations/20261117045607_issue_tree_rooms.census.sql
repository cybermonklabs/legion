-- This migration makes block ids unique within an issue: a converted issue's blocks are one
-- document's rows under its tree artifact, and document_blocks' key (artifact_id, block_id) holds
-- each id once there. It writes no row and rewrites none, and its restated kind check admits
-- every kind stored, so it refuses no row. This counts the block ids that stand twice under one
-- artifact of kind tree, which reads zero: no artifact is of that kind before this migration, and
-- the key admits no second row of one id after it. It names document_blocks and artifacts alone,
-- since a census that names a table only a pending migration creates is refused. document_blocks
-- is 0121_block_graph's, which shipped releases before this one.
select count(*)
from (
  select b.artifact_id, b.block_id
  from document_blocks b
  join artifacts a on a.id = b.artifact_id
  where a.kind = 'tree'
  group by b.artifact_id, b.block_id
  having count(*) > 1
) twice
