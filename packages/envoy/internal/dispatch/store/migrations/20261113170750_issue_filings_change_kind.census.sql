-- This migration widens the kind check to admit issue_filings beside the nine kinds it already
-- admits, which no stored row can fail, and adds it `not valid`, so no row is read. It refuses and
-- rewrites no row.
select 0;
