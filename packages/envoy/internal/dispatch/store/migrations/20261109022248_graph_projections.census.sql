-- Each of the three tables gains two nullable text columns and a composite foreign key over
-- them. A nullable column with no default is a catalog change, so no stored row is rewritten,
-- and the new columns are null in every existing row: a MATCH SIMPLE foreign key, which is what
-- a key written without MATCH FULL is, checks no row where one of its columns is null, so the
-- constraint's validation scan refuses nothing. parent_block_id is read by nothing yet. The
-- number of existing rows this migration would refuse or rewrite is therefore 0, whatever the
-- three tables hold.
select 0;
