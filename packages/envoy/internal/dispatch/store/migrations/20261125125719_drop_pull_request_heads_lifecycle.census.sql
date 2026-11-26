-- Two triggers, a function and a table dropped, under a lock of pull_request_heads and that table
-- held to the commit: no row of pull_request_heads is refused or rewritten. The table is
-- 20261125125715's, of this same release, so the census, taken before any migration of the release
-- applies, finds no row of it to count, and nothing reads the rows it holds once the install has.
select 0;
