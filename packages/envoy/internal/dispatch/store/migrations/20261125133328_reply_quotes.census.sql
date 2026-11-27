-- 20261125133328 adds two nullable columns with no default and no check to comments and to
-- messages, replies_to_quote text and replies_to_occurrence integer, and fills neither: every
-- existing row holds both null, so it reads no stored row, and refuses and rewrites none.
select 0;
