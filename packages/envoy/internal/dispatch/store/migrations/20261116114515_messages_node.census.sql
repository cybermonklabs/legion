-- messages gains two nullable text columns, a boolean column with a constant default and no
-- constraint beyond not null, and a composite foreign key over the two text columns. A nullable
-- column with no default and a column with a constant default are each a catalog change, so no
-- stored row is rewritten, the text columns are null in every existing row and the boolean reads
-- its default. The key is added NOT VALID, so the ALTER reads no row of the table at all; even
-- validated it would refuse nothing, since a MATCH SIMPLE foreign key holds no row where one of
-- its columns is null. The number of existing rows this migration would refuse or rewrite is
-- therefore 0, whatever messages holds.
select 0;
