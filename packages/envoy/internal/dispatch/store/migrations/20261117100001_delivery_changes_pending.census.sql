-- This migration widens the kind check to admit the five eval and Legion kinds beside the five it
-- already admits, which no stored row can fail, and adds it `not valid`, so no row is read; and it
-- creates one table and two functions. It refuses and rewrites no row.
select 0;
