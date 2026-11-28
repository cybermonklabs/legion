-- 20261128154633_delivery_pull_requests_trailer_keys.up.sql
--
-- The keys a pull request's `Dispatch:` trailer lines name, which the attribution ranks above
-- every key the same text only mentions (delivery/attribution.go, dispatchTrailer):
--
--   attribution_body_trailer_keys    the keys the body's trailer lines name
--   attribution_commit_trailer_keys  the keys the commit messages' trailer lines name, in commit
--                                    order
--
-- They are read from GitHub with the other four attribution inputs and stored beside them, so
-- AttributePullRequests judges a stored row by the same rule as the head row. A row stored before
-- this migration holds null in both, which the attribution reads as no trailer: such a row is
-- judged by the keys it stored, as before.
--
-- Two nullable columns with no default: the change is to the catalog alone and rewrites no row.
alter table delivery_pull_requests
  add column attribution_body_trailer_keys text[],
  add column attribution_commit_trailer_keys text[];
