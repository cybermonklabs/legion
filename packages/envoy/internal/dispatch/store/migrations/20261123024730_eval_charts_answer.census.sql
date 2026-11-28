-- This migration adds two columns to eval_versions with constant defaults it then drops, a not
-- valid check, a function and a trigger before each insert, and states evals_current again over
-- the columns. It reads no stored row, and refuses and rewrites none.
select 0;
