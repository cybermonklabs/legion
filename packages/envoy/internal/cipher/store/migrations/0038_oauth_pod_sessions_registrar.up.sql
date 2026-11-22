-- packages/envoy/internal/broker/store/migrations/0038_oauth_pod_sessions_registrar.up.sql
-- Which registrar wrote a pod's registration for a session, and whether that registrar has
-- released it.
--
-- registrar is the login account of the daemon whose write last set the row (the serviceAuth
-- caller's ServiceAccount subject, never the service name every Legion daemon shares, and never
-- its pod uid, which a restart changes). A registrar holds a session name while a live row of
-- that name names it: another registrar's write of the name is refused (409
-- SESSION_HELD_BY_ANOTHER_REGISTRAR), and the exchange admits a pod's trade under the name only
-- for a live row of that pod that names a registrar and is not released
-- (oauthstore.Store.RegisterPodSession, oauthstore.Store.PodSessionRegistered). Null is a row a
-- broker of the build before this column wrote, while the broker rolls or after a rollback: it
-- holds no name, no exchange honours it, and the next admitted write of its pod and name takes it
-- whole.
--
-- released_at is when the registrar released the row. A released row stays until it expires and
-- keeps the name bound to its registrar until then, so no other daemon takes a name while a token
-- of its old pod can still be presented, but no exchange honours it, and the registrar's next
-- write of its pod and name replaces it whole (registrar, key, generation, expiry, and this mark
-- cleared), since a release ends a registration and its key and generation must not carry over.
--
-- The index on session is what the write's holder check reads: every live row of the name, of
-- every pod. It is a plain index inside the migration's transaction, as 0021's
-- oauth_pod_sessions_expires is, since the broker's runner applies no migration outside one.
alter table oauth_pod_sessions
  add column registrar text,
  add column released_at timestamptz;
create index oauth_pod_sessions_session on oauth_pod_sessions (session);
