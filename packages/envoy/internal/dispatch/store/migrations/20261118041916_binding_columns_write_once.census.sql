-- 20261118041916 adds a check on people.sub and a foreign key from user_sessions to people, both
-- not valid, and plpgsql functions and triggers, all written into the catalogue alone: it reads,
-- refuses and rewrites no stored row. The check does refuse the next write of a row that holds an
-- empty sub, so the census counts those rows, and a deploy waits until there are none. people.sub
-- is 0112_people_sub's, which every deployed database applied releases before this one.
select count(*) from people where sub = '';
