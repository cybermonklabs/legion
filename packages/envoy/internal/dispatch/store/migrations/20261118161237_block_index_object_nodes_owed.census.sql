-- block_index_event_cursors gains one nullable column with no default and one not-null column
-- whose default is a constant, which are both catalog changes: no stored row is rewritten, and no
-- constraint is added that a stored row could fail, since every existing row reads the constant.
-- The number of existing rows this migration would refuse or rewrite is therefore 0, whatever
-- block_index_event_cursors holds.
select 0;
