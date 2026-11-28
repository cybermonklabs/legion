-- 20261120023712_agent_read_tokens.up.sql — the personal agent tokens that may only read.
--
-- INFRA-4619: a person can mint a personal token narrowed to reading. Dispatch answers such a
-- token on the GET routes it classes as reads and refuses it every other route, 403
-- READ_ONLY_TOKEN (api/server.go, refuseReadOnlyToken). The token keeps the `dsp_` form and
-- length of every personal token, so no redaction pattern changes.
--
-- A table of its own, and not a third word in agent_tokens.scopes, because Dispatch rolls: the
-- release before this one is still serving against this schema while the new one boots. That
-- release looks a bearer up in agent_tokens alone (`where token_hash = $1`), finds no row for a
-- token kept here, and answers it 401. Had the token been a row of agent_tokens, that release
-- would read its scopes, and a token holding `person` beside `read` would write as its person
-- through every route the old release serves. So a read-only token is refused by every build:
-- this one refuses its writes, and the one before refuses it whole.
--
--   scopes      what the token holds: `read` always, and `person` where the token reads as its
--               person (the routes a person reaches through the dashboard). Never `approve`,
--               since settling what a person settles is a write. The check holds every row to
--               that, so direct SQL and a repair by hand are held to it as the mint is
--               (api/agent_tokens.go).
--   token_hash  the SHA-256 of the token, unique, as in agent_tokens. Dispatch looks a bearer up
--               in both tables in one query by this hash.
--
-- The other columns are agent_tokens' own, and the index serves the list of a person's tokens,
-- newest first, as agent_tokens_owner_created_at serves it there.
--
-- A new table that no earlier release names: nothing existing is read or rewritten, so it rolls
-- (cmd/dispatch/README.md, "Database migrations") and the census answers 0. The identifier is the
-- UTC moment this was written, as `migrationid` requires of every migration past the numbered
-- series.
create table agent_read_tokens (
  id uuid primary key default gen_random_uuid(),
  owner text not null,
  name text not null,
  token_hash bytea not null unique,
  prefix text not null,
  scopes text[] not null
    check (scopes <@ array['read','person'] and 'read' = any(scopes)),
  created_at timestamptz not null default now(),
  last_used_at timestamptz,
  revoked_at timestamptz
);
create index agent_read_tokens_owner_created_at on agent_read_tokens (owner, created_at desc);
