-- 20261125002856 adds one column to issue_role_holders with a constant default, so every existing
-- row takes that default, false (unmarked), without being rewritten, and nothing is refused. It
-- cannot name the column, which does not exist when the census is taken.
select 0;
