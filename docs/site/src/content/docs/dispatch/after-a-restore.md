---
title: After a restore
description: Resetting every credential after Dispatch's database is restored or made again alone, recovering after the secrets broker's database is restored or made again alone, and moving a deployment between the Cognito pool and a domain.
sidebar:
  order: 9
---

These procedures belong to [Signing people in](/legion/dispatch/running-dispatch/#signing-people-in)
on Running Dispatch, which says why Dispatch's database and the secrets broker's are reset or
restored together, never one alone. Two things that section gives are used here:

- `$BROKER_PSQL_URL`, the libpq URL of the broker's database, which every block below that reads
  the broker's database names: make it first, as that section shows.
- **The broker clear**, the one transaction that lifts one refusal on the broker's database, which
  the steps below name as the broker clear above.

## After a Dispatch database restored or made again alone

Reset every credential a person holds, at Dispatch and at the broker, rather than mend the
addresses one by one. A restored database disagrees with the broker in ways no read of one
address finds: a replacement the broker never applied, because it was still owed when the
database was lost, leaves the broker's record of the address where it was; an address first
signed in after the backup has no row to compare; and a refusal at the broker can be the stale
side as well as Dispatch's row. The reset ends all of it, and costs every person one sign-in and
every agent a new personal token. Five steps, in order:

1. End every browser session and revoke every personal token, on Dispatch's database. This records
   the reset's start.
2. End every person's credential at the broker, at once.
3. Clear a refusal at the broker only where the Workspace directory names the refused account as
   the address's holder.
4. No sooner than an hour and ten minutes after step 1, end every person's credential at the broker
   again, then close the reset, which records its end.
5. Start Dispatch again.

Stop every Dispatch process before the first step (scale its Deployment to zero), as the move
between the pool and a domain below does, so no sign-in and no token mint runs while the steps
do; the broker keeps serving. Keep every process stopped until the reset is closed, and tell
people not to sign in at the broker (`agent-secrets login`, the approval page) until step 5: the
broker serves a sign-in without asking Dispatch. Step 1 settles every owed broker revocation, the
ones restored Dispatch recorded after the restore included, so the broker never refuses the
accounts they name, and step 2 ends at once whatever those accounts hold at the broker. A refusal
would also have stopped a Google ID token issued to such an account before its address moved,
which lives at most an hour, and the broker can still trade one during that hour. Every move
Dispatch recorded comes before step 1, an operator's block run after the stop included, so an hour
after step 1 every such token has expired. The ten minutes more cover two clocks of the broker's
against the clock of Dispatch's database, which stamps the reset's start: the broker process's,
which checks each token's expiry, and the broker database's, which stamps each credential's
creation (`created_at`, its `default now()`), the moment the close's lists compare with the wait's
end. Widen them where either is further from Dispatch's database's. Step 4 then ends whatever
such a token traded, and the close refuses until that wait has passed and its own read of the
broker finds no person credential live that was made before the wait ended. One made after it can
only be the address holder's, so the close leaves it, and a person who signed in early keeps
Dispatch down no longer.

**End every browser session and revoke every personal token.** A cookie carries its address and a
generation, and Dispatch admits it while the generation is its address's in `user_sessions`, so
raising every row ends every cookie. The block raises every row, in one transaction, to a number
above every generation the lost history can have issued: the greatest of the microseconds since
the epoch at the raise, the broker's greatest recorded generation plus one, and the highest row
and floor Dispatch holds plus one. It writes that number first as the deployment's floor
(`session_generation_floors`, the row `*`, migration `20261119183355`), where every session row
made later starts, so an address the restored database has never seen also starts above every
cookie of the lost history; then it raises the rows, revokes every personal token, and settles
every restored broker revocation, each of which the credentials and directory steps cover instead
of letting the first pass after the reset apply it at the broker. Last it records the reset's start
(`deployment_resets`, migration `20261120144819`) with the transaction's own `now()`, the very
`done_at` of every revocation it settles, which the recovery after a broker restored alone reads
(below). A restored database may have brought each token back from an account that no longer holds
its address. The time term is what keeps a second restore from undoing the first reset: restored
to before it, the database loses both the floor and the raised rows, and the cookies issued under
the first reset's number would match a raise to that same number again; a reset run later takes a
later time, so its number is above the first one's, as long as the database's clock has not been
set back by more than the time between the two. The broker's term keeps the number at or above
every generation the broker recorded, so it refuses no approve `APPROVER_REPLACED` for it. The
number is about 1.8 × 10¹⁵, inside a JSON number's exact range. The block reads the broker's
generation first, and prints `UNKNOWN` and changes nothing when it cannot:

```sh
(
set -o pipefail
greatest=$(psql "$BROKER_PSQL_URL" -v ON_ERROR_STOP=1 -At <<'SQL'
select coalesce(max(session_generation), 0) from person_replacement_generations;
SQL
) || { echo 'UNKNOWN: the broker database could not be read, so nothing was reset'; exit 1; }
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -v broker_greatest="$greatest" <<'SQL'
begin;
lock table session_generation_floors, user_sessions, agent_tokens, broker_revocations, deployment_resets in exclusive mode;
create temp table reset_generation on commit drop as select greatest((extract(epoch from clock_timestamp()) * 1000000)::bigint, :'broker_greatest'::bigint + 1, coalesce((select max(generation) from user_sessions), 0) + 1, coalesce((select max(generation) from session_generation_floors), 0) + 1) as generation;
insert into session_generation_floors (login, generation) select '*', generation from reset_generation on conflict (login) do update set generation = excluded.generation where excluded.generation > session_generation_floors.generation;
update user_sessions set generation = r.generation from reset_generation r where user_sessions.generation < r.generation;
update agent_tokens set revoked_at = now() where revoked_at is null;
update broker_revocations set done_at = now() where done_at is null;
insert into deployment_resets (mark) values ('start');
select generation from reset_generation;
commit;
SQL
)
```

It prints the number it raised to. Run again, it raises again, past the first number, and records
a later start. It locks `deployment_resets` with the other tables, and the close locks it before it
records the end, so a close run beside step 1 either records its end before step 1 takes the lock,
or waits for step 1 to commit, then finds step 1's start as the newest mark and records nothing.

**End every person's credential at the broker.** The broker serves a person's sign-in on a
machine, a machine login a person approved and an approval-page session without asking Dispatch,
so the reset ends each there: a sign-in and a machine login with the calls Dispatch's machine-login
page makes, and a page session with the broker's own `envoy-broker end-sessions`. A service's
machine login, the Legion daemon's, is not a person's and is left. The calls take the broker's
URL and a token of Dispatch's own ServiceAccount, kept off every command line as the machine-login
block below keeps it. The subcommand writes the broker's database as the serving broker does, so
it runs wherever your deployment runs a one-off process of the broker's image, with the serving
broker's settings: its database URL and whatever it signs in to the database with,
`BROKER_PUBLIC_URL`, and its secret store's settings, with `BROKER_SIGNIN=off` in place of every
`BROKER_SIGNIN_*` setting where the image has that setting. With that switch, leave every model
judge setting unset too: the broker refuses a judge beside sign-in off because no request waits for
a person, and the broker's [Running a subcommand](/legion/broker/operate/#running-a-subcommand)
lists them all. `broker_run` below is the
command that runs its arguments there. Each call that fails prints `NOT REVOKED <id>` or
`NOT ENDED <login>`, and the last step reads the broker's database again:

```sh
(
umask 077
set -o pipefail
header=$(mktemp) || exit 1
trap 'rm -f "$header"' EXIT
broker_url='<the URL in DISPATCH_AGENT_SECRETS_URL>'
broker_run() { '<the command that runs its arguments where your deployment runs a broker subcommand>' "$@"; }
token=$(kubectl -n '<the namespace Dispatch runs in>' create token '<the ServiceAccount Dispatch runs as>' --audience '<the audience in BROKER_K8S_OIDC_AUDIENCE>' --duration 1h) || { echo 'no token was minted'; exit 1; }
printf 'Authorization: Bearer %s\n' "$token" >"$header"
signins() { psql "$BROKER_PSQL_URL" -v ON_ERROR_STOP=1 -At -F ' ' -c "select id, person from person_signins where revoked_at is null order by id"; }
logins() { psql "$BROKER_PSQL_URL" -v ON_ERROR_STOP=1 -At -F ' ' -c "select c.id, r.approver from launcher_credentials c join credential_requests r on r.id = c.record_id and r.kind = 'launcher_credential' where c.service is null and c.revoked_at is null order by c.id"; }
pages() { psql "$BROKER_PSQL_URL" -v ON_ERROR_STOP=1 -At -c "select distinct login from browser_sessions where expires_at > now() order by login"; }
signins | while read -r id person; do
  curl -sS --fail-with-body -w '\n' -X POST -H @"$header" -d "{\"person\":\"$person\"}" "$broker_url/v1/person-signins/$id/revoke" || echo "NOT REVOKED $id"
done
logins | while read -r id approver; do
  curl -sS --fail-with-body -w '\n' -X POST -H @"$header" -d "{\"approver\":\"$approver\"}" "$broker_url/v1/launcher-credentials/$id/revoke-by-approver" || echo "NOT REVOKED $id"
done
pages | while read -r login; do
  broker_run envoy-broker end-sessions "$login" </dev/null || echo "NOT ENDED $login"
done
if left=$( { signins && logins && pages; } ); then
  if [ -z "$left" ]; then echo 'no person credential is live at the broker'; else printf 'STILL LIVE:\n%s\n' "$left"; false; fi
else
  echo 'UNKNOWN: the broker database could not be read, so what is left is unknown'; false
fi
)
```

Only `no person credential is live at the broker` means done. `STILL LIVE` lists each sign-in and
machine login by id and person, and each login with a page session, that no call ended; the lines
above give the broker's answer for each: for `ACCOUNT_TOKEN_EXPIRED` mint another token, for any
other answer fix what it names, and run the block again. `UNKNOWN` says nothing about the broker:
run it again once `$BROKER_PSQL_URL` connects. Run the block right after step 1, and again for step
4, no sooner than an hour and ten minutes after step 1.

**Clear a refusal only for the directory's holder.** The broker refuses each account a
revocation named, under each address, and a restored Dispatch cannot tell which of those refusals
are right: a take-back the lost database still owed leaves the broker refusing the account Google
gives the address now. Only the identity provider settles it, so read, for each address the broker
refuses any account at, which account holds the address in the Workspace directory, whose user
`id` is the Google `sub` an ID token carries ("equivalent to the ID exposed in the Directory API",
Google's token types page). Set `DIRECTORY_TOKEN` to a Workspace directory read token from wherever
the deployment keeps it, holding
`https://www.googleapis.com/auth/admin.directory.user.readonly`. The directory's answer counts only
when it is `200` and names the address itself as `primaryEmail`, since `users.get` answers an alias
too; a `404` counts as no account. Every other status is `UNKNOWN`, leaves every refusal alone and
makes the block fail, so repair the directory read before continuing. The directory answers `404`
for an address that is no account of the organization: someone from outside the domain
([People from outside the domain](/legion/dispatch/running-dispatch/#people-from-outside-the-domain))
and a deleted Workspace user alike. Where it names a refused account, clear that one refusal, with
the broker clear above; every other refusal stays. The token goes in a file curl reads the header
from, never in the command's arguments, which every user of the box reads from `/proc/<pid>/cmdline`
while it runs:

```sh
(
umask 077
set -o pipefail
header=$(mktemp) || exit 1
trap 'rm -f "$header"' EXIT
printf 'Authorization: Bearer %s\n' "$DIRECTORY_TOKEN" >"$header"
people=$(psql "$BROKER_PSQL_URL" -v ON_ERROR_STOP=1 -At -c "select distinct person from replaced_person_subjects order by person") || {
  echo 'UNKNOWN: the broker database could not be read, so no refusal was cleared'
  exit 1
}
unknown=0
while read -r person; do
  [ -n "$person" ] || continue
  answer=$(curl -sS --fail-with-body -w '\n%{http_code}' -H @"$header" "https://admin.googleapis.com/admin/directory/v1/users/$person?projection=basic&viewType=admin_view" || true)
  status=${answer##*$'\n'}
  answer=${answer%$'\n'*}
  case "$status" in
  200) ;;
  404) echo "NO ACCOUNT $person: the directory answered no holder"; continue ;;
  *) echo "UNKNOWN $person: the directory read failed ($status)"; unknown=1; continue ;;
  esac
  id=$(printf '%s' "$answer" | jq -r '.id') && email=$(printf '%s' "$answer" | jq -r '.primaryEmail' | tr '[:upper:]' '[:lower:]') || { echo "UNKNOWN $person: the directory's 200 answer did not parse"; unknown=1; continue; }
  if [ "$email" != "$person" ]; then echo "NO ACCOUNT $person: the directory names $email"; continue; fi
  echo "HOLDER $person $id"
  psql "$BROKER_PSQL_URL" -v ON_ERROR_STOP=1 -v person="$person" -v issuer='https://accounts.google.com' -v subject="$id" <<'SQL' || { echo "NOT CLEARED $person: the broker database refused the clear"; unknown=1; }
begin;
select set_config('broker.replaced_subject_delete', jsonb_build_array(pg_current_xact_id()::text, :'person', :'issuer', :'subject')::text, true);
delete from replaced_person_subjects where person = :'person' and issuer = :'issuer' and subject = :'subject';
commit;
SQL
done <<< "$people"
[ "$unknown" -eq 0 ] || exit 1
)
```

`HOLDER` names each address whose holder the directory gave; the delete after it deletes nothing
where the broker did not refuse that account. `NO ACCOUNT` leaves the address's refusals as they
are. `UNKNOWN` leaves every refusal alone and makes the block fail, and so does `NOT CLEARED`, a
clear the broker's database refused, which leaves that holder refused: correct the read or the
database and run the block again. For an address the directory answered `404` for, the holder is
whoever signs in to Dispatch as that address once it is back: Google gives an ID token for an
address to the account that holds it alone. Once that person has signed in, read the account the
address records (`select sub from people where email = '<address>'`); if the broker refuses that
`sub` at the address, clear it with the broker clear above.

**End every person's credential at the broker again, and close the reset.** No sooner than an hour
and ten minutes after step 1, run **End every person's credential at the broker** again, until it
prints `no person credential is live at the broker`, then close the reset. The close first reads
Dispatch's database for the open reset, the newest mark by `id`, which must be a start, and its
wait, and it reads the broker only once the wait has passed, so no read made inside the wait can
stand for a close recorded after it. It then reads the broker's database with the credentials
block's own three lists, less what was made after the wait ended, which only an address's holder
can make, and records the reset's end only when none is live. It prints `NOT CLOSED` when no reset
is open or the wait has not passed, `STILL LIVE` with each credential it found, and `UNKNOWN` when
a database cannot be read, and changes nothing for any of them:

```sh
(
set -o pipefail
cutoff=$(psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -qAt <<'SQL'
select case when m.mark is distinct from 'start' then 'none' when now() < m.at + interval '1 hour 10 minutes' then 'early' else to_char((m.at + interval '1 hour 10 minutes') at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') end from (select 1) one left join lateral (select mark, at from deployment_resets order by id desc limit 1) m on true;
SQL
) || { echo "UNKNOWN: Dispatch's database could not be read or written, so the reset is not closed"; exit 1; }
case "$cutoff" in
none) echo 'NOT CLOSED: no reset is open, as the newest mark is not a start; run step 1 first'; exit 1 ;;
early) echo 'NOT CLOSED: an hour and ten minutes have not passed since step 1; run the credentials block again once they have, then this one'; exit 1 ;;
[0-9][0-9][0-9][0-9]-*Z) ;;
*) echo "UNKNOWN: Dispatch's database answered '$cutoff' for the reset's wait, so the reset is not closed"; exit 1 ;;
esac
signins() { psql "$BROKER_PSQL_URL" -v ON_ERROR_STOP=1 -At -F ' ' -c "select id, person from person_signins where revoked_at is null and created_at < '$cutoff' order by id"; }
logins() { psql "$BROKER_PSQL_URL" -v ON_ERROR_STOP=1 -At -F ' ' -c "select c.id, r.approver from launcher_credentials c join credential_requests r on r.id = c.record_id and r.kind = 'launcher_credential' where c.service is null and c.revoked_at is null and c.created_at < '$cutoff' order by c.id"; }
pages() { psql "$BROKER_PSQL_URL" -v ON_ERROR_STOP=1 -At -c "select distinct login from browser_sessions where expires_at > now() and created_at < '$cutoff' order by login"; }
left=$( { signins && logins && pages; } ) || { echo 'UNKNOWN: the broker database could not be read, so the reset is not closed'; exit 1; }
if [ -n "$left" ]; then printf 'STILL LIVE, so the reset is not closed: run the credentials block again, then this one\n%s\n' "$left"; exit 1; fi
ended=$(psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -qAt <<'SQL'
begin;
lock table deployment_resets in exclusive mode;
insert into deployment_resets (mark) select 'end' from (select mark, at from deployment_resets order by id desc limit 1) m where m.mark = 'start' and now() >= m.at + interval '1 hour 10 minutes' returning at;
commit;
SQL
) || { echo "UNKNOWN: Dispatch's database could not be read or written, so the reset is not closed"; exit 1; }
[ -n "$ended" ] || { echo 'NOT CLOSED: step 1 ran again since this close read the wait; run the credentials block again once its wait has passed, then this one'; exit 1; }
echo "the reset ended at $ended"
)
```

Only `the reset ended at <moment>` means done; for `STILL LIVE`, run the credentials block again
and then the close. A person who signed in at the broker during the wait is among what it lists,
and signs in again once Dispatch is back, as every person does. The database holds every end to
the same rule: `deployment_resets` takes an end only when the newest mark, by `id`, is a start an
hour and ten minutes old.

**Start Dispatch again**, once the close has printed `the reset ended at`. A sign-in by an account
other than the one the restored `people` row records is a replacement Dispatch records as any
other: it ends the recorded account's sessions, revokes its tokens, and owes the broker its
revocation, at the raised generation, which the broker applies. The directory's holder can sign
in and approve; an account the broker refuses that the directory did not name stays refused.

Only after step 5 has started Dispatch, each machine logs in again (`agent-secrets machine login`,
approved by its person) and each person signs in again on the approval page and with
`agent-secrets login`. Mint every agent a new personal token too, including the Legion daemon's
token in its `dispatch_token_file`.

`api.TestTheResetEndsEveryCredentialAndClearsOnlyTheHoldersRefusal` runs these blocks from this
page against Dispatch's database and a real broker's, over the histories a lone restore leaves:
a replacement the broker never applied, an address recorded with no account, and a take-back the
lost database still owed. `api.TestTheResetClosesOnlyAnHourAfterItsStartOnACleanRead` runs the
credentials block's two runs and the close, and `api.TestTheResetCloseWaitsForAStepOneStillOpen`
runs the close while step 1 is open. `store.TestTheResetEndsEveryCookieAndToken` runs the first
block alone, over a second restore from before a first reset.

## After the broker's database restored or made again alone

The other way round, when it is **the broker's database that was restored alone**, nothing above
finds it: every revocation Dispatch paid before the restore is settled at Dispatch, so the loop
claims none, and the owed gauges and the superseded counter read zero. The broker, though,
holds again every sign-in on a machine, machine login and approval-page session of the accounts
those revocations named, and no record refusing them: each reads the secrets the broker serves
without asking Dispatch. Dispatch refuses them itself from the replacing commit
(`brokerAccountReplaced`), so this is about the broker's own secrets.

So tell the broker again every revocation Dispatch settled at or after the moment the broker's
backup was taken, less an hour, and after the last reset's start where a reset ran: the block reads
that start from `deployment_resets`, the newest start by `id`, never by its moment, since the table
exists because a clock can be set back between two resets. The reset settled the revocations
restored Dispatch owed without paying them, each with that start's very moment as its `done_at`,
and its credentials and directory blocks stand in for them; told again, such a revocation names as
holder the account the restored `people` row records, so it would refuse again at the broker a
directory's holder the reset cleared, and end that holder's new broker sign-in at Dispatch's first
pass. The hour is a margin, and the comparison needs one: the moment comes from the backup's own
record, on another server's clock and often rounded to the minute or written as the backup's end
rather than its snapshot point, while `done_at` is Dispatch's database's `now()`. A revocation the
broker applied just after the snapshot, settled at Dispatch a few seconds before the moment as
written, would otherwise be left out, and that account keeps its credentials at the broker with no
gauge showing it. A revocation the restored broker still holds is told again too, and that is no
free call: the block records it again at the address's session generation now and names the
account `people` records now as the holder, so the broker applies it as a replacement at that
generation. It ends again whatever the replaced account holds, which a restored broker may have
brought back; it raises the generation the broker recorded for the address to Dispatch's, where it
was lower; and, since a revoke names the holder, it lifts any refusal of the address's account now.
So the margin only has to cover the skew, and an hour covers a rounded moment too; widen it where
the two clocks are further apart than that, or where the backup's moment is less exact.

A broker database **made again**, rather than restored, has no such moment: it has forgotten every
refusal and every replacement. Give the block `-infinity`, so it records again every revocation
Dispatch has settled since the last reset's start, or ever where none ran, and the loop tells the
broker each. Then read whether the reset's blocks run again (below), with `-infinity` for the
backup's moment: they do whenever a reset ran before this broker was made again.

The block records each again at its next generation, naming the account the address records now
as its holder and the address's session generation, exactly as the record-again block of
[Signing people in](/legion/dispatch/running-dispatch/#signing-people-in) does for one row, and
leaves alone a row whose account holds the address again (the broker must not be
told that the holder was replaced) and one at an address that records no account. It locks the
session row of every address it will write first, as the single-row blocks do, so a sign-out at
one of those addresses waits for it rather than moving a generation between this statement's read
of it and the database's own check, which would refuse the whole statement with 23514:

```sh
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -v backup='<the moment the broker''s backup was taken>' <<'SQL'
begin;
select u.generation from user_sessions u where u.login in (select r.email from broker_revocations r where r.done_at >= :'backup'::timestamptz - interval '1 hour' and r.done_at > coalesce((select d.at from deployment_resets d where d.mark = 'start' order by d.id desc limit 1), '-infinity'::timestamptz)) order by u.login for update;
update broker_revocations set generation = generation + 1, holder_subject = (select sub from people where people.email = broker_revocations.email), session_generation = (select generation from user_sessions where user_sessions.login = broker_revocations.email), done_at = null, recorded_at = clock_timestamp() where done_at >= :'backup'::timestamptz - interval '1 hour' and done_at > coalesce((select d.at from deployment_resets d where d.mark = 'start' order by d.id desc limit 1), '-infinity'::timestamptz) and (select sub from people where people.email = broker_revocations.email) is not null and (select sub from people where people.email = broker_revocations.email) <> broker_revocations.subject;
commit;
SQL
```

The loop then pays each, and the broker ends those accounts' credentials and refuses them again
(`api.TestABrokerRestoredAloneIsRecoveredByTellingItAgainWhatWasSettledSince` runs this block
against a real broker restored behind Dispatch, a take-back among its addresses, and
`api.TestABrokerRestoredBeforeTheResetLeavesTheDirectorysHolderCleared` runs it after a reset).
It recovers nothing the broker's backup already holds. A revocation still owed when the broker
comes back, one Dispatch recorded while it was down, is the loop's own and is paid as any other. A
revocation an operator settled by hand with `clear-broker-revocation` after the last reset's start
is recorded again too, which is right — the broker was never told — unless the account it names
holds the address again, which the statement leaves out; one settled before that start is the
reset's, whose blocks stand in for it.

A broker backup can also predate the end of a reset that ended every person credential at the
broker, and bring back what the reset ended, which no revocation names. After the record-again
block, ask Dispatch's database whether it may, with the backup's moment (`-infinity` for a broker
made again):

```sh
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -At -v backup='<the moment the broker''s backup was taken>' <<'SQL'
select case when not exists (select 1 from deployment_resets) then 'no reset ran' when :'backup'::timestamptz - interval '1 hour' < coalesce((select e.at from deployment_resets e where e.mark = 'end' and e.id > (select s.id from deployment_resets s where s.mark = 'start' order by s.id desc limit 1) order by e.id desc limit 1), 'infinity'::timestamptz) then 'run the reset''s blocks again' else 'the backup holds the last reset' end;
SQL
```

For `run the reset's blocks again`, run **End every person's credential at the broker** and
**Clear a refusal only for the directory's holder** again, after the record-again block and before
starting Dispatch. The first ends every restored person credential; the second restores the
directory's decision about any refusal the backup brought back. The read compares the backup with
the last reset's end, the end that follows its start by `id`, and not with its start, since the
reset's broker steps run until its close, an hour or more after its start; a reset that was never
closed counts as one still running, whatever the backup's moment. It takes the same hour's margin
as record-again, since the backup's moment can be written that much after its snapshot.
`api.TestABrokerBackupWrittenWithinTheHourAfterTheResetRepeatsItsBlocks`,
`api.TestABrokerBackupTakenDuringTheResetRepeatsItsBlocks` and
`api.TestABrokerBackupAfterAnUnclosedResetRepeatsItsBlocks` run it over a backup on each side and
an unclosed reset, and `api.TestTheResetsBoundsFollowTheMarksOrderNotTheirClock` over a clock set
back between two resets. Where the last reset is still open, Dispatch is still stopped: finish that
reset, steps 4 and 5, rather than starting Dispatch at the end of this recovery.

A broker backup can also predate a move from the Cognito pool to a domain, and bring back the
pool-era machine logins that move's sweep ended (below), which no revocation names either. If the
deployment ever moved from the pool to a domain, run the sweep block of that move again after the
record-again block and before starting Dispatch. It ends every machine login that records no
approving account: the pool's, and any that a Dispatch older than this change approved, since its
approve named no account either. Each such machine logs in again, approved by its person
(`api.TestABrokerRestoredFromBeforeThePoolSweepIsSweptAgain`).

A refusal an operator lifted by hand with the broker clear above comes back with the restore too,
and nothing records it: the clear writes no row, and no revocation of Dispatch names it. Each such
refusal is of the account Dispatch records for its address now, so find them by comparing each
address's `people.sub` with the accounts the broker refuses there. The block prints
`REFUSES ITS HOLDER <address> <sub>` for each, or that there is none, and `UNKNOWN` when a database
cannot be read:

```sh
(
set -o pipefail
recorded=$(psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -At -F ' ' -c "select email, sub from people where sub is not null order by 1, 2") || { echo "UNKNOWN: Dispatch's database could not be read, so no refusal was compared"; exit 1; }
refused=$(psql "$BROKER_PSQL_URL" -v ON_ERROR_STOP=1 -At -F ' ' -c "select person, subject from replaced_person_subjects where issuer = 'https://accounts.google.com' order by 1, 2") || { echo 'UNKNOWN: the broker database could not be read, so no refusal was compared'; exit 1; }
held=$(comm -12 <(printf '%s\n' "$recorded" | LC_ALL=C sort) <(printf '%s\n' "$refused" | LC_ALL=C sort) | sed '/^$/d')
if [ -z "$held" ]; then echo 'the broker refuses no account Dispatch records'; exit 0; fi
while read -r person sub; do echo "REFUSES ITS HOLDER $person $sub"; done <<< "$held"
)
```

For each `REFUSES ITS HOLDER` line, check the address in the Workspace directory as the directory
block does, then clear that refusal with the broker clear above, before starting Dispatch
(`api.TestTheRefusalsOfAnAddressesRecordedAccountAreFound`).

A revocation an earlier head of this branch recorded holds no session generation, and the broker
refuses a revoke without one (`400 SESSION_GENERATION_REQUIRED`), so the loop's ERROR line for it
repeats each retry. It holds back no other revocation of its address; record it again with the
record-again block, which gives it the address's generation.

## Moving between the Cognito pool and a domain

Moving a deployment between the Cognito pool and a domain, either way, ends every browser session
and clears the refresh tokens at the cutover, and keeps the recorded Google accounts: the database
refuses taking a recorded account off `people.sub` (migration `20261118041916`'s
`people_binding_columns`, which replaced `20261117175133`'s trigger). Run each statement on its
own, so a refusal of one does not undo another.

From the Cognito pool to a domain: first end sign-in through the pool and the refresh tokens it
issued, with two calls in this order, since no one Cognito call is documented to do both. Deleting
Dispatch's app client stops sign-in through it: "After you delete an app client, users can no
longer sign in to the associated application" (`DeleteUserPoolClient`,
<https://docs.aws.amazon.com/cognito-user-identity-pools/latest/APIReference/API_DeleteUserPoolClient.html>),
but that page says nothing of the refresh tokens already issued. Signing each user out ends those:
"Amazon Cognito no longer accepts a signed-out user's refresh tokens in refresh requests"
(`AdminUserGlobalSignOut`,
<https://docs.aws.amazon.com/cognito-user-identity-pools/latest/APIReference/API_AdminUserGlobalSignOut.html>),
but it leaves sign-in working, so it comes second. It ends every token the pool issued the user,
for every app client of the pool, not Dispatch's alone. The pool's id is the last part of
`DISPATCH_SIGNIN_ISSUER`, and a later return to the pool needs a new app client. Delete the app
client first; `aws` prints the error of a call that fails and exits non-zero:

```sh
aws cognito-idp delete-user-pool-client --user-pool-id '<the pool id, the last part of DISPATCH_SIGNIN_ISSUER>' --client-id '<DISPATCH_SIGNIN_CLIENT_ID>'
```

Only once that call has succeeded, sign every user of the pool out. A second sign-out of a user
does no harm, so the block can run again until it is done:

```sh
(
pool='<the pool id, the last part of DISPATCH_SIGNIN_ISSUER>'
users=$(aws cognito-idp list-users --user-pool-id "$pool" --query 'Users[].Username' --output text) || { echo 'UNKNOWN: the pool could not be listed, so no user was signed out'; exit 1; }
failed=0
while read -r user; do
  [ -n "$user" ] || continue
  aws cognito-idp admin-user-global-sign-out --user-pool-id "$pool" --username "$user" </dev/null || { echo "NOT SIGNED OUT $user"; failed=1; }
done <<< "$(printf '%s\n' "$users" | tr '\t' '\n')"
[ "$failed" -eq 0 ] || exit 1
echo 'every user of the pool is signed out'
)
```

Only `every user of the pool is signed out` means done. `UNKNOWN` signs no one out, and
`NOT SIGNED OUT` names each user whose call failed, after the error `aws` printed for it: fix what
the error names and run the block again.

Then stop every Dispatch process still running in pool mode, and only once the last has exited run
the statements below and end the pool-era machine logins (further down), all before any Dispatch
process starts with the new settings: a pool-mode process that serves after them can sign someone
in through the pool at the new session generation, mint a personal token that records no account,
or relay an approve that names none, and nothing here would end what it made. The statements clear
the refresh tokens still stored in plain text, end every browser session, and revoke every personal
token that records no Google account:

```sql
update people set refresh_token = null, confirmed_at = null where refresh_token not like 'v1:%';
update user_sessions set generation = generation + 1;
update agent_tokens set revoked_at = now() where subject is null and revoked_at is null;
```

Domain mode's own retirement would send a plain-text pool token to Google's revocation endpoint,
which cannot revoke a token Google never issued, and log an ERROR every boot until the row is
cleared or its person signs in again. A person's own next sign-in under the domain overwrites their
sealed pool token anyway, but someone who never signs in again keeps theirs in `people`
indefinitely, still redeemable at the pool until it is revoked there. Ending the sessions ends every
pool-era cookie, whatever account the address's row records: an address that records no account
has its sessions ended again by its first Google sign-in, and one that records an account from an
earlier time in domain mode keeps it, with the time it was first recorded, so the same Google
account signing in again is no replacement.

That kept account is why the third statement is there. A personal token minted under the pool
records no account, and it was minted after that recording, so without the statement Dispatch
would admit it on the return and bind it to the recorded account: its approves would be relayed
naming that account, though whoever the pool signed in as the address minted it. The statement
revokes every personal token that records no account, those from before Dispatch recorded
accounts on tokens included, which is what the first recording at the return did when the switch
cleared `people.sub` (Dispatch then refused every token recording no account made before it); each
person mints a new one once signed in. A token that records an account is untouched: Dispatch
refuses one that records another account than the address's.

The secrets broker serves a machine login without asking Dispatch, and a machine login approved
under the pool records no approving account, since a pool-era approve names none. No statement
here ends it, and neither did the step that cleared `people.sub`: it goes on reading the secrets
its person may read. The broker has no call that ends every such login at once, so list them on
the broker's database and revoke each as its approver, with the call Dispatch's machine-login page
makes (**Revoke** on that page, signed in as the approver, ends them one at a time). The call takes
the broker's URL, which Dispatch's `DISPATCH_AGENT_SECRETS_URL` holds, and a token of Dispatch's
own ServiceAccount for the broker's audience (`BROKER_K8S_OIDC_AUDIENCE`), the kind of token
Dispatch reads from `DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE`. Mint one for the run: the broker
refuses a token that lives longer than an hour (`ACCOUNT_TOKEN_TOO_LONG_LIVED`) and one that has
expired (`ACCOUNT_TOKEN_EXPIRED`), so a list that takes longer needs another. The block runs in a
subshell that keeps the token off every command line: it is written into a header file only the
operator can read, which curl reads with `-H @file`, and the file is removed when the subshell
ends, whether or not it fails. Each call that fails prints `NOT REVOKED <id>` after the broker's
answer, and the last step reads the list again:

```sh
(
umask 077
header=$(mktemp) || exit 1
trap 'rm -f "$header"' EXIT
broker_url='<the URL in DISPATCH_AGENT_SECRETS_URL>'
token=$(kubectl -n '<the namespace Dispatch runs in>' create token '<the ServiceAccount Dispatch runs as>' --audience '<the audience in BROKER_K8S_OIDC_AUDIENCE>' --duration 1h) || { echo 'no token was minted'; exit 1; }
printf 'Authorization: Bearer %s\n' "$token" >"$header"
unended() { psql "$BROKER_PSQL_URL" -v ON_ERROR_STOP=1 -At -F ' ' -c "select c.id, r.approver from launcher_credentials c join credential_requests r on r.id = c.record_id and r.kind = 'launcher_credential' where c.service is null and c.revoked_at is null and c.approver_subject is null"; }
unended | while read -r id approver; do
  curl -sS --fail-with-body -w '\n' -X POST -H @"$header" -d "{\"approver\":\"$approver\"}" "$broker_url/v1/launcher-credentials/$id/revoke-by-approver" || echo "NOT REVOKED $id"
done
if left=$(unended); then
  if [ -z "$left" ]; then echo 'no machine login records no approving account'; else printf 'STILL LIVE:\n%s\n' "$left"; false; fi
else
  echo 'the broker database could not be read, so what is left is unknown'; false
fi
)
```

Only the first answer of the last step means done. `STILL LIVE` lists, by id and approver, each
login no call ended, and the `NOT REVOKED` lines above give the broker's answer for it: for
`ACCOUNT_TOKEN_EXPIRED` mint another token, for any other answer fix what it names, then run the
whole block again until it answers `no machine login records no approving account`. An answer
that the database could not be read says nothing about the logins: run it again once
`$BROKER_DATABASE_URL` connects.

That ends every person's machine login that records no approving account, those from before the
broker recorded approving accounts (its migration 0041) included, with every session each
enrolled; each machine logs in again (`agent-secrets machine login`) and its person approves the
new code. A service's login, the Legion daemon's, is not among them. Until it runs, whoever the
pool signed in as an address during the pool era keeps the machines that person logged in reading
the address's secrets.

From a domain back to the Cognito pool: revoke the refresh tokens at Google (remove the client's
access). Then stop every Dispatch process still serving in domain mode, and only once the last has
exited clear the tokens in Dispatch's database and end every browser session, before any Dispatch
process starts with the new settings: a domain-mode process that serves after the statements can
issue a cookie at the new session generation, which pool mode would then accept:

```sql
update people set refresh_token = null, confirmed_at = null;
update user_sessions set generation = generation + 1;
```

and start Dispatch with the new settings. The group sign-in path never records or compares an
account, so nothing would end the domain sessions on its own: skipping the session step leaves a
cookie issued before the switch valid, since the first sign-in under the pool records a fresh
confirmation and that cookie's generation still matches. The recorded accounts stay, unread under
the pool, and a later return to domain mode ends the pool-era sessions, personal tokens and
machine logins with the steps above. Everyone then signs in again under the new issuer; a refresh
token left from the old one would be refused at the first hourly confirmation, and is still
redeemable at the old issuer until it is revoked there.
`store.TestTheModeSwitchRunsOnTheGuardedSchema` runs the five statements, in both directions,
against the guarded schema.
