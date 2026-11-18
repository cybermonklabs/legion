-- packages/envoy/internal/broker/store/migrations/0034_credential_request_dismissals.up.sql
-- One row for each credential-request record somebody took out of the approvers' lists after it
-- expired (dispatch://INFRA-3921). A record expires when its deadline passes before anyone decided
-- it, and the broker then refuses every decision of it. Before this, the list of an approver
-- (requests.Machine.PendingForApprover) held an expired record only until the expiry sweep ended
-- it, so it left the Inbox with no act of the person. Now the list holds every expired record that
-- has no row here, before the sweep and after it, and a dismissal (requests.Machine.Dismiss)
-- writes the row. A record is dismissed once, by the first person whose list holds it, and for
-- every list: a shared request's approver is anyone. dismissed_by is that person's canonical
-- login, or broker for the rows written below.
--
-- The backfill writes a row, dismissed by broker, for each record the sweep had already taken out
-- of the lists when this migration runs: each secret request whose request row is expired, or
-- cancelled at or after its deadline (pending_expires_at <= decided_at, a cancel that came after
-- the deadline, which the list now holds as expired), and each machine login with an expired
-- event. Without it, every request that expired since the broker began would come back into its
-- approver's Inbox at once. A request that is still pending is not written, overdue or not: it
-- was listed until now, and it stays listed, as pending until its deadline and as expired after it
-- until somebody dismisses it. A requests row names only a secret request's record (Create writes
-- record_id on a request it leaves pending, and nothing else does), so the first insert reads no
-- record. on conflict do nothing writes one row per record whatever the rows it reads hold.
--
-- The foreign key is added after the backfill rather than in the create table. Added first, it
-- checks each row the backfill inserts on its own, row-locking the record it names: measured on a
-- schema of 0032 seeded with 300,000 expired records (200,000 secret requests, 100,000 machine
-- logins, 624 MB of credential_requests), this file took 12.2 s that way and takes 4.4 s to 4.9 s
-- this way, of which the constraint's one validating join is under a second. Its SHARE ROW
-- EXCLUSIVE on credential_requests holds off every new record (a secret request that needs a
-- person, a machine login) until the runner's one transaction commits, so it is taken last: the
-- inserts before it read credential_requests, requests and credential_request_events under ACCESS
-- SHARE alone, and decisions and the sweep's events go on while it is held, since their foreign
-- key checks take only ROW SHARE. A record is never deleted (credential_requests_refuse_update),
-- so no row the backfill wrote can lose its record before the constraint is added.
create table credential_request_dismissals (
  record_id text primary key,
  dismissed_by text not null,
  dismissed_at timestamptz not null default now()
);

insert into credential_request_dismissals (record_id, dismissed_by)
select r.record_id, 'broker'
from requests r
where r.record_id is not null and (r.state = 'expired' or r.state = 'cancelled' and r.pending_expires_at <= r.decided_at)
on conflict do nothing;

insert into credential_request_dismissals (record_id, dismissed_by)
select ev.record_id, 'broker'
from credential_request_events ev join credential_requests cr on cr.id = ev.record_id and cr.kind = 'launcher_credential'
where ev.event = 'expired'
on conflict do nothing;

alter table credential_request_dismissals add constraint credential_request_dismissals_record_id_fkey
  foreign key (record_id) references credential_requests(id);
