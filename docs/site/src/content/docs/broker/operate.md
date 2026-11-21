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

`BROKER_SERVICE_ACCOUNT_SECRETS` binds each `google-service-account` secret to the one Google
service account the broker may mint a token for, whitespace-separated
`<SECRET NAME>=<account email>` entries:

```sh
BROKER_SERVICE_ACCOUNT_SECRETS="GCP_ADMIN=gcp-admin@example-project.iam.gserviceaccount.com DEV_ADA=dev-ada@example-project.iam.gserviceaccount.com"
```

The name is the uppercase name a session asks for, not the secret's store id or its slug; the
account is a user-managed service account, `<account id>@<project id>.iam.gserviceaccount.com` in
lowercase. The broker refuses to start, naming the entry, on a name no agent secret can carry, an
address that is not a user-managed account, a Google-managed service agent (Google's own services
run as those accounts, and the broker must not be able to impersonate one), or a secret named
twice. One account may back two secrets, since only the secret's name is the key.

This setting, and not a secret's stored value, chooses the account: the broker holds
`roles/iam.serviceAccountTokenCreator` on every account bound here, so a mint happens only where
the binding and the stored value name the same account. A secret the setting does not name, and a
stored value naming any other account, release nothing: the session is answered
`502 MINT_FAILED` and the broker logs `class=binding`, before any call reaches Google. Unset, no
secret is bound and every `google-service-account` release is refused, which is where a
deployment stands between this build and the apply that writes its bindings; the broker still
boots and still serves every other type. The policy loader reads the setting as well, for one
rule: a secret bound to a `dev-<name>` account is one person's own, so it may list no registered
service among its `requesters`.

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
`BROKER_EXCHANGE_ACCOUNTS`, `BROKER_EXCHANGE_READ_ACCOUNTS`, `BROKER_EXCHANGE_WORKFLOWS` or
`BROKER_VM_EXCHANGE_ACCOUNTS` fixes. The broker refuses to start, naming the entry, on an entry
not of that form, a namespace's `default` account, an account paired with itself, and a pair given
twice, in either order. Set the pairs, bind the new account in `BROKER_SERVICES` beside the old
one, and, where the pods trade their projected tokens at the exchange, list the new account in
`BROKER_EXCHANGE_ACCOUNTS`, before the deployment moves its pods: a pair added after a role's
enrollment was refused takes effect at that role's next enrollment, and the grants and pending
requests of the old enrollment come across only while the broker still keeps them,
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
account with the pod's, and the pod's account must be one `BROKER_EXCHANGE_ACCOUNTS` lists,
`403 POD_ACCOUNT_NOT_LISTED`. A deployment that does not want that pairs no such account with
itself.

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
account whose `BROKER_EXCHANGE_ACCOUNTS` entry fixes a session: such an entry fixes the session its
tokens name, so its pods need no registration.
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
Set no `exchange_audience` until your daemon, its pods and its broker carry three more changes:
each role trading with a key of its own, since until then a role's own trade is refused; each
session name registered at `POST /v1/oauth/exchange/pod-sessions` bound to the daemon that
registered it, not only each registrar to the pods of its own daemon, since until then any
`BROKER_SERVICE_LOGINS` account can register a pod, its own daemon's among them, for another
claim's session; and the daemon choosing each role's session id rather than taking its agent's
report. A broker of this release carries the second of them, the session-name binding (below). The
daemon already holds each session id to the claim of its project that first recorded it and
registers no session another of its claims holds.

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
alone (above). Today's Legion daemon does not call this route yet, so the exchange honours each of
its registrations until it lapses, unless another caller that logs in as the daemon's account
releases it.

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
- a mapped account that `BROKER_EXCHANGE_ACCOUNTS` maps to another person;
- a mapped account that a `BROKER_POD_REGISTRARS` entry names as its registrar, since the person's
  pod would then choose the session names other pods exchange under (a mapped account on the pod
  side of an entry is not refused);
- a mapped account that a `BROKER_EXCHANGE_READ_ACCOUNTS` entry names, since the exchange would
  then mint the person's pod a read-only token that names a service and no person;
- a mapped account that a `BROKER_API_ACCOUNTS` entry names, since that caller's routes relay
  approvals in the name the caller gives;
- a mapped account that a `BROKER_NATS_SERVICES` entry names, since the NATS auth service would
  then answer a connection of the person's pod with that service's NATS grant;
- a mapped account that either side of a `BROKER_SESSION_ACCOUNT_PAIRS` pair names, since a pair
  lets each account take the session names the other holds.

A mapped account on the pod side of a `BROKER_POD_REGISTRARS` entry is not refused, so a daemon may
register a person's pod under a session name. The registration's body carries that pod's token, and
unless `BROKER_POD_REGISTRATION_AUDIENCE` is set it is a token of the broker's own audience, the one
the person's pod logs in with, so the daemon then holds a token that acts as the person. For such an
entry, set `BROKER_POD_REGISTRATION_AUDIENCE` and hand the daemon the pod's token of that audience,
which no other route accepts.

The pod enrols itself with `agent-secrets enroll --pod --service <its login service>`, which names
no session. Its login, made with its own projected token, carries the person, and so does its
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
  that person's approval, and a shared agent-tier secret is still granted;
- the pod's requests are its own enrollment's alone. The pod no longer reads or cancels a request
  of the person's box or of another session of the person, and those sessions no longer read or
  cancel the pod's requests: each is refused `403 NOT_YOURS`, and the request stays pending. The
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
  it expires, its approver revokes it, the policy refuses it to the account, or the enrollment ends;
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
- the grant log (`GET /v1/grant-log`), the first person's **Live grants** (`GET /v1/grants`) and
  the record of each request's approval keep naming the first person, since they read the
  enrollment's record. That person still sees the pod's grants and may revoke them. Each new
  request's `request.created` row records, in its own detail, the person the broker decided for:
  none.

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
tokens write as one person: in the one Dispatch project the entry names, or, for an entry that names
no project (`<service account>==<person>`), wherever Dispatch's own rules let that person write.
`BROKER_EXCHANGE_READ_ACCOUNTS` lists the accounts whose tokens only read, as whitespace-separated
`<service account>=<service>=<session>` entries, for example
`BROKER_EXCHANGE_READ_ACCOUNTS=system:serviceaccount:example-ns:example-reader=example-reader=example-reader-session`.
The token of such an entry holds the OAuth scope `read` alone and the project scope `none`, names
that service and that session, and names no person and no project. Dispatch serves it on the routes
its read list names, today `GET /api/v1/projects/{key}/architecture` alone, and answers it
`403 READ_ONLY_TOKEN` on every other route. That token reads the whole answer of
`GET /api/v1/projects/{key}/architecture` for every project that has an architecture source: each
source, each component, and every issue of the project, closed and iceboxed ones included. No
setting holds a read entry to some projects. The Envoy listener answers it `401`. Both settings need
`BROKER_K8S_OIDC_ISSUER` and `BROKER_EXCHANGE_K8S_AUDIENCE`, and the broker refuses to start on a
read entry without the audience. It also refuses an entry with a missing field, a namespace's
`default` account, a service that is not lowercase letters, digits and hyphens of at most 64 or is
`shared`, and an account that one setting lists twice or that both settings list. A broker of a
build older than this setting does not read it, so it mints those accounts no token. The boot line
`token exchange state=on` counts the read entries as `read_accounts`.

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
  `BROKER_API_ACCOUNTS` or a VM service in `BROKER_VM_SERVICES`.
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
`agent-secrets` and the helper hold theirs by doing nothing. A person's sign-in cannot do
otherwise: a sign-in holds its credential, so a trade of a person's that declined one is refused
`400 invalid_request`. The clients that
want a Dispatch token and read no secret — the `dispatch` command, the Oh My Pi and Claude Code
plugins, `envoy-send` — decline it, since each of them makes a key per process and per session
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
registered under it; or `pool`, a token
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
approver named in it as `login_hint`.

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
  endpoint to redeem an invalid code with the broker's client credentials. `ready` is the provider
  accepting the client, `client_rejected` is the provider refusing it (nobody can sign in: the
  client id, its secret or its registration is wrong), and `issuer_unreachable` is a provider that
  could not be asked, or answered `429` or a `5xx` whatever OAuth error its body named, which says
  nothing about the client. A check neither answers a person nor stops the broker: it writes this
  row, and it logs only a change. When it finds `client_rejected` or `issuer_unreachable` and its
  pod did not log that state last, it logs `broker: the sign-in client check`. When it finds
  `ready` after such a line, it logs `broker: the sign-in client check finds the client ready
  again` ([Health and logs](#health-and-logs)). So a provider that stays away for a day is one
  line, not one each hour.
- `last_sign_in_at`: when a person last finished signing in. Long silence on a deployment people
  use is the signal, read beside `last_decision_at`.
- `last_failed_sign_in_at` and `last_failed_sign_in_reason`: when a sign-in last failed at the code
  exchange or at the ID-token check, and why, as one of `invalid_client`, `unauthorized_client`,
  `redirect_uri_mismatch`, `invalid_grant`, `issuer_unreachable`, `bad_issuer`, `bad_audience`,
  `bad_signature`, `bad_nonce`, `not_in_domain` and `email_not_verified`. The first four are the
  token endpoint's own error, written only from its `400` or `401` answer: a code exchange it
  answers `429` or `5xx`, whatever error its body names, or does not answer, is
  `issuer_unreachable`, as is a discovery document that could not be read. The first three are
  the client's registration, `not_in_domain` and `email_not_verified` are the person rather than
  the deployment, and a failure none of the eleven names leaves the row alone and is logged
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
another file](#a-migration-recorded-under-another-file)) and `airlock-check`; anything else is
refused with a usage line, and `envoy-broker` with no argument serves. `end-sessions` and
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
| The secret store | The agent secrets: every secret whose name starts with `BROKER_SECRETS_PREFIX`, carrying `owner` and `tier` (AWS tags, or Secret Manager annotations), encrypted with the agent-secrets key, and holding a current value. A `type` tag makes the broker mint a release from that value. [Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret) describes the tags, the name each secret is asked for by, and how often the broker rereads them. `BROKER_SECRETS_STORE` names which store, and the two rows below say what each needs. |
| AWS Secrets Manager (`aws-secrets-manager`) | The broker calls AWS with the SDK's default credential chain (environment, shared config, or the workload's role), in a region (`AWS_REGION`, `AWS_DEFAULT_REGION` or the shared config). It needs `secretsmanager:ListSecrets` (which takes no resource, so on `*`), `secretsmanager:DescribeSecret` (to reread one secret) and `secretsmanager:GetSecretValue` on the namespace's secrets, `kms:Decrypt` on the agent-secrets key (`BROKER_SECRETS_KMS_KEY_ARN`), `kms:ListAliases` (on `*`, called only when a secret names its key by an alias), and, when it signs in to RDS by IAM token, `rds-db:connect` on its database user; nothing else. |
| Google Cloud Secret Manager (`gcp-secret-manager`) | The broker calls Secret Manager under [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials): on GKE, Workload Identity Federation for the pod's Kubernetes ServiceAccount, with no key file and no Google service account; the boot log's `secret store … credentials=application-default` line says so. Its principal needs `secretmanager.secrets.list` on the project (the listing takes the project as its parent) and `secretmanager.secrets.get`, `secretmanager.versions.get` and `secretmanager.versions.access` on the namespace's secrets: `roles/secretmanager.viewer` on the project and `roles/secretmanager.secretAccessor` with an IAM condition `resource.name.startsWith("projects/<number>/secrets/<id prefix>")` cover them. The condition narrows only the accessor binding, so the broker reads values from the namespace alone; the viewer role reaches every secret's metadata in the project (its annotations, its versions' state and its IAM policy), never a value. A secret is served while its `versions/latest` is `ENABLED`, encrypted with the key `BROKER_SECRETS_KMS_KEY_NAME` names (`replication.automatic.customerManagedEncryption`; a secret on Google's default encryption is refused `not-on-agent-secrets-key`); one with an `expire_time` is scheduled for deletion and left out, as a Secrets Manager secret in its recovery window is, and a grant made before it no longer reads its value, though Secret Manager still would until that time. Every Secret Manager call but reading a value counts against the project's read quota, [600 a minute, shared by everything in the project](https://docs.cloud.google.com/secret-manager/quotas) (reading a value has its own 90,000). The broker's worst case against it is the sum of: a reload every five minutes, one listing plus one call per secret (a 200-secret namespace, about 40 a minute); the public reread route, two each, bounded across all callers of **each process** at one reread every `N² × 1 second`, where `N` is the pods that process counted at its last beat and never fewer than two — one every 4 seconds at two pods, one every 9 at three, one every 16 at four — which holds the whole fleet at the same 120 metadata reads a minute whatever `N` is (see [Running more than one pod](#running-more-than-one-pod)); each session's rereads for names it was not served, two each, at most one per session every ten seconds; and value releases, a type secret's among them, one per distinct secret per 30 seconds (two for a secret whose latest version is disabled), since the broker trusts its last check of a secret, readable or not, for that long (so a delete made outside `agent-secrets secret` stops releases at most 30 seconds after Secret Manager's own metadata shows it, which Google documents as eventually consistent; `agent-secrets secret delete` rereads the name, and the broker then releases nothing more). A busy fleet of sessions requesting unknown names can still spend the quota, and other workloads in the same project draw on it too, so give the agent secrets a project of their own. On the metadata calls the broker retries an `UNAVAILABLE` answer and never a `RESOURCE_EXHAUSTED` one, which would only spend more of that quota; reading a value keeps the client library's own retry of both, against its separate quota. On Secret Manager the agent-secrets key gates no caller: Secret Manager's service agent makes the KMS calls, so writing a secret onto the key and reading it need no KMS permission, and being on the key is the namespace's convention, not a second permission check as it is on AWS. Secret Manager makes the KMS calls itself, so the broker needs no KMS permission; the Secret Manager service agent needs `roles/cloudkms.cryptoKeyEncrypterDecrypter` on the key. |
| Type secrets | A `github-app` secret needs outbound HTTPS access to `api.github.com`, and a `github-owners` tag naming the GitHub accounts it mints for, which the deployment sets when it creates the secret. A `google-login` secret needs outbound HTTPS access to `oauth2.googleapis.com`. A `google-service-account` secret needs outbound HTTPS access to `iamcredentials.googleapis.com`, an entry in `BROKER_SERVICE_ACCOUNT_SECRETS` binding it to one account, and the broker's own Google identity holding `roles/iam.serviceAccountTokenCreator` on each bound account and on no other, since the broker mints the token by impersonating that account; without the role Google refuses the mint and the session is answered `502 MINT_FAILED`. **The deployment's binding, not the secret's stored value, chooses which account the broker impersonates.** A secret bound to no account, and a stored value naming an account other than the bound one, release nothing and are logged `class=binding`, so a secret someone writes with another person's account in it mints no token, and a secret the deployment never bound mints none either. What the secret's own tags still decide is who may ask for it (`requesters`) and what the token can do (`scopes`), so the write access to a bound secret is an administrator's ([People who manage secrets](#people-who-manage-secrets)). The broker reads the stored value for each release, as it reads any secret, and returns only the minted token. It also reads each type secret's value once for each version and type a pod lists, to check it, and refuses a secret whose value its type's minter could not mint from ([the value read](#the-value-read)). It needs no IAM permission to mint a GitHub App or a Google login value, and `BROKER_SERVICE_ACCOUNT_SECRETS` is the one `BROKER_*` setting any of the three types needs. A mint GitHub or Google refuses answers the session `502 MINT_FAILED`, naming their reason, and logs `broker: mint failed` (below). A call to GitHub or Google that answered a 5xx or a rate limit, or got no answer, is attempted up to three times before that ([the mint retry](#the-mint-retry)). A narrowing the secret cannot serve, an account its tag does not list or one GitHub refuses, answers `403 NARROWING_OWNER_NOT_LISTED` or `422 NARROWING_REFUSED` instead and logs `broker: narrowing refused`. |
| A local stand-in (development only) | `BROKER_FAKE_SECRETS_FILE` names a JSON file the broker reads in place of the store, in Secrets Manager's shape whatever `BROKER_SECRETS_STORE` says, each secret's `kms_key_id` naming the agent-secrets key as the selected store configures it (`BROKER_SECRETS_KMS_KEY_ARN`, or `BROKER_SECRETS_KMS_KEY_NAME` with the GCP store); the [configuration reference](/legion/broker/reference/config/#variables-the-broker-reads) gives its format. |
| Dispatch | Dispatch's server calls the broker's account routes as its own ServiceAccount: it lists the requests and machine logins waiting on the signed-in person, each secret request with its page on the broker (`approve_url`), which its Inbox links to, and relays a person's machine-login decisions and revokes. Set Dispatch's `DISPATCH_AGENT_SECRETS_URL` to the broker's URL and `DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE` to the path of its projected token for `BROKER_K8S_OIDC_AUDIENCE`, and list that account in the broker's `BROKER_API_ACCOUNTS`. Without the URL, Dispatch hides its credential pages. Deploy a broker with the approval page before a Dispatch whose Inbox links it ([Approval sign-in](#approval-sign-in)). |
| The sign-in provider | A person's browser signs in to the broker at the provider (Google, under the Workspace's internal consent screen), and the broker reaches its token endpoint and keys over HTTPS at a sign-in, and its discovery document at the first sign-in each process serves (kept once a read succeeds) and at the client check it takes at startup and every hour after. A discovery that could not be read, or a code exchange the token endpoint answers `429` or `5xx` or does not answer, answers the page `503` naming the issuer and logs `approval sign-in: reach the provider`; machines are served as before, and `GET /signin/status` reports `issuer_unreachable` ([What the sign-in is doing](#what-the-sign-in-is-doing)). |
| Kubernetes (optional) | To enroll pods, and to let a service log in with its token (`BROKER_SERVICE_LOGINS`), `BROKER_K8S_OIDC_ISSUER` and `BROKER_K8S_OIDC_AUDIENCE` name the cluster's service-account token issuer and the audience the pods' and the services' projected tokens carry. The broker fetches the issuer's discovery document at startup and refuses to start if it cannot. |
| Google Cloud (optional) | To let machines trade their Google identity tokens, `BROKER_GOOGLE_OIDC_AUDIENCE` names the audience of those tokens, and `BROKER_VM_SERVICES`, `BROKER_VM_PROJECTS`, `BROKER_VM_INSTANCES` and `BROKER_VM_EXCHANGE_ACCOUNTS` describe each fleet. The broker fetches Google's discovery document at startup and refuses to start if it cannot. |
| Envoy (optional) | With `BROKER_ENVOY_URL`, the broker tells a waiting agent session, best effort, when its pending request expires. It calls the listener with its own projected ServiceAccount token, which `BROKER_ENVOY_ACCOUNT_TOKEN_FILE` names and which it reads for each notice, so the listener must admit that account. The shared bearer `BROKER_ENVOY_TOKEN` is removed, and a broker started with it refuses to start. |
| The Envoy hub's NATS auth service (optional) | With `BROKER_NATS_URLS`, the broker answers the authorization of each connection the Envoy hub sends it: a session presenting its broker access token and a proof of its key gets its own delivery channel, and a service presenting a platform token for `BROKER_NATS_K8S_AUDIENCE` or `BROKER_NATS_GOOGLE_AUDIENCE`, from an account `BROKER_NATS_SERVICES` lists, gets its kind's grant. It connects to the hub as the user `BROKER_NATS_SEED_FILE` holds, signs with the account seed in `BROKER_NATS_ISSUER_SEED_FILE` and opens each request with the curve seed in `BROKER_NATS_XKEY_SEED_FILE`; each file is refused at startup unless it holds a seed of its kind. Every replica answers in one queue group, so a stopped replica delays no connection. With none of these set the broker runs no auth service. |
| The model judge (optional) | Outbound HTTPS access to the model endpoint `BROKER_JUDGE_MODEL_URL` names, which the broker reaches with its machine's own Google identity token for `BROKER_JUDGE_MODEL_AUDIENCE`, and to the Dispatch `BROKER_JUDGE_DISPATCH_URL` names, which it reads with the projected ServiceAccount token in the file `BROKER_JUDGE_DISPATCH_TOKEN_FILE` names, read at call time and never at boot, so a rotation needs no restart. It reads one issue, that issue's spec document and the ask the reason cites there for the judge, and one issue's assignee for a Legion worker pod's request for a shared secret, and writes nothing. With those settings unset the broker makes neither call and runs no judge ([Concepts](/legion/broker/concepts/#the-model-judge)). With `BROKER_JUDGE_MODEL_ACCOUNT` set, the token it presents to the model is that service account's, which it mints at IAM Service Account Credentials' `generateIdToken`, so it also needs outbound HTTPS to `iamcredentials.googleapis.com` on 443, and a read of the metadata server's token path (`/computeMetadata/v1/instance/service-accounts/default/token`, plain HTTP on the node's own link) for the access token the mint spends; the broker's principal holds `roles/iam.serviceAccountOpenIdTokenCreator` on the account; and the account's project has the IAM Service Account Credentials API turned on. A mint that fails hands the request to its person, and the broker never falls back to the metadata server's identity token. |
| Dispatch's project access (optional) | To let a token name the projects its person may see, `BROKER_PROJECT_ACCESS_URL` names the Dispatch the token exchange asks whether a person may see a project and what kind of member a person is, through Dispatch's `GET /api/v1/access/projects/{key}` and `GET /api/v1/access/person` routes, and `BROKER_PROJECT_ACCESS_TOKEN_FILE` names the broker's own projected ServiceAccount token for Dispatch's audience, read at call time and never at boot. No Dispatch release serves those routes yet: leave both unset until the broker can reach one that does, since against one that does not, every trade naming a project is refused `temporarily_unavailable`. Dispatch must list that account as one that may ask: until it does, Dispatch answers 403 and every trade that asks it is refused `server_error`, saying Dispatch refused the broker's account; so is every such trade while the broker cannot read the token file, and while Dispatch answers a member kind the broker does not know. The two are set together or not at all. Answers are kept 30 seconds, and the questions are asked before the exchange's database transaction begins. While Dispatch cannot be asked, a token naming no project is minted as before, and each such mint logs a warning naming the account or person and the session; every trade naming a project, an operator entry's included, is refused `temporarily_unavailable`. With neither set the broker asks Dispatch nothing: a token naming no project and an operator entry's token are minted as before, and a person's trade naming a project is refused. The boot log's `project access check state=on` or `state=off` line says which. |

## Network and trust

- Agents, their helpers and launchers, and Dispatch's server reach the broker at
  `BROKER_PUBLIC_URL`. Every signed proof names that URL plus the request's path, so clients'
  `AGENT_SECRETS_URL` must be exactly it, and a proxy in front of the broker must not change the
  path. The broker checks each proof against `BROKER_PUBLIC_URL`, never against the `Host`,
  `X-Forwarded-Proto` or `X-Forwarded-Host` a request arrives with: a proxy may rewrite `Host` or
  terminate TLS without setting `X-Forwarded-Proto`, and a client that sends those headers itself
  changes nothing. That holds for the machine login's token endpoint (`POST /v1/oauth/token`)
  too, whose DPoP proofs name `BROKER_PUBLIC_URL/v1/oauth/token`.
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
- Behind a reverse proxy, set `BROKER_TRUSTED_PROXY_HEADER` so the machine-login, token-endpoint
  and secret-reread rate limiters see each caller's address rather than the proxy's. The token
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
  machine could register a session under it. When the airlock rules give the model judge
  seconds of any secret it also answers `judge_keys`: each such name with its judge seconds and its
  `guidance` — the rules file's own words, never a value. It is the rules' answer, the same on a
  broker that runs no judge, and `agent-secrets secret show` reads it to say who decides a request
  for a secret and what the judge looks for. An administrator's secret is listed there when the
  rules give it seconds, and its person decides it all the same; `secret show` reads its tag and
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
    serves the secret, as which type (`type`: `github-app`, `google-login`, `azure-login` or
    `google-service-account`, absent for a stored secret) and, if not, why (`absent`, or a
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

One type is not a person's to write. A `google-service-account` secret the deployment binds to an
account (`BROKER_SERVICE_ACCOUNT_SECRETS`) releases a token that acts as that account, and the
secret's own tags say who may ask for it and which scopes its token carries, so whoever may write
those tags decides who can get a token as that account. A deployment that binds names adds one
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

A `google-service-account` secret under a name the deployment binds to nothing mints nothing, so
a person who creates one of their own under such a name, with any account in its value, is
answered `MINT_FAILED` rather than a token. The `requesters` tag bounds who may *ask* for a
secret; the binding is what bounds which account it mints for.

### AWS Secrets Manager

| Form | Permissions | What a condition can match |
| --- | --- | --- |
| every form | `sts:GetCallerIdentity`, which needs no permission; a sign-in with a profile's `role_arn` needs `sts:AssumeRole` on that role, and one with a web identity token file the role's trust of it (`sts:AssumeRoleWithWebIdentity`) | |
| `list` | `secretsmanager:ListSecrets` (on `*`: it takes no resource) | |
| `show` | `secretsmanager:DescribeSecret` | The secret's own tags (`aws:ResourceTag/owner`, `aws:ResourceTag/tier`) |
| `create` | `secretsmanager:CreateSecret` and `secretsmanager:TagResource`, since it tags the secret as it creates it; `kms:GenerateDataKey` and `kms:Decrypt` on the agent-secrets key | The new secret's name under the namespace (its ARN, and `secretsmanager:Name`); the request tags: `aws:RequestTag/owner` (a person's email, from `--owner me` or `--owner <email>`, or `shared`) and `aws:RequestTag/tier` always; `aws:RequestTag/type` and `aws:RequestTag/mint-service-account` (the rollback guard, `type-secret`, written beside every type) with `--type`; `aws:RequestTag/github-owners` (GitHub logins separated by spaces) with `--type github-app`, which needs it; `aws:RequestTag/requesters` (emails and service names separated by spaces) with `--requesters`; `aws:RequestTag/scopes` (OAuth scope names separated by spaces) with `--scopes`; `aws:RequestTag/admin` (`true`) with `--admin`; `aws:RequestTag/max-grant` with `--max-grant`; and `aws:RequestTag/default-grant` with `--default-grant` (`aws:TagKeys` is the tags sent, so a policy that limits it must allow `owner`, `tier`, `type`, `mint-service-account`, `github-owners`, `requesters`, `scopes`, `admin`, `max-grant` and `default-grant`, the whole set a create carries, to which a retag's untag adds `kind` (`retag` below); unproved: no check runs this policy); the key, named by its ARN (`secretsmanager:KmsKeyArn`); the `TagResource` check made on the new secret sees the requested tags as its own (`aws:ResourceTag/owner`, `aws:ResourceTag/tier`), so a policy that reserves a shared secret's tags for administrators still needs a statement that admits a shared create (request and resource owner both `shared`, the same tier). A secret created with `--owner shared` and `--tier agent` is served to every agent session without approval, so only a person whose access allows a shared create can create one. |
| `set` | `secretsmanager:DescribeSecret`, since a set reads the secret's tags for the type it checks the value as before it writes, and a refused read names that permission; `secretsmanager:PutSecretValue`; `kms:GenerateDataKey` on the agent-secrets key, and `kms:Decrypt` where the value is one a version already holds, which Secrets Manager decrypts to compare | The secret's own tags (`aws:ResourceTag/owner`) |
| `retag` | `secretsmanager:DescribeSecret`, and `secretsmanager:TagResource`, or `secretsmanager:UntagResource` for `--type none`, `--github-owners none`, `--requesters none`, `--scopes none`, `--admin none`, `--max-grant none` or `--default-grant none`. A retag that changes the secret's type also reads the secret's value to check it as that type's minter reads it, before it writes: `secretsmanager:GetSecretValue` on the secret and `kms:Decrypt` on the agent-secrets key. The check needs that read and the retag does not: where Secrets Manager refuses the read with `AccessDeniedException`, the retag writes its tags and prints one line on standard error that says the value was not checked, and the broker then reads the value itself and refuses the secret `value-malformed` when its minter could not mint from it. Any other failure of the read refuses the retag, which then writes nothing. A retag never prints the value | A retag reads the secret's tags, sends one request, then reads them back. A tag request carries owner and tier, the unchanged one re-sent as the secret holds it, plus `type` and `mint-service-account` (the rollback guard) when the command sets a type, `github-owners` when it names owners, `requesters` when it names who may ask, `scopes` when it names scopes, `admin` when it marks the secret an administrator's, `max-grant` when it sets one, and `default-grant` when it sets that; those ten keys — `owner`, `tier`, `type`, `mint-service-account`, `github-owners`, `requesters`, `scopes`, `admin`, `max-grant` and `default-grant` — are the whole set a tag request carries. An untag request's `aws:TagKeys` is the tags `none` removes, whether or not the secret holds them: `--type none` removes `type`, the old classifier `kind` and `mint-service-account` together, and `--github-owners none`, `--requesters none`, `--scopes none`, `--admin none`, `--max-grant none` and `--default-grant none` each remove the tag they name; a retag that removes a tag changes nothing else, so a refused call leaves the secret as it was. So a policy that limits `aws:TagKeys` with `ForAllValues:StringEquals` lists exactly those ten keys and `kind` (unproved: no check runs this policy), and one written for a release before the `type` tag, which lists `kind` in place of `type`, refuses every `--type` create and retag until it adds `type`. A retag that would leave a `github-app` secret without `github-owners`, or an `admin` or `google-service-account` secret without a `max-grant` of an hour or less, is refused before any write. When the read-back finds another writer's change left a `github-app` secret without `github-owners`, the retag writes nothing more: it names the tags it read, the tags it found and the retag that sets the secret right, and exits 1, so the other writer's change stands. The secret's own tags let a policy allow a person to tag their own secret (`aws:ResourceTag/owner` their email) to themselves or `shared`, and reserve a shared secret's tags for administrators |
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
| `retag` | `secretmanager.secrets.get` and `secretmanager.secrets.update`: owner and tier annotations in one call, plus `type` and `mint-service-account` (the rollback guard) when the command sets a type, `github-owners` when it names owners, `requesters` when it names who may ask, `scopes` when it names scopes, `admin` when it marks the secret an administrator's, `max-grant` when it sets one, and `default-grant` when it sets that; or, for `none`, the annotations it removes (`--type none` removes `type`, the old classifier `kind` and `mint-service-account` together), and nothing else. A retag that changes the secret's type also reads the value of `versions/latest` to check it as that type's minter reads it, before it writes: `secretmanager.versions.access` on the secret. The check needs that read and the retag does not, and people who add versions and change annotations often hold no accessor role: where Secret Manager refuses the read with `PermissionDenied`, the retag writes its annotations and prints one line on standard error that says the value was not checked, and the broker then reads the value itself and refuses the secret `value-malformed` when its minter could not mint from it. Any other failure of the read refuses the retag, which then writes nothing. A retag never prints the value |
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

## Health and logs

`GET /healthz` answers `200 {"status":"ok"}` while the broker can reach Postgres, and
`503 DATABASE_UNAVAILABLE` when it cannot. From the signal that stops it, it answers
`503 SHUTTING_DOWN` instead, for the whole drain, while every other route goes on being served
exactly as before. Use it as a readiness probe and never as a liveness probe: a liveness probe
reads a draining pod as a dead one, so the kubelet would kill the container in the middle of the
drain and drop the requests the drain exists to finish
([Running more than one pod](#running-more-than-one-pod)).

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
| `broker: the sign-in client check state=<state> issuer=<issuer> error=…` | At WARN, from the check each pod takes at startup and every hour after, when it finds a state other than `ready` that the pod did not log last: the provider refused the broker's client (`state=client_rejected`, so nobody can sign in) or could not be asked (`state=issuer_unreachable`). A check that finds the state the pod logged last, or finds the client ready with no such line before it, logs nothing, so a provider that stays away for a day is one line. Every check writes its answer to `GET /signin/status`. |
| `broker: the sign-in client check finds the client ready again issuer=<issuer> was=<state>` | At INFO: the first check that finds the client ready after the line above. `was` is the state that line named. |
| `broker: the sign-in client check was not written state=<state> error=…` | At WARN: a check could not write its answer to Postgres after the check before it wrote, so `GET /signin/status` answers the last answer written. The checks after it log nothing more until one writes. |
| `broker: the sign-in client check is written again state=<state>` | At INFO: the first check that writes its answer after the line above. |
| `approval sign-in login=<email>` / `approval sign-out login=<email>` | A person signed in to, or out of, the approval page. |
| `approval sign-in refused …` | At WARN: a sign-in made no session, the line saying why: the provider refused it, its state or return path was not the sign-in's, the code exchange or ID token failed, or the person is outside `BROKER_SIGNIN_DOMAIN` or unverified. |
| `approval page decision login=<email> record=<record id> decision=<approve or deny> outcome=<outcome>` | A person decided a secret request on the page; `outcome` is what became of it (`granted`, for one). |
| `approval sign-in: reach the provider issuer=<issuer> error=…` | At ERROR: the provider's discovery document could not be read at a sign-in, and the page answered `503` naming the issuer. |
| `broker listening addr=<host:port>` | Startup finished: configuration, migrations and the first read of the namespace all succeeded, and every port is bound: the API's, at this address; the airlock's proxy port, where the deployment has one (`airlock proxy listening addr=<host:port>`, just after); and the metrics port (the line below). |
| `broker metrics listening addr=<host:port>` | After `broker listening`, and after `airlock proxy listening` where the deployment has an airlock: the metrics port is bound at this address ([Metrics](#metrics)). A pod that cannot bind it does not start, and logs `broker: fatal` naming the address. |
| `session account pairs state=on pairs=<pairs>` / `session account pairs state=off detail=…` | At INFO, at startup, after `broker metrics listening` and before the broker serves its first request: `state=on` names each pair `BROKER_SESSION_ACCOUNT_PAIRS` gives, as `<account>=<account>` in the setting's order, joined by one space, and `state=off` says the setting is unset, so an account is refused a session name another account holds. |
| `broker: fatal error=…` | Startup refused; the error names the variable or dependency. The process exits 1. |
| `agent secret policy refused name=<secret's name in the store> reason=<reason> owner=<owner tag> first_load=<true or false>` | At ERROR, when a secret becomes one the broker refuses to serve, and when it is refused for another reason than before, with `was=<the reason before>`: one line at the change, not one for each read of the store. A pod's first load writes one for each secret it refuses, each with `first_load=true`. A load or a reread that finds a secret refused for the reason it had writes nothing, and `broker_unservable_secrets` ([Metrics](#metrics)) holds every secret refused now. `name` is the secret's whole name in the store, such as `production/agent-secrets/deel-api-key`. `owner` is its owner tag, and empty for `owner-tag-missing` and `owner-tag-malformed`. Reasons include `owner-tag-missing`, `owner-tag-malformed`, `tier-tag-missing`, `tier-tag-malformed`, `name-malformed`, `service-owner-human-tier`, `not-on-agent-secrets-key`, `no-current-value`, `type-unknown`, `github-owners-missing`, `github-owners-malformed`, `requesters-malformed` (a `requesters` list holding a comma, naming nobody, carrying an email with a capital letter, or naming a service the deployment does not register), `max-grant-required` (an `admin` secret, or a `google-service-account` one, with no `max-grant` or one longer than an hour), `requesters-not-owner` (a `google-service-account` secret whose `requesters` are not its one person owner, or that lists a service it may not: any service at agent tier, and any service at all on a secret `BROKER_SERVICE_ACCOUNT_SECRETS` binds to a `dev-<name>` account), `admin-malformed` (an `admin` tag set to anything but `true`), `admin-tier` (an `admin` secret that is not human tier), `admin-requesters` (an `admin` secret with no `requesters` list), `scopes-malformed` (a `scopes` list holding a comma, naming no scope, naming one twice, or holding an entry that is no scope name, a whole `https://www.googleapis.com/auth/…` URL among them), `scopes-type` (a `scopes` tag on a secret that is not a `google-service-account` one), `max-grant-malformed`, `default-grant-malformed` and `value-malformed` (a `github-app`, `google-login`, `google-service-account` or `azure-login` secret whose stored value that type's minter could not mint from: [the value read](#the-value-read)). |
| `agent secret policy no longer refused name=<secret's name in the store> reason=<the reason it had> owner=<owner tag> now=<servable, pending or absent>` | At INFO, when a secret the broker refused is refused no longer: `now=servable` when the broker serves it, `now=pending` when it is a new secret of that name waiting for its value (the line below), and `now=absent` when the store no longer holds it (deleted, or scheduled for deletion). |
| `agent secret policy waiting for a value name=<secret's name in the store> reason=pending-value owner=<owner tag> first_load=<true or false>` | At INFO, once, when the broker first finds a secret that `agent-secrets secret create` made and has not yet filled: it carries the `agent-secrets-create` label the create gives a Google Secret Manager secret it makes before the value, holds no version at all, and the store created it less than ten minutes ago, while the person types or pipes the value. On AWS Secrets Manager the create makes the secret with its value and writes no such tag, so a secret there waits only where another writer gave it a tag of that name and no value. The broker serves it to nobody, a request for it is refused `PENDING_VALUE`, with no request recorded and no approver asked, and a reread answers `reason=pending-value`, but it is no refusal: `broker_unservable_secrets` holds no series for it, and the loads that find it still waiting write nothing. Its value landing serves it and writes no line, as does its leaving the store. Ten minutes after its creation the create that made it gives up and deletes it, or, where the person was still typing or pasting then, deletes it when that entry ends: at Enter, or at a signal that ends the process, such as a hang-up, or Ctrl-C typed outside a paste where it still sends SIGINT. One still there without a value past the ten minutes is refused `no-current-value` (the `agent secret policy refused` line, and a `broker_unservable_secrets` series until the create deletes it). The broker itself deletes nothing: a create that was killed leaves its secret in the store with no version, refused from then on, until its owner frees the name with `gcloud secrets delete <id> --project <project number>` or stores its value; `agent-secrets secret delete NAME` only schedules the deletion 30 days out, and the name stays taken until then. A secret without that label, one holding any version, and one whose listing names no creation time are never waiting: one without a value is refused `no-current-value` at once. |
| `agent secret policy load failed; previous policy kept error=…` | At ERROR, in two cases. The five-minute reload of the namespace failed (the secret store out of reach, or refusing the broker; a Secret Manager failure names its gRPC status code alone, never the status message), and the broker keeps serving the policy from its last good read: the first failed reload of a run writes the line, and the reloads after it write nothing until one succeeds, which writes `agent secret policy load succeeded again`. Or, with `name=<NAME>` (the name a request asked for), the request named a secret the broker did not serve and the broker's reread of that one secret failed, so the request is refused `UNKNOWN_SECRET`: one line for each such request. A read in flight when the broker shuts down, and a request's reread its own caller abandoned, are not one of these and log nothing. |
| `agent secret policy load succeeded again` | At INFO: the first reload that succeeds after the line above began a run of failed ones. |
| `agent secret value read failed; its state kept name=<secret's name in the store> version=<version> error=…` | At WARN: the broker could not read a type secret's stored value to check it ([the value read](#the-value-read)), for a permission, a timeout or the store's own failure, which `error` names by its call and its class and never by a byte of the value. A stored value that is not text is no failed read: it is refused `value-malformed`. The secret keeps the state it had, but for a `value-malformed` refusal of an older version: a failed read of a version the store listed since ends that refusal, which writes `agent secret policy no longer refused`. Each load after it reads the value again and writes nothing more until a read succeeds. |
| `agent secret value read succeeded again name=<secret's name in the store> version=<version>` | At INFO: the first read of that secret's value that succeeds after the line above. |
| `agent secret uses legacy classifier tag; retag it with --type name=<secret's name in the store> old_tag=kind tag=type` | At WARN, once per secret for as long as the broker process runs, the first time a load or a reread reads a secret's type from the old `kind` tag because the secret carries no `type` tag. The broker serves the secret as that type. `agent-secrets secret retag NAME --type <type>` writes `type`, after which the line is not written again for that secret. |
| `broker: answer refused answer_id=<id> status=<status> code=<code> method=<method> route=<route> caller_class=<class> caller=<caller> secrets=[<names>] secrets_count=<n>` | At WARN, once for each refusal of the API port and of the airlock's proxy port: every answer from 400 to 499 but a `429` and the two answers a machine gets while it polls a device login, `slow_down` and `authorization_pending`, which are counted and not logged. It is written before the answer is sent, and it names the answer id the refused caller was given ([Refusals and the answer id](#refusals-and-the-answer-id)). It lists the first secret names the request named, up to 32 KiB of names as the line writes them (JSON-escaped, each with its two quotes), counts them all in `secrets_count`, and adds `secrets_omitted=<n>` when it left names out, since Cloud Logging refuses an entry over 256 KiB. An answer Go's HTTP server writes itself before any of the broker's code runs, such as a `400` for a malformed request line, has no answer id and writes no line. |
| `broker: <operation> failed error=…` | A request failed with a 500 or 503; the line carries the cause the response does not. A reread whose own request ended before it finished - its caller went away, or it reached the 45-second bound - is not one of these: it answers `503 REQUEST_ENDED` and logs nothing, since anyone can end a request. One reread whose request ended only while it read the value of a `github-app`, `google-login`, `google-service-account` or `azure-login` secret has already put the secret's new tags in force on that pod, so it still tells the other pods to reread the secret, as a `200` does. A held read of a request (`GET /v1/requests/{id}?wait=`) whose caller went away before its wait ran out or in the half second after it, by a Ctrl-C, a timeout of its own or a front door that cut the call, is not one of these either: it answers `503 REQUEST_ENDED`, logs nothing, and is counted `cancelled`, not `failed` ([Metrics](#metrics)). Every other read of a request that ends early is one of these, `read request failed` with a failed `503 DATABASE_UNAVAILABLE`: a read with no wait, which owes its answer at once, and a held read the database stalled until its caller gave up or the 45-second bound ended it. A held read's wait counts from when the request arrived, before its proof is checked, so a caller that goes away while its proof is still being checked leaves inside its wait; one that goes away while the broker looks its enrollment up or records its proof's jti ends that check instead, which is `verify proof failed`, a failed `503 DATABASE_UNAVAILABLE`. |
| `broker: mint failed name=<NAME> type=<type> class=<class> attempts=<attempts> error=…` | At ERROR, for each release of a type secret its minter could not make. `class` says whose fix it is: `stored-value`, the secret's value is not a key or login the type takes, so its owner stores a new one; `binding`, a `google-service-account` secret the deployment binds to no account, or to an account its stored value does not name, so the operator fixes `BROKER_SERVICE_ACCOUNT_SECRETS` — this one is not the secret's owner to fix by storing a new value, and the mint is refused before any call reaches Google; `refused`, GitHub or Google answered and refused, such as a Google login that needs a new sign-in or a GitHub App with no installation on a listed account, so its owner signs in again or fixes the App; `upstream`, GitHub or Google could not be asked, answered no usable reply or rate limited the call, or answered a narrowed GitHub token holding more than the narrowing asked, which the broker revokes at GitHub, so check the broker's egress and the upstream's status. `attempts` is how many times the broker sent the call the mint failed at, `1` to `3` ([the mint retry](#the-mint-retry)), and `0` for a failure that was no call's, such as a stored value the minter could not read. A `stored-value` failure also refuses the secret as `value-malformed`, so its grants release nothing until a value that decodes is stored. A narrowing the caller got wrong is never this line: see `broker: narrowing refused`. The error never holds the stored value, a token or a value from GitHub's answer. The session is answered `502 MINT_FAILED` with the same error. |
| `broker: a stored value's trailing line ending was removed name=<NAME>` | At INFO, once for each value of a release the line-ending rule changed, so twice for one release that changed two values: both the value a session is handed and the input a minter reads pass that rule. The line carries the secret's name and no part of any value. The rule removes the trailing run of carriage returns and line feeds only where what is left is not empty and holds no line ending inside it, so a value whose last line ending is part of it is released as it is. |
| `broker: GitHub did not revoke a narrowed token it minted wider than asked; it lapses at its expiry app=… installation=… expires_at=… error=…` | At WARN, beside a `mint failed … class=upstream` line: GitHub answered a narrowed token holding more than asked, the broker refused it, and GitHub did not confirm the revoke the broker then sent. A `401` or `404` to the revoke's first attempt is GitHub refusing it and writes this line, since nothing in the revoke can have revoked a token minted moments before. A `401` or `404` to a retried attempt counts as confirmed and writes no line, since an earlier attempt may have revoked the token and lost its answer; an earlier attempt GitHub rate-limited, which revoked nothing, cannot be told apart from that. No caller got the token; GitHub honours it until `expires_at`, at most an hour. Check the broker's route to GitHub, as for the line it follows. |
| `broker: GitHub did not revoke a token minted for a check; it lapses at its expiry app=… installation=… expires_at=… error=…` | At WARN, during the check the broker makes of a `github_app` narrowing before it opens the approval request: the check mints the token the release would mint and revokes it at once, and GitHub did not confirm that revoke, most often because the check's own time ran out first. Nobody got the token; GitHub honours it until `expires_at`, at most an hour. Check the broker's route to GitHub. |
| `broker: GitHub did not revoke a token it minted whose answer the broker could not use; it lapses at its expiry app=… installation=… error=…` | At WARN, once for each token request whose answer GitHub sent `201`, so it minted the token, and the broker could not use: cut off, larger than the broker reads, undecodable or without an expiry. The broker sent GitHub the revoke of the token the part that arrived names, under a bound of its own (10 seconds) that the caller's own deadline does not end, for a release and for a check alike, and GitHub did not confirm it. The line comes before whatever the mint does next: a release may send the request again and hand over the next token, or fail with a `mint failed … class=upstream` line, and a check is skipped. Nobody got this token; GitHub honours it for at most an hour. Check the broker's route to GitHub. |
| `broker: GitHub minted a token the broker cannot revoke, since the answer it holds does not name it; that token stays live until it lapses at its expiry app=… installation=… error=…` | At WARN, once for each token request GitHub answered `201`, so it minted a token, whose answer the broker holds does not name that token: a whole answer with no `token` member, or the part of a cut-off answer that arrived before the token. Nothing can revoke the token, and GitHub honours it, held by nobody, for at most an hour. Like the line above, it comes before a resend that may succeed, a `mint failed` line or a skipped check. An answer lost before any of it arrived writes no line, since nothing says GitHub minted. A check does not send again a token request that may have reached GitHub and failed, so it leaves at most one such token; a release sends it again, as for any failed call. |
| `broker: GitHub did not revoke a token a release minted and handed to nobody; it lapses at its expiry app=… installation=… expires_at=… error=…`, or `name=…` in place of `app=…` | At WARN: a release minted a token that it hands to nobody, so it revoked that token, and GitHub did not confirm the revoke. With `app=`, beside a `mint failed` line, the release of several installations failed at a later installation. With `name=`, the secret the token was minted for: the release failed after that name, at a later name of the grant (its value no longer in the store, a split the airlock refused, a stored source the policy no longer serves), at the airlock's placeholders or at its `grant.used` row; or the name is behind the airlock, so the session was handed placeholders and the proxy mints a token of its own, and the token this release minted only to learn the placeholders' slots goes to nobody. Nobody got the token; GitHub honours it until `expires_at`, at most an hour. Check the broker's route to GitHub. |
| `broker: narrowing refused grant=… enrollment=… name=<NAME> error=…` | At WARN, for each release whose narrowing of its GitHub App tokens names what the secret cannot serve: an account its `github-owners` tag does not list, refused before any minter runs (`403 NARROWING_OWNER_NOT_LISTED`), or a narrowing GitHub answered 422, such as a repository the installation does not reach (`422 NARROWING_REFUSED`); a 422 that says it is a rate limit is an upstream `mint failed` instead. Nothing was released. The caller fixes its narrowing; the App and the secret are not at fault, so this line needs no action unless one session repeats it. |
| `broker: narrowing refused enrollment=… name=<NAME> error=…`, with no `grant=` key | The same line for a refusal the broker made before the approval request: a request that carries a `github_app` narrowing and that a person would decide is checked first, and this is what that check refused. The keys are the enrollment, the secret the refusal is about (empty for `NARROWING_NOT_APPLICABLE`, which is about the request as a whole) and the error. There is no `grant=` key because no grant exists: no request row was written, no record was made and nobody was asked to approve anything. The refusal names the account its `github-owners` tag does not list (`403 NARROWING_OWNER_NOT_LISTED`), a request no name of which is a `github-app` secret (`400 NARROWING_NOT_APPLICABLE`), a name the airlock holds back (`409 NARROWING_BEHIND_AIRLOCK`), or what GitHub refused (`422 NARROWING_REFUSED`). The broker learns of a repository the installation does not reach, or of read access the App does not hold, by minting the token the release would mint and revoking it at once. It learns of an App with no installation on that account at all from the App's list of installations, and a release answers that one `502 MINT_FAILED` instead, its own answer for every refused mint. It checks a narrowing to an account alone from that list alone, and mints no token for it. The caller fixes its narrowing, as above. |
| `broker: the narrowing check was skipped enrollment=… name=… reason=… error=…` | At INFO, each time trouble, never a refusal, ended that check. `reason=` is one of eight, and each is the whole reason it ended: `the requester could not be read`; `this broker has no github-app minter`; `this enrollment has reached GitHub for more checks than its bound allows` (five at once, one more every twelve seconds); `the stored value could not be read` (the secret store out of reach, or refusing the broker); `the stored value is not an App id and private key the minter can read`, which the secret's owner fixes by storing a new value, as they do for a release's `mint failed … class=stored-value`; `the check's time ran out, or its caller went away, before GitHub answered` (the check's budget is ten seconds for the whole check and five for one mint, and a caller that goes away ends it too); `GitHub refused the App rather than the narrowing` (a 401 to the App's own token, a 403, or a 404 — the App's key, its clock or its installation, and nothing a caller mends by narrowing differently); or `GitHub's answer decided nothing about the narrowing` (a transport failure, a 5xx, or a rate limit). `error=` is there only where something failed, so the two reasons that need no failure carry none. `name=` is the secret the check had reached, and empty where it had reached none. The request then takes the path it took before this check existed: the person is asked, and the release makes the refusal. Nothing is wrong with the request, so one line needs no action; a run of them says GitHub, the secret store or the bounds are leaving the verdict unreached. |
| `broker: grant log …` | At INFO, once for each request decision, each `request.judged` row and each grant end. Every line carries the event's `kind`, its `actor`, whichever of the enrollment, request and grant ids the row names, and `raw`, the audit row's own detail; it carries no audit id and no timestamp, since the line is written once the row has committed and the log keeps its own time. The rest is what the writer of that line had in hand, which differs by line: a `request.created` line adds the session and its person, the service the broker verified, the service the session's credential claims (`claimed_service`), the secrets and how each was decided, the state, the approver, the lifetime, the reason and the detail; a `request.granted` or `request.denied` line adds the session and its person, the claimed service, the state, the approver, who decided, the lifetime, the reason and the detail, and neither the verified service nor the secrets; a `request.cancelled` or `request.expired` line adds the state, the detail and, for a cancel, who decided; a `request.judged` line, written for every judge run that reached a verdict, its hand-overs to a person and its failed checks included, adds the session and its person, the claimed service, and the grant where the verdict granted one; a `grant.revoked` or `grant.withheld` line adds nothing more, except the session's person when a person's revoke ended it. A reader joins a line to its request's own `request.created` line by `request_id` for the rest, or reads that row from `GET /v1/grant-log`, which joins the tables and holds every field. The line is the copy a deployment's log pipeline keeps of the rows that route answers from the `audit` table, so a closed period's decisions can be read without the database ([the audit record](/legion/broker/concepts/#the-audit-record)). A release (`grant.used`) writes the line below instead, and an enrollment row writes none. |
| `broker: a pending request's record is signed for another broker address; it is ended, and its session's new request replaces it request=… record_id=… new_request=… enrollment=…` | At INFO, once for each pending request the broker ended because its record is signed for another broker address: its session asked a broker at another `BROKER_PUBLIC_URL` that shares this database, or this broker before its public URL changed, and then asked this broker again for one of its names, where no person and no judge could decide the request it left waiting. The broker cancelled that request, recording itself as the one who decided it, with a detail that says the broker moved, and wrote the `broker: grant log` line of its `request.cancelled` row beside this one. `request` is the ended request, `record_id` its record, `new_request` the request the session is referred to now (the one its ask made, pending, granted or denied; the pending request of this broker the ask joined or was refused `REQUEST_PENDING` for; or the request of the live grant the ask was answered with), and `enrollment` the enrollment that asked. The line is written once the end has committed, so an ask whose transaction failed writes none; each value is an id, and the line holds no secret value, no token and no request object. It needs no action: the broker cleared a request its session left behind when it moved. |
| `broker: a grant's approval chain does not verify; it releases nothing grant=… record_id=… enrollment=… at=<values, airlock or reuse> reason=…` | At WARN, each time a release finds that a grant's whole approval chain no longer verifies. `at` says where: `values`, the session's own release, answered `403 GRANT_CHAIN_INVALID`; `airlock`, the airlock proxy's read of the values it swaps in, which the proxy answers the same; or `reuse`, the broker's check of a live grant it would hand back to a new request for the same names, which passes that grant over for the next newest and, with none left, asks afresh. `grant` is the grant, `record_id` the record its chain rests on, `enrollment` the enrollment that holds it, and `reason` the refusal's text, which names the link that failed. Most often that is `aud does not match`: the grant's record was signed for another `BROKER_PUBLIC_URL`, by a broker at another address that shares this database or by this broker before its public URL changed, and the grant's session, which asks here now, still holds it. A database failure during the check writes no such line, and the line holds no secret value, no token and no request object. |
| `broker: released a grant grant=… request=… enrollment=… names=[<NAME> …] reason=<reason> call_reason=<reason> command=[<argument> …] github_app=…` | At INFO, once for each release of a grant, after its `grant.used` audit row is written: what each grant was spent on, and why. `reason` is the reason the grant's request gave. A session's release adds `call_reason`, the `--reason` its own call gave, where that differs from the grant's: a session that asks again for secrets it holds a live grant of gets that grant back, and its new reason is in no request. The broker drops the characters a request's reason may not hold and cuts it to 400 characters. A session's release also adds `command`, the argument vector the `agent-secrets NAME -- <command>` form said the release starts: its first 32 arguments, each cut to 256 bytes, and `github_app`, the narrowing of its GitHub App tokens the release applied, where it applied one. In the cut command the broker replaces with `[redacted]` each secret it can name: each value of eight bytes or more that the release handed out, each string of eight bytes or more inside a stored JSON value, each token inside a document a minter made, each placeholder of a name the airlock holds back, and every GitHub token, AWS access key id, Google OAuth access token, Slack token of any `xox` prefix and PEM private key, wherever it came from; an argument the cut ended loses any end that starts one of them. **A secret of no such kind — a value from another store, a password typed in, a value shorter than eight bytes — is logged as the session typed it, so pass secrets in the command's environment, as the exec form does, and never on its command line.** `call_reason` and `command` are the session's own word, which the broker records and does not check, and a client from before the fields names neither, which logs an empty `command`. The airlock proxy's release of a grant writes `airlock=true` in their place, since it starts no command. The `grant.used` row's `detail` holds the same fields, and `GET /v1/grant-log` answers that detail to every account with the power to read the grant log. While a client of this version meets a broker from before these fields, that broker refuses each release once with `400 INVALID_NARROWING` naming the unknown field, and the client sends it again without them: the older broker writes a `broker: answer refused … code=INVALID_NARROWING` line and counts `broker_answers_total{outcome="refused",code="INVALID_NARROWING"}` for each, which is expected until the broker is rolled. |
| `broker: an enrollment took a session name from the account paired with its own session=<session> from=<principal> to=<principal> enrollment=<id>` | At WARN, once the enrollment has committed: a pod of an account `BROKER_SESSION_ACCOUNT_PAIRS` pairs with the name's holder enrolled under that name while the holder's claim on it had not run out (90 days past its last lease or token), and took the name. `from` is the holder and `to` the enrolling pod's account, each a principal, `account:` and the service account. Where a login of the same account enrolled both pods, the enrollment also took what the holder's enrollments of that name held: their grants, pending requests and withheld names ([Concepts](/legion/broker/concepts/#requests-and-grants)). The line alone does not say that anything moved: where another daemon's login enrolled the new pod, the line is written and nothing moves, and the old enrollment keeps what it holds, dormant, until `BROKER_SESSION_CARRY_SECONDS` runs out or a pod its own daemon's login enrols takes it. It is expected while a deployment moves its pods to another account; outside such a move it is a pod of one account of a pair taking a session of the other's. |
| `broker: the token exchange took a session name from the account paired with its own session=<session> from=<principal> to=<principal>` | At WARN, once the exchange has committed: the same take, made by a pod's token exchange rather than its enrollment. The pod's next enrollment under the name takes what the holder's enrollments of it hold that a login of the same account enrolled, and nothing where another daemon's login enrols it. |
| `broker: the assignee of a Legion pod's issue could not be read; its request is listed for everyone issue=… enrollment=… error=… bound_reached=…` | At WARN, once for a Legion worker pod's request for a shared secret whose issue's assignee the broker could not read: Dispatch refused the read or answered nothing it could decode, the read ran past its three seconds (`bound_reached=true`), or the broker could not read the pod's own requester first. `issue` is the issue the pod's claim names and `enrollment` the pod's enrollment. The request is listed in every person's Inbox, as when the issue has no assignee, which logs nothing ([Concepts](/legion/broker/concepts/#approvals)). A line on every such request means that step is off: check the token file `BROKER_JUDGE_DISPATCH_TOKEN_FILE` names and that `BROKER_JUDGE_DISPATCH_URL` answers. The error names no token. |
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

Every answer of the API port and of the airlock's proxy port carries an answer id, 32 hexadecimal
characters, in its `X-Broker-Answer-Id` header. An answer from 400 to 499 carries it in its body
too: a JSON one as its `answer_id` member, a refusal of the airlock's proxy port at the end of its
text as `(answer <id>)`, and a refusal page of the approval page as an id to quote. A `429` is no
refusal, so two bodies carry no id: the text of a `429` that the Azure token endpoint answered and
the airlock's proxy port forwards, and the approval page's `429` page. The header of each still
carries it. The exception on both ports is the answers Go's HTTP server writes itself before any
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

A refused call to a bound host writes this line in place of its call line,
`airlock proxy: a call to a bound host`, so each such call still writes one line
([Run the broker locally](/legion/broker/guides/run-locally/#reach-a-secret-behind-the-airlock)
shows both). A `429` that the Azure token endpoint answers and the proxy forwards is no refusal:
it keeps its call line at INFO, which names its answer id and its status, and writes no refusal
line.

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
  `BROKER_AIRLOCK_CLUSTER_RANGES`), `unresolved` (a host the resolver answered no address or an
  error for), `unreachable` (every address was dialled and none answered), or `cancelled` (the
  command closed its connection while the proxy looked its host up or dialled it, and every
  attempt was given up for that, so its error says nothing about the service; a dial one of
  whose addresses failed on its own before the command left keeps that failure's class).
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
together are two. A line of one JSON object that holds no `\u` escape, no byte past ASCII and no
`agent-secrets-placeholder-` in any ASCII case cannot hold a placeholder, and is handed on as it
came: every other escape JSON has decodes to a character no placeholder holds. So a value is
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
the one record of the API port's `429` and of a device grant's poll answers.

On the API port, an answer that no refusal line records names its id in no line: a `429`, the two
answers of a device login's poll, and every answer of 500 and above. `agent-secrets` prints the id
of a `429` and of a failure as well. For a `502 MINT_FAILED`, read the `broker: mint failed` line
instead ([Health and logs](#health-and-logs)). It names the secret as `name` and holds no answer
id, so find it by that name and the time of the answer.

### The value read

The broker reads the stored value of each `github-app`, `google-login`, `google-service-account`
and `azure-login` secret once for each version and type of it that a pod lists and has not
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
500 or above, a rate limit (a `429`, GitHub's `X-RateLimit-Remaining: 0`, or a body naming a rate
limit), or a call or a read of its answer that failed while the caller's request was still open.
The broker does not retry a `4xx` that is no rate limit, a call whose request its caller ended, an
answer larger than the minter reads, or a `2xx` or `3xx` answer other than the one the call asked
for. One more call is not retried: the GitHub token request that the check of a `github_app`
narrowing before the approval request makes, once it failed in any way but one known to come
before a connection, since GitHub may have received it whole and minted a token that the check
will never hold, whatever the broker's own write reported. The same call that failed before a
connection, because its name did not resolve, its connection or a proxy's tunnel could not be
made, or the broker did not trust GitHub's certificate, minted nothing and is retried as any other
call is. A TLS record error or alert after the handshake counts as a failure after the request
went out; a TLS failure during the handshake comes before a connection, so the call is retried.
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
answers `405`. The boot binds it after the API's port and the airlock's proxy port and logs
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
| `broker_answers_total` | counter | `outcome`, `code`, `caller_class` | Each answer the broker's code writes on the API port and on the airlock's proxy port; the answers Go's HTTP server writes itself before that code runs are not counted ([Refusals and the answer id](#refusals-and-the-answer-id)). `outcome` is `served` (below 400), `refused` (a refusal the `broker: answer refused` line records), `rate_limited` (a `429`, and the device grant's `slow_down`), `pending` (the device grant's `authorization_pending`, a machine's poll while its person has not decided), `cancelled` (a held read of a request, `GET /v1/requests/{id}?wait=`, whose caller went away before its wait ran out or in the half second after it, which writes no line; a read with no wait is never counted here, and one whose caller left while the broker looked its enrollment up or recorded its proof's jti is `failed`, under `caller_class` `unauthenticated`, with the `verify proof failed` line) or `failed` (500 and above, but a `cancelled` one). `code` is a refusal's code, as its line names it, or the code the airlock's proxy port names for a failure of its own, such as `UPSTREAM_UNREACHABLE`. Every other answer's `code` is `HTTP_<status>`. `caller_class` is as the refusal line names it. A call the proxy carried to a service counts as `served`, with the service's own status as its `HTTP_<status>`. |
| `broker_mint_attempts_total` | counter | `type`, `outcome`, `attempt` | Each attempt of each call a minter makes to GitHub or Google ([the mint retry](#the-mint-retry)). `type` is the secret's type. `outcome` is `ok`, `upstream` (an answer of 500 or above, no answer, or an answer it could not use), `rate_limited`, `refused` (a `4xx` that is no rate limit) or `cancelled` (the caller's request ended). `attempt` is `1`, `2` or `3`. |
| `broker_unservable_secrets` | gauge | `secret`, `reason`, `owner` | One series of value 1 for each secret the broker refuses to serve now. `secret` is the secret's whole name in the store, as the `agent secret policy refused` line names it, `reason` is that line's reason, and `owner` its owner tag, empty for `owner-tag-missing` and `owner-tag-malformed`. A secret that becomes servable, or leaves the store, is gone from the next scrape, and a reread of a name nobody created adds no series. Nor does a secret waiting for its value in the ten minutes after its creation (`agent secret policy waiting for a value`), which is no refusal. |
| `broker_policy_store_reads_total` | counter | `read` | Each call the policy makes to the secret store, a failed one too: `list` for a load of the whole namespace, `describe` for one secret's reread, and `value` for one type secret's value ([the value read](#the-value-read)). It is how to see that this pod read the store: the broker logs no line for a read whose secrets it serves. On Secret Manager each of them spends the project's read quota. A `list` is one `ListSecrets` for each page and one `GetSecretVersion` for each secret, and a `describe` is one `GetSecret` and one `GetSecretVersion`. A `value` is one `GetSecret`, or none when this pod read that secret in the 30 seconds before, and then the `AccessSecretVersion` that reads the value, which counts against Secret Manager's separate access quota. |
| `broker_nats_callout_decisions_total` | counter | `kind`, `outcome`, `reason` | Each NATS connection the auth service answered, where the broker runs it. `kind` is the credential the connection presented: `session`, `service` or `none`. `outcome` is `admitted` or `refused`, and `reason` is a refusal's one fixed word (empty when admitted), such as `waiting_bound` for one refused at once because the replica already held 1024 requests waiting for a decision. |
| `broker_nats_callout_seconds` | histogram | | How long the auth service took to answer one connection, from the request reaching the replica. Its buckets straddle the decision's 1.5-second budget and the two seconds the hub waits for an answer. |
| `broker_nats_callout_dropped_total` | counter | `reason` | Each authorization request the auth service answered not at all; the hub refuses that client at its own timeout. `reason` is `past_bounds` (the replica already held 1024 requests waiting and 1024 being refused), `hub_gave_up` (the hub had stopped waiting for the answer, read from the hub's own clock, so no skew between the broker's clock and the hub's counts here), `unopened` (not sealed to the service's curve key), `not_a_request`, or `pending_limit` (dropped by the NATS client on arrival, past the subscription's pending limits). The count moves every second whether or not the log takes writes. The broker logs the first drop in a minute, at WARN, `natscallout: dropped authorization requests unanswered` with each reason's count since the last such line and each reason's first drop (`<reason>_first`); a replica that stops logs what it counted since its last line. |

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
`BROKER_VM_PROJECTS`, `BROKER_VM_INSTANCES` and `BROKER_VM_EXCHANGE_ACCOUNTS`. From then every
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
  shipped with; a later edit of the same file keeps it first and adds one naming the edit before
  it. The line names a file of the same version and 64 lower-case hex digits; a line that begins
  `-- replaces:` and is written any other way, or that sits inside a quoted string or a function
  body, stops every start, naming the file and the line. A pull request that edits a migration
  main already holds without such lines fails CI's `envoy-go-checks` job before it merges.

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
([Concepts](/legion/broker/concepts/#the-model-judge)):

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
