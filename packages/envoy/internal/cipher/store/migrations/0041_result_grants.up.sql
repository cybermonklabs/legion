-- packages/envoy/internal/cipher/store/migrations/0041_result_grants.up.sql
-- A CI run's result grant: what the token exchange decided for a result entry's run
-- (BROKER_EXCHANGE_RESULT_WORKFLOWS), kept by the broker in place of an access token the run would
-- hold. The run holds only the grant's airlock placeholder; every broker pod reads the grant by that
-- placeholder's SHA-256 (placeholder.Hash) and mints the run's results token itself, so the grant
-- lives here rather than in one pod's memory, and a pod's end ends no grant.
--
-- The row holds the session the exchange decided: its subject and principal, the OAuth scopes it
-- granted, and its extra claims (the run as "ci", the person, the project scope, the projects), the
-- claims a minted token's session carries. It holds no token and no key. expires_at is compared with
-- the database's own now() by every read (oauthstore.Store.ResultGrantByPlaceholder, ResultGrantByID)
-- and by the sweep, which deletes a row once it has passed, through the index below.
create table if not exists result_grants (
  id uuid primary key,
  placeholder_sha256 text not null unique,
  subject text not null,
  session text not null,
  principal text not null,
  scopes text[] not null,
  extra jsonb not null,
  expires_at timestamptz not null,
  created_at timestamptz not null default now()
);
create index if not exists result_grants_expires on result_grants (expires_at);
