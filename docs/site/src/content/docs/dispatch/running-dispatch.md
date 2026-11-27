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
| Google sign-in | `DISPATCH_SIGNIN_ISSUER`, `DISPATCH_SIGNIN_CLIENT_ID`, `DISPATCH_SIGNIN_CLIENT_SECRET`, and `DISPATCH_SIGNIN_GROUP` or `DISPATCH_SIGNIN_DOMAIN` | The OpenID Connect issuer people sign in through, Dispatch's client there and that client's secret, and exactly one of: the group a person must be in, through a Cognito sign-in pool; or the Google Workspace domain whose people may sign in, with Google itself as the issuer. Required for the default cookie sign-in. `DISPATCH_SIGNIN_PASSKEY=1` lets the people a project names sign in from outside the domain with a passkey, and `DISPATCH_SIGNIN_WORKSPACE_DOMAINS` names the organization's other domains, whose addresses no person from outside may hold. [Signing people in](#signing-people-in) covers all of it. |
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
organization's own accounts can complete the sign-in at Google. Google accepts an `https` redirect
whose host ends in a public suffix, or plain `http` to a loopback address such as
`http://127.0.0.1:8766/auth/callback` for a server on your own machine. Its client ID and secret
are `DISPATCH_SIGNIN_CLIENT_ID` and `DISPATCH_SIGNIN_CLIENT_SECRET`. Dispatch asks Google for
offline access, with consent on every sign-in, so each sign-in returns the refresh token Dispatch
confirms the person with, and names the domain to Google's account chooser as a hint; the check
that admits a person is Dispatch's own.

Either way, Dispatch keeps the issuer's refresh token, sealed, and confirms the person with it at
least hourly: the first request after the hour refreshes the sign-in and applies the same rule to
the refreshed ID token. A person the rule no longer admits loses every browser session, and so
does anyone whose refresh token the issuer refuses as expired or revoked. Through the pool,
Dispatch then forgets the refresh token; with Google directly, it also holds the person's agent
tokens and broker credentials until they sign in again
([A person Google refuses](#a-person-google-refuses)). A refresh the issuer does not answer, as in
an outage at the issuer, signs nobody out: that request is refused with `503 SIGNIN_UNAVAILABLE`,
and the person's first request after the issuer answers again refreshes and goes through. Logout
ends Dispatch's session only, not the issuer's; with Google directly, Dispatch can keep the
refresh token after it, for these checks alone.

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

Every block below reaches its database with `psql`, given a URL that holds no password and the
password in `PGPASSWORD`, so no password is ever an argument of a command, which every user of the
box can read from `/proc/<pid>/cmdline` while it runs. The URLs are libpq's, while
`BROKER_DATABASE_URL` is the broker's own, pgx's: a deployment sets `pool_max_conns` on it to size
the broker's pool, and libpq refuses a URI parameter it does not know (`invalid URI query
parameter`), so `psql` given that URL reads nothing and the reset
([After a restore](/legion/dispatch/after-a-restore/)) would print `UNKNOWN: the broker database
could not be read` for a reason that has nothing to do with the broker. So make each URL once, from
the server's own with its `pool_` parameters and its password taken out, and read each database's
password from the terminal (Enter alone for a database that asks none), as the blocks name them,
`$DISPATCH_PSQL_URL` with `$DISPATCH_PGPASSWORD` and `$BROKER_PSQL_URL` with `$BROKER_PGPASSWORD`:

```bash
psql_url() {
  case $1 in
    postgres://*|postgresql://*) ;;
    *) echo "psql_url: refused: not a postgres:// URL. A keyword/value string (host=... password=...) would put its password on a command line; give the server's URL form" >&2; return 1 ;;
  esac
  if printf '%s' "$1" | grep -Eq '^[a-z]+://[^@/?]*/[^@?]*@'; then
    echo "psql_url: refused: the URL's user part holds an unencoded /, so the password would stay in it; percent-encode the / as %2F" >&2; return 1
  fi
  printf '%s' "$1" | sed -E 's/(\?|&)(pool_[a-z_]+|password)=[^&]*/\1/g; s/&&+/\&/g; s/\?&/?/; s/[?&]$//; s|^([a-z]+://[^:/?@]*):[^@/?]*@|\1@|'
}
DISPATCH_PSQL_URL=$(psql_url "$DATABASE_URL")
BROKER_PSQL_URL=$(psql_url "$BROKER_DATABASE_URL")
[ -n "${DISPATCH_PGPASSWORD+set}" ] || { read -rsp "Dispatch's database password: " DISPATCH_PGPASSWORD; echo >&2; }
[ -n "${BROKER_PGPASSWORD+set}" ] || { read -rsp "The broker's database password: " BROKER_PGPASSWORD; echo >&2; }
export DISPATCH_PSQL_URL BROKER_PSQL_URL DISPATCH_PGPASSWORD BROKER_PGPASSWORD
```

The block is bash's: `read -rsp` prompts for a password without echoing it in bash, and zsh reads
`-p` as a coprocess instead. Those `pool_` parameters are the only ones the broker's deployment
adds that libpq does not take; a URL without them and without a password is left as it is. A
password the environment already holds in `DISPATCH_PGPASSWORD` or `BROKER_PGPASSWORD`, from
wherever the deployment keeps it, is used as it is, and nothing asks for it. `printf` is the
shell's own, so the URL reaches `grep` and `sed` on their input and is never an argument either.
`psql_url` makes no URL and says why for a server's URL whose password it cannot take out: a
keyword/value string (`host=… password=…`), and a URL whose user part holds an unencoded `/`,
which libpq and Dispatch's own server read as the end of the host rather than part of the
password. Write that server's URL in the form it names, and run the block again.

A revocation settled without the broker, by that command or by a statement written by hand, tells
the broker nothing, and the old account's credentials go on reading secrets there. It also leaves
the broker refusing the account the address came back to, when the broker had recorded that
account replaced: a revocation names the account that holds the address, and the broker stops
refusing that account only when a revocation naming it reaches it. To have the server's loop pay
it after all, which recovers both, record it again, naming the account the address records now
as its holder, with the address as Dispatch holds it, the issuer and the replaced account's `sub`:

```sh
PGPASSWORD="$DISPATCH_PGPASSWORD" psql "$DISPATCH_PSQL_URL" -v ON_ERROR_STOP=1 -v email='<address>' -v issuer='https://accounts.google.com' -v subject='<the replaced sub>' <<'SQL'
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
names that account as the holder, which a sign-in to Dispatch that replaces another account sends,
and so does the account binding's catch-up of a change written by hand. A statement written by
hand that sets `people.sub` back to such an account, rather than the account signing in, records
no revocation itself, but the database keeps the change, as it keeps every change of `sub` that a
writer other than Dispatch's own sign-in makes, and once the account binding holds the revocation
loop catches it up as that sign-in would have. Say A held the address and B took it: writing A back
is caught up by settling every revocation still owed for A, recording the revocation of B naming A
as its holder, which the loop pays, and revoking the address's personal tokens the binding
refuses. Until then B's machine logins and sign-ins stay live at the broker, which serves them
secrets without asking Dispatch, and the broker goes on refusing A; the catch-up waits while a
build without the binding is in the fleet. To recover at once, by hand, do on Dispatch's database
what that sign-in would have done, with the address as Dispatch holds it, the issuer and the `sub`
the statement overwrote: settle every revocation still owed for the account the address records
now (A), as a sign-in that takes an address back does, so the loop never pays one that would end
what A holds; then record the revocation of the account the statement replaced (B), naming A as
its holder, exactly as a sign-in records one. The block locks the address's `user_sessions` row
first, as the one above does:

```sh
PGPASSWORD="$DISPATCH_PGPASSWORD" psql "$DISPATCH_PSQL_URL" -v ON_ERROR_STOP=1 -v email='<address>' -v issuer='https://accounts.google.com' -v subject='<the sub the statement overwrote>' <<'SQL'
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
block). A statement that records an account where the address recorded none overwrote no `sub`;
the database keeps it as a first recording, and once the binding holds the catch-up revokes the
address's personal tokens that recorded another account or none and records a revocation of each
other account, as the first sign-in would have.

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
PGPASSWORD="$BROKER_PGPASSWORD" psql "$BROKER_PSQL_URL" -v ON_ERROR_STOP=1 -v person='<address>' -v issuer='https://accounts.google.com' -v subject='<sub>' <<'SQL'
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
broker database restored alone, with `-infinity` for the backup's moment
([After a restore](/legion/dispatch/after-a-restore/#after-the-brokers-database-restored-or-made-again-alone)).

`envoy_dispatch_broker_revocations_superseded_total` counts each answer `superseded` the broker
gives a revocation still owed, which Dispatch leaves owed, so the owed gauges above show it too,
and logs at ERROR with the address, the replaced account's `sub`, both generations the broker
compared and the address's generation at Dispatch now; the loop tries it again after its retry
window. Alert on the counter being above zero (`envoy_dispatch_broker_revocations_superseded_total
> 0`), not on its `rate()`: it writes no sample until its first count, and any count is a broken
pair. While the two databases agree, Dispatch's loop never hears that answer for a row still owed.
It means the broker applied a later replacement at the address than the one the row names: a
Dispatch database recreated or restored alone, or a generation written by hand at either. Outside
a reset, run the reset ([After a restore](/legion/dispatch/after-a-restore/)). After one, the
reset has already ended every credential the row would end and settled every restored debt, so
settle a later `superseded` row with
`envoy-dispatch clear-broker-revocation '<address>' '<subject>:<generation>'`, naming it as the
command's list prints it.

### A person Google refuses

With Google directly (`DISPATCH_SIGNIN_DOMAIN`, with passkey sign-in on or off), a
person whose sign-in the hourly refresh ends is also **held**: Dispatch refuses their agent tokens,
and the broker tokens that name them, with `401 MEMBERSHIP_HELD`, and the secrets broker refuses
each credential that names them, until they sign in to Dispatch again. A hold revokes nothing.
After the sign-in, every token, machine login and session still within its own lifetime works
again, at Dispatch at once and at the broker within 40 seconds, with no new token, login or
approval. When someone reports `MEMBERSHIP_HELD`, tell them to sign in to Dispatch.

Every refusal of the refresh holds: Google refusing the refresh token (`invalid_grant`, whatever
Google's reason), a refreshed sign-in that names another Google account or another address, one
whose email Google has not verified, and one the rule no longer admits, such as one of your people
whose `hd` is no longer the domain. A person from outside the domain signs in with a passkey, not
with Google ([People from outside the domain](#people-from-outside-the-domain)), so no refresh of
theirs is refused and none of them is held this way: their tokens follow the access lists at each
use.

While Google cannot be reached, nobody is held for 24 hours after their last confirmation; after
that, a person whose refresh still fails is held until a refresh confirms them, and works again
then with no sign-in. A refresh token Dispatch cannot open (one sealed under a key removed from
`DISPATCH_SIGNING_KEY`, say) is the same: served for 24 hours after the last confirmation, then
held until the person signs in again, or the key is back and Google confirms them.

Dispatch checks everyone who has a stored refresh token about once an hour, whether or not they
use Dispatch: its maintenance, once a minute on one server, refreshes up to 20 of the tokens whose
last confirmation or attempt is more than an hour old, and logs
`dispatch: membership pass confirmed=<n> held=<n> unasked=<n> forgotten=<n>` for each
run that checked one. A request with an agent token whose owner was confirmed more than an hour
ago refreshes in that request too, which can add up to 2 seconds to it.

**Signing out.** A sign-out ends the person's browser sessions and changes nothing else they hold.
While they still hold an agent token, a machine login of their own, a sign-in on a machine
(`agent-secrets login`), an entry of the broker's exchange settings that names them, or a pod
account the broker's `BROKER_POD_OPERATORS` maps to them, Dispatch keeps their refresh token after
the sign-out, for these checks alone, so Google's later refusal still holds them; the kept token
signs nobody in. A pod account counts whether or not one of its pods is logged in, since a pod of
it can log in as the person at any time. Dispatch forgets the token at the sign-out when they hold
none of these, and otherwise at the first hourly check after the last one ends. It asks the
broker at each sign-out and at each such check, and while the broker cannot be asked it keeps the
token. With no broker configured, it counts agent tokens alone.

**Who is not checked.** A person for whom Dispatch stores no refresh token and records no hold
keeps their credentials: one who signed out holding nothing that outlives the browser, one whose
sign-in an earlier release ended, and one who never signed in. At every boot Dispatch logs
`dispatch: people census checkable=<n> held=<n> uncheckable=<n>`, and `uncheckable` counts those
people's rows. Read it after the first deploy. A person's next sign-in stores their refresh token,
and they are checked from then on.

**The broker's part.** The broker refuses a held person's credentials only where its
`BROKER_PROJECT_ACCESS_URL` and `BROKER_PROJECT_ACCESS_TOKEN_FILE` are set and Dispatch lists the
broker's account in `DISPATCH_ACCESS_ACCOUNTS`
([A person Dispatch holds](/legion/broker/operate/#a-person-dispatch-holds)). Without the URL the
broker holds nobody and the Envoy listener serves every broker token; Dispatch still refuses a
broker token whose person it has recorded as held.

### Changing the Workspace domain

With Google directly, each sign-in and each hourly confirmation records the Workspace domain its
ID token carried. Dispatch refuses to start when at least one of your people's rows holds a
refresh token and records a domain, and none records `DISPATCH_SIGNIN_DOMAIN`, compared byte for
byte: a mistyped domain would hold every member at their next hourly check. The error, logged as
`dispatch: check the sign-in domain against the people`, names the domain it was given, the
domains the rows record, and `envoy-dispatch forget-hosted-domains`. Correct the setting to the
recorded domain the error names. A row records its domain at its next confirmation, so until
then the check has nothing to compare it with.

For an intended change of domain: stop every Dispatch process; once Google gives the new `hd`,
run `envoy-dispatch forget-hosted-domains` once; then start every process with the new domain. A
process that runs with the old domain after Google's change holds each of your people whose
refresh it runs, and each must then sign in again. If the change also changes your people's
addresses, each credential that names an old address stays held: issue new ones under the new
address.

### Ending a person's access

A hold stops a person's credentials only until they sign in again, and nothing ends them by
itself, so when someone leaves, end what they hold. Behind Identity-Aware Proxy this is also how an
address created again for someone else is confirmed for its new person
([Checking people through the Workspace Directory](#checking-people-through-the-workspace-directory)).
A person holds five kinds of access that outlive the browser, the ones a sign-out asks about:

- **Agent tokens.** Revoke every token of the address on Dispatch's database; each answers
  `401 TOKEN_REVOKED` from then on.
- **Machine logins of their own** and **sign-ins on a machine** (`agent-secrets login`), at the
  broker. Revoke each with the calls the reset's broker block makes
  ([After a restore](/legion/dispatch/after-a-restore/)), filtered to the person: a machine login
  as its approver, and a sign-in as its person. A service's machine login the person
  approved is the service's, and is left.
- **Exchange entries** that name the person: remove each from the broker's
  `BROKER_EXCHANGE_ACCOUNTS`, `BROKER_EXCHANGE_WORKFLOWS` and `BROKER_VM_EXCHANGE_ACCOUNTS`, and
  roll the broker's pods.
- **Pod accounts** that `BROKER_POD_OPERATORS` maps to the person: remove each mapping, and roll
  the broker's pods. From then on a pod of the account acts for no person, and its login is no
  longer counted as the person's machine login.

The block does the first two and then prints what the broker still counts for the person
(`GET /v1/person-access`), with the address as Dispatch holds it and a token of Dispatch's own
ServiceAccount, kept off every command line as the reset's block keeps it. Each call that fails
prints `NOT REVOKED <id>` after the broker's answer:

```sh
(
umask 077
set -o pipefail
person='<address>'
header=$(mktemp) || exit 1
trap 'rm -f "$header"' EXIT
broker_url='<the URL in DISPATCH_AGENT_SECRETS_URL>'
token=$(kubectl -n '<the namespace Dispatch runs in>' create token '<the ServiceAccount Dispatch runs as>' --audience '<the audience in BROKER_K8S_OIDC_AUDIENCE>' --duration 1h) || { echo 'no token was minted'; exit 1; }
printf 'Authorization: Bearer %s\n' "$token" >"$header"
PGPASSWORD="$DISPATCH_PGPASSWORD" psql "$DISPATCH_PSQL_URL" -v ON_ERROR_STOP=1 -v email="$person" <<'SQL' || { echo 'NOT REVOKED: the agent tokens'; exit 1; }
update agent_tokens set revoked_at = now() where canonical_login(owner) = :'email' and revoked_at is null;
SQL
signins() { PGPASSWORD="$BROKER_PGPASSWORD" psql "$BROKER_PSQL_URL" -v ON_ERROR_STOP=1 -At -v person="$person" <<'SQL'
select id from person_signins where person = :'person' and revoked_at is null order by id;
SQL
}
logins() { PGPASSWORD="$BROKER_PGPASSWORD" psql "$BROKER_PSQL_URL" -v ON_ERROR_STOP=1 -At -v person="$person" <<'SQL'
select c.id from launcher_credentials c join credential_requests r on r.id = c.record_id and r.kind = 'launcher_credential' where c.service is null and c.revoked_at is null and r.approver = :'person' order by c.id;
SQL
}
signins | while read -r id; do
  curl -sS --fail-with-body -w '\n' -X POST -H @"$header" -d "{\"person\":\"$person\"}" "$broker_url/v1/person-signins/$id/revoke" || echo "NOT REVOKED $id"
done
logins | while read -r id; do
  curl -sS --fail-with-body -w '\n' -X POST -H @"$header" -d "{\"approver\":\"$person\"}" "$broker_url/v1/launcher-credentials/$id/revoke-by-approver" || echo "NOT REVOKED $id"
done
curl -sS --fail-with-body -w '\n' -G -H @"$header" --data-urlencode "person=$person" "$broker_url/v1/person-access"
)
```

Done is an answer whose counts read
`"machine_logins":0,"signins":0,"exchange_entries":0,"pod_operator_mappings":0`; a count above
zero is what is left, an exchange entry or a pod mapping to remove or a call the `NOT REVOKED`
lines name, so fix it and run the block again. A broker database the block cannot read lists
nothing, so read the last answer, not the absence of `NOT REVOKED`. A machine login revoked as its
approver also ends every session it enrolled.

### After a restore

A Dispatch database restored or made again alone, a broker database restored or made again alone,
and a move between the Cognito pool and a domain each have a procedure of their own, on
[After a restore](/legion/dispatch/after-a-restore/): the five-step reset of every credential a
person holds, the recovery that tells the broker again what Dispatch settled, and the cutover's
statements.

### People from outside the domain

Google sign-in admits only accounts of the domain, and only at `DISPATCH_SERVER_URL`. A Google
account outside the domain is refused there, with a page saying it is not in the domain, whatever
an access list names, and Dispatch's outside address serves no Google sign-in at all, so the Google
client's consent screen stays **Internal**. A person from outside the domain is a **restricted
member**, and signs in with a passkey (below), while a project they are to work on names them: the
project's owner is that email, or its owner put the email on the project's access list. A group on
an access list never admits such a person; only the email itself does.

An address on a domain the organization owns is never a restricted member's. The organization's
domains are `DISPATCH_SIGNIN_DOMAIN` and those `DISPATCH_SIGNIN_WORKSPACE_DOMAINS` lists,
comma-separated, such as `example.net,example.org`, each with its subdomains. Write an
internationalized domain in Unicode, such as `bücher.example`, or as its A-labels,
`xn--bcher-kva.example`: the two are the same domain, and an address written either way is on it.
List every other domain the organization owns: then nobody from outside can hold one of your
people's addresses, even before your person ever signs in. Your own people sign in at an address
on any of them as before. On a domain the organization owns that the setting does not list, a
restricted member still never takes the address of one of your people once they have signed in
with it. A restricted member who held such an address before your person first signed in loses it
at that first sign-in: Dispatch signs them out and revokes every agent token they made, so none
of them acts as your person.

Dispatch checks again at least hourly, as for everyone, and signs the person out once no list
names them, once their address is on a domain the organization owns (including one you add to
`DISPATCH_SIGNIN_WORKSPACE_DOMAINS` later), or once the deployment admits nobody from outside.
The sign-in page they then reach says why: that no project's access list names their address any
more, that their address signs in only as an account of the organization, or that Dispatch no
longer signs in people from outside the domain. The API's `401 NO_IDENTITY` says the same, as a
`reason` of `NOT_LISTED`, `WORKSPACE_ADDRESS` or `OUTSIDE_SIGNIN_OFF` beside their address as
`login`. A person who signs out, or whose sign-in simply expires, is told nothing more, and a
deployment that never admitted a restricted member answers as it always did.

The agent tokens such a person makes in **Settings → Agent tokens**, and the tokens the secrets
broker issues to their agents, follow the very rule that admits the person, checked at every
request rather than hourly: a token stops at once when the deployment admits nobody from outside,
when its owner's address is on a domain the organization owns (including one you add to
`DISPATCH_SIGNIN_WORKSPACE_DOMAINS` later), or when no project names its owner any more, and the
answer is `401 TOKEN_OWNER_NOT_ADMITTED`, saying which. None of these revokes the token, so it
works again once all hold again; only another account's sign-in to the address revokes the
Settings tokens, as above, after which each answers `401 TOKEN_REVOKED`. Your own people's tokens
are not affected.

People from outside reach Dispatch at its second address, `DISPATCH_OUTSIDE_ORIGIN`, such as
`https://dispatch-outside.example.com`: an `https` address with no path, on another host than
`DISPATCH_SERVER_URL`. Under Google sign-in its host must be a DNS name, not an IP address, whose
top-level domain is on the public suffix list, because that host is the relying party of every
passkey enrolled there, which must be a registrable domain name; Dispatch refuses to start with any
other. A request to that host is answered as that address, and a write from a page there must come
from that address. There `/auth/start` and `/auth/callback`, Google's sign-in, answer `404`, with
passkey sign-in on or off, so the Google client lists no callback of the outside address, only
`<DISPATCH_SERVER_URL>/auth/callback`. A link to a Dispatch page written on either address opens on
the address the reader is on.

### People from outside with a passkey

`DISPATCH_SIGNIN_PASSKEY=1` lets a person from outside sign in with a passkey and no Google account
at all. It needs `DISPATCH_SIGNIN_DOMAIN` and `DISPATCH_OUTSIDE_ORIGIN`: Google signs in your own
people alone, and a person from outside signs in only at the outside address, where Dispatch serves
no Google sign-in. So the Google client lists no callback on the outside address, only
`<DISPATCH_SERVER_URL>/auth/callback`, and its consent screen's audience stays Internal. It also
needs `DISPATCH_TRUSTED_PROXY_RANGES`, the load balancer's ranges whose `X-Forwarded-For` Dispatch
reads to limit passkey sign-ins per client address: behind Google's load balancer,
`35.191.0.0/16,130.211.0.0/22,2600:2d00:1:1::/64`
([Google's firewall rules](https://cloud.google.com/load-balancing/docs/firewall-rules)). A
project's owner puts the person's email on the project's access list, then makes an
invitation link for them on the project's **Access** tab and hands it over however they like. The
link works once, within 24 hours, for that person only, and making a new one ends the old one. With
it the person enrolls a passkey on their own device, and from then on signs in with that passkey.
The owner sees on the Access tab when each passkey was enrolled and who made the link it came
through, so a link someone else used first shows up there. One owner binds a person from outside:
a passkey signs its person in to every project whose list names them, so an owner makes a link
only for a person no other owner's project names, the enrollment checks that again, and no other
owner can name a person on their list, or be given a project naming them, while a passkey another
owner's link enrolled stands. The Access tab names the people another owner's project names, and
offers no link for them; that block clears when the other project's owner takes the person off
its list, or an operator does. **Reset** ends the person's sign-in, deletes their passkeys,
revokes the agent tokens they made and the machine logins they approved at the secrets broker,
ends every link still waiting for them on any project, and keeps them on the list, so they enroll
again with a new link, under a new account. Taking the person off the last list that names them
ends their access the same way at once, and so does Dispatch's check, at least hourly, of a person
no list names any more, so whoever names the address later names a new account. No link is made
for an address on a domain the organization owns (`DISPATCH_SIGNIN_DOMAIN`, a domain
`DISPATCH_SIGNIN_WORKSPACE_DOMAINS` lists, or a subdomain of one), nor for one of your people who
has signed in with Google. List every domain the organization owns there: a domain left out is not
the organization's to Dispatch, so an owner can make a link for an address of one of your people
there before they first sign in with Google, and the passkey enrolled with it keeps signing in as
that address after your organization removes that person, since no passkey sign-in asks Google.
While the setting is off, Dispatch refuses every such person's sign-in at their next request,
telling them Dispatch does not sign them in now, and refuses their agent tokens as above; turned on
again, their passkeys, sign-ins and tokens work again, so end a person's access for good with
Reset or by taking them off the lists. The outside address never offers Google sign-in, with
passkeys on or off.

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

### Checking people through the Workspace Directory

IAP keeps no refresh token, so Google's refusal of a refresh cannot reach Dispatch. Instead,
Dispatch reads each of your people's Workspace accounts through the Directory API (`users.get` for
the person's address) about once an hour, held and idle people included. An answer that the
person's own account is active confirms them, releases a hold with no sign-in, and keeps them
served for 24 hours. An answer that the account is suspended or archived, a `404`, or an answer
for another address's account (the address made an alias of a colleague's account) holds them:
their agent tokens and broker tokens are refused `401 MEMBERSHIP_HELD`, and the secrets broker
refuses their credentials. Any other result changes nothing. A valid IAP assertion releases
nothing. After 24 hours with no active answer, a person's agent tokens answer
`503 IDENTITY_UNAVAILABLE`, and their broker tokens `503 BROKER_UNAVAILABLE`, until the Directory
answers again, so an outage of the Directory longer than a day stops everyone's tokens; a person
IAP admits for the first time is in that state until their first check.

An address the Workspace deleted and created again for someone else answers with the new
account. Dispatch's row for the address still belongs to the earlier person, and so does every
credential that names it, so the new account is confirmed only once the address holds nothing that
outlives the browser, the same question a sign-out asks: no unrevoked agent token, and at the
broker no live machine login, no sign-in on a machine, no exchange entry that names it and no pod
account `BROKER_POD_OPERATORS` maps to it. Until then, or while that question cannot be answered,
the address stays held. End the earlier person's access
([Ending a person's access](#ending-a-persons-access)), and the next hourly check confirms the new
person, with no sign-in and no command. A Dispatch with `DISPATCH_AGENT_SECRETS_URL` unset asks no
broker and counts agent tokens alone, so there the new account confirms with no check of what the
earlier person still holds at the broker.

Two settings configure the reads, both required with `DISPATCH_IDENTITY=iap` and refused without
it: `DISPATCH_DIRECTORY_ACCOUNT`, the email of a service account with no key and no project role,
on which Dispatch's own workload identity holds `roles/iam.serviceAccountTokenCreator`, so
Dispatch signs through it and no key leaves Google; and `DISPATCH_DIRECTORY_ADMIN`, the email of
the Workspace user the reads act as. Enable two Google APIs in the Google Cloud project that holds
that service account: `iamcredentials.googleapis.com`, whose `signJwt` Dispatch calls on the
service account, and `admin.googleapis.com`, which serves the Directory's `users.get`. A server
that cannot build its reader, with no Application Default Credentials say, logs
`dispatch: build the request identity` and exits at boot. The secrets broker of such a deployment
sets `BROKER_HOLD_REQUIRES_DEADLINE=true`
([A person Dispatch holds](/legion/broker/operate/#a-person-dispatch-holds)). On `envoy-infra`'s
`gcp:Dispatch`, set `directoryAccount` and `directoryAdmin` beside `iapAudience`
(`packages/envoy/infra/README.md`).

Before IAP admits anyone, a Workspace super administrator makes two changes once: give the
delegated user an administrator role whose only privilege is Users > Read; and, in the Admin
console under Security, Access and data control, API controls, Manage Domain Wide Delegation,
authorize the service account's numeric client ID for exactly
`https://www.googleapis.com/auth/admin.directory.user.readonly`. Google states that the grant can
take up to 24 hours to take effect. Then run, with the two settings, an active account and a
suspended, archived or deleted one:

```sh
envoy-dispatch directory-check --active '<an active login>' --held '<a suspended, archived or deleted login>' --scope-probe https://www.googleapis.com/auth/admin.directory.user
```

It exits 0 only when the first login is `Active`, the second `Held`, and Google refuses the probe
scope with `unauthorized_client`, so the grant reads users and gives nothing more; the command's
section of `packages/envoy/cmd/dispatch/README.md` ("Checking the Directory reads") gives its
output. A run that fails at the token exchange soon after the grant may pass later.

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
   there. The migrations also need `btree_gin`, a contrib extension that PostgreSQL includes, and a
   migration creates each of the two extensions. A `dispatch-pg` made from another image or
   PostgreSQL major is refused, with the commands that dump its databases and restore them into a
   new container on the right one.

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
- **Projects → Default assignee**: name the person each project's unowned work lands on, in the
  projects table's own column. An issue created with no assignee, by an agent on the shared token
  and under no parent that names one, goes to that person, and so does every question on an issue
  nobody holds. Every project names one, so the page moves a default from one person to another
  and clears none. A person moves it here, and an agent names a project's first one in the call
  that creates the project.
- **Projects → Default repository**: name the repository each project's work is done in, in the
  projects table's own column, as `owner/name`. An issue that names no repository, under no parent
  that names one, takes it. **Save** sets it, **Clear** removes it, and a project with none reads
  **None**. An import of a GitHub issue names its project, since Dispatch chooses no project from
  a repository.
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

What each credential sees. A credential's list of projects decides only which private projects it
sees: a private project needs a token that lists it.

- A person at the browser sees every project the rule above lets them see.
- A full member's agent token, a personal `dsp_` token from Settings or an access token the
  secrets broker issued, sees every public project its person sees, whatever it lists, and the
  private projects it lists that its person sees. It reads what belongs to no project as its
  person does, but for the sessions and roles of private projects it does not see. A token that
  lists projects is refused a write of the whole deployment, such as a new project, a repository
  mapping or a broadcast, `403 PROJECT_SCOPE_DEPLOYMENT`; a broker access token acting as its
  person is refused it only while some project is private.
- A restricted member's token, their personal token or one the secrets broker issued for their
  agent, holds no scope and lists exactly one project; one of theirs that lists none or several
  acts for nobody, `403 RESTRICTED_MEMBER`. It sees its project if its person sees it, and
  nothing else. Of what belongs to no project it reads only the people and sessions of that
  project, its own, and its person's direct conversations with those sessions. Their browser's
  mint in Settings gives a personal token that shape, and a stored one holding a scope acts with
  none.
- A credential that acts for no person, the shared `DISPATCH_AGENT_TOKEN` or a Kubernetes
  service-account token, sees the public projects and the private projects its list names.

With no private project in the deployment, a full member's token sees every project its person
sees, whatever it lists.

A restricted member's dashboard holds only what they may read. Their rail shows Inbox, Agents,
Delivery, their projects and Settings, with no Priorities or Pipeline and no Peek. Delivery shows
them the pull requests whose issue they see and the deploys that shipped them, with no run's jobs,
no setup form and no session-groups editor. The Priorities, Pipeline and broadcasts pages answer
**Page not found**, and Settings shows no Groups and no Delivery timeline. The machine-login page
is theirs, listing their own machine logins alone: there they approve, at their browser, the device
login of their own agents, and list and revoke their own machine logins, and Dispatch decides or
ends for them no other credential. The API refuses them, and every token they make, every read of
the whole deployment, `403 DEPLOYMENT_READ_REFUSED`, and every write of it, `403 RESTRICTED_MEMBER`,
but for the machine-login calls at their browser, and except that they revoke their own credential
grants and end their own sign-ins, as a full member does: the secrets broker decides whose each one
is. The machine-login calls are theirs at their browser alone: through a token of theirs each one,
the list `GET /api/v1/machine-logins` included, is `403 OAUTH_SCOPE_REQUIRED` where the token lacks
the call's scopes, as their agent's token does,
and `403 RESTRICTED_MEMBER` otherwise. The secrets broker holds their agents' tokens to one
project, which their agents name in `AGENT_SECRETS_PROJECTS`, and a token the broker scoped to
their person gets no scope at Dispatch. Taking such a person off a project's list ends their tokens
for it within 325 seconds, since the broker checks every trade against Dispatch. Every removal of
them from a project's list also has Dispatch revoke all their machine logins at the broker, whatever
other list still names them, after the removal commits and again until the broker confirms; the
log line `the secrets broker revoked the machine logins of a restricted member taken off an access
list` records each. Until it is done, about a minute after the removal (the broker may still commit
an approve it took before it), their approve of a new machine login is
`409 MACHINE_LOGIN_REVOCATION_OWED`; a person another list still names then approves a new login
for the projects that list them.

A private project's Legion daemon needs a `dsp_` token that lists the project: the shared token and
a token that does not list it read the private project as missing. A person who sees the project
makes it in **Settings → Agent tokens**, picking the project under **Projects**, and the daemon
reads it from the file its `dispatch_token_file` names. The token lists every private project in
the daemon's `projects`; it sees each public project whatever it lists. Every write of the
daemon's agents is then attributed to the person who made the token.

A project can be made private only once the deployment names it, in one of two settings.
`DISPATCH_PRIVATE_PROJECTS` holds, comma-separated, the keys of the projects whose private
namespace, stores and grants the deployment has made; their switch to private is their full
switch. `DISPATCH_PRIVATE_BEFORE_FULL_SWITCH` holds, in the same form, the keys of the projects it
makes private before their full switch (below). On a deployment built by `envoy-infra`'s
`gcp:Dispatch`, the component's `privateProjects` and `privateBeforeFullSwitch` inputs, each a
list of keys, render them. An entry that is not a project key, the old value `1` included, is
ignored, and the start logs an ERROR line naming it. With both unset or empty, no project can be
made private.

The owner switches a project private on its Access tab
(`PATCH /api/v1/projects/{key}/access` with `{"visibility": "private"}`), and Dispatch checks, in
this order:

1. With both lists empty, every switch to private is `409 PRIVATE_PROJECTS_OFF`, before anything
   is read.
2. A caller who does not see the project is `404 PROJECT_NOT_FOUND`, as for a key no project
   holds, and one who sees it but does not own it is `403 PROJECT_OWNER_REQUIRED`.
3. A key neither list names is `409 PRIVATE_PROJECT_NOT_PROVISIONED`.
4. A store that cannot hold the project private yet is `409 PRIVATE_PROJECTS_INCOMPLETE`, naming
   what repairs it: each row of the project that has no node naming its project, with the cure
   the project census prints for it (`envoy-dispatch backfill-graph-store`, or none for a node
   that names another project, which a person compares with its row),
   `envoy-dispatch backfill-session-projects` until the sessions' projects are filled, and
   `envoy-dispatch scrub-access-events` until the stored access events are scrubbed.
5. Last, for a project's full switch, in the same transaction, Dispatch writes the project's key
   in the hub's `envoy_private_projects` bucket, the record by which Envoy's listeners are to hold
   the project's messages back from a token that does not list it. When the hub refuses that write,
   as a hub without its auth service does, or Dispatch has no bus to write it to, the switch is
   `409 PRIVATE_PROJECTS_UNAVAILABLE` and nothing changes. This release of Dispatch has no writer
   of that bucket, so every full switch that passes steps 1 to 4 is
   `409 PRIVATE_PROJECTS_UNAVAILABLE`: no project takes its full switch until a release carries the
   writer.

Of these, only the owner check of step 2 applies to a switch to public.

A project private before its full switch is the outside person's project on their first day. Its
switch to private skips steps 3 and 5: it needs no provisioning and writes no record, while step 4
still applies. In its own reads Dispatch then hides it from every person and credential that does
not see it, as any private project, while its namespace, its stores and its bus gate come with its
full switch. Until then, the project's notifications on the hub reach any agent session that
subscribes to them. So nobody takes it for a fully switched project, its access carries
`"private_before_full_switch": true` while it is private, the server logs at INFO, at start, each
such project as private before its full switch, and `/healthz` says one exists (below). A key both
settings name takes its full switch. From the apply that adds such a key to
`DISPATCH_PRIVATE_PROJECTS`, its access, the start's lines and `/healthz` no longer say its project
is private before its full switch, though that switch has not run.

A project the database holds private that neither list names, because its key was taken off
the lists or the lists were emptied, stays private. Nothing of it reaches anyone who could not see
it before, and every change of its visibility but its owner's switch to public is refused.
Dispatch still starts and serves every other project: it logs an ERROR line naming the project and
both repairs at start and at every maintenance pass, once a minute, and `/healthz` answers
`degraded: ["private_project_unprovisioned"]`, naming no project. The owner decides the repair: put the key back on either list, or
make the project public, from the Access tab or, with the database credential alone and no server
running if need be, with:

```sh
envoy-dispatch project-visibility <KEY> public
```

So to take a key off both lists, make the project public first, and only then remove its key. At
start Dispatch also logs, at INFO, the listed keys no private project holds yet. With either list, each
server logs an ERROR line as it starts, and then once an hour, while the store cannot hold a
private project for one of the reasons of step 4, naming the reason and what repairs it. Every
response carries `Dispatch-Private-Projects: 1` while either list names a project, the database
holds a private one, or the server's last read of the database's private projects failed, and
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
- `degraded` lists fixed reasons, as the server's last read found them: it reads every 30 seconds,
  so a probe adds no read. `private_project_unprovisioned` is there while the database holds a
  private project that neither list names, and `private_before_full_switch` while it holds a
  project private before its full switch ([Private projects](#private-projects)). Neither names a
  project or counts one, because the check answers anyone. The project's key is in the line the
  server logs at start, so a project made private after the start is named at the next one; an
  unprovisioned one is named in every maintenance pass too. It is absent when neither holds, and
  it never decides `ok` or the status.

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
  be listed), a project's default repository that the App is not installed on, a pull request head it
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
shared agent token and a full member's credential that lists projects among them, and a
restricted member's credential, or one acting for one, is refused `403 DEPLOYMENT_READ_REFUSED`.
A caller with no credential is refused `401`, whatever
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
to 64 connections for requests, up to 27 for the work it runs once across the servers, the
delivery intakes' holds and the keyword fill's hold,
up to 8 for saving what people type in documents, up to 4 for loading
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
goes out in the transaction that writes the edit, and needs no connection of its own. That is 118
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
  `gone`; an `orphaned` line says `no in-band cure, unless an older build's removal made it during a two-build roll: this build's block-index tick then states that node gone`.
- A comment that a server of the earlier version writes during the rollout, and a document block
  it indexes then, can keep a record that names no project: `envoy-dispatch census --store` then
  names it `unscoped`. Once every server runs this version, run
  `envoy-dispatch backfill-graph-store` again, which gives it its project.
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
again. Rolling it back brings the hazard back the other way: a tab still open, and every
`dispatch` client, which sends a failed write again under its key and asks nothing first, retry a
write whose answer was lost against the earlier server, which runs it again. So fix it forward,
as above; if you must roll back, stop this version first, knowing a write in flight at that
moment may land twice. Rolling back to a build whose keyed layer passes a caller it cannot
resolve on to the handler, rather than answering that caller itself, brings the double write back
too, for as long as that build serves: a `dispatch` command that sends a failed write again
without reading `/healthz` can have that build run it once with no key and again under its key.
Once your sessions run such a command, do not roll Dispatch back past `b1c42d1d7d` (the first
build whose keyed layer answers that caller itself, cybermonklabs/legion#4350); fix forward instead.
Once a later release has dropped the broadcast send's old key table (migration `0141`),
rolling back past this version also stops broadcasts: the earlier version's broadcast send reads and
writes that table, so every broadcast send fails while the rest of that version keeps working. A tab opened
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

### Backfilling the graph store

After a rollout whose release notes ask for it, run the graph-store backfill once, where the
server runs, as you run its census:

```sh
DATABASE_URL='<your database URL>' envoy-dispatch backfill-graph-store
```

It writes the store's record of every row an earlier version wrote without one, and the project
of every comment and document block that an earlier version recorded with none. It works in
batches, one transaction each, and it is safe to stop and run again: a second run starts where
the first stopped.

The run throttles itself, so that the searches and pages people load keep the database's CPU
they need. Each pass reads and writes batches of 100 rows and, after each batch, pauses until
that batch has taken a fifth of a second, so it reads at most 500 rows a second. Two flags, each
with a setting the Kubernetes Job that runs the backfill can set instead, change that; a flag
wins over its setting:

- `--batch-rows` (`DISPATCH_GRAPH_BACKFILL_BATCH_ROWS`, default `100`): the rows each batch reads
  and each transaction writes, 1 to 1000.
- `--rows-per-second` (`DISPATCH_GRAPH_BACKFILL_ROWS_PER_SECOND`, default `500`): the most rows a
  second each pass reads. `0` sets no bound and pauses nowhere. Use it only on a database nobody
  is using, such as a copy or a test database: it is never the default.

A value that is not a whole number, a batch outside 1 to 1000 and a rate below 0 stop the command
with exit 2 before it opens the database.

The defaults come from production's first run of the backfill, on 2026-11-24, which had no pause
and batches of 200 rows. It wrote 2,676,178 document blocks in 31 minutes, about 1,400 rows a
second. The database's CPU went from 37-48% before the run to 52, 69, 75 and 66% during it, and
18.2% of searches failed with a statement timeout, against 0.8% before it. At 500 rows a second
the run's connection is busy about a third of the time (500 of 1,400), so it adds about a third
of that CPU, and a batch of 100 rows holds its locks half as long. A pass over 2.7 million rows
then takes about 90 minutes.

The first line of the run names its throttle, such as
`backfill-graph-store: throttle batch=100 rows_per_second=500`. While a pass runs, it prints a
line every 30 seconds, and once more when it ends:

```text
backfill-graph-store: pace kind=block rows=15000 batches=150 seconds=30.2 rate=497.2 paused=14.7 done=false
```

`rate` is the rows a second since the pass began, and `paused` the seconds it has spent in its
pauses. A `rate` near the limit with `paused` above 0 is the throttle at work; `paused=0.0` with
a lower `rate` means the batches themselves take longer than their share, and the database is
already the bound. Each pass then prints a `kind=` line with what it wrote, and the command exits
1 when it left a row undone.

## The HTTP API

`GET /api/v1` lists, to any signed-in person or agent credential, every API route with its
method, who may call it, and a description. The
[HTTP API reference](/legion/dispatch/reference/api/) is the same list.
