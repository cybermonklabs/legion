# broker

The secrets broker: it enrolls agent sessions, decides their secret requests from each secret's
owner and tier tags or by a person's approval on its own approval page, where the person signs in
to the broker, records every request and decision, and releases granted values.
The broker's documentation, generated reference included, is the "Secrets Broker" section of the
docs site, built from `docs/site/src/content/docs/broker/`.

## Build

```sh
cd packages/envoy
go build -o envoy-broker ./cmd/cipherd
```

The Envoy image (`packages/envoy/docker/Dockerfile`) ships it as `envoy-broker`.

## Run

The broker takes no flags and reads its configuration from `BROKER_*` environment variables
(`internal/cipher/config/config.go`). It needs Postgres and the agent secrets in its secret store
(`BROKER_SECRETS_STORE`: AWS Secrets Manager, or Google Cloud Secret Manager under Application
Default Credentials): every secret under `BROKER_SECRETS_PREFIX`, carrying `owner` and `tier` (AWS
tags, or Secret Manager annotations) and encrypted with the agent-secrets key
(`BROKER_SECRETS_KMS_KEY_ARN`, or `BROKER_SECRETS_KMS_KEY_NAME`). It also needs its own OpenID
Connect client, through which a person signs in to the approval page: `BROKER_SIGNIN_ISSUER`,
`BROKER_SIGNIN_CLIENT_ID`, `BROKER_SIGNIN_CLIENT_SECRET_FILE` and `BROKER_SIGNIN_DOMAIN` are all
required, and a broker missing any of them refuses to start, naming the missing ones.
`GET /signin/status` reports what that sign-in has done, for an operator or a production check.
The one broker that starts without them is one with `BROKER_SIGNIN=off`, which signs nobody in:
it serves no approval page and denies at once every request a person would have to approve,
while everything the policy grants at once is granted as ever.
The docs site's "Operating the broker" page
(`docs/site/src/content/docs/broker/operate.md`) gives a minimal production configuration and
everything the broker depends on, and its generated configuration reference lists every variable
with its default.

`envoy-broker` with no argument serves.
It takes three subcommands in place of serving; the third, `envoy-broker versions`, is in the
operate page's "A migration recorded under another file".
`envoy-broker end-sessions <login>` ends every browser session that login holds on the approval
page and prints how many it ended;
it reads the same configuration, binds no listener and applies no migration.
`envoy-broker profiles check <file> [--against <old file>] [--secrets-json <file>] [--private-upstream '<host>=<url> ...']`
reads a profiles file with the broker's own code and exits 1, naming the kind or the profile and
the field, or the parse position, when the broker would not serve that file. A `proxy` block of
any kind but `dpop` is refused, naming `kinds`, which replaced it.
On a file it would serve it prints every field of each kind and each profile the file adds and
removes against the file `--against` names, and each field one both files hold changes: a kind's
hosts, headers, variables, ways, `allow` rows, `placeholder_prefix` and `max_grant`; a profile's
source, tier, requesters and `maxGrant`, the kind its `proxy` key names as a string, its own `allow`
rows, its GitHub accounts, repositories, permissions and value, its Google scopes, and a `dpop`
block's hosts, headers, `allow` and `kind`. Who decides a request is no field of the file: it is
the source key's `judge` and `guidance` tags.
So a pull request changing the deployment's `profiles.json` leaves a reviewer only the question of
whether each of those is right.
It also refuses each fault for which the policy load would refuse a name behind the proxy, with
the load's own rules and in its words, one line each, `profile "<name>": <reason>: <detail>` or
`secret "<name>": <reason>: <detail>`: a `proxy` string that names another kind than its source's
`proxy` tag, a source only the proxy scopes or an `allow` behind no kind, a way or a `max-grant`
the kind does not take, an `allow` whose source lacks `proxy-narrowed=true`, a row on a host the
kind does not bind, rows outside the kind's `allow`, a tag naming no kind, the narrowed rule
(`Profiles.NarrowedFaults`) for a secret tagged `proxy-narrowed=true`, and a profile whose source
the load does not serve: refused, absent, or another profile, which is never a source. It reads the
store's tags and not its values, so a source the load refuses for its value, or one still waiting
for its value, reads as a served one. A profile's kind is its own `proxy` string, or else its
source's `proxy` tag, which is in the secret store, not the file: `--secrets-json` reads the tags
from the output of
`gcloud secrets list --filter='name~agent-secrets-' --format='json(name,annotations)'`. Without it,
a profile with a `proxy` string is held to all the file decides of it, and each profile with none,
in a file that declares a kind, gets an `UNKNOWN` line naming it; a run that refuses nothing then
exits 3 after the whole report, so a caller that cannot read the store can accept 3 and still fail
on 1.
With `--against`, it lists under `widens` each row of a name's `allow` under the file that its
`allow` under the old file does not cover, for each name behind the proxy under both files (its
profile's own `allow`, else its kind's, else its proxy block's; a missing `allow` allows every
method and path), and each host the change adds to a name. A widening exits 0: the reviewer reads
it, and a gate that must see none reads the list. A name whose `allow` or hosts only the tags give
is listed as not compared. Without `--secrets-json`, a stored secret behind a kind by its own
`proxy` tag cannot be named, so each kind both files declare is compared on its own instead
(`widens kind "<kind>": <row>`, `hosts kind "<kind>": <host> added`). Each of these makes a run
without `--secrets-json` exit 3.
With `--secrets-json`, it lists each secret's `proxy` and `proxy-narrowed` tags, then each name
behind the proxy under the old file and how the file serves it, and exits 1 when a name leaves the
proxy. The old file may be in either form, kinds or the proxy blocks they replaced.
A profile whose proxy block is of kind `dpop`, a capability the broker mints and holds, is held to
the boot's rules for it: one exact host, a non-empty `allow` list none of whose entries is every path
(`""` or `/`), the `Authorization` header alone, and no kind that binds that host.
Its host must also be one private destination's, which only the broker's own
`BROKER_PROXY_PRIVATE_UPSTREAM` names: `--private-upstream '<host>=<url> ...'` checks that too, and
without the flag each such profile gets one line saying its tie to the destination was not checked.
`profiles check` reads no configuration, opens no database and binds nothing.
The removed `envoy-broker airlock-check` exits 2 naming `profiles check`.

For a local stack (Postgres in Docker, or one you run named by `DEV_BROKER_POSTGRES_URL`, a fake
secrets file standing in for Secrets Manager, and the clients), run
`packages/envoy/scripts/dev-broker.sh`.

## Google groups

A secret's `owner` tag, and any entry of its `requesters` tag, may name a Google group as `group:`
and the group's lowercase email (`group:approvers@example.com`); an entry that starts `group:` and
holds no lowercase email is refused, as a malformed owner or requester is. Each member of an owner
group decides the secret's requests on the approval page, which Dispatch's Inbox lists for each
member, who may dismiss one once it expires. A member the group's last good read listed, however
old that read is, lists and revokes the grants the group approved, as the one owner of a person's
secret does. No rule stops a member approving a request their own session made, as a sole owner
may approve their own: a group owner is not a second person. An agent-tier secret a group owns
goes to a member's own sessions at once and waits for a member's approval from any other session.
A group in `requesters` admits its members' sessions. A list, revoke or dismissal that names a
group as its approver, rather than a member, is refused `400 APPROVER_IS_GROUP`.

The broker reads the members of each group a served secret names with the Directory API's
`members.list`, nested members included (`internal/cipher/groups`): a group the five-minute policy
load names that it has not read yet, at once, a group whose first read failed included, and every
group once an hour after that. At start, the pod waits up to 15 seconds for its first reads before
it serves. A reread of one secret names no group to read, so a `retag` that names a new group
admits nobody until the next five-minute load reads it. A read that fails keeps the last good
members. A group whose last good read is more than 2 hours old, or that was never read, admits
nobody: no member decides, no session gets its agent-tier secret at once, and a grant a member
approved, or an automatic grant of a member's session, releases nothing from its next release. A
grant the model judge approved for a group's secret rests on no member, so it keeps releasing, and
the judge keeps deciding new requests for that secret, while the profiles in force give the judge
that secret. A `requesters` entry naming such a group cannot say who is on the list, so a
request it decides is denied `requester-group-unread`, a live grant it decides is paused, not
revoked, and releases again once a read lists the person, and an approval of a pending request
waits rather than being recorded. A member the group's last good read listed may still revoke the
group's grants while it is stale, since a revoke only takes power away. Each read writes one log
line, `group members read`, with
`group`, `members` (how many members the broker holds for the group after the read) and `result`:
`ok` at INFO, or the cause at WARN, with Google's reason where Google gave one, and a failed call
named by its class alone. The Directory and IAM calls follow no redirect.

The broker reads the members as a reader account, which `BROKER_DIRECTORY_ACCOUNT` names: a
user-managed Google service account that holds Workspace's Groups Reader role and no other, and on
which the broker's own credential holds `roles/iam.serviceAccountTokenCreator`. The broker mints
that account's access token at IAM's `generateAccessToken` with the one scope
`admin.directory.group.member.readonly`, and sends it to the Directory API alone, which refuses
the pod's own token. Unset, the broker reads no group, each read logs
`result="no directory account"`, and every group admits nobody. The broker refuses to start on an
account that `BROKER_SERVICE_ACCOUNT_SECRETS` binds to a secret, since a grant of that secret
would release the token the group read uses. `BROKER_DIRECTORY_URL` is the Directory API's base
URL, Google's `https://admin.googleapis.com` when unset; a test names its stand-in there. It is an
https URL, or http to a loopback host for a run on one machine, since the reader account's token
goes to it.

A grant a member approved records the group as its approver and the member as the person who
decided it, and it releases only while the group admits that member. When the group does not, the
release is refused, and the broker writes one WARN line, `group approval lapsed`, with `grant`,
`record_id`, `secret`, `decider`, `group`, `reason` (`decider is not a member now`) and `at`
(`values`, `airlock` or `reuse`). Where the group is stale or was never read, that is a lapsed
grant: the session is answered `GRANT_NOT_LIVE`, and no other line is written. Where the group was
read in the last 2 hours and does not list the decider, that is a broken approval chain: the
session is answered `403 GRANT_CHAIN_INVALID`, and the line
`broker: a grant's approval chain does not verify; it releases nothing` is written beside it.

## Anthropic federation grants

An `anthropic-federation` secret gives a session an Anthropic access token for one organization,
which the broker gets by workload identity federation as a Google service account
([Anthropic's reference](https://platform.claude.com/docs/en/manage-claude/wif-reference)). Its
stored value is a JSON object: `organization_id` (a UUID), `federation_rule_id` (`fdrl_…`),
`service_account_id` (`svac_…`), `service_account`, the Google service account the broker mints
as (`<id>@<project>.iam.gserviceaccount.com`), and `workspace_id` (`wrkspc_…`), which the value
holds only where the federation rule is enabled for more than one workspace.
`BROKER_SERVICE_ACCOUNT_SECRETS` binds each such secret to one account, as it binds a
`google-service-account` secret: a secret it binds to no account, or whose `service_account` names
another one, releases nothing, and the broker logs `class=binding`. A release mints a new Google
identity token of the bound account for the audience `https://api.anthropic.com` at IAM's
`generateIdToken`, with the account's email in it, and trades it at
`POST https://api.anthropic.com/v1/oauth/token` (the jwt-bearer grant, with the value's ids, and
`workspace_id` only where the value holds one). It refuses a token that does not start
`sk-ant-oat01-`. Each release reads an identity token for its own exchange, which it sends once and
never again: the identity token may be good for one exchange only, so the next release reads a new
one and asks again. A failed mint is logged `class=refused` when Anthropic refused it (its
`401 authentication_error` when no rule admits the token), `class=stored-value` for a value the
decoder refuses, and `class=upstream` for any other failure. The federation rule's
`token_lifetime_seconds` must be 3600 or less: a grant of the secret lasts an hour at most, so the
broker refuses a token that lives longer, `class=refused`, naming that setting. The minted value
expires at the token's own end, which the airlock's proxy reads to mint again five minutes before
it, as it does for every minted value; the release's `expires_at` is the grant's end. A rule that
sets five minutes or less works, but every proxied call then mints again. The secret is human tier
alone (the broker refuses one at agent tier as `type-human-tier`), and a person approves every
release of it: the model judge decides none, whatever seconds its `judge` tag gives the judge, as
for a secret tagged `admin`. It takes a `max-grant` of an hour or less, names its owner in
`requesters` as a `google-service-account` secret does (one person, or one group, beside
registered services), may carry `admin: true`, and carries no `scopes`.

Behind the airlock, such a secret is served by the `anthropic` kind: its `proxy` tag names the
kind, which binds `api.anthropic.com`, the header `Authorization`, the way `mint`, a `max_grant`
of `1h` and an `allow` of `GET`, `POST` and `DELETE` under `/v1/organizations/`, so its token
reaches Anthropic's Admin API alone and never the token exchange or the Messages API on the same
host. The kind also declares the variable `ANTHROPIC_ADMIN_TOKEN`, which no release applies yet: a
command reads the placeholder in the variable named as its secret. A secret narrowed further
carries `proxy-narrowed=true` and one profile with an `allow` of its own inside the kind's. The
local stack mints at stand-ins:
`BROKER_AIRLOCK_TEST_IAM_CREDENTIALS_URL` names the origin of the `generateIdToken` it reads the
identity token at, and `BROKER_AIRLOCK_TEST_ANTHROPIC_TOKEN_URL` the token exchange; a production
start (one with `BROKER_FAKE_SECRETS_FILE` unset) refuses either, naming it, as it refuses every
`BROKER_AIRLOCK_TEST_*` setting.

## Test

```sh
cd packages/envoy
go test ./internal/cipher/... ./cmd/cipherd/...
```

Postgres-backed tests skip unless `BROKER_TEST_DATABASE_URL` is set;
`packages/envoy/scripts/dev-postgres.sh` starts a local Postgres for them. `FuzzAirlock` skips
unless `AIRLOCK_MODEL` names a built Lean model (`internal/cipher/proxyrules/airlock_fuzz_test.go`).
The broker's docs (`docs/site/src/content/docs/broker/`: `concepts.md`, `operate.md` and
`guides/`) say how its packages, policy, enrollment and CLI work; `cmd/cipher-refgen` writes their
reference pages from the code's doc comments at the docs build, not in `go test`.

Cipher's proxy signs each bound host's leaf with a pod's own intermediate; "The proxy's chain" in
`concepts.md` says which names its constraints bind, and in which clients.
A copied intermediate key can sign, for up to 7 days, a certificate with no DNS alternative name that curl, Python and Node accept for a name outside the allowed ones when its common name is one label, such as `localhost`, or a wildcard over two or more labels, such as `*.evil.example` for `a.evil.example`. They refuse a plain name, such as `evil.example`, and a wildcard over one label, such as `*.example`, so for them the key reaches names of one label, and names of three or more labels through a wildcard, but no name of two labels. Bun accepts any name through the common name when the certificate also has a URI, email or registered-ID alternative name. git on GnuTLS rejects every form. Only the 7-day life limits this.
