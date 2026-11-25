-- The migration adds one nullable jsonb column with no default to comments, and nothing else: a
-- catalog change, which rewrites no stored row and adds no constraint a stored row could fail. The
-- column is null in every existing row. The number of existing rows this migration would refuse or
-- rewrite is therefore 0, whatever comments holds.
select 0
