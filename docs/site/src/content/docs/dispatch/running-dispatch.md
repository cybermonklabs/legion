---
title: Running Dispatch
description: What the Dispatch server needs, how to run it, private projects, its health check, running more than one copy, and its migrations.
sidebar:
  order: 8
---

Dispatch is one Go program. It serves the web app, the HTTP API, live documents, and Google
sign-in. Its source is `packages/envoy/cmd/dispatch`, and the web app it serves is built from
`packages/dispatch`. The container image `ghcr.io/cybermonklabs/legion/envoy` carries it as
`/usr/local/bin/envoy-dispatch`.

Pin the image by digest. Each `ghcr.io/cybermonklabs/legion/envoy:<commit>` image that
`.github/workflows/release-envoy-listener.yaml` publishes carries a GitHub artifact attestation
from that workflow (images published before attestation began carry none), and a tag does not say
which run pushed it (a run dispatched by hand from any branch pushes one too), so accept a digest
only when a run on `main` built it:

```bash
gh attestation verify oci://ghcr.io/cybermonklabs/legion/envoy@sha256:… --repo cybermonklabs/legion \
  --cert-identity https://github.com/cybermonklabs/legion/.github/workflows/release-envoy-listener.yaml@refs/heads/main \
  --source-ref refs/heads/main --deny-self-hosted-runners
```

`latest` is a separate image index with its own digest and no attestation. To start from it, read
the commit it names and pin that commit's image instead:

```bash
docker buildx imagetools inspect --raw ghcr.io/cybermonklabs/legion/envoy:latest \
  | jq -r '.annotations["org.opencontainers.image.revision"]'
docker buildx imagetools inspect ghcr.io/cybermonklabs/legion/envoy:<that commit>   # its Digest is the one to pin
```

## What it needs

| Need | Setting | Notes |
| --- | --- | --- |
| Postgres | `DATABASE_URL` | Required. Dispatch keeps everything here and applies its own migrations at start. Leave `pool_max_conns` out of the URL; Dispatch refuses it. |
| A shared agent token | `DISPATCH_AGENT_TOKEN` | Required to start. A fallback credential for agents, which sees only the public projects; agents normally use personal tokens made in Settings. To change it without locking agents out, set the new value followed by the old one, separated by a space, until every agent has moved, then the new one alone. |
| Google sign-in | `DISPATCH_SIGNIN_ISSUER`, `DISPATCH_SIGNIN_CLIENT_ID`, `DISPATCH_SIGNIN_CLIENT_SECRET`, and `DISPATCH_SIGNIN_GROUP` or `DISPATCH_SIGNIN_DOMAIN` | The OpenID Connect issuer people sign in through, Dispatch's client there and that client's secret, and exactly one of: the group a person must be in, through a Cognito sign-in pool; or the Google Workspace domain whose people may sign in, with Google itself as the issuer. Required for the default cookie sign-in. `DISPATCH_SIGNIN_OUTSIDE=1` lets the people a project names sign in from outside the domain with a Google account, or `DISPATCH_SIGNIN_PASSKEY=1` with a passkey, and `DISPATCH_SIGNIN_WORKSPACE_DOMAINS` names the organization's other domains, whose addresses only its own accounts sign in with. [Signing people in](#signing-people-in) covers all of it. |
| The browser address | `DISPATCH_SERVER_URL` | The exact address people type, such as `https://dispatch.internal.example`. Dispatch's client at the issuer lists this address followed by `/auth/callback` as a callback URL. `DISPATCH_OUTSIDE_ORIGIN` is a second address, for people from outside ([People from outside the domain](#people-from-outside-the-domain)). |
| The GitHub App | `DISPATCH_APP_CLIENT_ID`, `DISPATCH_APP_PEM_B64` | `DISPATCH_APP_PEM_B64` holds the App's private key as the PEM GitHub gives or the base64 of that PEM; a key that does not parse stops boot. Without the App the server starts, but the web app's GitHub reads answer `503` and architecture sources cannot be saved. |
| A stable session key | `DISPATCH_SIGNING_KEY` | Signs sign-in cookies and seals the issuer's refresh tokens Dispatch stores. It holds one key, or several separated by spaces: the first signs and seals, and every one is accepted, which is how the key is changed without signing anyone out ([Changing the session key](#changing-the-session-key)). When unset, Dispatch creates one under `~/.local/share/dispatch`; keep that directory on a volume, or every restart signs everyone out. Give every server the same keys when you run more than one: a server whose keys another running server does not share refuses to start ([Running more than one copy](#running-more-than-one-copy)). |
| An Envoy listener | `ENVOY_URL` | Where the Agents page finds live agent sessions. Defaults to `http://127.0.0.1:9020`. |
| NATS | `NATS_URLS`, `NATS_NKEY_SEED_FILE` | Where Dispatch publishes its events for agents. The Agents page's live view also needs it. Dispatch refuses a NATS server on another machine unless `ENVOY_ALLOW_REMOTE_NATS=1`. A NATS that is down, or refuses Dispatch's credential, when Dispatch starts does not stop it: Dispatch serves, connects once NATS answers, and publishes what was written meanwhile. Set `DISPATCH_NATS_DISABLED=1` to run without it. |
| An address to listen on | `DISPATCH_LISTEN_HOST`, `DISPATCH_PORT` | Port `8766` by default. |
| Meaning search | `DISPATCH_EMBEDDER`, and for Azure the `DISPATCH_AZURE_*` settings | Search by meaning embeds text with Cohere Embed v4 on AWS Bedrock (the default, with the AWS SDK's region and credentials) or on Azure AI Foundry (`DISPATCH_EMBEDDER=azure`: the endpoint, the deployment's tokens-per-minute quota, and either the deployment's key or a keyless sign-in through a federated token). Without it, search answers by words alone and says so. The server embeds everything it already holds by itself, at start and every ten minutes; `GET /api/v1/search/coverage` shows how far it has got. |

These are the settings most deployments need, not all of them. The full list, generated from the
server, is in [Configuration](/legion/dispatch/reference/configuration/).
When neither `NATS_URLS` nor `DISPATCH_NATS_DISABLED=1` is set, Dispatch reads the NATS address
from `envoy.natsUrls` in `${XDG_CONFIG_HOME:-~/.config}/legion/envoy.json`, the file agents use,
whatever that file's `envoy.enabled` says, so set one of them explicitly.
A Dispatch that reaches NATS only through a SOCKS5 proxy, such as a tailnet sidecar's, sets
`ENVOY_NATS_SOCKS5_PROXY` to the proxy's `host:port` (for example `127.0.0.1:1055`) with
`ENVOY_ALLOW_REMOTE_NATS=1`, and every NATS connection then goes through the proxy, which is
handed each server's host name unresolved.
Dispatch's server creates and updates the `ENVOY_NOTIFICATIONS` stream as the Envoy listener
does, so give it the listener's `ENVOY_NATS_REPLICAS` and `ENVOY_STREAM_MAX_BYTES`
([Running the listener](/legion/dispatch/envoy/#running-the-listener)): it creates the stream and
its webhook redelivery bucket with that many copies, raises one it finds with fewer, raises a
smaller byte limit, or none, to its own, never lowers either, and with no byte limit of its own
keeps the one the stream has. The stream holds the most copies and the largest limit any Envoy
program of the deployment carries, so a clustered deployment sets both alike in every listener and
in Dispatch's server. Like the listener, it reads the stream again every 30 seconds and raises
copies and a limit a start with other settings lowered
([Keeping the stream](/legion/dispatch/envoy/#keeping-the-stream)).

People sign in through a Cognito sign-in pool or with Google directly by default, and Dispatch
names each person by their lowercase email. `DISPATCH_IDENTITY=iap` names people from the
assertion Google Cloud's Identity-Aware Proxy signs instead
([Behind Identity-Aware Proxy](#behind-identity-aware-proxy)).
`DISPATCH_IDENTITY=header:<Header-Name>` names people by a request header; it is for
tests and local harnesses only, needs `DISPATCH_IDENTITY_HEADER_TRUSTED=1`, and refuses every
`DISPATCH_SIGNIN_*` setting. Serve Dispatch over HTTPS. `DISPATCH_INSECURE_COOKIE=1` is only for
trying it over plain HTTP on your own machine.

## Signing people in

Dispatch signs people in with OpenID Connect's authorization code flow, in one of two ways, chosen
by which of `DISPATCH_SIGNIN_GROUP` and `DISPATCH_SIGNIN_DOMAIN` is set beside the issuer, the client
ID and the client secret. Setting both, or some of the settings without the others, refuses the
start, naming what is wrong.

- **Through a Cognito sign-in pool**, federating whatever identity provider the deployment
  chooses: `DISPATCH_SIGNIN_ISSUER` is
  the pool's issuer, the client is Dispatch's confidential app client in the pool, and
  `DISPATCH_SIGNIN_GROUP` names the pool group (`cognito:groups`) a person must be in. Dispatch
  names the person by the email in their pool username, never by the `email` claim, which a
  person can write.
- **With Google directly**, for one Workspace domain: `DISPATCH_SIGNIN_ISSUER` is
  `https://accounts.google.com`, the client is a Google OAuth client, and
  `DISPATCH_SIGNIN_DOMAIN` is the domain, a lowercase name such as `example.com`. A domain under
  any other issuer, and a group under Google's, refuse the start: only Google's ID tokens carry
  `hd`, and they carry no group. A person signs in when their ID token's `hd` (the account's
  Workspace domain) is the domain and Google has verified their `email`; Dispatch names them by
  that email, lowercased. Anyone else reaches a page saying why: not in the domain, email not
  verified, or no email. No AWS account is involved.

  Set `DISPATCH_SIGNIN_DOMAIN` to the organization's **primary** domain: Google puts the primary
  domain in `hd` for every account of the organization, whichever domain its address is on. A
  secondary domain in the setting admits nobody, and a member whose address is on a secondary
  domain signs in, named by that address.

The Google client to create, in the Google Cloud console of the organization whose Workspace it
is, is a **web application** OAuth client with `<DISPATCH_SERVER_URL>/auth/callback` as an
authorized redirect URI. Set its OAuth consent screen's user type to **Internal**, so that only the
organization's own accounts can complete the sign-in at Google, unless people from outside sign in
too (below). Google accepts an `https` redirect
whose host ends in a public suffix, or plain `http` to a loopback address such as
`http://127.0.0.1:8766/auth/callback` for a server on your own machine. Its client ID and secret
are `DISPATCH_SIGNIN_CLIENT_ID` and `DISPATCH_SIGNIN_CLIENT_SECRET`. Dispatch asks Google for
offline access, with consent on every sign-in, so each sign-in returns the refresh token Dispatch
confirms the person with, and names the domain to Google's account chooser as a hint; the check
that admits a person is Dispatch's own.

Either way, Dispatch keeps the issuer's refresh token, sealed, and confirms the person with it at
least hourly: the first request after the hour refreshes the sign-in and applies the same rule to
the refreshed ID token. A person the rule no longer admits loses every browser session, and so
does anyone whose refresh token the issuer refuses as expired or revoked. A refresh the issuer
does not answer, as in an outage at the issuer, signs nobody out: that request is refused with
`503 SIGNIN_UNAVAILABLE`, and the person's first request after the issuer answers again refreshes
and goes through. Logout ends Dispatch's session only, not the issuer's.

Dispatch does not need the issuer to start. It reads the issuer's settings in the background,
retrying until the issuer answers, and until then a sign-in answers `503 SIGNIN_UNAVAILABLE` while
agents, and people whose hour has not lapsed, are served. A sign-in started on one server completes
on whichever server the issuer sends the browser back to, since what it needs travels in a signed
cookie, as long as every server has the same `DISPATCH_SERVER_URL`.

A person is their email address, so an address that passes from one account to another is one
person to Dispatch. With Google directly, Dispatch also records the Google account (its `sub`,
which Google never reuses) that signed in to each address: when another account signs in to the
same address, as when a Workspace admin gives a deleted account's address to a new one, Dispatch
ends every session of the address and revokes every agent token made under it before it signs
the new account in, so nothing the old account signed in or made acts as the new one; the new
account makes its own tokens in Settings. With a secrets broker configured
(`DISPATCH_AGENT_SECRETS_URL`), whatever member kind the old account's row held, one of your
people or an account from outside, Dispatch also tells the broker which account it replaced, and the broker
ends every machine login the old account approved for its own machines, every sign-in it made
on a machine with `agent-secrets login` and every session it holds on the broker's own approval
page, whenever it made them, with the tokens issued under them,
and refuses it any new one, a sign-in with a Google token it got before the address moved
included. The new account's machines are left alone, however soon after its sign-in they log in.
If the old account gets the address back while its revocation is still owed, Dispatch drops that
revocation, so nothing the old account makes after taking the address back is ended, unless a call
to the broker for it was already under way, and revokes only the account it took the address back
from.
Dispatch tries again every 30 seconds until the broker confirms. A broker token names the Google
account the broker recorded for it, the account that signed in on the machine or approved the
machine login, and Dispatch holds each token to that account: the new account's own tokens act
throughout, the old account's are refused from the commit of the new account's sign-in, and a
token of an account Dispatch does not record for its address acts only once that account has
signed in to Dispatch. A token from a sign-in or machine login made before the broker recorded
accounts names none and might be either account's, so Dispatch holds it to when the broker made
that sign-in or login: made before the new account signed in to Dispatch, or less than five
minutes after (the broker's clock and Dispatch's are two, and a skew refuses), it is refused for
good; made later, it acts. A personal token that names no account is held to its own creation
time the same way. Nothing waits on the broker's revocation for any of this. Deploy the broker
that serves this before Dispatch, and set `DISPATCH_AGENT_SECRETS_URL` on every Dispatch process:
a process without it records no revocation, so a replacement it serves is never revoked at the
broker, where the old account's machine logins would go on reading secrets. A revocation the
broker never confirms shows on `/metrics`
(`envoy_dispatch_broker_revocation_oldest_owed_seconds` past a few minutes), one it answered
`superseded` on `envoy_dispatch_broker_revocations_superseded_total` (below), and
`envoy-dispatch clear-broker-revocation <email>` lists what is owed and, given the names it lists,
settles those without the broker, so revoke the old account's machine logins at the broker first.
A revocation settled without the broker, by that command or by a statement written by hand, tells
the broker nothing, and the old account's credentials go on reading secrets there. It also leaves
the broker refusing the account the address came back to, when the broker had recorded that
account replaced: a revocation names the account that holds the address, and the broker stops
refusing that account only when a revocation naming it reaches it. To have the server's loop pay
it after all, which recovers both, record it again, naming the account the address records now
as its holder, with the address as Dispatch holds it, the issuer and the replaced account's `sub`:

```sh
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -v email='<address>' -v issuer='https://accounts.google.com' -v subject='<the replaced sub>' <<'SQL'
begin;
select generation from user_sessions where login = :'email' for update;
update broker_revocations set generation = generation + 1, holder_subject = (select sub from people where people.email = broker_revocations.email), session_generation = (select generation from user_sessions where user_sessions.login = broker_revocations.email), done_at = null, recorded_at = clock_timestamp() where email = :'email' and issuer = :'issuer' and subject = :'subject';
commit;
SQL
```

(`store.TestARevocationSettledByItselfIsPaidOnceRecordedAgain` runs this block.) The database
holds a revocation to its writers and refuses every other write, its deletion included (migration
`20261118152422`): a recording, like this one, names its holder, which must be the account the
address records, and its session generation, which must be the address's own; a pass's claim
stamps when it was tried; and only a settlement, like the command's, leaves one unpaid. The block
locks the address's `user_sessions` row first, so a sign-out at the same moment waits for it,
rather than moving the generation between the statement's read of it and the database's own
check, which would refuse the statement.

A machine login's approve or deny made through Dispatch reaches the broker only while its sender
still holds the address, checked again once the request's body has arrived, however long the
sender held the body back, so the old account can neither approve a machine login under the address
nor deny one of the new account's.
The hourly confirmation ends the sessions should the
refreshed sign-in name another account. Through a Cognito pool there is no such record: the old
holder of a reassigned address keeps the sessions they already have until their cookie expires,
30 days after it was issued, unless the pool user is removed from `DISPATCH_SIGNIN_GROUP` or
disabled, and keeps their agent tokens until someone revokes them.

The broker refuses an account it was told no longer holds an address until a later revocation
names that account as the holder, which only a sign-in to Dispatch that replaces another account
sends. A statement written by hand that sets `people.sub` back to such an account, rather than the
account signing in, skips the revocation that sign-in would have recorded, and that leaves a
credential acting. Say A held the address and B took it: writing A back records no revocation of
B, so B's machine logins and sign-ins stay live at the broker, which serves them secrets without
asking Dispatch, and the broker goes on refusing A. To recover, do on Dispatch's database what that
sign-in would have done, with the address as Dispatch holds it, the issuer and the `sub` the
statement overwrote: settle every revocation still owed for the account the address records now
(A), as a sign-in that takes an address back does, so the loop never pays one that would end what
A holds; then record the revocation of the account the statement replaced (B), naming A as its
holder, exactly as a sign-in records one. The block locks the address's `user_sessions` row
first, as the one above does:

```sh
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -v email='<address>' -v issuer='https://accounts.google.com' -v subject='<the sub the statement overwrote>' <<'SQL'
begin;
select generation from user_sessions where login = :'email' for update;
update broker_revocations set done_at = now() where email = :'email' and issuer = :'issuer' and subject = (select sub from people where people.email = :'email') and done_at is null;
insert into broker_revocations (email, issuer, subject, holder_subject, recorded_at, session_generation) values (:'email', :'issuer', :'subject', (select sub from people where people.email = :'email'), clock_timestamp(), (select generation from user_sessions where user_sessions.login = :'email')) on conflict (email, issuer, subject) do update set generation = broker_revocations.generation + 1, holder_subject = excluded.holder_subject, recorded_at = excluded.recorded_at, session_generation = excluded.session_generation, done_at = null;
commit;
SQL
```

The server's loop then pays it: the broker ends B's credentials and, since a revoke naming A as the
holder deletes its record of A, stops refusing A
(`store.TestAnAccountWrittenBackByItselfIsRecoveredByRecordingTheSkippedRevocation` runs this
block). A statement that records an account where the address recorded none overwrote no
`sub`, and this recovery does not cover it: the first sign-in would also have revoked the address's
personal tokens that recorded another account or none, and recorded a revocation of each other
account, which nothing then records.

Every block below that reads the broker's database reaches it with `psql`, which takes libpq's
URL, while `BROKER_DATABASE_URL` is the broker's own, pgx's: a deployment sets `pool_max_conns`
on it to size the broker's pool, and libpq refuses a URI parameter it does not know
(`invalid URI query parameter`), so `psql "$BROKER_DATABASE_URL"` reads nothing and the reset
below would print `UNKNOWN: the broker database could not be read` for a reason that has nothing
to do with the broker. So make a libpq URL once, from the broker's own, and use it where a block
names `$BROKER_PSQL_URL`:

```sh
BROKER_PSQL_URL=$(printf '%s' "$BROKER_DATABASE_URL" | sed -E 's/(\?|&)pool_[a-z_]+=[^&]*/\1/g; s/&&+/\&/g; s/\?&/?/; s/[?&]$//')
export BROKER_PSQL_URL
```

Those `pool_` parameters are the only ones the broker's deployment adds that libpq does not take;
a URL without them is left as it is.

A refusal the operator chooses to lift by itself, of an account the address records again, is
cleared on the broker's database instead; it lifts that refusal and ends nothing, so it is no
recovery from the write above. First check on Dispatch's database that the address records the
account (`select sub from people where email = '<address>'`), then delete the broker's record of
it on the broker's database, with the address in lowercase, the issuer as the broker spells it
(`https://accounts.google.com`) and the account's `sub`. The broker refuses a delete from that
table unless the deleting transaction first names the one row it deletes, by its own transaction
id, in `broker.replaced_subject_delete`, as a JSON array of the transaction id, the address, the
issuer and the `sub`, so the clear is one transaction that sets that marker for the row the
operator chose to clear and then deletes it; a delete without the marker is refused:

```sh
psql "$BROKER_PSQL_URL" -v ON_ERROR_STOP=1 -v person='<address>' -v issuer='https://accounts.google.com' -v subject='<sub>' <<'SQL'
begin;
select set_config('broker.replaced_subject_delete', jsonb_build_array(pg_current_xact_id()::text, :'person', :'issuer', :'subject')::text, true);
delete from replaced_person_subjects where person = :'person' and issuer = :'issuer' and subject = :'subject';
commit;
SQL
```

The account's next sign-in trade at the broker then goes through
(`store.TestAnOperatorsClearLetsAReplacedAccountTradeAgain` runs this block).

Reset or restore Dispatch's database and the broker's together, never one alone, or replaced
accounts keep live broker credentials: to reset, drop both, create both empty, and let each server
migrate its own; to restore, restore both from backups taken at one moment. A Dispatch database
made again starts every address's session generation over, and one restored from a backup to an
earlier point holds the lower generations it held then, while a broker database kept holds the
generations its revokes recorded since. The broker then refuses every approve at such an address
`403 APPROVER_REPLACED`, as checked before a replacement, and answers every revocation there
`superseded`, ending nothing. A broker database made again beside a Dispatch database kept has
forgotten every refusal and every replacement Dispatch already paid, and is recovered like a
broker database restored alone, with `-infinity` for the backup's moment (below).

`envoy_dispatch_broker_revocations_superseded_total` counts each answer `superseded` the broker
gives a revocation still owed, which Dispatch leaves owed, so the owed gauges above show it too,
and logs at ERROR with the address, the replaced account's `sub`, both generations the broker
compared and the address's generation at Dispatch now; the loop tries it again after its retry
window. Alert on the counter being above zero (`envoy_dispatch_broker_revocations_superseded_total
> 0`), not on its `rate()`: it writes no sample until its first count, and any count is a broken
pair. While the two databases agree, Dispatch's loop never hears that answer for a row still owed.
It means the broker applied a later replacement at the address than the one the row names: a
Dispatch database recreated or restored alone, or a generation written by hand at either. Outside
a reset, run the reset below. After one, the reset has already ended every credential the row
would end and settled every restored debt, so settle a later `superseded` row with
`envoy-dispatch clear-broker-revocation '<address>' '<subject>:<generation>'`, naming it as the
command's list prints it.

### After a Dispatch database restored or made again alone

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
([People from outside the domain](#people-from-outside-the-domain)) and a deleted Workspace user
alike. Where it names a refused account, clear that one refusal, with the broker clear above;
every other refusal stays. The token goes in a file curl reads the header from, never in the
command's arguments, which every user of the box reads from `/proc/<pid>/cmdline` while it runs:

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
as its holder and the address's session generation, exactly as the record-again block above does
for one row, and leaves alone a row whose account holds the address again (the broker must not be
told that the holder was replaced) and one at an address that records no account. It locks the
session row of every address it will write first, as the single-row blocks do, so a sign-out at
one of those addresses waits for it rather than moving a generation between this statement's read
of it and the database's own check, which would refuse the whole statement with 23514. It owes
again, too, each revocation of a restricted member's machine logins that a removal from an access
list recorded and Dispatch settled since the same moment (`machine_login_revocations`): a broker
restored from before it holds those logins live again, and their refresh works. The statement
keeps each row's `recorded_at`, so the pass again revokes only the logins the member approved
before that removal, and a login they approved after being put back on a list survives it; its
write of `generation` wakes the loop, as a removal's does:

```sh
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -v backup='<the moment the broker''s backup was taken>' <<'SQL'
begin;
select u.generation from user_sessions u where u.login in (select r.email from broker_revocations r where r.done_at >= :'backup'::timestamptz - interval '1 hour' and r.done_at > coalesce((select d.at from deployment_resets d where d.mark = 'start' order by d.id desc limit 1), '-infinity'::timestamptz)) order by u.login for update;
update broker_revocations set generation = generation + 1, holder_subject = (select sub from people where people.email = broker_revocations.email), session_generation = (select generation from user_sessions where user_sessions.login = broker_revocations.email), done_at = null, recorded_at = clock_timestamp() where done_at >= :'backup'::timestamptz - interval '1 hour' and done_at > coalesce((select d.at from deployment_resets d where d.mark = 'start' order by d.id desc limit 1), '-infinity'::timestamptz) and (select sub from people where people.email = broker_revocations.email) is not null and (select sub from people where people.email = broker_revocations.email) <> broker_revocations.subject;
update machine_login_revocations set generation = generation + 1, done_at = null where done_at >= :'backup'::timestamptz - interval '1 hour';
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
broker recorded approving accounts (its migration 0040) included, with every session each
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

### People from outside the domain

With Google directly, `DISPATCH_SIGNIN_OUTSIDE=1` also signs in a person whose Google account is
outside the domain, as a **restricted member**, while a project they are to work on names them:
the project's owner is that email, or its owner put the email on the project's access list. A
group on an access list never admits such a person; only the email itself does. Their Google
account's email must be verified, and an address on a domain the organization owns that is not
one of the organization's own accounts is refused, with a page saying that such an address signs
in only as an account of the organization. The organization's domains are `DISPATCH_SIGNIN_DOMAIN`
and those `DISPATCH_SIGNIN_WORKSPACE_DOMAINS` lists, comma-separated, such as
`example.net,example.org`, each with its subdomains. Write an internationalized domain in Unicode,
such as `bücher.example`, or as its A-labels, `xn--bcher-kva.example`: the two are the same domain,
and an address written either way is on it. List every other domain the organization
owns: then a personal Google account holding one of your people's addresses cannot sign in with
it, even before your person ever has. Your own people sign in at an address on any of them as
before, and without `DISPATCH_SIGNIN_OUTSIDE=1` or `DISPATCH_SIGNIN_PASSKEY=1` the setting
changes nothing. On a domain the organization owns that the setting does not list, an outside
account still never takes the
address of one of your people once they have signed in with it: it reaches a page saying the
address belongs to a member of the organization, and your person stays signed in. An outside
account that signed in with such an address before your person first did loses it at that first
sign-in: Dispatch signs it out and revokes every agent token it made, so none of them acts as your
person. An account from outside the organization holds no machine login at the secrets broker, so
the machine logins your person made there are kept. Someone outside the domain whom no list names
reaches a page saying that no project's
access list names them. Dispatch checks again at least hourly, as for everyone, and signs the
person out once no list names them, once their address is on a domain the organization owns
(including one you add to `DISPATCH_SIGNIN_WORKSPACE_DOMAINS` later), or once the setting is off.
The sign-in page they then reach says why: that no project's access list names their address any
more, that their address signs in only as an account of the organization, or that Dispatch no
longer signs in accounts from outside the domain. The API's `401 NO_IDENTITY` says the same, as a
`reason` of `NOT_LISTED`, `WORKSPACE_ADDRESS` or `OUTSIDE_SIGNIN_OFF` beside their address as
`login`. A person who signs out, or whose sign-in simply expires, is told nothing more, and a
deployment that never sets `DISPATCH_SIGNIN_OUTSIDE` or `DISPATCH_SIGNIN_PASSKEY` answers as it
always did.

The agent tokens such a person makes in **Settings → Agent tokens**, and the tokens the secrets
broker issues to their agents, follow the very rule that signs the person in, checked at every
request rather than hourly: a token stops at once when the setting is off, when its owner's
address is on a domain the organization owns (including one you add to
`DISPATCH_SIGNIN_WORKSPACE_DOMAINS` later), or when no project names its owner any more, and the
answer is `401 TOKEN_OWNER_NOT_ADMITTED`, saying which. None of these revokes the token, so it
works again once all hold again; only another account's sign-in to the address revokes the
Settings tokens, as above, after which each answers `401 TOKEN_REVOKED`. Your own people's tokens
are not affected.

For this the Google client's consent screen's audience is **External**. Dispatch asks for
`openid`, `email` and `profile` alone, so the app can stay in Testing status with no list of test
users, and its sign-ins do not lapse after seven days. Dispatch then names no domain to Google's
chooser, so a person from outside sees their own accounts; your own people see every account in
their browser and pick their organization's one.

People from outside reach Dispatch however the deployment lets them, at its second address,
`DISPATCH_OUTSIDE_ORIGIN`, such as `https://dispatch-outside.example.com`: an `https` address with no
path, on another host than `DISPATCH_SERVER_URL`. Google must accept its callback as a redirect
URI, so its host is a DNS name, not an IP address, whose top-level domain is on the public suffix
list; Dispatch refuses to start with any other. A request to that host is answered as that
address: the sign-in comes back to `<DISPATCH_OUTSIDE_ORIGIN>/auth/callback`, which the Google
client lists as a second redirect URI, and a write from a page there must come from that address.
A link to a Dispatch page written on either address opens on the address the reader is on.

### People from outside with a passkey

`DISPATCH_SIGNIN_PASSKEY=1`, set instead of `DISPATCH_SIGNIN_OUTSIDE=1` (Dispatch refuses to start
with both), lets a person from outside sign in with a passkey and no Google account at all. It needs
`DISPATCH_SIGNIN_DOMAIN` and `DISPATCH_OUTSIDE_ORIGIN`: Google then signs in your own people alone,
and a person from outside signs in only at the outside address, where Dispatch serves no Google
sign-in. So the Google client lists no callback on the outside address, only
`<DISPATCH_SERVER_URL>/auth/callback`, and its consent screen's audience can stay Internal. A
project's owner puts the person's email on the project's access list, then makes an
invitation link for them on the project's **Access** tab and hands it over however they like. The
link works once, within 24 hours, for that person only, and making a new one ends the old one. With
it the person enrolls a passkey on their own device, and from then on signs in with that passkey.
The owner sees on the Access tab when each passkey was enrolled and who made the link it came
through, so a link someone else used first shows up there. One owner binds a person from outside:
a passkey signs its person in to every project whose list names them, so an owner makes a link
only for a person no other owner's project names, the enrollment checks that again, and no other
owner can name a person on their list, or be given a project naming them, while a passkey another
owner's link enrolled stands. **Reset** ends the person's sign-in, deletes their passkeys, revokes
the agent tokens they made and the machine logins they approved at the secrets broker, ends every
link still waiting for them on any project, and keeps them on the list, so they enroll again with
a new link. Taking the person off the last list that names them ends the sign-in and deletes the
passkeys at once, and Dispatch also checks at least hourly, as above. No link is made for an
address on a domain the organization owns (`DISPATCH_SIGNIN_DOMAIN`, a domain
`DISPATCH_SIGNIN_WORKSPACE_DOMAINS` lists, or a subdomain of one), nor for one of your people who
has signed in with Google. List every domain the organization owns there: a domain left out is not
the organization's to Dispatch, so an owner can make a link for an address of one of your people
there before they first sign in with Google, and the passkey enrolled with it keeps signing in as
that address after your organization removes that person, since no passkey sign-in asks Google.
While the setting is off, Dispatch refuses every such person's sign-in at their next request,
telling them Dispatch does not sign them in now, and refuses their agent tokens as above; turned on
again, their passkeys, sign-ins and tokens work again, so end a person's access for good with
Reset or by taking them off the lists. Without `DISPATCH_SIGNIN_OUTSIDE=1`, the outside address
never offers Google sign-in, with passkeys on or off.

## Behind Identity-Aware Proxy

A Dispatch served behind a Google Cloud load balancer with Identity-Aware Proxy (IAP) turned on
lets IAP sign people in: `DISPATCH_IDENTITY=iap`. On each request it lets through, IAP adds an
assertion it signs, the `X-Goog-IAP-JWT-Assertion` header, and Dispatch names the person from that
assertion alone. It never reads IAP's unsigned `X-Goog-Authenticated-User-Email` or
`X-Goog-Authenticated-User-Id`, which a request that reached Dispatch around IAP can carry.

Dispatch checks each assertion as Google documents
([Securing your app with signed headers](https://cloud.google.com/iap/docs/signed-headers-howto)):
an ES256 signature by one of IAP's keys, which it reads from
`https://www.gstatic.com/iap/verify/public_key-jwk` on the first request and caches; the issuer
`https://cloud.google.com/iap`; the audience `DISPATCH_IAP_AUDIENCE`; and the expiry and issue
times, each with 30 seconds of clock skew. Set `DISPATCH_IAP_AUDIENCE` to the audience IAP shows as
the **Signed Header JWT Audience** of the load balancer's backend service,
`/projects/<project number>/global/backendServices/<backend service id>`.

Who may use Dispatch is the same rule as Google sign-in's: set `DISPATCH_SIGNIN_DOMAIN` to the
organization's primary Workspace domain, and a person is admitted when the assertion's `hd` is that
domain and it names a Google account, named by the assertion's `email`, lowercased. The other
`DISPATCH_SIGNIN_*` settings are refused: Dispatch runs no sign-in of its own, so `/auth/start`
answers `503 SIGNIN_UNCONFIGURED`. Grant IAP access to the people who should reach Dispatch as
well; Dispatch's own check stands behind IAP's, so a request that bypasses IAP, or carries an
assertion for another backend service, is refused.

IAP signs a fresh assertion for every request, so Dispatch keeps no session for it: a person IAP or
the domain stops admitting is refused from their next request, and Dispatch's logout signs nobody
out of IAP. A request with no valid assertion, or whose person the domain does not admit, is
answered `401 NO_IDENTITY`; one that finds IAP's key set unreadable is answered
`503 SIGNIN_UNAVAILABLE`, and the next one after the keys can be read goes through. A write from a
page of another origin is refused, as it is for a session cookie, since a browser sends IAP's own
cookie with it.

A page's live connections, its live socket and each open document's socket, are the one place an
assertion ages: they re-check the assertion they opened with every 15 seconds, and IAP gives an
assertion about ten minutes. When it expires, Dispatch closes the socket with a code on which the
page reopens it at once (`4001` for the live socket, `4412` for a document's), and the reopened
socket carries IAP's next assertion, which is checked again. So each of an IAP user's sockets
reopens about every ten minutes: the live socket shows nothing of it, and a document reconnects
as it does after any close the server makes, its sync restoring whatever was in flight. Only an
assertion IAP signed counts as expired: one IAP's keys no longer verify, or whose person the domain
no longer admits, is refused, and the socket closes as it does when its person signs out.

## Changing the session key

`DISPATCH_SIGNING_KEY` takes the new key beside the old one, so the key changes without signing
anyone out. Make the change in three steps, and restart every server after each before the next:

1. Add the new key second, after the current one: `<old key> <new key>`. Every server still signs
   with the old key and now accepts the new one.
2. Move the new key first: `<new key> <old key>`. Every server signs with the new key and still
   accepts the old one, so the sign-ins made under it keep working.
3. Remove the old key, no sooner than 30 days after step 2: a sign-in cookie lasts 30 days, and each
   stored refresh token is sealed again under the new key by its person's next hourly confirmation.

Each step keeps every server's keys shared with the step before, so a rolling restart passes the
check that refuses a server whose keys another running server does not share. Replacing the key in
one step fails that check beside a server still on the old key, and on a single server signs
everyone out.

Two rules come with it. The first release of Dispatch that takes several keys is never the release
you first change the key in, and while the setting holds two keys, never roll back to a release
older than that: such a release reads both keys as one, and signs everyone out. Removing a key
revokes no refresh token at the issuer: whoever holds a database backup and the old key can still
use the tokens sealed under it until they expire, so after a key leaks, sign people out at the
issuer too.

Separate the keys with one space or one line feed. Every whitespace character separates two keys,
so a key itself holds none, and two in a row, a CR LF among them, enclose an empty key, which
Dispatch refuses to start with.

On a deployment built by `envoy-infra`'s `gcp:Dispatch`, you write no key and edit no Secret:
each step above is one `pulumi up` of the component's `signingKeyGenerations`, the ordered list
of key generations `DISPATCH_SIGNING_KEY` holds. A deployment running one key is at `[0]`; step 1
is `[0, 1]`, step 2 is `[1, 0]`, and step 3 is `[1]`. The component generates each generation's
key, renders them in the list's order and replaces the app's pods on each apply, which is the
restart each step needs. Its README (`packages/envoy/infra/README.md`) holds the rest of the
procedure: which pods roll, what a dropped generation costs, and the two lists the component
refuses.

## Running it on your machine

To see Dispatch working, seeded with example issues, asks, agents and a broadcast and opened
already signed in, run `bun run dispatch:local` at the repository root. It starts Dispatch beside
stand-ins for an Envoy listener and GitHub, empties and reseeds its database every time it starts,
and prints the address to open; `packages/dispatch/README.md` ("Local instance") covers it.

To run Dispatch on a database that keeps what you put in it, from a checkout of this repository,
with Docker, Bun, and Go installed:

1. Start a Postgres:

   ```sh
   packages/envoy/scripts/dev-postgres.sh
   ```

   It runs a container named `dispatch-pg` on `127.0.0.1:55432` from a pinned image, pgvector on
   PostgreSQL 18 (Dispatch's migrations need its `vector` extension, and 18 is the version
   Dispatch's tests and dev databases run), and starts or reuses that container when it is already
   there. A `dispatch-pg` made from another image or PostgreSQL major is refused, with the commands
   that dump its databases and restore them into a new container on the right one.

2. Build the web app:

   ```sh
   bun install
   (cd packages/dispatch && bun run build:web)
   ```

3. Start the server, still from the repository root:

   ```sh
   DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable' \
   DISPATCH_AGENT_TOKEN='<a long random string>' \
   DISPATCH_DEV_SIGNIN=1 \
   DISPATCH_SERVER_URL=http://127.0.0.1:8766 \
   DISPATCH_LISTEN_HOST=127.0.0.1 \
   DISPATCH_INSECURE_COOKIE=1 \
   DISPATCH_NATS_DISABLED=1 \
   DISPATCH_WEB_DIST="$PWD/packages/dispatch/web/dist" \
   go -C packages/envoy run ./cmd/dispatch
   ```

   `DISPATCH_WEB_DIST` is where the web app was built. `DISPATCH_NATS_DISABLED=1` keeps this run off
   any message bus. `DISPATCH_DEV_SIGNIN=1` lets you sign in without an issuer.

4. Open `http://127.0.0.1:8766/auth/_dev/signin?login=<your email>`. It signs in anyone by the email
   it names, with no issuer. Dispatch accepts it only on your own machine, with every address in the
   setup on that machine, and that server needs a database of its own.

To sign in for real instead, replace `DISPATCH_DEV_SIGNIN=1` in step 3 with the issuer, client ID,
client secret and group or domain settings, and list `http://127.0.0.1:8766/auth/callback` as a
callback URL of Dispatch's client: the sign-in pool's app client, or a Google web client, with
`DISPATCH_SIGNIN_ISSUER=https://accounts.google.com` and `DISPATCH_SIGNIN_DOMAIN=example.com` for
your domain. With Google directly, the server needs no AWS account or credentials. Search by meaning
needs AWS credentials for Bedrock, or the Azure settings ([What it needs](#what-it-needs)); without
either, the server logs that meaning search is unavailable and serves everything else.

## First steps after it starts

Sign in, then open **Settings**:

- **Projects**: create your first project, with a key such as `CORE` and a name.
- **Repositories → Projects**: choose which project issues from each GitHub repository land in.
  `DISPATCH_DEFAULT_PROJECT` catches repositories with no mapping; that project must already exist.
- **Projects → Default assignee**: name the person each project's unowned work lands on, in the
  projects table's own column. An issue created with no assignee, by an agent on the shared token
  and under no parent that names one, goes to that person, and so does every question on an issue
  nobody holds. Every project names one, so the page moves a default from one person to another
  and clears none. A person moves it here, and an agent names a project's first one in the call
  that creates the project.
- **Architecture sources**: add each repository branch holding part of a project's architecture
  model. A project can have several, one per repository, and its model is all of their components
  together. Saving checks that the GitHub App can read the repository.
- **Delivery timeline**: name the repository that deploys, its deploy and pull-request-check
  workflows, the job whose success is a production deploy, and whose merged pull requests count.
  Three job names are optional and feed the [Pipeline](/legion/dispatch/delivery/#pipeline) page:
  the deploy workflow's staging end-to-end job and staging deploy job (each left empty when the
  pipeline has no such step, and the page then omits its card), and the PR-checks workflow's
  roll-up job, which only reports the other jobs' result and which the failing-jobs table leaves
  out. Saving checks that the GitHub App can read the repository. Until it is set, the
  [Delivery](/legion/dispatch/delivery/) page shows the same form. Every save, of any field,
  starts the delivery import over: what is already stored stays on both pages, the freshness line
  reads "never checked" until the next five-minute pass, that pass lists the last 28 days from
  GitHub again, and two re-listings walk those 28 days a day per pass, about 28 passes in all:
  one of each run's branch and event, and one of each deploy run's creation time, which decides
  the deploy a pull request is credited to.
- **Agent tokens**: make a token for each agent you run, listing under **Projects** the projects
  it works on. [Dispatch for agents](/legion/dispatch/for-agents/#connecting-an-agent) covers the
  rest.

## Private projects

Every project has one owner and an access list of people and groups, both on the project's
**Access** tab. A full member sees every public project. A private project is seen only by its
owner, the people its list names and the full members of the groups it names. A restricted
member, who signs in from outside the company, sees a project, public or private, only when its
list names them directly. Every other caller reads a private project, and everything in it, as a
project that does not exist.

What each credential sees:

- A person at the browser sees every project the rule above lets them see.
- An agent's token that lists projects, a personal `dsp_` token from Settings or an access token
  the secrets broker issued for a project, sees those of them its person sees and nothing else.
  Of what belongs to no project it reads only the people and sessions of those projects, its own,
  and its person's direct conversations with those sessions. A read of the whole deployment, such
  as the priorities, the Pipeline page, the broadcasts or the groups, is refused
  `403 DEPLOYMENT_READ_REFUSED`, and a write of it, such as a new project, a repository mapping or
  a broadcast, `403 PROJECT_SCOPE_DEPLOYMENT`.
- An agent's token that lists no project sees the public projects its person sees, and never a
  private one.
- A credential that acts for no person, the shared `DISPATCH_AGENT_TOKEN` or a Kubernetes
  service-account token, sees the public projects.

With no private project in the deployment, a token that lists no project sees every project its
person sees.

A restricted member's dashboard holds only what they may read. Their rail shows Inbox, Agents,
Delivery, their projects and Settings, with no Priorities or Pipeline and no Peek. Delivery shows
them the pull requests whose issue they see and the deploys that shipped them, with no run's jobs,
no setup form and no session-groups editor. The Priorities, Pipeline and broadcasts pages answer
**Page not found**, and Settings shows no Groups and no Delivery timeline. The machine-login page
is theirs, listing their own machine logins alone: there they approve, at their browser, the device
login of their own agents, and list and revoke their own machine logins, and Dispatch decides or
ends for them no other credential. The API refuses them, and every token they make, every read of
the whole deployment, `403 DEPLOYMENT_READ_REFUSED`, and every write of it, `403 RESTRICTED_MEMBER`,
but for those calls at their browser, and except that they revoke their own credential grants and
end their own sign-ins, as a full member does: the secrets broker decides whose each one is. The
secrets broker holds their agents' tokens to one project, which
their agents name in `AGENT_SECRETS_PROJECTS`. Taking such a person off a project's list ends
their tokens for it within 325 seconds, since the broker checks every trade against Dispatch, and
Dispatch then revokes their machine logins at the broker, after the removal commits and again until
the broker confirms; the log line `the secrets broker revoked the machine logins of a restricted
member taken off an access list` records each, and the person approves a new machine login for a
project that still lists them.

A private project's Legion daemon needs a `dsp_` token that lists the project: the shared token and
a token that lists no project read the private project as missing. A person who sees the project
makes it in **Settings → Agent tokens**, picking the project under **Projects**, and the daemon
reads it from the file its `dispatch_token_file` names. A token that lists projects sees no other
project, so it lists every project in the daemon's `projects`. Every write of the daemon's agents
is then attributed to the person who made the token.

A project can be made private only once the deployment has provisioned it.
`DISPATCH_PRIVATE_PROJECTS` holds, comma-separated, the keys of the projects whose private
namespace, stores and grants the deployment has made; on a deployment built by `envoy-infra`'s
`gcp:Dispatch`, the component's `privateProjects` input, a list of keys, renders it. An entry that
is not a project key, the old value `1` included, stops the start. Unset or empty, no project can
be made private.

The owner switches a project private on its Access tab
(`PATCH /api/v1/projects/{key}/access` with `{"visibility": "private"}`), and Dispatch checks, in
this order:

1. With an empty list, every switch to private is `409 PRIVATE_PROJECTS_OFF`, before anything is
   read.
2. A caller who does not see the project is `404 PROJECT_NOT_FOUND`, as for a key no project
   holds, and one who sees it but does not own it is `403 PROJECT_OWNER_REQUIRED`.
3. A key the list does not name is `409 PRIVATE_PROJECT_NOT_PROVISIONED`.
4. A store that cannot hold the project private yet is `409 PRIVATE_PROJECTS_INCOMPLETE`, naming
   what repairs it: each row of the project that has no node naming its project, with the cure
   the project census prints for it (`envoy-dispatch backfill-graph-store`, or none for a node
   that names another project, which a person compares with its row),
   `envoy-dispatch backfill-session-projects` until the sessions' projects are filled, and
   `envoy-dispatch scrub-access-events` until the stored access events are scrubbed.
5. Last, in the same transaction, Dispatch writes the project's key in the hub's
   `envoy_private_projects` bucket, the record by which Envoy's listeners are to hold the
   project's messages back from a token that does not list it. When the hub refuses that write,
   as a hub without its auth service does, or Dispatch has no bus to write it to, the switch is
   `409 PRIVATE_PROJECTS_UNAVAILABLE` and nothing changes. This release of Dispatch has no writer
   of that bucket, so every switch that passes steps 1 to 4 is
   `409 PRIVATE_PROJECTS_UNAVAILABLE`: no project can be made private until a release carries the
   writer.

Of these, only the owner check of step 2 applies to a switch to public.

A project the database holds private that the list does not name, because its key was taken off
the list or the list was emptied, stays private. Nothing of it reaches anyone who could not see
it before, and every change of its visibility but its owner's switch to public is refused.
Dispatch still starts and serves every other project: it logs an ERROR line naming the project and
both repairs at start and at every maintenance pass, once a minute, and `/healthz` answers
`degraded: ["private_project_unprovisioned"]`, naming no project. The owner decides the repair: put the key back on the list, or
make the project public, from the Access tab or, with the database credential alone and no server
running if need be, with:

```sh
envoy-dispatch project-visibility <KEY> public
```

So to take a key off the list, make the project public first, and only then remove its key. At
start Dispatch also logs, at INFO, the listed keys no private project holds yet. With a list, each
server logs an ERROR line as it starts, and then once an hour, while the store cannot hold a
private project for one of the reasons of step 4, naming the reason and what repairs it. Every
response carries `Dispatch-Private-Projects: 1` while the list names a project, the database holds
a private one, or the server's last read of the database's private projects failed, and
`Dispatch-Private-Projects: 0` otherwise.

## Health check

`GET /healthz` needs no credential. It answers `200` while the server can serve requests, which is
while Postgres answers:

```json
{"ok":true,"db":true,"nats":null,"files":null,"signin":null,"service_tokens":null,"github":null,"commit":null,"fleet_commit":null,"schema_version":121,"outbox_publisher":false}
```

It is a readiness check, not a liveness check: it fails while the database cannot be reached, so a
liveness probe on it would restart every server at once during a database failover. Point a load
balancer's or Kubernetes' readiness probe at it.

- `ok` is whether this server can serve requests, the same as `db`.
- `db` is whether Postgres answered within two seconds. When it does not, the answer is `503` with
  `db: false`.
- `nats` is whether NATS is connected, or `null` when NATS is turned off. A server serves while NATS
  is down, so it never decides `ok` or the status, and a NATS that stops answering without closing
  the connection reads `false` within three seconds.
- `files` is whether the file store's bucket (`DISPATCH_FILE_STORE_BUCKET`) answered within two
  seconds, or `null` when no bucket is set. It never decides `ok` or the status: a bucket that
  stops answering is `200` with `files: false`, so an outage of the bucket fails uploads and
  downloads and nothing else.
- `signin` and `service_tokens` are whether the sign-in issuer and the service-account token issuer
  have been reached, or `null` when that issuer is not set. Neither decides `ok` or the status.
- `github` names each GitHub read the server makes in the background, with how its newest attempt
  went, or is `null` when no GitHub App is configured. It never decides `ok` or the status.
  [GitHub reads](#github-reads), below, says what it holds.
- `commit` is the source commit this server's image was built from, or `null` in a build that was
  not stamped.
- `fleet_commit` is the commit every running server was last found serving, the latest announced
  build, or `null` before any was found. It is read from the database, so it is reported while NATS
  is down too. It can trail by a few seconds.
- `schema_version` is the newest database migration applied.
- `outbox_publisher` is whether this server is the one publishing notifications to NATS now. One
  server publishes at a time, so a healthy server can say `false`, and it always does when NATS is
  turned off. It never decides `ok` or the status.
- `degraded` is `["private_project_unprovisioned"]` while the database holds a private project
  that `DISPATCH_PRIVATE_PROJECTS` does not name ([Private projects](#private-projects)), as the
  server's last read found it: it reads every 30 seconds, so a probe adds no read. It names no
  project and counts none, because the check answers anyone; the project's key is in the ERROR
  line. It is absent when there is none, and it never decides `ok` or the status.

A check that needs NATS, the bucket or sign-in asserts `nats: true`, `files: true` or `signin: true`
itself.

### GitHub reads

Four background loops read GitHub. Each runs under a lease, so one server makes each read in each
period. `github` holds one entry for each loop, in this order:

- `webhook_redelivery`: the sweep that asks GitHub to redeliver the webhook deliveries that
  failed, every 2 minutes.
- `architecture_import`: the import of every enabled architecture source, every 5 minutes.
- `delivery_reconcile`: the read of the pull requests and workflow runs that the
  [Delivery](/legion/dispatch/delivery/) page shows, and of the pull request heads that results
  are compared with, every 5 minutes.
- `ci_reconcile`: the read of each open pull request's checks, and of its head's commit statuses
  where the App's token for the repository holds Commit statuses, every 5 minutes.

Each entry is `{"state": …, "at": …, "cause": …}`:

- `state` is one of five words:
  - `succeeded`: the newest attempt sent requests to GitHub and met no failure.
  - `failed`: the newest attempt met a failure. The lists below say which failures count.
  - `no_attempt`: no attempt is recorded yet.
  - `unknown`: this server has not read the record of attempts yet.
  - `off`: this server does not run the read. All four are `off` without the App's private key,
    and `webhook_redelivery` and `ci_reconcile` are `off` without NATS too.
- `at` is when the newest attempt was recorded, as its pass ended, by the database's clock, in
  RFC 3339 UTC. It is `null` unless the state is `succeeded` or `failed`.
- `cause` is the class of the first failure of that attempt: `status <n>`, the HTTP status GitHub
  answered, or `no status` for a failure that names none: GitHub never answered, or the read
  itself failed on an answer it got. It is `null` unless the state is `failed`.

`github` never decides `ok` or the status: a server serves everything but these reads while
GitHub fails. Each server keeps a copy of the record of attempts in memory, reads it again each
time any server records an attempt, and answers from that copy, so `github` sends the database
nothing and waits on nothing.

The failure's own text can name a repository, because a GitHub error carries the address it
asked, and `/healthz` needs no credential, so whoever reaches the server's HTTP port can read the
answer. So `/healthz` answers the class alone. Read the text, at most 1,024 bytes, in the
database's `github_read_attempts` table: `select read, at, cause from github_read_attempts;`. A
longer text is cut, and ends with `… (cut from <n> bytes)`.

The delivery intakes also read GitHub, for each webhook event, and have no entry of their own.
They report through `delivery_reconcile`: the reconcile reads the same pull requests and runs with
the same App every 5 minutes, so a GitHub failure of those reads shows there within one pass.

An attempt is one pass of a loop, on the server that holds the loop's lease. It is recorded when
the pass ends, in place of the attempt before it. A pass records nothing when it sent GitHub
nothing and met no failure: for example, the sweep before NATS connects, a CI reconcile with no
repository to read, an architecture import with no enabled source, or a delivery reconcile with no
delivery settings and no pull request to read again. A pass that the server's shutdown cut short
records nothing either. The same reads made by a route or a command record nothing.

An attempt is `failed` when it meets a failure of one of two kinds. The first kind is an answer
that the GitHub client counts on every request a pass sends:

- GitHub gave no answer: a network error, the client's own 20-second timeout, or a body the client
  could not read or that was past its size limit.
- GitHub answered with a 5xx status or with 401.
- A rate limit: a 429, a 403 that carries a limit, a GraphQL answer that reports a limit (also
  with status 200), or a limit already recorded that held the request before it was sent.

The second kind is a part of its read that a loop gives up:

- The webhook redelivery sweep: the list of the App webhook's deliveries fails, GitHub refuses a
  redelivery request, or a rate limit stops the sweep.
- The CI reconcile: a repository it reads that it cannot read (it gets no token for it, GitHub does
  not answer for it or gives it a name that is not `owner/name`, or its open pull requests cannot
  be listed), a repository a project maps that the App is not installed on, a pull request head it
  cannot read (an answer too large included), a failed read of a head's commit statuses, or a
  rate limit that ends the pass, also one already in force when the pass starts. A read that the
  pass's own time limit cut short is not one of these.
- The architecture import: the sync of a source fails at a step that reads GitHub, which is its
  installation token, its branch head, its directory listing or its files. That includes the App
  not installed on the source's repository, a missing Contents permission, and a branch that
  GitHub does not have.
- The delivery reconcile: the App's permission check of the deploy repository fails; a step that
  reads GitHub fails (merged pull requests, partial pull requests, pull request heads, the deploy
  and PR-checks workflow runs, or the attribution inputs of stored pull requests); or the pass
  gives up one row of such a step: a pull request head it cannot read again, a head it attributes
  without its commit messages, a partial pull request it cannot complete, or a stored pull request
  whose attribution inputs it cannot read.

At those points a failure counts whatever caused it, a database error inside the step included,
because that error also ends that part of the read. The cause is the loop's own error, which
names what failed and where.

These answers are not failures. The loop takes each one as data and goes on, so the attempt stays
`succeeded`:

- a delivery that GitHub no longer has (the sweep marks it expired);
- a pull request or a workflow run that GitHub says is gone, or whose repository the App is no
  longer installed on (the delivery reconcile marks it as one it cannot fetch);
- a pull request head that GitHub no longer has, or of a repository the App cannot read (the
  reconcile marks it as read);
- a directory of an architecture source that GitHub answers `404 Not Found` for at the commit
  (the import reads it as an empty model);
- a repository that only an issue names, which the App is not installed on (the CI reconcile
  passes over it);
- a `304 Not Modified`;
- a page that the delivery reconcile asks for again after a 502, a 504 or a timeout, and then
  reads;
- a CI reconcile query that fails when the reconcile then reads the same repository over REST;
- a sync of an architecture source that fails and finds that another sync or a settings save
  changed the source first, once the import syncs the source again and that sync reaches GitHub:
  it sends a request, or meets a failure such as a rate limit already recorded. When the import
  stops before a later sync reaches GitHub, because the time for the sync ran out or the database
  failed, the failure of the last sync that reached GitHub counts.

The GitHub client counts no other 403, and no 404, 409 or 422, because the same answer is data at
one point and a failure at another. It does not count a request that was cut off because its
caller's time ran out or the server shut down. A failure of the database or NATS outside the
points above is not a failure here: `db` and `nats` report those. What the files of an
architecture source say is not a GitHub read either: the source's own error records a note larger
than 1 MiB, a note that Dispatch cannot parse, or a model it refuses. The delivery reconcile's
re-listings of older runs never fail its pass, but a failure of the first kind that they meet still
counts.

## Metrics

`GET /metrics` needs a credential, as every route but sign-in, `/healthz`, `/livez` and the
dashboard's own files does: a caller that sees every public project reads it, the deployment's
shared agent token among them, and a restricted member or a credential that lists its projects
is refused `403 DEPLOYMENT_READ_REFUSED`. A caller with no credential is refused `401`, whatever
address or network it comes from.
It answers in the Prometheus text format with two counters and four gauges.
`envoy_dispatch_outbox_publish_failures_total` counts each destination of an event publish that
was not reached, labelled `reason`. `denied` is a publish NATS refused under the server's own
grant, `refused` one NATS refuses however often it is retried, `unanswered` a forward NATS took
whose stored copy the stream did not acknowledge - which is the whole of a forward's answer, for
every holder alike - and `failed` any other failure. A denied publish
also logs `dispatch outbox: publish event` at ERROR, naming the subject.

`envoy_dispatch_outbox_role_forwards_held_total` counts a role-routed event the outbox held
rather than sent, labelled `holder_state` with the state its holder was in: `unresponsive` for a
holder that stopped answering, `undated` for one nothing dates, `unresolved` for a claim or
session read that did not answer. A hold is a wait and not a failure - nothing was sent, the
holder keeps its claim, and the event is retried until the session answers - so it is counted
here rather than with the failures above, and its event's back-off is logged at INFO
(`dispatch outbox: event waits for its role's holder`). An event that reaches the dead-letter
threshold logs at ERROR however it was held.

A copy of a comment's mention waits until every earlier event of its issue that a session may
take first has been published, so the session takes them in order. The wait has no limit, so it
shows. `envoy_dispatch_mention_sends_held` counts the mention sends that wait, and
`envoy_dispatch_mention_send_held_oldest_seconds` is how long the oldest of them has waited.
`envoy_dispatch_outbox_fallback_copies_held` and
`envoy_dispatch_outbox_fallback_copy_held_oldest_seconds` are the same for the copies the outbox
holds for a mention whose send did not settle. The four carry no label: an issue key is private,
and anyone who reaches the port reads this endpoint. A copy that waits a minute logs one WARN
that names the owner, the earlier event and that event's last publish error. The cause is the
earlier event's failed publish, which logs `dispatch outbox: publish event` at ERROR at each
attempt and counts in `envoy_dispatch_outbox_publish_failures_total`.

## Behind a proxy

A page's live updates ride one WebSocket per tab, `GET /ws/live`. A browser keeps WebSockets apart
from the six connections it opens to a host for ordinary requests, so a socket that stays open
never takes one of them. That holds only while the load balancer or proxy in front of Dispatch
does not advertise HTTP/2 extended CONNECT (`SETTINGS_ENABLE_CONNECT_PROTOCOL`): a browser that
sees it runs the socket inside its HTTP/2 connection instead, and pages can hang until the tab is
reloaded. From a checkout of this repository with `bun install` run,
`bun scripts/e2e/lib/h2-settings.ts https://<dispatch>` prints the settings the proxy sends and
exits 1 when it advertises extended CONNECT, 0 when it does not or speaks no HTTP/2, and 2 when it
gets no answer. Run it after any change to the proxy.

An open document keeps a second WebSocket, `GET /ws/doc/<id>`, for as long as it is open. Each
socket carries a message while the page is quiet: Dispatch writes a heartbeat to `/ws/live` every
15 seconds, and on `/ws/doc` the page's document provider renews its presence every 15 to 18
seconds, which ygo sends back to it. A page connected read-only gets an empty presence reply
instead: a closed issue's document, and also a page on an open issue whose `schema_version` no
longer matches the server's, such as a tab left open across an upgrade. A proxy whose idle timeout
is longer than about 20 seconds therefore never closes either one.

## Running more than one copy

Dispatch runs as more than one server against one database, so a new version rolls out without
taking Dispatch away from everyone: start the new version, let it take traffic, then stop the old
one. Three things have to be true first; Dispatch checks the first for you.

- **Give every server the same `DISPATCH_SIGNING_KEY`.** A sign-in made on one server is checked by
  whichever server answers next, so each server must accept the key the others sign with. Left
  unset, each server makes a key of its own in its own data directory. Each server records the
  servers that are running in the database, with a fingerprint of each key that reveals nothing of
  it, and a server whose keys a running server does not share refuses to start: it logs why,
  naming the other server and the steps of [changing the session key](#changing-the-session-key),
  and exits before it serves anything. Before it exits it waits up to 15 seconds and one more
  report (5 seconds) for the other server to stop reporting in, so a server started right after
  another was killed, which never removed itself from the database, starts once the killed
  server's record lapses; a server that is running keeps reporting in and keeps it refused. A
  rolling update to a changed key stalls the same way on the first new server while the old ones
  keep serving. A server that stopped reporting in for longer than 15 seconds (frozen, or cut off
  from the database) is not counted meanwhile, so it checks its keys again before it counts once
  more: beside a running server whose keys it does not share, it logs why, finishes the requests
  it is answering, and exits 1.
- **Give every server the same embedder settings.** Each stored vector records the embedder that
  made it, and each server drops the vectors another embedder made and embeds that text again, so
  two servers set up differently keep replacing each other's work. A rollout that changes the
  settings settles once the last old server stops. For the same reason, a new model on Azure goes
  under a new deployment name: the name, not the model, tells the vectors apart.
- **Every migration leaves the version before it able to read and write the database**
  ([Migrations](#migrations)), because a rolling update runs both versions against the one
  database while it lasts.

A second server also costs connections, and Postgres counts them per server. Each server opens up
to 64 connections for requests, up to 22 for the work it runs once across the servers and the
delivery intakes' holds, up to 8 for saving what people type in documents, up to 4 for loading
documents, up to 2 for compacting documents, up to 2 for the background fill of documents'
display trees, one for the health check, two for publishing
notifications, up to 2 for reading where the event log is safe to stream from (the event horizon),
and one for its entry in the list of running servers, which it renews every five seconds. It also
holds up to five more open for as long as it runs. Four of them wait on Postgres notifications:
one for delivery, one for the live socket's event-feed relay, one for publishing notifications,
and one that the server shares for everything else, its document sessions and the waking of its
background work when the rows that work reads change included. The other servers on the same
database find each server by the name of that shared connection. The fifth sends the cursors of
the server's document sessions and its questions and answers to the other servers about a
document. It opens the first time the server has one to send. A document edit's own notification
goes out in the transaction that writes the edit, and needs no connection of its own. That is 113
connections for one server at its ceiling, and twice that for two, past a default Postgres's
limit of 100. Check the database's connection limit before you start the second server: a limit
sized for one server, with whatever else uses that database counted in, is the thing that runs
out.

What a reader sees while two servers are serving:

- Nothing they write is lost, whichever server takes it. Every edit of a document is appended
  under that document's lock, edits made at the same time merge rather than overwrite, and the
  stored history holds all of them.
- Notifications are published by one server at a time, so a second server does not reorder an
  issue's notifications. When the publishing server stops, is killed or loses its connection to
  NATS, the other takes over within ten seconds; the health check's `outbox_publisher` says which
  server publishes. A publishing server whose NATS goes silent without the connection dropping,
  while it has nothing to send, is found out only by its next send, which waits five seconds for
  NATS to answer and then two for a probe of the connection: the other server then takes over
  within ten seconds of the next notification, not of the moment NATS went silent.
- Two people on one document see each other's typing, cursors and presence, whichever server each
  one is on. Each server tells the others through Postgres, so this works while the message bus
  is down. A read never returns text older than what is stored. An agent's edit sees what a
  person on another server just typed, when that server answers within half a second.
- A document session stays open while the other servers write. The server closes it, and its
  browsers reconnect, in these cases only:
  - The document's stored history is replaced, for example by a rebuild.
  - Its issue closes or reopens. The editors on every server reconnect read-only or editable to
    match.
  - The session fails on its server, which then reloads it.
  - Its server shuts down while an editor is connected.
- A server keeps an edit that it took before an issue's close was saved, or up to 2 seconds after
  that, even when the edit reaches the database after the close. An edit typed while the close is
  still being saved is kept too, however long the close takes. The server refuses an edit that it
  took later than that, and the session reconnects read-only.
- When a server's listener connection to the database drops, the server reconnects. Its first
  wait is 1 second, plus up to half again. Each next wait doubles, up to 30 seconds plus up to half
  again, so one wait can last 45 seconds. Until it reconnects, the editors open on that server do
  not receive the other servers' edits and cursors. They catch up when the connection is back. In
  that time a read through the API still returns the stored text. Nothing anyone writes is lost,
  except an edit to a document whose issue closed more than 2 seconds before (above).
- During the rollout that brings this version, a server of the earlier version behaves as before.
  It still closes a session that falls behind, and its browsers reconnect. It sends no cursors to
  the other servers and shows none of theirs. It does not act when an issue is closed or reopened
  through a server of this version, so its open sessions of that issue keep the access they had.
  A document that the close or reopen writes is the exception. A close writes the issue's open
  asks into their documents as resolved, and a reopen writes them open again. The earlier server's
  session of such a document falls behind, so it closes, and its browsers reconnect with the
  access the issue now allows.
- A page of either version keeps loading. Each server stores its version of the dashboard's files
  in the database when it starts, before it takes traffic, and serves the other version's files
  from there, so a page one server sent finds its files whichever server answers next. A server
  that cannot store them does not start, and the roll keeps the servers it would have replaced. A
  version older than the first that stores them stores nothing, so a roll away from one of those
  versions can still leave a page asking for files no server has.
- A document that a server of the earlier version removes or makes again during the rollout can
  leave this version's record of it out of step: `envoy-dispatch census --store` then names it
  `orphaned` or `gone`. Once the rollout is done, run `envoy-dispatch backfill-graph-store` for
  `gone`, and for each `orphaned` line the `envoy-dispatch erase` command that line prints.
- The dashboard's **A new version is available** notice waits until every server runs one
  version: while two versions are serving, nobody is told to reload. A page that fails to load one
  of its files reloads itself, at most three times in five minutes.
- Signing in works whichever server each step reaches, and a sign-in made on one server is good on
  the other.

Dispatch announces a new version on NATS (`server.started`) once every running server serves it,
not as each one starts: a rollout announces once its last old server has stopped, restarting
servers on the version already announced announces nothing, and rolling back announces the
earlier version again.

## Migrations

Dispatch applies its database migrations every time it starts, before it serves anything. Servers
starting together take turns, so a migration runs once. Each migration waits at most five seconds
for a lock. When something holds the table longer, the start fails and names the migration and
what it waited on; end the holder, or let it finish, and start again.

Dispatch records each migration it applies: the name and sha256 of the file the version first
shipped with, which a later edit of the file names in its first `-- replaces:` line, and the sha256
of the text it applied. At every start, before it applies anything, it compares each recorded
migration with the file of the same version in the release it runs. A migration recorded before
this check, with no name, takes the identity the release records of the file and is never
refused, so the first start of the release that brings the check refuses nothing. When the
database records another file under a version, or a text of it the release's file does not name,
as a database that ran another branch's edit does, the start fails before it applies any
migration, and the log names the version, what the database records and what the release's file
is. Dispatch judges every recorded migration again before each one it applies, so a start that
finds such a row only then, because another server recorded it meanwhile, stops before that
migration and keeps the ones it applied. A database that ran another branch's or an earlier
commit's migration, or an edit of one, such as a test or preview database, is reset: drop it,
create it empty, and start Dispatch, which migrates it from the first file. This includes a fresh
preview or development database that a newer build created and an older build then starts on:
the older build refuses it, and you reset it. It includes too a development or preview database
whose rows recorded no name when a branch's build first started on it under this check, where that
branch edits a migration in place and its file does not yet declare the main branch's identity of
it in a replaces line: that start recorded the branch's own identity, not the main branch's, and
the main branch's release refuses it. A branch whose edited file already declares the main
branch's identity, as the pre-merge check requires, records that identity, and the main branch's
release starts on it. A migration edited or renamed on purpose takes the
`-- replaces: <name> sha256:<hex>` line the refusal gives, after the replaces lines it has, and the
next start accepts the recorded row as it is. A release cannot accept a text it has never seen, so
it refuses a database whose migration a
later release applied from an edited text. On a production database, created once, that happens
only on a rollback by hand to a release the database never ran: roll back only to a release it
ran, as the auto-deployer does, which goes back to the image that was running. An edited text a
database ran before this check is not detected: the first start records its own file. To see
what a start would do, without changing the database, run
`DATABASE_URL='<your database URL>' envoy-dispatch versions` with the image you mean to roll
out, where the server would run, as you run its census. On the Kubernetes component
(`packages/envoy/infra/README.md`, "The pods"), both run in a one-off pod of that image under the
app's account, with the app's Cloud SQL Auth Proxy sidecar and its `DATABASE_URL`; the README
gives the commands.

Before you roll a new version out, you can see what its migrations will touch:

```sh
DATABASE_URL='<your database URL>' envoy-dispatch census
```

It reads the database without changing it, and reports each pending migration's tables, their
sizes, and any long-running transactions in the way. Run before a deploy, it is a gate: a census
that refuses, such as one counting rows a migration would rewrite, should fail the deploy before
any server moves. Dispatch repairs rows it already holds in code, at start, for that reason.

A new version rolls into the running one: every migration Dispatch ships is written to leave the
version before it able to read and write the database, so both can run against it while the roll
lasts ([Running more than one copy](#running-more-than-one-copy)). A change that cannot be
written that way is called out in its pull request, and that deploy stops the running server
before the new version starts (on Kubernetes, `strategy: Recreate`).

Once a new version has migrated, don't start the older one again: if the new version fails, fix
it forward. Dispatch does not refuse to start against a schema newer than its own, so an older
server keeps writing to the migrated database as if nothing changed. Some releases depend on that
never happening: the one that adds each issue's repository (migration `0113`) holds every issue to
32 links below its root in its API alone, and an issue an older server puts deeper fails to load
until you clear the parent of an ancestor that still loads. That release was deployed with the
running server stopped first, and no server older than it has run against the database since.

The release that moves live updates onto one WebSocket per tab and keys every write (INFRA-258)
is one of those changes: deploy it with no server of the earlier version running. An earlier
server ignores a write's `Idempotency-Key`, so while both run a write whose answer was lost can be
run once by each: the page and the `dispatch` command both retry such a write, and its retry can
reach the other version. At one replica the Deployment's `Recreate` already stops the old pod
first; above one replica, scale to zero before the new version starts. Releases after it roll
again. Rolling it back brings the hazard back the other way: a tab still open, and a `dispatch`
client that saw this version keep keys in the last five minutes, retry a write whose answer was
lost against the earlier server, which runs it again. So fix it forward, as above; if you must roll
back, stop this version first, knowing a write in flight at that moment may land twice. Once a
later release has dropped the broadcast send's old key table (migration `0141`), rolling back past
this version also stops broadcasts: the earlier version's broadcast send reads and writes that
table, so every broadcast send fails while the rest of that version keeps working. A tab opened
before the deploy needs a reload: its event route is gone and it shows
**Live updates unavailable**, and its broadcast body still carries a field the new server refuses.

From that release on, every page write that could otherwise happen twice carries an
`Idempotency-Key`, and Dispatch keeps each key's answer for 24 hours, so a change tried again after
a lost answer, a timeout or an outage is made once. `PUT`, `DELETE`, and the sign-out
(`POST /auth/logout`) carry no key; the first two are already idempotent and sign-out cannot create
content twice. The `dispatch` command keys every `POST` and `PATCH` it makes. A page that has not
heard back keeps trying and says so, and a script that calls the API itself can send a key of its
own ([For agents](/legion/dispatch/for-agents/#the-http-api)).

The release that lets a project import its components from several repositories (migration
`0117`) is deployed the same way: stop every server of the earlier version, or scale it to zero,
before the new version starts, and never start a version older than it again. An older server
cannot work with the new database: saving an architecture source fails with a server error, an
import that changes a project's components fails, and an import of an empty architecture folder
deletes the components of every source of the project. Its census refuses while any component
belongs to a project with no architecture source, which only a change made directly in the
database leaves behind; delete those rows on the version still running, then take the census
again:

```sql
delete from components c
where not exists (select 1 from architecture_sources s where s.project_key = c.project_key);
```

## The HTTP API

`GET /api/v1` lists, to any signed-in person or agent credential, every API route with its
method, who may call it, and a description. The
[HTTP API reference](/legion/dispatch/reference/api/) is the same list.
