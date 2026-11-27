-- 20261126182717_claim_cyber_policy_sessions.up.sql — every session of a claim whose content OpenAI
-- refused under its cyber policy, kept past the retry that forgets it (dispatch://INFRA-204).
--
-- A claim held for that refusal (supervise.Claim.HeldForCyberPolicy,
-- 20261125223008_claim_held_for) is held no more once a retry someone decided launches its agent
-- again: the retry forgets the session, which a new session replaces, and clears the failure. The
-- forgotten session's conversation stays in the daemon's history under the same token, and the
-- answer loop can still be asked to answer from it: a person's reply to an ask that session opened,
-- or a role message sent before the new session registers. So the loop calls no model for a
-- session this column lists (supervise.Claim.CyberPolicySessions and SessionHeldForCyberPolicy, the
-- daemon's answerGate). The claim's machine adds its session in the write that fails it under
-- `cyber_policy`, and again where it forgets the session of a claim still held, which a release
-- that recorded none failed (supervise's fail and forgetSession). The claim store writes the stored
-- list with each session of the machine's that it lacks added after it (upsertClaim), so no write
-- takes a session out.
--
-- A claim held now that still records its session needs no row here: it is held whatever session
-- the loop answers from, and the retry that ends the hold records the session it forgets. The
-- update below records two sessions that nothing of this release would find otherwise.
--
-- One is the session a retry of the release before forgot after such a refusal. That retry
-- abandoned the session under the failure's sentence (supervise's forgetSession,
-- claims.abandoned_session), and the sentence names the refusal in the words classifyProviderError
-- gives it, `OpenAI's cyber_policy refusal of this session's content`.
--
-- The other is the abandoned session of a claim held now that records no session. The leg reads
-- the hold as supervise.DecodeCause does: held_for = 'cyber_policy' beside failure_cause =
-- 'provider_error', on a claim failed or retired, and not held_for alone, since after a rollback
-- the release before 20261125223008_claim_held_for rewrites the cause and leaves held_for as it
-- was. The release before forgets a session first, under the asker's sentence rather than the
-- failure's (supervise's forgottenBy), for its `legion claims forget-session`, its `legion claims
-- retry --fresh-session` and the controller's fresh_session, and a forget-session alone leaves the
-- claim held. The retry of this release that ends the hold then forgets no session, so the session
-- the claim left behind is recorded here, and the machine records it at that retry too (supervise's
-- holdCyberPolicySession), for a claim forgotten so after this migration ran. Both take only a
-- session abandoned after the failure (abandoned_session's `at` after failed_at): a session
-- abandoned before it was not the one refused. This leg records nothing for a row whose failed_at
-- is null, as 20261111212544_claim_failure_cause left each claim it backfilled.
--
-- A claim records one abandoned session, its last, so of a claim that release held and retried
-- twice the second session alone is recorded. One claim stays unrecorded: a claim whose session
-- that release forgot after such a refusal and that it then retried, `legion claims retry
-- --fresh-session` and the controller's fresh_session among them. The retry ended its hold, and
-- its abandoned session carries the asker's sentence, which does not name the refusal, so no text
-- marks that session as refused, and the loop answers from its conversation.
--
-- There is no down file: migrations here are forward-only. The release before this one names each
-- column of claims it reads and writes (the claim store's claimSelect and upsertClaim), so it boots
-- with this column standing and leaves it as it is on every write; the database needs nothing
-- taken off for a rollback. That release records no session of a claim it fails under
-- `cyber_policy`. After the roll-forward such a claim is held while it stays failed or retired, and
-- the retry of this release that ends the hold records its session, or the session that release's
-- forget-session left behind after the failure. A session the release before retried away during
-- the rollback is recorded nowhere but in that claim's abandoned session, which no read of this
-- release takes as a hold.
alter table claims add column cyber_policy_sessions text[] not null default '{}';

update claims set cyber_policy_sessions = array[abandoned_session->>'session']
where abandoned_session->>'session' <> ''
  and (strpos(abandoned_session->>'why', 'OpenAI''s cyber_policy refusal of this session''s content') > 0
    or held_for = 'cyber_policy' and failure_cause = 'provider_error' and state in ('failed', 'retired')
      and session = '' and (abandoned_session->>'at')::timestamptz > failed_at);
