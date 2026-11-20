-- messages gains two nullable text columns and a composite foreign key over them. A nullable
-- column with no default is a catalog change, so no stored row is rewritten, and the new columns
-- are null in every existing row. The key is added NOT VALID, so the ALTER reads no row of the
-- table at all; even validated it would refuse nothing, since a MATCH SIMPLE foreign key holds no
-- row where one of its columns is null. The number of existing rows this migration would refuse
-- or rewrite is therefore 0, whatever messages holds.
select 0;
