-- packages/envoy/internal/cipher/store/migrations/0042_result_grants.up.sql
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
--
-- entry_digest is the results entry the grant was made under: the hex SHA-256 of the profile
-- DISPATCH_RESULTS_TOKEN's fingerprint (policy.Profiles.Digest) and the scheme and host of the
-- private destination (BROKER_AIRLOCK_PRIVATE_UPSTREAM), as the granting broker computed it at
-- boot. Every pod compares it with its own before it mints the grant's token, and refuses a grant
-- of another entry without ending it, so the pods whose entry is the grant's still serve it. It is
-- a column rather than a member of extra: extra holds the session's claims, which a minted token's
-- session copies whole, and the entry is the broker's, no claim of the run's. No stored digest is
-- empty: a broker with no results entry has the empty digest, grants nothing and serves no grant.
create table if not exists result_grants (
  id uuid primary key,
  placeholder_sha256 text not null unique,
  subject text not null,
  session text not null,
  principal text not null,
  scopes text[] not null,
  extra jsonb not null,
  entry_digest text not null check (entry_digest ~ '^[0-9a-f]{64}$'),
  expires_at timestamptz not null,
  created_at timestamptz not null default now()
);
create index if not exists result_grants_expires on result_grants (expires_at);
