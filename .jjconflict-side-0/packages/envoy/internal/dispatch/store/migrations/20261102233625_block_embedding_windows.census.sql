-- This migration widens the kind check to admit four kinds beside the five it already admits,
-- which no stored row can fail, and adds it `not valid`, so no row is read; it adds a column
-- with no default and a function. It refuses and rewrites no row; the index has its own migration.
select 0;
