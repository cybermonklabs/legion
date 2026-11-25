-- packages/envoy/internal/cipher/store/migrations/0043_request_secrets_github_app_field.up.sql
-- A request may come from an agent-secrets of a release before --target, which narrows a stored
-- github-app secret in the body's github_app field rather than in its signed locations
-- (requests.Machine.CreateNarrowed). Such a request is served as one profile of that secret, and
-- its release hands over a token outside the airlock, which no other request's release does. Each
-- name of such a request records that it was made that way, and what the field's read_only said,
-- so that:
--
--   * a release that carries a github_app field is answered only for a grant made that way, with
--     the same read_only (requests' narrowedRelease), and never for a --target grant, whose
--     release the airlock holds;
--   * a live grant is used again only for a request of the same kind and the same read_only
--     (requests.Machine.reuseLiveGrant), so neither form inherits the other's grant.
--
-- github_app_field is false for every row written before this migration and for every --target
-- request after it, and the column's default writes no row: Postgres 11 and later add a column
-- with a constant default without rewriting the table. github_app_read_only is null for every row
-- but a narrowed request's. So the two tell a narrowed request apart from every other request,
-- and a row written before this migration reads as every --target row does.
alter table request_secrets
	add column github_app_field boolean not null default false,
	add column github_app_read_only boolean null;
