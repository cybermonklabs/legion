-- Each of the six tables gains two nullable text columns with no default, which is a catalog
-- change: no stored row is rewritten, and no constraint is added that a stored row could fail.
-- Both columns are null in every existing row. The number of existing rows this migration would
-- refuse or rewrite is therefore 0, whatever the six tables hold.
select 0;
