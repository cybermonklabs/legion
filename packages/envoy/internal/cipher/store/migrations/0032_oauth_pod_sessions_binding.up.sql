-- packages/envoy/internal/broker/store/migrations/0032_oauth_pod_sessions_binding.up.sql
-- Which key a pod's registration for a session is bound to, and which of its registrar's writes
-- last set it.
--
-- jkt is the RFC 7638 thumbprint of the key that session's own role proves (its agent-secrets
-- key), which the Legion daemon names as it registers each role. The roles of one issue pod read
-- one exchange token and are registered for one pod uid, so without this any role holding the
-- pod's token and a key of its own could trade for a sibling role's session; with it, the
-- exchange admits a trade for the session only with a DPoP proof of this key
-- (oauthstore.Store.PodSessionRegistered). Null is a registration bound to the pod alone, as every
-- row was before this column and as a session watch registers a session pod's one session, which
-- runs no sibling role. A write that names no key keeps the key a live row names
-- (oauthstore.Store.RegisterPodSession), so only the sweep's deletion of a lapsed row ever
-- unbinds it.
--
-- generation is the registrar's own order of its writes for this pod and session: the Legion
-- daemon sends the role's incarnation, so a write an old incarnation sent, which the broker
-- commits only after the new incarnation's write, finds a higher generation and changes nothing,
-- and cannot put the old key back for up to a renewal interval. A write sets the key, the expiry
-- and the generation only when its generation is at least the row's. A registrar that sends none
-- writes 0, as every row was before this column and as a session watch writes, so its writes
-- apply exactly as before against rows that are 0 too.
alter table oauth_pod_sessions
  add column jkt text,
  add column generation bigint not null default 0;
