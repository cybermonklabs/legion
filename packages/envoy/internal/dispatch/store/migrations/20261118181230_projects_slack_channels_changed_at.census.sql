-- 20261118181230 adds one nullable column to projects with no default, so every existing row reads
-- null without being rewritten, and nothing refuses any of them. It cannot name the column, which
-- does not exist when the census is taken.
select 0;
