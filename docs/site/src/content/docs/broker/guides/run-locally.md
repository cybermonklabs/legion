---
title: Run the broker locally
description: Start a whole Secrets Broker stack on your machine with one script and drive a machine login, a session and an approval by hand.
sidebar:
  order: 13
---

`packages/envoy/scripts/dev-broker.sh` creates a Postgres database of its own, builds the broker,
its clients, `dev-oidc-issuer` and `dev-airlock-stands`, writes a fake secrets file, which stands
in for Secrets Manager, starts `dev-airlock-stands`, whose GitHub stand is the GitHub the two
`github-app` secrets mint at, starts `dev-oidc-issuer`, which stands in both for a cluster's
ServiceAccount token issuer and for the identity provider a person signs in to the broker at, and
starts the broker on a free port, trusting that issuer for both. It needs Go, `jq`, `curl` and a
Postgres server: by default it
starts one in Docker through `packages/envoy/scripts/dev-postgres.sh` (a container named
`dispatch-pg`, reused when it is there; one made from another image than that script's is
refused, with the commands that move its data onto the right one). Without Docker, run Postgres
yourself and set `DEV_BROKER_POSTGRES_URL` to `postgres://<user>@<host>:<port>/<database>`, for a
role that may create databases; the script then creates and drops its database there with `psql`.

Set `DEV_BROKER_GCP_PREFIX` (`projects/<project number>/secrets/<id prefix>`) and
`DEV_BROKER_GCP_KMS_KEY_NAME` (a global Cloud KMS key) together to give the broker Google Cloud
Secret Manager as its store under them, with an empty fake secrets file: `GET /v1/settings` then
describes that store, so `agent-secrets secret` writes the real Secret Manager under the
credential you choose for it ([Manage a secret](/legion/broker/guides/manage-a-secret/#before-you-start)),
while the broker serves none of the demo secrets below. One of the two alone is refused (exit 2).

## Start the stack

With Docker the command is the script alone; this run used a Postgres server of its own:

```console
$ DEV_BROKER_POSTGRES_URL='postgres://<user>@<host>:<port>/<database>' bash packages/envoy/scripts/dev-broker.sh
dev-broker: workdir /legion/scratch/tmp/agent-secrets-dev.70a6Eu
dev-broker: created isolated database dev_broker_70a6eu
dev-broker: building broker, dev-oidc-issuer, dev-airlock-stands, agent-secrets, agent-secrets-helper and agent-secrets-devrelay...
dev-broker: built all six into /legion/scratch/tmp/agent-secrets-dev.70a6Eu/bin
dev-broker: starting dev-airlock-stands (its GitHub stand mints the github-app secrets)...
dev-airlock-stands: stands listening records=http://127.0.0.1:57249
dev-broker: starting dev-oidc-issuer...
dev-oidc-issuer: /legion/scratch/tmp/agent-secrets-dev.70a6Eu/account-token holds system:serviceaccount:example-ns:example-dispatch's token for audience legion-broker-pod (pod b7fdd2f3-e4ae-4ab5-9539-739df70be470)
dev-oidc-issuer: /legion/scratch/tmp/agent-secrets-dev.70a6Eu/listener-token holds system:serviceaccount:example-ns:example-listener's token for audience legion-broker-pod (pod d29d7435-7cb4-4efa-a9e9-9c172a4c4b08)
dev-oidc-issuer: /legion/scratch/tmp/agent-secrets-dev.70a6Eu/exchange-token holds system:serviceaccount:example-ns:example-worker's token for audience legion-broker-exchange (pod 82b2b4e8-09bf-4ab5-b9ff-4ae8f3c57cb0)
dev-oidc-issuer: /legion/scratch/tmp/agent-secrets-dev.70a6Eu/read-token holds system:serviceaccount:example-ns:example-reader's token for audience legion-broker-exchange (pod 4efe0005-1da7-4c4b-b055-cecf6f87ac9d)
dev-oidc-issuer: /legion/scratch/tmp/agent-secrets-dev.70a6Eu/pod-token holds system:serviceaccount:example-ns:example-worker's token for audience legion-broker-pod (pod 82b2b4e8-09bf-4ab5-b9ff-4ae8f3c57cb0)
dev-oidc-issuer: /legion/scratch/tmp/agent-secrets-dev.70a6Eu/registrar-token holds system:serviceaccount:example-ns:example-daemon's token for audience legion-broker-pod (pod 5a8afd1c-1cc8-4260-bc4b-42487058c662)
dev-oidc-issuer: code flow serves client dev-broker; an authorization with none queued at /_dev/sign-in-as signs in ada@example.com
dev-oidc-issuer: issuer listening url=http://127.0.0.1:31247
dev-broker: starting broker...
{"time":"2026-11-16T17:14:38.440535554Z","severity":"INFO","message":"broker stop budget","drain":"0s","shutdown":"60s","total":"60s"}
{"time":"2026-11-16T17:14:38.44068581Z","severity":"INFO","message":"database sign-in","method":"password"}
{"time":"2026-11-16T17:14:38.440703001Z","severity":"INFO","message":"approval sign-in on","issuer":"http://127.0.0.1:31247","client_id":"dev-broker","domain":"example.com"}
{"time":"2026-11-16T17:14:38.724608823Z","severity":"INFO","message":"secret store","store":"aws-secrets-manager","credentials":"fake secrets file"}
{"time":"2026-11-16T17:14:38.749006352Z","severity":"INFO","message":"broker: listening for the other pods' secret rereads","channel":"broker_secret_reread"}
{"time":"2026-11-16T17:14:38.749954832Z","severity":"INFO","message":"broker listening","addr":"127.0.0.1:32605"}
{"time":"2026-11-16T17:14:38.750031637Z","severity":"INFO","message":"broker metrics listening","addr":"127.0.0.1:34409"}
{"time":"2026-11-16T17:14:38.750108035Z","severity":"INFO","message":"model judge","state":"off","detail":"none of BROKER_JUDGE_MODEL_URL, BROKER_JUDGE_MODEL, BROKER_JUDGE_MODEL_AUDIENCE, BROKER_JUDGE_DISPATCH_URL, BROKER_JUDGE_DISPATCH_TOKEN_FILE is set, so every human-tier request waits for the person who decides it today"}
{"time":"2026-11-16T17:14:38.753592857Z","severity":"INFO","message":"project access check","state":"off","detail":"BROKER_PROJECT_ACCESS_URL is unset, so a token naming no project is minted as before and a person's trade naming one is refused"}
{"time":"2026-11-16T17:14:38.753658826Z","severity":"INFO","message":"token exchange","state":"on","kubernetes":true,"github":false,"accounts":1,"read_accounts":1,"workflows":0,"machines":0}
{"time":"2026-11-16T17:14:38.753890376Z","severity":"INFO","message":"machine login device grant","state":"on","verification_uri":"http://127.0.0.1/credentials/machine"}
{"time":"2026-11-16T17:14:38.786731457Z","severity":"INFO","message":"broker reread rate","replicas":2,"every":"1s","burst":10}
{"time":"2026-11-16T17:14:39.40407662Z","severity":"INFO","message":"broker: registered a pod to write under a session name","service":"legion-daemon","account":"system:serviceaccount:example-ns:example-worker","pod":"82b2b4e8-09bf-4ab5-b9ff-4ae8f3c57cb0","session":"example-worker-session"}
dev-broker: registered the exchange pod to write under example-worker-session

dev-broker: ready.

  export AGENT_SECRETS_URL=http://127.0.0.1:32605
  export AGENT_SECRETS_ACCOUNT_TOKEN_FILE=/legion/scratch/tmp/agent-secrets-dev.70a6Eu/account-token
  export AGENT_SECRETS_APPROVER=ada@example.com
  export AGENT_SECRETS_HELPER_SOCK=/legion/scratch/tmp/agent-secrets-dev.70a6Eu/helper.sock
  export AGENT_SECRETS_OPERATOR_FILE=/legion/scratch/tmp/agent-secrets-dev.70a6Eu/operator
  export AGENT_SECRETS_MACHINE_FILE=/legion/scratch/tmp/agent-secrets-dev.70a6Eu/machine.json
  export AGENT_SECRETS_STATE_FILE=/legion/scratch/tmp/agent-secrets-dev.70a6Eu/sessions.json
  export DEV_BROKER_DIR=/legion/scratch/tmp/agent-secrets-dev.70a6Eu
  export DEV_BROKER_METRICS_URL=http://127.0.0.1:34409/metrics
  export DEV_BROKER_POD_UID=82b2b4e8-09bf-4ab5-b9ff-4ae8f3c57cb0
  export DEV_BROKER_SERVICE=legion-daemon
  export DEV_BROKER_GITHUB_URL=http://127.0.0.1:18701
  export DEV_BROKER_GITHUB_RECORDS_URL=http://127.0.0.1:57249/records/github
  export DEV_BROKER_GITHUB_ANSWER_URL=http://127.0.0.1:57249/answers/github
  export DEV_BROKER_GITHUB_SECRET=DEMO_GITHUB_APP
  export DEV_BROKER_GITHUB_OWNER=acme-org
  export DEV_BROKER_GITHUB_CASE_SECRET=DEMO_GITHUB_APP_CASE_PAIR
  export DEV_BROKER_GITHUB_CASE_OWNER=ada
  export DEV_BROKER_GITHUB_HUMAN_SECRET=DEMO_GITHUB_APP_HUMAN
  export PATH=/legion/scratch/tmp/agent-secrets-dev.70a6Eu/bin:$PATH
...
```

The JSON lines are the broker's own log, which the script also keeps in
`$DEV_BROKER_DIR/broker.log` ([Read the log and the metrics](#read-the-log-and-the-metrics)). The
script reads the address the broker bound from its `broker listening` line, and the address of its
metrics port from its `broker metrics listening` line.

With Docker, the script says `dispatch-pg is already running, reusing it` (or starts it) before it
creates the database.

Leave it running, and paste the exports into a second shell. They point the clients at this broker
and name the caller token, the ServiceAccount token with which Dispatch's server, and here
`agent-secrets-devrelay` in its place, decides a machine login for an approver
([Concepts](/legion/broker/concepts/#approvals)). No token decides a secret request: that is
decided on the broker's own page, by the person signed in to the broker. The broker lists that
token's account, `system:serviceaccount:example-ns:example-dispatch`, as its caller `dispatch`, and
`dev-oidc-issuer` rewrites the file before its token lapses, as a kubelet rewrites a pod's
projected token, so the exports stay good for as long as the stack runs. They also keep the
helper's socket, its operator file (the Dispatch login the helper's machine login names,
`ada@example.com`) and its
machine file (the login the helper keeps across restarts) in the stack's workdir, so the helper
never touches one already running for your user, or your own machine's login, and nothing lands in
your current directory. Check that `type agent-secrets` names the workdir's `bin/`: a tool manager
that puts its own directories first on `PATH` at every prompt runs an installed `agent-secrets`
instead.

The broker lists a second caller too, and a caller's name decides which of its named-caller routes
that account reaches. `dispatch` reaches all of them: the approval, denial and listing routes
Dispatch's server calls, the machine-login and grant revocations beside them, and the two OAuth
routes below. `listener`, whose token is `$DEV_BROKER_DIR/listener-token`, reaches
`POST /v1/oauth/introspect` alone, since the Envoy listener reads a token back and relays nobody's
decision. On any other of those routes it is refused by name. `curl -H @-` reads the header from
standard input, so the token is in no command line another process can read:

```console
$ printf 'Authorization: Bearer %s' "$(cat "$DEV_BROKER_DIR/listener-token")" | curl -s -H @- "$AGENT_SECRETS_URL/v1/pending?approver=$AGENT_SECRETS_APPROVER"
{"code":"ACCOUNT_LACKS_POWER","error":"the caller \"listener\" may not call GET /v1/pending","answer_id":"e288edff6253ddc22cc3bc93e480eb42"}
```

`answer_id` names the broker's one log line for that refusal, `broker: answer refused`, which the
broker writes before it answers ([Read the log and the metrics](#read-the-log-and-the-metrics)).

An account `BROKER_API_ACCOUNTS` does not list at all is `403 ACCOUNT_NOT_LISTED` instead, and an
entry naming a caller the broker does not know refuses the boot, naming that name and the names it
knows, so a typo never becomes a caller that reaches nothing.

The fake secrets file holds three secrets of `ada@example.com`'s, under the namespace
`example/agent-secrets/` and on its example key: `DEMO_READ_TOKEN`, tagged `tier=agent`, which her
own sessions ([the kinds of session](/legion/broker/concepts/#sessions-and-enrollment)) get without
asking, and `DEMO_API_KEY` and `DEMO_GITHUB_APP_HUMAN`, tagged `tier=human`, which she approves.
The last of those is a `github-app` secret whose `github-owners` tag names `acme-org`, and it is
the one GitHub App secret of this stack a request for reaches a person: a secret the registered
service owns is granted to that service's own sessions and denied to every other, and put to
nobody. Three more are the registered service's own, each tagged
`owner=legion-daemon tier=agent`: a pod session enrolled under that service gets each without
asking, and no person's session sees any of them at all.
They are `DEMO_SERVICE_TOKEN`, a stored value, and two `github-app` secrets minted at the GitHub
stand this stack starts: `DEMO_GITHUB_APP`, whose `github-owners` tag names `acme-org`, and
`DEMO_GITHUB_APP_CASE_PAIR`, whose tag names `ada`, on which that stand's App is installed twice
under logins that differ only in case (`ada` and `Ada`), so a release of it holds two
installations and `agent-secrets --github-token` refuses it. The exports name all three
`github-app` secrets, the stand's address, the route that reads back every call it was sent, and
the route that sets what it answers each token request from then on: `ordinary`, `refuse`,
`refuse-repository`, `refuse-permissions`, `widen`, or `fail-once`, which answers the next one
`503` and the rest as `ordinary`, so the broker's minter retries that one; `repository` names the
repository `refuse-repository` refuses. They also name the broker's metrics address
(`DEV_BROKER_METRICS_URL`), and the pod and
the registered service of the enrolment command the banner prints (`DEV_BROKER_POD_UID` and
`DEV_BROKER_SERVICE`), so a script enrols that same session from the exports alone. A grant lasts
for the `--ttl` its request names; with none, 15 minutes when any secret it names is human tier
and 12 hours otherwise
(until its session ends for a secret tagged `default-grant=session`), and never past its session. Every command below runs in that second shell.

## Run a helper and log it in

Start the helper in the background, with its log in the workdir, and log it in. It runs from this
shell rather than through `agent-secrets-helper install`, since installing would rewrite your own
helper's `systemd --user` unit to point at the development broker:

```sh
agent-secrets-helper serve 2>"$DEV_BROKER_DIR/helper.log" &
agent-secrets machine login
```

`machine login` prints a code and waits. In a third shell with the same exports, approve it as
Dispatch would:

```console
$ agent-secrets-devrelay machine-approve --code YE9L-5FM5
{"credential_id":"3bc28cd1-d04b-4481-92a8-ff10350fe795","grant_id":null,"state":"approved"}
```

and `machine login` exits 0. The helper's log, `$DEV_BROKER_DIR/helper.log`, says
`machine login issued; the helper holds a launcher credential`, and a helper you stop and start
again with the same exports resumes that login from `$AGENT_SECRETS_MACHINE_FILE`.

## Use secrets from a session

Start a shell as a registered session and use the secrets:

```console
$ agent-secrets register --wait 10 --exec -- bash
$ agent-secrets self
enrollment_id: 2b8627f4-9a2a-4c12-85bf-910144bc0b5c
kind: session
operator: ada@example.com
lease_expires_at: 2026-10-03T03:36:40Z
source: the broker AGENT_SECRETS_URL names (AWS Secrets Manager)
$ agent-secrets DEMO_READ_TOKEN -- printenv DEMO_READ_TOKEN
agent-secrets: DEMO_READ_TOKEN from the broker AGENT_SECRETS_URL names (AWS Secrets Manager)
demo-read-token-value
$ agent-secrets DEMO_API_KEY --reason "Deploy the example service" -- sh -c 'echo "deploying with a ${#DEMO_API_KEY}-character key"'
```

Every line about a secret names where it came from: the broker this stack runs, and the kind of
store that broker reads. The local stack's store is `BROKER_FAKE_SECRETS_FILE`, which stands in for
AWS Secrets Manager. No session's line names the broker's host or the store's account or region,
since an agent's output is often quoted; `agent-secrets secret show` names the account and region
its key ARN carries.

The last command waits for approval, and names who approves it and where (`waiting for
ada@example.com to approve it on the secrets broker: <broker>/approve/<record>`). Open that
address in a browser on this machine: the broker sends you to `dev-oidc-issuer`, which signs you
in as `ada@example.com` with no page to fill in, and brings you back to the request, where
**Approve** grants it ([approving a request](/legion/broker/guides/approve-a-request/#without-dispatch)
covers the page), and the command runs:

```console
deploying with a 18-character key
```

A request has two ids: the request id `agent-secrets` prints and takes, and the record id the
approval page's address ends in. `agent-secrets request NAME --json` prints both. **Deny** on the
same page denies the request, and `agent-secrets` then exits 77.

Each grant so far lasts as long as the default for a request with no `--ttl`: 15 minutes for
`DEMO_API_KEY`, which is human tier, and 12 hours for the agent-tier secrets (`agent-secrets self`
lists each expiry). A request may ask for another lifetime with `--ttl`, and the grant then ends on its own once that has
passed, while the session lives on:

```console
$ agent-secrets DEMO_READ_TOKEN --ttl 2m -- printenv DEMO_READ_TOKEN
agent-secrets: DEMO_READ_TOKEN from the broker AGENT_SECRETS_URL names (AWS Secrets Manager)
demo-read-token-value
$ agent-secrets self
enrollment_id: 7cf72ec3-d10f-46fd-984f-ddd856b229c2
kind: session
operator: ada@example.com
lease_expires_at: 2026-10-11T01:51:13Z
source: the broker AGENT_SECRETS_URL names (AWS Secrets Manager)
grant: f81251ab-6552-450c-9eca-12a793a29f4c (request 86237327-1f78-4c79-82a5-4f71174e71c5, expires 2026-10-11T01:38:14Z)
```

Two minutes later that grant releases nothing (`GRANT_NOT_LIVE`), `self` no longer lists it, and
the same command asks again. `--ttl` is a whole number of seconds, `2m` or `90s`; anything else
exits 2 before asking. When the broker or the machine's helper is from before `--ttl`, which would
grant for longer, `agent-secrets` ends the grant it got (or cancels the request still waiting),
says so, runs nothing and exits 1.

## List and revoke from your own shell

In the third shell, which is outside every session, list your machine logins and the live grants
as Dispatch's pages would, under the helper's machine login
([revoke a session or a grant](/legion/broker/guides/revoke-a-session/#revoke-a-grant-from-your-shell)):

```console
$ agent-secrets machine list
CREDENTIAL_ID                         HOST                 APPROVED_BY      ISSUED                EXPIRES               STATE
a0324632-0cc0-4113-bb26-d4123fab9da6  example-host-devbox  ada@example.com  2026-10-10T15:01:31Z  2026-10-17T15:01:31Z  ok
Every row is a login to the broker AGENT_SECRETS_URL names.
$ agent-secrets grant list
GRANT_ID                              SECRETS          GRANTED    APPROVER  DECIDED_BY  SESSION                                       OPERATOR         EXPIRES
8fe92a26-b355-4c28-8679-60e8ce98b1d9  DEMO_READ_TOKEN  automatic  -         -           session/example-host-devbox:2856922:35395441  ada@example.com  session-end
Every row is a grant from the broker AGENT_SECRETS_URL names (AWS Secrets Manager).
```

Each list says on standard error which broker and store its rows are from, so the table on
standard output stays one row per login or grant for a script.

`machine list` shows your own machines' logins alone; Dispatch's machine-login page also lists
every service's. `agent-secrets grant revoke <grant id>` ends a grant there, and
`agent-secrets machine revoke <credential id>` one of your logins. `grant revoke` run in a
session's shell is the session's own revoke, as `agent-secrets revoke` is: it ends only a grant
given to the broker identity (enrollment) the session signs as. Run there, `machine list`,
`machine revoke` and `grant list` are refused `IN_SESSION`, so the session's own commands cannot
act as you:

```console
$ agent-secrets machine list
agent-secrets machine list: IN_SESSION: pid 2857619 is inside a registered machine session, which acts on itself alone; run machine and grant commands from your own shell
```

The refusal covers the session's process tree only: a process the session sends out of it (with
`( cmd & )`, `setsid -f` or a tmux server it started) passes, and any process running as your user
can stop the helper. Through these commands such a process can list and revoke your own machine
logins, never a service's machine login, and your grants, which include every grant whose
approver is you — those you approved and those the model judge approved in your stead — and
every grant the judge approved in anyone's stead. `agent-secrets enroll --helper` is open to any
process of your user, so such a process can also enroll a box and read your agent secrets.

## Write a secret and see it served

The broker reads the fake secrets file again each time it lists the secrets, describes one or
reads a value, so an edit to the file is what a write to Secrets Manager is in production, where a
person writes with `agent-secrets secret` ([manage a secret](/legion/broker/guides/manage-a-secret/));
that CLI calls AWS itself, so it cannot write this file. Add a secret of `ada@example.com`'s to it,
here with `jq`:

```sh
jq '.secrets += [{"name": "example/agent-secrets/demo-new-token",
  "kms_key_id": "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab",
  "tags": {"owner": "ada@example.com", "tier": "agent"}, "value": "demo-new-token-value"}]' \
  "$DEV_BROKER_DIR/fake-secrets.json" >"$DEV_BROKER_DIR/fake-secrets.json.new"
mv "$DEV_BROKER_DIR/fake-secrets.json.new" "$DEV_BROKER_DIR/fake-secrets.json"
```

and ask the broker to reread it, as `agent-secrets secret` does right after each write
([Concepts](/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret)). The same call
answered `{"name":"DEMO_NEW_TOKEN","served":false,"reason":"absent"}` before the edit:

```console
$ curl -s -X POST "$AGENT_SECRETS_URL/v1/secrets/DEMO_NEW_TOKEN/reread"
{"name":"DEMO_NEW_TOKEN","served":true}
```

The registered session gets it at once:

```console
$ agent-secrets DEMO_NEW_TOKEN -- printenv DEMO_NEW_TOKEN
demo-new-token-value
```

Writing the file whole, as the `mv` does, keeps it readable throughout: while it does not parse,
each of those calls fails, naming the file.

The broker reads a type secret's value at the first load that lists it, so a value its minter
cannot mint from is refused before anyone asks for it. Here a copy of `DEMO_GITHUB_APP` whose value
has no `app_id` is added, and then read twice:

```console
$ jq '.secrets += [(.secrets[] | select(.name=="example/agent-secrets/demo-github-app") | .name="example/agent-secrets/demo-broken-app" | .value=(.value|fromjson|del(.app_id)|tojson))]' "$DEV_BROKER_DIR/fake-secrets.json" >"$DEV_BROKER_DIR/fake-secrets.json.new" && mv "$DEV_BROKER_DIR/fake-secrets.json.new" "$DEV_BROKER_DIR/fake-secrets.json"
$ curl -s -X POST "$AGENT_SECRETS_URL/v1/secrets/DEMO_BROKEN_APP/reread"
{"name":"DEMO_BROKEN_APP","served":false,"reason":"value-malformed"}
$ curl -s -X POST "$AGENT_SECRETS_URL/v1/secrets/DEMO_BROKEN_APP/reread"
{"name":"DEMO_BROKEN_APP","served":false,"reason":"value-malformed"}
```

The first reread wrote one line, `agent secret policy refused` with `reason` `value-malformed`, and
the second wrote none, since the secret's state did not change
([the value read](/legion/broker/operate/#the-value-read)):

```json
{"time":"2026-11-14T02:26:54.90436888Z","severity":"ERROR","message":"agent secret policy refused","name":"example/agent-secrets/demo-broken-app","reason":"value-malformed","owner":"legion-daemon","first_load":false}
```

## Exchange a pod's token

The broker's RFC 8693 token exchange is on, for one account entry,
`system:serviceaccount:example-ns:example-worker`, writing the project `EXAMPLE` for
`ada@example.com`, and for one read entry
([Exchange a read entry's token](#exchange-a-read-entrys-token)).
`$DEV_BROKER_DIR/exchange-token` holds the account entry's pod token for the exchange's own
audience, `legion-broker-exchange`.

That account's entry fixes no session name, so its pods name their own — and a pod writes under a
name its deployment registered for it and no other. The registration is a route of its own,
`POST /v1/oauth/exchange/pod-sessions`, which the deployment's daemon calls as the service
`BROKER_SERVICE_LOGINS` lets it log in as, here `legion-daemon`
(`$DEV_BROKER_DIR/registrar-token`); its body carries that pod's own token for the broker's
audience (`$DEV_BROKER_DIR/pod-token`, the token a daemon enrolls the pod with) and the name the
pod may write under. The script makes that call before it prints `ready.`, which is the
`registered the exchange pod to write under example-worker-session` line above. Making it again
extends the registration, which lasts 24 hours from each write, and the same call with another
name is how you add one. The body carries a token too, so it goes on standard input
(`--data @-`), and the header from a file curl reads (`-H @<(printf …)`, a bash process
substitution), which keeps both tokens out of every command line:

```console
$ printf '{"pod_token": "%s", "session": "example-worker-session"}' "$(cat "$DEV_BROKER_DIR/pod-token")" | curl -s -X POST -H @<(printf 'Authorization: Bearer %s' "$(cat "$DEV_BROKER_DIR/registrar-token")") -H 'Content-Type: application/json' --data @- "$AGENT_SECRETS_URL/v1/oauth/exchange/pod-sessions"
{"pod_uid":"87f7f873-65a4-47a7-b54d-abe68ac9db83","session":"example-worker-session","expires_at":"2026-10-31T00:20:03.764291Z"}
```

A `POST /v1/oauth/token` with `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`,
`client_id=dispatch-token-exchange`, that pod's exchange token as `subject_token`,
`subject_token_type=urn:envoy:params:oauth:token-type:kubernetes-service-account` and
`session=example-worker-session`, signed with a DPoP proof of a key of the caller's own, mints a
DPoP-bound access token. A `session` no registration names is refused, and the refusal names it.
Its description begins `pod_session_not_registered:`, which both clients read as a refusal to
trade again on, since a pod's trade can reach the broker before its registration does:

```console
{"error":"invalid_grant","error_description":"pod_session_not_registered: No registration says this pod may write under the session name 'other-session'. A pod writes under a session name its deployment registered for it.","answer_id":"ff2fd8523c22d74ed9dffcb333ea004a"}
```

Either listed caller reads a minted token back (`POST /v1/oauth/introspect` with the token in the
form, sent the same way). Here the `listener` token, which may do that and nothing else:

```console
$ printf 'token=%s' "<access token>" | curl -s -H @<(printf 'Authorization: Bearer %s' "$(cat "$DEV_BROKER_DIR/listener-token")") --data @- "$AGENT_SECRETS_URL/v1/oauth/introspect"
{"active":true,"session":"example-worker-session","person":"ada@example.com","project_scope":"named","projects":["EXAMPLE"],"exp":1793320811,"client_id":"dispatch-token-exchange","jkt":"oufCNV-MCX2NLGXXB5yj7Pdnwumhbp1dGcxoxpSTyWk"}
```

`POST /v1/oauth/revoke` (RFC 7009) ends one before its expiry, and is the only thing that can: an
exchanged token names no machine login for a credential revocation to reach. Its form names the
token, the client it was issued to and, optionally, which kind to look for first. Ending one of the
broker's tokens relays a decision, so it takes the caller token, and the `listener` token is
`403 ACCOUNT_LACKS_POWER` here as on every other such route. The answer is empty, and
introspection says `active: false` from the moment it commits:

```console
$ printf 'token=%s&token_type_hint=access_token&client_id=dispatch-token-exchange' "<access token>" | curl -s -H @<(printf 'Authorization: Bearer %s' "$(cat "$AGENT_SECRETS_ACCOUNT_TOKEN_FILE")") --data @- "$AGENT_SECRETS_URL/v1/oauth/revoke"
$ printf 'token=%s' "<access token>" | curl -s -H @<(printf 'Authorization: Bearer %s' "$(cat "$AGENT_SECRETS_ACCOUNT_TOKEN_FILE")") --data @- "$AGENT_SECRETS_URL/v1/oauth/introspect"
{"active":false}
```

A token the broker never issued is answered the same empty `200`, since the request's purpose —
that the token not work — holds either way. A token issued to another client than `client_id`
names is `400`:

```console
$ printf 'token=%s&client_id=agent-secrets-helper' "<access token>" | curl -s -H @<(printf 'Authorization: Bearer %s' "$(cat "$AGENT_SECRETS_ACCOUNT_TOKEN_FILE")") --data @- "$AGENT_SECRETS_URL/v1/oauth/revoke"
{"error":"unauthorized_client","error_description":"The client is not authorized to request a token using this method. Make sure that client id and secret are correctly specified and that the client exists.","answer_id":"1b8f254014120af2c5d4442558531dd7"}
```

Ending an access token ends that token alone: only a caller presenting a machine login's own
refresh token ends that login and every token of its family.

## Exchange a read entry's token

The exchange lists one read entry too (`BROKER_EXCHANGE_READ_ACCOUNTS`):
`system:serviceaccount:example-ns:example-reader`, with the service `example-reader` and the
session `example-reader-session`. `$DEV_BROKER_DIR/read-token` holds that account's pod token for
the exchange's audience. The token request above, with that token as `subject_token` and either
`session=example-reader-session` or no `session`, mints a token that holds the OAuth scope `read`
alone. Its introspection answer carries `"service":"example-reader"`, `"project_scope":"none"` and
`"scope":"read"`, and no `person` and no `projects`. A request that names another session is
refused, and one that names `scope=read` is refused `invalid_scope`: the entry decides the scope,
and the request never does. The entry fixes its session, so the script registers no pod for it,
and the registration route refuses one with `409 SESSION_FIXED`.

Dispatch serves that token on the routes its read list names, today
`GET /api/v1/projects/{key}/architecture` alone, and answers it `403 READ_ONLY_TOKEN` on every
other route of its route table and on `GET /ws/live`. `packages/dispatch/e2e/read-token-live.ts`
shows both against this stack. Leave the stack running. `bun run dispatch:local` runs until
Ctrl-C, so it takes a shell of its own: open a new one and paste the dev broker's exports into it
too, as you pasted them into the second shell. A shell without them expands `$AGENT_SECRETS_URL`
and `$AGENT_SECRETS_ACCOUNT_TOKEN_FILE` to nothing, and Dispatch then starts with no broker. In
that new shell, from `packages/dispatch`, start a local Dispatch that reads this broker (where
there is no Docker, also set `DATABASE_URL` to a Postgres database of its own):

```console
$ DISPATCH_E2E_AGENT_SECRETS_URL=$AGENT_SECRETS_URL DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE=$AGENT_SECRETS_ACCOUNT_TOKEN_FILE bun run dispatch:local
```

Its `Run a spec against it` line prints the variables a command reaches it by. Leave it running
there, and run the script in the second shell, which already holds the exports, from
`packages/dispatch`, with those variables and the broker's `AGENT_SECRETS_URL` and
`DEV_BROKER_DIR`:

```console
$ <the variables that line prints> AGENT_SECRETS_URL=$AGENT_SECRETS_URL DEV_BROKER_DIR=$DEV_BROKER_DIR bun e2e/read-token-live.ts
seeded EXAMPLE: architecture source example/arch, issue EXAMPLE-1
ok   GET /api/v1/projects/EXAMPLE/architecture [read-only token]: 200 - (want 200)
ok   POST /api/v1/issues/EXAMPLE-1/comments [read-only token]: 403 READ_ONLY_TOKEN (want 403 READ_ONLY_TOKEN)
ok   GET /api/v1/issues/EXAMPLE-1/comments [alice]: 0 comment(s) with this run's body (want 0)
ok   GET /api/v1/issues/EXAMPLE-1 [read-only token]: 403 READ_ONLY_TOKEN (want 403 READ_ONLY_TOKEN)
ok   POST /api/v1/issues/EXAMPLE-1/comments [named-project token]: 201 - (want 2xx)
```

The script creates the project `EXAMPLE`, an architecture source and an issue in that Dispatch. It
sends three requests with the read-only token, reads the issue's comments as alice, and then sends
the same comment with the account entry's token, which may write `EXAMPLE`. It exits 1 when any
answer is not the one it wants. Project `EXAMPLE` then exists, so a second run needs
`bun run dispatch:local` started again, which empties the instance.

## Read the log and the metrics

The broker writes its log to the first shell and to `$DEV_BROKER_DIR/broker.log`, one JSON object
per line, so `jq` finds a line by any of its fields. Each refusal writes one line,
`broker: answer refused`, but a `429` and the two answers of a device login's poll, which the
broker counts and does not log. The line names the answer id, which the answer carries in its
`X-Broker-Answer-Id` header and, in a JSON body, as `answer_id`. A reread of a name no secret can
carry is one such refusal, and its route takes no credential:

```console
$ curl -si -X POST "$AGENT_SECRETS_URL/v1/secrets/bad%20name/reread"
HTTP/1.1 400 Bad Request
Content-Type: application/json
X-Broker-Answer-Id: 852d8e549ba00de0a3e50f4e60644aa4
Date: Sat, 14 Nov 2026 02:26:38 GMT
Content-Length: 187

{"code":"SECRET_NAME_INVALID","error":"not an agent secret name: uppercase letters, digits and single underscores, starting with a letter","answer_id":"852d8e549ba00de0a3e50f4e60644aa4"}
$ jq -c 'select(.answer_id == "852d8e549ba00de0a3e50f4e60644aa4")' "$DEV_BROKER_DIR/broker.log"
{"time":"2026-11-14T02:26:38.530366315Z","severity":"WARNING","message":"broker: answer refused","answer_id":"852d8e549ba00de0a3e50f4e60644aa4","status":400,"code":"SECRET_NAME_INVALID","method":"POST","route":"POST /v1/secrets/{name}/reread","caller_class":"public","caller":"","secrets":["bad name"],"secrets_count":1}
```

`agent-secrets` ends the broker's refusal with the same id, as `(answer <id>)`, for whoever was
refused to quote ([Refusals and the answer id](/legion/broker/operate/#refusals-and-the-answer-id)).

`$DEV_BROKER_METRICS_URL` is the broker's metrics port ([Metrics](/legion/broker/operate/#metrics)).
This scrape was taken after several of this page's calls, `DEMO_BROKEN_APP` above among them, and
after one release of `DEMO_GITHUB_APP` to the pod session the banner's enrolment command makes
(its key directory in `$KEYS`), with the GitHub stand set to `fail-once` first:

```console
$ curl -s -X POST -d '{"mode": "fail-once"}' "$DEV_BROKER_GITHUB_ANSWER_URL"
{"mode":"fail-once","repository":""}
$ AGENT_SECRETS_KEY_DIR=$KEYS agent-secrets DEMO_GITHUB_APP --github-owner acme-org --github-token -- sh -c 'printf "%s characters, starting %s\n" "${#DEMO_GITHUB_APP}" "${DEMO_GITHUB_APP%%_*}"'
agent-secrets: DEMO_GITHUB_APP from the broker AGENT_SECRETS_URL names (AWS Secrets Manager)
22 characters, starting ghs
$ curl -s "$DEV_BROKER_METRICS_URL" | grep -v '^#'
broker_policy_store_reads_total{read="describe"} 6
broker_policy_store_reads_total{read="list"} 6
broker_policy_store_reads_total{read="value"} 3
broker_mint_attempts_total{type="github-app",outcome="ok",attempt="1"} 1
broker_mint_attempts_total{type="github-app",outcome="ok",attempt="2"} 1
broker_mint_attempts_total{type="github-app",outcome="upstream",attempt="1"} 1
broker_answers_total{outcome="refused",code="ACCOUNT_LACKS_POWER",caller_class="unauthenticated"} 1
broker_answers_total{outcome="refused",code="SECRET_NAME_INVALID",caller_class="public"} 1
broker_answers_total{outcome="refused",code="UNKNOWN_SECRET",caller_class="session"} 2
broker_answers_total{outcome="refused",code="invalid_grant",caller_class="public"} 1
broker_answers_total{outcome="refused",code="unauthorized_client",caller_class="account"} 1
broker_answers_total{outcome="served",code="HTTP_200",caller_class="public"} 8
broker_answers_total{outcome="served",code="HTTP_200",caller_class="service"} 2
broker_answers_total{outcome="served",code="HTTP_200",caller_class="session"} 2
broker_answers_total{outcome="served",code="HTTP_201",caller_class="launcher"} 1
broker_unservable_secrets{secret="example/agent-secrets/demo-broken-app",reason="value-malformed",owner="legion-daemon"} 1
```

The release made two calls to the stand. Its listing of the App's installations took one attempt,
and its token request two: the first answered `503`, `outcome="upstream"`, and the broker's second
attempt minted. `broker_unservable_secrets` holds the one secret the broker refuses now. It drops
that secret at the first scrape after a reread or a load reads a new version of its value, unless
the decoder refuses that value too. After an edit of the fake secrets file, that is the next
reread of the name (`curl -s -X POST "$AGENT_SECRETS_URL/v1/secrets/<NAME>/reread"`) or the
five-minute reload.

## Reach a secret behind the airlock

`packages/envoy/scripts/dev-airlock.sh` starts the whole airlock, in a stack of its own. It runs
`dev-broker.sh` first, for Postgres, `dev-oidc-issuer` and the binaries, and then adds what the
airlock needs: `cmd/dev-airlock-stands`, which serves the stand-in services and records every
request each one is sent; a rules file (`BROKER_AIRLOCK_RULES_FILE`) and a fake secrets file that
name the same secrets; two broker processes on one database, each with an API port and a proxy
port of its own, behind one TCP front over both API ports and one over both proxy ports; one
enrolled pod session, whose key directory the exports name; and the model judge on both brokers
([let the judge decide a request](#let-the-judge-decide-a-request)). Run it in place of
`dev-broker.sh`, from `packages/envoy`, and set the same `DEV_BROKER_POSTGRES_URL` where you have
no Docker. Each bound host listens on port 443 of a loopback address of its own (127.0.0.2 and
up), so the machine must let this user bind 443, and one such stack runs at a time.

This run used the same Postgres server as the first section. The lines `dev-broker.sh` prints are
the ones that section shows, and are cut here:

```console
$ DEV_BROKER_POSTGRES_URL='postgres://<user>@<host>:<port>/<database>' bash scripts/dev-airlock.sh
dev-airlock: workdir /tmp/agent-secrets-airlock.eGQ8I8
dev-airlock: starting dev-broker.sh (Postgres, dev-oidc-issuer, its broker)...
...
dev-broker: ready.
...
dev-airlock: created isolated database dev_airlock_egq8i8
dev-airlock: starting dev-airlock-stands...
dev-airlock-stands: stands listening records=http://127.0.0.1:16053
dev-airlock: starting the API and proxy fronts...
dev-airlock-stands: front listening addr=127.0.0.1:49793
dev-airlock-stands: front listening addr=127.0.0.1:32100
dev-airlock: starting broker 1 of 2...
dev-airlock: starting broker 2 of 2...
...
dev-airlock: enrolling a pod session...
dev-airlock: enrolled pod session 43353ee9-a37a-4531-b082-0b247a9d1662 (pod e9d053df-9faf-4b33-82fc-0b069708961d, service legion-daemon)

dev-airlock: ready.

  export AGENT_SECRETS_URL=http://127.0.0.1:49793
  export AGENT_SECRETS_KEY_DIR=/tmp/agent-secrets-airlock.eGQ8I8/session-key
  export AIRLOCK_PROXY_URL=http://127.0.0.1:32100
  export AIRLOCK_RULES_FILE=/tmp/agent-secrets-airlock.eGQ8I8/airlock.json
  export AIRLOCK_FAKE_SECRETS_FILE=/tmp/agent-secrets-airlock.eGQ8I8/fake-secrets.json
  export AIRLOCK_S_URL=https://s.airlock.test
  export AIRLOCK_S_RECORDS=http://127.0.0.1:16053/records/s
...
  broker 1:            api 127.0.0.1:34621, proxy 127.0.0.1:62559, pid 46403, log /tmp/agent-secrets-airlock.eGQ8I8/broker1.log
  broker 2:            api 127.0.0.1:27522, proxy 127.0.0.1:20097, pid 46404, log /tmp/agent-secrets-airlock.eGQ8I8/broker2.log
  secrets:             AIRLOCK_DEMO_TOKEN and AIRLOCK_SCOPED_TOKEN (stored, bound to s.airlock.test; AIRLOCK_SCOPED_TOKEN in
                       authorization and x-key, GET /read alone), AIRLOCK_GITHUB_APP (github-app,
                       installation 7 on acme-org), AIRLOCK_GOOGLE_LOGIN (google-login),
                       AIRLOCK_AZURE_LOGIN (kind azure, human tier) and AIRLOCK_AWS_KEY (kind aws)
...
```

Paste these exports into a second shell, as above, and check that `type agent-secrets` names the
stack's `bin/`. The rules file binds `AIRLOCK_DEMO_TOKEN` to the stand-in service S,
`s.airlock.test`, and names no headers for it, so its value travels in `authorization`, the one
header an entry with none takes. The command gets a placeholder of 58 characters in place of the
value, and its `HTTPS_PROXY` names the proxy front with that grant's proxy password, so the
command's call to S goes through the broker:

```console
$ agent-secrets AIRLOCK_DEMO_TOKEN --reason 'read the example service behind the airlock' -- sh -c 'printf "AIRLOCK_DEMO_TOKEN: %s characters, starting %s\n" "${#AIRLOCK_DEMO_TOKEN}" "${AIRLOCK_DEMO_TOKEN%%-*}"; printf "Authorization: Bearer %s" "$AIRLOCK_DEMO_TOKEN" | curl -sS https://s.airlock.test/read -H @-'
agent-secrets: AIRLOCK_DEMO_TOKEN from the broker AGENT_SECRETS_URL names (AWS Secrets Manager)
AIRLOCK_DEMO_TOKEN: 58 characters, starting agent
the s stand-in answered
```

What S was sent is at its recording route, `$AIRLOCK_S_RECORDS`. The header it read is not the
placeholder the command held: it is the value the fake secrets file holds for that secret, which
the proxy swapped in. The two SHA-256 digests below are of the whole `Authorization` header S
recorded and of `Bearer ` and that stored value, and they are the same. Neither command prints
either string:

```console
$ curl -sS "$AIRLOCK_S_RECORDS" | jq -r '.[-1] | .method + " " + .path + " host=" + .host'
GET /read host=s.airlock.test
$ curl -sS "$AIRLOCK_S_RECORDS" | jq -j '.[-1].header.Authorization[0]' | sha256sum
a2ecdfa9af6567f28055293e907aaec873a545fa2b0ee30c14e8e2a7b27aa149  -
$ jq -j '.secrets[] | select(.name | endswith("airlock-demo-token")) | "Bearer " + .value' "$AIRLOCK_FAKE_SECRETS_FILE" | sha256sum
a2ecdfa9af6567f28055293e907aaec873a545fa2b0ee30c14e8e2a7b27aa149  -
```

The rules file's second entry, `AIRLOCK_SCOPED_TOKEN`, is bound to the same host for `GET /read`
alone, so the proxy refuses a `POST /write` with it, names the rule that refused, and ends the
refusal with its answer id:

```console
$ agent-secrets AIRLOCK_SCOPED_TOKEN --reason 'a path its rule does not allow' -- sh -c 'printf "Authorization: Bearer %s" "$AIRLOCK_SCOPED_TOKEN" | curl -sS -o /dev/stdout -w " (%{http_code})\n" -X POST https://s.airlock.test/write -H @-'
agent-secrets: AIRLOCK_SCOPED_TOKEN from the broker AGENT_SECRETS_URL names (AWS Secrets Manager)
airlock: not allowed by rule (answer 920e65ea84ffcaae7eb60c224f5d340b)
 (403)
```

Each of those two calls wrote one line in the log of whichever broker the proxy front sent it to
(`$AIRLOCK_DIR/broker1.log` and `broker2.log`). The call the proxy carried wrote
`airlock proxy: a call to a bound host`, with its answer id, the enrollment, the grant, the
secret, the issue its reason named, the host, the method, the path, the answer's status and
`allowed` `true`. The refused call wrote the refusal's line, `broker: answer refused`, in place of
a call line, with the answer id its text ended with, the code `REQUEST_NOT_ALLOWED`, the caller as
`<grant>@<enrollment>`, the secret, the host, the path and the reason
([Refusals and the answer id](/legion/broker/operate/#refusals-and-the-answer-id)). This run's two
lines:

```json
{"time":"2026-11-14T02:30:23.932111149Z","severity":"INFO","message":"airlock proxy: a call to a bound host","answer_id":"d7550e31e79f8c6ab5d43d414cec83d1","enrollment":"cd6d7cba-56a7-41e9-b251-de51d4593c3c","grant":"11db25ae-3e7e-40fa-8fbb-aabff18d1d75","secret":"AIRLOCK_DEMO_TOKEN","issue":"","host":"s.airlock.test","method":"GET","path":"/read","status":200,"allowed":true}
{"time":"2026-11-14T02:30:24.097750394Z","severity":"WARNING","message":"broker: answer refused","answer_id":"920e65ea84ffcaae7eb60c224f5d340b","status":403,"code":"REQUEST_NOT_ALLOWED","method":"POST","route":"","caller_class":"airlock","caller":"8c6d834c-9f0d-423a-97c8-319585e6e022@cd6d7cba-56a7-41e9-b251-de51d4593c3c","secrets":["AIRLOCK_SCOPED_TOKEN"],"secrets_count":1,"host":"s.airlock.test","reason":"not allowed by rule","path":"/write","issue":""}
```

No line of either holds a value or a placeholder. Ctrl-C in the first shell stops every process
this script started, drops its database and prints the workdir it kept.

## Let the judge decide a request

The stack of the first section runs no model judge. The airlock stack runs one, on both broker
processes. Three more stand-ins of `cmd/dev-airlock-stands` serve it: a model endpoint, a
Dispatch holding one open issue and one done one, and a Compute Engine metadata server, where
the broker mints the identity token it presents to the model. Three human-tier secrets tell the
judge's class from everything outside it:

| Secret | Its rules entry and owner | A request for it |
| --- | --- | --- |
| `AIRLOCK_JUDGED_TOKEN` | `judge: 900`, owned by `ada@example.com` | The judge decides it. |
| `AIRLOCK_SHARED_TOKEN` | `judge: 900`, `owner=shared` | The judge decides it, in the place of anyone who signs in. |
| `AIRLOCK_UNJUDGED_TOKEN` | no `judge`, owned by `ada@example.com` | It waits for her, as every human-tier request does without a judge. |

Ask for one with a lifetime inside that entry's seconds and a reason naming the Dispatch stand's
open issue. The exports name the secret and the issue:

```sh
agent-secrets request "$AIRLOCK_JUDGED_SECRET" --ttl 10m \
  --reason "read the example service for $AIRLOCK_DISPATCH_OPEN_ISSUE" --wait 60s
```

`agent-secrets status <request id>` then names the judge as the decider,
`decided_by: judge:$AIRLOCK_JUDGE_MODEL`, and the broker that decided it logs one line,
`broker judge: wrote a verdict`, with the verdict, the model's reason and, where a check stopped
the run, the check that failed. The same verdict is one `request.judged` row in the `audit` table
([Concepts](/legion/broker/concepts/#the-model-judge)). What the model was sent is at its
recording route, `$AIRLOCK_MODEL_RECORDS`, and what Dispatch was asked at
`$AIRLOCK_DISPATCH_RECORDS`, which holds two GETs for each run that reached the check, and no
write at all.

The model stand approves until something tells it otherwise. A `POST` to
`$AIRLOCK_MODEL_ANSWER_URL` sets what it answers from then on:
`{"verdict": "refer", "reason": "…"}` for the other verdict, `{"text": "…"}` for a reply that is
no such object, and `{"delay": "30s"}` for an answer that never arrives inside the judge's 20
seconds. Each of those leaves the request pending for `ada@example.com`, which is what the judge
does with everything it does not approve.

Starting the stack with `--judge-skip-checks` sets `BROKER_JUDGE_TEST_SKIP_CHECKS`: the judge
still makes every check and records each, and none of them holds a request back, so the model is
asked about a request a check would have stopped. It is the negative control of those checks, and
a broker refuses it on a production start. `packages/envoy/scripts/judge-check.sh --help` lists
the checks that script drives end to end, each on a stack of its own, printing what it observed
beside what the check expected.

## Stop

1. In the first shell, press Ctrl-C: the script stops the broker and `dev-oidc-issuer`, drops its
   database, and prints the workdir it kept. A token the issuer minted verifies nowhere else, and no
   longer verifies once it stops.
2. In the second shell, leave the registered session (`exit`), then stop the helper: `kill %1`.
3. Remove the workdir, which holds the binaries, the fake secrets, the token files and the logs:
   `rm -rf "$DEV_BROKER_DIR"`.

## What is real and what is not

The broker, its database, its policy, its approval page and every client are the real ones. Four
things stand in for production: `agent-secrets-devrelay` plays Dispatch's machine-login relay
alone (it presents the caller token and names the decider itself; it cannot decide a secret
request, and neither can Dispatch); `dev-oidc-issuer` plays both the cluster's ServiceAccount
token issuer and the Workspace a person signs in to the broker at (the Go tests' own local issuer,
`internal/oidc/oidctest`, served on a loopback port: it publishes a key it holds in memory, mints
the stack's six token files, one pod uid per service account, so the exchange pod's two — one for
each audience — are one pod, and signs every authorization in as `ada@example.com` with no page to
fill in); the pod-session registration the script makes before it is ready stands in for the
deployment's own daemon, which makes that call as it starts a pod; and the fake secrets file
(`BROKER_FAKE_SECRETS_FILE`: each secret's name, key, tags and value, as JSON) plays Secrets
Manager and KMS. The broker reads that file again on every call it makes to it, so editing it is
how you write a secret here.

The airlock's proxy, its rules, its decision and its swap are the real ones too, and so is each
broker process the airlock stack runs. Five more things stand in there. `cmd/dev-airlock-stands`
plays every host the rules bind: S and T, which answer one line and record what they were sent,
the GitHub and Google endpoints a `github-app` and a `google-login` secret mint at, the Microsoft
token endpoint the `azure` kind exchanges at, which also plays the resource host that token is
spent at, and an AWS service that verifies Signature Version 4. Each is a `.test` name no
resolver answers, which `BROKER_AIRLOCK_TEST_RESOLVE` and `BROKER_AIRLOCK_TEST_DIAL_ALLOW` point
at one loopback address each, so the proxy reaches those five and refuses every other private
address, as it does in production. The judge's own run — its six checks, its one model call and
what it writes — is real as well, and three more of that command's stands serve it, on loopback
addresses the broker reaches directly rather than through the proxy: a model endpoint that
answers whatever was last set for it, a Dispatch holding one open issue and one done one, and a
metadata server that mints the broker's identity token. The rules file the script writes stands
in for the deployment's own, which lives in the deployment's repository. The proxy's certificate
authority is made at each start, where a deployment keeps it in a Kubernetes Secret both broker
pods mount. The two brokers behind one TCP front stand in for two pods behind a Service, so a
call reaches either process. And the pod session the script enrols stands in for the Legion
daemon's enrollment of a worker pod, which is why `agent-secrets self` answers `kind: pod` there.
