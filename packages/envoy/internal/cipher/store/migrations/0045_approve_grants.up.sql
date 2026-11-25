-- packages/envoy/internal/cipher/store/migrations/0045_approve_grants.up.sql
-- A person's approve grant: what the token exchange decided when a session on a box where the
-- person signed in traded that sign-in for a placeholder of Cipher's proxy for the approve
-- capability (requests.MintedKind DISPATCH_APPROVE_TOKEN). The session holds only the
-- placeholder; every broker pod reads the grant by that placeholder's SHA-256 and mints the
-- approve token itself, bound to its own key, so the grant lives here rather than in one pod's
-- memory.
--
-- The row holds the session the exchange decided: its subject and principal, the scopes (approve
-- alone) and the extra claims a minted token carries, the digest of the dpop block and private
-- destination it was made under, and the sign-in's launcher credential. It holds no token and no
-- key. Every read compares expires_at with the database's own now() and joins the credential, so
-- a grant past its end, or one whose sign-in was revoked or has expired, reads as not live; the
-- credential's delete removes the grant with it.
create table if not exists approve_grants (
  id uuid primary key,
  placeholder_sha256 text not null unique,
  credential_id uuid not null references launcher_credentials(id) on delete cascade,
  subject text not null,
  session text not null,
  principal text not null,
  scopes text[] not null,
  extra jsonb not null,
  entry_digest text not null check (entry_digest ~ '^[0-9a-f]{64}$'),
  expires_at timestamptz not null,
  created_at timestamptz not null default now()
);
create index if not exists approve_grants_expires on approve_grants (expires_at);
create index if not exists approve_grants_credential on approve_grants (credential_id);
