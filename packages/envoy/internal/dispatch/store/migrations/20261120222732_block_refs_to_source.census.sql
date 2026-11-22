-- An ordinary, non-unique index over columns block_refs already has refuses and rewrites no row.
-- The census report still names block_refs, the table it builds on, so a deployment sees what the
-- build locks and the table's size.
select 0;
