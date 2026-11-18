-- packages/envoy/internal/broker/store/migrations/0033_pod_carry_launcher.up.sql
-- A pod's grants follow the pair of its account and the token login that enrolled it, not its
-- account alone (enroll.carryPrincipal, exchange.PodLaunchPrincipal; plan E's
-- t-e-54). Every public Legion project's pods run as one account, legion-worker, and each
-- project's daemon logs in as an account of its own, so under the account alone a role of one
-- project's pod that enrolled under another project's session name took that session's grants and
-- pending requests (enroll's adopt). Under the pair it takes none of them.
--
-- Every pod row a login that proved an account enrolled, which carries its account alone, is
-- rewritten to the pair that login spells, read from the credential the row was enrolled under
-- (launcher_credentials.subject, the account that token login proved). The spelling here is
-- exchange.PodLaunchPrincipal's, character for character: 'account:' <pod account>
-- ' launched-by ' 'account:' <login account>, which enroll's daemon_carry_test.go compares
-- (TestARunningSessionKeepsItsGrantsAcrossTheCarryMigration).
--
-- On a new replica the rewrite changes the adoption of no row. enroll's adopt takes an
-- account-alone pod row as a predecessor when the row's login proved the same account as the
-- enrolling login's, which is the condition this rewrite requires, and the pair it writes matches
-- on the same two values, so a running session keeps its grants for its own daemon's next
-- enrolment with or without it.
--
-- The rewrite is for the roll. The broker rolls one replica at a time, and an older replica's
-- adopt matches the account alone, so it would hand a row written before this ran to a pod of
-- that account that any daemon enrolled under the row's session name. A rewritten row carries the
-- pair, which no older replica matches, so an older replica adopts none of the rows this rewrote
-- (TestARunningSessionKeepsItsGrantsAcrossTheCarryMigration asserts that its match finds none).
-- That closes the takeover for those rows alone. Until the roll ends, an older replica still
-- adopts, for another daemon's pod enrolled under their session name, the account-alone rows it
-- and every other older replica go on writing after this has run, and the rows this leaves alone
-- (below). A new replica adopts an account-alone row an older replica wrote only for an enrolment
-- whose login proved the account the row's login proved.
--
-- The rewrite costs two things. While the broker rolls, a session enrolled on an older replica
-- adopts no row this rewrote, so it starts with no grants until its next enrolment reaches a new
-- replica, inside the carry window; the grants stay on their rows meanwhile. And a rollback to a
-- broker before this one strands every row that carries the pair, the ones this rewrote and the
-- ones a new replica wrote: that broker spells the account alone and adopts none of them, so
-- their grants are swept at the carry window's end and the sessions ask again.
--
-- A pod row enrolled under a login that proved no account (one a person approved through a
-- machine-login code; a service's login holds no operator) keeps its account principal. No
-- enrolment a new replica serves spells that string, since such a login's pods now carry nothing,
-- so no new replica adopts the row; its own end leaves what it holds dormant for the carry sweep
-- to revoke, as it would for any session that never comes back.
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
