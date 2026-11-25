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
`envoy-broker profiles check <file> [--against <old file>] [--private-upstream <host>=<url>]`
reads a profiles file with the broker's own code and exits non-zero, naming the profile and the
field or the parse position, when the broker would not serve that file, or when its start check of
the results profile `DISPATCH_RESULTS_TOKEN` refuses it;
`--private-upstream` takes the value `BROKER_AIRLOCK_PRIVATE_UPSTREAM` gives the broker, and holds
that profile to the destination's host as the start does, and without it a file that holds the
profile prints one line saying that tie was not checked;
on a file it would serve it prints every field of each profile the file adds and removes against
the file `--against` names, and each field a profile both files hold changes: its source, tier,
requesters and `maxGrant`, its GitHub accounts, repositories, permissions and value, its Google
scopes, and its proxy block's hosts, headers, `allow`, `kind`, `judge` and guidance.
So a pull request changing the deployment's `profiles.json` leaves a reviewer only the question of
whether each of those is right for its profile.
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
release of it: the model judge decides none, whatever seconds a profile's `proxy` block gives the
name, as for a secret tagged `admin`. It takes a `max-grant` of an hour or less, names its owner in
`requesters` as a `google-service-account` secret does (one person, or one group, beside
registered services), may carry `admin: true`, and carries no `scopes`.

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
