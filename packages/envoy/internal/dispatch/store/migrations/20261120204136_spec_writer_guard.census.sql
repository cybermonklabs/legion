-- 20261120204136 adds one column to projects with a constant default, so every existing row takes
-- that default without being rewritten, and creates project_role_holder_marks, which holds no row at
-- this schema and which the migration fills from issue_role_holders and issues, which it only reads:
-- nothing is refused or rewritten. It cannot name the column or the table, which do not exist when
-- the census is taken.
select 0;
