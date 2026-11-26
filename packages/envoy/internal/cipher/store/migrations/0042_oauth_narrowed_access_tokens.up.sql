-- packages/envoy/internal/cipher/store/migrations/0042_oauth_narrowed_access_tokens.up.sql
-- The access tokens a trade narrowed to reading: a token exchange that asked for the scope read
-- mints a token that keeps the identity its trade would have had, holds read, and writes nothing
-- (oauthstore.Store.CreateAccessTokenSession). Each row has oauth_access_tokens' columns and two
-- of its own: subject_signature, the signature of the machine login's access token the token was
-- traded from (null for a trade whose subject is a JWT, which the broker keeps no row of), and
-- jwk_thumbprint, the key the token is bound to, which its session's jwk_thumbprint also holds.
--
-- A broker build before this migration reads nothing of this table, and that is why it is a table
-- of its own: such a build, running on this store during a roll or a rollback, finds no narrowed
-- token when it introspects one, and so answers it inactive, and finds none when one is presented
-- as a subject token, and so refuses it. None of those builds answers a narrowed token as one
-- that writes.
--
-- A narrowed token's end is decided at every read of it (oauthstore's narrowedLive), not written
-- here when what it was traded from ends: it is live while its own row is active and unexpired,
-- while the launcher credential it names (a person's sign-in's, or a machine login's) is neither
-- revoked nor expired, and while the machine login's access token named by subject_signature is
-- still in oauth_access_tokens, active and unexpired. A revoke, a retire and a sweep made by an
-- older build end it the same way. A credential deleted outright takes its narrowed tokens with
-- it.
--
-- The last index is on oauth_access_tokens: the key a read entry's token is bound to, among the
-- active rows whose project scope is none, which the writers of a launcher credential's key read
-- (oauthstore.KeyHoldsReadToken) so that no key holds both a live read token and a live launcher
-- credential. The table is swept of expired rows, so the index stays small, and it covers the
-- read entry tokens older builds mint as well.
create table oauth_narrowed_access_tokens (
  signature text primary key,
  request_id text not null,
  credential_id uuid references launcher_credentials(id) on delete cascade,
  request jsonb not null,
  active boolean not null default true,
  expires_at timestamptz not null,
  subject_signature text,
  jwk_thumbprint text not null
);
create index oauth_narrowed_access_tokens_request on oauth_narrowed_access_tokens (request_id);
create index oauth_narrowed_access_tokens_expires on oauth_narrowed_access_tokens (expires_at);
create index oauth_narrowed_access_tokens_credential on oauth_narrowed_access_tokens (credential_id);
create index oauth_narrowed_access_tokens_key on oauth_narrowed_access_tokens (jwk_thumbprint) where active;
create index oauth_access_tokens_read_key on oauth_access_tokens ((request #>> '{session,jwk_thumbprint}'))
  where active and (request #>> '{session,extra,project_scope}') = 'none';
