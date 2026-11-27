-- This migration makes block ids unique within an issue: a converted issue's blocks are one
-- document's rows under its tree artifact, and document_blocks' key (artifact_id, block_id) holds
-- each id once there. It writes no row and rewrites none, and its restated kind check admits
-- every kind stored, so it refuses no row. A count of the block ids that stand twice under one
-- artifact of kind tree could read nothing but zero: no artifact is of that kind before this
-- migration, and that key, document_blocks' primary key since 0121_block_graph, admits no second
-- row of one id under one artifact at any time, so a group of the key never holds two rows. The
-- number of existing rows this migration would refuse or rewrite is therefore 0, whatever
-- document_blocks and artifacts hold.
select 0;
