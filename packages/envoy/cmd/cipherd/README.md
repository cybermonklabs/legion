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
It takes two subcommands in place of serving.
`envoy-broker end-sessions <login>` ends every browser session that login holds on the approval
page and prints how many it ended;
it reads the same configuration, binds no listener and applies no migration.
`envoy-broker airlock-check <file> [--against <old file>]` reads an airlock rules file with the
broker's own code and exits non-zero, naming the entry or the parse position, when the broker
would not serve that file;
on a file it would serve it prints the secrets, hosts and headers the file adds and removes
against the file `--against` names, with a changed `allow`, `kind` or `judge` beside them, so
that a pull request changing the deployment's `airlock.json` leaves a reviewer only the question
of whether each host is the right one for its secret.
`airlock-check` reads no configuration, opens no database and binds nothing.

For a local stack (Postgres in Docker, or one you run named by `DEV_BROKER_POSTGRES_URL`, a fake
secrets file standing in for Secrets Manager, and the clients), run
`packages/envoy/scripts/dev-broker.sh`.

## Google groups

A secret's `owner` tag, and any entry of its `requesters` tag, may name a Google group as `group:`
and the group's lowercase email (`group:approvers@example.com`); an entry that starts `group:` and
holds no lowercase email is refused, as a malformed owner or requester is. Each member of an owner
group decides the secret's requests on the approval page, which Dispatch's Inbox lists for each
member, and lists and revokes the grants the group approved, as the one owner of a person's secret
does. An agent-tier secret a group owns goes to a member's own sessions at once and waits for a
member's approval from any other session. A group in `requesters` admits its members' sessions.

The broker reads the members of each group a served secret names with the Directory API's
`members.list`, nested members included (`internal/cipher/groups`): a group the five-minute policy
load names for the first time at once, and every group once an hour after that. A reread of one
secret names no group to read, so a `retag` that names a new group admits nobody until the next
five-minute load reads it. A read that fails keeps the last good members. A group whose last good
read is more than 2 hours old, or that was never read, admits nobody: no member decides, no
session gets its agent-tier secret at once, its `requesters` entry admits nobody, and every grant
that rests on it releases nothing from its next release, an automatic grant included. Each read
writes one log line, `group members read`, with `group`, `members` (how many members the broker
holds for the group after the read) and `result`: `ok` at INFO, or the cause at WARN, with
Google's reason where Google gave one.

The broker reads the members as a reader account, which `BROKER_DIRECTORY_ACCOUNT` names: a
user-managed Google service account that holds Workspace's Groups Reader role and no other, and on
which the broker's own credential holds `roles/iam.serviceAccountTokenCreator`. The broker mints
that account's access token at IAM's `generateAccessToken` with the one scope
`admin.directory.group.member.readonly`, and sends it to the Directory API alone, which refuses
the pod's own token. Unset, the broker reads no group, each read logs
`result="no directory account"`, and every group admits nobody. The broker refuses to start on an
account that `BROKER_SERVICE_ACCOUNT_SECRETS` binds to a secret, since a grant of that secret
would release the token the group read uses. `BROKER_DIRECTORY_URL` is the Directory API's base
URL, Google's `https://admin.googleapis.com` when unset; a test names its stand-in there.

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
another one, releases nothing, and the broker logs `class=binding`. A release mints the bound
account's Google identity token for the audience `https://api.anthropic.com` at IAM's
`generateIdToken`, with the account's email in it, and trades it at
`POST https://api.anthropic.com/v1/oauth/token` (the jwt-bearer grant, with the value's ids, and
`workspace_id` only where the value holds one). It refuses a token that does not start
`sk-ant-oat01-`. The exchange is sent once and never again, since the identity token it carries may
be good for one exchange only; a failed mint is logged `class=refused` when Anthropic refused it
(its `401 authentication_error` when no rule admits the token), `class=stored-value` for a value
the decoder refuses, and `class=upstream` for any other failure. The release's expiry is the
token's own end, and the airlock's proxy mints again
five minutes before it, as it does for every minted value. The secret is human tier alone (the
broker refuses one at agent tier as `type-human-tier`), takes a `max-grant` of an hour or less,
names its owner in `requesters` as a `google-service-account` secret does (one person, or one
group, beside registered services), may carry `admin: true`, and carries no `scopes`.

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
