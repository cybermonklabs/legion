-- comment_block_texts gains one nullable timestamptz column with no default, and then a default for
-- it, which applies only to the rows inserted later. Each is a catalog change: no stored row is read
-- or rewritten, and every existing row reads null, which no constraint can refuse. The number of
-- existing rows this migration would refuse or rewrite is therefore 0, whatever comment_block_texts
-- holds.
select 0;
