---
title: Operating the broker
description: Running the Secrets Broker - its configuration, its Postgres and secret-store dependencies, Dispatch's connection to it, health, logs, metrics and the audit record.
sidebar:
  order: 3
---

The broker is a stateless HTTP process, and a deployment may run more than one of them behind one
address ([Running more than one pod](#running-more-than-one-pod)). Everything it knows lives in
Postgres; it reads who may have which secret, and the secrets' values, from its secret store, AWS
Secrets Manager or Google Cloud Secret Manager, and it starts nothing else.

## What to run

The broker ships as `envoy-broker` in the Envoy image, `ghcr.io/cybermonklabs/legion/envoy:<commit>`,
which `.github/workflows/release-envoy-listener.yaml` publishes on a push to `main` that touches
the image's sources (the image's default
entrypoint is the Envoy listener, so run it with `--entrypoint envoy-broker`). Of the builds
waiting at once only the newest runs, and the build that replaces another holds that commit's
change, so a commit may reach `main` without an image tag of its own. Pin the image by digest
and verify it as [Running Dispatch](/legion/dispatch/running-dispatch/) shows. To build it yourself:

```sh
cd packages/envoy
go build -o envoy-broker ./cmd/cipherd
```

It takes no flags (it refuses any flag it is given) and reads all of its configuration from the
environment. The [configuration reference](/legion/broker/reference/config/), generated from the
loader, lists every variable with its default and accepted values. A minimal production
configuration is:

```sh
BROKER_LISTEN_ADDR=0.0.0.0:13380
BROKER_PUBLIC_URL=https://secrets.internal.example
BROKER_MACHINE_LOGIN_URL=https://dispatch.internal.example/credentials/machine
BROKER_DATABASE_URL='postgres://broker:<password>@db.internal.example:5432/broker'
BROKER_K8S_OIDC_ISSUER=https://<cluster issuer>
BROKER_K8S_OIDC_AUDIENCE=legion-broker
BROKER_API_ACCOUNTS=dispatch=system:serviceaccount:example-ns:example-dispatch
BROKER_SIGNIN_ISSUER=https://accounts.google.com
BROKER_SIGNIN_CLIENT_ID=<client id>.apps.googleusercontent.com
BROKER_SIGNIN_CLIENT_SECRET_FILE=/var/run/secrets/broker-signin/client-secret
BROKER_SIGNIN_DOMAIN=example.com
BROKER_SECRETS_PREFIX=production/agent-secrets/
BROKER_SECRETS_KMS_KEY_ARN=arn:aws:kms:<region>:<account>:key/<key id>
AWS_REGION=<region>
```

`BROKER_MACHINE_LOGIN_URL` is the page where a person types a machine login's code, Dispatch's
`/credentials/machine`. It is optional. With it, the device authorization grant's
`verification_uri` names that page, so a machine shows its person where to go. Without it there is
no device grant at all: `POST /v1/oauth/device_authorization` is not mounted and answers `404`,
and the broker's authorization server metadata leaves the device code out of
`grant_types_supported`, which is how Dispatch knows to serve no machine-code page. Everything
else the OAuth provider serves — the token endpoint, the RFC 8693 token exchange, introspection
and revocation — is served either way. Leave it unset in a deployment where every machine holds a
cloud identity and nobody ever types a code. The boot line says which of the two this broker is:
`machine login device grant state=on verification_uri=…`, or `state=off` naming the unset
variable.

`GET /.well-known/oauth-authorization-server` is that metadata, RFC 8414. It needs no credential,
since it is what a client reads before it holds one, and nothing in it is a fact a caller could
not learn by calling the endpoints it names. It names this broker's issuer
(`BROKER_PUBLIC_URL`), its token, introspection and revocation endpoints, the grants it serves,
the DPoP signing algorithms it takes, and the subject token types its exchange is configured
with, so a client reads off one document what this deployment turned on rather than being
configured with it twice.

`BROKER_API_ACCOUNTS` lists the callers of the broker's named-caller routes, here Dispatch's
server as the caller `dispatch`, each by its own account. An account is written in one of two
forms, and the entry's own text says which: a Kubernetes ServiceAccount subject, as above, or
`google:` and a Google service account's address
(`listener=google:example-listener@example-project.iam.gserviceaccount.com`). A Kubernetes caller
presents its projected token for `BROKER_K8S_OIDC_AUDIENCE`, verified against
`BROKER_K8S_OIDC_ISSUER`, and is looked up by the token's `sub`, so such an entry needs that pair.
A Google caller presents its machine's own Google identity token for `BROKER_GOOGLE_OIDC_AUDIENCE`
— the audience every machine of the deployment already holds a token for, so the Envoy listener on
a machine presents what its metadata server already hands it and mints no second token — verified
against `BROKER_GOOGLE_OIDC_ISSUER`, and is looked up by the token's `email`, so such an entry
needs that audience and no cluster or VM service. Leave `BROKER_VM_SERVICES` unset when only API
callers use the Google audience: listing an API caller does not give it a machine login, but listing
its account as a VM service does. A project's default Google accounts and Google-managed service
agents are refused here as they are in `BROKER_VM_SERVICES`. No secret is shared between the
broker and its callers. A token that lives longer than an hour from its issue is refused, and
an account the list does not name is answered `403 ACCOUNT_NOT_LISTED`. Through those routes
Dispatch lists what waits on a person and relays a person's machine-login decisions, their
machine-login revokes and their sign-in revokes; it decides no secret request
([Network and trust](#network-and-trust)).

The four `BROKER_SIGNIN_*` settings are required, and a broker started without one of them exits
naming the missing ones. They are the page where a person approves a secret request, signed in to
the broker itself ([Approval sign-in](#approval-sign-in)). A deployment that has not settled how
its people sign in sets `BROKER_SIGNIN=off` instead, and that broker approves nothing
([A broker that signs nobody in](#a-broker-that-signs-nobody-in)).

A caller's **name** decides which of those routes its account reaches, so listing an account is
not one authority but a choice between them. The broker knows three names: `dispatch`, which
reaches every route Dispatch's server calls — the approval, denial, revocation and listing
routes, `POST /v1/oauth/revoke`, and `POST /v1/oauth/introspect`; `listener`, which reaches
`POST /v1/oauth/introspect` alone, for a resource server such as the Envoy listener that reads
one of the broker's access tokens back and relays nobody's decision; and `access-review`, which
reaches `GET /v1/grant-log` alone, for the quarterly review that reads the broker's record of
who asked for what and who said yes, and changes nothing in it. The grant log is no part of what
`dispatch` and `listener` carry, and neither of them reaches it. A listed caller on a route its
name does not carry is answered `403 ACCOUNT_LACKS_POWER`, naming the caller and the route, and an
entry naming a caller the broker does not know refuses the start, naming that name and the names
it knows, so a typo cannot quietly produce a caller that reaches nothing.

An **edge listener**, an Envoy listener that serves the sessions of named projects to agents
outside the deployment's network, calls one route with the same kind of identity token, and the
broker knows it by its `BROKER_NATS_SERVICES` entry of kind `edge-listener`, not by
`BROKER_API_ACCOUNTS`: `edge-listener/<machine id>/<KEY>[,<KEY>…]=<account>`, which lists the
projects it serves, at most 16, each once. The account is written as `BROKER_API_ACCOUNTS` writes
one and needs the same issuer: a Kubernetes service account needs `BROKER_K8S_OIDC_ISSUER`, and a
`google:` account needs `BROKER_GOOGLE_OIDC_AUDIENCE`, for which it counts as a Google caller. An
entry without its issuer refuses the start, naming the entry. It reaches
`GET /v1/project-repositories` alone, and only about the projects its entry lists: the
repositories the issues of that project resolve to,
which the broker reads from Dispatch on every call and never keeps. Any other account, the Envoy
listener's included, is answered `403 NOT_AN_EDGE_LISTENER`, and a project the entry does not
list `403 PROJECT_NOT_LISTED`, before Dispatch is asked. Whenever the broker cannot get
Dispatch's whole answer within 2 seconds, or has no `BROKER_PROJECT_ACCESS_URL`, it answers
`503 REPOSITORIES_UNKNOWN` and never a list. Past 16 calls of one edge listener waiting on
Dispatch at once on one broker process it answers `503 REPOSITORIES_BUSY`. That bound is counted
in each process, so a fleet of N replicas lets one edge listener hold 16 × N calls on Dispatch
(32 on two replicas). It is a guard on Dispatch's load, not a security bound: the entry's
projects are what hold an edge listener to its own answers. The broker reads the edge-listener
entries of `BROKER_NATS_SERVICES` whether or not it runs the NATS auth service. Set alone, with
every entry an edge listener's, `BROKER_NATS_SERVICES` starts no auth service and needs no NATS
audience: the auth service starts only with `BROKER_NATS_URLS` and its three seed files, and any
other kind of entry without them refuses the start. The boot's `NATS auth service state=on` or
`state=off` line says which, with the number of edge listeners read. The auth service, where it
runs, answers an edge listener's own connection with the edge listener's NATS grant for the
projects its entry lists, so there an edge listener's entry needs its NATS audience as every
other entry does: `BROKER_NATS_K8S_AUDIENCE` for a Kubernetes account and
`BROKER_NATS_GOOGLE_AUDIENCE` for a Google one. Every entry is read once, at the start: an entry
of no form refuses the start for its form,
before any other check reads its kind or its account.

Five settings turn the model judge on, which decides one narrow class of the requests that would
otherwise wait for a person ([Concepts](/legion/broker/concepts/#the-model-judge)): the endpoint
and the id of the model it asks, the audience of the Google identity token the broker presents to
that endpoint, and the Dispatch it reads an issue from with the file holding the broker's own
projected ServiceAccount token for it. They are all-or-none: with none of them set there is no
judge, and every human-tier request waits for the person who decides it today, except on a broker
with `BROKER_SIGNIN=off`, which denies every such request at once and takes no judge
([A broker that signs nobody in](#a-broker-that-signs-nobody-in)); its `model judge state=off`
boot line says so. A sixth setting names the protocol that endpoint speaks and has a default. A
seventh, `BROKER_JUDGE_MODEL_ACCOUNT`, is optional: it names the Google service account whose
identity token the judge presents, which the broker mints for that audience at IAM Service Account
Credentials' `generateIdToken`, spending the access token the metadata server answers for the
pod. It is for a pod that runs as a bare Workload Identity principal, whose metadata server
answers an access token and no identity token. Unset, the judge presents the metadata server's
identity token as before. The `model judge state=on` boot line names which: `identity=metadata`,
or `identity=` and the account's email. The
[configuration reference](/legion/broker/reference/config/) names each and gives its accepted
values. `BROKER_JUDGE_TEST_SKIP_CHECKS` turns the judge's checks before the model off, as the
local stack's negative control of them, and a production start — one with
`BROKER_FAKE_SECRETS_FILE` unset — is refused naming it. The broker reads the same Dispatch for
one more thing: the assignee of the issue a Legion worker pod's claim names, whose Inbox alone lists
that pod's request for a shared human-tier secret
([Concepts](/legion/broker/concepts/#approvals)). With the judge's settings unset it reads no
assignee, and such a request waits in every person's Inbox.

`BROKER_SECRETS_STORE` picks the secret store: `aws-secrets-manager`, the default when it is unset,
as above, or `gcp-secret-manager`, which takes the Google Cloud forms of the namespace and the key
and reads nothing from AWS:

```sh
BROKER_SECRETS_STORE=gcp-secret-manager
BROKER_SECRETS_PREFIX=projects/<project number>/secrets/agent-secrets-
BROKER_SECRETS_KMS_KEY_NAME=projects/<project>/locations/global/keyRings/<ring>/cryptoKeys/<key>
```

The prefix names the project by its number, not its id, because Secret Manager answers every
resource name with the number and the broker matches what the API answers (`gcloud projects
describe <id> --format='value(projectNumber)'` finds it); the id after `secrets/` ends in `-`, so
the secret a session asks for as `DEEL_API_KEY` is `projects/<number>/secrets/agent-secrets-deel-api-key`.
The key is in the `global` location, the only one a secret with automatic replication can be
encrypted with. Each store refuses the other's key variable, so a deployment moved between stores
cannot keep the old key set and silently mean nothing by it.

`AWS_REGION` is the AWS SDK's own setting, not the broker's, read by the AWS store alone: the SDK
takes the region of Secrets Manager and KMS only from `AWS_REGION`, `AWS_DEFAULT_REGION` or the
shared AWS config file, never from the instance it runs on. Without one, the broker's first read of
the namespace fails and it exits at startup with `broker: fatal error="list secrets under
production/agent-secrets/: operation error Secrets Manager: ListSecrets, failed to resolve service
endpoint, endpoint rule error, Invalid Configuration: Missing Region"`.

`BROKER_SERVICE_ACCOUNT_SECRETS` binds each `google-service-account` and `anthropic-federation`
secret to the one Google service account the broker may mint a token for, whitespace-separated
`<SECRET NAME>=<account email>` entries:

```sh
BROKER_SERVICE_ACCOUNT_SECRETS="GCP_ADMIN=gcp-admin@example-project.iam.gserviceaccount.com DEV_ADA=dev-ada@example-project.iam.gserviceaccount.com"
```

The name is the stored secret's uppercase name, as a session asks for it, not its store id or its
slug, and never a profile's: a profile of a bound secret mints under its source's binding, whatever
its own name, and `BROKER_AZURE_LOGIN_SECRET` names the stored `azure-login` secret in the same way.
The account is a user-managed service account, `<account id>@<project id>.iam.gserviceaccount.com`
in lowercase. The broker refuses to start, naming the entry, on a name no agent secret can carry,
an address that is not a user-managed account, a Google-managed service agent (Google's own
services run as those accounts, and the broker must not be able to impersonate one), or a secret
named twice. One account may back two secrets, since only the secret's name is the key.

This setting, and not a secret's stored value, chooses the account: the broker holds
`roles/iam.serviceAccountTokenCreator` on every account bound here, so a mint happens only where
the binding and the stored value name the same account. A secret the setting does not name, and a
stored value naming any other account, release nothing: the session is answered
`502 MINT_FAILED` and the broker logs `class=binding`, before any call reaches Google. Unset, no
secret is bound and every release of either type is refused, which is where a
deployment stands between this build and the apply that writes its bindings; the broker still
boots and still serves every other type. The policy loader reads the setting as well, for one
rule: a secret bound to a `dev-<name>` account is one person's own, so it may list no registered
service among its `requesters`.

A secret whose `owner`, or an entry of whose `requesters`, names a Google group (`group:` and the
group's lowercase email) needs the broker to read that group's members
([A group's secret](/legion/broker/concepts/#a-groups-secret)). It reads them with the Directory
API's `members.list`, as a reader account that `BROKER_DIRECTORY_ACCOUNT` names:

```sh
BROKER_DIRECTORY_ACCOUNT=group-reader@example-project.iam.gserviceaccount.com
```

The reader account is a user-managed service account, held to the rule
`BROKER_SERVICE_ACCOUNT_SECRETS`' accounts are held to. It holds Workspace's Groups Reader admin
role and no other role, and the broker's own Google identity holds
`roles/iam.serviceAccountTokenCreator` on it. The broker mints the account's access token at IAM
Service Account Credentials' `generateAccessToken`, with the one scope
`https://www.googleapis.com/auth/admin.directory.group.member.readonly`, and sends that token to
the Directory API alone, since the Admin SDK refuses the pod's own token. The broker refuses to
start on an account that `BROKER_SERVICE_ACCOUNT_SECRETS` binds to a secret, since a grant of that
secret would release the token the group read uses, and on a Google-managed service agent. Unset,
it reads no group and every group admits nobody: each read logs
`group members read … result="no directory account"`. `BROKER_DIRECTORY_URL` names the Directory
API's base URL, `https://admin.googleapis.com` when unset; a test sets it to a stand-in. It must be
an https URL, or http to a loopback host for a run on one machine, since the reader account's token
goes to it; the broker refuses to start on any other. The Directory and IAM calls follow no
redirect. At start, the pod waits up to 15 seconds for its first group reads before it serves, and
a group whose first read failed is read again at the next five-minute read of the namespace.
Whoever may change a group's members in Workspace decides who decides that group's secrets, so an
owner group moves that power from the secret's tags to the group.

The broker refuses to start, naming the variable, when one is missing, malformed or out of range,
and also while a variable it no longer reads is still set. `BROKER_DATABASE_URL` is the only
database variable: a password goes in the URL itself, URL-escaped, and on Amazon RDS the broker
needs none ([Signing in to RDS by IAM token](#signing-in-to-rds-by-iam-token)).

A secret owned by a service rather than a person needs that service registered.
`BROKER_SERVICES` lists each service with the Kubernetes service accounts its pods run as,
whitespace-separated `name=<service account>` entries, for example
`BROKER_SERVICES=legion-daemon=system:serviceaccount:legion:legion-worker` for the Legion daemon's
worker pods. A service whose pods run as several accounts gets one entry for each, for example
`legion-daemon=system:serviceaccount:legion:legion-worker legion-daemon=system:serviceaccount:legion:legion-controller`.

A pod gets the service's agent-tier secrets at once when two things hold: a machine login for the
listed service enrolled it, and its projected token proved one of that service's accounts. Every
other session is denied them. For the same reason each account is bound to one service: the broker
refuses to start, naming both services, if `BROKER_SERVICES` binds one account to two of them. It
also refuses, naming the account, an account given twice for the same service.

The account must be one the service's pods name. The broker refuses to start, naming the entry,
if `BROKER_SERVICES` binds a namespace's `default` account
(`system:serviceaccount:<namespace>:default`). Every pod in a namespace whose spec names no account
runs as `default`, and any of those pods can get a token for the broker's audience. A `default`
binding would therefore prove nothing about which pods are the service's. Create a dedicated
account, such as `legion-worker`, set it as the pods' `serviceAccountName`, and bind that account.

While a secret's owner tag names a service the list leaves out, the broker refuses the secret as
`owner-tag-malformed` ([Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret)).

A deployment that moves a service's pods from one Kubernetes service account to another pairs the
two accounts in `BROKER_SESSION_ACCOUNT_PAIRS`, whitespace-separated `<account>=<account>` entries,
for example
`BROKER_SESSION_ACCOUNT_PAIRS=system:serviceaccount:legion:legion-worker=system:serviceaccount:legion:<project>-worker`.
A session name belongs to the principal that claimed it, and a pod's principal is the account its
projected token proves, so without the pair each role's next pod, on the new account, is refused
its own session name (`403 SESSION_HELD`), and the session's grants, pending requests and withheld
names stay behind. Either account of a pair takes a session name the other holds, by an enrollment
or by a token exchange, and an enrollment under that name takes what the other account's
enrollments of it hold that a login of the same account enrolled, so move the pods' account and
keep the launcher's login account ([Concepts](/legion/broker/concepts/#requests-and-grants)). A
pair works both ways, so moving the pods back needs no change here, and it does not chain: with
`A=B` and `B=C`, `C` does not take a name `A` holds. No pair takes a name an entry of
`BROKER_EXCHANGE_ACCOUNTS`, `BROKER_EXCHANGE_READ_ACCOUNTS`, `BROKER_EXCHANGE_JOB_ACCOUNTS`,
`BROKER_EXCHANGE_WORKFLOWS`, `BROKER_EXCHANGE_JOB_WORKFLOWS`, `BROKER_VM_EXCHANGE_ACCOUNTS` or
`BROKER_VM_EXCHANGE_JOB_ACCOUNTS` fixes. The broker refuses to start, naming the entry, on an entry
not of that form, a namespace's `default` account, an account paired with itself, and a pair given
twice, in either order. Set the pairs, bind the new account in `BROKER_SERVICES` beside the old
one, and, where the pods trade their projected tokens at the exchange, list the new account in
`BROKER_EXCHANGE_ACCOUNTS`, or in `BROKER_EXCHANGE_JOB_ACCOUNTS` for a job's pods, before the
deployment moves its pods: a pair added after a role's enrollment was refused takes effect at
that role's next enrollment, and the grants and pending requests of the old enrollment come across
only while the broker still keeps them,
`BROKER_SESSION_CARRY_SECONDS` after that enrollment ended. While a pair stands, a pod of either
account can claim any session name the other holds, not only its own role's, so remove the pairs
with the old account once no live pod runs as it. The start line `session account pairs` names
the pairs the broker read ([Health and logs](#health-and-logs)).

A service's launcher, such as the Legion daemon, logs in with its own projected ServiceAccount
token, and no person approves it. `BROKER_SERVICE_LOGINS` lists which ServiceAccounts may log in
as which service, whitespace-separated `<service>=<service account>` entries, for example
`BROKER_SERVICE_LOGINS=legion-daemon=system:serviceaccount:legion-system:legion-daemon-legion`,
one entry per daemon. Each account is named exactly, with no wildcard, and logs in as the one
service its entry names.
The broker refuses to start on an entry naming a namespace's `default` account, which every pod in
that namespace whose spec names no account runs as, so any pod there could log in as the daemon.
An account `BROKER_SERVICES` binds for a service's pods may be listed here as well, which is a pod
of that account logging the service in to enrol itself (`agent-secrets enroll --pod`).
Every pod of such an account holds a token for it and so logs the service in, and each login
enrols the one pod whose token made it: any other pod UID is refused `403 POD_SELF_ENROLL_ONLY`.
A pod enrolling itself states no session name either, `403 POD_SELF_ENROLL_NO_SESSION`: the pods
of one account enrol under one principal, so a name such a pod picked would be a sibling pod's,
and the enrolment would take that sibling's grants. `agent-secrets enroll --pod` sends none.
The broker verifies the token as it verifies a pod's, against
`BROKER_K8S_OIDC_ISSUER` and `BROKER_K8S_OIDC_AUDIENCE`, so the setting needs both, and refuses a
token that lives longer than an hour from its issue (`exp` minus `iat`, beside a minute of skew) as
`TOKEN_TOO_LONG_LIVED`: give the daemon's projected token an `expirationSeconds` of at most 3600,
and `kubectl create token` a `--duration` of at most `1h`. Each account holds one launcher
credential: every login moves it to the login's key and to the token's expiry, at most an hour
away. A running daemon proves the same key at each login, so nothing about it changes but its
expiry; a restart's login brings a new key, so the daemon's earlier key stops working at once and
the pods it enrolled before the restart stay its own. A device-code login that names a service is
refused `SERVICE_LOGS_IN_WITH_TOKEN`. Dispatch's machine-login page lists a daemon's token login
for everyone signed in, naming its account and no approver, and anyone may revoke it, which ends
every pod it enrolled at once; the daemon then logs in again with its token under a new credential,
which enrolls the pods it launches next, while a pod already running stays without secrets until it
is replaced. To stop a daemon logging in, remove its entry and redeploy the broker: its credential
stops authenticating at its next call, and its next login is refused.

An account on that list that `BROKER_POD_OPERATORS` does not map (below) may also call
`POST /v1/oauth/exchange/pod-sessions`, the route that registers which pod may write under which
session name. It is how a deployment's daemon tells the
broker that a pod it has just started owns an issue's session. The login lets an account call the
route and does not let it register a pod: `BROKER_POD_REGISTRARS`, not `BROKER_SERVICE_LOGINS`, is
the registration's authority. A session pod logs in as a service too, and with the login alone it
could register a pod whose token it holds for a session name of its choice.

`BROKER_POD_REGISTRARS` lists which registrar may register the pods of which account,
whitespace-separated `<registrar account>=<pod account>` entries, one entry per pair, for example
`BROKER_POD_REGISTRARS=system:serviceaccount:legion-system:legion-daemon-legion=system:serviceaccount:legion:legion-worker`.
The registrar is the account the caller logs in as, and the pod account is the subject of the pod
token in the request's body. Each account is a ServiceAccount subject,
`system:serviceaccount:<namespace>:<name>`, named exactly, as `BROKER_SERVICE_LOGINS` names its
accounts. The route admits a registration only where an entry pairs the two accounts, and refuses
every other one `403 POD_REGISTRAR_NOT_LISTED`, naming both accounts. A registrar may have several
pod accounts, a pod account may have several registrars, and an entry may pair an account with
itself, for a daemon that registers the sibling pods of its own account.

The broker refuses to start, naming the setting and the entry, on an entry without exactly one `=`,
an account of any other form (a `google:` one included), a namespace's `default` account, and a pair
given twice. It also refuses the list set without `BROKER_K8S_OIDC_ISSUER`, and a registrar that
`BROKER_POD_OPERATORS` maps to a person (below). It does not refuse a registrar that
`BROKER_SERVICE_LOGINS` does not list, since the list may come from another source than the logins:
such a registrar cannot call the route, so it registers nothing, and the boot logs
`pod-session registrar cannot log in` at WARN, naming it. The boot also logs one
`pod-session registrars` line, with the number of `entries` and whether a `registration_audience` is
set.

Unset, the broker starts and the route admits no registration: it refuses every one
`403 POD_REGISTRAR_NOT_LISTED`, the one `403` of this route that the Legion daemon retries. So
a deployment sets `BROKER_POD_REGISTRARS` before its first daemon sets `exchange_audience`.

`BROKER_POD_REGISTRATION_AUDIENCE` (optional) is a second audience the route accepts for the pod
token in the body, beside the broker's own `BROKER_K8S_OIDC_AUDIENCE`, which the pod token a daemon
sends today carries. The broker verifies a token of either audience against
`BROKER_K8S_OIDC_ISSUER`, which the setting needs. It refuses an expired token of either audience
`401 POD_TOKEN_EXPIRED`, and every other token that does not verify, one of neither audience
included, `401 POD_TOKEN_INVALID`. No other route accepts a token of this audience: enrolment and
every ServiceAccount login verify the broker's own audience, the token exchange
`BROKER_EXCHANGE_K8S_AUDIENCE`, and the NATS auth service `BROKER_NATS_K8S_AUDIENCE`. So a daemon
can be handed a pod's token that registers that pod and that no other route takes.

The broker refuses to start, naming `BROKER_POD_REGISTRATION_AUDIENCE` and the other setting, when
the audience equals `BROKER_K8S_OIDC_AUDIENCE`, `BROKER_EXCHANGE_K8S_AUDIENCE` or
`BROKER_NATS_K8S_AUDIENCE`, since a token of it would then be spendable on that setting's routes
too. It also refuses the audience set without `BROKER_K8S_OIDC_ISSUER`. Unset, the route accepts the
broker's own audience alone.

The request carries the pod's own projected ServiceAccount token, which the broker verifies
against the cluster's issuer as it verifies an enrolling pod's, bar the one check it does not make
here, that the token names a pod: such a token verifies and the exchange then refuses the
registration `400 POD_NOT_BOUND`. So a registrar registers a pod whose token it holds and no other.
A caller whose own token names a pod may not register that pod, `403 POD_CALLER_IS_THE_POD` — a
pod that wrote its own row would pick the session name it writes under, including a name a sibling
pod holds. The refusal reads the caller's own token, so it never holds a caller whose token names
no pod, such as one `kubectl create token` mints: whoever mints one for a registrar's account
registers every pod whose token they hold, of each account an entry pairs with it, the pod they
run in included.

The route checks the list before the caller's own pod, so listing a session account in
`BROKER_SERVICE_LOGINS` for `agent-secrets enroll --pod` (above) gives its pods no authority to
register a pod: a login no entry names is refused `403 POD_REGISTRAR_NOT_LISTED` for every pod,
its own included. An entry that pairs such an account with itself lets its pods register one
another, and only by token: a pod registers another only while it holds that pod's own token, and
a pod holding its own alone registers no pod, since its own is the one `POD_CALLER_IS_THE_POD`
takes away. A pod that does hold a sibling's registers it, which is how one generation of a rollout
registers the next. The authority goes no wider than the cluster's own rights to mint or read a
pod's token — the same kind of access over that pod's account as the paragraph that opens "Whoever
can get a token" (below) names over a listed one — and it stops short of them: the caller needs a
`BROKER_SERVICE_LOGINS` token of its own and a `BROKER_POD_REGISTRARS` entry that pairs its
account with the pod's, and the pod's account must be one `BROKER_EXCHANGE_ACCOUNTS` or
`BROKER_EXCHANGE_JOB_ACCOUNTS` lists, `403 POD_ACCOUNT_NOT_LISTED`. A deployment that does not
want that pairs no such account with itself.

A release (below) belongs to the account the caller logs in as, not to the caller's pod. Every pod
of an account paired with itself can log in as that account: the pair registers nothing unless
`BROKER_SERVICE_LOGINS` lists it. So each such pod can release every registration that account
wrote, a sibling's included, by session name alone. It needs no token of that sibling. The token
exchange then refuses the sibling's trade under that name until the account registers that pod
and name again. Every pod that runs as any registrar's account can log in as that account in the
same way and release every registration that account wrote. So list as a registrar only an
account whose pods may end the trades of every pod that account registers. An account paired with
itself is the common case of an account whose pods may not.

An account `BROKER_EXCHANGE_READ_ACCOUNTS` lists (below) is refused `409 SESSION_FIXED`, as is an
account whose `BROKER_EXCHANGE_ACCOUNTS` or `BROKER_EXCHANGE_JOB_ACCOUNTS` entry fixes a session:
such an entry fixes the session its tokens name, so its pods need no registration.
A daemon that stops registering costs its pods their writes within the registration's own retention
(24 hours past the last write). Do not list such an account in `BROKER_API_ACCOUNTS` as well: that
is the separate list of the callers that relay a person's decisions and read tokens back.

A registration may also name the thumbprint of the key that session's own role proves
(`thumbprint`, 43 base64url characters, else `400 THUMBPRINT_INVALID`). The broker then admits a
trade for that session only with a DPoP proof of that key, and refuses one proved with another
`invalid_grant`, naming the session. The roles of one Legion issue pod read one exchange token and
are registered for one pod, so this is what keeps one role from trading for another role's
session; the Legion daemon names each role's key. A later write by the same registrar that names
no key keeps the one the registration names, unless that registrar released the registration
(below), and a registration that names none, as a session pod's is, admits any key of that pod.
A Legion daemon's `exchange_audience` waits for three changes of plan E, and this release holds all
three. In the daemon and its worker image, t-e-53 makes a role trade with its own key, and t-e-39
has the daemon choose each Oh My Pi role's session id rather than take its agent's report. Before
t-e-53, a role's own trade is refused. In this broker, t-e-41 (dispatch://INFRA-4424) is the
registrar list above, and the session-name binding and the release below. Before t-e-41, any
`BROKER_SERVICE_LOGINS` account can register a pod, its own daemon's among them, for another
claim's session. So a deployment sets `exchange_audience` only once its daemon, the daemon's worker
image and this broker run this release or a later one, and only once `BROKER_POD_REGISTRARS` is
set. The daemon already holds each session id to the claim of its project that first recorded it
(t-e-54) and registers no session another of its claims holds.

A registration may also name its `generation` (0 or more, 0 when absent; a negative one is
`400 GENERATION_INVALID`), the registrar's own order of its writes for that pod and session. The
broker applies a registrar's write to a registration of its own that it has not released only when
its generation is at least the registration's, and answers the registration as it stands after the
write, its generation included when it is not 0. The Legion daemon sends a role's launch
generation, so a write its old incarnation sent, which the broker commits only after the new
incarnation's, changes nothing and cannot put the old key back. A registrar that sends none, as a
session watch does, writes 0 as before and is answered no generation, exactly as before. A write
that takes a registration whole (below) is applied whatever its generation.

A session name belongs, for good, to the registrar that first registered it. Each registration
records the login account of the caller that wrote it, and the name's first registration binds the
name to that account in the table `pod_session_registrars` (migration 0039). A registration of a
bound name by another registrar, for any pod, is refused
`409 SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR`, naming the session and the registrar that holds it,
and the broker writes nothing, whether the holder's registrations of the name are live, lapsed,
released or swept. The registrar that holds a name registers any number of pods for it, as a
relaunch does. No sweep and no route moves or removes a binding, so a daemon that stops using a
name keeps it. The broker logs the refusal at WARN, `broker: a pod's session registration was
refused: another registrar holds the session name`, naming the registrar, the holder, the pod and
the session.

So two daemons listed for one pod account register no pod, their own or each other's, under a name
the other holds, and the exchange admits a pod under a name only when the registrar that holds the
name registered that pod. Of two daemons' first writes of one name at the same moment, exactly one
is applied, since the second write's binding of the name waits for the first's to commit and then
finds the first registrar.

The token exchange refuses a pod's trade under a live registration that another registrar wrote,
or under a name bound to no registrar, `invalid_grant`, with a description that begins
`session_name_not_this_registrars:` and names the session, never the registrar that holds it. The
route writes no such registration: such a row was written by something other than this release's
route.
Like the refusal of a released registration (below), that refusal is final: both clients stop on
it rather than trade again, since only the holding registrar's registration of that pod admits it,
or, under a name bound to no registrar, a registration that binds the name to the pod's registrar.
A pod that no registration names keeps the `pod_session_not_registered:` refusal. The broker logs
each case at WARN on a line of its own. For a name another registrar holds, the line is `broker:
the token exchange refused a pod's trade under a session name another daemon holds`, naming the
principal, the pod, the session, the registrar that wrote the registration and the holder. For a
name bound to no registrar, the line is `broker: the token exchange refused a pod's trade under a
session name bound to no daemon`, naming the principal, the pod, the session and the registrar
that wrote the registration.

The registrar is the daemon's login account, never its service, which every Legion daemon shares,
nor its pod, which a restart changes. So a daemon whose login ServiceAccount changes is a new
registrar, and it cannot register the names its old account bound: the broker refuses it
`409 SESSION_NAME_HELD_BY_ANOTHER_REGISTRAR` for each of them, for good, after the old account's
registrations lapse or are released too. No route moves a binding to another account yet. Today's
Legion daemon reads that `409` as permanent: it logs it and does not write that registration again
for that role's process.

A daemon releases the session names it registered at `POST /v1/oauth/exchange/pod-sessions/release`,
with the same login as the registration route, which answers `401` for a token that does not verify,
`403 ACCOUNT_NOT_LISTED` for an account `BROKER_SERVICE_LOGINS` does not list, `403 PERSON_ACCOUNT`
for an account `BROKER_POD_OPERATORS` maps to a person, and `429` with `Retry-After`.
`BROKER_POD_REGISTRARS` does not gate a release, since a release touches only the registrations of
the account the caller logs in as. A registrar whose entry was taken out therefore still releases
what it registered, unless `BROKER_POD_OPERATORS` then maps its account to a person: the route
refuses that account `403 PERSON_ACCOUNT`, so a registrar that is to be mapped releases its
registrations before it is mapped. A release is that account's, not the caller's pod's. So every pod
of an account paired with itself can release every registration that account wrote, by session name
alone (above). The Legion daemon calls this route from an operator's `legion claims release-names`
when its `exchange_audience` is set: before it frees any of a claim's names, it sends the session
ids among them in one call, tries it up to three times, a second and then two apart, and frees them
only after a `200` whose `sessions` counts the distinct names it sent. Any other answer frees
nothing: the daemon refuses the release `503`, which a run again can change, or `502` for a `4xx`
other than `404`, `405` and `429`, which a run again answers the same. A daemon with no
`exchange_audience` registers no pod session and makes no release call.

The body is `{"sessions": ["<name>", ...]}`. A list that is missing, `null` or empty, or that holds
more than 4096 entries counted as sent, is `400 SESSIONS_INVALID`, and a body over 1 MiB is
`413 REQUEST_TOO_LARGE`. An unknown field, a body that is not JSON, and an entry that is not a
string are `400 INVALID_REQUEST`. A `null` entry is not a string, so the broker refuses it
`400 INVALID_REQUEST` too, naming its index.

Any string is a name, the empty string included: one the registration route could never have
taken matches no registration, so it is released with nothing to release. The broker drops such a
name before it queries its store. That also keeps a NUL byte (JSON's `\u0000`) out of the query:
Postgres refuses a NUL byte in text, so without the drop the release would fail with a `500`.

In one transaction, the broker marks released every live registration of each named session that
the caller's account registered, whichever of its pods wrote it. It deletes nothing, moves no
expiry, and neither touches nor reveals a registration of another registrar. The answer is `200`
with `sessions`, the number of distinct names asked, and `released`, the number of that account's
live registrations of those names that are released after the call. A repeat of the call answers
the same and changes nothing, so a retry after a lost answer is safe.

Every other answer releases nothing, a `404` or `405` from a broker of an older build included.
The broker logs each release at INFO, `broker: released a registrar's pod-session registrations`,
naming the registrar, the number of names and the number of registrations released.

From the release's commit, the token exchange refuses a pod's trade under a released registration
`invalid_grant`, with a hint that names the session. That refusal is not the
`pod_session_not_registered:` one that both clients trade again on: no later trade cures it, and
only the registrar's next registration of that pod and name makes it live again. The broker logs
it at WARN, `broker: the token exchange refused a pod's trade under a registration its registrar
released`, naming the principal, the pod, the session and the registrar.

A released registration stays until it expires, at most 24 hours after its last write. The name
stays bound to its registrar after that too, as every name stays bound to the registrar that first
registered it (above), so a release hands no name to another daemon. The registrar's next write of
that pod and name takes the registration whole: its key, its generation and its expiry become that
write's, so a later claim may count its generations from lower. A registrar stops every write of
the names it releases before it releases them, its renewal and any write in flight included, since
a registration write after the release makes that registration live again.

A release revokes no access token. A token minted before the release commits, or by an exchange
that read the registration just before, lives to its own expiry, at most
`BROKER_ACCESS_TOKEN_SECONDS` (900 seconds by default) after the release.

The token exchange does not read `BROKER_POD_REGISTRARS`: it honours a registration written while
its entry stood until the registration lapses or its registrar releases it. So removing an entry
and redeploying the broker stops that registrar's new registrations at once and its old ones
within 24 hours. The names that registrar bound stay bound to it: no other registrar registers
them once the entry is gone, and no route moves them yet.

A registration that a broker of a build before migration 0038 wrote names no registrar. It holds
no name, and no exchange honours it: the exchange answers it as it answers no registration at all,
with the `pod_session_not_registered:` refusal. The next admitted write of its pod and name takes
it whole. Such a registration comes from before the migration, from a replica of an older build
while the broker rolls, or from a rollback.

Rolling the broker back past the release that brought migration 0032 drops the binding without a
word: an older broker reads no key or generation, so it admits a trade for a bound session proved
with any key of that pod, and its route refuses a body naming `thumbprint` or `generation` with
`400`, so the daemon's registrations fail. Unset `exchange_audience` in every Legion daemon before
you roll the broker back past this release.

Rolling the broker back past the release that brought migrations 0038 and 0039 drops the
session-name binding and the release without a word: an older broker reads neither the binding,
the registrar nor the release mark, so its exchange honours a released registration and a
registration of any registrar, and its route takes a registration from every account
`BROKER_SERVICE_LOGINS` lists. The table
`pod_session_registrars` stays, and an older broker neither reads nor writes it, so a roll forward
finds each binding as it stood before the rollback. Unset `exchange_audience` in every Legion
daemon before you roll the broker back past this release. In a roll either way, the binding holds
only once no broker replica of an older build takes registrations, because such a replica's write
neither checks the binding nor sets the registrar, and another daemon's write through it can
extend a lapsed registration that the daemon a name is bound to wrote, which the newer replicas'
exchange then honours.
Rolling the broker back past the release in which each key says who decides its requests leaves
the column `requests.judge_until` in place, which an older broker never reads, so a request the
newer broker held for the judge is an ordinary card on the older one. An older broker also refuses
a rules file that gives an entry `guidance` or no `hosts` (`unknown field "guidance"`, or the
entry's `no host`) and does not start with one. Put the rules file back to one without either
before you roll the broker back past this release.

Rolling the broker back past the release in which a key's judge and guidance became its own tags
gives the judge nothing: an older broker reads them from the profiles file's `proxy` blocks, and
ignores the `judge` and `guidance` tags, so every request goes to its person until the file sets
them again. A broker of this release refuses a profiles file that still sets `judge` or
`guidance` on a profile, naming the tag to write instead, and does not start with one: tag the
keys (`agent-secrets secret retag <NAME> --judge <seconds> --guidance '<text>'`) and take the two
fields out of the file before you roll this release out. A profile behind a kind is served with
its source's `judge` seconds in its fingerprint, where its block held them, so tagging the source
with the seconds its block held and then taking the two fields out ends no grant. Tag it with
other seconds, or leave it untagged, and each live grant of that profile ends at the first broker
of this release.

Whoever can get a token for a listed account logs in as its service. Kubernetes access control on
the daemon's namespace is that boundary: anyone who may create pods there naming the account,
create `serviceaccounts/token` for it, or exec into the daemon's pod and read its token file can
get one. Deleting the daemon's ServiceAccount stops new tokens, but a token already issued keeps
verifying until it expires, at most an hour after it was minted, because the broker checks tokens
against the cluster's published keys rather than asking the API server. A token login spends its
source address's rate-limit bucket before its token is verified and its account's own after, so a
daemon behind an address other callers share is held to that address's bucket. A login the broker
could not complete (a 5xx, such as while its database is away) spends neither bucket, so the
daemon's retries through a database outage leave it free to log in once the database is back. Set
`BROKER_TRUSTED_PROXY_HEADER` behind a proxy, so each caller is keyed on its own address, and
keep the daemon's and its pods' traffic to the broker on their own pod addresses rather than
translated to a node's or a gateway's. The daemon renews halfway through its credential's life,
which carries it through brief contention for its address's bucket; a caller that keeps that
bucket drained for the rest of the credential's life stops it logging in.

A service login a person approved through a device code before this broker keeps working until
it expires, at most `BROKER_LAUNCHER_CREDENTIAL_SECONDS` (30 days by default) after its approval:
nothing refreshes it.

`BROKER_POD_OPERATORS` maps a Kubernetes ServiceAccount to a person, so that a pod running as that
account acts as the person on the terms of the person's dev box
([Concepts](/legion/broker/concepts/#a-persons-pod)). It takes whitespace-separated
`<email>=<service account>` entries, for example
`BROKER_POD_OPERATORS=ada@example.com=system:serviceaccount:example-ns:ada`. The email is the
person as they sign in, in lower case. A person may have several accounts, and each account maps
to one person. Unset, the setting maps no account. Each mapped account logs in with its own token
under a login service of its own, so it needs a `BROKER_SERVICE_LOGINS` entry, such as
`person-ada=system:serviceaccount:example-ns:ada`. No `BROKER_SERVICES` or `BROKER_VM_SERVICES`
entry may name that login service, and no `BROKER_SERVICES` entry may bind the account. The
broker refuses to start, naming the entry or the account, on:

- an entry that is not `<email>=system:serviceaccount:<namespace>:<name>`, or whose person is not a
  lower-case email;
- a namespace's `default` account, which every pod there that names no account runs as;
- an account given twice;
- a mapped account that `BROKER_SERVICE_LOGINS` does not name;
- a mapped account whose login service a `BROKER_SERVICES` entry names, since a secret could then
  name that service as its owner;
- a mapped account that a `BROKER_SERVICES` entry binds, since a launcher logged in as that service
  could then enrol the person's pods as its own workers;
- a mapped account whose login service a `BROKER_VM_SERVICES` entry names, since the broker
  registers those services as owners a secret may name too;
- a mapped account that `BROKER_EXCHANGE_ACCOUNTS` maps to another person or lists with no person;
- a mapped account that a `BROKER_POD_REGISTRARS` entry names as its registrar, since the person's
  pod would then choose the session names other pods exchange under (a mapped account on the pod
  side of an entry is not refused);
- a mapped account that a `BROKER_EXCHANGE_READ_ACCOUNTS` entry names, since the exchange would
  then mint the person's pod a read-only token that names a service and no person;
- a mapped account that a `BROKER_EXCHANGE_JOB_ACCOUNTS` entry names, since the exchange would
  then mint the person's pod a job's token that names the account as its service and no person
  and reaches the entry's projects;
- a mapped account that a `BROKER_API_ACCOUNTS` entry names, since that caller's routes relay
  approvals in the name the caller gives;
- a mapped account that a `BROKER_NATS_SERVICES` entry names, since the person's pod would then be
  answered as that service: by the NATS auth service with that service's NATS grant, and, for an
  edge-listener entry, also by `GET /v1/project-repositories`;
- a mapped account that either side of a `BROKER_SESSION_ACCOUNT_PAIRS` pair names, since a pair
  lets each account take the session names the other holds.

A mapped account on the pod side of a `BROKER_POD_REGISTRARS` entry is not refused, so a daemon may
register a person's pod under a session name. The registration's body carries that pod's token, and
unless `BROKER_POD_REGISTRATION_AUDIENCE` is set it is a token of the broker's own audience, the one
the person's pod logs in with, so the daemon then holds a token that acts as the person. For such an
entry, set `BROKER_POD_REGISTRATION_AUDIENCE` and hand the daemon the pod's token of that audience,
which no other route accepts.

The pod enrols itself with `agent-secrets enroll --pod --service <its login service>`, which names
no session. It names its Dispatch projects as the person's session does, with
`AGENT_SECRETS_PROJECTS` or `--projects`; a restricted member's pod enrols nothing, whatever it
names (`403 RESTRICTED_MEMBER_ENROLLS_NOTHING`, [Concepts](/legion/broker/concepts/#a-persons-pod)).
Its login, made with its own projected token, carries the person, and so does its
enrollment: the person's agent-tier secrets go to it at once, the person's human-tier secrets wait
for the person's approval, and another person's secret waits for its owner's. A token of the
account that is bound to no pod, such as one `kubectl create token` mints, logs in for no person
and enrols nothing. The login enrols its own pod alone: another pod of the account and a pod of any
other account are each refused `403 POD_SELF_ENROLL_ONLY`, and a session name is refused
`403 POD_SELF_ENROLL_NO_SESSION`. The pod lists and revokes none of the person's machine logins
and grants: `GET /v1/operator/machines`, `POST /v1/operator/machines/{id}/revoke`,
`GET /v1/operator/grants` and `POST /v1/operator/grants/{id}/revoke` refuse its login
`403 PERSON_POD_LOGIN`. The person lists and revokes those from a box or from Dispatch. The routes
a service calls as itself refuse the mapped account `403 PERSON_ACCOUNT`, whether or not its token
is bound to a pod, so it registers no pod's session. Whoever can run a pod as a mapped account, or
get a token bound to such a pod, acts as its person: Kubernetes access control on that account's
namespace is the boundary, as it is for a service's account above.

Dispatch's machine-login page lists that login to its person alone, and only that person may
revoke it, whichever of the account's pods or tokens made it. A revoke ends every enrollment the
login made, with their grants, and does not last: the pod logs in again with its token, under a
new credential, the next time it enrols. To stop the pod logging in, take the account out of both
`BROKER_POD_OPERATORS` and `BROKER_SERVICE_LOGINS` and redeploy the broker. Taking it out of
`BROKER_SERVICE_LOGINS` alone refuses the start. The enrollment the pod already holds keeps
renewing until its login is revoked or the pod stops renewing it, as the list below says.

The broker reads the setting when it starts, so a change is an edit and a redeploy of the broker.
Every decision of what a pod may do reads the setting as it is then, while the stored records keep
the person each enrollment was made for. After the deployment removes an entry, or moves it to
another person:

- the enrollment the pod holds keeps renewing, and the broker answers each of its calls as its
  account's own, for no person, also after a move: the first person's agent-tier secrets wait for
  that person's approval, and a shared agent-tier secret is still granted. A broker with
  `BROKER_PROJECT_ACCESS_URL` still asks Dispatch about the first person, the one the pod was
  enrolled for: when Dispatch calls that person a restricted member, the pod is denied every secret
  `restricted-member`, a shared agent-tier secret included, and each grant it holds ends at its
  next use, whatever the items below say; while Dispatch cannot say what kind of member that
  person is, each secret request, release and new airlock call of the pod is refused
  `503 MEMBER_KIND_UNCHECKABLE`;
- the pod's requests are its own enrollment's alone. The pod no longer reads or cancels a request
  of the person's box or of another session of the person, and those sessions no longer read or
  cancel the pod's requests: each is refused `403 NOT_YOURS`, or `404 NOT_FOUND` for a request of
  a private project the reading session's credential does not name, and the request stays pending. The
  pod still reads and cancels its own requests. Putting the entry back gives the pod the person's
  requests again;
- a request for a shared secret keeps the Inbox it was listed in when it was made, since the broker
  records the person responsible for a request once, as it makes the request. So a request the pod
  made before the change still waits in the first person's Inbox alone, also after a move, until
  someone decides it, the pod cancels it or it expires. Anyone signed in may still decide it from
  its page. A request the pod makes after the change waits in every person's Inbox;
- an automatic grant of the person's own agent-tier secret, which the pod got because it acted as
  the person, stops releasing (`GRANT_NOT_LIVE`) and is not given back to a new request, so the
  pod's next request for that secret waits for the person. It is not revoked, and putting the entry
  back makes it release again;
- a grant a person approved stands on that approval. It keeps releasing, whoever approved it, until
  it expires, its approver revokes it, the policy refuses it to the account, or the enrollment ends,
  unless Dispatch calls the first person a restricted member or cannot say, as the first item says;
- a grant of a secret whose `requesters` tag names the person ends for good
  (`requester-not-listed`), and putting the entry back does not revive it;
- the pod's login keeps the first person until the pod logs in again, and lives at most an hour.
  Meanwhile it enrols nothing and revokes nothing (`403 OPERATOR_MISMATCH`), and the operator
  routes still refuse it `403 PERSON_POD_LOGIN`. The pod's next login carries no person after a
  removal, and the second person after a move;
- the pod's own login cannot end the enrollment it holds. That enrollment ends at a revoke of the
  pod's login: after a removal anyone signed in may revoke it, since the login is a service's
  again, and after a move only the second person may. It also ends once the pod stops renewing it,
  when its lease lapses, as `BROKER_LEASE_SECONDS` and `BROKER_SWEEP_SECONDS` say in the
  [configuration reference](/legion/broker/reference/config/)
  ([when no broker is serving](/legion/broker/concepts/#when-no-broker-is-serving));
- after a move, the pod enrolling again in the same slot with the same key gets that old
  enrollment back, and another key in that slot is refused `409 ALREADY_ENROLLED`. So in that slot
  the second person's access starts only once the old enrollment ends, and a new slot is a new
  enrollment, the second person's. `agent-secrets whoami` reads the enrollment's record, so for the
  old enrollment it still names the first person;
- the grant log (`GET /v1/grant-log`) and the record of each request's approval keep naming the
  first person, since they read the enrollment's record. Their **Live grants** (`GET /v1/grants`)
  still finds the pod's grants, which it selects by that record, but names no operator on them,
  since the pod no longer acts for them: they see each one they may see, and end it only if they
  approved it or its approver is `anyone`, withholding nothing from the pod. A grant the policy
  gave the pod without asking names no approver either, so it is listed, as a service's pod's is,
  to every signed-in person who may see it, and its row reads **Ends with its machine login**:
  revoking the pod's login ends it, as its expiry does. A grant of a private project the first
  person is not on, which the pod may get through its exchange entry, is hidden from them as from
  anyone outside the project, and is on the pages of that project's people. Each new request's
  `request.created` row records, in its own detail, the person the broker decided for: none.

After a move, have the second person revoke the pod's login on Dispatch's machine-login page, which
ends the old enrollment, or replace the pod.

A broker of an earlier release reads a person's pod login as that person's own machine login, since
it holds an operator. Such a broker lets the login enrol a session under the name of one of the
person's box sessions and take that session's grants, end the person's box sessions, and call the
four operator routes above. It also answers the pod's enrollment as the person whatever this
setting says. A roll serves the previous release beside this one against one database
([running more than one pod](#running-more-than-one-pod)), so:

- set `BROKER_POD_OPERATORS` only in a deploy after every broker replica runs this release;
- before you roll the broker back to an earlier release, remove every entry and redeploy, then
  revoke on Dispatch's machine-login page each login that a mapped account made, which anyone
  signed in may do once its entry is gone and which ends the enrollments that login made. Roll
  back only after that. The pods log in again with no person, so no login or enrollment that
  carries a person is left for the earlier release to read.

A pod trades its own projected ServiceAccount token, minted for `BROKER_EXCHANGE_K8S_AUDIENCE`, at
the token exchange for a Dispatch access token. `BROKER_EXCHANGE_ACCOUNTS` lists the accounts whose
tokens write as one person. A token of an entry that names a project names that project, and
writes each object of another project that a token naming no project may write, except in a private
project it does not name; a write that belongs to the deployment is refused it
(`403 PROJECT_SCOPE_DEPLOYMENT`). A token of an entry that names no project
(`<service account>==<person>`) writes wherever Dispatch's own rules let that person write. An
entry may name neither a person nor a project (`<service account>==`, or
`<service account>===<session>` to fix its session): its tokens name no person and no project and
name the account as their service, so they act as the account itself, write wherever Dispatch's own
rules let a credential naming no project write, and are recorded under that account. That includes
the writes that belong to the deployment rather than to one project and need no OAuth scope:
priorities (`POST`, `PATCH` and `DELETE /api/v1/priorities`), a new project with its default
assignee (`POST /api/v1/projects`), the delivery settings (`PUT /api/v1/settings/delivery`), a
project's Legion daemon URL (`PUT` and `DELETE /api/v1/projects/{key}/legion/daemon`) and default
repository (`PUT /api/v1/projects/{key}/default-repository`), and the delivery session groups
(`POST`, `PATCH` and `DELETE /api/v1/delivery/session-groups`). The broker refuses to start on an
entry that names a project and no person: an entry that names a project names its person, so no
account entry's token that names a project is personless. A broker of a build older than the entry
naming no person refuses to start on one, and an Envoy listener or a Dispatch of a build older than
it answers such a token `401`, so deploy the broker, Dispatch and every Envoy listener before you
write one. For the same reason, before you roll the broker, Dispatch or an Envoy listener back to a
build older than this release, remove every entry that names no person and redeploy the broker: an
older broker refuses to start on such an entry and restarts in a loop, and an older Dispatch or
listener answers the account's tokens `401`. Bring the session images' pi-envoy and claude-envoy
builds that carry the same release's `dispatch` command there first too: an older `dispatch whoami`
ignores Dispatch's `sign_in_required` and tells such a session that its writes are refused, though
they succeed.
An entry whose fixed session is `legion-daemon:<its own project>` is a Legion daemon's own, for
example `system:serviceaccount:example-ns:example-daemon=EXAMPLE=ada@example.com=legion-daemon:EXAMPLE`.
The token of such an entry holds the OAuth scope `legion-status` alone, which Dispatch reads
(`DISPATCH_LEGION_STATUS_SCOPE`): where that setting is on, a write of a Legion issue's status
needs it or the `person` scope, but for a status write of the architect of an issue above,
which writes with its own broker token. The broker reads the session the entry fixes, letter case
included, and never a session a request names. No other token the exchange mints holds
`legion-status`, whatever session it carries, and a request that names that scope is refused
`invalid_scope` under every entry.
`BROKER_EXCHANGE_READ_ACCOUNTS` lists the accounts whose tokens only read, as whitespace-separated
`<service account>=<service>=<session>` entries, for example
`BROKER_EXCHANGE_READ_ACCOUNTS=system:serviceaccount:example-ns:example-reader=example-reader=example-reader-session`.
The token of such an entry, traded with no scope, holds the OAuth scope `read` alone and the project
scope `none`, names that service and that session, and names no person and no project. Dispatch
serves it on the GET routes it classes as reads, and answers it `403 READ_ONLY_TOKEN` on every other
route. That token reads every read of Dispatch that needs no OAuth scope, for every project: each
issue, closed and iceboxed ones included, with its documents, comments and asks. No setting holds a
read entry to some projects. The Envoy listener answers it `401`. Both settings need
`BROKER_K8S_OIDC_ISSUER` and `BROKER_EXCHANGE_K8S_AUDIENCE`, and the broker refuses to start on a
read entry without the audience. It also refuses an entry with a missing field, a namespace's
`default` account, a service that is not lowercase letters, digits and hyphens of at most 64 or is
`shared`, and an account that one setting lists twice or that both settings list. A broker of a
build older than this setting does not read it, so it mints those accounts no token. The boot line
`token exchange state=on` counts the read entries as `read_accounts`.

A job that writes to Dispatch for no person, such as a sweeper or a release workflow, gets a job
entry in place of an entry that names a person. `BROKER_EXCHANGE_JOB_ACCOUNTS` lists a pod's
service account, as whitespace-separated `<service account>=<projects>[=<session>]` entries, and
`BROKER_EXCHANGE_JOB_WORKFLOWS` lists a GitHub Actions workflow of one repository on one ref, as
`<repository>=<workflow path>=<ref>=<projects>[=<session>]` entries, for example:

```sh
BROKER_EXCHANGE_JOB_ACCOUNTS=system:serviceaccount:example-ns:example-job=EXAMPLE
BROKER_EXCHANGE_JOB_WORKFLOWS=example-org/example-repo=.github/workflows/sweep.yaml=refs/heads/main=public,PRIVA=example-sweep
```

`<projects>` is one project key, or `public` followed by `,<key>` for each of at most 16 private
projects. The token the exchange mints for a job entry names the entry's identity as its service
(the account, or `<repository>/<workflow path>@<ref>` for a workflow) and no person, holds no OAuth
scope, so a route that needs `person` or `approve` refuses it `403 OAUTH_SCOPE_REQUIRED` before
Dispatch's project check runs, and carries the entry's projects whatever the request names: under
the project scope `named` its one project, or under the project scope `public` its private
projects, beside which it reaches every public project. A request may name no project or only
projects of the entry, and the broker refuses one that names any other, a public project's key
under `public` included, since it cannot tell a public project's key from a key no project holds.
The broker asks Dispatch nothing for such a token: the entry chose its projects. Dispatch records
each write of the token under its session, for its service and under no person. Dispatch's
project check judges a `named` token as it judges a person's `named` token for the same project:
it writes another project's objects as a token that names no project does, and an object of a
private project that the entry does not name gets the answer of an object that does not exist. A
write that belongs to the deployment, such as a project create, is `403 PROJECT_SCOPE_DEPLOYMENT`
for both. At this release Dispatch does not read a `public` token's list, so such a token writes
wherever a token that names no project may. An entry that fixes no session takes the session the
trade names, and the broker refuses a trade that names none; a daemon may register a pod of a job
account whose entry fixes none (above).

The broker refuses to start, naming the entry, on an entry of either setting not of its form or
with a blank field, a pod account that is not `system:serviceaccount:<namespace>:<name>` or is a
namespace's `default` account, an identity listed twice, and projects of any other spelling. It
also refuses an account that `BROKER_EXCHANGE_JOB_ACCOUNTS` and `BROKER_EXCHANGE_ACCOUNTS` or
`BROKER_EXCHANGE_READ_ACCOUNTS` both list, and a workflow that `BROKER_EXCHANGE_JOB_WORKFLOWS` and
`BROKER_EXCHANGE_WORKFLOWS` both list, since one identity has one answer, and an account
`BROKER_POD_OPERATORS` maps to a person (above). `BROKER_EXCHANGE_JOB_ACCOUNTS` needs
`BROKER_K8S_OIDC_ISSUER` and `BROKER_EXCHANGE_K8S_AUDIENCE`, and `BROKER_EXCHANGE_JOB_WORKFLOWS`
needs `BROKER_EXCHANGE_GITHUB_ISSUER` and `BROKER_EXCHANGE_GITHUB_AUDIENCE`. A broker of a build
older than these settings does not read them, so it mints those identities no token. The boot line
`token exchange state=on` counts their entries as `job_accounts` and `job_workflows`.

A GitHub Actions job trades its run's own token, with no stored secret. Give the job the
permission `id-token: write`, so that GitHub sets `ACTIONS_ID_TOKEN_REQUEST_URL` and
`ACTIONS_ID_TOKEN_REQUEST_TOKEN` in it, and set `AGENT_SECRETS_URL`,
`AGENT_SECRETS_MACHINE_IDENTITY=github-actions` and `AGENT_SECRETS_MACHINE_AUDIENCE`, which must
equal the broker's `BROKER_EXCHANGE_GITHUB_AUDIENCE`: the client asks GitHub for a token of that
audience, and the broker refuses a token of any other. Each trade asks GitHub for a new token,
since the broker takes each GitHub token once. `dispatch api --path <route>`, the `bin/dispatch`
of `@sjawhar/codex-envoy` or `@sjawhar/pi-envoy`, then sends the job's requests to Dispatch under
the traded token: a `GET`, or with `--method POST`, `PUT`, `PATCH` or `DELETE` a write, whose body
`--body-file` reads from a file or, as `--body-file -`, from standard input. `agent-secrets`
refuses `github-actions` (exit 2), as it refuses `kubernetes`, since the broker mints no launcher
credential for a workflow run.

No setting turns on a read trade. Every trade the exchange takes but a job entry's may ask for
the scope `read` (`scope=read`), and its token keeps the identity the same trade would have had
and writes nothing ([Concepts](/legion/broker/concepts/#sessions-and-enrollment)). Such a trade may name a cut
(`as_of`), an RFC 3339 time with at most six fractional digits, no later than the trade. The broker
refuses each of these with `400 invalid_request`, naming what is wrong:

- A cut on a trade that does not ask for `read`, a cut later than the trade, a cut at the zero
  time (`0001-01-01T00:00:00Z`, under any offset), which would read as no cut, and a cut in any
  other form.
- `launcher_credential=true` on a read trade, which makes, moves and extends no credential.
- A read trade of a job entry's identity: a job's token is minted whole, with no OAuth scope.
- A `DPoP-Holder` header on a trade that does not ask for `read`, a holder proof that does not
  verify, and a holder proof signed with the DPoP proof's own key.
- A person's read trade with no `DPoP-Holder` proof.
- A read token that would be bound to a key that a live launcher credential is bound to, a read
  entry's token traded with no scope included. The answer names the cure.

A person's read trade whose sign-in holds no launcher credential yet is refused
`400 invalid_grant`, naming `agent-secrets login`. One whose credential has lapsed is refused
`400 invalid_grant` too, naming the cure: one ordinary trade of the sign-in, which `agent-secrets`
makes and which renews the credential. A driver that renews a read token for a copy elsewhere
keeps the sign-in trading ordinarily as well. The other side of the key rule is refused where a
key another live credential holds is refused, with a code or a hint of its own that names the read
token and the cure, a new key: a device login's approval and a token login whose key a live read
token is bound to are `409 KEY_BINDS_READ_TOKEN`, beside the `409 KEY_HOLDS_LIVE_CREDENTIAL` of a
key another live credential holds, and a Google machine's or a person's trade is
`400 invalid_grant`.

The broker keeps a token narrowed to reading in a table of its own, which migration 0042 adds
(`oauth_narrowed_access_tokens`), and decides at each introspection whether it is still live: its
own row, the launcher credential it names and, for a machine login's trade, that login's access
token must all be live. A broker of an older build reads nothing of that table, so while the
broker rolls, an older replica answers a narrowed token's introspection inactive and refuses it as
a subject token, and a revoke or a retire that replica makes still ends the token. A Dispatch or a
listener of an older build refuses a narrowed token's project scope, so it answers the token `401`.

A machine holding a Google identity token — a Compute Engine VM, a pod on Google Kubernetes
Engine, or anything a workload identity pool mints one for — trades that token at the broker's
token exchange for a Dispatch access token and the machine login that trade mints. It holds no
stored credential and no person approves it. Set the Google audience and list each exchanging
account as a VM service. The remaining fleet settings are optional, subject to these checks:

- `BROKER_GOOGLE_OIDC_AUDIENCE`: the audience the machines ask their metadata server for. The
  issuer is Google's, `https://accounts.google.com`, unless `BROKER_GOOGLE_OIDC_ISSUER` names a
  stand-in for a dev stack; the broker reads its keys at startup only when the audience is set,
  and then logs one `google machine identity` line naming the issuer, the audience and the machine
  services. Unset, the broker reads and logs nothing about Google and takes no such token. Set, it
  needs a Google caller, or the broker refuses to start: a `google:` entry in
  `BROKER_API_ACCOUNTS`, a VM service in `BROKER_VM_SERVICES` or a `google:` edge-listener entry
  in `BROKER_NATS_SERVICES`.
- `BROKER_VM_SERVICES`: whitespace-separated `<service>=<service account email>` entries. Use a
  dedicated user-managed account for each fleet: the broker refuses a project's default Compute
  Engine or App Engine account, which every workload in the project runs as by default. Without a
  listed VM service, no Google identity token can be exchanged for a machine login. An API caller
  alone needs no VM service, but `BROKER_SIGNIN_DEVICE_AUDIENCE` does: the person's device sign-in
  presents a machine's identity token as its actor.
- `BROKER_VM_PROJECTS` and `BROKER_VM_INSTANCES`: `<service account email>=<project id>` and
  `<service account email>=<instance-name glob>`, in Go's `path.Match` syntax. They pin a fleet to
  one Compute Engine project and the instance names its machines have. The two go together: set
  both for an account, or neither. An account pinned in neither takes its machines wherever they
  run, a token carrying no `google.compute_engine` claims included, which is what a workload
  identity pool and a pod's metadata server mint.
- `BROKER_VM_EXCHANGE_ACCOUNTS`: `<service account email>=<project>=<person>[=<session>]`, the
  person and the Dispatch project a fleet writes for. An account `BROKER_VM_SERVICES` lists and
  this setting does not gets a token that names its service and no person, which Dispatch and the
  Envoy listener read. Either way it reaches the fleet's Dispatch token and never its
  secrets: the machine login the exchange mints names no person, so a machine reads what its
  service owns and the shared agent-tier secrets, and a person reaches their own on that machine
  only by signing in on it. The person an entry names is also whose Inbox lists a request one of
  the fleet's machines makes for a shared human-tier secret; an account with no person leaves such
  a request in every person's Inbox.
- `BROKER_VM_EXCHANGE_JOB_ACCOUNTS`: `<service account email>=<projects>[=<session>]`, a job's
  projects in place of a person, spelled as `BROKER_EXCHANGE_JOB_ACCOUNTS` spells them, for an
  account `BROKER_VM_SERVICES` lists. A machine of that account trades for a job's token, which
  names the account's address as its service and no person; the machine login that trade mints
  still names the fleet's service. Every machine that runs as the account then writes to Dispatch
  as the job, for no person, so list an account here only when every machine that runs as it is
  that job's. The broker refuses to start on an account this setting and
  `BROKER_VM_EXCHANGE_ACCOUNTS` both name, an account `BROKER_VM_SERVICES` does not list, and
  projects of any other spelling. The boot line counts these entries as `job_machines`.

One fleet's entry, `example-fleet`, whose machines run as one account in `example-project`, are
named `example-vm-...`, and write for one person:

```sh
BROKER_GOOGLE_OIDC_AUDIENCE=example-vm-audience
BROKER_VM_SERVICES=example-fleet=example-fleet@example-project.iam.gserviceaccount.com
BROKER_VM_PROJECTS=example-fleet@example-project.iam.gserviceaccount.com=example-project
BROKER_VM_INSTANCES=example-fleet@example-project.iam.gserviceaccount.com=example-vm-*
BROKER_VM_EXCHANGE_ACCOUNTS=example-fleet@example-project.iam.gserviceaccount.com=EXAMPLE=ada@example.com
```

A machine of that fleet then reads the secrets `example-fleet` owns and the shared agent-tier
ones, and asks for the rest. `ada@example.com` is on the Dispatch token it trades for, not on its
machine login: her own secrets stay hers until she signs in on that machine.

When she does — `agent-secrets login`, which needs `BROKER_SIGNIN_DEVICE_AUDIENCE` set — that
sign-in's trade mints a machine login of its own on that box, carrying her rather than the fleet,
and a session enrolled under it reads her secrets and the shared agent-tier ones and nobody else's.
It is bound to the box key the command makes there, so her sign-in on one machine is not her
sign-in on another. Revoking that sign-in (`POST /v1/person-signins/{id}/revoke`, which Dispatch
relays from her page) ends the access tokens it minted and the machine login with them, so every
session it enrolled ends at once; the fleet's own machine login, and her sign-ins on other
machines, are untouched.
That revoke ends the box key, not her grant at Google: `agent-secrets login` keeps Google's
refresh token on the machine beside the key, and whoever can read that machine's files can copy
both and sign in again from elsewhere, as a sign-in of its own that this revoke does not reach.
To cut off a box you believe is compromised, have her revoke the grant at Google — her Google
Account's third-party connections page, the entry for the deployment's `agent-secrets login`
client — which ends every copy of that refresh token at once; the broker's own revoke is for a
box she has finished with.
A sign-in belongs to the machine it was made on: the same box key presented with another
machine's identity token is refused `invalid_grant` naming `agent-secrets login`, since the
machine login it holds is keyed by the person, the machine and the key together.
The broker refuses an exchange whose token is for another audience or does not verify, whose email
Google does not hold verified, whose account no entry lists, or whose Compute Engine project or
instance name the entry's pins do not allow, each `400 invalid_request` naming what did not match.

Two settings turn a person's sign-in on. `BROKER_SIGNIN_DEVICE_AUDIENCE` is the OAuth client id of
the device-flow client `agent-secrets login` runs Google's device authorization grant as, which is
the audience a person's own Google identity token must carry, since Google puts a client's id in
the `aud` of the ID tokens it mints for it; set it to the same client id the machines are given as
`AGENT_SECRETS_SIGNIN_CLIENT_ID`. `BROKER_SIGNIN_DOMAIN`, which the approval sign-in already
needs, is the Workspace domain the person's `hd` claim must name, so a person outside it signs in
nowhere. The issuer of both the person's token and the machine's is `BROKER_GOOGLE_OIDC_ISSUER`,
`https://accounts.google.com` unless a dev stack names a stand-in, and the audience setting needs
`BROKER_GOOGLE_OIDC_AUDIENCE` and a VM service in `BROKER_VM_SERVICES`, whose machines are the
boxes a person may sign in on. Unset, the broker accepts no person's sign-in and refuses either
token type by name.

Each machine holds a launcher credential of its own, keyed by the machine and the key its proof
presented: `<account>@<project>/<zone>/<instance id>#<key thumbprint>` for a token minted on a
Compute Engine instance, and `<account>#<key thumbprint>` for one minted anywhere else. The key is
part of the name because the identity token is a bearer: a copy of it presented with a key of the
copier's asks for a credential of its own and moves nothing of the machine's. So one machine's
trade never moves another's credential, and ending one machine's enrollment leaves the others.
`agent-secrets-helper` keeps that key in `~/.config/agent-secrets/identity-key.pem`, mode 0600,
beside the machine login a machine that proves no identity keeps, and makes one only at this
machine's first trade. A restart therefore trades the same subject and moves the one credential
the machine already holds, so the sessions enrolled under it keep renewing. A machine that makes
a new key — one whose key file was removed, or a machine rebuilt — leaves its old credential
behind until that credential's own expiry, and starts a new one at its next trade; the sessions
enrolled under the old one stop renewing when it lapses, and a box enrolled with
`agent-secrets enroll --helper` is enrolled again by running that command again.

Only a trade that asks for a credential holds one, and a trade asks unless it says otherwise, so
`agent-secrets` and the helper hold theirs by doing nothing. A trade that asks for the scope `read`
is the exception: it never asks, makes, moves or extends a credential, and it is refused
`400 invalid_request` when it sends `launcher_credential=true`. A person's ordinary trade cannot
decline: a sign-in holds its credential, so a trade of a person's that declined one is refused
`400 invalid_request`. The clients that
want a Dispatch token and read no secret — the `dispatch` command, the Oh My Pi and Claude Code
plugins, `envoy-send` — decline it, since each of them proves a key of one process, or a pod
role's own agent-secrets key,
and a machine-logins list with a row per command run is a list nobody can read. So the machines
on that list are the boxes that enrol sessions, not every process that ever traded.

The credential expires with the token it was minted from, an hour after that token was minted, and
a machine's session renews only while that credential is live: not revoked, not past its expiry,
and its account still listed for its service. A machine that stops being able to prove its
identity therefore stops renewing within the hour, and its sessions lapse one
`BROKER_LEASE_SECONDS` later; a copy of a session's key and enrollment id taken off the box renews
nothing once the box's login has ended. The broker answers such a renewal `401
ENROLLMENT_NOT_LIVE` and logs `broker: machine session renewal refused: its machine login has
ended`. Only a machine's own session is held this way: a person's session on that box renews until
they revoke the sign-in, which ends it at once, and a pod's renewal proves the pod's own projected
token.

Dispatch's machine-login page lists each machine's login, its host and no
approver, and anyone signed in may revoke it, which ends that machine's enrollment; the machine
trades again at its next call. A revoke ends the enrollment, not the Google identity token: someone
who copied that token off the machine trades it again within its hour, under a machine login of
their own key, and reads what that fleet's service may read. To end a copied token for certain, take
the machine out of its fleet and wait out the hour, as [a copied machine
token](#a-copied-machine-token) says. To stop a fleet trading, remove its entries and redeploy the
broker: each machine's credential stops authenticating at its next call, its next trade is
refused, and its sessions stop renewing.

A revoke alone does not cut off a machine whose key may have been copied. The credential is
keyed by the machine and the key it proved, and `agent-secrets-helper` keeps that key at
`~/.config/agent-secrets/identity-key.pem` across restarts, so the box's next trade — within the
hour, since the credential lives only as long as the identity token it was minted from — mints a
new live credential bound to that same key, and whoever holds a copy of the file signs launcher
proofs under the new credential's id. Three steps, in this order, end that:

1. remove `~/.config/agent-secrets/identity-key.pem` on the machine;
2. restart the helper (`systemctl --user restart agent-secrets-helper.service`, or
   `agent-secrets-helper install --restart-with-sessions`, which also ends the grants of the
   sessions registered with it), so it makes a new key and its next trade holds a credential of a
   subject of its own;
3. then revoke the old login on the machine-login page, by the credential id the old key held,
   which ends it and every session it enrolled.

In the other order the revoke is undone: the helper still holds the key, and its next trade
inserts a live credential on it again, since a revoked row is no longer the one a trade of that
subject moves. None of this reaches the machine's Google identity token, which is what the trade
itself proves: a box you believe is compromised comes out of its fleet, as [a copied machine
token](#a-copied-machine-token) says.

Whoever can get an identity token for the fleet's account logs in as its machines. Google Cloud IAM
is that boundary: who may run a workload as the account (`iam.serviceAccounts.actAs` on it), and
who may run code on a machine of the fleet, which can fetch that machine's token.

On the machine itself there is no enrollment command to run and no key to place: `agent-secrets`,
`agent-secrets-helper` and the Legion controller trade the identity the machine already holds
whenever they find no key directory, no sign-in and no helper session. `agent-secrets` needs no
setting for it beyond `AGENT_SECRETS_URL`, the broker's public URL: it reads the broker's Google
exchange audience from `GET /v1/settings` and finds the identity that can carry it, an
`external_account` credential file or the metadata server
([where a command takes its identity](/legion/broker/concepts/#where-a-command-takes-its-identity)).
The helper and the Legion controller detect nothing; they read `AGENT_SECRETS_MACHINE_IDENTITY`,
which also makes `agent-secrets` trade that one identity and no other, and which says
which identity this machine proves: `metadata` with `AGENT_SECRETS_MACHINE_AUDIENCE`, a Compute
Engine machine's own token from its metadata server, which is the fleet above; `kubernetes` with
`AGENT_SECRETS_MACHINE_TOKEN_FILE`, a pod's projected service-account token, which `agent-secrets`
refuses (exit 2) since the broker mints no launcher credential for it, so no session could be
registered under it; `github-actions` with `ACTIONS_ID_TOKEN_REQUEST_URL`,
`ACTIONS_ID_TOKEN_REQUEST_TOKEN` and `AGENT_SECRETS_MACHINE_AUDIENCE`, a GitHub Actions run's
token (above), which `agent-secrets` refuses in the same way; or `pool`, a token
minted through a workload identity pool, which takes all four of
`AGENT_SECRETS_MACHINE_TOKEN_FILE`, the projected service-account token the pool federates as the
external credential; `AGENT_SECRETS_POOL_AUDIENCE`, the pool provider that credential is
presented to, Google's full
`//iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/<pool>/providers/<provider>`
resource name; `AGENT_SECRETS_POOL_ACCOUNT`, the service account the pool lets that credential
act as, by email address or unique id, which `generateIdToken` is called on; and
`AGENT_SECRETS_MACHINE_AUDIENCE`, here the `aud` of the token that mint returns, the client id the
broker checks against `BROKER_GOOGLE_OIDC_AUDIENCE`. A `pool` machine missing any of the four
trades nothing and its helper restart-loops, as Azure dev boxes did for want of the token file
(example-org/deployment#2200). `AGENT_SECRETS_SESSION` names the session a trade writes under
for a caller that acts as no session of its own — `agent-secrets`, the helper, a daemon — and is
the only name such a caller has. A caller that is a session writes under its own session id and
reads that variable for nothing: an agent session's id is minted when the session starts, and its
token must name it, since the Envoy listener refuses a subscribe, an unsubscribe, a role claim or a
send from a session whose token names another. So a fleet whose entry fixes a session, or whose
pods write under registered names, is a fleet no agent session can trade at: the broker refuses
the trade, naming the session the entry's tokens carry. For a person to sign in there, give the
machine the device-flow client as `AGENT_SECRETS_SIGNIN_CLIENT_ID` and
`AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE`, the client whose
id is `BROKER_SIGNIN_DEVICE_AUDIENCE`; `agent-secrets login` finds the machine's identity the way
a command does, so it needs no `AGENT_SECRETS_MACHINE_IDENTITY` either. With
`AGENT_SECRETS_MACHINE_IDENTITY` unset the helper and the controller trade nothing and work as they
did, through a machine login a person approves, and `agent-secrets` trades only once the broker
names a Google exchange audience, which a broker with no `BROKER_GOOGLE_OIDC_AUDIENCE` never does.
The broker names its Google audience only where
`BROKER_VM_SERVICES` lists a machine: a Google audience set for API callers alone
(`BROKER_API_ACCOUNTS`' `google:` entries) is not published, since its exchange takes no machine.
`agent-secrets` reads those settings only from an `https` broker, or an `http` one on loopback,
and keeps the first Google audience it finds an identity for, refusing a later one that differs.
That file is per user: change `BROKER_GOOGLE_OIDC_AUDIENCE` on a deployed broker only together
with removing `agent-secrets/machine/google-exchange-audience` from each user's state directory
(`$XDG_STATE_HOME`, else `~/.local/state`) on each machine whose `agent-secrets` has used its own
identity.

## Earlier clients' github_app field

An `agent-secrets` of a release before `--target` narrows a stored `github-app` secret with
`--github-owner`, `--github-repo`, `--github-read-only` and `--github-token`, and sends that
narrowing in the `github_app` field of its request and of its release. The broker answers it, so
those clients go on running while the deployment moves to a client with `--target`.

**Which Apps leave the airlock.** `BROKER_GITHUB_APP_FIELD_SECRETS` names the stored
`github-app` secrets whose release this answer hands over outside the airlock, separated by
commas or spaces; unset, it names none and the field is answered for no secret. The answer is
outside the airlock: it hands over the minted token, not a placeholder, whatever kind the secret
is behind. The broker this field comes from refused a narrowed release of a name its airlock
held, so a deployment names the Apps that broker handed over raw and no others. Each entry is an
agent-secret name, and the start is refused for anything else.

**Who it is answered for.** `BROKER_GITHUB_APP_FIELD_REQUESTERS` names them: registered services
and people, separated by commas or spaces, such as
`BROKER_GITHUB_APP_FIELD_REQUESTERS=example-service,ada@example.com`. A person asking for their
own App, which the stored secret's `owner` tag names, is answered besides. **Unset, the broker
answers no service at all**, so a deployment's own value is what turns the answer on for its
pods. This list, and not the profiles file alone, decides: a profile with no `requesters` list is
granted to every agent-tier caller, and this answer hands over a real token outside the airlock,
which such a caller's own `--target` request never gets.

**What it answers.**

- The request is served as one profile of the stored secret it names: of that secret's own profile
  and the profiles whose `source` it is, in the profiles file's order, one the file grants the
  caller at once and whose scope holds the narrowing's account and repositories. Of those, the one
  whose permissions hold the most at `write`, then the greatest sum of its permissions' levels;
  the stored secret's own name wins a tie before the profiles of it, and each profile before the
  ones after it in the file. Where none is, the request is refused
  `400 GITHUB_APP_FIELD_REMOVED`: there is no wider path. One check decides for the request and
  for its release alike, and neither admits what the other refuses. A request that would wait for
  a person is refused too.
- The request records that profile, the narrowing as its target, and that it was made this way,
  with the field's `read_only`. A live grant of one form is never handed to the other: a
  `--target` request takes no such grant, and a narrowed request takes no `--target` grant.
- The release hands over the token in the `installations` form under the stored secret's name,
  narrowed to the account and repositories, **outside the airlock**, and echoes `github_app`, as
  legion-envoy-v33.7.0 answered. A read-only body gets each of that broker's read set —
  `actions`, `checks`, `contents`, `metadata`, `pull_requests` and `statuses` — at read where the
  profile holds it, and none it does not; any other gets the profile's permissions. Each is cut
  to what the installation holds.
- Such a grant releases only through that field, with the same `read_only`: a release that drops
  it, or that asks for another `read_only`, is refused and hands over nothing, so neither widens
  the token the request was granted. A `--target` grant, including one a person approved, is
  refused the field, so no grant of a client with `--target` is released past the airlock.

**Rolling it out and back.** A broker of the release before this one reads a narrowed grant's row
without the two columns the answer records, so it releases that grant without the field, as the
profile's own value, and the recorded `read_only` has no effect there; that release gives no
permission the policy does not grant the caller at once. So set `BROKER_GITHUB_APP_FIELD_REQUESTERS` and
`BROKER_GITHUB_APP_FIELD_SECRETS` only once no replica of an earlier broker serves releases, and before you roll the broker back, take
`BROKER_GITHUB_APP_FIELD_SECRETS` out, redeploy, and wait until every grant made from the field
has ended.

Each answer logs `broker: answered the github_app field of an earlier agent-secrets` (below), so
you can count the clients that still send the field. Once none does, set
`BROKER_GITHUB_APP_FIELD=off` and roll the broker: the field is then refused
`400 GITHUB_APP_FIELD_REMOVED`, whoever sends it, as before this answer. The setting takes `off`
or nothing; any other value refuses the start.

## Approval sign-in

A person approves or denies a secret request on the broker's own page,
`<BROKER_PUBLIC_URL>/approve/<record>`, signed in to the broker with OpenID Connect through an
OAuth client of the broker's own; Dispatch's Inbox only links there, and no route a service calls
decides a secret request ([Concepts](/legion/broker/concepts/#approvals)). Four settings are the
sign-in, and every one of them is required unless `BROKER_SIGNIN=off`:

| Setting | What it is |
| --- | --- |
| `BROKER_SIGNIN_ISSUER` | The provider's issuer, whose discovery document the broker reads at startup and every hour after, in a check of its own ([What the sign-in is doing](#what-the-sign-in-is-doing)), and for the page at the first sign-in each process serves, which it keeps once a read succeeds; until then a sign-in reads it again at most every 5 seconds. A read that fails stops neither the boot nor the machines the broker serves, so a provider outage never stops it serving them. `https://accounts.google.com` for Google. An absolute `https` URL, or `http` only to a loopback host, for a run on one machine. |
| `BROKER_SIGNIN_CLIENT_ID` | The broker's own OAuth client at that provider. Not Dispatch's: a token or code made for Dispatch's client signs no one in to the broker. |
| `BROKER_SIGNIN_CLIENT_SECRET_FILE` | A file holding that client's secret, read at startup and trimmed; mount it from a Kubernetes Secret. The broker refuses to start while `BROKER_SIGNIN_CLIENT_SECRET` is set, so the secret is never in its environment. |
| `BROKER_SIGNIN_DOMAIN` | The Google Workspace domain a person must belong to: the ID token's `hd` claim must name it, and its email must be verified. A person's login is their lowercased email, the form a secret's `owner` tag names. |
| `BROKER_SIGNIN_SESSION_SECONDS` | Optional. How long a sign-in lasts, counted from the moment it is made and never extended: 86400 (24 hours), the default, at most. Signing out ends it at once. A person whose Workspace account is suspended keeps approving until their session's expiry, which `broker end-sessions` ends sooner ([Ending a person's sessions](#ending-a-persons-sessions)). |

A broker missing any of the four refuses to start, naming the ones it is missing: `broker: fatal
error="the broker's approval page needs BROKER_SIGNIN_ISSUER, BROKER_SIGNIN_CLIENT_ID,
BROKER_SIGNIN_CLIENT_SECRET_FILE, BROKER_SIGNIN_DOMAIN; missing BROKER_SIGNIN_DOMAIN"`. The one
broker that starts without them is one told outright to sign nobody in, `BROKER_SIGNIN=off`
([A broker that signs nobody in](#a-broker-that-signs-nobody-in)): a deployment either signs
people in, says it signs nobody in, or does not run.

On Google, create the client once, in the Google Cloud project whose OAuth consent screen is the
Workspace's **Internal** one (APIs & Services → Credentials → Create credentials → OAuth client ID,
application type **Web application**), with one authorized redirect URI:
`<BROKER_PUBLIC_URL>/signin/callback`, for example `https://secrets.internal.example/signin/callback`.
Its client id is `BROKER_SIGNIN_CLIENT_ID`, and its secret goes in the file
`BROKER_SIGNIN_CLIENT_SECRET_FILE` names. The broker asks for `openid email profile` with PKCE,
and asks for no offline access and keeps no refresh token. A browser with no session is sent to
sign in with no `login_hint`: the page reads no record before a person signs in, so the redirect
is the same for every record id and says nothing of whether the record exists or whose it is. A
person signed in to one Google account is asked nothing; one signed in to several is shown
Google's account chooser. The page's **Sign in as someone else** link, which a person already
signed in follows, opens that account chooser (`prompt=select_account`) with the record's
approver named in it as `login_hint`. It names none for a request Dispatch could not answer for,
and a request the person may not see answers as an id no record has.

Deploy the broker with these settings before the Dispatch release whose Inbox links the page. In
between, an open Dispatch tab's **Approve** on a secret request gets the broker's
`403 DECIDE_ON_BROKER_PAGE` naming the page, and nothing is approved on Dispatch's word.

### What the sign-in is doing

`GET /signin/status` answers what this deployment's sign-in has done, read from Postgres on every
call, so every pod answers the same thing. It takes no credential, and every word in it is one of
two sets the broker itself chose: no provider's message, no login and no secret reaches it.

```json
{
  "client_check": "ready",
  "client_checked_at": "2026-03-04T09:00:11Z",
  "last_sign_in_at": "2026-03-04T08:41:02Z",
  "last_failed_sign_in_at": null,
  "last_failed_sign_in_reason": null,
  "last_decision_at": "2026-03-04T08:41:40Z"
}
```

A fact that has never happened is `null`, so a deployment nobody has signed in to yet is not a
deployment whose sign-in broke. What an operator reads from each field:

- `client_check` and `client_checked_at`: what the broker's own client was found to be at the
  provider, and when. Each pod checks it at startup and every hour after, by asking the token
  endpoint to redeem an invalid code with the broker's client credentials. The status of the
  answer alone decides. `ready` is the provider refusing that code (`invalid_grant` at a refusal's
  status, a `4xx` that is no `408` or `429`), which it does only for a client it accepts.
  `client_rejected` is the provider refusing the check's request (a random code, no redirect URI,
  no verifier) at a refusal's status for any other reason or none (`invalid_client`, another OAuth
  error, or no error at all); a wrong client id or secret is the usual cause, and the log line
  names the status and the error. `issuer_unreachable` is a check with no verdict on the client:
  an outage (`408`, `429` or a `5xx`, whatever OAuth error its body named, an answer cut off before
  its body arrived whole, or no answer), or an answer that is no refusal (a `2xx` naming an error,
  or a `3xx`). The check's log line says which. A check neither answers a person nor stops
  the broker: it writes this row, and it logs only a change. When it finds `client_rejected` or
  `issuer_unreachable` and its pod did not log that state last, it logs `broker: the sign-in client
  check`. When it finds `ready` after such a line, it logs `broker: the sign-in client check finds
  the client ready again` ([Health and logs](#health-and-logs)). So a provider that stays away for
  a day is one line, not one each hour.
- `last_sign_in_at`: when a person last finished signing in. Long silence on a deployment people
  use is the signal, read beside `last_decision_at`.
- `last_failed_sign_in_at` and `last_failed_sign_in_reason`: when a sign-in last failed at the code
  exchange or at the ID-token check, and why, as one of `invalid_client`, `unauthorized_client`,
  `redirect_uri_mismatch`, `invalid_grant`, `issuer_unreachable`, `bad_issuer`, `bad_audience`,
  `bad_signature`, `bad_nonce`, `not_in_domain` and `email_not_verified`. The status of the token
  endpoint's answer alone decides whether it answered, once the answer arrived whole: a code
  exchange it answers `408`, `429` or `5xx`, whatever error its body names, answers with a body cut
  off before it arrives whole, or does not answer, is `issuer_unreachable`, as is a discovery
  document that could not be read; every other answer is the endpoint answering, a failed sign-in
  refused `403`. The first four are that answer's own error, written only from a `4xx`; a `4xx`
  naming no error or another, and a `2xx` naming one, write nothing. The first three
  are the client's registration, `not_in_domain` and `email_not_verified` are the person rather
  than the deployment, and a failure none of the eleven names leaves the row alone and is logged
  instead.
- `last_decision_at`: when a person last approved or denied a request on the page.

### Ending a person's sessions

A broker session lasts `BROKER_SIGNIN_SESSION_SECONDS` from the moment it was made and is never
extended, so a person whose Workspace account is suspended can still approve until it expires. To
end every browser session one login holds, wherever and whenever it was made, run the subcommand
`end-sessions` with that login ([Running a subcommand](#running-a-subcommand) says where):

```text
end-sessions: ended 2 sessions of ada@example.com
```

It writes: it deletes that login's rows of `browser_sessions`, and nothing else. It reads the same
settings the serving process does, so it reaches the same database and signs in to it with the
same login, binds no listener and applies no migration: an image older than the servers cannot
migrate the database behind them on its way to ending a session. The person's next visit to a
request's page signs them in again, if the provider still lets them; to stop that, take them out
of the Workspace or out of `BROKER_SIGNIN_DOMAIN` first.

### Running a subcommand

The broker takes three subcommands, `end-sessions`, `versions` ([A migration recorded under
another file](#a-migration-recorded-under-another-file)) and `profiles check`, which reads the
file `BROKER_PROFILES_FILE` names and needs no setting; the removed `airlock-check` is refused
naming `profiles check`, anything else is refused with a usage line, and `envoy-broker` with no
argument serves. `end-sessions` and
`versions` read the broker's settings and sign in to its database as the server does, and bind no
listener, so either runs in any one-off process of the image that has what the serving broker has:

- a route to the broker's database, such as any proxy the broker's pods reach it through, and the
  serving broker's `BROKER_DATABASE_URL`, with whatever else it signs in to the database with:
  where that URL is an RDS IAM one, the AWS identity the broker mints its tokens from, through the
  AWS SDK's default credential chain; where it signs in by password and the URL holds none, the
  password from `PGPASSWORD` or a passfile;
- `BROKER_PUBLIC_URL` and the secret store's settings, as the serving broker has them
  (`BROKER_SECRETS_STORE`, `BROKER_SECRETS_PREFIX` and that store's key setting), which every
  process of the image requires;
- `BROKER_SIGNIN=off`, in place of the serving broker's sign-in: neither subcommand signs anyone
  in. `BROKER_SIGNIN=off` refuses to start beside any sign-in or judge setting, so leave unset the
  four of the sign-in client, `BROKER_SIGNIN_SESSION_SECONDS` and `BROKER_SIGNIN_DEVICE_AUDIENCE`,
  and the judge's six (`BROKER_JUDGE_MODEL_URL`, `BROKER_JUDGE_MODEL`,
  `BROKER_JUDGE_MODEL_PROTOCOL`, `BROKER_JUDGE_MODEL_AUDIENCE`, `BROKER_JUDGE_DISPATCH_URL` and
  `BROKER_JUDGE_DISPATCH_TOKEN_FILE`), since a broker that signs no one in runs no judge.

Take those from wherever your deployment keeps them, and run the subcommand wherever it lets you
run a one-off process of the image: an exec into a broker pod,
`kubectl -n <namespace> exec <broker pod> -- envoy-broker end-sessions ada@example.com`, or a
one-off pod, job or container of the image with those settings in its environment. `end-sessions`
writes through that login; `versions` writes nothing. Run `end-sessions` with the image that
serves now, and `versions` with the image you mean to roll out.

### A broker that signs nobody in

A deployment whose way of signing people in is not settled yet runs the broker with
`BROKER_SIGNIN=off` and none of the `BROKER_SIGNIN_*` settings. It serves machines and pods as any
broker does and approves nothing.

In a Kubernetes manifest, quote the value. YAML 1.1 reads a bare `off` as the boolean `false`, so
the API server refuses the manifest, or a template renders it as `"false"`, which the broker
refuses (`BROKER_SIGNIN must be off or unset, got "false"`):

```yaml
env:
  - name: BROKER_SIGNIN
    value: "off"
```

Such a broker:

- It serves no approval page: every route under `/approve` and `/signin/`, `GET /signin/status`
  among them, answers 404, and no sign-in client check runs.
- A request naming a secret whose answer would be a person's approval — a human-tier secret,
  another person's `tier=agent` secret, a secret the session's operator withheld from it — is
  denied at once, whatever its reason says, and nothing waits in anyone's Inbox. The session reads
  `<NAME>: a person must approve it, and this broker signs nobody in (BROKER_SIGNIN=off)` after
  `was denied: `, and the command exits 77.
- What the policy grants at once is granted as on any broker: a person's own and a shared
  `tier=agent` secret for their sessions, and a service's own secret for its pods. A machine
  login, which a person decides on Dispatch's machine-login page, is unchanged.
- A request still pending from a broker with sign-in on — one an earlier run left, or one a pod
  still on sign-in queues during a roll — is denied by the next sweep, within
  `BROKER_SWEEP_SECONDS`, and leaves the approver's Inbox: the session reads `was denied: a person
  must approve it, and this broker signs nobody in (BROKER_SIGNIN=off)`, and the request records
  `broker:signin-off` as having decided it. The sweep expires overdue requests first, as on any
  broker, so one whose deadline has passed is normally expired instead; if that expiry fails,
  the same sweep denies it. No clock bounds the selection, so the database's `created_at` cannot
  hide a request from it. A grant approved before stays until it ends as any grant does, and a
  request signed for another broker sharing the database, at another `BROKER_PUBLIC_URL`, is
  left to that broker.
- `BROKER_PUBLIC_URL` is held to an `http` or `https` URL whether sign-in is on or off, by the
  broker's last configuration check: `BROKER_PUBLIC_URL must be an http or https URL, got the
  scheme "ftp"`.

`off` is the one value it takes: any other, `OFF` or `disabled` among them, refuses to start
naming it, `broker: fatal error="BROKER_SIGNIN must be off or unset, got \"disabled\""`. `off`
beside any `BROKER_SIGNIN_*` setting refuses to start naming the ones set, since the deployment
has asked for both: `BROKER_SIGNIN=off turns the broker's person sign-in off, and
BROKER_SIGNIN_DOMAIN is set: set BROKER_SIGNIN=off or the sign-in settings, not both`. `off`
beside the [model judge](/legion/broker/concepts/#the-model-judge)'s settings refuses too, since
the judge decides only a request that waits for a person. With `BROKER_SIGNIN` unset, a broker
missing a sign-in setting refuses exactly as above, so a deployment that lost its sign-in settings
never starts without its approval page.

Moving between the two settings is a roll of the broker's pods, and while pods of both kinds
share the database, a request a person would decide is refused unless it is approved on a pod
with sign-in on, by a person or by that pod's model judge, before the sweep reaches it. A grant
approved that way stays when the switch is made, as any grant does. An `off` pod denies at once
each such request it serves, and its sweep, within `BROKER_SWEEP_SECONDS`, denies each one a pod
with sign-in on queued that is still pending. A pod cannot tell a request queued in the overlap
from one an earlier run left, so a cutover that denies nothing queued during it has no overlap:
stop every pod of the old setting before the first pod of the new one starts.

- **Turning sign-in on.** Set the four `BROKER_SIGNIN_*` settings, unset `BROKER_SIGNIN`, and
  roll the pods. A session denied during the roll reads `BROKER_SIGNIN=off` in its denial and asks
  again once the last `off` pod has stopped.
- **Turning sign-in off.** Set `BROKER_SIGNIN=off`, unset every `BROKER_SIGNIN_*` setting and
  the model judge's, and roll the pods. Every request still pending when the first `off` pod's
  sweep reaches it, whether an earlier run or a pod still on sign-in queued it, is denied.

## Signing in to RDS by IAM token

On Amazon RDS or Aurora the broker can sign in to its database with an
[IAM auth token](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/UsingWithRDS.IAMDBAuth.html)
instead of a password, so no database password exists for it to hold or for RDS to rotate under
it. It does so when `BROKER_DATABASE_URL` names a user and no password and its host is an RDS
endpoint, one ending in `.rds.amazonaws.com`:

```sh
BROKER_DATABASE_URL='postgres://broker@<cluster endpoint>:5432/broker?sslmode=verify-full&sslrootcert=/etc/ssl/rds/global-bundle.pem'
AWS_REGION=<region>
```

Each new connection signs in with a token the broker mints for that user and host, signed with
the AWS SDK's default credentials in the region `AWS_REGION`, `AWS_DEFAULT_REGION` or the shared AWS
config names; with none, the broker refuses to start. A token is good for 15 minutes from its mint and
is checked only when a connection signs in, so a connection the broker holds longer keeps working,
and the next connection it opens brings a fresh token. The broker's AWS identity needs
`rds-db:connect` on the database user,
`arn:aws:rds-db:<region>:<account>:dbuser:<cluster resource id>/<user>`, and the database user
must be a member of `rds_iam`. The cluster needs IAM database authentication turned on. Give the
broker a database user of its own, and never grant `rds_iam` to the RDS master user, directly or
through a role it belongs to: once a user holds it, IAM authentication takes precedence over that
user's password
([AWS's limitations of IAM database authentication](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/UsingWithRDS.IAMDBAuth.html#UsingWithRDS.IAMDBAuth.Limitations)),
so the master could no longer sign in with its password.

A token is a password to the database until it expires, so the broker sends one only to a server
it has verified. The URL must name that one host, with `sslmode=verify-full` and an `sslrootcert`
file (the Envoy image ships the RDS CA bundle at `/etc/ssl/rds/global-bundle.pem`); otherwise the
broker refuses to start, naming the host: `sslmode=require` encrypts but verifies nothing, and
`sslrootcert=system` (in the URL or `PGSSLROOTCERT`) names the system trust store, which holds no RDS
CA, so every sign-in would fail. The bundle sits outside the system trust store, so nothing else in
the image trusts it. It is the
[global bundle](https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem) AWS publishes,
vendored as `packages/envoy/docker/rds-global-bundle.pem` with the date it was fetched and its
checksum.

A URL with a password, a passwordless URL whose password libpq supplies (`PGPASSWORD` or a
passfile), or a host that is not an RDS endpoint (a local Postgres that trusts its clients, say)
connects as given and mints nothing. The broker substitutes no password into the URL: it refuses
to start while the removed `BROKER_DATABASE_PASSWORD` is set or the URL names its
`${BROKER_DATABASE_PASSWORD}` placeholder.

## What it depends on

| Dependency | What the broker needs from it |
| --- | --- |
| Postgres | A database the broker owns (`BROKER_DATABASE_URL`). The broker applies its own migrations at startup; they only move forward, so never run an older broker against a database a newer one has migrated. Each process also holds one connection outside its pool, for as long as it runs, to hear what the other processes reread ([Running more than one pod](#running-more-than-one-pod)). On Amazon RDS it can sign in by IAM token instead of a password ([Signing in to RDS by IAM token](#signing-in-to-rds-by-iam-token)). |
| Rolling back | Once any secret carries `type`, never roll the broker back past the first release that reads it, the first whose `GET /v1/settings` answers `secret_type_tag: true`: that release is the rollback floor. Every broker from legion-envoy-v16.0.0 to the release before the floor reads only the old `kind` tag, and serves a secret that carries `type` alone as a stored secret, releasing its stored GitHub App key or refresh token to every grant as its value. The `mint-service-account: type-secret` tag `agent-secrets secret` writes beside every type does not stop such a broker: only the brokers from legion-envoy-v13.19.0 to the release before v16.0.0 read it, and refuse the secret as `mint-target-malformed`, while every broker from v16.0.0 to the release before the floor ignores it and one older than legion-envoy-v13.19.0 reads no such tag. A rolling deploy to the floor leaves the same hole while a pod of an earlier release still serves, so write no type until every pod runs the floor or later. To go back past the floor anyway, first disable every type secret's versions. |
| Rolling back past the project tags | Never roll back past the release that reads `dispatch-project` and `project-channels` while any secret carries either tag without its rollback guard ([Rolling back past the project tags](#rolling-back-past-the-project-tags)). |
| The secret store | The agent secrets: every secret whose name starts with `BROKER_SECRETS_PREFIX`, carrying `owner` and `tier` (AWS tags, or Secret Manager annotations), encrypted with the agent-secrets key, and holding a current value. A `type` tag makes the broker mint a release from that value. [Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret) describes the tags, the name each secret is asked for by, and how often the broker rereads them. `BROKER_SECRETS_STORE` names which store, and the two rows below say what each needs. |
| AWS Secrets Manager (`aws-secrets-manager`) | The broker calls AWS with the SDK's default credential chain (environment, shared config, or the workload's role), in a region (`AWS_REGION`, `AWS_DEFAULT_REGION` or the shared config). It needs `secretsmanager:ListSecrets` (which takes no resource, so on `*`), `secretsmanager:DescribeSecret` (to reread one secret) and `secretsmanager:GetSecretValue` on the namespace's secrets, `kms:Decrypt` on the agent-secrets key (`BROKER_SECRETS_KMS_KEY_ARN`), `kms:ListAliases` (on `*`, called only when a secret names its key by an alias), and, when it signs in to RDS by IAM token, `rds-db:connect` on its database user; nothing else. |
| Google Cloud Secret Manager (`gcp-secret-manager`) | The broker calls Secret Manager under [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials): on GKE, Workload Identity Federation for the pod's Kubernetes ServiceAccount, with no key file and no Google service account; the boot log's `secret store … credentials=application-default` line says so. Its principal needs `secretmanager.secrets.list` on the project (the listing takes the project as its parent) and `secretmanager.secrets.get`, `secretmanager.versions.get` and `secretmanager.versions.access` on the namespace's secrets: `roles/secretmanager.viewer` on the project and `roles/secretmanager.secretAccessor` with an IAM condition `resource.name.startsWith("projects/<number>/secrets/<id prefix>")` cover them. The condition narrows only the accessor binding, so the broker reads values from the namespace alone; the viewer role reaches every secret's metadata in the project (its annotations, its versions' state and its IAM policy), never a value. A secret is served while its `versions/latest` is `ENABLED`, encrypted with the key `BROKER_SECRETS_KMS_KEY_NAME` names (`replication.automatic.customerManagedEncryption`; a secret on Google's default encryption is refused `not-on-agent-secrets-key`); one with an `expire_time` is scheduled for deletion and left out, as a Secrets Manager secret in its recovery window is, and a grant made before it no longer reads its value, though Secret Manager still would until that time. Every Secret Manager call but reading a value counts against the project's read quota, [600 a minute, shared by everything in the project](https://docs.cloud.google.com/secret-manager/quotas) (reading a value has its own 90,000). The broker's worst case against it is the sum of: a reload every five minutes, one listing plus one call per secret (a 200-secret namespace, about 40 a minute); the public reread route, two each, bounded across all callers of **each process** at one reread every `N² × 1 second`, where `N` is the pods that process counted at its last beat and never fewer than two — one every 4 seconds at two pods, one every 9 at three, one every 16 at four — which holds the whole fleet at the same 120 metadata reads a minute whatever `N` is (see [Running more than one pod](#running-more-than-one-pod)); each session's rereads for names it was not served, two each, at most one per session every ten seconds; and value releases, a type secret's among them, one per distinct secret per 30 seconds (two for a secret whose latest version is disabled), since the broker trusts its last check of a secret, readable or not, for that long (so a delete made outside `agent-secrets secret` stops releases at most 30 seconds after Secret Manager's own metadata shows it, which Google documents as eventually consistent; `agent-secrets secret delete` rereads the name, and the broker then releases nothing more). A busy fleet of sessions requesting unknown names can still spend the quota, and other workloads in the same project draw on it too, so give the agent secrets a project of their own. On the metadata calls the broker retries an `UNAVAILABLE` answer and never a `RESOURCE_EXHAUSTED` one, which would only spend more of that quota; reading a value keeps the client library's own retry of both, against its separate quota. On Secret Manager the agent-secrets key gates no caller: Secret Manager's service agent makes the KMS calls, so writing a secret onto the key and reading it need no KMS permission, and being on the key is the namespace's convention, not a second permission check as it is on AWS. Secret Manager makes the KMS calls itself, so the broker needs no KMS permission; the Secret Manager service agent needs `roles/cloudkms.cryptoKeyEncrypterDecrypter` on the key. |
| Type secrets | A `github-app` secret needs outbound HTTPS access to `api.github.com`, and a `github-owners` tag naming the GitHub accounts it mints for, which the deployment sets when it creates the secret. A `github-installations` secret needs no outbound access and no setting: it needs the same `github-owners` tag and a minter outside the broker that stores new installation tokens in it more often than every 40 minutes (every ten minutes leaves three runs in a row to fail), since the broker hands a stored token over only while more than 20 minutes of it are left ([Requests and grants](/legion/broker/concepts/#requests-and-grants)). A `google-login` secret needs outbound HTTPS access to `oauth2.googleapis.com`. A `google-service-account` secret needs outbound HTTPS access to `iamcredentials.googleapis.com`, an entry in `BROKER_SERVICE_ACCOUNT_SECRETS` binding it to one account, and the broker's own Google identity holding `roles/iam.serviceAccountTokenCreator` on each bound account, on the reader account `BROKER_DIRECTORY_ACCOUNT` names where the deployment reads groups (Google Workspace groups, below), and on no other, since the broker mints the token by impersonating that account; without the role Google refuses the mint and the session is answered `502 MINT_FAILED`. An `anthropic-federation` secret needs outbound HTTPS access to `iamcredentials.googleapis.com` and `api.anthropic.com`, an entry in `BROKER_SERVICE_ACCOUNT_SECRETS` binding it to one account, the broker's own Google identity holding `iam.serviceAccounts.getOpenIdToken` on that account (`roles/iam.serviceAccountOpenIdTokenCreator`, or Token Creator, which holds it too), since the broker mints that account's identity token for the audience `https://api.anthropic.com`, and an Anthropic federation rule that admits that account's token for the organization and service account its value names, with a `token_lifetime_seconds` of 3600 or less ([Anthropic's reference](https://platform.claude.com/docs/en/manage-claude/wif-reference)). **The deployment's binding, not the secret's stored value, chooses which account the broker impersonates.** A secret bound to no account, and a stored value naming an account other than the bound one, release nothing and are logged `class=binding`, so a secret someone writes with another person's account in it mints no token, and a secret the deployment never bound mints none either. What the secret's own tags still decide is who may ask for it (`requesters`) and what the token can do (`scopes`, on a `google-service-account` secret), so the write access to a bound secret is an administrator's ([People who manage secrets](#people-who-manage-secrets)). The broker reads the stored value for each release, as it reads any secret, and returns only the minted token. It also reads each type secret's value once for each version and type a pod lists, to check it, and refuses a secret whose value its type's minter could not mint from ([the value read](#the-value-read)). It needs no IAM permission to mint a GitHub App or a Google login value, and `BROKER_SERVICE_ACCOUNT_SECRETS` is the one `BROKER_*` setting any of these types needs. A mint GitHub, Google or Anthropic refuses answers the session `502 MINT_FAILED`, naming their reason, and logs `broker: mint failed` (below); so does a `github-installations` token with 20 minutes or less left (`class=upstream`). A call to GitHub or Google that answered a 5xx or a rate limit, or got no answer, is attempted up to three times before that ([the mint retry](#the-mint-retry)); an Anthropic federation exchange is sent once. A GitHub profile the installation cannot serve, because its permissions cut to what the installation holds leave none or because GitHub refuses a repository it names, answers `422 PROFILE_MINT_REFUSED` instead and logs `broker: narrowing refused`; so does a `github-installations` profile whose stored token holds a permission past the profile's, holds none of its permissions but `metadata`, or reaches other repositories than the profile names. A target outside the profile answers `400 TARGET_REFUSED`, naming the profile's accounts and repositories, and a target naming a repository of a `github-installations` profile answers `400 TARGET_REPOSITORIES_REFUSED`. |
| Google Workspace groups (optional) | To serve a secret whose `owner` or `requesters` names a group, outbound HTTPS access to `admin.googleapis.com` (or the base URL `BROKER_DIRECTORY_URL` names) and to `iamcredentials.googleapis.com`, and the reader account `BROKER_DIRECTORY_ACCOUNT` names, with Workspace's Groups Reader admin role assigned to it, `roles/iam.serviceAccountTokenCreator` on it for the broker's own Google identity, and the IAM Service Account Credentials API turned on in its project. The broker reads each group a served secret names when a five-minute read of the namespace first names it and once an hour after that, and a group it has not read for 2 hours admits nobody. So a Directory or IAM outage, a missing grant or a missing role leaves every group's requests with nobody to decide them, and every grant that rests on a group without a release, within 2 hours; each read's `group members read` line names the cause ([Health and logs](#health-and-logs)). |
| A local stand-in (development only) | `BROKER_FAKE_SECRETS_FILE` names a JSON file the broker reads in place of the store, in Secrets Manager's shape whatever `BROKER_SECRETS_STORE` says, each secret's `kms_key_id` naming the agent-secrets key as the selected store configures it (`BROKER_SECRETS_KMS_KEY_ARN`, or `BROKER_SECRETS_KMS_KEY_NAME` with the GCP store); the [configuration reference](/legion/broker/reference/config/#variables-the-broker-reads) gives its format. |
| Dispatch | Dispatch's server calls the broker's account routes as its own ServiceAccount: it lists the requests and machine logins waiting on the signed-in person, each secret request with its page on the broker (`approve_url`), which its Inbox links to, and relays a person's machine-login decisions and revokes. Set Dispatch's `DISPATCH_AGENT_SECRETS_URL` to the broker's URL and `DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE` to the path of its projected token for `BROKER_K8S_OIDC_AUDIENCE`, and list that account in the broker's `BROKER_API_ACCOUNTS`. Without the URL, Dispatch hides its credential pages. Deploy a broker with the approval page before a Dispatch whose Inbox links it ([Approval sign-in](#approval-sign-in)). |
| The sign-in provider | A person's browser signs in to the broker at the provider (Google, under the Workspace's internal consent screen), and the broker reaches its token endpoint and keys over HTTPS at a sign-in, and its discovery document at the first sign-in each process serves (kept once a read succeeds) and at the client check it takes at startup and every hour after. A discovery that could not be read, or a code exchange the token endpoint answers `408`, `429` or `5xx`, answers with a body cut off before it arrives whole, or does not answer, answers the page `503` naming the issuer and logs `approval sign-in: reach the provider`; machines are served as before, and `GET /signin/status` reports `issuer_unreachable`: in `last_failed_sign_in_reason` for a failure at the sign-in's callback, and in `client_check` once a client check finds the provider down, which is the only field that shows a failure at the sign-in's start ([What the sign-in is doing](#what-the-sign-in-is-doing)). |
| Kubernetes (optional) | To enroll pods, and to let a service log in with its token (`BROKER_SERVICE_LOGINS`), `BROKER_K8S_OIDC_ISSUER` and `BROKER_K8S_OIDC_AUDIENCE` name the cluster's service-account token issuer and the audience the pods' and the services' projected tokens carry. The broker fetches the issuer's discovery document at startup and refuses to start if it cannot. |
| Google Cloud (optional) | To let machines trade their Google identity tokens, `BROKER_GOOGLE_OIDC_AUDIENCE` names the audience of those tokens, and `BROKER_VM_SERVICES`, `BROKER_VM_PROJECTS`, `BROKER_VM_INSTANCES`, `BROKER_VM_EXCHANGE_ACCOUNTS` and `BROKER_VM_EXCHANGE_JOB_ACCOUNTS` describe each fleet. The broker fetches Google's discovery document at startup and refuses to start if it cannot. |
| Envoy (optional) | With `BROKER_ENVOY_URL`, the broker tells a waiting agent session, best effort, when its pending request expires. It calls the listener with its own projected ServiceAccount token, which `BROKER_ENVOY_ACCOUNT_TOKEN_FILE` names and which it reads for each notice, so the listener must admit that account. The shared bearer `BROKER_ENVOY_TOKEN` is removed, and a broker started with it refuses to start. |
| The Envoy hub's NATS auth service (optional) | With `BROKER_NATS_URLS`, the broker answers the authorization of each connection the Envoy hub sends it: a session presenting its broker access token and a proof of its key gets its own delivery channel, and a service presenting a platform token for `BROKER_NATS_K8S_AUDIENCE` or `BROKER_NATS_GOOGLE_AUDIENCE`, from an account `BROKER_NATS_SERVICES` lists, gets its kind's grant; an `edge-listener` entry's connection gets the edge listener's grant for the projects its entry lists. It connects to the hub as the user `BROKER_NATS_SEED_FILE` holds, signs with the account seed in `BROKER_NATS_ISSUER_SEED_FILE` and opens each request with the curve seed in `BROKER_NATS_XKEY_SEED_FILE`; each file is refused at startup unless it holds a seed of its kind. It places every client it admits, the operator and the hold's probe included, in the hub account `BROKER_NATS_ACCOUNT` names, `$G`, the global account, when unset; a hub that keeps JetStream in an account of its own names the account its clients sit in, never the account JetStream sits in (`JETSTREAM`, which refuses the start), and must declare that account before the broker names it, since the hub refuses every client placed in an account it does not have. A value that is neither `$G` nor one subject token of at most 64 letters, digits, `_` and `-`, or one that holds an nkey seed in any spelling (in either case, behind a prefix, or split by `-` or `_`), refuses the start, naming the setting and never the value. Every replica answers in one queue group, so a stopped replica delays no connection. With none of these set the broker runs no auth service. `BROKER_NATS_SERVICES` may list one `operator` entry, the deployment's one operator identity, whose grant follows the NATS writer hold's phase, and one `probe` entry, the hold's probe. The hold's row lives in this broker's Postgres, signed by the operator's Ed25519 key, whose public halves are `BROKER_NATS_HOLD_SIGNING_KEYS`, and only the operator writes it, through `/v1/nats-hold`. With `BROKER_NATS_HOLD_READ=true` (default `false`) the auth service reads the row at each decision on a pool of its own (`BROKER_NATS_HOLD_POOL_SIZE`, default 2) and, while no row that verifies can be read, refuses every client but the operator; the broker then needs the `operator` entry and the signing keys to start, and starts with no row, so the operator writes the first row (`POST /v1/nats-hold/restore`) before the setting is turned on. |
| The model judge (optional) | Outbound HTTPS access to the model endpoint `BROKER_JUDGE_MODEL_URL` names, which the broker reaches with its machine's own Google identity token for `BROKER_JUDGE_MODEL_AUDIENCE`, and to the Dispatch `BROKER_JUDGE_DISPATCH_URL` names, which it reads with the projected ServiceAccount token in the file `BROKER_JUDGE_DISPATCH_TOKEN_FILE` names, read at call time and never at boot, so a rotation needs no restart. It reads one issue, that issue's spec document and the ask the reason cites there for the judge, and one issue's assignee for a Legion worker pod's request for a shared secret, and writes nothing. With those settings unset the broker makes neither call and runs no judge ([Concepts](/legion/broker/concepts/#the-model-judge)). With `BROKER_JUDGE_MODEL_ACCOUNT` set, the token it presents to the model is that service account's, which it mints at IAM Service Account Credentials' `generateIdToken`, so it also needs outbound HTTPS to `iamcredentials.googleapis.com` on 443, and a read of the metadata server's token path (`/computeMetadata/v1/instance/service-accounts/default/token`, plain HTTP on the node's own link) for the access token the mint spends; the broker's principal holds `roles/iam.serviceAccountOpenIdTokenCreator` on the account; and the account's project has the IAM Service Account Credentials API turned on. A mint that fails hands the request to its person, and the broker never falls back to the metadata server's identity token. |
| Dispatch's project access (optional) | `BROKER_PROJECT_ACCESS_URL` names the Dispatch the broker asks about projects and people, through Dispatch's `GET /api/v1/access/projects/{key}` and `GET /api/v1/access/person` routes, and `BROKER_PROJECT_ACCESS_TOKEN_FILE` names the broker's own projected ServiceAccount token for Dispatch's audience, read at call time and never at boot. No Dispatch release serves those routes yet: leave both unset until the broker can reach one that does, since against one that does not, every trade naming a project, and every trade of a machine login's token, is refused `temporarily_unavailable`, every enrollment under a person's machine login or pod login, and every secret request, release and new airlock call of a person's session, is refused `503 MEMBER_KIND_UNCHECKABLE`, and every approval of one decides nothing (the approval page says it could not decide the request). Dispatch must list that account as one that may ask: until it does, Dispatch answers 403, every trade that asks it is refused `server_error`, saying Dispatch refused the broker's account, an enrollment under a person's machine login or pod login is refused `503 MEMBER_KIND_UNCHECKABLE` saying the same, a person's session enrollment under a sign-in is refused `503 PROJECTS_UNCHECKED` whether it names projects or not (where an outage enrolls one naming none), and every other question below is answered as Dispatch being unreachable; the trade and the enrollments are refused the same way while the broker cannot read the token file. While Dispatch answers a member kind the broker does not know, a trade naming no project is refused, so is a person's session enrollment under a sign-in naming none, and every enrollment under that person's machine login or pod login and every secret request and release of their sessions is refused `503 MEMBER_KIND_UNCHECKABLE`; a sign-in's enrollment naming projects asks only whether its person may see each. The two are set together or not at all. The same two settings build the hold of refused people: the broker asks Dispatch's person route whether Dispatch holds the person a credential names, and refuses a held person `403 MEMBERSHIP_HELD` on its session and machine-credential routes, its airlock and its approval page, and `membership_held` at its token endpoint, while it still serves a held person's refresh grant, lease renewal and the calls that only end access. It keeps each answer, so while Dispatch cannot be asked a person it last answered held stays held and everyone else is served, with one WARN line a minute per person and `broker_hold_unchecked_total{last_answer}`; a broker that cannot tell is `503 HOLD_UNCHECKABLE`, or `server_error` at its token endpoint. A Dispatch that answers no `held` field is handled as an older Dispatch: the broker serves each person on their last answer, logs one WARN line a minute for the deployment and counts none; a kept held answer stays held, a kept not-held answer is served, and a person of whom no answer is kept is served unless the deadline rule below requires one. `BROKER_HOLD_REQUIRES_DEADLINE` is `true` on the broker of a deployment whose Dispatch runs behind Identity-Aware Proxy (`DISPATCH_IDENTITY=iap`), and unset or `false` everywhere else. With it `true`, the hold serves Dispatch's answer that a person is not held only until the deadline Dispatch put on it (`not_held_until`), refuses an answer naming no deadline as one it cannot tell, refuses a person of whom it keeps no answer while Dispatch cannot be asked, and says in each refusal of a held person that the Workspace Directory, not a sign-in, ends the hold. The broker refuses to start with it `true` and `BROKER_PROJECT_ACCESS_URL` unset, since that broker holds nobody, and with a value strconv.ParseBool does not accept (`BROKER_HOLD_REQUIRES_DEADLINE must be true or false`); it accepts `1` and `t` as true and `0` and `f` as false. The broker asks: at the token exchange, whether a person may see a project a token names and what kind of member a person is; at every enrollment under a person's machine login or pod login, every secret request, every approval and every release of a person's session, and every airlock connection of its grants, what kind of member that person is; at a person's session enrollment, whether its person may see each project its `--projects` list names, and, for a session under a sign-in naming none, what kind of member its person is (a service's pod and a machine's own session ask nothing; a person's pod whose account `BROKER_POD_OPERATORS` no longer maps to that person, or maps to another, is still asked about as the person it was made for); at each release of a secret carrying a `dispatch-project` tag, and at each airlock connection of a grant holding one, whether that project is private and whether the credential's person may still see it; for a `project-channels` secret asked for by a credential that names a private project, which Slack channels that project's owner named; for the approval page, the pending lists and each decision, whether the signed-in person may see the projects a request touches (its secrets' tags, the projects of the credential that asked and of the one the request hangs off now, and the project of each issue key its reason names: at most 16 projects for a new request, which `400 REQUEST_INVALID` enforces when it is made, and every project, at most 80 in a reason of 400 characters, for a request made before that rule or by a broker with neither setting; the broker keeps at most 4,096 answers, and keeps no new one past that); and, for the model judge, what kind of member the session's person is and whether a request is a private project's, which the judge then leaves to a person: before the model, inside the 5 seconds its policy, project and issue reads share, and again, afresh, when its approval is written. A pending list or a grant list gives its questions at most 3 seconds from the moment it reads its rows, inside the 5 seconds Dispatch waits for it, and leaves out, with a log line naming it, a request or grant whose answers have not come by then; it reads up to 64 of the rows waiting on its reader at once and asks Dispatch at most 16 questions at once, a row waiting on another row's question taking none of them, so one project Dispatch does not answer costs the list only the rows that name it, unless they are the first 64 rows waiting on its reader that it reads: a row waiting on such a project keeps its place until the 3 seconds have run out, so the rows after those 64 start only then, and each is listed only where it asks Dispatch nothing or the broker already holds every answer it needs. While the pending list reads the requests that name someone else as responsible, for at most 2 seconds, those reads take one of the 64 places between them, so for that long 63 rows waiting on its reader are read at once, and a row behind 63 such rows starts when those reads end, inside the 3 seconds. Answers are kept 30 seconds, apart from that last question, which is always asked of Dispatch, and every question is asked before a database transaction begins. No decision rests on an answer older than 30 seconds: the questions are asked in turn, a request's projects 16 at a time and each once for a whole list, so when the later ones take long enough for an earlier answer to pass 30 seconds before the decision, the broker asks every question again, and refuses as though Dispatch could not be asked if the answers pass 30 seconds a second time. While Dispatch does not answer, the reads of one person's member kind in flight together share one call, and a read that failed is the answer to that person's next reads for 2 seconds after it came back. What that leaves is one call at a time for each person: a caller in the first 2 seconds of a call waits for it up to its own bound; a later one is answered at once from what the broker kept, as when Dispatch does not answer, so a Dispatch that answers in 2 to 10 seconds is read as down for that person's other reads; and the first caller after the failure's 2 seconds starts the next call. While Dispatch cannot be asked, a token naming no project is minted as before, but for a machine login's: a machine login's trade asks what kind of member the login's approver is, since a restricted member's machine login is held to one project, and it is refused `temporarily_unavailable`, a list naming no project included, unless the broker keeps Dispatch's last answer that the approver is a full member. An operator entry's token naming no project whose entry names a person asks that person's member kind afresh, never answered by a failure the broker kept, and each such mint logs a warning naming the account, the session and the person, so such mints stop as soon as Dispatch answers again; where the broker keeps Dispatch's last answer that the person is a full member, it logs instead, at most once a minute per person, that it serves them as one. Every trade naming a project, an operator entry's included, is refused `temporarily_unavailable`, and each trade naming a person is held to the hold above as well. The same setting holds a restricted member's agents to no secret at all: an enrollment under a machine login or a pod login whose person Dispatch calls a restricted member is refused `403 RESTRICTED_MEMBER_ENROLLS_NOTHING`, and each secret request, each approval, each release and each call the airlock proxy carries asks again of the session's person, so a session such a person already had, enrolled while the setting was unset, is denied every secret `restricted-member`, an approval of a request it left pending is recorded as the policy's denial `restricted-member`, and its grants end at their next use, the airlock proxy refusing its next call `407 GRANT_NOT_LIVE` though it already holds the grant's values. While Dispatch cannot be asked, a person whose kept full member kind is no older than an hour (`projectaccess.KeptAnswerTTL`) is served at these checks, with one WARN line a minute and `broker_member_kind_unchecked_total{kept="full"}`; a restricted, missing, unreadable or stale kept kind refuses. A check with no serving full kind refuses: while the broker cannot learn from Dispatch whether a person is a restricted member, every enrollment under that person's machine login or pod login, every secret request of their sessions, every release of their grants and every new call the airlock proxy would carry for them is refused `503 MEMBER_KIND_UNCHECKABLE` naming the person and why, and every approval of one is refused (the approval page says it could not decide the request), with nothing written and nothing released; the caller asks again. What is logged depends on the path. The API port (enrollments, secret requests, releases) logs an error naming the person and the failure, and writes no line when the caller hung up first, which it counts as an answer the caller cancelled. The airlock proxy logs an error, or an INFO line when the caller hung up. The approval page writes no error, only its usual decision line, whose outcome says the request was unavailable. The model judge logs a warning that the request waits for its person, and hands it to that person. A person's sign-in on a machine is not held to the member-kind check at enrollment, since the broker checked its domain itself; its session is held to the list rule alone. With neither set the broker asks Dispatch nothing: it refuses every secret carrying a `dispatch-project` tag as `project-unchecked` and serves every other secret as before; a token naming no project and an operator entry's token are minted as before; a person's trade naming a project is refused, and so is a session enrollment naming projects (`503 PROJECTS_UNCHECKED`); every other enrollment and request is decided as before, a restricted member's included; and a new request or machine login whose reason names the issue keys of more than 16 projects is taken as before, since only a broker that asks Dispatch about a reason's projects refuses one (`400 REQUEST_INVALID`). The boot log's `project access check state=on` or `state=off` line says which. The same URL and token serve `GET /v1/project-repositories`, which asks Dispatch's `GET /api/v1/access/projects/{key}/repositories` on every call an edge listener makes and keeps no answer; with both unset, and whenever Dispatch's whole answer does not arrive within 2 seconds, that route answers `503 REPOSITORIES_UNKNOWN`. Each question above, and this route's, is its own call to that Dispatch, and every one of those calls goes through the same code (`internal/cipher/dispatchread`) with the same account token, so a token Dispatch refuses, or a token file the broker cannot read, is the broker's own credential failing for each of them alike: each question above is then answered as its sentence above says, and this route answers `503 REPOSITORIES_UNKNOWN`, with an error log line saying the broker's own account token was refused or could not be read. |

## Rolling back past the project tags

A broker from before the `dispatch-project` and `project-channels` tags reads neither, so it serves
a secret carrying one by owner and tier alone: a private project's secret goes to the sessions of
every project, and the Slack bot token goes out with no channel limit. **No rollback past this
release while any secret carries either tag without its guard, and no rollback past
legion-envoy-v29.23.0 while any secret carries either tag at all.**

The guard is the entry `rollback-guard:project-tags` in the secret's `requesters` tag.
`agent-secrets secret create` and `retag` write it beside either project tag, after the people and
services the `requesters` tag lists, keep it while either tag remains (`--requesters none` then
removes the list and keeps the guard), and take it out with the last of the two. A broker from
legion-envoy-v29.23.0, the first release that reads `requesters`, up to the release before the
project tags reads every `requesters` entry as a person's email or a registered service, and no
service name holds a colon, so it refuses a guarded secret as `requesters-malformed`. A broker
older than legion-envoy-v29.23.0 reads no `requesters` tag, so the guard does not stop it: it serves
a guarded secret by owner and tier as well. This broker takes the entry out before it reads the
list, so the guard changes nothing here, and `secret show` and `secret list --json` print the list
without it, with a `rollback_guard` field saying whether the secret carries it.

The guard covers the tags the CLI writes. A `dispatch-project` or `project-channels` tag written
by a console or an infra-applier without the guard is served by every earlier broker. Before a
rollback, read the store with `agent-secrets` built from this release:

```sh
agent-secrets secret list --json > secrets.json
jq 'length' secrets.json
jq '[.[] | select((.project != null or .project_channels != null) and (.rollback_guard | not))] | length' secrets.json
```

The first count must be more than 0, or the list read nothing. The second must be 0. For each secret
the second count finds, run `agent-secrets secret retag NAME --project <KEY>` (or
`--project-channels slack`) to write the guard, or remove the tag. After the rollback, every guarded
secret is refused, so no session gets one until the broker is rolled forward again.

## Rolling back past the kinds

A broker from before the kinds reads neither a `kinds` list nor the `proxy` and `proxy-narrowed`
tags. It refuses to start on a profiles file in the kinds form, so a rollback also puts the file
back to its proxy-block form. And it serves a secret that is behind the airlock by its `proxy` tag
alone, with no proxy block in that older file, as its value: an agent-tier secret then goes out
with no approval. **No rollback past this release while any secret carries `proxy` or
`proxy-narrowed` without its guard, and no rollback past legion-envoy-v29.23.0 while any secret
carries either at all.**

The guard is the entry `rollback-guard:kinds` in the secret's `requesters` tag
(`policy.KindsRollbackGuard`). `agent-secrets secret create` and `retag` write it beside a
`proxy` or a `proxy-narrowed` tag, after the people and services the `requesters` tag lists and
after a project tags' guard, keep it while either tag remains, and take it out with the last of
the two. A broker from legion-envoy-v29.23.0, the first release that reads `requesters`, up to
the release before the kinds reads every `requesters` entry as a person's email, a registered
service or the project tags' guard, and no service name holds a colon, so it refuses a secret that
carries this guard as `requesters-malformed`. A broker older than legion-envoy-v29.23.0 reads no
`requesters` tag, so the guard does not stop it. This broker takes the entry out before it reads
the list, so the guard changes nothing here, and `secret show` and `secret list --json` print the
list without it, with a `kinds_rollback_guard` field saying whether the secret carries it.

The guard covers the tags the CLI writes. A `proxy` or `proxy-narrowed` tag written by a console
or an infra-applier without the guard is served as its value by every earlier broker. Before a
rollback, read the stored `requesters` tag as the older broker will. On Google Secret Manager,
with the project the broker reads:

```sh
gcloud secrets list --project <project> --filter='name~agent-secrets-' --format='json(name,annotations)' > secrets.json
jq 'length' secrets.json
jq '[.[] | select((.annotations.proxy != null or .annotations["proxy-narrowed"] != null) and ((.annotations.requesters // "") | split(" ") | index("rollback-guard:kinds") | not)) | .name]' secrets.json
```

On AWS Secrets Manager, list the same tags with
`aws secretsmanager list-secrets --filters Key=name,Values=<prefix>` and select each secret whose
`Tags` hold `proxy` or `proxy-narrowed` and whose `requesters` tag holds no `rollback-guard:kinds`.
On either store, `agent-secrets` built from this release gives the same answer from its own read
of the tags:

```sh
agent-secrets secret list --json > secrets.json
jq 'length' secrets.json
jq '[.[] | select((.proxy != "" or .proxy_narrowed != "") and (.kinds_rollback_guard | not)) | .name]' secrets.json
```

In each read, the first count must be more than 0, or the list read nothing. The second list must
be empty. For each secret it names, run `agent-secrets secret retag NAME --proxy <kind>` to write
the guard, or remove the tag. After the rollback, every guarded secret is refused, so no session
gets one until the broker is rolled forward again.

## Network and trust

- Agents, their helpers and launchers, and Dispatch's server reach the broker at
  `BROKER_PUBLIC_URL`. Every signed proof names that URL plus the request's path, so clients'
  `AGENT_SECRETS_URL` must be exactly it, and a proxy in front of the broker must not change the
  path. The broker checks each proof against `BROKER_PUBLIC_URL`, never against the `Host`,
  `X-Forwarded-Proto` or `X-Forwarded-Host` a request arrives with: a proxy may rewrite `Host` or
  terminate TLS without setting `X-Forwarded-Proto`, and a client that sends those headers itself
  changes nothing. That holds for the machine login's token endpoint (`POST /v1/oauth/token`)
  too, whose DPoP proofs name `BROKER_PUBLIC_URL/v1/oauth/token`; at the broker's edge they name
  `BROKER_EDGE_URL/v1/oauth/token` instead ([The edge](#the-edge-device-login-from-outside-the-private-network)).
- The model judge adds two outbound calls. The broker reads one Dispatch issue, that issue's spec
  document and the ask the reason cites at `BROKER_JUDGE_DISPATCH_URL`, with its own projected
  ServiceAccount token, and writes nothing there: no ask, no comment, no status. At the same
  address and with the same token it reads the issue a Legion worker pod's claim names, for its
  assignee alone, when that pod asks for a shared human-tier secret. It calls the model at
  `BROKER_JUDGE_MODEL_URL`, presenting its machine's own Google identity token for
  `BROKER_JUDGE_MODEL_AUDIENCE`. Each call carries the broker's own identity and no session's,
  and neither carries a secret value: what reaches the model is the secret's name and rules, the
  request's reason and the issue, as data to judge. With `BROKER_JUDGE_MODEL_ACCOUNT` set, the
  token it presents to the model is that service account's, and minting it adds two calls: the
  metadata server's token path (`/computeMetadata/v1/instance/service-accounts/default/token`)
  for the access token of the broker's own principal, and `generateIdToken` at
  `iamcredentials.googleapis.com` on 443, which spends that token. That principal holds
  `roles/iam.serviceAccountOpenIdTokenCreator` on the account, and the account's project has the
  IAM Service Account Credentials API turned on. A mint that fails hands the request to its
  person: the broker never falls back to the metadata server's identity token.
- A person approves a secret request in a browser at `BROKER_PUBLIC_URL`, which must therefore be
  an `https` address people's browsers reach: the page's two cookies, `__Host-broker-session` and
  `__Host-broker-signin`, are `Secure` and host-only. A decision is accepted only from a form on
  the broker's own origin (`Origin`, or `Sec-Fetch-Site: same-origin`), bound to that browser's
  sign-in and to the requests its page printed, so a page on another origin, Dispatch's included,
  cannot make one even when both are under one parent domain. The page runs no script and cannot be
  framed.
- A caller listed as `dispatch` holds an approval credential: its ServiceAccount token can approve
  a machine login and revoke grants and machine logins in any person's name, the broker recording
  whichever approver that caller names, so whoever holds that account's token can decide those as
  anyone. It cannot decide a secret request. List Dispatch's own account under that name and
  nothing else, never one an agent's pod runs as. An account listed as `listener` reads tokens
  back and decides nothing, which is the whole of what that name carries.
- A Dispatch access token the token exchange mints never holds Dispatch's OAuth scope `approve`,
  whatever its project scope: Dispatch gives a token that acts as its person the scope `person`
  alone, and every other token none. So in Dispatch no token the exchange mints, a person's
  sign-in on a box and a machine login included, can call a route whose `x-oauth-scopes` name
  `approve` in Dispatch's OpenAPI document, such as a machine login's decision, a revoke of a
  grant, a machine login or a sign-in, or a personal token's mint, or answer an approval request:
  Dispatch answers `403 OAUTH_SCOPE_REQUIRED`, or `403 READ_ONLY_TOKEN` to a read-only token. The
  person does those in a browser signed in to Dispatch, or with a personal token they gave
  `approve`. A copy of a machine's file (`AGENT_SECRETS_MACHINE_FILE`) still reaches this broker's
  own operator routes, which take a launcher proof signed with that file's key: it can revoke the
  person's grants (`POST /v1/operator/grants/{id}/revoke`) and end their other machine logins
  (`POST /v1/operator/machines/{id}/revoke`). Those take access away and grant none.
- Behind a reverse proxy, set `BROKER_TRUSTED_PROXY_HEADER` so the machine-login, token-endpoint
  and secret-reread rate limiters see each caller's address rather than the proxy's. The broker
  reads the last entry of the header's last line: a proxy that folds the lines into one, appends
  to the last line or adds a line of its own leaves its entry there, after any line a caller sent,
  so a line a caller adds picks it no bucket. A proxy that appends to the first line and passes a
  later line the caller sent would leave the caller's text last; the setting assumes the proxy is
  not of that kind, and one that sends a single line, folding or overwriting the header, never
  is. The token
  endpoint (`POST /v1/oauth/token`) has its own per-address bucket in each process, a burst of 60
  refilled at two a second, far above a machine's poll every five seconds and refresh about once a
  day; a refused request is `429` `invalid_request` (`RATE_LIMITED: …`) with `Retry-After`.
- The settings route takes no credential, and the reread route needs none. `GET /v1/settings`
  stays open because two of its callers hold no credential of the broker's when they read it: a
  machine's `agent-secrets` reads `google_exchange_audience` there to mint the identity token it
  trades for its first credential, and the secret forms read the store, the prefix and the key
  under a cloud credential alone. It answers the store, the
  namespace prefix and, per store, the agent-secrets key's ARN with its AWS account and region, or
  the Cloud KMS key's name with the project number; and, on either store,
  `google_exchange_audience` (`BROKER_GOOGLE_OIDC_AUDIENCE`, absent unless `BROKER_VM_SERVICES`
  lists a machine), from which `agent-secrets` finds the machine's own identity with no setting;
  and `secret_type_tag: true`, which says the broker reads a secret's type from the `type` tag,
  without which `agent-secrets` writes no typed secret
  ([People who manage secrets](#people-who-manage-secrets)).
  It names no Kubernetes exchange audience: a pod's trade mints no launcher credential, so no
  machine could register a session under it. When a key's `judge` tag gives the model judge
  seconds of it it also answers `judge_keys`: each such name with its judge seconds and its
  `guidance` tag — the key owner's own words, never a value — and, for a profile made from a
  stored secret of another name, that secret as `source`. It is the live policy's answer, the
  same on a broker that runs no judge, and `agent-secrets secret show` reads it to say who decides a
  request for a secret and what the judge looks for: for a source the broker serves only through
  its profiles, it names the judged profiles made from it. An administrator's secret is listed there when
  its `judge` tag gives it seconds, and its person decides it all the same; `secret show` reads its tag and
  says so. The route answers any caller, so it tells anyone who can reach the broker which secrets
  the judge may take and on what guidance. Guidance is no secret: write nothing in it that a
  caller of the broker may not read.
  `POST /v1/secrets/{name}/reread` makes the broker read one secret from the store, on this pod
  and then on every other. Whether it says what the broker serves depends on the caller, never on
  the name:
  - A call with no `Proof` header is answered `202`
    `{"accepted":true,"reason":"the reread was taken; this broker says what it serves only to a signed session, so upgrade agent-secrets to the broker's release (the write stands)"}`
    at once, the same for a name the store holds as for one it does not, and the reread runs after
    that answer: 45 seconds at most, and 5 more for its notice to the other pods. The notice goes
    out once the metadata merge has run, including when the value read after it is cut; a timeout
    before that merge sends none. So neither the answer nor how long it takes tells a caller which
    names exist, though the store takes one call to say it holds no secret by a name and more to
    describe one it holds. The `reason` is the same for every name; it is there for an
    `agent-secrets` of an earlier release, which reads this answer as a refusal and prints its
    reason. A pod that stops waits for such rereads after its shutdown, out of the shutdown's own
    budget, before it ends its reread listener and closes its database pools, so a reread it
    answered `202` for still reaches the other pods. The reread run after a `202` writes these
    lines:
    - `broker: reread secret failed`, at ERROR, when the store or the writer lock fails it: the
      line an alert on a failing store counts;
    - `broker: reread panicked` and `broker: peer notice panicked`, at ERROR with the panic and
      its stack, when the reread or its notice to the other pods panics, which the broker
      recovers;
    - `broker: reread after its answer was not run because the pod is stopping`, at WARN, for a
      reread answered after the pod began to wait for them, which it neither counts nor runs;
    - `broker: a reread run after its answer was still running when the shutdown's budget ran out`,
      at WARN, from the stop sequence.
  - A call an enrolled session signs is answered once the reread is done: whether the broker now
    serves the secret, as which type (`type`: `github-app`, `github-installations`, `google-login`,
    `azure-login`, `google-service-account` or `anthropic-federation`, absent for a stored secret) and, if not, why (`absent`, or a
    refusal such as `owner-tag-missing` or `not-on-agent-secrets-key`). That is more than a
    request for the secret tells a session, whose `UNKNOWN_SECRET` never separates a secret that
    does not exist from one the broker refuses; it is what anyone allowed to list the namespace
    reads from the secrets' own names and tags (or annotations). A proof the broker does not
    accept is `401` `PROOF_INVALID`, whatever the name.
  A name no secret can carry is `400` `SECRET_NAME_INVALID` to every caller, a check of its shape
  alone. Neither route releases a value.
  Rereads are limited twice, by limits each pod derives rather than by settings
  (`RereadOverallFor` and `RereadLimitFor` in
  `packages/envoy/internal/cipher/api/limits.go`): the broker as a whole takes one reread every
  `N² × 250 ms` on the default store, burst 10, however many addresses the rereads come from —
  each one takes the policy's writer lock while it reads the secret, so that lock, not the
  caller, is what this limit bounds — and each source address gets a burst of 30, refilled one
  every twice that. `N` is how many broker pods the pod serving the call counted at its last
  beat, never fewer than two, so a two-pod fleet allows one reread a second and an address one
  every 2 seconds, a three-pod fleet one every 2.25 seconds and an address one every 4.5, and a
  four-pod fleet one every 4 seconds and an address one every 8. On Google Secret Manager,
  where a reread spends two reads of the project's shared metadata quota, the same arithmetic
  runs over a budget four times smaller: one reread every `N² × 1 second` broker-wide, burst
  10, and an address's own one every twice that — 4 and 8 seconds at two pods, 16 and 32 at
  four. A reread counts against either limit
  only when both let it run, so one address flooding past its own limit leaves the broker-wide
  one to everyone else. Past either, the broker answers `429 RATE_LIMITED` with a `Retry-After`
  header naming the limit that refused, at the rate that limit runs at now: at two pods, 1
  second for the broker-wide limit, 4 seconds for Secret Manager's, and for an address's own
  twice whichever of those this process mounted — 2 seconds on the default store, 8 under
  Secret Manager.
  Each of these buckets is one process's, and the broker-wide pair is derived from what the
  whole fleet spends of the secret store, which is not the same as what it spends in HTTP
  rereads: one reread the broker accepts costs the store **one read per pod**, because the pod
  that served it rereads the secret and then tells every other pod to reread it too. So the
  fleet reads the store `replicas × this rate × replicas` times a second, and each bucket is
  the fleet's whole budget divided by the replica count **squared**. At two pods: `2 pods × 1
  reread/s × 2 store reads = 4 store reads a second`, which is what one pod alone was allowed,
  and on Secret Manager `2 pods × 0.25 reread/s × 2 store reads × 2 metadata reads = 2 a
  second`, the same 120 metadata reads a minute as before. Read those as the arithmetic rather
  than as the number two: a third replica divides the fleet's budget by nine, not by four, and
  the pod does that division itself.
  Their **burst** is not divided at all: it stays at the budget's ten. A burst is a depth and
  what it has to admit is one person's write burst, a hard floor that the replica count does not
  change, while what it guards — each pod's own transient, its own burst plus every peer's — is a
  soft cost, latency rather than quota, spent once and re-bounded immediately by the rate.
  Dividing it by the replica count put the depth at 3 for a three-pod fleet, under the floor of
  four that `agent-secrets`' own end-to-end test measures, so ten now carries six tokens of
  headroom at any fleet size. The **per-address** bucket is
  derived from the bucket this process mounts rather than chosen: it refills at half that
  bucket's rate, so it always takes at least two addresses to hold the fleet's budget at zero,
  on either store and without tightening a deployment for a bucket it does not mount. That
  ordering matters because the route takes no credential and a refusal costs a caller nothing,
  so a caller that merely keeps succeeding at the shared refill rate would otherwise starve the
  fan-out for everyone. Each pod re-derives all three whenever the number of pods it counts
  changes, and logs the rate it moved to (`broker reread rate`,
  [Health and logs](#health-and-logs)).
  A reread one pod's notification drives on another spends nothing from either bucket, since the
  pod that served the original call already paid for it — which is what makes the figures above
  the whole of the fleet's store traffic rather than a floor under it
  ([Running more than one pod](#running-more-than-one-pod)). Every other limiter is per process
  and unchanged, so a fleet of N pods allows N times each: this per-address reread bucket, the
  two machine-login buckets, the token endpoint's, and each enrollment's budget of rereads for
  names it was not served.
  `agent-secrets secret` sends one reread after each write it makes, from the person's own
  address, so writing more than ten secrets at once meets the broker-wide burst.
  When a limit refuses it, the write stands in the secret store and the CLI exits 1 saying
  so ([manage a secret](/legion/broker/guides/manage-a-secret/#what-the-broker-answers-after-a-write));
  only the narrowing of who may read that secret waits for the next token.
- Agents never see the broker's database or the secret store; whoever can write the database can
  forge a record, so its access control is part of the broker's. Whoever can tag (or annotate) a
  secret under the namespace decides who gets it, so that write access is part of the broker's too.

## The edge: device login from outside the private network

A person outside the deployment's private network, with no way onto it, logs a machine in through
the broker's edge: a fourth port of the same process, which a public load balancer reaches and
nothing else does. It serves three routes; every other path answers `404`, and any method but
`POST` on the two login paths answers `405`:

- `POST /v1/oauth/device_authorization`, the start of a machine login;
- `POST /v1/oauth/token`, the machine's poll, its refreshes and the trade of its access token;
- `GET /healthz`, the load balancer's health check, which answers as the API's does and fails
  from the signal that stops the broker, through the drain.

Each is served by the handler the API's port serves it with. The person decides the login where
every machine login is decided, on the routes Dispatch relays to on `BROKER_PUBLIC_URL`, and the
launcher credential the approval mints works as any other.

| Setting | What it is |
| --- | --- |
| `BROKER_EDGE_LISTEN_ADDR` | The edge's `host:port`, such as `0.0.0.0:13383`, with its port written as a number. That number may not be the port of `BROKER_LISTEN_ADDR`, `BROKER_METRICS_LISTEN_ADDR` or `BROKER_AIRLOCK_LISTEN_ADDR`, each read as the bind reads it, so `013382` is `13382`: the boot is refused naming both settings. Unset, the broker has no edge and binds nothing for one. |
| `BROKER_EDGE_URL` | The broker's address as the edge's callers reach it, an absolute URL with no path, whose origin is not `BROKER_PUBLIC_URL`'s. The two are compared as a DPoP proof's `htu` is, the scheme and the host in any case and the scheme's default port the same as none, so `https://secrets.example:443` is refused beside `https://secrets.example`. A login made at the edge signs its request object for this audience, and every DPoP proof sent there names this URL and the path, so a proof or request object made for one port is refused at the other. |
| `BROKER_EDGE_MACHINE_LOGIN_URL` | The page the person types the code into, which a device authorization at the edge answers as its `verification_uri` in place of `BROKER_MACHINE_LOGIN_URL`: a page the person can reach from outside. |
| `BROKER_SIGNIN_WORKSPACE_DOMAINS` | Optional: the other domains the Workspace owns beside `BROKER_SIGNIN_DOMAIN`, separated by commas. |

The first three are set together or not at all. The edge also needs `BROKER_MACHINE_LOGIN_URL`,
since the device grant is what it serves, and `BROKER_SIGNIN_DOMAIN`, so `BROKER_SIGNIN=off`
refuses it. Unless `BROKER_FAKE_SECRETS_FILE` makes this a local run, a `BROKER_EDGE_URL` or
`BROKER_EDGE_MACHINE_LOGIN_URL` that is not `https` refuses the start, naming the setting: both
are on the path of a person on the internet, and over plain http anyone on that path could rewrite
the device authorization or serve a page of their own where the person types the user code.

What the edge does that the API's port does not:

- **The caller's address is the second-to-last entry of the one `X-Forwarded-For` line.**
  Google's external Application Load Balancer writes the header as `[<what the client
  sent>,]<client address>,<load balancer address>`, so the last entry is the balancer's own, the
  same for every caller, and anything before the second-to-last is the caller's own text. A login
  request that carries the header as more than one line is refused `400 invalid_request`
  `REQUEST_INVALID`: Google documents the header as one line and not which of several lines it
  appends to, and a balancer that appended to the first line would leave the client's own line
  last. So is one whose line holds fewer than two entries, or whose entry is no IP address: it did
  not come through the balancer as it writes the header. `GET /healthz` reads no address: the
  balancer's own probe calls it. `BROKER_TRUSTED_PROXY_HEADER` is not read at the edge.
- **The operator puts one proxy in front of the edge: Google's load balancer, and nothing between
  it and the broker.** The rule above takes the second-to-last entry because that balancer is the
  one hop that appends to the header. A second proxy that appends its own entry, such as a
  sidecar or an ingress behind the balancer, would make the second-to-last entry the balancer's
  for every caller and put the whole internet in one bucket. The broker cannot see that at boot,
  so the deployment must not add one.
- **Only Google's load balancer may reach the edge's port.** The broker does not check the address
  of the peer that connects to the edge: it reads `X-Forwarded-For` from whatever reaches that
  port, and holds no list of trusted proxies. A caller that reached the port without the balancer
  could write the header itself and pick its own buckets, and would pass no Cloud Armor rule. So
  the deployment's ingress firewall must allow the edge's port from Google's front-end proxy
  ranges alone, the ranges a global external Application Load Balancer connects from
  ([Google's load balancer firewall rules](https://docs.cloud.google.com/load-balancing/docs/firewall-rules)):
  `35.191.0.0/16` and `130.211.0.0/22` for IPv4, and `2600:2d00:1:1::/64` for IPv6. The
  balancer's health checks come from `35.191.0.0/16` for IPv4, inside the first range, and from
  `2600:2d00:1:b029::/64` for IPv6, which the rule must allow too when the backends are IPv6.
- **Its rate-limit buckets are its own.** The edge spends the address, login and token buckets of
  [Network and trust](#network-and-trust) at the same limits, in buckets the API's port never
  reads, and its per-login keys open with `edge:`. A flood at the edge spends nothing a caller of
  the API's port is held to, and naming a person there cannot lock that person's machine logins
  out on the private network. Three of its buckets differ from the API's port:
  - A person's bucket is that person's at the caller's address
    (`edge:person:<length of login>:<login>:<address>`, the length first because a login and an
    IPv6 address can both hold `:`), and only a login the broker opened spends it, so anyone on the
    internet naming a person can spend their own address's bucket for that person and never the
    one the person uses.
  - An IPv6 caller is keyed on the `/64` that holds its address, since one host or one site holds
    a whole `/64`.
  - Every login the edge opens spends one token of one more bucket that all callers share: a
    burst of 30 and one every two seconds, per pod. A request the device login would refuse
    (another client's id, an object that does not verify, a service's login, a missing
    `login_hint`, a `dpop_jkt` that is not the object's key) spends nothing of it; one that passes
    those checks and opens no login, a replay among them, spends one. It caps
    the pending edge logins a pod holds, about 480 at the 15-minute pending time, three rows each,
    however many addresses send them. Its cost:
    a caller who sends valid device authorizations from about 15 addresses, each within its own
    address bucket, holds it empty on every pod, and every outside person's new device login at
    the edge is refused `429` until the flood stops; the burst refills in a minute after that.
    Refresh, device-code polls and trades at the edge, and the broker's own address, do not spend
    it. It is a constant of the release, with no setting. Per-address rules at the load balancer
    (Cloud Armor), once the deployment's edge security policy applies them, refuse one address's
    flood before it reaches a pod; the deployment must apply them, since the broker cannot.
- **Its health check pings Postgres at most once a second.** `GET /healthz` at the edge answers
  from one ping for a second, so a caller on the internet who asks at any rate costs the database
  at most one ping a second per pod. The ping runs on a context of its own for at most two
  seconds, so a caller who leaves neither cancels it, which would close the pool's connection,
  nor keeps its result from the next check. A check that asks while a ping runs waits for it and
  leaves when its own request ends. Each failing ping, one that hangs past its two seconds
  included, writes one `broker: ping Postgres failed` line at ERROR, naming `port=edge`, and no
  caller writes one, so the edge logs a failing database at most once a second per pod. A
  draining pod still fails it at once.
- **A person of the Workspace cannot be named there.** A device authorization whose `login_hint`
  is an address on `BROKER_SIGNIN_DOMAIN` or a `BROKER_SIGNIN_WORKSPACE_DOMAINS` domain, or on a
  subdomain of one, in any case, with a trailing dot or written in characters IDNA maps onto one,
  or a login that is no address at all, is refused `400 invalid_request` `EDGE_WORKSPACE_LOGIN`
  before it spends any bucket or writes any row. The edge reads the `login_hint` as the verifier
  of the request object reads it. A person of the Workspace logs machines in on
  `BROKER_PUBLIC_URL`. The answer to a login the edge takes is the same whether or not the broker
  knows the person it names.
- **A person of the Workspace cannot use a machine login there either.** A machine login of a
  person of the Workspace is opened on `BROKER_PUBLIC_URL`, and the edge serves none of its
  tokens, so a machine-login file copied off a developer's machine is no use from the internet:
  at the edge its refresh and the poll of its device code are refused `400 invalid_grant`
  `EDGE_WORKSPACE_LOGIN`, before the refresh moves the credential's expiry, and the trade of its
  access token is refused `400 invalid_request` `EDGE_WORKSPACE_LOGIN`. The login refreshes and
  trades on `BROKER_PUBLIC_URL` as before.
- **The token route takes three grants.** The device code, a refresh token, and the RFC 8693
  trade of one of the broker's own access tokens (`subject_token_type`
  `urn:ietf:params:oauth:token-type:access_token`) with no actor token. Every other grant, a pod's,
  a workflow run's and a Google trade among them, is `400 unsupported_grant_type`
  `EDGE_GRANT_REFUSED`, before any bucket is spent. The trade takes only a device login's own
  access token. A token another trade minted, a Google machine's, a person sign-in's and a
  session's among them, is the exchange client's own, which the exchange never trades again (`400
  invalid_grant`), and the edge refuses any other token but a device login's `400 invalid_request`
  `EDGE_GRANT_REFUSED`.
- **Both login routes take a form and nothing else.** A body of any type but
  `application/x-www-form-urlencoded`, `multipart/form-data` included, is refused `400
  invalid_request` `REQUEST_INVALID` before it is read.
- **The method is the edge's to hold.** The load balancer's route to the edge can match a path
  and not a method, so the edge answers `405` with `Allow: POST` to any other method on
  `/v1/oauth/device_authorization` and `/v1/oauth/token`.
- **Every request writes an access-log line.** No proxy of the deployment's own stands between
  the edge and its load balancer to keep an access log, so the broker writes one
  `broker: edge request` line at INFO for each request the edge answers, the health check, a `404`
  or `405` of its router and a refusal before any bucket among them
  ([Health and logs](#health-and-logs)). It names the method, the path without its query string,
  the protocol, the status, the `User-Agent` and `X-Forwarded-For`, and never the query string,
  the `Referer`, the body, the `Authorization` header, the DPoP proof or a cookie.
- **It keeps an idle connection open for 620 seconds.** Google's external Application Load
  Balancer keeps its own connection to a backend idle for 600 seconds, a fixed value, and asks
  that a backend's keepalive be longer
  ([Backend HTTP keepalive timeout](https://docs.cloud.google.com/load-balancing/docs/https/request-distribution#timeout-keepalive-backends)).
  It sends no `POST` again, so a connection the edge closed first would answer a login `502`. The
  API's port keeps its two minutes.

A machine login the edge opened is signed for `BROKER_EDGE_URL`, and the broker reads its record
back on `BROKER_PUBLIC_URL`'s routes. Every reader of a stored record (the pending lists, the
lookup, the decision and the launcher credential's chain) takes a machine login signed for either
address, and a session's request signed for `BROKER_PUBLIC_URL` alone, since the edge never opens
one. Each compares origins as a DPoP proof's `htu` is compared, so a record signed for
`https://secrets.example:443` is read back by a broker whose setting is `https://secrets.example`.

The load balancer's health check reads `GET /healthz` on the edge's port; route nothing else of
the broker through it, and admit to the port only the balancer's own source ranges.

## People who manage secrets

The broker writes no secret. A person or an agent creates, changes and deletes agent secrets with
`agent-secrets secret` ([manage a secret](/legion/broker/guides/manage-a-secret/)), which calls
the store itself under the credential of the first source its environment gives it - AWS, the
AWS SDK's own order: access keys in the environment, then `AWS_WEB_IDENTITY_TOKEN_FILE`, then
the profile `AWS_PROFILE` or `AWS_DEFAULT_PROFILE` names, then the rest of the SDK's default
chain; GCP, an access token in `CLOUDSDK_AUTH_ACCESS_TOKEN`, else the credentials file
`GOOGLE_APPLICATION_CREDENTIALS` names, else the gcloud configuration or login
`CLOUDSDK_ACTIVE_CONFIG_NAME` or `CLOUDSDK_CORE_ACCOUNT` names, else the well-known credentials
file, else the active gcloud account, else the metadata server; a source present but broken stops
the command, and every form names the source it used - so that cloud's own IAM is what decides who
may change which secret, and with the
tags (AWS) or annotations (GCP), who gets it. The CLI asks the broker only for its settings
(`GET /v1/settings`) and, after each write, for a reread. Before anything else it refuses, on AWS,
a sign-in in another account than the agent-secrets key's (GCP needs no equivalent: every GCP call
already names the project explicitly, so the API itself refuses one in another project). It
refuses no credential for the kind of identity it is, a person's or a machine's: the cloud's IAM
is the boundary, so grant the namespace's write permissions to no machine identity that should
not write the agent secrets. Whoever holds a credential needs each form's permissions on the
namespace's secrets, and the tables below say what each request carries that a policy's
conditions can match.

The CLI writes a type, and stores a value in a secret that holds one, only through a broker whose
`GET /v1/settings` answers `secret_type_tag: true`, which every broker from the first release that
reads the `type` tag answers. Against an earlier broker, which reads no `type` tag and, from
legion-envoy-v16.0.0 on, would serve a secret that carries `type` alone as a stored secret,
`secret create --type`, `secret set` on a type secret and a `secret retag` that leaves the secret
a type each exit 1 before they read a value or write anything, naming the release the CLI is
from; a write that leaves no type goes ahead. After the write, `create`, `set` and `retag` each
read the secret's type tag back from the store, under the sign-in they wrote with, and exit 1
when it is not the type they wrote, when it is gone, or when the read back fails, naming the
secret and both types; they ask the broker to reread the secret in every case, even one whose
reread the broker refused. The CLI signs that reread only with a broker identity the process
already holds: a key directory holding its key and enrollment, a person's sign-in, a helper that
answers, or the machine's cached session. It waits on no launcher still enrolling and on no
machine registration in progress, and trades no machine identity. A person's sign-in is the one
that registers again: nothing renews its lease, so after `BROKER_LEASE_SECONDS` (900 by default)
the broker answers its proof `401` "not live", and the CLI registers the sign-in's session again
once, under the same box key — one trade of that person's sign-in, with the machine's identity as
its actor, and one enrollment — and sends the reread again, signed, at a second token of both
reread limits. Signed, the reread is answered with the type the broker serves, and a form exits 1
when it names another type than the secret holds, saying the retag that asks the broker to read
it again. Where the proof itself fails — no identity, an identity that cannot sign, a sign-in that
cannot register again, or a broker that does not accept the proof or cannot check it (`503
DATABASE_UNAVAILABLE`) — the CLI sends the reread again with no proof, which the broker answers
`202` and says no more of. The form then prints one line saying the broker's view was not checked
and why, and exits on the type tag alone: 0 when the store holds the type. That exit 0 says only
that the store holds the tag, not that the broker serves the secret as that type, so store no App
key or login in the secret until a retag run where the CLI has a broker identity exits 0 with
`broker: serving NAME from … (type …)`. A signed reread that ran and failed (`500`, `503
REQUEST_ENDED`), a spent reread limit (`429`) and a name no secret can carry (`400`) are not sent
again: the form exits 1 saying the broker could not be asked to reread, and the broker serves the
write from its next reload.

The CLI and the broker ship in one legion-envoy release, so move the broker's pin to that release
before people use its CLI. An `agent-secrets` from an earlier release still writes `kind`, which
this broker reads where `type` is absent, but it reads the reread's old `kind` field, which this
broker does not answer: every `secret retag` it makes that leaves a kind exits 1 saying the broker
still sees the old tags and to store no value until a retag exits 0, whatever the broker serves;
and its `--kind none` removes only `kind` and the guard and exits 0, so a secret that also carries
`type` keeps minting as that type. An `agent-secrets` from before the release whose broker
answers a reread with no proof `202` sends every reread with no proof and reads that answer as a
refusal: its `create`, `set`, `retag` and `restore` print
`broker: refusing NAME from … (reason=the reread was taken; … upgrade agent-secrets to the broker's release (the write stands))`
and exit 1, and its `delete` prints the same line and exits 0. Every one of those writes stands.
Use the CLI of the broker's own release.

Two types are not a person's to write. A `google-service-account` or `anthropic-federation`
secret the deployment binds to an account (`BROKER_SERVICE_ACCOUNT_SECRETS`) releases a token that
acts as that account, or, for `anthropic-federation`, as the Anthropic service account the value
names, and the secret's own tags say who may ask for it and which scopes a `google-service-account`
token carries, so whoever may write those tags decides who can get a token as that account. A
deployment that binds names adds one
term for each bound name to the grant it gives its
people, rather than withholding the grant: on GCP a
`!resource.name.startsWith("projects/<number>/secrets/<bound name>")` term for each, so the
people's write stops at the namespace's bound names; on AWS a `Deny` on those secrets' ARNs for
`CreateSecret`, `PutSecretValue`, `TagResource` and `UntagResource`. Name those secrets in the
term, since the setting names them, rather than testing the `type` tag.

The binding names a secret by its name alone, so a bound name with no secret behind it is open to
whoever may create a secret in the namespace: they create it under that name, own it, list
themselves, store the bound account as its value, and the broker mints that account's token for
them. So keep every bound name held by the administrator's own secret, which is what the term
above leaves to the administrator. Create the secret before
its binding is written, and remove the binding before the secret is deleted, since a deleted
secret's name is free again once its recovery window ends.

A `google-service-account` or `anthropic-federation` secret under a name the deployment binds to nothing mints nothing, so
a person who creates one of their own under such a name, with any account in its value, is
answered `MINT_FAILED` rather than a token. The `requesters` tag bounds who may *ask* for a
secret; the binding is what bounds which account it mints for.

### AWS Secrets Manager

| Form | Permissions | What a condition can match |
| --- | --- | --- |
| every form | `sts:GetCallerIdentity`, which needs no permission; a sign-in with a profile's `role_arn` needs `sts:AssumeRole` on that role, and one with a web identity token file the role's trust of it (`sts:AssumeRoleWithWebIdentity`) | |
| `list` | `secretsmanager:ListSecrets` (on `*`: it takes no resource) | |
| `show` | `secretsmanager:DescribeSecret` | The secret's own tags (`aws:ResourceTag/owner`, `aws:ResourceTag/tier`) |
| `create` | `secretsmanager:CreateSecret` and `secretsmanager:TagResource`, since it tags the secret as it creates it; `kms:GenerateDataKey` and `kms:Decrypt` on the agent-secrets key | The new secret's name under the namespace (its ARN, and `secretsmanager:Name`); the request tags: `aws:RequestTag/owner` (a person's email, from `--owner me` or `--owner <email>`, or `shared`) and `aws:RequestTag/tier` always; `aws:RequestTag/type` and `aws:RequestTag/mint-service-account` (the rollback guard, `type-secret`, written beside every type) with `--type`; `aws:RequestTag/github-owners` (GitHub logins separated by spaces) with `--type github-app`, which needs it; `aws:RequestTag/requesters` (emails and service names separated by spaces) with `--requesters`, and with `--project` or `--project-channels`, which write the project tags' rollback guard `rollback-guard:project-tags` after the list ([Rolling back past the project tags](#rolling-back-past-the-project-tags)); `aws:RequestTag/scopes` (OAuth scope names separated by spaces) with `--scopes`; `aws:RequestTag/admin` (`true`) with `--admin`; `aws:RequestTag/dispatch-project` (one Dispatch project key) with `--project`; `aws:RequestTag/project-channels` (`slack`) with `--project-channels`; `aws:RequestTag/max-grant` with `--max-grant`; and `aws:RequestTag/default-grant` with `--default-grant` (`aws:TagKeys` is the tags sent, so a policy that limits it must allow `owner`, `tier`, `type`, `mint-service-account`, `github-owners`, `requesters`, `scopes`, `admin`, `dispatch-project`, `project-channels`, `max-grant` and `default-grant`, the whole set a create carries, to which a retag's untag adds `kind` (`retag` below); unproved: no check runs this policy); the key, named by its ARN (`secretsmanager:KmsKeyArn`); the `TagResource` check made on the new secret sees the requested tags as its own (`aws:ResourceTag/owner`, `aws:ResourceTag/tier`), so a policy that reserves a shared secret's tags for administrators still needs a statement that admits a shared create (request and resource owner both `shared`, the same tier). A secret created with `--owner shared` and `--tier agent` is served to every agent session without approval, so only a person whose access allows a shared create can create one. |
| `set` | `secretsmanager:DescribeSecret`, since a set reads the secret's tags for the type it checks the value as before it writes, and a refused read names that permission; `secretsmanager:PutSecretValue`; `kms:GenerateDataKey` on the agent-secrets key, and `kms:Decrypt` where the value is one a version already holds, which Secrets Manager decrypts to compare | The secret's own tags (`aws:ResourceTag/owner`) |
| `retag` | `secretsmanager:DescribeSecret`, and `secretsmanager:TagResource`, or `secretsmanager:UntagResource` for `--type none`, `--github-owners none`, `--requesters none`, `--scopes none`, `--admin none`, `--project none`, `--project-channels none`, `--max-grant none` or `--default-grant none`. Three kinds of removal send a tag request too, because of the project tags' rollback guard ([Rolling back past the project tags](#rolling-back-past-the-project-tags)): `--requesters none` on a secret that keeps a `dispatch-project` or `project-channels` tag sends a tag request alone, writing `requesters` as the guard alone; a `--project none` or `--project-channels none` that removes the last of those two tags from a secret whose `requesters` tag holds a list beside the guard sends its untag request and then a tag request writing that list without the guard; and any other removal on a secret that keeps a project tag whose `requesters` tag lacks the guard (a tag a console wrote) sends its untag request and then a tag request writing `requesters` with the guard. A retag that changes the secret's type also reads the secret's value to check it as that type's minter reads it, before it writes: `secretsmanager:GetSecretValue` on the secret and `kms:Decrypt` on the agent-secrets key. The check needs that read and the retag does not: where Secrets Manager refuses the read with `AccessDeniedException`, the retag writes its tags and prints one line on standard error that says the value was not checked, and the broker then reads the value itself and refuses the secret `value-malformed` when its minter could not mint from it. Any other failure of the read refuses the retag, which then writes nothing. A retag never prints the value | A retag reads the secret's tags, sends one request (two for the removals the first column names that then write the guard), then reads them back. A tag request carries owner and tier, the unchanged one re-sent as the secret holds it, plus `type` and `mint-service-account` (the rollback guard) when the command sets a type, `github-owners` when it names owners, `requesters` when it names who may ask or when it leaves the secret with a project tag whose rollback guard the `requesters` tag lacks, `scopes` when it names scopes, `admin` when it marks the secret an administrator's, `dispatch-project` when it names a project, `project-channels` when it sets that tag, `max-grant` when it sets one, and `default-grant` when it sets that; those twelve keys — `owner`, `tier`, `type`, `mint-service-account`, `github-owners`, `requesters`, `scopes`, `admin`, `dispatch-project`, `project-channels`, `max-grant` and `default-grant` — are the whole set a tag request carries. An untag request's `aws:TagKeys` is the tags `none` removes, whether or not the secret holds them: `--type none` removes `type`, the old classifier `kind` and `mint-service-account` together, and `--github-owners none`, `--requesters none`, `--scopes none`, `--admin none`, `--project none`, `--project-channels none`, `--max-grant none` and `--default-grant none` each remove the tag they name, and a removal of the last project tag also removes `requesters` where it held the rollback guard alone; a retag that removes a tag changes nothing else, so a refused call leaves the secret as it was. So a policy that limits `aws:TagKeys` with `ForAllValues:StringEquals` lists exactly those twelve keys and `kind` (unproved: no check runs this policy), and one written for a release before the `type` tag, which lists `kind` in place of `type`, refuses every `--type` create and retag until it adds `type`. A retag that would leave a `github-app` secret without `github-owners`, or an `admin` or `google-service-account` secret without a `max-grant` of an hour or less, is refused before any write. When the read-back finds another writer's change left a `github-app` secret without `github-owners`, the retag writes nothing more: it names the tags it read, the tags it found and the retag that sets the secret right, and exits 1, so the other writer's change stands. The secret's own tags let a policy allow a person to tag their own secret (`aws:ResourceTag/owner` their email) to themselves or `shared`, and reserve a shared secret's tags for administrators |
| `delete` | `secretsmanager:DeleteSecret` | The secret's own tags; `secretsmanager:RecoveryWindowInDays` is 30, and `secretsmanager:ForceDeleteWithoutRecovery` is never set |
| `restore` | `secretsmanager:RestoreSecret` | The secret's own tags |

Secrets Manager makes the KMS calls itself, on the caller's behalf, so the agent-secrets key's
policy can allow them only through Secrets Manager (`kms:ViaService` of
`secretsmanager.<region>.amazonaws.com`, with `kms:CallerAccount`). The email in
`aws:RequestTag/owner` is lowercase: `--owner me` writes the Identity Center session name in
lowercase, and `--owner <email>` lowercases the email it names, so a condition that compares it
with a principal tag holding the caller's email matches only where that tag is lowercase too.
No form prints a value. One form reads one: a `retag` that changes the type calls
`GetSecretValue` to check the value and then drops it (the row
above). `delete` and `restore` are permissions of their own: an access that grants a person every
other form on their own secret but not these refuses those two with `AccessDeniedException`.

### GCP Secret Manager

| Form | Permissions |
| --- | --- |
| `list` | `secretmanager.secrets.list` on the project and `secretmanager.versions.get` on each secret (to tell whether its `versions/latest` is `ENABLED`) |
| `show` | `secretmanager.secrets.get` and `secretmanager.versions.list`, `secretmanager.versions.get` (for its versions and whether `versions/latest` is `ENABLED`) |
| `create` | `secretmanager.secrets.create` and `secretmanager.versions.add`. Automatic replication encrypts with the CMEK key through Secret Manager's own service agent, not the person's identity: that service agent (`service-<project number>@gcp-sa-secretmanager.iam.gserviceaccount.com`) needs `roles/cloudkms.cryptoKeyEncrypterDecrypter` on the key, granted once at setup, not per person. The create labels the secret `agent-secrets-create` with a random value of its own, which stays on the secret, so a create needs no `secretmanager.secrets.update`; the broker reads only whether a secret carries that label, never its value. Before it asks for the value, the create asks IAM, through `TestIamPermissions` on the new secret, which needs no permission of its own, whether the sign-in holds `secretmanager.versions.add`, and deletes the empty secret and stops where it does not. A create also uses, without needing them to store a value: `secretmanager.secrets.get`, to read the secret back before any delete, or after a create whose answer was lost, touching it only where that label holds its value; and `secretmanager.versions.list` and `secretmanager.secrets.delete`, to delete again the empty secret a create that stores no value made. Without them a create that succeeds is unchanged, and one that fails leaves the empty secret and names the `gcloud secrets delete` command that clears it |
| `set` | `secretmanager.secrets.get`, since a set reads the secret's annotations for the type it checks the value as before it writes, and a refused read names that permission, and `secretmanager.versions.add` |
| `retag` | `secretmanager.secrets.get` and `secretmanager.secrets.update`: owner and tier annotations in one call, plus `type` and `mint-service-account` (the rollback guard) when the command sets a type, `github-owners` when it names owners, `requesters` when it names who may ask or when it leaves the secret with a project tag whose rollback guard the `requesters` annotation lacks ([Rolling back past the project tags](#rolling-back-past-the-project-tags)), `scopes` when it names scopes, `admin` when it marks the secret an administrator's, `dispatch-project` when it names a project, `project-channels` when it sets that annotation, `max-grant` when it sets one, and `default-grant` when it sets that; or, for `none`, the annotations it removes (`--type none` removes `type`, the old classifier `kind` and `mint-service-account` together, and a removal of the last project tag removes `requesters` too where it held the rollback guard alone), and nothing else. `--requesters none` on a secret that keeps a project tag writes `requesters` as the guard alone, and a removal of the last project tag from a secret whose `requesters` holds a list beside the guard is followed by a second update writing that list without it. A retag that changes the secret's type also reads the value of `versions/latest` to check it as that type's minter reads it, before it writes: `secretmanager.versions.access` on the secret. The check needs that read and the retag does not, and people who add versions and change annotations often hold no accessor role: where Secret Manager refuses the read with `PermissionDenied`, the retag writes its annotations and prints one line on standard error that says the value was not checked, and the broker then reads the value itself and refuses the secret `value-malformed` when its minter could not mint from it. Any other failure of the read refuses the retag, which then writes nothing. A retag never prints the value |
| `delete` | `secretmanager.secrets.update` (sets `expire_time` to 30 days out) |
| `restore` | `secretmanager.secrets.update` (clears `expire_time`) |

Unlike AWS's tags, a GCP IAM condition cannot test an annotation's value, so GCP has no per-secret
counterpart to AWS's "a person may tag only their own secret" conditions above, and nothing stops
a write from reaching another person's secret: whoever is granted `secretmanager.secrets.update`
on the namespace can retag any secret under it to any owner, tier or type, shared secrets included,
and whoever is granted `secretmanager.versions.add` can replace any secret's value under it. A
deployment may grant its people every write under the namespace (`secrets.create`, `.get`,
`.update`, `.delete`, `versions.add`, `.get`, `.list`) with one condition,
`resource.name.startsWith("projects/<number>/secrets/<prefix>")`, and no project-wide list and no
`versions.access`. Whoever holds it can then retag any secret under the namespace to any owner
and tier, and the broker serves by those tags, so the owner tag records whose secret it is, and
the grant reaches every secret under the namespace. A deployment that wants fewer writers grants
it to fewer people. The AWS counterpart is a policy of `secretsmanager:CreateSecret`,
`TagResource`, `UntagResource`, `PutSecretValue`, `DescribeSecret`, `DeleteSecret` and
`RestoreSecret` on `arn:aws:secretsmanager:<region>:<account>:secret:<prefix>*`, with
`kms:GenerateDataKey` and `kms:Decrypt` through Secrets Manager on the agent-secrets key, and no
name or owner condition, so both backends behave alike (unproved: no check runs this policy).

## Changing the proxy's hosts

Every intermediate Cipher's proxy signs with carries name constraints, marked critical, of two
parts ([the proxy's chain](/legion/broker/concepts/#the-proxys-chain)): its permitted DNS names are
exactly the names of the hosts the profiles file binds, and it excludes every IPv4 and every IPv6
address (`0.0.0.0/0` and `::/0`) and permits no IP range. The exclusion is there because a name
constraint applies only to names of its own kind (RFC 5280, section 4.2.1.10): permitted DNS
names alone constrain no IP address, so the intermediate's key would sign a trusted leaf for any
address. URI and email names are left unconstrained, since no TLS client takes either as the
server's identity.

What the constraints bound depends on the client, measured on an intermediate of this shape with
leaves signed by its own key:

- Go's `crypto/tls` never takes a subject's common name as the server's name, so it refuses every
  leaf that names a host only there. GnuTLS (which git uses) refused every such leaf too.
- Clients built on OpenSSL (`openssl verify`, curl, Python's `ssl`, Node) read the subject's
  common name when the leaf has no DNS alternative name. They accept a name outside the allowed
  ones when that common name is one label, such as `localhost`, or a wildcard over two or more
  labels, such as `*.evil.example` for `a.evil.example`. They refuse a plain name, such as
  `evil.example`, and a wildcard over one label, such as `*.example`. So for them the key reaches
  names of one label, and names of three or more labels through a wildcard, but no name of two
  labels.
- Bun refuses a leaf with no subject alternative name under a constrained issuer. Once the leaf
  also carries a URI, email or registered-ID alternative name, Bun accepts any name through its
  common name.

So against OpenSSL-based clients and Bun, a leaked pod key is bounded by the intermediate's life,
at most 7 days and 5 minutes, and not by its names.

The pool and its issuance policy hold to five settings, each of which a pod checks in every answer
or relies on:

- **One certificate authority in the pool.** A pod's request names no authority, so the service
  picks one of the pool's, and the pod refuses any answer under another root than its first
  answer's, since every release it made names that root. With two authorities enabled, pods pin
  different roots, and a command whose release came from one pod fails at another. To rotate, put
  the new root into `BROKER_PROXY_RELEASE_ROOTS_FILE` and roll the pods first, then add the new
  authority, disable the old one, and roll the pods again.
- **`critical: true` on the policy's name constraints.** The service's `critical` field is a plain
  boolean, so a policy that leaves it out issues constraints that are not critical, and every pod
  refuses them (`its name constraints are not marked critical`).
- **`serverAuth` as the policy's one extended key usage.** The pod refuses an intermediate whose
  extended key usage is anything else (`its extended key usage is […], not serverAuth alone`).
- **`backdateDuration: 300s`.** An intermediate the service issues starts at the second of its
  issue unless the policy backdates it, and a command whose clock runs behind would find it not
  yet valid. The pod covers a minute of that itself, signing with its previous intermediate until
  a minute after the new one's start, but a backdated intermediate never needs it. The service
  keeps the requested lifetime, so a backdated intermediate still lasts 7 days, within the bound.
- **`maximumLifetime: 604800s`, 7 days, the life a pod asks for.** A pod renews when less than 2
  days remain, so a pool that answers 7 days is asked once every 5 days for each pod. A pool whose
  policy answers 2 days or less leaves less than 2 days the moment an answer arrives, so the pod
  asks again 5 minutes after each take: about 288 certificates a day for each pod, about $144 a
  day at $0.50 a certificate. Nothing but the `proxy intermediate` line, once for each answer,
  shows it. An answer shorter than about 10 minutes, the 5-minute floor plus the 300-second
  backdate, ends before that floor, so the pod holds an ended intermediate and is not ready
  (`503 NO_INTERMEDIATE`) from the answer's end until the request at the floor brings the next.

A pod refuses an intermediate with a DNS name more or a name less, or one without the exclusion.
So a change that adds a bound host to the profiles file, or removes one, changes the names every
intermediate must carry, and it follows these rules.

- **On a pool (`BROKER_PROXY_CA_POOL`), apply the pool's issuance policy first, then roll the
  pods.** The policy's name constraints become the new hosts' names, and only then do pods start
  with the new profiles file. The policy's name constraints overwrite the request's, so they carry
  the exclusion too (`excludedIpRanges: ["0.0.0.0/0", "::/0"]`): a policy without it gives every
  pod an answer it refuses (`its name constraints exclude the IP ranges []; …`). A pod started on
  the new file while the policy still carries the old names is refused its first intermediate
  (`its name constraints permit […]; want exactly the bound hosts' names […]`) and is not ready,
  so the roll keeps the old pods. To apply both in one change, make the broker's Deployment depend
  on the policy, so that the same apply updates the policy and then rolls the pods; a deployment
  has that order only where its own deployment code declares the dependency. Between the two, an
  old pod that renews is refused the new names in the same way, keeps signing with the
  intermediate it holds, and retries until the roll replaces it. So a roll that stalls leaves each
  old pod signing only until that intermediate ends: between about 2 days (the time a pod renews
  ahead) and 7 days after the policy change, and less where the pod was already retrying.
- **On a mounted intermediate (`BROKER_AIRLOCK_CA_CERT_FILE`), issue a new intermediate for the new
  names at every host change.** It carries the same constraints, the IP exclusion included. A
  broker started on the new profiles file with the old intermediate refuses to start. Today's
  self-signed root is not an intermediate, carries no name constraints, and is exempt from this
  rule.
- **A mounted intermediate is never renewed by the running broker.** Issue the next one and restart
  the pods before it ends, at most 7 days and 5 minutes after its start, the longest life a broker
  takes. Once it has ended, each pod answers `GET /healthz` with `503 NO_INTERMEDIATE`, signs no
  leaf, and a restart on it refuses to start.

The pod compares the names after it renders the profiles file's hosts as the certificate request
asks for them (`requestedNames`): each host as written, with a `*.` pattern's `*.` removed, so
`*.googleapis.com` is `googleapis.com`, never `.googleapis.com`. Case is ignored, and a name two
hosts give appears once. Write the policy's names, or a mounted intermediate's name constraints,
in that form, with no leading dot.

## Health and logs

`GET /healthz` answers `200 {"status":"ok"}` while the broker can reach Postgres, and
`503 DATABASE_UNAVAILABLE` when it cannot. A broker that runs Cipher's proxy answers
`503 NO_INTERMEDIATE` while its proxy holds no certificate it may sign with: before the first
answer of the pool `BROKER_PROXY_CA_POOL` names, or once its intermediate has ended with no new
one ([the proxy's chain](/legion/broker/concepts/#the-proxys-chain)), so a rolling update keeps
the old pods until a new pod has its intermediate. That reach is the whole broker: the process
goes on serving every other route, but Kubernetes takes a pod that fails its readiness probe out
of every Service that selects it, and the edge's `GET /healthz` answers from the same check. So a
pool outage longer than the 2 days a pod renews ahead, a lost `privateca` IAM grant, or a policy
whose every answer the pods refuse takes down the whole broker once each pod's intermediate ends,
releases of names no kind binds included. Alert on the `proxy intermediate request failed`
log line, which a pod writes for each failed request from the first one, well before its
intermediate ends. From the signal that stops it, it answers `503 SHUTTING_DOWN` instead, for the
whole drain, while every other route goes on being served exactly as before. Use it as a
readiness probe and never as a liveness probe: a liveness probe reads a draining pod as a dead
one, so the kubelet would kill the container in the middle of the drain and drop the requests the
drain exists to finish ([Running more than one pod](#running-more-than-one-pod)).

The broker writes its log to stderr as one JSON object per line, in the shape Cloud Logging reads
as a structured entry with a severity of its own. Each object carries `time` (RFC 3339 with
nanoseconds, in UTC), `severity` (`INFO`, `WARNING` or `ERROR`: the broker writes nothing below
`INFO`), `message`, and each field of the line as a member of its own. A duration is written as
Go writes one, such as `"every":"1m30s"`. This is the line a broker writes once it serves, here on
the [local stack](/legion/broker/guides/run-locally/):

```json
{"time":"2026-11-14T02:26:30.105447105Z","severity":"INFO","message":"broker listening","addr":"127.0.0.1:46052"}
```

A line records a change of state, not a pass of a loop. The five-minute policy reload, each of
the request sweeper's sweeps, the hourly sign-in client check and the read of a type secret's
value each write one line when they start to fail and one when they succeed again. The policy
writes a line when a secret's state changes, not at each load. A standing state is what the
metrics hold ([Metrics](#metrics)).

The table names each line by its `message`, then its fields as `field=<value>`, and says the
severity it is written at (WARN is written `WARNING`). The lines worth alerting or searching on:

| Log line | Meaning |
| --- | --- |
| `broker stop budget drain=<drain> shutdown=60s total=<their sum>` | The first line of the run, before the database or the secret store is reached for: the whole budget a stop spends, `BROKER_DRAIN_SECONDS` and the 60 seconds in-flight requests get after it. `total` is what a deployment's `terminationGracePeriodSeconds` has to cover, and with the defaults the line reads `drain=5s shutdown=60s total=65s` ([Running more than one pod](#running-more-than-one-pod)). |
| `database sign-in method=<method>` | At startup, before the broker reaches for AWS or its database: `rds-iam` when each connection signs in with an RDS IAM token the broker mints, `password` when it signs in as `BROKER_DATABASE_URL` and libpq's defaults say (a password in the URL, `PGPASSWORD`, a passfile, or none for a server that trusts it). |
| `secret store store=<store> credentials=<source>` | At startup, once the store is built: `aws-secrets-manager` under the AWS SDK's credential chain (`aws-sdk`), `gcp-secret-manager` under Application Default Credentials (`application-default`), or either name over `BROKER_FAKE_SECRETS_FILE` (`fake secrets file`). |
| `approval sign-in on issuer=<issuer> client_id=<client id> domain=<domain>` | At startup: the approval page signs people in through that client, holding each person to that Workspace domain ([Approval sign-in](#approval-sign-in)). The client's secret is never logged. |
| `approval sign-in off detail=…` | At startup, in place of the line above: `BROKER_SIGNIN=off`, so the broker serves no approval page and denies at once a request a person would approve ([A broker that signs nobody in](#a-broker-that-signs-nobody-in)). No sign-in client check runs. |
| `broker: the sign-in client check state=<state> issuer=<issuer> error=…` | At WARN, from the check each pod takes at startup and every hour after, when it finds a state other than `ready` that the pod did not log last: the provider refused the check's request (`state=client_rejected`; a wrong client id or secret is the usual cause, and `error` names the status and the OAuth error), or the check got no verdict (`state=issuer_unreachable`: an outage, a `5xx`, `408`, `429`, an answer cut off before its body arrived whole, or no answer, or an answer that is no refusal, a `2xx` or `3xx`). A check that finds the state the pod logged last, or finds the client ready with no such line before it, logs nothing, so a provider that stays away for a day is one line. Every check writes its answer to `GET /signin/status`. |
| `broker: the sign-in client check finds the client ready again issuer=<issuer> was=<state>` | At INFO: the first check that finds the client ready after the line above. `was` is the state that line named. |
| `broker: the sign-in client check was not written state=<state> error=…` | At WARN: a check could not write its answer to Postgres after the check before it wrote, so `GET /signin/status` answers the last answer written. The checks after it log nothing more until one writes. |
| `broker: the sign-in client check is written again state=<state>` | At INFO: the first check that writes its answer after the line above. |
| `approval sign-in login=<email>` / `approval sign-out login=<email>` | A person signed in to, or out of, the approval page. |
| `approval sign-in refused …` | At WARN: a sign-in made no session, the line saying why: the provider refused it, its state or return path was not the sign-in's, the code exchange or ID token failed, or the person is outside `BROKER_SIGNIN_DOMAIN` or unverified. |
| `approval page decision login=<email> record=<record id> decision=<approve or deny> outcome=<outcome>` | A person decided a secret request on the page; `outcome` is what became of it (`granted`, for one). |
| `approval sign-in: reach the provider issuer=<issuer> error=…` | At ERROR, and the page answered `503` naming the issuer: at the start of a sign-in, the provider's discovery document could not be read; at its callback, the discovery document could not be read, or the token endpoint answered the code exchange `408`, `429` or a `5xx`, answered with a body cut off before it arrived whole, or did not answer. That closed set is the provider down; every other answer of the token endpoint is the provider answering, which this line never logs: the sign-in is refused `403` and logs `approval sign-in refused`. A failure at the callback writes `issuer_unreachable` to `last_failed_sign_in_reason` in `GET /signin/status`; one at the start writes no status, and `client_check` says `issuer_unreachable` once a client check, at startup or on the hour, finds the provider down ([What the sign-in is doing](#what-the-sign-in-is-doing)). A callback whose browser left during the exchange is not this line: it logs `approval sign-in refused`, and writes no status. |
| `broker listening addr=<host:port>` | Startup finished: configuration, migrations and the first read of the namespace all succeeded, and every port is bound: the API's, at this address; the airlock's proxy port, where the deployment has one (`airlock proxy listening addr=<host:port>`, just after); the edge, where the deployment has one (the line below); and the metrics port. |
| `proxy certificate authority source=pool pool=<pool> renew_before=<duration> release_roots=<n>` / `proxy certificate authority source=mounted release_roots=<n>` | At INFO, at startup, on a broker with the airlock: where the proxy's certificate authority comes from, the pool `BROKER_PROXY_CA_POOL` names or the mounted pair, how long before its intermediate's end a pod on a pool asks for the next (2 days, unless the local stack's `BROKER_PROXY_TEST_RENEWAL_MARGIN` says otherwise), and how many roots of `BROKER_PROXY_RELEASE_ROOTS_FILE` each release carries beside the chain's own. |
| `proxy intermediate serial=<hex> names=<names> not_before=<time> not_after=<time> self_signed=<bool>` | At INFO, once for each certificate the proxy signs with: the mounted one at startup, and each intermediate a pod on a pool takes, the first and each renewal. `names` are its name constraints, the names it may sign a leaf for and the names under them, space-separated, and empty for a certificate that has none. The pool also lists each intermediate it issued, by this serial. |
| `proxy intermediate request failed pool=<pool> error=… retry_in=<duration>` | At WARN, once for each request for an intermediate that got none: the pool did not answer or refused, or its answer is refused because it certifies another key, reaches no root, chains to another root than the pod's first intermediate, is past a bound every intermediate is held to (`its path length is …, not 0`, `its key usage does not name signing certificates`, `its extended key usage is […], not serverAuth alone`, `its life is …, past the 168h0m0s a pod asks for and 5m0s of slack`, `its name constraints permit […]; want exactly the bound hosts' names […]`, the line a roll writes when the pool's policy does not yet name a host the profiles file added, `its name constraints permit IP ranges …`, `its name constraints exclude the IP ranges …; want them to say what a pod's CSR does, that excludes 0.0.0.0/0 and ::/0`, `… do not exclude 0.0.0.0/0` or `… do not exclude ::/0`, the line of a policy without the exclusion, or `its name constraints are not marked critical`), or could sign no trusted leaf for a bound host (`it signs no leaf a command trusts for <host>`). The pod asks again after `retry_in`, which starts at 1 second and doubles to 5 minutes. A pod that has no intermediate is not ready meanwhile; one that holds one goes on signing with it until its end. A pool that did not answer or refused issued nothing, but each refused answer is a 7-day certificate the pool issued and bills, for a new key each time. While the cause lasts, as on a roll before the policy apply, a second authority in the pool whose root is not the pod's first, or a policy past a bound, each pod asks about ten times in its first 9 minutes and then once every 5 minutes: about 288 certificates a day for each pod, 576 for two pods, about $288 a day at $0.50 a certificate, where the authority and the intermediates a deployment takes cost about $206 to $320 a month. This line reports each one, so alert on it. The pod does not stop asking on its own: apply the policy, or fix the pool's authorities, to stop it. |
| `broker edge listening addr=<host:port> url=<BROKER_EDGE_URL> verification_uri=<BROKER_EDGE_MACHINE_LOGIN_URL> workspace_domains=<domains>` | After `broker listening`, and after `airlock proxy listening` where the deployment has an airlock, on a broker with `BROKER_EDGE_LISTEN_ADDR` set: the edge is bound at this address, takes request objects and proofs naming `url`, sends a person to `verification_uri`, and refuses a login on any of `workspace_domains` ([The edge](#the-edge-device-login-from-outside-the-private-network)). A broker with no edge writes no such line. |
| `broker metrics listening addr=<host:port>` | After `broker listening`, after `airlock proxy listening` where the deployment has an airlock, and after `broker edge listening` where it has an edge: the metrics port is bound at this address ([Metrics](#metrics)). A pod that cannot bind it does not start, and logs `broker: fatal` naming the address. |
| `session account pairs state=on pairs=<pairs>` / `session account pairs state=off detail=…` | At INFO, at startup, after `broker metrics listening` and before the broker serves its first request: `state=on` names each pair `BROKER_SESSION_ACCOUNT_PAIRS` gives, as `<account>=<account>` in the setting's order, joined by one space, and `state=off` says the setting is unset, so an account is refused a session name another account holds. |
| `broker: fatal error=…` | Startup refused; the error names the variable or dependency. The process exits 1. |
| `agent secret policy refused name=<secret's name in the store> reason=<reason> owner=<owner tag> first_load=<true or false>` | At ERROR, when a secret becomes one the broker refuses to serve, and when it is refused for another reason than before, with `was=<the reason before>`: one line at the change, not one for each read of the store. A pod's first load writes one for each secret it refuses, each with `first_load=true`. A load or a reread that finds a secret refused for the reason it had writes nothing, and `broker_unservable_secrets` ([Metrics](#metrics)) holds every secret refused now. `name` is the secret's whole name in the store, such as `production/agent-secrets/deel-api-key`, or `profile:<NAME>` for a profile of the profiles file that the broker refuses against its source. `owner` is its owner tag (a profile's is its source's), and empty for `owner-tag-missing` and `owner-tag-malformed`. A refusal of the profiles file's ends with `detail=<what fails>`: the source and its own reason, the account or scope a block names that the source does not, the looser tier, requester or `maxGrant`. Reasons include `owner-tag-missing`, `owner-tag-malformed` (among others, a `group:` owner whose address is no lowercase email), `tier-tag-missing`, `tier-tag-malformed`, `name-malformed`, `service-owner-human-tier`, `not-on-agent-secrets-key`, `no-current-value`, `type-unknown`, `github-owners-missing`, `github-owners-malformed`, `requesters-malformed` (a `requesters` list holding a comma, naming nobody, carrying an email with a capital letter, holding a `group:` entry whose address is no lowercase email, or naming a service the deployment does not register), `type-human-tier` (an `anthropic-federation` secret that is not human tier), `max-grant-required` (an `admin` secret, or a `google-service-account` or `anthropic-federation` one, with no `max-grant` or one longer than an hour), `requesters-not-owner` (a `google-service-account` or `anthropic-federation` secret whose `requesters` do not name its owner once, a person or a group, and no other person or group, or that lists a service it may not: any service at agent tier, and any service at all on a secret `BROKER_SERVICE_ACCOUNT_SECRETS` binds to a `dev-<name>` account), `admin-malformed` (an `admin` tag set to anything but `true`), `admin-tier` (an `admin` secret that is not human tier), `admin-requesters` (an `admin` secret with no `requesters` list), `scopes-malformed` (a `scopes` list holding a comma, naming no scope, naming one twice, or holding an entry that is no scope name, a whole `https://www.googleapis.com/auth/…` URL among them), `scopes-type` (a `scopes` tag on a secret that is not a `google-service-account` one), `project-tag-misspelled` (a tag key that differs from `dispatch-project` or `project-channels` only in case or in its separators, such as `Dispatch-Project`, `dispatch_project` or `PROJECT-CHANNELS`, which the broker would otherwise read as no project tag at all), `project-malformed` (a `dispatch-project` tag that is not one Dispatch project key), `project-unchecked` (a `dispatch-project` tag on a broker with no `BROKER_PROJECT_ACCESS_URL`), `project-channels-malformed` (a `project-channels` tag set to anything but `slack`), `max-grant-malformed`, `default-grant-malformed` and `value-malformed` (a `github-app`, `github-installations`, `google-login`, `google-service-account`, `azure-login` or `anthropic-federation` secret whose stored value that type's minter could not mint from, among them a `github-installations` value of a `shapeVersion` other than 1 or of none: [the value read](#the-value-read)); of the airlock's kinds, `proxy-kind-unknown` (a `proxy` tag that names no kind of the profiles file; the detail names the tag's value), `proxy-way-not-in-kind` (the way the secret's type takes, `swap`, `mint`, `sign` or `exchange`, is not one of its kind's `ways`; the detail names both), `proxy-max-grant-above-kind` (a secret of a kind with a `max_grant` that sets no `max-grant`, or one above the kind's; the detail names both), `proxy-kind-conflict` (a profile whose `proxy` string names another kind than its source's `proxy` tag), `proxy-narrowed-malformed` (a `proxy-narrowed` tag set to anything but `true`), `proxy-narrowed-profiles` (a secret tagged `proxy-narrowed=true` that no profile names, whose one profile has no `allow`, or that more than one profile names; the secret and each such profile are refused); and, of the profiles file, `profile-source-missing` (the profile's source is no secret the broker serves: absent, refused, waiting for its value, or itself a profile), `profile-name-taken` (the profile's name is a stored secret's other than its source, and that stored secret carries no `superseded-by` tag whose value is exactly the profile's source; that stored secret is refused by its own name too. With such a tag the profile is served by the name and the stored secret is not), `profile-block-mismatch` (a `github` or `google` block that does not fit the source's type, `github-owners` or `scopes`, or a `github-app` or `github-installations` source's profile with no `github` block), `profile-looser` (agent tier on a human-tier source, a requester the source's `requesters` does not name, or a `maxGrant` above the source's `max-grant`), `profile-source-not-proxied` (a profile whose source is a stored token, a `google-login`, an `azure-login` or an `anthropic-federation` secret with no `proxy` tag and no `proxy` string; the source is refused by its own name too), `profile-allow-not-narrowed` (a profile with an `allow` of its own whose source carries no `proxy-narrowed=true` tag), `profile-allow-outside-kind` (a row of a profile's `allow` that no row of its kind's `allow` covers; the detail names the row), `profile-allow-host` (a row of a profile's `allow` naming a host its kind does not bind), `github-app-no-profile` (a `github-app` secret no profile names, which the broker refuses by its own name) and `github-installations-no-profile` (the same of a `github-installations` secret). A profile is held to the tag rules on its own tags over its source's, so a profile of an `admin` source can be refused `admin-tier`, and an agent-tier profile of an `anthropic-federation` source `type-human-tier`. Every secret behind a kind whose `max_grant` is set needs a `max-grant` tag no longer than it, or it is refused `proxy-max-grant-above-kind`: before a deployment adds a `max_grant` to a kind, it gives each secret of that kind such a tag (`agent-secrets secret retag NAME --max-grant <duration>`). |
| `broker: profiles applied served=<count> refused=<count> file_digest=<SHA-256 of the file>` | At INFO, once for each broker process, at its first load: how many of the profiles file's entries the broker serves and how many it refuses, each refused one beside it as an `agent secret policy refused` line named `profile:<NAME>`, and the SHA-256 of the file it read (empty when `BROKER_PROFILES_FILE` names none). A pod that wrote it read its profiles; one that did not has not loaded its policy. |
| `broker: answered the github_app field of an earlier agent-secrets at=<request or release> name=<stored secret> profile=<profile served> request=… state=… grant=… enrollment=… kind=… person=… service=… owner=… repositories=[…] read_only=<true or false>` | At INFO, once for each request and each release that answered the `github_app` field of an `agent-secrets` of a release before `--target` ([Earlier clients' github_app field](#earlier-clients-github_app-field)). A release line names the grant and no request or state. When none has been written for long enough that no released client still runs, set `BROKER_GITHUB_APP_FIELD=off`. |
| `agent secret policy no longer refused name=<secret's name in the store> reason=<the reason it had> owner=<owner tag> now=<servable, pending or absent>` | At INFO, when a secret the broker refused is refused no longer: `now=servable` when the broker serves it, `now=pending` when it is a new secret of that name waiting for its value (the line below), and `now=absent` when the store no longer holds it (deleted, or scheduled for deletion). |
| `agent secret policy waiting for a value name=<secret's name in the store> reason=pending-value owner=<owner tag> first_load=<true or false>` | At INFO, once, when the broker first finds a secret that `agent-secrets secret create` made and has not yet filled: it carries the `agent-secrets-create` label the create gives a Google Secret Manager secret it makes before the value, holds no version at all, and the store created it less than ten minutes ago, while the person types or pipes the value. On AWS Secrets Manager the create makes the secret with its value and writes no such tag, so a secret there waits only where another writer gave it a tag of that name and no value. The broker serves it to nobody, a request for it is refused `PENDING_VALUE`, with no request recorded and no approver asked, and a reread answers `reason=pending-value`, but it is no refusal: `broker_unservable_secrets` holds no series for it, and the loads that find it still waiting write nothing. Its value landing serves it and writes no line, as does its leaving the store. Ten minutes after its creation the create that made it gives up and deletes it, or, where the person was still typing or pasting then, deletes it when that entry ends: at Enter, or at a signal that ends the process, such as a hang-up, or Ctrl-C typed outside a paste where it still sends SIGINT. One still there without a value past the ten minutes is refused `no-current-value` (the `agent secret policy refused` line, and a `broker_unservable_secrets` series until the create deletes it). The broker itself deletes nothing: a create that was killed leaves its secret in the store with no version, refused from then on, until its owner frees the name with `gcloud secrets delete <id> --project <project number>` or stores its value; `agent-secrets secret delete NAME` only schedules the deletion 30 days out, and the name stays taken until then. A secret without that label, one holding any version, and one whose listing names no creation time are never waiting: one without a value is refused `no-current-value` at once. |
| `agent secret policy load failed; previous policy kept error=…` | At ERROR, in two cases. The five-minute reload of the namespace failed (the secret store out of reach, or refusing the broker; a Secret Manager failure names its gRPC status code alone, never the status message), and the broker keeps serving the policy from its last good read: the first failed reload of a run writes the line, and the reloads after it write nothing until one succeeds, which writes `agent secret policy load succeeded again`. Or, with `name=<NAME>` (the name a request asked for), the request named a secret the broker did not serve and the broker's reread of that one secret failed, so the request is refused `UNKNOWN_SECRET`: one line for each such request. A read in flight when the broker shuts down, and a request's reread its own caller abandoned, are not one of these and log nothing. |
| `agent secret policy load succeeded again` | At INFO: the first reload that succeeds after the line above began a run of failed ones. |
| `agent secret value read failed; its state kept name=<secret's name in the store> version=<version> error=…` | At WARN: the broker could not read a type secret's stored value to check it ([the value read](#the-value-read)), for a permission, a timeout or the store's own failure, which `error` names by its call and its class and never by a byte of the value. A stored value that is not text is no failed read: it is refused `value-malformed`. The secret keeps the state it had, but for a `value-malformed` refusal of an older version: a failed read of a version the store listed since ends that refusal, which writes `agent secret policy no longer refused`. Each load after it reads the value again and writes nothing more until a read succeeds. |
| `agent secret value read succeeded again name=<secret's name in the store> version=<version>` | At INFO: the first read of that secret's value that succeeds after the line above. |
| `agent secret uses legacy classifier tag; retag it with --type name=<secret's name in the store> old_tag=kind tag=type` | At WARN, once per secret for as long as the broker process runs, the first time a load or a reread reads a secret's type from the old `kind` tag because the secret carries no `type` tag. The broker serves the secret as that type. `agent-secrets secret retag NAME --type <type>` writes `type`, after which the line is not written again for that secret. |
| `broker: answer refused answer_id=<id> status=<status> code=<code> method=<method> route=<route> caller_class=<class> caller=<caller> secrets=[<names>] secrets_count=<n>` | At WARN, once for each refusal of the API port, of the edge and of the airlock's proxy port: every answer from 400 to 499 but a `429` and the two answers a machine gets while it polls a device login, `slow_down` and `authorization_pending`, which are counted and not logged. It is written before the answer is sent, and it names the answer id the refused caller was given ([Refusals and the answer id](#refusals-and-the-answer-id)). It lists the first secret names the request named, up to 32 KiB of names as the line writes them (JSON-escaped, each with its two quotes), counts them all in `secrets_count`, and adds `secrets_omitted=<n>` when it left names out, since Cloud Logging refuses an entry over 256 KiB. An answer Go's HTTP server writes itself before any of the broker's code runs, such as a `400` for a malformed request line, has no answer id and writes no line. The edge's lines add `port=edge`. The edge writes no refusal line, and only counts, for the `404` and `405` its router answers to a path or a method it serves no route for, the `400` its router answers to a request for `*` (such as `GET * HTTP/1.1`), and the refusals its two login routes make before any bucket: a request with no one `X-Forwarded-For` line or with a body that is not a form (`REQUEST_INVALID`), a grant it does not take (`EDGE_GRANT_REFUSED`) and a device authorization naming a person of the Workspace (`EDGE_WORKSPACE_LOGIN`). Anyone on the internet can send those at any rate; `broker_answers_total` still counts each with its `code` and `port="edge"`, and each writes its `broker: edge request` line (below). An alert on this line for the private ports leaves the edge out with `NOT jsonPayload.port="edge"` in Cloud Logging; the API port's and the proxy port's lines carry no `port`. |
| `broker: edge request method=<method> path=<path> protocol=<protocol> status=<status> user_agent=<User-Agent> x_forwarded_for=<X-Forwarded-For>` | At INFO, once for each request the edge answers, once its handler has returned: the edge's access log ([The edge](#the-edge-device-login-from-outside-the-private-network)). Every request writes one, the health check, a `404` or `405` of the edge's router and a refusal before any bucket among them. It names the method, the path without its query string, the protocol, the status the broker answered with, the `User-Agent` and `X-Forwarded-For`, its lines joined by `, `, and nothing else of the request: never the query string, the `Referer`, the body, the `Authorization` header, the DPoP proof or a cookie. The status is the handler's: a client the load balancer's 30-second backend timeout or the server's write timeout cut off first gets an error instead. The method, the path and the `User-Agent` keep their first 256 bytes and `X-Forwarded-For` its last 256, which hold the entries the load balancer appends after what the caller sent. A handler that panics writes it as a `500` with `aborted=true`. An answer Go's HTTP server writes itself before any of the broker's code runs writes none, and the API port and the airlock's proxy port write no such line. |
| `broker: <operation> failed error=…` | A request failed with a 500 or 503; the line carries the cause the response does not. A reread whose own request ended before it finished - its caller went away, or it reached the 45-second bound - is not one of these: it answers `503 REQUEST_ENDED` and logs nothing, since anyone can end a request. One reread whose request ended only while it read the value of a `github-app`, `github-installations`, `google-login`, `google-service-account`, `azure-login` or `anthropic-federation` secret has already put the secret's new tags in force on that pod, so it still tells the other pods to reread the secret, as a `200` does. A held read of a request (`GET /v1/requests/{id}?wait=`) whose caller went away before its wait ran out or in the half second after it, by a Ctrl-C, a timeout of its own or a front door that cut the call, is not one of these either: it answers `503 REQUEST_ENDED`, logs nothing, and is counted `cancelled`, not `failed` ([Metrics](#metrics)). Every other read of a request that ends early is one of these, `read request failed` with a failed `503 DATABASE_UNAVAILABLE`: a read with no wait, which owes its answer at once, and a held read the database stalled until its caller gave up or the 45-second bound ended it. A held read's wait counts from when the request arrived, before its proof is checked, so a caller that goes away while its proof is still being checked leaves inside its wait; one that goes away while the broker looks its enrollment up or records its proof's jti ends that check instead, which is `verify proof failed`, a failed `503 DATABASE_UNAVAILABLE`. |
| `broker: mint failed name=<NAME> type=<type> class=<class> attempts=<attempts> error=…` | At ERROR, for each release of a type secret its minter could not make. `class` says whose fix it is: `stored-value`, the secret's value is not a key or login the type takes, so its owner stores a new one; `binding`, a `google-service-account` or `anthropic-federation` secret the deployment binds to no account, or to an account its stored value does not name, so the operator fixes `BROKER_SERVICE_ACCOUNT_SECRETS` — this one is not the secret's owner to fix by storing a new value, and the mint is refused before any call reaches Google; `refused`, GitHub, Google or Anthropic answered and refused, such as a Google login that needs a new sign-in, a GitHub App with no installation on a listed account, or Anthropic's `401 authentication_error` when no federation rule admits the account's token (its Console's authentication history names why), so its owner signs in again, fixes the App or fixes the rule; `upstream`, GitHub, Google or Anthropic could not be asked, answered no usable reply or rate limited the call, or answered a narrowed GitHub token holding more than the narrowing asked, which the broker revokes at GitHub, so check the broker's egress and the upstream's status. For an `anthropic-federation` mint, a failure to mint the identity token at Google is `upstream`, with Google's own reason in the error, and so is an answer from Anthropic whose token does not start `sk-ant-oat01-` or names no lifetime. For a `github-installations` secret, a token its minter stored with 20 minutes or less left is `upstream`, naming the token's `expires_at`: check the minter, whose next write serves again. `attempts` is how many times the broker sent the call the mint failed at, `1` to `3` ([the mint retry](#the-mint-retry)), and `0` for a failure that was no call's, such as a stored value the minter could not read. An `anthropic-federation` exchange is sent once and never again (`attempts=1`): the identity token it carries may be good for one exchange only, so the next release mints a new one and asks again. A `stored-value` failure also refuses the secret as `value-malformed`, so its grants release nothing until a value that decodes is stored. A narrowing the caller got wrong is never this line: see `broker: narrowing refused`. The error never holds the stored value, a token or a value from GitHub's answer. The session is answered `502 MINT_FAILED` with the same error. |
| `broker: a stored value's trailing line ending was removed name=<NAME>` | At INFO, once for each value of a release the line-ending rule changed, so twice for one release that changed two values: both the value a session is handed and the input a minter reads pass that rule. The line carries the secret's name and no part of any value. The rule removes the trailing run of carriage returns and line feeds only where what is left is not empty and holds no line ending inside it, so a value whose last line ending is part of it is released as it is. |
| `broker: GitHub did not revoke a narrowed token it minted wider than asked; it lapses at its expiry app=… installation=… expires_at=… error=…` | At WARN, beside a `mint failed … class=upstream` line: GitHub answered a narrowed token holding more than asked, the broker refused it, and GitHub did not confirm the revoke the broker then sent. A `401` or `404` to the revoke's first attempt is GitHub refusing it and writes this line, since nothing in the revoke can have revoked a token minted moments before. A `401` or `404` to a retried attempt counts as confirmed and writes no line, since an earlier attempt may have revoked the token and lost its answer; an earlier attempt GitHub rate-limited, which revoked nothing, cannot be told apart from that. No caller got the token; GitHub honours it until `expires_at`, at most an hour. Check the broker's route to GitHub, as for the line it follows. |
| `broker: GitHub did not revoke a token minted for a check; it lapses at its expiry app=… installation=… expires_at=… error=…` | At WARN, during the check the broker makes of a GitHub profile before it opens the approval request: the check mints the token the release would mint and revokes it at once, and GitHub did not confirm that revoke, most often because the check's own time ran out first. Nobody got the token; GitHub honours it until `expires_at`, at most an hour. Check the broker's route to GitHub. |
| `broker: GitHub did not revoke a token it minted whose answer the broker could not use; it lapses at its expiry app=… installation=… error=…` | At WARN, once for each token request whose answer GitHub sent `201`, so it minted the token, and the broker could not use: cut off, larger than the broker reads, undecodable or without an expiry. The broker sent GitHub the revoke of the token the part that arrived names, under a bound of its own (10 seconds) that the caller's own deadline does not end, for a release and for a check alike, and GitHub did not confirm it. The line comes before whatever the mint does next: a release may send the request again and hand over the next token, or fail with a `mint failed … class=upstream` line, and a check is skipped. Nobody got this token; GitHub honours it for at most an hour. Check the broker's route to GitHub. |
| `broker: GitHub minted a token the broker cannot revoke, since the answer it holds does not name it; that token stays live until it lapses at its expiry app=… installation=… error=…` | At WARN, once for each token request GitHub answered `201`, so it minted a token, whose answer the broker holds does not name that token: a whole answer with no `token` member, or the part of a cut-off answer that arrived before the token. Nothing can revoke the token, and GitHub honours it, held by nobody, for at most an hour. Like the line above, it comes before a resend that may succeed, a `mint failed` line or a skipped check. An answer lost before any of it arrived writes no line, since nothing says GitHub minted. A check does not send again a token request that was sent and got no answer the broker could read whole, so it leaves at most one such token; a release sends it again, as for any failed call. A token request GitHub answered with a `5xx`, a `408` or a rate limit names no token, and a check sends it again as a release does ([the mint retry](#the-mint-retry)). |
| `broker: GitHub did not revoke a token a release minted and handed to nobody; it lapses at its expiry app=… installation=… expires_at=… error=…`, or `name=…` in place of `app=…` | At WARN: a release minted a token that it hands to nobody, so it revoked that token, and GitHub did not confirm the revoke. With `app=`, beside a `mint failed` line, the release of several installations failed at a later installation. With `name=`, the secret the token was minted for: the release failed after that name, at a later name of the grant (its value no longer in the store, a split the airlock refused, a stored source the policy no longer serves), at the airlock's placeholders or at its `grant.used` row; or the name is behind the airlock, so the session was handed placeholders and the proxy mints a token of its own, and the token this release minted only to learn the placeholders' slots goes to nobody. Nobody got the token; GitHub honours it until `expires_at`, at most an hour. Check the broker's route to GitHub. |
| `broker: narrowing refused grant=… enrollment=… name=<NAME> error=…` | At WARN, for each release whose GitHub profile names what the installation cannot serve, which the session is answered `422 PROFILE_MINT_REFUSED`: the profile's permissions, cut to what the installation holds, leave none but `metadata` of a profile that asks for more, which is refused before any token request, or GitHub answered the profile's token request 422, such as for a repository the installation does not reach; a 422 that says it is a rate limit is an upstream `mint failed` instead. For a profile of a `github-installations` secret it is the token its minter stored: one holding a permission the profile does not name or a level above the profile's, one holding none of the profile's permissions but `metadata`, or one reaching other repositories than the profile names; the minter's permissions or repositories, or the profile, fixes it, and the other profiles of the secret are served as before. `name` is the profile's name. Nothing was released. The caller's target or the profiles file fixes it, and so, for a cut that leaves none but `metadata`, does granting the App those permissions on the account. The secret is not at fault, so this line needs no action unless one session repeats it. |
| `broker: narrowing refused enrollment=… name=<NAME> error=…`, with no `grant=` key | The same line for a refusal the broker made before the approval request: a request that names a GitHub profile and that a person would decide is checked first, and this is what that check refused. The keys are the enrollment, the profile the refusal is about and the error. There is no `grant=` key because no grant exists: no request row was written, no record was made and nobody was asked to approve anything. The check answers `422 PROFILE_MINT_REFUSED` for three causes: the profile's permissions, cut to what the installation holds, leave none but `metadata` of a profile that asks for more; GitHub answered the profile's token request 422, such as for a repository the installation does not reach; or the App has no installation on the accounts in effect, which a release answers `502 MINT_FAILED` instead, its own answer for every refused mint. The check makes the release's own calls: it reads the App's list of installations, cuts the profile's permissions to what each installation holds, and mints the token the release would mint and revokes it at once. Where GitHub refused a scope of one account and more than one repository, the check asks again with each repository alone, to name the one GitHub refused. The caller fixes its target or the profile, as above. |
| `broker: the narrowing check was skipped enrollment=… name=… reason=… error=…` | At INFO, each time trouble, never a refusal, ended that check. `reason=` is one of ten, and each is the whole reason it ended: `the requester could not be read`; `this broker has no <type> minter` (`github-app` or `github-installations`, the profile's source's type); `this enrollment has made more checks than its bound allows` (five at once, one more every twelve seconds, counting checks of both types, though a `github-installations` check reads the secret store and not GitHub); `the stored value could not be read` (the secret store out of reach, or refusing the broker); `the stored value is not an App id and private key the minter can read`, or, for a `github-installations` secret, `the stored value is not installation tokens the minter can read`, which the secret's owner fixes by storing a new value, as they do for a release's `mint failed … class=stored-value`; `the stored installation token is within the margin of its expiry` (a `github-installations` token with 20 minutes or less left, which the minter's next write mends, so the person is asked); `the check's time ran out, or its caller went away, before GitHub answered` (the check's budget is ten seconds for the whole check and five for one mint, and a caller that goes away ends it too); `GitHub refused the App rather than the narrowing` (a 401 to the App's own token, a 403, or a 404 — the App's key, its clock or its installation, and nothing a caller mends by narrowing differently); or `GitHub's answer decided nothing about the narrowing` (a transport failure, a 5xx, or a rate limit). `error=` is there only where something failed, so the two reasons that need no failure carry none. `name=` is the secret the check had reached, and empty where it had reached none. The request then takes the path it took before this check existed: the person is asked, and the release makes the refusal. Nothing is wrong with the request, so one line needs no action; a run of them says GitHub, the secret store or the bounds are leaving the verdict unreached. |
| `broker: a profile's permission was left out, since the installation does not hold it name=<NAME> account=<account> permission=<entry>` | At WARN, from a release of a GitHub profile, once for each entry that the cut of the profile's permissions to what the installation on that account holds left out: `permission` is the permission alone, such as `issues`, where the installation holds none of it, or the permission and the level the profile asks for, such as `contents:write`, where the installation holds a lower level, which the token holds in its place. The token was released without what the line names. The release's answer names the same entries in `github_left_out`, by profile and then by account, `{"<NAME>": {"<account>": ["<entry>", …]}}`, a field present only where a cut left something out. The App's owner grants the permission on that account, or the profiles file stops asking for it. |
| `broker: grant log …` | At INFO, once for each request decision, each `request.judged` row and each grant end. Every line carries the event's `kind`, its `actor`, whichever of the enrollment, request and grant ids the row names, and `raw`, the audit row's own detail; it carries no audit id and no timestamp, since the line is written once the row has committed and the log keeps its own time. The rest is what the writer of that line had in hand, which differs by line: a `request.created` line adds the session and its person, the service the broker verified, the service the session's credential claims (`claimed_service`), the secrets and how each was decided, the state, the approver, the lifetime, the reason and the detail; a `request.granted` or `request.denied` line adds the session and its person, the claimed service, the state, the approver, who decided, the lifetime, the reason and the detail, and neither the verified service nor the secrets; a `request.cancelled` or `request.expired` line adds the state, the detail and, for a cancel, who decided; a `request.judged` line, written for every judge run that reached a verdict, its hand-overs to a person and its failed checks included, adds the session and its person, the claimed service, and the grant where the verdict granted one; a `grant.revoked` or `grant.withheld` line adds nothing more, except the session's person when a person's revoke ended it. A reader joins a line to its request's own `request.created` line by `request_id` for the rest, or reads that row from `GET /v1/grant-log`, which joins the tables and holds every field. The line is the copy a deployment's log pipeline keeps of the rows that route answers from the `audit` table, so a closed period's decisions can be read without the database ([the audit record](/legion/broker/concepts/#the-audit-record)). A release (`grant.used`) writes the line below instead, and an enrollment row writes none. |
| `broker: a pending request's record is signed for another broker address; it is ended, and its session's new request replaces it request=… record_id=… new_request=… enrollment=…` | At INFO, once for each pending request the broker ended because its record is signed for another broker address: its session asked a broker at another `BROKER_PUBLIC_URL` that shares this database, or this broker before its public URL changed, and then asked this broker again for one of its names, where no person and no judge could decide the request it left waiting. The broker cancelled that request, recording itself as the one who decided it, with a detail that says the broker moved, and wrote the `broker: grant log` line of its `request.cancelled` row beside this one. `request` is the ended request, `record_id` its record, `new_request` the request the session is referred to now (the one its ask made, pending, granted or denied; the pending request of this broker the ask joined or was refused `REQUEST_PENDING` for; or the request of the live grant the ask was answered with), and `enrollment` the enrollment that asked. The line is written once the end has committed, so an ask whose transaction failed writes none; each value is an id, and the line holds no secret value, no token and no request object. It needs no action: the broker cleared a request its session left behind when it moved. |
| `broker: a grant's approval chain does not verify; it releases nothing grant=… record_id=… enrollment=… at=<values, airlock or reuse> reason=…` | At WARN, each time a release finds that a grant's whole approval chain no longer verifies. `at` says where: `values`, the session's own release, answered `403 GRANT_CHAIN_INVALID`; `airlock`, the airlock proxy's read of the values it swaps in, which the proxy answers the same; or `reuse`, the broker's check of a live grant it would hand back to a new request for the same names, which passes that grant over for the next newest and, with none left, asks afresh. `grant` is the grant, `record_id` the record its chain rests on, `enrollment` the enrollment that holds it, and `reason` the refusal's text, which names the link that failed. Most often that is `aud does not match`: the grant's record was signed for another `BROKER_PUBLIC_URL`, by a broker at another address that shares this database or by this broker before its public URL changed, and the grant's session, which asks here now, still holds it. A database failure during the check writes no such line, and the line holds no secret value, no token and no request object. |
| `group members read group=<address> members=<n> result=<result>` | At INFO with `result=ok`, and at WARN otherwise: one line for each read of a group a served secret names, when a five-minute read of the namespace first names the group and once an hour after that ([A group's secret](/legion/broker/concepts/#a-groups-secret)). `members` is how many members the broker holds for the group after the read, since a failed read keeps the last good read's members. `result` names what refused a read: `no directory account` (`BROKER_DIRECTORY_ACCOUNT` is unset), `the token mint for <account> answered <code> …` with IAM's status and reason (`IAM_PERMISSION_DENIED` where the broker's identity holds no Token Creator on the reader account), or `the Directory answered <code> …` with Google's status and reason (where the reader account holds no Groups Reader role, among other causes). A group whose last `result=ok` line is more than 2 hours old admits nobody. |
| `group approval lapsed grant=… record_id=… secret=<NAME>,… decider=<login> group=<address> reason="decider is not a member now" at=<values, airlock or reuse>` | At WARN, each time a release or a reuse refuses a grant that a member of a group approver decided, because the group does not admit that member now. Where the group is stale or was never read, it is a lapsed grant: the session is answered `GRANT_NOT_LIVE`, and no other line is written. Where the group was read in the last 2 hours and does not list the member, the approval chain is broken: `broker: a grant's approval chain does not verify` is written beside this line, and the session is answered `403 GRANT_CHAIN_INVALID`. `at` says where, as on that line. |
| `broker: released a grant grant=… request=… enrollment=… names=[<NAME> …] reason=<reason> call_reason=<reason> command=[<argument> …]` | At INFO, once for each release of a grant, after its `grant.used` audit row is written: what each grant was spent on, and why. `reason` is the reason the grant's request gave. A session's release adds `call_reason`, the `--reason` its own call gave, where that differs from the grant's: a session that asks again for secrets it holds a live grant of gets that grant back, and its new reason is in no request. The broker drops the characters a request's reason may not hold and cuts it to 400 characters. A session's release also adds `command`, the argument vector the `agent-secrets NAME -- <command>` form said the release starts: its first 32 arguments, each cut to 256 bytes. What the cut of a GitHub profile's permissions left out is not in this line: each entry has a line of its own (`broker: a profile's permission was left out …`, above). In the cut command the broker replaces with `[redacted]` each secret it can name: each value of eight bytes or more that the release handed out, each string of eight bytes or more inside a stored JSON value, each token inside a document a minter made, each placeholder of a name the airlock holds back, and every GitHub token, AWS access key id, Google OAuth access token, Slack token of any `xox` prefix and PEM private key, wherever it came from; an argument the cut ended loses any end that starts one of them. **A secret of no such kind — a value from another store, a password typed in, a value shorter than eight bytes — is logged as the session typed it, so pass secrets in the command's environment, as the exec form does, and never on its command line.** `call_reason` and `command` are the session's own word, which the broker records and does not check, and a client from before the fields names neither, which logs an empty `command`. The airlock proxy's release of a grant writes `airlock=true` in their place, since it starts no command. The `grant.used` row's `detail` holds the same fields, and `GET /v1/grant-log` answers that detail to every account with the power to read the grant log. While a client of this version meets a broker from before these fields, that broker refuses each release once with `400 INVALID_NARROWING` naming the unknown field, and the client sends it again without them: the older broker writes a `broker: answer refused … code=INVALID_NARROWING` line and counts `broker_answers_total{outcome="refused",code="INVALID_NARROWING"}` for each, which is expected until the broker is rolled. |
| `broker: an enrollment took a session name from the account paired with its own session=<session> from=<principal> to=<principal> enrollment=<id>` | At WARN, once the enrollment has committed: a pod of an account `BROKER_SESSION_ACCOUNT_PAIRS` pairs with the name's holder enrolled under that name while the holder's claim on it had not run out (90 days past its last lease or token), and took the name. `from` is the holder and `to` the enrolling pod's account, each a principal, `account:` and the service account. Where a login of the same account enrolled both pods, the enrollment also took what the holder's enrollments of that name held: their grants, pending requests and withheld names ([Concepts](/legion/broker/concepts/#requests-and-grants)). The line alone does not say that anything moved: where another daemon's login enrolled the new pod, the line is written and nothing moves, and the old enrollment keeps what it holds, dormant, until `BROKER_SESSION_CARRY_SECONDS` runs out or a pod its own daemon's login enrols takes it. It is expected while a deployment moves its pods to another account; outside such a move it is a pod of one account of a pair taking a session of the other's. |
| `broker: the token exchange took a session name from the account paired with its own session=<session> from=<principal> to=<principal>` | At WARN, once the exchange has committed: the same take, made by a pod's token exchange rather than its enrollment. The pod's next enrollment under the name takes what the holder's enrollments of it hold that a login of the same account enrolled, and nothing where another daemon's login enrols it. |
| `broker: the assignee of a Legion pod's issue could not be read; its request is listed for everyone issue=… enrollment=… error=… bound_reached=…` | At WARN, once for a Legion worker pod's request for a shared secret whose issue's assignee the broker could not read: Dispatch refused the read or answered nothing it could decode, the read ran past its three seconds (`bound_reached=true`), or the broker could not read the pod's own requester first. `issue` is the issue the pod's claim names and `enrollment` the pod's enrollment. The request is listed in every person's Inbox, as when the issue has no assignee, which logs nothing ([Concepts](/legion/broker/concepts/#approvals)). A line on every such request means that step is off: check the token file `BROKER_JUDGE_DISPATCH_TOKEN_FILE` names and that `BROKER_JUDGE_DISPATCH_URL` answers. The error names no token. |
| `an approver's list leaves out a row whose project Dispatch could not be asked about; it is listed again once Dispatch answers list=<pending or grants> record=… grant=… error=…` | At WARN: a reader's own Inbox or Settings list left out a row of a private project, or one whose reason names an issue, because Dispatch did not answer whether the reader may see it (`grant` appears on the grants list only), or because the broker's own read of it panicked (the ERROR line below). The row is listed again once Dispatch answers. Written at most once every 30 seconds for each row, however often the list is read. |
| `requests: a read of whose request this is panicked, and the row it was read for fails closed error=… stack=…` | At ERROR: a read of whether a person may see a request or a grant panicked, a defect in the broker. The read's row fails closed: a list leaves it out, with the WARN line above, or the one below for a request whose responsible person is someone else, and goes on; a decision or a record's page answers as though Dispatch could not be asked. `stack` is the stack of the goroutine that panicked; report the line with it. |
| `an approver's list leaves out a row whose responsible person is someone else, since Dispatch could not be asked whether the row belongs there too; it belongs there only where that person may not see it list=pending record=… responsible=<email> error=…` | At WARN: a pending request whose responsible person is someone else, read to see whether it waits on this list because that person may not see it, could not be read, since Dispatch did not answer. `responsible` names that person. While Dispatch does not answer, a request that person may not see waits on no list, so these lines name the requests to look up. Written at most once every 30 seconds for each request. |
| `an approver's list leaves out, unread, rows whose responsible person is someone else, since its time for reading them ran out; each belongs there only where that person may not see it list=pending reader=<email> rows=<n> bound=2s error=…` | At WARN, once for a list: its two seconds for other people's requests ran out before it read `rows` of them, so it left them out unread. Each belongs on it only where its responsible person may not see it. The reader's own rows are never cut by this clock. Written at most once every 30 seconds for each reader. |
| `an approver's list leaves out every row it holds the person as an approver of, a secret request, a grant or a service's machine login, since Dispatch could not say whether the person is a restricted member; they are listed again once Dispatch answers list=<pending or grants> person=<email> error=…` | At WARN: a person's Inbox or Live grants left out every row it holds them as an approver of, every grant of a session that does not act for them, and every service's machine login an older broker opened, because Dispatch did not answer what kind of member the person is, refused the broker's own account, or answered a kind this broker does not know (`error` says which). A restricted member is no approver of any of them, so the list fails closed for that read alone; the person's own machine logins and own sessions' grants stay on it. Written at most once every 30 seconds for each person, however often the list is read. |
| `broker: a revoke as the grant's approver goes ahead though Dispatch could not say whether the person is a restricted member; a revoke only takes access away person=<email> error=…` | At WARN: a person ended a grant that names them as its approver while Dispatch did not answer what kind of member they are (a timeout, the 2 seconds the broker waits for it included, a failed connection, a gateway's `502`, `503` or `504`, Dispatch's own `DATABASE_UNAVAILABLE` or `OIDC_ISSUER_UNAVAILABLE`, or a `500` that names no code or names `INTERNAL`, which is how Dispatch answers while its database fails). On no answer only the grant's named approver ends it through the broker as an approver: a revoke only takes access away, so it goes ahead. Every revoke through a group or as anyone then, whatever session holds the grant, and every revoke while Dispatch answers what the broker cannot read (a refused account, a kind this broker does not know, a `500` naming another code, another status or code, a body that does not decode, a certificate that does not verify, a name that does not resolve), is refused `503 MEMBER_KIND_UNCHECKABLE` instead and writes no line; so is a revoke whose caller hung up while the broker asked, and one whose caller hung up after it is `503 REQUEST_ENDED`, also with no line. A grant on a person's session is still ended by that person as its operator, asking Dispatch nothing. A grant on a session that acts for no person (a service's pod, a person's pod `BROKER_POD_OPERATORS` no longer maps, or a machine's own session) nobody but its named approver ends through the broker until Dispatch answers; what remains meanwhile is cluster access: delete the pod. A run of these lines for a person Dispatch calls a restricted member means they ended grants that name them, which they would be refused once Dispatch answers. |
| `broker: a machine-login route holds the person to their own logins, since Dispatch could not say whether the person is a restricted member route=<list or revoke> person=<email> error=…` | At WARN, at most once a minute for each person: Dispatch's machine-login page listed, or revoked, a machine login while Dispatch did not answer what kind of member the person is, or answered what the broker cannot read. The broker then holds the person to their own machines' and pods' logins, as it holds a restricted member: they still list and end those, and a service's login is off their list and `404 NOT_FOUND` to their revoke until Dispatch answers. What remains meanwhile for a service's login is cluster access: delete the service's pods, or remove its account from `BROKER_SERVICE_LOGINS` and redeploy the broker. |
| `approval page: Dispatch could not say whether the person may decide secret requests login=<email> error=…` | At WARN: the broker's approval page answered `503`, showing no request, because Dispatch did not say what kind of member the signed-in person is within the 2 seconds the page waits for it. It shows requests again once Dispatch answers. |
| `broker sweeper: ended an enrollment whose lease lapsed enrollment_id=… kind=… runtime_id=… slot=… lease_expired_at=… grants_revoked=… requests_cancelled=… left_dormant=… handed_to=…` | A session stopped renewing (a pod that is gone, a box whose `agent-secrets renew` stopped) while a broker was serving; the sweep ended it, revoking its grants and cancelling its pending requests. `left_dormant=true` says it left them for the session's next enrollment instead, and a non-empty `handed_to` that it handed them to that enrollment, a running pod of the same Legion claim ([Concepts](/legion/broker/concepts/#requests-and-grants)). A lease that ran out while no broker was serving is not ended ([when no broker is serving](/legion/broker/concepts/#when-no-broker-is-serving)). |
| `broker sweeper: end lapsed enrollments sweep=lapsed-enrollments error=…`, and the failure lines of the six other sweeps | At WARN, when one of the seven sweeps the sweeper runs every `BROKER_SWEEP_SECONDS` fails after its run before did not. That sweep logs nothing more until it succeeds, which writes `broker sweeper: a sweep that failed succeeded again`, so a database that stays away for an hour is two lines for each sweep and not one for each run. Each sweep's line has its own message and `sweep`: `broker sweeper: end lapsed enrollments` (`lapsed-enrollments`), `broker sweeper: end dormant enrollments` (`dormant-enrollments`), `broker sweeper: expire or deny pending requests` (`pending-requests`: the expiry of overdue requests and, with `BROKER_SIGNIN=off`, the denial of the rest), `broker sweeper: expire pending machine logins` (`pending-machine-logins`), `broker sweeper: delete spent OAuth state` (`spent-oauth-state`), `broker sweeper: delete expired browser sessions` (`expired-browser-sessions`) and `broker sweeper: delete the placeholders of grants that have ended` (`ended-grant-placeholders`). A sweep the broker's shutdown cuts short logs nothing. |
| `broker sweeper: a sweep that failed succeeded again sweep=<sweep>` | At INFO: the first run of that sweep that succeeds after its failure line. `sweep` names it as that line did. |
| `broker: gave back the time no broker was serving to every lease still running when the last broker served gap=… last_serving_at=… enrollments=…` | At INFO, on the first beat after more than three sweeps in which no broker beat: every broker process was down, or none could reach the database. Each enrollment not revoked whose lease was still running at `last_serving_at` had its lease moved on by `gap`, but no further than `BROKER_LEASE_SECONDS` from now; `enrollments` counts them. Their sessions renew as before. |
| `broker: the serving clock could not beat; it retries every interval and logs again once it beats error=… timed_out=…` | At WARN: this process could not record its beat, usually because the database is out of reach. A beat may take at most one `BROKER_SWEEP_SECONDS`; `timed_out=true` says it ran out of that (its deadline, or a lock or statement past it), as when another transaction holds this process's row, while the other brokers go on beating. It retries every `BROKER_SWEEP_SECONDS` and logs `broker: the serving clock beats again` once it does. |
| `broker reread rate replicas=<n> every=<interval> burst=10` | At INFO, at this pod's first beat of the serving clock and then each time the number of pods it counts changes: the limits it now serves `POST /v1/secrets/{name}/reread` at. `every` is how often the broker-wide bucket takes one reread back, and an address's own bucket takes one back every twice that. A pod that has not beaten yet serves at the two-pod rate ([Running more than one pod](#running-more-than-one-pod)). |
| `broker reread rate above the replica count this sizing supports replicas=<n> every=<interval> store_reads_at_burst=<reads>` | At WARN, beside the line above when a pod counts more than four: the rate is derived the same way, but the burst of 10 is not divided, so a burst at that count reads the secret store `store_reads_at_burst` times at once, which this sizing was not chosen for. A pod logs it once, when its own count first goes above four, and again only if that count changes while it stays above four. A fleet that stays larger has outgrown the budget `limits.go` divides, which is where to change it. |
| `broker: pod enrollment renewed without its projected token enrollment=… pod_uid=… slot=…` | At WARN: a pod enrollment renewed without its token, and no enrollment naming its pod UID has ever renewed with one, so the broker accepted it; for that pod a token copied out of it still keeps an enrollment alive. It is expected from a pod on a worker image whose `agent-secrets renew` predates `--pod-token-file`. A pod on the current image sends its token from its first renewal, so a line naming such a pod means a holder of an enrollment key for it is renewing outside the cluster before the pod's own first token renewal. List pod images below and replace an older-image pod. If its pod UID is absent or every image is current, end the login that made the line's enrollment, which ends every enrollment it made, once its `subject` shows whether it is a Legion daemon's ([the renew token check](#the-renew-token-check) gives the query and both ways to end it); never pick a machine-login row by its machine name. |
| `broker: pod enrollment renewal refused: projected token required enrollment=…` | At WARN: a pod enrollment renewed without its token after an enrollment naming the same pod UID, this one or another, had renewed with one, and was refused. One cause is someone holding its key, possibly enrolled under another machine login with a token copied out of the pod, renewing outside the cluster. The other is an old-image pod refused because someone else marked its UID first: whoever holds an unexpired copy of an old-image pod's token and a launcher credential that enrolls pods can enroll a key of their own on that UID and renew it with the copy, which marks the UID, and from then on the real old-image pod, whose renewer sends no token, is refused here until its lease lapses. That is weaker than what the copy already allowed, which was to enroll and renew as the pod. The refused enrollment's pod UID names the pod: list pod images (below). If that pod runs an older image, the copy's rows are the marked rows of that pod UID: end each one's login once its `subject` shows whether it is a Legion daemon's ([the renew token check](#the-renew-token-check)), never picking a machine-login row by its machine name, and replace the pod with a current image. Otherwise the copy is the line's own enrollment, as that section says. A current-image renewer that cannot read its token file sends nothing at all, so the broker logs nothing for it; `agent-secrets renew` prints `ERROR: enrollment <id> cannot read its pod token` instead, which the worker shim relays into the pod's log. Its lease and grants end one `BROKER_LEASE_SECONDS` after its last valid renewal. |
| `broker: machine session renewal refused: its machine login has ended enrollment=…` | At WARN: a machine's own session asked to renew while the launcher credential its Google identity token minted was no longer live — revoked, past the hour's expiry that token gave it, or its account no longer listed for its service in `BROKER_VM_SERVICES`. The renewal is answered `401 ENROLLMENT_NOT_LIVE` and the session lapses within one `BROKER_LEASE_SECONDS`. It is expected after a fleet is taken out of the settings and after a machine stops trading. A line for a machine that is still running and still listed means something holding that session's key is renewing after its box's login has ended, which is what this refusal is for: see [a copied machine token](#a-copied-machine-token). |
| `broker draining drain=<drain>` | SIGTERM or SIGINT: from here on `GET /healthz` answers `503 SHUTTING_DOWN` while every other route is served and the listener stays open, for `BROKER_DRAIN_SECONDS`. A second signal ends the wait at once, logged as `broker: a second signal ended the drain early`. |
| `broker shutting down` | The drain is over: the broker stops accepting, and the requests still in flight get 60 seconds to finish. That is the server's own `WriteTimeout` — the 45-second request deadline plus the 15 seconds a handler needs to write its answer — because it is the longest exchange the broker allows a request past its headers: one admitted in the drain's last instant still gets its whole deadline. Shutdown closes idle connections at once, and one that has gone five seconds without sending its headers counts as idle, so the budget covers the requests in flight rather than every connection open. |
| `broker: listening for the other pods' secret rereads channel=…` | The reread fan-out's receiving half has its `LISTEN` connection. The boot waits up to five seconds for this line before it binds, so on a healthy pod it comes just before `broker listening`; after that it reappears on every redial. |
| `broker: a Postgres listener has no connection; redialing channels=… error=… attempt=… wait_ms=…` | At WARN, from `pglisten`: this pod has lost its `LISTEN` connection and is redialing with a backoff that reaches thirty seconds. **A pod logging this is outside the fan-out for every name** — losing the `LISTEN` does not by itself fail readiness, so the pod can go on passing `/healthz` and serving the whole time while a retag another pod rereads does not reach it until its own five-minute reload. It often does fail readiness at the same time, since `/healthz` pings Postgres and a database the pod cannot reach is the commonest reason the `LISTEN` went: a 200 beside this line is the case to worry about, not the normal one. It is not the only way a pod falls outside: the three rows below leave one behind for at least one name, and of the four only `the other pods were not told to reread a secret` and `a notification's reread of a secret did not run` can be matched to a secret. Repeating lines mean the database is not carrying `LISTEN` sessions for this pod; one or two around a failover are expected. |
| `broker: binding before the reread listener has had an answer waited=5s` | At WARN: the boot's five-second wait for that `LISTEN` ran out, so this pod began serving before it could hear its peers. It serves normally and the listener keeps redialing; what it loses is the notifications sent in the gap, with its own five-minute reload as the backstop. A pod that logs this takes no catch-up load either, there being no subscription to order a read after. |
| `broker: serving without the boot's catch-up policy load, so a retag another pod served while this one was starting may not be in force here until its five-minute reload error=…` | At WARN: once its `LISTEN` is in place a booting pod reads the namespace a second time, so that a retag a peer served while it was still loading is in force before it binds. That read failed. The pod serves what its first load gave it and catches up at its five-minute reload; the first load succeeded, so this is never a pod with no policy at all. |
| `broker: the other pods were not told to reread a secret secret=… error=…` | At WARN, **not a failed request**: the reread this pod was asked for read the secret's tags and answered a session 200, or `503 REQUEST_ENDED` when its request ended during the value read alone, or ran after the `202` a call with no proof is answered, and only the Postgres `NOTIFY` that tells the other pods failed. The retag is in force here and reaches the others at their own next reload. |
| `broker: a notification's reread of a secret did not run secret=… sender=… error=…` | At WARN, on no request at all: another pod told this one to reread a secret and the reread failed. This pod goes on serving the tags it last read until its own five-minute reload. |
| `broker: a second signal arrived while the broker was still starting up, so it is stopping without a drain: it had not begun serving when that signal was read` | At ERROR: two signals reached a process that had not finished booting, so it exited non-zero without draining. The line says what was true when the second signal was read, which is what the process can observe; a boot that bound its socket in the two statements after that read logs it too. The lines above it say how far the boot got. Expected only when someone or something stopped a pod twice during startup. |
| `broker: signalled while starting up, before it had bound anything stage=…` | One signal reached the process before it bound its socket, so it finished the stage it was in and stopped cleanly, serving nothing and running no drain. `stage` is `the first policy load` or `the catch-up policy load`. |

Each request is bounded to 45 seconds. Request bodies are JSON of at most 1 MiB.

### Refusals and the answer id

Every answer of the API port, of the edge and of the airlock's proxy port carries an answer id, 32
hexadecimal characters, in its `X-Broker-Answer-Id` header, and the edge's refusals write the same
refusal line and counts as the API port's. An answer from 400 to 499 carries it in its body
too: a JSON one as its `answer_id` member, a refusal of the airlock's proxy port at the end of its
text as `(answer <id>)`, and a refusal page of the approval page as an id to quote. A `429` is no
refusal, so two bodies carry no id: the text of a `429` that the Azure token endpoint answered and
the airlock's proxy port forwards, and the approval page's `429` page. The header of each still
carries it. The exception on every port is the answers Go's HTTP server writes itself before any
of the broker's code runs, such as a `400` for a malformed request line, an invalid header name
or value, or a missing, repeated or malformed `Host` header, a `431` for headers over its limit,
a `417` for an `Expect` header it does not know, the `400`
`Client sent an HTTP request to an HTTPS server` on the airlock's bound connections, and the
`200` to `OPTIONS *`. They carry no answer id and write no refusal line. `agent-secrets` ends
each error the broker answers with the id, a refusal or a failure, so whoever was refused has a
thing to quote to whoever runs the broker:

```console
$ agent-secrets NO_SUCH_SECRET -- true
agent-secrets: no agent secret has this name (UNKNOWN_SECRET) (answer ed22ee417e5933b629d7182fc52ce9e6); asked the broker AGENT_SECRETS_URL names (AWS Secrets Manager)
```

For a refusal, the broker writes its line, `broker: answer refused`, before it sends the answer,
so that id finds the line:

```json
{"time":"2026-11-14T02:28:40.768728789Z","severity":"WARNING","message":"broker: answer refused","answer_id":"ed22ee417e5933b629d7182fc52ce9e6","status":400,"code":"UNKNOWN_SECRET","method":"POST","route":"POST /v1/requests","caller_class":"session","caller":"be3624bc-7b88-4e9c-8657-ebbf4b2651d7","secrets":["NO_SUCH_SECRET"],"secrets_count":1}
```

Its fields:

- `answer_id`: the id the answer carries.
- `status`: the answer's HTTP status, from 400 to 499.
- `code`: the code the answer names. The broker reads it from the answer: its JSON body's `code`,
  else the body's OAuth `error`, else the code the approval page or the airlock's proxy port names
  for that refusal, else `HTTP_<status>`, which is what a path no route serves (`404`) and a
  method a route does not take (`405`) are written as. A code is a letter and then at most 63
  letters, digits and underscores, and anything else is written as `HTTP_<status>`.
- `method` and `route`: the request's method, at most its first 256 bytes, and the API route
  that took it, such as `POST /v1/requests`. The route is empty where no route took the request,
  and on the airlock's proxy port.
- `caller_class` and `caller`: how the broker learnt who sent the request, and who that is.
  - `public`: a route that takes no credential, or a path no route serves. `caller` is empty, but
    on the OAuth token route, where it is the OAuth client the token request named.
  - `unauthenticated`: a route that takes a credential, refused before the broker knew who sent
    the request: a missing or bad credential, a listed caller on a route its name does not
    carry, a reread whose proof the broker did not accept, and a refusal of the approval page or
    its sign-in to a browser with no live sign-in. `caller` is empty.
  - `launcher`: a machine's launcher, by the id of its launcher credential.
  - `session`: an agent session or a pod, by its enrollment id.
  - `account`: a caller `BROKER_API_ACCOUNTS` lists, by the name it gives that caller.
  - `service`: a service logged in with its own token, as `<service>@<namespace>/<name>`, the
    ServiceAccount it presented.
  - `edge-listener`: an edge listener, by the account its `BROKER_NATS_SERVICES` entry names.
  - `sign-in`: a person signed in to the approval page, by their login.
  - `airlock`: a call through the airlock's proxy port, as `<grant>@<enrollment>` once its proxy
    password proved a grant, and empty before.
- `secrets`: the secret names the request named, as a list, in the request's order.
  It is empty when the broker refused the request before it read a name.
  It lists the first of them up to 32 KiB of names as the line writes them, JSON-escaped and each
  with its two quotes, each name cut to at most its first 256 bytes, and leaves the rest out, since
  Cloud Logging refuses an entry over 256 KiB.
- `secrets_count`: how many secret names the request named, the ones `secrets` left out included.
- `secrets_omitted`: how many names `secrets` left out, and only when it left any out.
- `correlation_id`: the request's `X-Correlation-Id`, at most 256 bytes of its printable ASCII,
  and only when the request sent one. On the airlock's proxy port a placeholder in it is written
  as `[placeholder]`, as it is in every other field of that port's lines (below).

A refusal of the airlock's proxy port has more fields:

- `host`: the host the call was for, at most its first 256 bytes.
- `reason`: the proxy's own words for the refusal, such as `not allowed by rule`, at most its
  first 256 bytes, since some of them name the host the caller asked for.
- `path`: the request's path, without its query, at most its first 256 bytes.
- `issue`: the issue key the grant's reason named, once a grant is proved.
- `slot`: which of the secret's values the call used, where the line names one.

The proxy port's `method`, `path`, `host`, `reason` and `correlation_id` are the command's own
text, and each has a placeholder in it written as `[placeholder]` (below).

A call the proxy carried, or could not carry, writes `airlock proxy: a call to a bound host` at
INFO, with `answer_id`, `enrollment`, `grant`, `secret`, `issue`, `host`, `method`, `path`
(without its query), `status` and `allowed`; `slot` where the secret has more than one value;
`way`, how its credential went in or would have (`swap`, `mint`, `sign`, `exchange` or `dpop`),
for each call the proxy chose a name for; and `reason` for a call that was not carried. A refused
call to a bound host writes its refusal line in place of that call line, so each such call still
writes one line
([Run the broker locally](/legion/broker/guides/run-locally/#reach-a-secret-behind-the-airlock)
shows both). A `429` that the Azure token endpoint answers and the proxy forwards is no refusal:
it keeps its call line at INFO, which names its answer id and its status, and writes no refusal
line.

Each credential the broker mints for a grant writes `broker: a token was minted for a call` at
INFO: each token a minter makes on a release, one for each slot of its value (so one for each
account of a GitHub App's installations), and each access token the proxy gets at an exchange.
Its fields are `enrollment`, `grant`, `secret`, `way` (`mint` or `exchange`), `type` (the
secret's type), `slot`, and, where the broker knows them, `app` (the GitHub App's id), `account`
(the GitHub account of the slot, or the service account the token was minted as), `repositories`
and `permissions` (what the GitHub mint asked for), and `sha256`, the hex SHA-256 of the
credential. No field holds the credential or any part of it: a scan compares the hash against a
token found somewhere, to tell which grant's mint it came from. A session's own release of a
minted name writes the line too, since it mints to learn the name's slots, so one grant can write
one line at its release and one when the proxy mints for its first call.

A profile whose `proxy` string names the kind its source's `proxy` tag names too writes
`profile's proxy string names its source's proxy kind; drop the string` at WARN, with `profile`,
`source` and `kind`, once at each full policy load and never at a reread. The profile is served
behind that kind; the line stays until the profiles file drops the string, which then changes
nothing a grant was decided under.

Beside those, the proxy writes one line for each socket it opens for a tunnel or a plain-HTTP
request and one for each dial it does not make, which is the whole of what the broker records
about where a command went: a tunnel carries bytes the proxy never reads, and a plain-HTTP
request is forwarded with nothing decided and nothing put into it, so no call line is written for
either.

- `airlock proxy: a tunnel`, at INFO, as soon as the socket is open and before the command is
  answered, so a command that goes away before its `200` still leaves the line. A `CONNECT`
  tunnel writes it, and so does each plain-HTTP request the proxy forwards, an upgrade and an
  absolute `https` URL sent to the proxy as it is among them: the proxy keeps no connection for
  such a request and speaks HTTP/1.1 to its service, even one that offers HTTP/2, so each request
  opens a socket of its own, carries nothing of another caller's, and writes its own line. It
  names the caller — `enrollment`, `grant`, and `session` and `runtime`, the session the grant
  belongs to and the runtime it runs as, which is the enrollment's runtime id — the `issue` the
  grant's reason named, the `host` and `port` asked for, and the `address` the broker opened the
  socket to, as `ip:port`, which is the address its own check read rather than a second lookup's.
- `airlock proxy: a dial that did not happen`, at WARNING, for a dial the proxy refused or could
  not make, on a `CONNECT`, a bound host's request and a plain-HTTP request alike. It names the
  same fields and three more. `resolved` is how many addresses the host resolved to. `failure`
  is `refused` (an address this proxy never dials, a private one or one inside
  `BROKER_AIRLOCK_CLUSTER_RANGES`, or an IPv6 address that carries such an IPv4 address in its
  NAT64, 6to4, Teredo, IPv4-compatible, IPv4-translated or ISATAP form), `unresolved` (a host
  the resolver answered no address or an error for), `unreachable` (every address was dialled
  and none answered), or `cancelled` (the command closed its connection while the proxy looked
  its host up or dialled it, and every attempt was given up for that, so its error says nothing
  about the service; a dial one of whose addresses failed on its own before the command left
  keeps that failure's class).
  The one exception to `refused` is the private destinations
  (`BROKER_PROXY_PRIVATE_UPSTREAM`): an allowed request of a host they name is dialled at that host's
  destination's address only when every address that address resolves to is one this list refuses,
  and never a loopback, link-local, unspecified, multicast or cloud metadata address, or any
  tunnelled or translated address (its NAT64, 6to4, Teredo, IPv4-compatible, IPv4-translated or
  ISATAP form); a tunnel or a plain-HTTP request to those addresses stays `refused`.
  `reason` is the broker's own words. The `address` is the one refused, as `ip:port`, for
  `refused`; empty for `unresolved` and for a lookup that was `cancelled`; and for `unreachable`
  and a dial that was `cancelled`, the addresses dialled, as `ip:port` each, separated by spaces,
  in the order they were dialled, as many whole addresses as fit in the field — `resolved` says
  how many there were in all. A `CONNECT` or a plain-HTTP request the proxy's own check stops —
  a refused address, a host that resolved to nothing, a lookup given up — writes this line
  beside its refusal line, the first saying where the command asked to go and the second how it
  was answered. A bound host's request, and a plain-HTTP request whose check passed, are dialled
  by the proxy's transport, which checks the name again: when that check or that dial fails, the
  line beside this one is `airlock proxy: upstream`, the `502` the command was answered, and no
  refusal line.

The proxy reads the port a command asks for once, as the number it names, and writes it as that
number: a port written with leading zeros is dialled and logged as its number, a URL that names
no port is read with its scheme's (`80`, or `443` for `https`), and a port that is no number from
1 to 65535 is refused `400` before any lookup or dial. The proxy refuses `0` and a number past
16 bits itself: `CONNECT_TARGET_INVALID` on a `CONNECT` and `PROXY_TARGET_INVALID` on a
plain-HTTP request, with a reason that names the host and not the port. It refuses a service name
such as `https`, or a sign, the same way where one reaches it, which is only the Host header a
`CONNECT` with a path for its target is read by: in a `CONNECT`'s own authority or in a
plain-HTTP URL, Go's HTTP server refuses such a port before the proxy reads the request, with a
bare `400 Bad Request` that names no code and writes no line.

Each field whose text the caller chose is cut to its first 256 bytes, so whatever a caller sends,
each line stays within one Cloud Logging entry. On the refusal line those are the method, the
host, a reason that names the host, the path and the correlation id; on the call line, the
method, the host and the path, which is all of the caller's text it names; on a tunnel's line and
a failed dial's, the host, the port, the address and that dial's reason, which names the host.

Many fields of these lines are the command's own text. From its request's headers: a refusal's
`correlation_id`, which is the `X-Correlation-Id` header, and the host of a `CONNECT` whose target
is a path, which is its `Host` header. From the rest of its request: the method, the path, the
host and the port it asked for, the address that host resolved to, and a `reason` or `error` that
quotes them. A host is written as the command sent it, lowercased: a client that sends a Unicode
name as its punycode (`xn--…`) form, as Go's and curl's do, has the punycode written. A
plain-HTTP request whose URL names a Unicode host is the one place the proxy's side
converts it: net/http's transport dials the punycode form, so the line of the transport's dial —
the tunnel's, or a failed dial's when the transport's own check or dial fails — names
`xn--caf-dma.example.net`, while the line of the proxy's own check before it, the refusal line
and `airlock proxy: upstream` name `café.example.net`. A cut to 256 bytes keeps a line small,
and lowercasing a host keeps it one spelling; neither is redaction. What keeps a placeholder off
a line is the broker's mask, which reads each line its JSON handler writes, after the handler has
written it: it decodes each string of the line — each value, each key, each group's name — and
writes again each string whose decoded text holds a placeholder, with each placeholder replaced
by `[placeholder]`, and leaves every other byte as the handler wrote it. A placeholder is
`agent-secrets-placeholder-`, in any case, and the letters and digits after it, at most the 32
the broker mints and up to where another placeholder starts, so two placeholders written
together are two; or a kind's own `placeholder_prefix` and the 32 letters and digits after it,
where the character before the prefix is no ASCII letter or digit, so a word such as `html-`
that only holds a prefix like `tml-` is not one. The broker reads every kind's prefix at boot. A
line of one JSON object that holds no `\u` escape, no byte past ASCII and no
`agent-secrets-placeholder-` and no kind's `placeholder_prefix` in any ASCII case cannot hold a
placeholder, and is handed on as it came: every other escape JSON has decodes to a character no
placeholder holds. So a value is
masked in the one form the line holds it, and a name a command chose
changes no other field of its line, whatever bytes it holds. That holds for every line the
broker writes through slog or Go's `log`
package: every line of the airlock's proxy port, a refusal line and net/http's and httputil's own
`airlock proxy: net/http` lines among them, the API port's lines, the standard logger's, the AWS
SDK's, which the broker writes as `broker: aws sdk` at `WARNING`, grpc's, written as
`broker: grpc` (its errors at `ERROR`; its infos and warnings, which grpc drops by default, at
`DEBUG`, which the broker drops too), and OpenTelemetry's internal errors. grpc's own
`GRPC_GO_LOG_SEVERITY_LEVEL` and `GRPC_GO_LOG_VERBOSITY_LEVEL` therefore change nothing the broker
writes. net/http's transport has no error log of its own: when a service sends bytes after a
whole answer on a connection the proxy keeps, the transport writes them to the standard logger,
which the broker writes as one line for each write, its first line cut to 256 bytes and the rest
kept as `stack`. So no such line holds a placeholder as it was minted or lowercased: a command
that writes its placeholder into its request's path, its `X-Correlation-Id` or the host it asks
for has `[placeholder]` written there. A line the mask cannot read as one JSON object is written
as a fixed line in its place, and none of its text
is written. A value whose methods fail or panic is written as slog writes that failure —
`!ERROR:…` or `!PANIC: …`, with a placeholder in that text masked too; the mask calls no method
of a value and so cannot panic for one, though slog itself can panic for a value whose failure
it cannot print. Two kinds of text are not masked. A library that writes to stderr or stdout
itself writes past it — the Anthropic SDK's warnings, go-retryablehttp's default client, the
pg_query WebAssembly module's stdout and stderr, and the Google client libraries' own logger when
`GOOGLE_SDK_GO_LOGGING_LEVEL` is set and a client was given no logger (the broker's Secret
Manager client is given one that drops every line, since at debug it prints each response, and a
read's response holds the value) — as does the Go runtime's report of a fatal error or a panic no
handler recovered, and anything written before `main` runs. And a placeholder the line holds in
another spelling: a `[]byte`, which the JSON handler writes in base64 wherever it is in a line,
and a placeholder encoded on purpose — its characters percent-escaped, written in fullwidth
letters, a separator written after the prefix, an invisible character written inside it, or its
32 characters after the prefix written alone — is text the mask cannot tell from any other, and
is written as it came; that is out of the mask's scope, since a command that wants its
placeholder sent somewhere can send it through a tunnel, and the mask is for a placeholder
written as it is.

No field of a line is read from a request's body or from a header the swap puts a value in. A
bound service is sent the real value, though, and can send it back, and the token endpoint of an
Azure exchange is sent the real federated token; the placeholder mask can see neither. net/http,
net/textproto and httputil quote each run of bytes they repeat from a peer — an answer's bytes on
an idle connection, a header, trailer or status line they could not read, a client's ALPN list —
so a library's text from the first quotation mark of each of its lines to that line's end is
written as `"[service bytes not shown]"`: in the lines those libraries write (the standard
logger's and `airlock proxy: net/http`), in the `error` of `airlock proxy: upstream` and in the
`502` the proxy answers with it, and in the reason, line and answer of an Azure exchange the
endpoint's answer failed. The whole rest of the line goes, not only the quoted run, since a stray
quotation mark before a run would otherwise pair with the run's opening one and leave the run in
clear. A service's text a library writes without quotes is written as it came, but for the names
a certificate holds: crypto/x509's error for a certificate that names other hosts than the one
asked for lists every name in it, so the `error` of `airlock proxy: upstream`, the `502`, and the
reason, line and answer of an Azure exchange whose token endpoint's certificate names other hosts
write `tls: the service's certificate is not valid for this host; the names it holds are not shown`
in its place. The API port's refusal line takes its `correlation_id` from the caller's
`X-Correlation-Id` too; its line holds no message text, and names the code. Each
answer, a refusal or not, also counts in `broker_answers_total` ([Metrics](#metrics)), which is
the one record of the API port's and the edge's `429` and of a device grant's poll answers.

On the API port and the edge, an answer that no refusal line records names its id in no line: a
`429`, the two answers of a device login's poll, and every answer of 500 and above.
`agent-secrets` prints the id of a `429` and of a failure as well. For a `502 MINT_FAILED`, read
the `broker: mint failed` line instead ([Health and logs](#health-and-logs)). It names the secret
as `name` and holds no answer id, so find it by that name and the time of the answer.

### The value read

The broker reads the stored value of each `github-app`, `github-installations`, `google-login`,
`google-service-account`, `azure-login` and `anthropic-federation` secret once for each version and type of it that a pod lists and has not
checked, at the pod's first load too, and decodes the value as that type's minter does. It drops
the value at once: no value is written, logged, counted or kept. It reads no value of a secret
that has no type this way. Each read counts as `read="value"` in
`broker_policy_store_reads_total` ([Metrics](#metrics)). So a pod reads each such secret once
after it starts, and once more for each new version or type, and never at each reload.

A value the decoder refuses makes the secret unservable as `value-malformed` from that load,
before any mint: one `agent secret policy refused` line and one `broker_unservable_secrets`
series. So does a stored value that is not text, which no minter can read either: an empty value
or one stored as binary alone on Secrets Manager, and a payload that is not valid UTF-8 on Secret
Manager. That is a refusal of the value and no failed read, so it writes no
`agent secret value read failed` line and is not read again at each load. A live grant of the
secret releases nothing (`GRANT_NOT_LIVE`).

The refusal holds while the store lists that version, until a read under a new type
decodes the value. A new version, or a retag of the type, is read and decoded again,
so when `agent-secrets secret set` stores a value that decodes, the reread that follows the write
serves the secret again and writes one `agent secret policy no longer refused` line. A mint whose
minter cannot read the value refuses the secret in the same way, so a pod whose load could not
read the value still refuses the secret at its first mint. That refusal is of the version and
type the release read the value under. When a load or a reread has listed another one since,
a retag's among them, the mint refuses nothing, and the broker decodes the value under the new
one.

Any other read that fails, for a permission, a timeout or the store's own failure, refuses
nothing, and each load reads the value again. The secret keeps its state, but for a
`value-malformed` refusal of an older version: a failed read of a version the store listed since
ends that refusal, so the broker serves the secret again, and a mint whose minter cannot read the
new value refuses it again. The first failure writes
`agent secret value read failed; its state kept`, and the read that succeeds after it writes
`agent secret value read succeeded again`. The read needs no permission the broker lacks, since
a release reads the same value ([What it depends on](#what-it-depends-on)). The `version` these
lines name is the store's own: Secrets Manager's version id, Secret Manager's version number, and
`local-<n>` over `BROKER_FAKE_SECRETS_FILE`.

### The mint retry

A minter's call to GitHub or Google is made again when another attempt can succeed: an answer of
500 or above, a `408` (the upstream timed the request out), a rate limit (a `429`, GitHub's
`X-RateLimit-Remaining: 0`, or a body naming a rate limit), or a call or a read of its answer that
failed while the caller's request was still open. The broker does not retry a `4xx` that is no
`408` and no rate limit, a call whose request its caller ended, an answer larger than the minter
reads, or a `2xx` or `3xx` answer other than the one the call asked for. One more call is not
retried: the GitHub token request that the check of a GitHub profile before the approval request
makes, once it was sent and got no answer, since GitHub may have received it whole and minted a
token that the check will never hold, whatever the broker's own write reported. The same call
that GitHub answered, a `5xx`, a `408` or a rate limit, is retried as any other call is: its
answer names no token, and the check revokes the token a later attempt mints, as it revokes every
token it gets. The same call that failed before a connection, because its name did not resolve,
its connection or a proxy's tunnel could not be made, or the broker did not trust GitHub's
certificate, minted nothing and is retried too. A TLS record error or alert after the handshake
counts as a failure after the request went out; a TLS failure during the handshake comes before
a connection, so the call is retried.
The revokes of a give-back, which return to GitHub a token a release minted and handed to nobody
(after a release that failed, and after every session release of a github-app name behind the
airlock), are made once each and never retried, so they take nothing from the retry budget below.
A revoke GitHub does not confirm is logged, and its token lapses within its hour.

- Each call gets at most 3 attempts, each under a 5-second timeout.
- Before the second attempt the broker waits a time drawn at random from 0 to 2 seconds, and
  before the third, from 0 to 4 seconds.
- A wait the upstream names takes the place of that draw: `Retry-After`, in seconds or as an HTTP
  date, or GitHub's `X-RateLimit-Reset` when `X-RateLimit-Remaining` is `0`. The broker waits it
  in full when it fits in what is left of the mint's 10 seconds (the next item) and of the
  request's deadline, and otherwise ends the mint with no further attempt. A rate limit that names
  no wait counts as 60 seconds, so it ends the mint. A wait longer than an hour counts as an hour,
  the longest GitHub asks for, since GitHub counts its primary rate limits per hour; that ends the
  mint too. A negative wait counts as none.
- One mint waits at most 10 seconds in all, over every call it makes. A GitHub mint makes several:
  a page of installations, a token for each owner, perhaps one more without `actions`, perhaps a
  revoke.
- Every retry of the process takes one from a budget of 60, which gains one back each second. When
  the budget is spent, a call that fails ends its mint at once.

Each attempt counts in `broker_mint_attempts_total` ([Metrics](#metrics)), and a mint that fails
logs `broker: mint failed` with the attempts its last call made. A caller that gives up closes its
request, and that stops the retry at once.

## Metrics

The broker serves its metrics in the Prometheus text format at `GET /metrics`, on a port of its
own: `BROKER_METRICS_LISTEN_ADDR`, by default `127.0.0.1:13382`. The same port serves `GET /livez`,
the NATS auth service's liveness (below). The API's port serves neither, and the metrics port
serves nothing else: every other path answers `404`, and a method other than `GET` or `HEAD`
answers `405`. The boot binds it after the API's port, the airlock's proxy port and the edge and logs
`broker metrics listening addr=<host:port>`, and a pod that cannot bind it does not start. A
draining pod goes on answering scrapes through its drain, and the port closes with the others.

The default address answers on the pod itself alone, so a scraper in another pod needs an address
it can reach, such as `0.0.0.0:13382`. Let that scraper alone reach the port, and never route it
through the address callers reach the broker at: `broker_unservable_secrets` names the store id
and the owner of each secret the broker refuses, which tells whoever reads it whose secrets the
store holds and which of them are broken. No metric holds a value, a token or a name a caller
typed: each code is one the broker chose, and a secret's name reaches a label only from the store's
own listing.

| Metric | Type | Labels | What it holds |
| --- | --- | --- | --- |
| `broker_answers_total` | counter | `outcome`, `code`, `caller_class`, and `port` on the edge's | Each answer the broker's code writes on the API port, on the edge and on the airlock's proxy port. The edge's samples carry `port="edge"`, and the API port's and the proxy port's carry no `port`, so `broker_answers_total{port!="edge"}` reads the private ports alone and an alert on refusals there is not fired by traffic from the internet. The answers Go's HTTP server writes itself before that code runs are not counted ([Refusals and the answer id](#refusals-and-the-answer-id)). `outcome` is `served` (below 400), `refused` (a refusal the `broker: answer refused` line records, and at the edge also the `404` and `405` its router answers, which write no refusal line), `rate_limited` (a `429`, and the device grant's `slow_down`), `pending` (the device grant's `authorization_pending`, a machine's poll while its person has not decided), `cancelled` (a held read of a request, `GET /v1/requests/{id}?wait=`, whose caller went away before its wait ran out or in the half second after it, which writes no line; a read with no wait is never counted here, and one whose caller left while the broker looked its enrollment up or recorded its proof's jti is `failed`, under `caller_class` `unauthenticated`, with the `verify proof failed` line) or `failed` (500 and above, but a `cancelled` one). `code` is a refusal's code, as its line names it, or the code the airlock's proxy port names for a failure of its own, such as `UPSTREAM_UNREACHABLE`. Every other answer's `code` is `HTTP_<status>`. `caller_class` is as the refusal line names it. A call the proxy carried to a service counts as `served`, with the service's own status as its `HTTP_<status>`. |
| `broker_mint_attempts_total` | counter | `type`, `outcome`, `attempt` | Each attempt of each call a minter makes to GitHub or Google ([the mint retry](#the-mint-retry)). `type` is the secret's type. `outcome` is `ok`, `upstream` (an answer of 500 or above, a `408`, no answer, or an answer it could not use), `rate_limited`, `refused` (a `4xx` that is no `408` and no rate limit) or `cancelled` (the caller's request ended). `attempt` is `1`, `2` or `3`. |
| `broker_unservable_secrets` | gauge | `secret`, `reason`, `owner` | One series of value 1 for each secret the broker refuses to serve now. `secret` is the secret's whole name in the store, or `profile:<NAME>` for a refused profile, as the `agent secret policy refused` line names it, `reason` is that line's reason, and `owner` its owner tag, empty for `owner-tag-missing` and `owner-tag-malformed`. A secret that becomes servable, or leaves the store, is gone from the next scrape, and a reread of a name nobody created adds no series. Nor does a secret waiting for its value in the ten minutes after its creation (`agent secret policy waiting for a value`), which is no refusal, nor a source served only through its profiles. |
| `broker_policy_store_reads_total` | counter | `read` | Each call the policy makes to the secret store, a failed one too: `list` for a load of the whole namespace, `describe` for one secret's reread, and `value` for one type secret's value ([the value read](#the-value-read)). It is how to see that this pod read the store: the broker logs no line for a read whose secrets it serves. On Secret Manager each of them spends the project's read quota. A `list` is one `ListSecrets` for each page and one `GetSecretVersion` for each secret, and a `describe` is one `GetSecret` and one `GetSecretVersion`. A `value` is one `GetSecret`, or none when this pod read that secret in the 30 seconds before, and then the `AccessSecretVersion` that reads the value, which counts against Secret Manager's separate access quota. |
| `broker_nats_callout_decisions_total` | counter | `kind`, `outcome`, `reason` | Each NATS connection the auth service answered, where the broker runs it. `kind` is the credential the connection presented: `session`, `service` or `none`. `outcome` is `admitted` or `refused`, and `reason` is a refusal's one fixed word (empty when admitted), such as `waiting_bound` for one refused at once because the replica already held 1024 requests waiting for a decision, or `hold_unknown` for a client other than the operator refused while the NATS writer hold's row cannot be read (`BROKER_NATS_HOLD_READ` true only). |
| `broker_nats_callout_seconds` | histogram | | How long the auth service took to answer one connection, from the request reaching the replica. Its buckets straddle the decision's 1.5-second budget and the two seconds the hub waits for an answer. |
| `broker_nats_callout_dropped_total` | counter | `reason` | Each authorization request the auth service answered not at all; the hub refuses that client at its own timeout. `reason` is `past_bounds` (the replica already held 1024 requests waiting and 1024 being refused), `hub_gave_up` (the hub had stopped waiting for the answer, read from the hub's own clock, so no skew between the broker's clock and the hub's counts here), `unopened` (not sealed to the service's curve key), `not_a_request`, or `pending_limit` (dropped by the NATS client on arrival, past the subscription's pending limits). The count moves every second whether or not the log takes writes. The broker logs the first drop in a minute, at WARN, `natscallout: dropped authorization requests unanswered` with each reason's count since the last such line and each reason's first drop (`<reason>_first`); a replica that stops logs what it counted since its last line. |
| `broker_nats_callout_hold_unknown` | gauge | | 1 while the auth service's last reading of the NATS writer hold's row was unknown: no row, a row no key of `BROKER_NATS_HOLD_SIGNING_KEYS` signed, a gen below the one this replica already read, a phase outside `released`, `prepare` and `held`, or a failed read. Every client but the operator is then refused `hold_unknown`, so it alerts. 0 otherwise, and 0 on a broker whose `BROKER_NATS_HOLD_READ` is false, which reads no row. |

Each pod counts its own answers, attempts and reads, from zero at its start.
[Run the broker locally](/legion/broker/guides/run-locally/#read-the-log-and-the-metrics) shows a
scrape.

## Running more than one pod

Nothing in a broker process is private to it but its cache of the policy and its rate-limit
buckets, so a deployment may run several identical pods behind one address. A sign-in to the
approval page is held in the browser's cookie until its callback and its session in Postgres, so
any pod finishes any sign-in and honours any session. What it has to set,
and what it gets:

**The stop budget its grace period must cover.** On SIGTERM the broker drains for
`BROKER_DRAIN_SECONDS` (5 by default, 0 to 120; a second signal ends the wait at once): it answers
`GET /healthz` with `503 SHUTTING_DOWN` while serving every other route exactly as before, and
keeps its listener open, because a pod is removed from its Service's EndpointSlice concurrently
with the kubelet beginning termination rather than before it, so for a while after the signal new
connections are still routed to it. Only then does it stop accepting and give the requests still
in flight 60 seconds. Those 60 are the server's own `WriteTimeout`, the 45-second request deadline
plus the 15 seconds a handler needs to write its answer, because that is the longest exchange the
broker itself allows a request past its headers: one admitted in the drain's last instant still
gets its whole deadline. A connection that has sent no headers for five seconds is closed as
idle rather than waited for; its header read has its own ten-second `ReadHeaderTimeout`. So `terminationGracePeriodSeconds` must be at least any preStop pause, plus
`BROKER_DRAIN_SECONDS`, plus 60 seconds — 65 seconds with the defaults and no pause. A preStop
pause and the drain do the same job and are both paid out of the one grace period, so size the
period for their sum, or set the pause to 0 and let the drain do it alone. Past the grace period
the kubelet kills the container whatever the broker still believes it is allowed, and the
in-flight requests the budget exists for are dropped anyway. The broker logs the whole figure at
boot, so nobody adds it up by hand: `broker stop budget drain=5s shutdown=60s total=65s`.

**`/healthz` as the readiness probe, and as nothing else.** A liveness probe on it has the kubelet
kill the container in the middle of the drain, which is what the drain exists to prevent
([Health and logs](#health-and-logs)).

**`/livez` on the metrics port as the liveness probe, where the broker runs the NATS auth
service.** It answers `200` while the service's loop has turned in the last ten seconds, and `503`
with a line naming how long ago it last turned once it has not; a broker that runs no auth service
answers `200`. A stalled replica keeps its place in the hub's queue group and is sent requests it
never answers, so the probe must read the status: a probe that only checks the port answers passes
a `503`. Probe it on the metrics port, which a pod must make reachable to the kubelet (the default
address answers on the pod alone), every five seconds with three failures before a restart. Unlike
the `/livez` of `envoy-dispatch` and `envoy-listener`, which answer `204` reading nothing, this one
reads the service's own heartbeat, which a decision that never ends stops.

**One more Postgres connection per pod, on a session-mode path.** The fan-out below listens on a
connection of its own, held for as long as the process runs and outside whatever `pool_max_conns`
sizes, since a `LISTEN` ends when a pooled connection is recycled. Count one extra connection per
pod against the database's connection limit, and give the broker a session-mode path to Postgres:
a transaction pooler (pgbouncer in transaction mode, RDS Proxy) hands each transaction whichever
backend is free and carries no long-lived `LISTEN`. A pod that cannot hold the connection still
boots and still serves; its listener redials for as long as it runs.

**A reread that reaches every pod, draining ones included.** `POST /v1/secrets/{name}/reread`
rereads the secret on the pod the load balancer picked and then, once that pod has put the tags it
read in force (a session's `200` answer, a `503 REQUEST_ENDED` whose request ended during the value
read alone, or the reread that runs after the `202` a call with no proof is answered), tells every
other pod serving the same database to reread the same name, over
Postgres `LISTEN`/`NOTIFY` on a channel of its own. So one call puts a retag in force across the
fleet within seconds rather than at each pod's own next five-minute reload, and it needs no
infrastructure beyond the database the broker already has. A pod that is draining is still one of
those pods: it goes on being sent requests for the whole drain, so its listener and its
five-minute reload both run until the shutdown has answered the last request or spent its 60
seconds, not until the signal. A pod waits for its first successful `LISTEN`
before it binds, up to five seconds, so an address the load balancer can reach is normally an
address a retag can reach; a pod whose `LISTEN` has not succeeded by then logs a warning and
serves anyway, its listener still redialing.
The exception, plainly: while a pod's listener is redialing it hears nothing, and nothing is
caught up when it reconnects, so that pod goes on serving what it last read until its own
five-minute reload. The promise is therefore "within seconds, except while a pod's listener is
redialing, where the next five-minute reload is the backstop". A reread a notification drives
spends no rate-limit token: the pod that served the original call already paid for it.

**The reread limits are three numbers each pod derives from one budget, not one number halved.**
One reread the broker accepts costs the secret store one read on every pod — the pod that served
it, and each peer the fan-out tells — so the fleet's store traffic is `replicas² × the per-pod
rate`, and the two broker-wide buckets' **rate** is the fleet's whole budget divided by that: at
two pods one reread a second, or one every 4 seconds against Google Secret Manager, which leaves
the store exactly the 4 reads a second, and Secret Manager the 120 metadata reads a minute, that
one pod alone was allowed. At three pods it is one every 2.25 seconds, or one every 9; at four,
one every 4 seconds, or one every 16. Each pod does that division itself: its serving clock
counts the pods beating in `broker_serving` at each beat, a count below two counts as two, and a
pod whose count changed logs `broker reread rate` and moves both of its buckets without
emptying or refilling either ([Health and logs](#health-and-logs)).
Their **burst** is not divided at all and stays at ten, because a burst is a
depth and what it admits is one person's write burst, which the replica count does not change,
while what it guards is a transient on each pod's own writer lock — latency, spent once, and
re-bounded by the rate. A burst momentarily lets the fleet accept `replicas × burst` rereads and
each costs a read on every pod, so the store sees `replicas² × burst`: 40, 90 and 160 reads at
two, three and four pods, twice that in metadata reads under Secret Manager (80, 180, 320), which
is 8, 18 and 32 seconds of its 600 a minute — transients, re-bounded immediately by the rate.
That cost grows with the square of the replica count while the rate is already divided by it, so
four pods is the largest count this sizing was chosen for. Above it the rates are derived the
same way and every pod logs a warning naming the count and the store reads a burst then costs,
because a fleet that stays that large wants its budget re-derived against the quota rather than
carrying ten forward unexamined. The **per-address** bucket is derived from them in turn — it
refills at half the rate of the bucket this process mounts, 2 seconds on the default store and 8
under Secret Manager at two pods — so no single address can hold the
fleet's budget at zero on a route that takes no credential. Every other limiter is per process, so
a fleet of N pods allows N times each ([Network and trust](#network-and-trust)).

**A rolling deploy counts one pod more than the Deployment runs.** The pod a surge adds beats
beside the pod it replaces, and a pod that has stopped goes on being counted until its row is
three sweeps old, so a roll of two pods holds the fleet at the three-pod rate for the length of
the roll and can reach the four-pod rate while a stopped pod's row is still live. Nothing has to
be set for that: each pod follows the count it sees and goes back to the two-pod rate within a
few sweeps of the roll finishing. A roll of a four-pod fleet counts five, which is past what this
sizing was chosen for, and each pod logs the warning above once, when its count first goes above
four.

**A roll serves two generations of the broker on one database, so a schema change that one of
them cannot read is staged over two releases.** The per-pod machine login is the current one.
This release's migration adds the two indexes it needs and drops none, so the pods still running
the previous image go on serving every token login and every machine token exchange through the
index they resolve their own statement against; nothing has to be ordered or held, and a roll of
this release is an ordinary roll. The release after it drops that index, and the per-pod login —
a launcher credential for each pod of a ServiceAccount rather than one for the account — begins
there. Each broker reads which of the two schemas it is on at every token login, so an image
from either release serves either schema: a roll forward and a roll back both work, and a pod of
this release goes on serving while the next release's pod migrates the database beneath it.

**Before you roll a broker, read the lease refusal.** A broker whose `BROKER_LEASE_SECONDS` is
too short for its `BROKER_SWEEP_SECONDS` exits at boot instead of serving, and that rule is
stricter than it was: it walks each renewer's own retry schedule rather than counting two thirds
of the lease, so the shortest lease it accepts is 91 s at a sweep of 1 or 5 seconds, 105 s at
10, 225 s at 30 and 360 s at 60.
A pair below that boots a broker of an earlier release and does not boot this one — a 60-second
lease at the default sweep, or anything from 271 s to 359 s at a sixty-second sweep — so raise
`BROKER_LEASE_SECONDS` to at least the figure for your sweep and roll that first.
The refusal prints the figure itself
([when no broker is serving](/legion/broker/concepts/#when-no-broker-is-serving)).

## The renew token check

A pod enrollment is not required to renew with its projected token yet. The broker ratchets each
pod UID instead: every enrollment starts unmarked, its first renewal carrying the pod's token marks
it, and once any enrollment naming a pod UID, live or ended, is marked, a renewal without the token
from every enrollment naming that pod, whichever machine login made it, is refused
`POD_TOKEN_REQUIRED`. A pod on a worker image whose `agent-secrets renew` sends the token
(`--pod-token-file`) marks its UID on its first renewal, so the copied-token gap closes for every
pod UID a current-image pod has renewed under. Until that first renewal a copied token is used
without refusal: seconds, since the Legion daemon logs in with its own token at every start and
enrolls a pod as soon as it is up. A pod that ends before it enrolls never renews with its token,
so its UID stays open as an old-image pod's does. A
pod on an older image never marks its UID: it renews as before, each renewal logged as the first
WARN line above, and the gap stays open for it until the release that requires the token. A pod
UID names one pod for ever, so a mark refuses nothing of another pod, and it keeps refusing a copy
between the pod's role generations and after the pod is gone.

Every deploy order keeps every pod renewing: an older broker ignores the token a newer renewer
sends, and this broker accepts a renewal without one from a pod no enrollment of which has sent
one. That is liveness, not exposure. A grant lasts until its session ends, so a copy renewing an
old-image pod's UID keeps the grant one approval gave it for as long as it renews. So, before or
right after the broker deploys, replace every old-image issue pod: a new worker image does not
replace a running issue pod, so list the pods' images to find them.

```sh
kubectl get pods -n <namespace> -l legion.dev/issue \
  -o custom-columns=POD:.metadata.name,UID:.metadata.uid,IMAGE:.spec.containers[*].image
```

Once that list shows no old-image pod, a token-less WARN line no longer names a pod of yours but a
copy, and for either line the copy is the line's own enrollment: a pod on the current image renews
with its token, so its own rows are the marked ones, and the line names the row that renewed
without it. (Marked rows are a copy's only when the refused pod runs an older image, as the
`projected token required` row above says.) List every enrollment of the line's pod UID with the
machine login that made it; `own` is the line's enrollment, and `subject` is the ServiceAccount a
token login logged in as (none for a login a person approved through a code):

```sql
select o.id = e.id as own, o.id as enrollment, o.pod_token_renewed_at is not null as marked,
       o.revoked_at is null and enrollment_lease_live(o.lease_expires_at) as live,
       c.id as credential_id, c.service, c.subject, c.host,
       to_char(c.created_at at time zone 'UTC', 'YYYY-MM-DD HH24:MI:SS') || ' UTC' as issued_at,
       (select ev.login from credential_request_events ev
        where ev.record_id = r.id and ev.event = 'approved') as approved_by
from enrollments e
join enrollments o on o.kind = 'pod' and o.runtime_id = e.runtime_id
join launcher_credentials c on c.id = o.launcher_credential_id
left join credential_requests r on r.id = c.record_id
where e.id = '<the line''s enrollment>'
order by o.created_at;
```

Whose login the line's enrollment came from is its `subject`. A Legion daemon logs in only with
its own ServiceAccount's token, so its login's `subject` is an account `BROKER_SERVICE_LOGINS`
names for its service; a login whose `subject` is none, or an account the setting does not name,
is never the daemon's. A login with no `subject` is one a person approved through a code: a
service login from before token logins (it lasts at most `BROKER_LAUNCHER_CREDENTIAL_SECONDS`
after its approval), since a person's own machine login enrolls no pod. Revoke it on Dispatch's
machine-login page (`/credentials/machine`), signed in: find the row whose **Credential** is the
query's `credential_id`, check that its **Approved by** is the query's `approved_by` and that its
**Issued** time, in the tooltip, is the query's `issued_at` (the tooltip shows that second in the
browser's time zone), then click **Revoke** and confirm. That ends every enrollment the login made
([end a machine's login](/legion/broker/guides/revoke-a-session/#end-a-machines-login)), and the
broker records the signed-in person as who revoked it. Never pick the row by its machine name: a
login's host is the name its machine reported.

A line whose own enrollment's `subject` is a daemon's account names an enrollment made under that
daemon's one credential: by the daemon itself, renewed by someone holding the enrollment's key in
that pod (the role whose namespace holds it), or by someone holding a token for the daemon's ServiceAccount. Each
login under the account is in the audit table, with the pod the token was bound to; the daemon's
own token is bound to the daemon's own pod, so a login whose `pod_uid` is another pod's, or none
(a token `kubectl create token` minted), did not use the daemon's own token unless you ran the
daemon off the cluster with such a token:

```sql
select at, detail->>'pod_uid' as pod_uid, detail->>'key_thumbprint' as key, detail->>'expires_at' as expires_at,
       detail->>'token_expires_at' as token_expires_at
from audit
where kind = 'launcher_credential.token_login' and detail->>'credential_id' = '<the query''s credential_id>'
order by at;
```

and compare `pod_uid` with the daemon's pod (`kubectl get pods -n <daemon namespace> -l <the
daemon's labels> -o custom-columns=POD:.metadata.name,UID:.metadata.uid`). A login under the
daemon's account moves the credential to its own key, so a copy's login also turns the daemon's
next call into a refusal and a fresh login of its own, logged by the daemon as `agent-secrets: the
token login failed; retrying` or `logged in with the daemon's ServiceAccount token`; a run of those
while nothing restarted the daemon is a copy fighting it. Replacing the line's pod makes the daemon
revoke its enrollment as it lets the process go. If the pod is gone and the enrollment is still
live (the daemon logs `supervise: agent-secrets: enrollment not revoked after every attempt; the
lease ends it`, which a copy that keeps renewing never lets happen), or a copy holds the daemon's
token, end the daemon's login at once: revoke it on Dispatch's machine-login page by its
**Credential** (the query's `credential_id`, **Approved by** reading `No one:` and the daemon's
account), which ends every pod it enrolled, and, while a copy holds the daemon's token, remove its
entry from `BROKER_SERVICE_LOGINS` and redeploy the broker so neither can log in again. Find and
remove what copied the token before you restore the entry: the broker refuses a token that lives
longer than an hour from its issue, so the copy's token ends at most an hour after it was minted,
and the latest `token_expires_at` above among the logins that are not the daemon's says when.
Pods the revoke ended stay without secrets until they are replaced.

To end a login you know by its code instead, such as a person's machine or a service login from
before token logins, find its row by that code:

```sql
select c.id as credential_id, c.service, c.host,
       to_char(c.created_at at time zone 'UTC', 'YYYY-MM-DD HH24:MI:SS') || ' UTC' as issued_at,
       (select ev.login from credential_request_events ev
        where ev.record_id = r.id and ev.event = 'approved') as approved_by,
       c.revoked_at is not null as revoked
from credential_requests r
join launcher_credentials c on c.record_id = r.id
where r.kind = 'launcher_credential' and r.code = '<the code>';
```

and revoke the row whose **Credential** is that `credential_id`, as above.

The release that requires the token from every pod is safe to deploy only when all three of these
hold:

- this query answers 0. A pod's row is unmarked until its first renewal, seconds after it enrolls,
  so the query leaves out rows made in the last minute, which a busy fleet always holds:

  ```sql
  select count(*) from enrollments
  where kind = 'pod' and revoked_at is null and pod_token_renewed_at is null
    and created_at < now() - interval '1 minute';
  ```

- the broker has logged no `broker: pod enrollment renewed without its projected token` line for
  one full `BROKER_LEASE_SECONDS`.
- the command above lists no issue pod on a worker image older than the one whose renewer sends
  the token. An old-image issue pod between role generations holds no enrollment, so it counts as
  0 and logs nothing, yet its next phase enrolls an unmarked row.

That release refuses every pod renewal without a token, so it also removes what only the ratchet
needs: the pod-UID lookup in `Renew` (`podTokenRatcheted`) and its partial index
`enrollments_pod_token_renewed`.

## A copied machine token

A machine's Google identity token logs in as that machine for up to an hour after it was minted,
wherever it is presented. A copy's trades show in the audit record as
`launcher_credential.token_login` rows for that machine's subject from more than one key:

```sql
select at, detail->>'credential_id' as credential_id, detail->>'key_thumbprint' as key,
       detail->>'token_expires_at' as token_expires_at
from audit
where kind = 'launcher_credential.token_login'
  and detail->>'subject' like '<account>%'
order by at;
```

A copy's access token is bound to the copier's own key, so it cannot spend the machine's token, and
its credential and enrollment are its own: a credential is keyed by the key its proof presented,
so a copy's trade mints a credential beside the machine's rather than moving the machine's onto
the copier's key, and ending one ends nothing of the machine's. What it does get is whatever
the fleet's service may read. Revoking the machine's login on Dispatch's machine-login page ends
every enrollment that login made, but not the copied token: within its hour the copy trades again
under a login of its own. To end it for certain, take the machine out of the fleet's settings and
redeploy the broker: change `BROKER_VM_INSTANCES` so the glob no longer matches the machine's name,
or, for the whole fleet, remove the account's entries from `BROKER_VM_SERVICES`,
`BROKER_VM_PROJECTS`, `BROKER_VM_INSTANCES`, `BROKER_VM_EXCHANGE_ACCOUNTS` and
`BROKER_VM_EXCHANGE_JOB_ACCOUNTS`. From then every
trade and every call of that machine, the copy's included, is refused. Find and remove what copied
the token before you put the machine back: the latest `token_expires_at` above says when the last
copied token ends, at most an hour after it was minted. Rotate any secret the copy may have read;
the broker cannot take back a value it released.

## A migration recorded under another file

The broker records each migration it applies in `broker_schema_migrations`: its version, the
name and sha256 of the file the version first shipped with (the file itself, or for one that
declares `-- replaces:` lines, the one its first line names), and the sha256 of the text it
applied. At every start, before it applies anything, it compares each recorded version with the
file of that version in its own image:

- A version recorded with no name, as every one a broker before this check wrote, takes the
  identity the broker records of the file, and the broker logs
  `pgmigrate: recorded the identity of a migration applied before the runners recorded identities`
  for each. It is never refused. The first start of the release that brings the check writes one
  such line for each migration, and refuses none.
- A version recorded under a name and sha256 that is neither the file's own nor one its
  `-- replaces: <name> sha256:<hex>` lines name stops the start.
- A version whose applied text is neither the file's own text nor one its replaces lines name
  stops the start too: the database ran an edit of the file this image does not hold, such as
  another branch's.
- A version no file of the image has is left as it is, as an image rolled back finds them.
- Any other version passes and is left as it is, so a pod of the release that recorded it still
  starts beside this one and after a rollback.

A start that stops applies nothing, logs `broker: fatal` and exits 1. On a database a broker
before this check created, the start first adds the identity columns in a transaction of its own,
and that catalog change stands even when the rows are then refused: the columns are nullable and
the release before ignores them.

The refusal names the version, what the database records and what the image's file is:

```text
migrations refused, none applied: version 32 is recorded as 0032_another_branch.up.sql sha256:<recorded>, and this build's file of version 32 is 0032_oauth_pod_sessions_binding.up.sql sha256:<file>: if this database ran another branch's or an earlier commit's migration, reset it (drop it, create it empty and let the server migrate it); if the edit is intended, add this line to 0032_oauth_pod_sessions_binding.up.sql, after the replaces lines it has (its first replaces line names the identity version 32 first shipped with): -- replaces: 0032_another_branch.up.sql sha256:<recorded>
```

A refusal of the applied text says so instead (`the text applied under it has sha256 …`), and
offers the lines that would accept that text. A lock timeout, or a failed read or write of the
table, reads `read or record what broker_schema_migrations records, none applied: …`, and is no
reason to reset anything.

What you do depends on how the database came to hold another file:

- **It ran another branch's or an earlier commit's migration, or an edit of one** (a development,
  test or preview database): its schema is not the one this image's files make. Reset it: drop
  the database, create it empty, and start the broker, which migrates it from the first file.
  This includes a fresh preview or development database that a newer image created and an older
  image then starts on: the older image refuses it, and you reset it. It includes too a
  development or preview database whose rows recorded no name when a branch's image first started
  on it under this check, where that branch edits a migration in place and its file does not yet
  declare the main branch's identity of it in a replaces line: that start recorded the branch's
  own identity, not the main branch's, and the main branch's image refuses it. A branch whose
  edited file already declares the main branch's identity, as the pre-merge check requires,
  records that identity, and the main branch's image starts on it. Never reset the production
  database this way; a refusal there is a release to stop and investigate.
- **The file was edited or renamed on purpose**, and the database already holds the schema it
  means: add the line the refusal names, alone on a line of its own and outside every quoted
  string or function body, to the file, after the replaces lines it has, and ship it. The next
  start accepts the row as it is. The first replaces line names the identity the version first
  shipped with; a later edit of the same file keeps every replaces line it has, in their order,
  and adds one after them naming the edit before it, its file name and checksum. The line names a
  file of the same version and 64 lower-case hex digits; a line that begins `-- replaces:` and is
  written any other way, or that sits inside a quoted string or a function body, stops every start,
  naming the file and the line. A pull request that edits a migration main already holds without
  such lines, or whose lines do not begin with main's file's lines, in their order, then the one
  naming main's file itself, fails CI's `envoy-go-build` job before it merges.

An image cannot accept a text it has never seen, so it refuses a database whose migration a later
image applied from an edited text: one that an image with the edit created from empty, and a
production database rolled back by hand to an image it never ran, one older than the image that
first applied a migration it holds. Roll back only to an image the database ran, as a Deployment
rollout undo to the revision that was serving does. An image starts on a database a later image
only filled, and an edited text a database ran before this check is not detected: the first start
records its own file.

A start fills the rows that record no name as it judges them. `envoy-broker versions` makes the
same judgment without changing the database, against the files of the image it runs: it reads the
broker's settings, signs in to the database with the broker's login, and reads the version table
in a read-only transaction. Run the image you are about to roll out, before you roll it out, in a
one-off process of that image with the serving broker's settings
([Running a subcommand](#running-a-subcommand)). An exec into a broker pod,
`kubectl -n <namespace> exec <broker pod> -- envoy-broker versions`, judges the image already
serving.

It prints what the next start would do with each version, and exits 1 when a version would be
refused or has no file in that release, or when the table records nothing, which it prints as
`NOTHING CHECKED`: a check of the wrong database must not read as a clean one.

## The audit record

Requests, records, decisions and the audit trail live in the broker's Postgres database
([Concepts](/legion/broker/concepts/#the-audit-record) explains them). To read the latest events:

```sql
select at, kind, actor, enrollment_id, request_id, grant_id, detail
from audit
order by at desc
limit 50;
```

and the decisions on credential requests:

```sql
select cr.id, cr.kind, cr.approver, r.state as request_state, e.event, e.actor, e.at
from credential_requests cr
left join requests r on r.record_id = cr.id
left join credential_request_events e on e.record_id = cr.id
order by cr.created_at desc
limit 50;
```

A secret request's state is its request's (`request_state`), which is what the broker goes by: a
request an older broker cancelled when its session ended carries no event on its record. A machine
login has no request, and its events alone say how it was decided.

A decision the model judge made names `judge:<model id>` in the event's `actor` and in the
request's `decided_by`, with no login beside it, and writes one `request.judged` row of its own.
That row is there for every judge run that reached a verdict, the hand-overs to a person and the
failed checks included, so it is where a request the judge did not take says why
([Concepts](/legion/broker/concepts/#the-model-judge)). Its `verdict` is what became of the
request (`granted`, `refused`, `handed to the person`, `not pending` or `unknown`), and its `calls`
say what each judge key's model call did (`granted`, `denied`, or `failed` with its error), so a
model that timed out reads `failed` beside a request handed to its person, never a deny:

```sql
select at, actor, request_id, detail
from audit
where kind = 'request.judged'
order by at desc
limit 50;
```

No row any of them answers holds a secret value.

## Running it locally

[Run the broker locally](/legion/broker/guides/run-locally/) starts a whole stack (Postgres, the
broker on a fake secrets file, and its clients) with one script.
