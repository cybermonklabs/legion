-- The migration drops issue_room_blocks' primary key and builds a unique index over
-- (tree_artifact_id, block_id_key(block_id)) in its place, and nothing else: it writes no row and
-- rewrites none. The index refuses a row only where two rows of one tree hold two ids under one
-- SHA-256, which would be a collision, and every row the primary key held, one id once under each
-- tree, it holds. The census names no table only a pending migration creates, and issue_room_blocks
-- is one (20261117045607_issue_tree_rooms), so it counts nothing. The number of existing rows this
-- migration would refuse or rewrite is therefore 0.
select 0
