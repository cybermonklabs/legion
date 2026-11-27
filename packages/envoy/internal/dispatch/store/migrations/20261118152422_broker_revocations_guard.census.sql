-- 20261118152422 adds broker_revocations.session_generation, where it is missing, as a nullable
-- bigint with no default, replaces a plpgsql function, and drops and makes its two triggers, all
-- written into the catalogue alone; the triggers run on later writes only, so the migration reads,
-- refuses and rewrites no stored row, and counts 0.
select 0;
