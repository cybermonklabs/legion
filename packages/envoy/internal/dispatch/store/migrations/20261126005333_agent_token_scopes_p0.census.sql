-- 20261126005333 replaces agent_tokens_scopes_check with one that admits a third scope, `p0`, and
-- writes no row. The count is the stored rows the new check would refuse. It reads
-- agent_tokens.scopes, which 20261103070013_agent_token_scopes creates and which ships in an
-- earlier release, so the column is there when a deployment takes this census, before any
-- migration of this release applies. The check being replaced admits two of the three scopes the
-- new one admits, under the same rule that `approve` needs `person`, so no stored row is refused,
-- and a non-zero answer would mean a word reached the column that no release writes.
select count(*)
from agent_tokens
where not (scopes <@ array['person','approve','p0'] and ('approve' <> all(scopes) or 'person' = any(scopes)));
