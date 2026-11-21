-- 20261121042211 adds one column to session_projects with a constant default, so every existing
-- row reads `event` without being rewritten, and its check refuses none of them. It cannot name
-- the column, which does not exist when the census is taken.
select 0;
