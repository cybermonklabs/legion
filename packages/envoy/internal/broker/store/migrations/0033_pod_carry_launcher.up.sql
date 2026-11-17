-- packages/envoy/internal/broker/store/migrations/0033_pod_carry_launcher.up.sql
-- A pod's grants follow the pair of its account and the token login that enrolled it, not its
-- account alone (enroll.carryPrincipal, exchange.PodLaunchPrincipal; plan E's
-- t-e-54). Every public Legion project's pods run as one account, legion-worker, and each
-- project's daemon logs in as an account of its own, so under the account alone a role of one
-- project's pod that enrolled under another project's session name took that session's grants and
-- pending requests (enroll's adopt). Under the pair it takes nothing.
--
-- Every pod row that recorded its account as its carry principal is rewritten to the pair its own
-- daemon's next enrolment spells, read from the credential the row was enrolled under
-- (launcher_credentials.subject, the account that token login proved), so a running session's live
-- and dormant rows keep their grants for the next box its own daemon enrols it on. The spelling
-- here is exchange.PodLaunchPrincipal's, character for character: 'account:' <pod account>
-- ' launched-by ' 'account:' <login account>, which the store's migration test compares.
--
-- A pod row enrolled under a login that proved no account (one a person approved through a
-- machine-login code; a service's login holds no operator) keeps its account principal. No new
-- enrolment spells that string, since such a login's pods now carry nothing, so nothing adopts the
-- row; its own end leaves what it holds dormant for the carry sweep to revoke, as it would for any
-- session that never comes back.
--
-- The broker rolls one replica at a time, so the older one goes on writing account-alone rows
-- after this has run. enroll's adopt takes such a row as a predecessor when its login proved the
-- same account as the enrolling login's, so a session the old replica enrolled keeps its grants
-- for its own daemon's next enrolment, and another daemon's still takes nothing. A rollback to a
-- broker before this one strands every row this rewrote: that broker spells the account alone and
-- adopts none of them, so their grants are swept at their end and the sessions ask again.
--
-- The rows are locked in id order before they are written, the order adopt locks its predecessors
-- in, so this cannot deadlock with an old replica's adoption of the same rows and fail the boot.
with rewritten as (
  select e.id, e.carry_principal || ' launched-by ' || 'account:' || c.subject as carry
  from enrollments e
  join launcher_credentials c on c.id = e.launcher_credential_id
  where e.kind = 'pod'
    and e.carry_principal = 'account:' || e.subject
    and c.subject is not null
  order by e.id
  for update of e)
update enrollments e
set carry_principal = r.carry
from rewritten r
where e.id = r.id;
