-- 20261108015800 sets one storage option on issues_search and empties that index's pending list.
-- It reads no row, writes no row and refuses none: the index holds the same entries after it, in
-- the index itself rather than in a list in front of it, and search answers what it answered
-- before. There is nothing for a census to count, so it counts nothing. What the pre-deploy
-- report should be read for here is the size of issues and the holders of its locks: the alter
-- takes ACCESS EXCLUSIVE on the index for the instant it writes one catalog row.
select 0;
