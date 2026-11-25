-- 20261124085214_check_states_backfill.up.sql
-- Marks owed every issue with an open attributed pull request head, so the settler's first pass
-- publishes the check states that exist before anything reads them. It runs after the triggers
-- (20261124085207 to 20261124085212), so a write that lands while it runs is marked by its own
-- trigger. Reads pull_request_heads (ACCESS SHARE), which no write waits behind, and writes only
-- check_states_owed; the notification each mark sends is delivered at this migration's commit.
select check_states_owe(issue_key)
from (select distinct issue_key from pull_request_heads where state = 'open' and issue_key is not null) owed;
