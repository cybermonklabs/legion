---
title: Concepts
description: Sessions and enrollment, machine logins, owner and tier, grants and their lifetime, approvals, and the audit record.
sidebar:
  order: 2
---

This page explains the ideas the rest of the broker's documentation leans on. Each section names
the code that implements it, so you can check the page against the source. A `BROKER_*` name is one
of the broker's settings; the [configuration reference](/legion/broker/reference/config/), generated
from the loader, gives each one's default and accepted values.

## Sessions and enrollment

An **agent session** is one running agent and everything it starts. On a machine, it is the
process `agent-secrets register --exec` starts (a coding agent, or a shell) and every process below
it; in a container or a Kubernetes pod, it is whatever holds the session's key directory. The
broker never deals with a person's or a machine's long-lived key on the agent's behalf. Instead,
every session has a P-256 signing key of its own that lives only as long as the session does, and
an **enrollment** that binds that key to the broker.

Every call a session makes carries a `Proof` header: a JSON Web Signature over the call's method
and URL, signed with the session's key, used once, and valid only within
`BROKER_PROOF_SKEW_SECONDS` of the broker's clock. There is no bearer token for a session to leak.

A **launcher** is the program that enrolls sessions: `agent-secrets-helper`, a per-user daemon on
a machine, for that machine's sessions, the Legion daemon for the pods it runs, or a Kubernetes
pod for itself. An enrollment has one of two kinds:

| Kind | What it is | Who holds its key | Who enrolls it |
| --- | --- | --- | --- |
| `session` | An agent session on a machine a person logged in: running directly on the machine, or in a container on it (a **box**). | For a session on the machine, the machine's helper, in memory; for a box, the container, in `key.pem` under `AGENT_SECRETS_KEY_DIR`. | The machine's helper: when the session registers with `agent-secrets register`, or, for a box, when whoever starts the container runs `agent-secrets enroll --helper` ([run an agent in a container](/legion/broker/guides/run-an-agent-in-a-container/)). |
| `pod` | A Kubernetes pod: a Legion worker pod, or one an operator launches by hand. | The pod, in its key directory. | The Legion daemon, which proves the pod with a projected service-account token the broker verifies against `BROKER_K8S_OIDC_ISSUER`; or the pod itself, with `agent-secrets enroll --pod`, when its ServiceAccount is one `BROKER_SERVICE_LOGINS` names. An enrollment's runtime id must be the pod UID of the token presented with it, which the broker verifies against the cluster's issuer and refuses otherwise, so a pod enrolling itself enrols the pod whose token it holds. |

A machine holding a Google identity token enrols a `session` too: it trades that token at the
broker's token exchange, which answers a Dispatch access token and the machine login that trade
minted, and the machine enrols with that login. That login names the machine's fleet and no
person, so the session acts for the fleet's service: it reads the secrets that service owns and
the shared agent-tier ones, and no person's. The person a fleet's
`BROKER_VM_EXCHANGE_ACCOUNTS` entry names reaches the Dispatch token and no secret, and is the
person whose Inbox lists a request the machine makes for a shared human-tier secret
([Approvals](#approvals)); that person reaches their own secrets on the machine only by signing in
on it. A fleet whose account `BROKER_VM_EXCHANGE_JOB_ACCOUNTS` lists is a job: its Dispatch token
names the account as its service, no person, and the job's projects, and its machine login still
names the fleet's service.

A person signs in on a machine with `agent-secrets login`, which runs Google's device flow and then
trades the person's own identity token at the exchange with the machine's as the actor token. That
trade mints a machine login of its own, carrying the person rather than the fleet, so a session
enrolled under it is that person's: it reads the secrets that person owns and the shared agent-tier
ones, and no other person's and none of the fleet's. Each sign-in is bound to a box key the command
makes there, so one person signed in on two machines holds a login on each, and two people signed
in on one machine hold one each. Revoking a sign-in on Dispatch ends its machine login with it:
every session it enrolled ends, and the box signs in again to work on.

A Kubernetes pod trades its own projected ServiceAccount token at the same exchange for a Dispatch
access token alone: that trade mints no machine login and enrols nothing. The entry that lists the
pod's account decides what the token is. An entry of `BROKER_EXCHANGE_ACCOUNTS` gives it a person
and the one Dispatch project its token names, or no project for an entry that names none
(`<service account>==<person>`), or neither a person nor a project for an entry that names
neither (`<service account>==`): such a token acts as the account itself, names the account as its
service, and Dispatch records its writes under that account. An entry of that setting that names a
project names its person. An entry of that setting whose fixed session is
`legion-daemon:<its own project>` is a Legion daemon's own, and its token holds the OAuth scope
`legion-status` too. Dispatch reads that scope (`DISPATCH_LEGION_STATUS_SCOPE`): where that setting
is on, a write of a Legion issue's status needs it or the `person` scope, but for a status write of
the architect of an issue above, which writes with its own broker token. No other token the exchange
mints holds `legion-status`, whatever session it carries. A job entry, of
`BROKER_EXCHANGE_JOB_ACCOUNTS`, gives the pod's token its projects and no person: the token names
the account as its service and holds no OAuth scope, so a route that needs one refuses it before
Dispatch's project check runs. For an entry that names one project, that check judges the token as
it judges a person's named token for that project; for an entry that names `public`, the token
reaches every public project and the private projects the entry lists. That list is the broker's
grant: at this release Dispatch does not read a `public` token's list, so such a token writes
wherever a token that names no project may. A read entry, of `BROKER_EXCHANGE_READ_ACCOUNTS`, gives
the pod's token a service and a fixed session instead, and no person and no project: the token holds
the OAuth scope `read` alone. Dispatch serves such a token on the GET routes it classes as reads,
and refuses it on every other route with `403 READ_ONLY_TOKEN`. The Envoy listener refuses it with
`401`, and the
NATS auth service refuses a connection that presents it as `not_a_session`
([Operate](/legion/broker/operate/#what-to-run)). That token reads every read of Dispatch that
needs no OAuth scope, for every project: each issue, closed and iceboxed ones included, with its
documents, comments and asks. No setting holds a read entry to some projects.

Any trade but a job entry's may also ask for the OAuth scope `read` (`scope=read`), whatever its
subject. That includes a pod's token under any other entry, an account entry that names no person
among them, whose read token names its account as the service, a workflow run's token, a
machine's Google identity, a person's sign-in and a machine login. A job entry's trade that asks
for `read` is refused `invalid_request`: a job's token is minted whole, with no OAuth scope. The token keeps the session, the person or the service, and
the projects that the same trade would have named, and it writes nothing. Its project scope is
the narrowed word of the one it would have had: `read-named`, `read-person` or `read-none`. It
holds `read`, and `person` beside it when it acts for a person, never `approve` and never
`legion-status`, so a Legion daemon's read token writes no status either. The trade may
also name a cut (`as_of`), the past time the token reads at: an RFC 3339 time no later than the
trade. Until Dispatch serves reads at a past time, it refuses a token with a cut on every read but
`GET /api/v1/whoami`, with `403 AS_OF_TOKEN_NOT_SERVED`. A trade that asks for no scope gets the
token it always got (`packages/envoy/internal/cipher/oauth/exchange.go`, `decide`).

A read trade makes, moves and extends no launcher credential, and its answer names none. A
person's read trade still proves the sign-in with the box key. Its token names the launcher
credential the sign-in already holds, so a revoke of the sign-in ends the token. A sign-in that
holds no credential yet is refused `invalid_grant`, which names `agent-secrets login`. A read
trade does not renew the sign-in's credential either. Once that credential lapses, read trades
are refused `invalid_grant` until one ordinary trade of the sign-in renews it, as `agent-secrets`
makes, and the read tokens that named it end with it. A narrowed token lives no longer than its
subject token or the credential it names, and a revoke of either ends it. A token that
holds `read`, a read entry's included, is never the subject of a trade.

No key is both a read token's key and a launcher credential's key. A launcher credential's key
signs launcher proofs, which enrol sessions and revoke a person's machines and grants, and a read
token is meant for a holder that only reads. So the broker refuses to bind a read token to a key
that a live launcher credential is bound to, with `invalid_request` that names the cure. It also
refuses to mint or move a launcher credential onto a key that a live read token is bound to, as it
refuses a key that another live credential holds. A launcher proof signed with a read token's key
is refused `401 LAUNCHER_INVALID`. A read trade whose DPoP proof is signed with a launcher
credential's key, such as a person's box key, the key in a machine login's `machine.json` or a
device login's key, sends a second proof of the same request in the `DPoP-Holder` header. That
proof is signed with a key made for the token's holder, and the broker binds the token to that
key. A person's read trade must always send one. This departs from RFC 9449, which binds a token
to the key of the request's DPoP proof (`packages/envoy/internal/cipher/oauthstore/store.go`,
`CreateAccessTokenSession` and `KeyHoldsReadToken`).

A GitHub Actions run trades its own token at the exchange the same way, with
`AGENT_SECRETS_MACHINE_IDENTITY=github-actions`, and that trade mints no machine login either. An
entry of `BROKER_EXCHANGE_WORKFLOWS` gives the run a person and a project, and a job entry of
`BROKER_EXCHANGE_JOB_WORKFLOWS` gives it its projects and no person, as for a pod. A job's token,
of either kind, holds no OAuth scope, and Dispatch records its writes under its session and its
service and under no person.

An enrollment is leased for `BROKER_LEASE_SECONDS` and renewed while its session runs: the helper
renews the sessions it registered, `agent-secrets renew` renews a box, and Legion's pod shim renews
a pod. A pod's renewal carries the pod's current projected token, and the broker refuses it
(`POD_IDENTITY_MISMATCH`) unless the token names the pod UID and service account the enrollment was
made for. The token is not yet required. The broker ratchets each pod UID instead: once any
enrollment naming a pod UID, live or ended, has renewed with that pod's token, the broker refuses a
renewal without one (`POD_TOKEN_REQUIRED`) from every enrollment naming that pod, whichever machine
login made it.
So the copied-token gap closes for every pod UID a current-image pod has renewed under: no
enrollment naming that pod renews without its token, and a token copied out of the pod stops
renewing anything once it expires. It stays open for the UID of a pod on an older worker image,
whose `agent-secrets renew` never sends a token: such a pod renews as it did before, the broker
logging each such renewal, until the release that requires the token. One window is left even for a
current-image pod: a copied token used to renew before the pod's own first renewal with its token.
That is seconds while the Legion daemon holds an approved machine login, but after a daemon restart
its pods enroll only once a person approves the daemon's new login, and a pod that ends before it
enrolls never renews with its token, so its UID stays open as an old-image pod's does. A grant lasts
until its session ends, so a copy that keeps renewing keeps the grant one approval gave it.

A machine's own session is held to its machine login as well: the broker reads that login on every
renewal, and refuses one whose login has ended — revoked, past the hour's expiry its Google
identity token gave it, or its account no longer listed for its service in `BROKER_VM_SERVICES`.
So a box dropped from its fleet's settings, and one that stops being able to prove its identity,
lose their sessions within one `BROKER_LEASE_SECONDS`: the renewal is refused
`ENROLLMENT_NOT_LIVE`, the broker logs `broker: machine session renewal refused: its machine login
has ended`, and the sweep ends the enrollment at its lease. A copy of such a session's key and
enrollment id taken off the box renews nothing once that box's login has ended. Only a machine's
own session is held this way: a pod's renewal proves the pod's own projected token, and a person's
session on such a box runs until they revoke the sign-in, which ends it at once.

An enrollment has **ended** once its launcher revokes it (the helper does
as soon as a registered session's process exits), whoever may revoke its machine login revokes that
login
([end a machine's login](/legion/broker/guides/revoke-a-session/#end-a-machines-login)), or its
lease lapses. A `pod` enrollment's launcher is the one machine login that made it: another login,
even another login of the same service, such as the Legion daemon's after a restart or after its
login reached its expiry and it logged in again, is refused
`NOT_ENROLLING_CREDENTIAL`, and that pod's enrollment ends with its lease or with that machine
login. From the moment the lease lapses, its next renewal is refused
`ENROLLMENT_NOT_LIVE`; other session calls remain `PROOF_INVALID`. The broker's sweep, which runs
every `BROKER_SWEEP_SECONDS`, then ends the enrollment on its first run after the lapse.
What an end does to what the enrollment still holds depends on who ended it.
An end a person makes, revoking the machine login or their sign-in on a box, revokes every grant
the enrollment held and cancels every request it still had pending, so those leave the approver's
Inbox and an approval can never land on a session that is gone.
An end of the box leaves both where they are: when the enrollment names a session, records the
principal that session belongs to, and still holds a live grant or a pending request, the
launcher's own revoke and a lapsed lease mark the row revoked and **dormant**, revoking nothing
(`packages/envoy/internal/cipher/enroll/enroll.go`, `endEnrollment`).
The row has to record the principal because only the pair, the session name and the principal, is
ever adopted: a row that records no principal can be taken by no later enrollment, so it ends as
every end did before the carry, its grants revoked and its pending requests cancelled
(`enroll.go`, `adopt`).
Every enrollment made before the broker recorded principals has none, among them every enrollment
live on the day the carry ships, so each of those ends as it always did and the enrollment that
replaces it carries.
A dormant enrollment proves nothing from the moment it goes dormant, as any ended enrollment proves
nothing, and its next renewal is refused `ENROLLMENT_NOT_LIVE`; what it holds waits for the next
enrollment of that session name under the same principal, or under a service account paired with
it whose pod a login of the same account enrols ([Requests and grants](#requests-and-grants)), to
take.
An approval given while the box is gone still mints a grant: the broker hangs it off the row the
carry is keeping for that session, and the session's next enrollment takes it with the rest
(`packages/envoy/internal/cipher/requests/machine_state.go`, `ApplyDecision`).
A box is replaced far more often than the session that runs on it, so an end of the box is no end of
the session.
[Requests and grants](#requests-and-grants) says what that next enrollment takes, and what ends a
grant no enrollment comes back for.

Every `session` enrollment records an **operator**: the person whose machine it runs on. The
operator is the person who approved the machine login that enrolled it, never a value the launcher
chooses. A `pod` enrollment records the service account the pod's projected token proved. It
records no operator when a service's login enrolled it, since the Legion daemon logs in as a
service with its own Kubernetes token rather than as a person. A pod whose account
`BROKER_POD_OPERATORS` maps to a person records that person as its operator as well
([a person's pod](#a-persons-pod)).

### When no broker is serving

A lease that runs out while no broker is serving is not ended. A session renews through a
broker, so while every broker process is down, or none can reach the database, no session can
renew. Every serving broker records a beat every `BROKER_SWEEP_SECONDS`, and a gap of more than
three sweeps between beats counts as time no broker was serving. A lease that was still running
at the last beat stays live through that gap, so the sweep does not end it. The first beat after
the gap gives the gap back to every such lease that was not revoked, at most
`BROKER_LEASE_SECONDS` from then, and the broker logs
`broker: gave back the time no broker was serving …`
(`packages/envoy/internal/cipher/enroll/serving.go`, `ServingClock`). The session's renewer then
renews as before. A revoked enrollment stays ended, and so does one whose lease ran out while a
broker was serving. A renewal with another pod's or service account's token is still refused
`POD_IDENTITY_MISMATCH`. A gap of three sweeps is given back to nobody, so a lease has to
outlive one and then the retries its renewer takes to renew once a broker serves again. The
broker refuses to start on a `BROKER_LEASE_SECONDS` too short for its `BROKER_SWEEP_SECONDS`,
and names the shortest lease that is long enough: 91 seconds at a sweep of 1 or 5 seconds, 105
at a sweep of 10, 225 at 30 and 360 at 60.
It walks both renewers' own retry schedules
(`packages/envoy/internal/cipher/leasetime`), counting each renewal the outage refuses as
failing at whatever instant costs the renewer most: anywhere from at once, when the connection
is refused, to the 30-second client timeout, when nothing behind a front door ever answers,
with a proxy that gives up or a terminating pod that resets ending one between the two.
Each renewer waits from the instant its failure comes back, so that instant is what decides
where its next renewal falls.
Which renewer sets the floor changes with the sweep: `agent-secrets renew`'s floor grows steadily
with the sweep, while `agent-secrets-helper`, which waits at least 30 seconds after any failed
renewal, stays at 91 seconds up to a 10-second sweep, where an outage can refuse only its first
renewal, and rises faster after that. So `agent-secrets renew` is the stricter of the two only
at sweeps of 7 to 10 seconds, and the helper at every other.
The default lease
of 900 seconds passes at every sweep the broker takes.
A deployment whose `BROKER_LEASE_SECONDS` is below its sweep's floor has to raise it before it
rolls a broker carrying this refusal, since each new pod exits at boot instead of serving.
This covers only an outage of the broker
itself:

- A broker that runs but that sessions cannot reach (ingress, DNS, a NetworkPolicy, a stale
  `AGENT_SECRETS_URL`) still counts as serving, so a lease that runs out then ends as before.
- A grant's lifetime and a pending request's expiry still run on wall-clock time.
- The first deploy of a broker with this behavior restores nothing that ran out before it.
- A pod whose renewer already exited 76 has no renewer until the pod is relaunched.

### Where a command takes its identity

`agent-secrets NAME -- command`, and every other form that acts as a session (`request`,
`status`, `cancel`, `revoke`, `self`, `whoami`, `sign`, `renew`), takes its broker identity from
the first of four sources that has one for the calling process, as the AWS SDK takes a credential
from the first provider in its chain that holds one. None of them needs a registered agent
session, so the same command works in a shell, a systemd unit, a cron job, an agent session, a box
and a pod.

| Source | What it is | Nothing for the caller (skipped) | Present but broken (stops the command) |
| --- | --- | --- | --- |
| `key-dir` | The `key.pem` and `enrollment` a launcher put in the key directory (`AGENT_SECRETS_KEY_DIR`, else `$XDG_RUNTIME_DIR/agent-secrets`): a box or a pod. | No `key.pem` there, and no `enrollment.pending` younger than 160 seconds. | A `key.pem` that does not parse, no `enrollment` beside it (with the launcher's `enrollment.error` when it left one), or a launcher still enrolling: a fresh `enrollment.pending` and no `key.pem` after `AGENT_SECRETS_ENROLL_WAIT`. |
| `sign-in` | A person's `agent-secrets login` on this machine (`AGENT_SECRETS_SIGNIN_DIR`, else `$XDG_STATE_HOME/agent-secrets/signins`). | Nobody has signed in on this machine. Or this machine cannot renew the sign-in (no sign-in client, or another client than the person signed in with) and it holds no live enrollment: none in its record, or one the broker answers not live to a read signed with the sign-in's key, made only for such a sign-in. The command then says so on stderr, in one line ending `; using the <source> source` where a later source serves, or `; tried the <source> source next` before that source's own refusal. `renew` never passes a sign-in over: it reports the ended enrollment. | Several people signed in and `AGENT_SECRETS_PERSON` names none of them, a record that does not parse, or a broker that refuses the sign-in or the read of its enrollment. |
| `helper` | `agent-secrets-helper`, for a caller inside a session `agent-secrets register` registered. The command asks the helper first, so a process that only inherited the socket goes on. | No socket file where no helper is expected (`AGENT_SECRETS_HELPER_SOCK` unset and no helper unit installed), or the helper answers `NOT_A_SESSION`. | A stopped helper: no socket file where `AGENT_SECRETS_HELPER_SOCK` names one or the helper's unit is installed (a clean stop removes the socket). A socket that does not answer, `NO_CREDENTIAL` (the machine is not logged in), or any other refusal. |
| `machine` | The machine's own cloud identity, found with no setting (below), whose session the **machine cache** keeps: `$XDG_STATE_HOME/agent-secrets/machine` (`~/.local/state/agent-secrets/machine`), a 0700 directory holding `key.pem` and `enrollment`, each 0600. A cached session signs with no network call. | No cached session, and no identity found: no `external_account` credential, no metadata server, or a broker that names no exchange audience. | A cached session that does not parse, a credentials file that does not parse, an `AGENT_SECRETS_MACHINE_IDENTITY` missing one of its variables or set to `kubernetes` or `github-actions`, a plain-http broker off loopback, a Google audience other than the one the machine first read, or any refusal of the broker's exchange. |

A source that is present but broken, and any other refusal of the broker's, stop the command with
that error: the command never acts under the next identity in its place. With no source, one refusal
names each source and why it had nothing:
`no broker identity for this process: key-dir: …; sign-in: …; helper: …; machine: …`.

The machine source finds the machine's identity from the Google exchange audience the broker
answers in `GET /v1/settings` (`google_exchange_audience`), and takes the first of these that
gives a token for it: an `external_account` credential file that `GOOGLE_APPLICATION_CREDENTIALS`
names, acting as the service account its `service_account_impersonation_url` names; and the
Compute Engine metadata server, which a machine with none answers "no metadata server" within 1.5
seconds (a GKE pod's metadata server answers for the pod's own Google service account). A pod's
projected Kubernetes token is not one of them: the broker's exchange mints no launcher credential
for a Kubernetes identity, so no session could be registered under it, and a pod enrolls through
its launcher or `agent-secrets enroll --pod` instead. A GitHub Actions run's token is not one of
them either, for the same reason. On EC2 with no such credential file the machine source has
nothing, since the broker takes no AWS identity itself: such a machine reaches the broker through a
launcher's key directory, a sign-in or the helper.

The credential file may take its subject token from a file, a URL, AWS, an executable (with
`GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES=1`, as Google's libraries require) or an X.509
certificate, whose configuration the file names (`certificate_config_location`) or takes from
`GOOGLE_API_CERTIFICATE_CONFIG`, else gcloud's default (`use_default_certificate_config`). A
certificate configuration must name a workload certificate and its private key: an enterprise
certificate proxy's is refused, since Google's library would start the signer program it names.
Which kind it is decides the token that mints the account's identity token with IAM Credentials'
`generateIdToken`, and so the grants the account needs:

- A file, a URL, AWS or an executable: the file's exchange at Google's security token service
  mints a federated access token, which the file's impersonation URL (IAM Credentials'
  `generateAccessToken`) spends on the account's access token, and that one mints the identity
  token. The federation's principal needs `iam.serviceAccounts.getAccessToken` on the account,
  which `roles/iam.workloadIdentityUser` carries, and the account needs
  `iam.serviceAccounts.getOpenIdToken` on itself, which `roles/iam.serviceAccountOpenIdTokenCreator`
  carries. An executable is told the account in `GOOGLE_EXTERNAL_ACCOUNT_IMPERSONATED_EMAIL`.
- An X.509 certificate: the federated access token mints the identity token itself, and the
  impersonation URL only names the account, as Google's own Go library mints an identity token
  with an `external_account` credential (`cloud.google.com/go/auth/credentials/idtoken`). The
  federation's principal needs `iam.serviceAccounts.getOpenIdToken` on the account, which
  `roles/iam.workloadIdentityUser` carries, and the account needs no role on itself.

`AGENT_SECRETS_MACHINE_IDENTITY` still names the one identity to trade (`metadata` or `pool`,
with its own variables; `kubernetes` and `github-actions` are refused, exit 2, before anything is
traded), and then
nothing is detected. Because the cache sits below
the sign-in, a person who runs `agent-secrets login` acts as themselves from their next command
on, while the machine's session stays for every process no person signed in for. A machine
session a client before this one kept in the key directory, marked `machine-session`, is moved to
the machine cache by the first command that finds it.

`GET /v1/settings` is public, and its Google audience chooses whom the identity token the
machine mints is good for, so the machine source reads it only from an `https` broker, or an
`http` one on loopback. Once it has found an identity to mint a token for that audience, and
before it mints one, it keeps the audience in `google-exchange-audience` in the machine cache;
a machine with no identity of its own mints nothing and keeps nothing. A broker that later
names another audience than the one kept is refused before any token is minted for it; if the
broker's audience really did change, remove that file. A broker publishes its Google audience
only where `BROKER_VM_SERVICES` lists a machine that may trade a token for it.

Each registration of the machine's session makes a key of its own, the first one and each one
after the broker says the session is no longer live, so a copy of `key.pem` (in a backup of the
home directory, or on a disk kept across a reboot) signs for no later enrollment. Commands that
find the cache empty at the same time register one session between them: a lock in the cache
holds a registration from its read of the cache to its write.

A process started with no environment of its own, such as a systemd unit or a cron job, reads
its settings from `~/.config/agent-secrets/config`, as the AWS CLI reads `~/.aws/config`: `KEY=value`
lines, `#` comments and blank lines, with no quotes taken off, no `export` and no expansion. It
takes nine keys — `AGENT_SECRETS_URL`, `AGENT_SECRETS_SIGNIN_ISSUER`,
`AGENT_SECRETS_SIGNIN_CLIENT_ID`, `AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE`,
`AGENT_SECRETS_MACHINE_IDENTITY`, `AGENT_SECRETS_MACHINE_TOKEN_FILE`,
`AGENT_SECRETS_MACHINE_AUDIENCE`, `AGENT_SECRETS_POOL_AUDIENCE` and `AGENT_SECRETS_POOL_ACCOUNT` —
and any other key is a usage error (exit 2) naming the file and the line. So is a path that is
not a regular file, a file another user than this one or root owns, and a file its group or
others can write: the file names the broker the machine proves its identity to, so nothing in it
is read until it is the user's own and `chmod go-w` is run on it. A variable that is set and not
empty wins over the file.
Where the environment gives no `HOME`, as for a system unit that names no `User=`, the file, the
sign-ins and the machine cache are under the home the password database gives the process's user
(`/root` for root).

`agent-secrets identity` answers by its exit status alone, as it always has: 0 for a launcher's
key directory or a helper session, 1 otherwise, asking nothing of the network. `agent-secrets
identity --source` walks the chain, registering nothing, and prints the source a command would
use (`key-dir: AGENT_SECRETS_KEY_DIR`, or where it is unset `key-dir: $XDG_RUNTIME_DIR/agent-secrets (AGENT_SECRETS_KEY_DIR is unset)` or `key-dir: /run/user/$UID/agent-secrets (AGENT_SECRETS_KEY_DIR and XDG_RUNTIME_DIR are unset)`,
`sign-in: <person>`, `helper: <socket>`, `machine: the cached enrollment in the machine cache (…)`,
or `machine: ` and the identity it would trade), then `helper: <socket>` whenever the helper
answered and is not that source; with none it prints the refusal above and exits 1. The key
directory and the machine cache each hold a `key.pem`, so no line names either by its path. Each
`helper: <socket>` line ends with the release of the helper process that answered, as that process
reports it (`(agent-secrets-helper <release>)`), not the release installed on disk, which differs
after an upgrade until the helper restarts. A helper from before that report answers
`unknown op version`, and the line ends `(a helper from before this release; its binary is
/proc/<pid>/exe)`, naming the process on the socket.

## Machine login

A launcher (a person's machine helper, or the Legion daemon) cannot enroll anything until the
broker has issued it a **launcher credential**: a credential bound to a key the launcher holds, never
a token, which the launcher uses by signing with that key. A person's machine gets one when that
person approves a **machine login**, an OAuth 2.0 device authorization grant (RFC 8628) on the
broker's own endpoints:

1. The machine generates a fresh key and asks the broker to log in (`agent-secrets machine login`
   asks the helper to, which calls `POST /v1/oauth/device_authorization`), naming the person who
   should approve it (the helper reads that person's Dispatch login from
   `AGENT_SECRETS_OPERATOR_FILE`).
2. The broker answers with an eight-character confirmation code, `XXXX-XXXX`, which the machine
   prints, and the machine polls `POST /v1/oauth/token` for the decision.
3. The person types the code into Dispatch, sees which machine is asking, and approves or denies it.
   Only the typed code selects a machine login: no link can approve one, and anyone else is refused
   `NOT_APPROVER`, since the machine's sessions act as that person.
4. On approval the broker mints the launcher credential bound to the machine's key, and the
   machine's next poll gets a refresh token for it, bound to the same key (DPoP, RFC 9449).

The device grant is served only while the broker is given a page for a person to type the code
into (`BROKER_MACHINE_LOGIN_URL`). A deployment whose machines hold a Google identity leaves that
unset: nothing prints a code, Dispatch serves no page to type one into, and each machine gets its
launcher credential from the trade above instead, with nobody to approve it.

The helper keeps the login (its key, the credential's id and expiry, and the current refresh
token) in `AGENT_SECRETS_MACHINE_FILE` (`~/.config/agent-secrets/machine.json` by default),
readable only by its user, so a restarted helper resumes it. It refreshes the login when it starts,
before it resumes it, and at most a day apart after: each refresh spends the refresh token for a new one and moves the
credential's expiry to `BROKER_LAUNCHER_CREDENTIAL_SECONDS` from then. A refresh token it has
replaced still works for five minutes, so a refresh the broker did not answer (its answer lost, a
timeout, the broker unreachable) or answered with a 5xx is retried every 15 s for as long as that
lasts. Spent after the five minutes, it is a reuse: the broker ends every refresh token of
the login and revokes the machine's credential as a revoke does, ending every session it enrolled,
and the helper holding it drops it at its next refresh. A newer login of the same machine instead
retires the one it replaced: that login can no longer refresh or enroll a session, and the
sessions it enrolled keep running.

A service, such as the Legion daemon, logs in with its own projected Kubernetes ServiceAccount
token instead, and no person approves it: the broker verifies the token as it verifies a pod's,
refuses one that lives longer than an hour from its issue, and issues the credential when the
token's account is one `BROKER_SERVICE_LOGINS` names for that service
([Operate](/legion/broker/operate/#what-to-run)). Each such account holds one credential, which
every login moves to the login's key and to the token's expiry, at most an hour away, so the
service logs in again before then and at every restart; a running daemon proves the same key at
each login, and a restart brings a new one. A device-code login that names a service is refused
`SERVICE_LOGS_IN_WITH_TOKEN`.

A person's launcher credential lapses `BROKER_LAUNCHER_CREDENTIAL_SECONDS` after its approval or
its last refresh, so a machine whose helper keeps reaching the broker stays logged in. It may be
revoked from Dispatch's machine-login page by the person who approved it and no one else, or from
any of their machines under its machine login (`agent-secrets machine revoke`), before or after it
expires, which also ends every session it enrolled and every refresh token it holds
([end a machine's login](/legion/broker/guides/revoke-a-session/#end-a-machines-login)). Its
sessions outlive its expiry: a session renews its lease with its own key, and the broker reads no
credential to renew one a person's machine login enrolled, so a box keeps working after the
machine's credential expires, and the page lists an expired login, marked as expired with sessions
still running, until its last session ends. (A machine's own session, enrolled under the login its
Google identity traded for, is the one exception: it renews only while that login is live, as
[Sessions and enrollment](#sessions-and-enrollment) says.) Whoever
approved a person's machine login is its operator. A lapsed or revoked login means logging the
machine in again. A machine login nobody decides expires after 15 minutes, a fixed time rather than
a setting (`machineLoginPendingTTL` in `packages/envoy/cmd/cipherd/main.go`). A person's machine
logs in through the device grant alone: the earlier `POST /v1/launcher-credentials` login answers
`405 Method Not Allowed` and its poll `GET /v1/launcher-credentials/{pending}` `404`. A service's
login has no operator. The machine-login page lists it for everyone signed in but a restricted
member (below), and any of them may revoke it, which ends every pod it enrolled at once: a service
login a person approved through a code before services logged in with their tokens, until it
expires, and a service's token login, which names no approver. A service whose token login was
revoked logs in again with its token at
its next call, under a new credential, which enrolls the pods it launches next; a pod the revoke
ended stays without secrets until it is replaced. To stop it logging in, remove its account from
`BROKER_SERVICE_LOGINS` and redeploy the broker. A token login of an account that
`BROKER_POD_OPERATORS` maps to a person is that person's own and no service's: the page lists it
to that person alone, and only that person may revoke it ([a person's pod](#a-persons-pod)).

A credential enrolls sessions only for its own operator: an enrollment naming anyone else is refused
`OPERATOR_MISMATCH`. Where `BROKER_PROJECT_ACCESS_URL` is set, a machine login or a pod login
whose person Dispatch calls a restricted member, someone an access list admitted from outside the
deployment's sign-in domain, enrolls nothing (`403 RESTRICTED_MEMBER_ENROLLS_NOTHING`), so that
person's agents are released no secret; the login still trades for a token that names their
projects. A session such a person already had is denied every secret `restricted-member`, every
name the profiles file answers included, before the file says anything of it, and its grants end
at their next use ([Operating the broker](/legion/broker/operate/)). Nor is such a
person any secret's approver, even of a secret they own, of a secret a Google group they are in
owns, or of a shared one whose approver is `anyone`, nor an approver of any service's machine
login, even one an older broker opened naming them: the broker's approval page signs in only the
deployment's domain and shows them no secret request (`403`), their **Inbox** lists them no
secret request and no service's machine login, their **Live grants** lists only the grants of the
sessions that act for them, and their machine-login page only their own machines' and pods'
logins. Their revoke or dismissal of anything else, and their deny of a service's machine login an
older broker opened, is refused `403 NOT_APPROVER`, saying why, and their revoke of a service's
machine login `404 NOT_FOUND`. Their own machine logins and the grants of their own sessions stay
theirs. The broker keeps the member kind Dispatch last answered for each person, so a person
Dispatch last answered a full member is served as one while Dispatch does not answer, for at most
an hour (`projectaccess.KeptAnswerTTL`), on every
route of this section, and "cannot say" below means a person it last answered a restricted member,
or of whom it keeps no kind ([Asking Dispatch](/legion/broker/operate/#what-it-depends-on)).
While Dispatch cannot say what kind of member a person is, those lists leave such rows
out, the machine-login page and its revoke hold the person to their own logins, so a lost
machine's login can still be ended there, while a service's login is out of reach there until
Dispatch answers and what remains is cluster access (delete the service's pods, or remove its
account from `BROKER_SERVICE_LOGINS` and redeploy the broker), such a dismissal or deny is refused
`503 MEMBER_KIND_UNCHECKABLE`, and the approval page shows no request (`503`). While Dispatch does
not answer at all (a timeout, a failed connection, a gateway's `502`, `503` or `504`, Dispatch's
own `DATABASE_UNAVAILABLE` or `OIDC_ISSUER_UNAVAILABLE`, or a `500` that names no code or names
`INTERNAL`, which is how Dispatch answers a read its database failed), only the person a grant
names as its approver ends it through the broker as an approver, since a revoke only takes access
away. Every revoke through a group or as anyone is then refused `503 MEMBER_KIND_UNCHECKABLE`,
whatever session holds the grant. A grant on a person's session can still be ended by that person,
its operator, asking Dispatch nothing; a grant on a session that acts for no person (a service's
pod, a person's pod `BROKER_POD_OPERATORS` no longer maps, or a machine's own session) nobody but
its named approver ends through the broker until Dispatch answers. What remains for a pod's grant
meanwhile is cluster access: delete the pod. Its grants then end when its lease lapses unrenewed
(`BROKER_LEASE_SECONDS`), except that a session that names a session id leaves them dormant, used
by nothing, for its next enrollment until `BROKER_SESSION_CARRY_SECONDS` passes, and a Legion
claim's pod hands them to the claim's next live pod at once; so where something starts that
session again (a Legion claim), stop that too. A grant also ends at its own expiry. Every revoke
while Dispatch answers anything else the broker
cannot read is refused `503 MEMBER_KIND_UNCHECKABLE` too: a refusal of the broker's own account, a
kind this broker does not know, a `500` naming another code, another code of Dispatch's, a
certificate that does not verify or a name that does not resolve. A revoke of a grant of one's own
session asks nothing. The member-kind read of each route Dispatch relays ends within 2 seconds,
inside the 5 that Dispatch waits for the broker, and so does the approval page's; a revoke's read
of a grant's project is not bounded so, and while all of Dispatch is down it refuses the revoke of
a grant whose project, or whose reason's issue key, Dispatch last answered hidden from the person,
or never answered for them; one Dispatch last answered public, visible to them, or no project at
all is answered so from what the broker kept, for at most an hour (`projectaccess.KeptAnswerTTL`).
The broker's rate
limiter caps how often anyone can start a machine login, per source address and per named person,
and how often anyone can try a token login: per source address before the token is verified, and
per account once it is. A token login the broker could not complete (a 5xx, such as while its
database is away) spends neither.

## Owner and tier: who may have which secret

The broker reads who may have a secret from the secret itself
(`packages/envoy/internal/cipher/policy/`). Every agent secret is a secret in the broker's secret
store (`BROKER_SECRETS_STORE`: AWS Secrets Manager, or Google Cloud Secret Manager) under one
namespace, `BROKER_SECRETS_PREFIX` (such as `production/agent-secrets/`, or
`projects/<project number>/secrets/agent-secrets-`), encrypted with one agent-secrets key
(`BROKER_SECRETS_KMS_KEY_ARN` in AWS KMS, or `BROKER_SECRETS_KMS_KEY_NAME` in Cloud KMS), and
marked with tags or annotations:

- **Its name** under the prefix is the name a session asks for, in lowercase with each underscore a
  hyphen: `production/agent-secrets/deel-api-key`, or
  `projects/<number>/secrets/agent-secrets-deel-api-key`, is `DEEL_API_KEY`. It is lowercase
  letters, digits and single hyphens, starting with a letter.
- **`owner`** is `shared`, a person's email in lowercase, the email they sign in to Dispatch with,
  a Google group as `group:` and the group's lowercase email, whose members each decide as the one
  owner does ([A group's secret](#a-groups-secret)), or a service the broker is configured with
  (`BROKER_SERVICES` or `BROKER_VM_SERVICES`).
- **`tier`** is `agent` or `human`.
- **`type`** is absent for a stored secret. It is `github-app` for a GitHub App key,
  `github-installations` for GitHub installation tokens a minter outside the broker stores
  ([Requests and grants](#requests-and-grants)),
  `google-login` for a Google authorized-user login, `azure-login` for the Entra tenant and client
  ids of the one secret the deployment names in `BROKER_AZURE_LOGIN_SECRET`, released with the
  broker's own projected Azure token at human tier alone, `google-service-account` for one Google
  service account the broker mints a token as, or `anthropic-federation` for an Anthropic token
  the broker gets by federation as one Google service account. For each of the last two, the
  account is the one the deployment binds the secret to (`BROKER_SERVICE_ACCOUNT_SECRETS`). A
  secret with a type must have a current value.
  For one release, when `type` is absent, the broker reads the old `kind` tag instead and logs
  one warning per secret per broker process. An explicitly empty `type` takes precedence too.
  Writers use `type` on both stores. Deploy this reader before changing production tags; then
  copy each old classifier to `type` and remove the old tag. Once a secret carries `type`, this
  release is a rollback floor: an earlier broker reads no `type` tag and serves the secret as
  stored ([Rolling back](/legion/broker/operate/#what-it-depends-on)). `agent-secrets` writes a
  type only through a broker whose settings answer `secret_type_tag: true`.
  `secret retag NAME --type none` removes both classifier names and the rollback guard.
- **`github-owners`** is required on a `github-app` or `github-installations` secret, and read on
  no other type. It lists the GitHub accounts the broker hands installation tokens over for,
  separated by spaces
  (`acme-org ada`) and compared without regard to case, a form both stores hold.
  A comma, or an entry no GitHub login can be (one that does not start with an ASCII letter or digit, holds anything but ASCII letters, digits, hyphens and underscores, is over 39 characters, or is the word `none`), is refused `github-owners-malformed`.
  An entry may hold an underscore, which GitHub's sign-up form refuses, because a managed user's login ends in `_<shortcode>` (`mona-cat_octo`).
  A public App can be installed by any account, so an
  installation on an account the list does not
  name gets no token. A `github-app` or `github-installations` secret without the tag, or with one
  that names no account, is refused `github-owners-missing`.
- **`requesters`** is optional. It names the only people, groups and services that may ask for the
  secret at all: people by the lowercase email they sign in with, Google groups as `group:` and the
  group's lowercase email, whose members' sessions it admits, and services by the name the
  deployment registers them under, separated by spaces
  (`ada@example.com group:approvers@example.com legion-daemon`). A session whose person the list
  does not name, and a pod whose service it does not name, is denied `requester-not-listed`. A
  comma, a list that names nobody, an email with a capital letter, a `group:` entry whose rest is
  no lowercase email, or a service the deployment does not register is refused
  `requesters-malformed`. The list narrows who may ask and bounds no grant by itself: a secret
  that is neither `admin`, `google-service-account` nor `anthropic-federation` keeps whatever
  `max-grant` it has, or none, so a person's own mail and Drive logins stay at their tier and are
  capped by nothing. A `google-service-account` or `anthropic-federation` secret's list names its
  owner once, as the owner tag writes it, and no other person or group, and the secret is refused
  `requesters-not-owner` otherwise, so a secret owned by `shared` or by a service carries neither
  type. A human-tier one may name registered services beside that owner, so a service's pod can
  ask and the owner approves each request; an agent-tier one may not, and neither may a secret the
  deployment binds to a `dev-<name>` account, at either tier, since that token is one person's own.
  Set the list with `agent-secrets secret create|retag --requesters "<email> <service>"`.
- **`scopes`** is optional, read on a `google-service-account` secret and refused on any other
  type (`scopes-type`). It lists the OAuth scopes the minted token carries, each the part of a
  Google scope after `https://www.googleapis.com/auth/`, separated by spaces
  (`cloud-platform bigquery`). A secret without it mints the `cloud-platform` scope alone, which
  is what every such secret minted before the tag existed. The list is in the fingerprint of the
  secret by its own name and of each profile of it with no `google` block of its own, so a changed
  list, or the tag added or taken off, ends each live grant decided under the old one at its next
  release, and the session asks again: a grant never mints at scopes it was not decided under. The
  names are the short ones because an AWS tag value holds at most 256
  characters, which five whole scope URLs are longer than. A comma, a list naming no scope, a
  whole scope URL, a name with a capital letter, and a name given twice are each refused
  `scopes-malformed`. Set it with `agent-secrets secret create|retag --scopes "<scope> <scope>"`.
- **`admin`** is optional, and `true` is its only value; anything else is refused
  `admin-malformed`. It marks a secret whose grant is an administrator's, which the broker cannot
  read off the type, an admin console password and a personal API key both being stored strings.
  Such a secret is human tier, names its `requesters` and bounds its grants to an hour, so a
  person approves every release, only someone the secret names may ask, and the grant is short
  enough to review while it is live; one that is not is refused `admin-tier`,
  `admin-requesters` or `max-grant-required`. A person approves every release of one even where
  a deployment runs a model judge: the judge refuses an administrator's secret, so a `judge`
  tag on that secret cannot undo the `admin` tag
  ([The model judge](#the-model-judge)). Set it with
  `agent-secrets secret create|retag --admin true`.
- **`max-grant`** is optional, except on an `admin` secret and on a `google-service-account` or
  `anthropic-federation` one: each of those needs it, of an hour or less, and is refused
  `max-grant-required` without it. It
  is a Go duration of at least one second. A request for that secret must ask for a lifetime no
  longer than the tag; a request with no `--ttl` asks for the default, 15 minutes when any
  secret it names is human tier and 12 hours otherwise.
- **`default-grant`** is optional. Its one value is `session`: a request with no `--ttl` whose
  secrets all carry it gets a grant that lasts until its session ends instead of that default. Any other
  value is refused `default-grant-malformed`. Set it with `agent-secrets secret create|retag
  --default-grant session`.
- **`judge`** is optional. It makes the secret one [the model judge](#the-model-judge) decides:
  whole seconds from 1 to 900, written in digits alone, the longest grant of it the judge may
  approve and the lifetime a request for it that names none gets. Any other value — `1800`, `0`,
  `15m`, a sign — is refused `judge-out-of-range`, so the secret goes to nobody rather than to a
  judge with no bound. A profile is decided as its source is, by the source's tag. The tag is in
  the policy version, so adding, lowering or removing it makes the broker check each live grant
  the judge made of the secret again at its next release. A profile of the secret that is behind
  a kind carries the seconds in its fingerprint, where its `proxy` block once held them, so a
  change of the tag ends that profile's grants, as an edit of its entry does. Set it with
  `agent-secrets secret create|retag --judge 900`, and remove it with `--judge none`.
- **`guidance`** is optional: the owner's instruction to the model judge, saying when to grant a
  request for the secret. The judge shows it whole to its model, so a value longer than 2000
  characters is refused `guidance-too-long`. It decides nothing a live grant may do, so it is in
  no policy version and writing it ends no grant. Guidance is no secret: `GET /v1/settings`
  answers it to anyone. Set it with `agent-secrets secret create|retag --guidance '<text>'`, and
  remove it with `--guidance none`.
- **`dispatch-project`** is optional. It names the one Dispatch project the secret belongs to, by
  its key as Dispatch writes it (`PRIV`: an upper-case letter, then one to nine upper-case
  letters or digits). A secret of a private project goes only to a session or pod whose
  credential names that project ([A private project's secrets](#a-private-projects-secrets)); a
  secret of a public project is decided by owner and tier as if it carried no tag. A key in
  another case, white space, an empty tag and the word `none` are refused `project-malformed`,
  and a broker that cannot ask Dispatch whether a project is private (no
  `BROKER_PROJECT_ACCESS_URL`) refuses every tagged secret `project-unchecked`, since it could not
  tell a private project's secret from a public one's. A key that differs from `dispatch-project`
  or `project-channels` only in case or in its separators (`Dispatch-Project`, `dispatch_project`)
  is refused `project-tag-misspelled`: read as written, it would leave the secret with no project
  tag. The tag is not called `project`, which AWS accounts commonly use for cost allocation, and a
  `project` key is read as no tag at all. Whoever may write the store's prefix may set the tag, and
  can already read every value there; which secrets belong to a project is its owner's call. Set
  it with `agent-secrets secret create|retag --project PRIV`, and remove it with `--project none`.
- **`project-channels`** is optional, and its one value is `slack`. It marks a Slack bot token,
  such as `LEGION_SLACK_BOT_TOKEN`, whose use a private project's owner limits to the channels
  they name: a credential that names a private project spends such a secret only behind the
  airlock, on a post to one of those channels
  ([A private project's secrets](#a-private-projects-secrets)), and every other credential is
  decided by owner and tier as before. Any other value, an empty tag and the word `none` included,
  is refused `project-channels-malformed`, so the secret goes to nobody rather than out with no
  limit. Set it with `agent-secrets secret create|retag --project-channels slack`, and remove it
  with `--project-channels none`.
- **`mint-service-account`** is written by `agent-secrets secret` beside every type, with the value
  `type-secret`, and this broker never reads it. Only the brokers from legion-envoy-v13.19.0 to
  the release before v16.0.0 read it, and refuse a type secret rather than serve its stored key.
  Every broker from v16.0.0 to the release before the `type` tag ignores it and serves a secret
  that carries `type` alone as a stored secret, so it makes no rollback past the first release
  that reads `type` safe: that release is the rollback floor.
- **`rollback-guard:project-tags`** is no tag of its own but an entry `agent-secrets secret`
  writes into the `requesters` tag, after the list it holds, beside a `dispatch-project` or
  `project-channels` tag, and takes out with the last of the two. This broker takes the entry out
  before it reads the list, so it changes nothing here, and `secret show` prints the list without
  it and a `rollback_guard: yes` line. A broker from legion-envoy-v29.23.0, the first to read
  `requesters`, up to the release before the project tags reads it as a service no deployment
  registers and refuses the secret `requesters-malformed`, rather than serve it by owner and tier
  to every session. A broker older than legion-envoy-v29.23.0 reads no `requesters` tag and serves
  the secret
  ([Rolling back past the project tags](/legion/broker/operate/#rolling-back-past-the-project-tags)).

In Secret Manager these fields are annotations rather than labels because a label's value cannot
hold an email (`@` and `.` are outside its alphabet), and the broker compares the owner with a
Dispatch login byte for byte. An annotation holds the email as it is.

People and agents write the secrets and their tags, not the broker: `agent-secrets secret` creates
and changes them under whatever cloud credential its environment gives it, in both stores, and the
store's IAM decides what that credential may change
([manage a secret](/legion/broker/guides/manage-a-secret/)).

A `requesters` list is read before owner and tier. Where a secret carries one, a session whose
person it does not name, and a pod whose service it does not name, is denied
`requester-not-listed`, whatever the table below would have given it. An entry starting `group:`
is a group, and admits a session whose person the group lists, as the broker last read it
([A group's secret](#a-groups-secret)). Of the rest, an entry holding an `@` is a
person and is matched against the session's person alone; one holding none is a service and is
matched against the service the broker verified alone. A pod acts for no person unless
`BROKER_POD_OPERATORS` maps its account to one, and then it has no service
([a person's pod](#a-persons-pod)). A session has no service. So each is matched only by an entry
of its own sort, and a person whose login reads like a service name matches no service entry. A
secret with no list is every session's to ask for.

A `dispatch-project` tag is read next, still before owner and tier. Where the tag names a private
project, a session or pod whose credential does not name that project is denied
`project-not-named`, whatever the table below would have given it; where it names a key Dispatch
holds no project for, everyone is denied, as `project-missing` in the broker's log and as
`project-not-named` to the session, so a refusal never says whether a project exists. A secret
of a public project, or of no project, is decided by the table alone
([A private project's secrets](#a-private-projects-secrets)).

Who gets it then follows from those two tags alone:

| The secret | Its owner's own session | Any other session or pod |
| --- | --- | --- |
| A person's, `tier=agent` | Granted at once. | Sent to the owner for approval. |
| A person's, `tier=human` | Sent to the owner for approval. | Sent to the owner for approval. |
| A group's, `tier=agent` | Granted at once to a member's own session. | Sent to the group for approval: any member decides. |
| A group's, `tier=human` | Sent to the group for approval: any member decides. | Sent to the group for approval: any member decides. |
| `owner=shared`, `tier=agent` | Granted at once. | Granted at once. |
| `owner=shared`, `tier=human` | Approved by anyone who signs in to the broker. | Approved by anyone who signs in to the broker. |
| A service's, `tier=agent` | Granted at once. | Denied; no one can approve it. |

A session is its owner's own when its operator is the owner and one of two things holds: the owner
approved the machine login it enrolled under, or it is a pod whose account `BROKER_POD_OPERATORS`
maps to the owner ([a person's pod](#a-persons-pod)). Any other pod is not the owner's own, whether
it acts for no person or for the person its account is mapped to, so its request for the owner's
agent-tier secret goes to the owner for approval. For a group's secret, a member's own session, in
that same sense, is the owner's own, while the broker's read of the group lists that member.

Every row that sends a secret for approval needs the request to carry a reason: a request with
none is denied instead of being put to a person
([Requests and grants](#requests-and-grants)).

An owner may also be a service the broker is configured with. `BROKER_SERVICES` binds each
service's name to the Kubernetes service accounts its pods run as, one entry per account, for
example `legion-daemon=system:serviceaccount:legion:legion-worker`. A service's own sessions are the
pods that a launcher logged in as that service enrolled, and whose projected token proved one of
that service's accounts: the Legion daemon's worker pods, for `legion-daemon`. A launcher logs in
as a service with its own token, for an account `BROKER_SERVICE_LOGINS` names; the pod's own token
proving the pods' account is what makes a pod the service's.

`BROKER_VM_SERVICES` binds a service to the Google service accounts its machines run as, one entry
per account, each account bound to one service; for each account, `BROKER_VM_PROJECTS` and
`BROKER_VM_INSTANCES` may pin the one Compute Engine project its machines run in and the glob
their instance names match. Those entries are what the broker's token exchange decides a machine's
Google identity token on, and a machine of a bound account is then that service's own session, so
the service's secrets go to it at once.

A service's secrets go to its own sessions and to no one else. That includes a person's own
session, and a pod run under another service account even when a launcher logged in as the service
enrolled it. A service's secret is agent tier, since no person approves it: the broker refuses one
tagged `tier=human`, and one whose owner names a service it is not configured with.

One thing besides the tags changes what a session gets at once: a session's own person, its
operator, who revokes a grant the session got without asking **withholds** its secrets from that
session, even when the grant had already ended by then
(`packages/envoy/internal/cipher/requests/revoke_approver.go`, `RevokeByApprover`). Each
withheld secret is human tier to that session for as long as the session lives: every other grant
the session got it on without asking ends with the revoked one, and the session's requests for it
are sent to the owner for approval, or to anyone who signs in to the broker for a shared secret, so the
session asks before it gets the secret again. Only the session's own person withholds: anyone else
who revokes one of its grants, the person who approved it, ends that grant alone. Other sessions,
the person's own included, still get the secrets at once.

The broker reads the namespace when it starts, and refuses to start when it cannot, then again every
five minutes, a fixed time rather than a setting (`policyRefresh` in
`packages/envoy/cmd/cipherd/main.go`). A change is served at once when something asks the broker to
reread it: `POST /v1/secrets/{name}/reread`, which `agent-secrets secret` sends after each write it
makes and any caller may send right after writing a secret, and a request naming a secret the
broker does not serve yet, which makes it reread that one name before refusing it (up to ten such
rereads at once per session in each broker process, refilled one every ten seconds:
`DefaultMissRereads` in `packages/envoy/internal/cipher/requests/machine_state.go`), so a secret
created a moment ago is served on its first request.

The reread route tells a caller what the broker now serves only when an enrolled session signs
the call. A call with no proof is answered `202` with `"accepted":true` and one fixed `reason` at
once, the same for every name, and the reread runs after that answer, so neither its answer nor
how long it takes says which secrets exist. `agent-secrets secret` signs its reread with a broker
identity the process already holds, and reads the answer; with none, or where the broker cannot
check the proof, it sends the reread unsigned all the same, and says the broker's view was not
checked.

A reread asked for over `POST /v1/secrets/{name}/reread` is the whole broker's, not one process's.
A deployment may run several broker processes against one database, and the one such a reread
reaches tells the others to read that same name again,
so a retag that narrows who may have a secret is in force everywhere within seconds rather than at
each process's own next five-minute read. The miss-path reread above is the other way round: it
stays in the process that served the request, deliberately, since fanning it out would spend every
peer a store read for a name that peer may never be asked for. One exception is worth stating: a process whose
connection to the database has dropped hears nothing while it is reconnecting, and nothing is
replayed to it when it reconnects, so until its own five-minute read it goes on serving what it
last read. That read is the backstop for everything the telling can miss.

A reread reads the one secret from
the store (on AWS, `DescribeSecret`, which shows a write at once; on Google Secret Manager,
`GetSecret`, which Google documents as eventually consistent, so a reread made right after a write
can still find the old state, usually for seconds) and serves it, or stops serving it,
as it finds it; a read of the whole namespace for as long as the store's listing may lag after (five minutes on
AWS Secrets Manager, six hours on Google Secret Manager, whose metadata Google documents as
eventually consistent for up to a few hours) reads that secret again alone, so a listing that has not caught up never undoes it (`Current.Refresh` in
`packages/envoy/internal/cipher/policy/current.go`). A reread that finds a secret as the broker
already serves it, or no secret under a name the broker was not serving, changed nothing, and is
not read again — which is the one case that
promise does not cover: a secret created and then deleted before any read served it is anchored by
nothing, so the next read of the namespace can serve it from a listing that shows the creation and
not the deletion, for up to that lag after the delete, and a request for it is granted while a
grant's read of its value is refused `SECRET_NOT_IN_STORE`. A change nothing rereads, such
as a tag edited in the AWS console, takes effect within about ten minutes on AWS: at the next read
of the namespace, or the one after, since Secrets Manager's listing can lag a change by up to five
minutes. On Google Secret Manager it usually takes effect at the next read of the namespace too,
but Google bounds its listing's lag only at a few hours, so a change made any other way than
`agent-secrets secret` (`gcloud`, the console) should be
followed by a reread. A read that fails (Secrets Manager or KMS out of reach) is logged when the
failures start and again when a read succeeds, and the policy from the last good read stays in
force. The broker leaves out a secret it cannot serve, logs it by name once, when it starts to
refuse it, and counts it in a metric for as long as it refuses it
([Operating the broker](/legion/broker/operate/#health-and-logs)): a missing or malformed `owner`
or `tier` tag (an email with a capital letter is malformed, and so is a `group:` owner whose rest
is no lowercase email), an unknown `type`, a `github-app`
secret whose `github-owners` names no account or holds a comma, a `requesters` list the broker
cannot read (`requesters-malformed`), an `admin` tag set to anything but `true`
(`admin-malformed`), an `admin` secret that is not human tier (`admin-tier`) or that names no
`requesters` (`admin-requesters`), an `anthropic-federation` secret that is not human tier
(`type-human-tier`), an `admin`, `google-service-account` or `anthropic-federation` secret with no
`max-grant` or one longer than an hour (`max-grant-required`), a `google-service-account` or
`anthropic-federation` secret whose `requesters` do not name its owner once, person or group, or
that lists a service where it may not (`requesters-not-owner`), a `scopes` list the broker cannot
read (`scopes-malformed`), a `scopes` tag on any other type (`scopes-type`), a `dispatch-project`
tag that is no project key (`project-malformed`), any `dispatch-project` tag on a broker that
cannot ask Dispatch about projects (`project-unchecked`), a `project-channels` tag set to anything
but `slack` (`project-channels-malformed`), a tag key that differs from `dispatch-project` or
`project-channels` only in case or in its separators (`project-tag-misspelled`), a malformed
`max-grant`, a name that is not in the form above, a secret encrypted with any key but the
agent-secrets key, the AWS-managed key included, a secret with no current value (created without
one, so none of its versions carries the `AWSCURRENT` label), or a type secret whose stored value
its type's minter could not mint from (`value-malformed`), which the broker finds by reading the
value once for each new version or type of the secret
([the value read](/legion/broker/operate/#the-value-read)). A request for a secret left out is
refused `UNKNOWN_SECRET` before anyone is asked to approve it, and a live grant of it releases
nothing until the broker serves the secret again.

The profiles file (`BROKER_PROFILES_FILE`) is applied to what the store holds at every load and
reread: each profile that fits its source is served by its name, as its source's owner, value and
type with the profile's own scope, and each that does not is refused alone, logged and counted as
`profile:<NAME>`: `profile-source-missing` (its source is no secret the broker serves, or is
another profile), `profile-name-taken` (a stored secret other than its source holds its name,
which is refused by that name too, unless that stored secret carries the tag `superseded-by`
whose value is exactly the profile's source: then the profile is served by the name and the stored
secret is not, whether the broker serves the stored secret, refuses it or waits for its value, which
is how a deployment moves a name from a key the broker held to the minted tokens that replace it
and then retires the key; any other value, and no tag, leaves the profile refused),
`profile-block-mismatch` (a `github` or `google` block that
does not fit the source's type, `github-owners` or `scopes`), `profile-looser` (looser than the
source in tier, `requesters` or `maxGrant`), `profile-source-not-proxied` (a stored-token,
`google-login`, `azure-login` or `anthropic-federation` source with no `proxy` tag and no `proxy`
string, refused with its source), one of the airlock's refusals ([The airlock](#the-airlock)), or
a tag rule its own tags over its source's break,
such as `admin-tier` for an agent-tier profile of an `admin` source. A source a profile of another
name uses is served only through its profiles, and a `github-app` secret no profile names is
refused by its own name (`github-app-no-profile`), as is a `github-installations` one
(`github-installations-no-profile`), so every GitHub token the broker hands over is a profile's.
A profile's `requesters` may name a Google group as a `requesters` tag does, and a full load reads
every group a profile names as it reads the stored secrets' groups. Against a source's list, a
group entry is matched by that same group alone: a profile that names a person in the source's
group is `profile-looser`, since the group's members change and the person's entry would not. A
profile of a secret a group owns is the group's ([A group's secret](#a-groups-secret)). An
`anthropic-federation` source's profile is behind the source's `proxy` tag, as a `github-app` or a
`google-service-account` source's is, and its type's rules hold the profile's own tags: human
tier, a `maxGrant` of an hour at most, and `requesters` that name the owner once.
A withhold covers every profile of the withheld name's source, and the source's own name, in both directions, on every live or dormant enrollment of the session: once a session's operator has withheld a source or any profile of it, the session asks for each of them as for a human-tier secret, and every grant it got one of them on without asking ends with the revoked one.
So a revoke of a read profile's grant withholds its write sibling too.

The policy's **version** is the SHA-256 of every served secret's name, owner, tier, source, max
grant, `requesters` list, `scopes` list, `admin` mark, project and project channels, and of the
fingerprint of each profile it serves, recorded on every request. A type secret's source names its
type, so a type change moves the version too, and so does any change to a list, to a project tag or
to a profile's entry, which ends that profile's grants. A secret with no list, no project tag and no
profile writes the same bytes into the digest that it always did, and the project tags' rollback
guard (`rollback-guard:project-tags`), which the broker takes out of the `requesters` list before it
reads it, moves nothing. Whether a project is private is Dispatch's, not the version's, so a grant
of a tagged secret is checked again on every release. The version holds no group's members either,
which change with no change to any tag, so the broker checks every grant of a group's secret again
at each release and each reuse, whatever the version ([A group's secret](#a-groups-secret)).

### A private project's secrets

A Dispatch project can be private: only its own people, and the agents working for them on it, see
it. A secret whose `dispatch-project` tag names a private project goes only to a **credential that
names that project** and whose person may still see it:

- A **session** names its projects when it enrols. The helper sends the list
  `AGENT_SECRETS_PROJECTS` holds (project keys separated by spaces, `PRIV CORE`), or
  `agent-secrets register --projects`, and the broker takes it only when Dispatch says the session's
  person may see every project on it. A restricted member's machine login and pod enrol nothing,
  whatever they name (`403 RESTRICTED_MEMBER_ENROLLS_NOTHING`); only a session under their
  sign-in must name at least one. A session naming none enrols as before. The list is fixed for
  the session's life: start a new session to name another, and `agent-secrets self` prints it as a
  `projects:` line.
- A **pod** names no list of its own: its project is the one its operator's entry in
  `BROKER_EXCHANGE_ACCOUNTS` maps its service account to, for that entry's person. A machine's own
  session likewise names the one `BROKER_VM_EXCHANGE_ACCOUNTS` maps its fleet's Google account to.
  A [person's pod](#a-persons-pod) acts for its person, so it names its list as that person's
  session does.
- A **job's pod** or a **job's machine**, whose account a job entry lists
  (`BROKER_EXCHANGE_JOB_ACCOUNTS`, `BROKER_VM_EXCHANGE_JOB_ACCOUNTS`), names that entry's list and
  no person. A credential of no person sees no private project, so it is denied every private
  project's secret `project-not-named`, a project of its own list included, with no question to
  Dispatch about a person. Where its list names a private project, it gets a `project-channels`
  secret only behind the airlock and on no channel (below); a list of public projects alone leaves
  that secret unlimited.

Every other credential is denied the secret `project-not-named`: a session naming another project
or none, a person on the project's access list whose session does not name it, and a pod whose
entry maps another project. The denial names no project, so `agent-secrets` says to start the
session with `AGENT_SECRETS_PROJECTS` naming it. A tag naming a key Dispatch holds no project for is
denied to everyone, with the same words.

A profile of a tagged secret carries its source's `dispatch-project` and `project-channels` tags
whatever its own name, so everything this section says of the secret holds for each of its
profiles: a profile never reaches a credential its source would be refused to.

Whether a project is private, and who may see it, is Dispatch's, so the broker asks Dispatch before
it decides a request, a release or an approval of a tagged secret, and never while it holds a
database lock. It keeps an answer for 30 seconds, and decides nothing on an answer older than that:
it asks one question at a time, so when the later questions take long enough for an earlier answer
to pass 30 seconds before the decision, it asks every question again, and a decision whose answers
pass 30 seconds a second time is refused as though Dispatch could not be reached. A live grant of
a tagged secret is checked again on every release: once the project turns private and the session
does not name it, or its person leaves the access list, the grant ends for good, and making the
project public again does not bring it back. The airlock is held to the same rule: a connection of
such a grant is refused, and a pod holding its values asks the broker again within ten minutes.
While Dispatch cannot be reached, or answers a project's visibility as neither `public` nor
`private`, a tagged secret is neither granted nor released (`503 PROJECT_ACCESS_UNAVAILABLE`) and
its grants stay live; a secret with no tag is served as before. A broker with no
`BROKER_PROJECT_ACCESS_URL` asks Dispatch nothing about projects.

A secret tagged `project-channels` is limited by the projects the credential names, whatever
project the secret itself belongs to. A credential that names a private project, whether or not
its person can still see it, gets the secret only behind the airlock, and is denied it
`project-channels-need-airlock` where it is not behind a kind of the airlock. The proxy then carries only
`POST /api/chat.postMessage` with `channel`, `thread_ts` and `text`, to a channel id that the owner
of every private project the credential names has listed in Dispatch, and refuses every other call
`CHANNEL_NOT_ALLOWED` without calling Slack. The credential has no channel at all when an owner
listed none, when its projects share none, when its person can no longer see one of them, or when
Dispatch's answer carries no list. A pod holding the secret reads the owners' lists again within
ten minutes.

A request of a private project is the project's own: one for a secret of the project, one from a
credential that names the project (the list above), or one whose reason names one of the
project's issues. A person who may not see the project does not find it in any list, cannot open
its page, and cannot decide it ([Approve a request](/legion/broker/guides/approve-a-request/)). To
tell, a broker with `BROKER_PROJECT_ACCESS_URL` asks Dispatch, for the person reading, about each
project a request touches, the project of each issue key its reason names included, even in a
deployment that tags nothing: the reason `INFRA-12: rotate the deploy key` costs one read of
`INFRA` for that person, kept 30 seconds, and `SHA-256` in a reason reads as a key too. Such a
broker refuses a new request whose reason names the issue keys of more than 16 projects when it
is made (`400 REQUEST_INVALID`, naming the bound), so the reads one request costs are bounded; a
broker without `BROKER_PROJECT_ACCESS_URL` reads no reason's projects and takes any reason. A
request made before that rule, or by a broker without the setting, whose reason names more is
read whole: each project its keys name is asked, at most 80 in a reason of 400 characters, and a
private one among them hides it as any other does. A request's projects are asked 16 at a time,
and a list asks each project once for each person however many of its requests name it, even
when the request that asked first is hidden by another of its projects before the answer comes,
so a reason of 80 projects costs a list five rounds of questions: half a second while Dispatch
takes 100 ms an answer, inside the 5 seconds Dispatch waits for the list. The broker keeps at most
4,096 answers at once and keeps no new one past that, so many people reading many such requests
can leave their answers unkept, and each of their lists then asks again. While Dispatch cannot
answer, a request it must ask about is left out of the pending list, and a grant it must ask
about out of the grant list of everyone but that grant's approver and its session's operator,
who still list it and may end it whatever project it touches, each with a line in the broker's
log naming its record; its page answers 503 and shows only its record id, and an approval of it
decides nothing. A list gives the requests it must ask Dispatch about at most 3 seconds from the
moment it reads them, inside the 5 seconds Dispatch waits for it, and reads up to 64 of the
requests waiting on its reader, and of its grants, at once, asking Dispatch at most 16 questions
at once: a request whose answers have not come by then is left out of that list and logged the
same way. A request waiting on a question another request asked takes none of those 16, so a
project Dispatch is slow to answer, or never answers, costs a list only the requests that name
it, unless they are the first 64 waiting on its reader that it reads. A request waiting on such a
project keeps its place until the 3 seconds have run out, so the requests after those 64 start
only then, and each is listed only where it asks Dispatch nothing or the broker already holds
every answer it needs. While the pending list reads the requests that name someone else as
responsible, for at most 2 seconds, those reads take one of the 64 places between them, so for
that long 63 of the requests waiting on its reader are read at once; a request behind 63 such
requests starts when those reads end, inside the 3 seconds. A read of a request that fails in
the broker itself leaves that request out in the same way, with an error line naming it, and the
list goes on. Every other request is listed and decided
as before. A decision reads the projects of both credentials a request has had:
the one that asked, which the lists and the page read, and the one it hangs off now, which a
session that moved to another box makes it; a person either would hide it from does not decide
it. The model judge grants no private project's request
([the model judge](#the-model-judge)): its check leaves such a request to a person, and its
approval asks Dispatch again, afresh and not from the 30 seconds of answers the broker keeps,
before it is written, so a project that turned private while the judge was reading, or a session
that took the request over and names a private project, leaves the request to a person. Its
refusal's write asks Dispatch nothing, so a refusal is final whatever project the request belongs
to; the checks that lead to it read Dispatch as every check does. A signed-out sign-in names no
approver to the provider for any record, since the page reads no record before a person signs in,
and a record the person may not see answers as an id no record has before any sign-in starts,
**Sign in as someone else** (`?switch=1`) included.

### A person's pod

A deployment can let a Kubernetes pod work for one person on the terms of that person's dev box:
`BROKER_POD_OPERATORS` maps the pod's ServiceAccount to the person
([Operate](/legion/broker/operate/#what-to-run)). Each such account logs in under a login service
of its own, which `BROKER_SERVICE_LOGINS` names and no `BROKER_SERVICES` or `BROKER_VM_SERVICES`
entry names. The pod enrolls itself with `agent-secrets enroll --pod --service <its login service>`.
The login it makes with its own projected token carries the person as its operator, and so does
its enrollment (`machine.Service.TokenLogin`, `enroll.PersonPodLogin`). A token of the account
that is bound to no pod, such as one `kubectl create token` mints, logs in for no person and
enrolls nothing.

Such a pod is its person's own session in the table above. The person's agent-tier secrets are
granted to it at once, the person's human-tier secrets go to the person for approval, and another
person's secrets go to their owner. A `requesters` list that names the person admits the pod too.
Its request for a shared human-tier secret waits in the person's Inbox, as a request of the
person's box does. No service entry admits it, since its login service is no service the
deployment registers, and it gets no service's secret.

Such a pod names its Dispatch projects as its person's session does
([a private project's secrets](#a-private-projects-secrets)): `agent-secrets enroll --pod` sends
the list `AGENT_SECRETS_PROJECTS` holds, or `--projects` over it, and the broker takes it only when
Dispatch says the person may see every project on it. That list, and not the project of any
`BROKER_EXCHANGE_ACCOUNTS` entry of its account, is what the pod names for a private project's
secret, and each release asks Dispatch again whether the person still sees the project. So the pod
gets no secret that its person's own session naming the same list would not get. A restricted
member's pod enrolls nothing, naming projects or not, as that member's machine login enrolls
nothing: `403 RESTRICTED_MEMBER_ENROLLS_NOTHING`. A pod naming a project its person may not see,
or one Dispatch does not hold, is refused `403 PROJECT_NOT_VISIBLE`, with the same words for both.
While Dispatch cannot say what kind of member the person is, the pod enrolls nothing
(`503 MEMBER_KIND_UNCHECKABLE`), but for a person Dispatch last answered a full member, whose
kept kind the broker serves while Dispatch does not answer; on a broker with no
`BROKER_PROJECT_ACCESS_URL`, a pod naming a project is refused `503 PROJECTS_UNCHECKED` and
nothing is enrolled, and a pod naming none enrolls
as the person's session does. Only a person's pod names projects: any other pod naming one is
refused `400 PROJECTS_NEED_A_PERSON`.

The pod's login enrolls that pod alone and names no session, so it never takes over a session of
the person's box. The routes that list and revoke the person's machine logins and grants refuse the
login `PERSON_POD_LOGIN`, so the pod lists and revokes none of them. The routes a service calls as
itself refuse the pod's account `PERSON_ACCOUNT`. Dispatch's machine-login page lists the login to
the person alone, and only that person may revoke it. The pod logs in again with its own token
under a new credential, so a revoke ends what the login enrolled and does not keep the pod out.

The broker reads the setting at each decision about the pod, not only when the pod enrolled
(`requests.Machine.requester`). When the deployment takes the account out of the setting, or maps
it to another person, the pod's enrollment acts for no person from its next call. A grant the pod
got at once because it was the person stops releasing and is not reused, a grant a person approved
still releases, and a grant of a secret whose `requesters` names the person ends for good. It also
names no list of its own from then on, only its account's `BROKER_EXCHANGE_ACCOUNTS` project, as
any pod of no person does, so a grant of a private project's secret that its list named ends for
good at its next release. The enrollment's own record keeps the person it was made for, so the
grant log names them, and their **Live grants** still lists each grant of the pod they may see,
naming no operator on it: the pod no longer acts for them, so they end a grant only if they
approved it or its approver is `anyone`, and their revoke withholds nothing from the pod. A grant
the policy gave the pod without asking names no approver either, so it is listed, as a service's
pod's is, to every signed-in person who may see it, and its row reads **Ends with its machine
login**: revoking the pod's login ends it. A grant of a private project they are not on, which
the pod may now get through its exchange entry, is hidden from them as from anyone outside the
project, and is on the pages of that project's people. The person it was made for still decides
whether it gets anything at all: a broker with `BROKER_PROJECT_ACCESS_URL` asks Dispatch about that
person at each request, approval, release and airlock connection of the pod, so the pod of a person
Dispatch calls a restricted member is denied every secret `restricted-member` after the change as
before it, and the pod's requests and releases are refused `503 MEMBER_KIND_UNCHECKABLE` while
Dispatch cannot say what kind of member that person is and the broker last kept them as no full
member.
[Operate](/legion/broker/operate/#what-to-run) says what else a removal or a move does.

### A group's secret

A secret's owner may be a Google group, written `group:` and the group's lowercase email, and so
may an entry of its `requesters`. Each member of an owner group stands where the one owner of a
person's secret stands: a member's own session gets the group's agent-tier secret at once (a
restricted member's session gets none of the group's secrets, as it gets no other secret:
[Machine login](#machine-login)); every other request for it, and every request for its
human-tier secret, waits for the group's approval, which any member gives on the broker's page,
and any member dismisses once it expires; and a member the group's last good read listed,
however old that read is, lists and revokes the grants a member or the model judge approved
([Approvals](#approvals)). A member Dispatch calls a restricted member is none of that, and
decides, lists, revokes and dismisses nothing of the group's ([Machine login](#machine-login)).
A member may approve a request their own session made, as a sole owner may approve their own: a
group owner is not a second person, and no rule asks for one. A `requesters` entry naming a
group admits a session whose person is a member. A list, revoke or dismissal that names the
group itself as its approver, rather than a member, is refused
`400 APPROVER_IS_GROUP`.

The broker reads who is a member from Google Workspace, with the Directory API's `members.list`,
the members of nested groups included (`packages/envoy/internal/cipher/groups`). It reads a group
as soon as the five-minute read of the namespace names one it has not read yet, a group whose first
read failed included, and every group a served secret names once an hour after that; a broker that
starts waits up to 15 seconds for its first reads before it serves. A reread of one secret, such as
the one `agent-secrets secret` asks for after a write, names no group, so a `retag` that names a
group no served secret named before admits nobody until the next five-minute read has named it and
its first read of the group has finished. A read that fails keeps the members of the last one that
succeeded. A group whose last good read is more than 2 hours old, or that was never read, admits
nobody: no member's session gets its agent-tier secret at once, no member may decide its requests,
no Inbox lists them, and its `requesters` entry admits no session. The broker reads as a reader
account the deployment names (`BROKER_DIRECTORY_ACCOUNT`); with none, it reads no group, and every
group admits nobody ([Operate](/legion/broker/operate/#what-to-run)).

Who is a member is no part of the policy's version, so the broker checks every grant of a group's
secret again at each release and at each reuse, whatever the version, as it checks every grant of
a secret with a `requesters` list (`anyGroupOwned` in
`packages/envoy/internal/cipher/requests/machine_state.go`). A grant the session got at once stops
releasing (`GRANT_NOT_LIVE`) once the group no longer admits the session's person, a stale group
included, and is not revoked. A grant of a secret whose `requesters` names the group ends for good
once a read of the group, two hours old or newer, no longer lists the session's person, as it does
once the list drops a person. While the group is stale or was never read, the list cannot say who
is on it: a new request it decides is denied `requester-group-unread`, a live grant is paused
(`GRANT_NOT_LIVE`), not revoked, and releases again once a read lists the person, and an approval
of a pending request is refused and leaves it pending. A grant a member approved rests on that
member, and releases only while the group admits the member who decided it, whoever owns the
secret now. Where the group is stale or was never read, its release is refused as a lapsed grant
(`GRANT_NOT_LIVE`); where the group was read in the last 2 hours and does not list that member, its
approval chain is broken (`GRANT_CHAIN_INVALID`; [The audit record](#the-audit-record)). Each of the
two writes the log line `group approval lapsed`, naming the grant, the decider and the group
([Health and logs](/legion/broker/operate/#health-and-logs)). A member the group's last good read
listed may still revoke the group's grants while it is stale, since a revoke only takes power away.

A group gives the power to decide to whoever may change its members in Google Workspace: a person
added to an owner group may approve its requests from the broker's next read of the group, within
the hour, with no change to the secret's tags.

## Requests and grants

A session asks for one or more secrets by name in a **request**, signed with its key and carrying a
reason of at most 400 characters; a request that reaches a person must carry one.
The broker evaluates every name
(`packages/envoy/internal/cipher/requests/machine_state.go`):

- A name the policy does not serve, even after the reread above, refuses the whole request with
  `UNKNOWN_SECRET`, and nothing is recorded. A secret `agent-secrets secret create` made and has not
  yet filled, in the ten minutes it waits for its value, refuses the request with `PENDING_VALUE`
  instead, which is not final: ask again once its owner has stored the value. A source asked for
  by its own name while a profile of another name uses it refuses the request with `403`
  `SERVED_ONLY_AS_PROFILES`, naming the profiles of it the broker serves, to ask for instead, or,
  where the broker refuses each of them, saying so and naming them; and a profile the broker
  refuses, or a `github-app` secret no profile names, with `403` `PROFILE_REFUSED`, giving the
  reason and its detail.
- A name the policy denies denies the whole request; nothing is ever half-granted. Only a service's
  secret, asked for by anyone but that service, is denied.
- A request that reaches a person needs a reason. Where a name's answer would be an approval —
  a human-tier secret, a secret the session's operator withheld from it, or a person's
  `tier=agent` secret another session asked for — and the request's reason is empty once its
  spaces are trimmed, that name is denied instead, and one such name denies the whole request.
  The session reads `<NAME>: a reason is necessary; ask again with --reason` after
  `was denied: `, and the command exits 77. A request every name of which is granted at once
  reaches nobody, so it needs no reason, and a machine login, which is always signed with an
  empty reason, never passes this way.
- On a broker that signs nobody in, `BROKER_SIGNIN=off`
  ([Operate](/legion/broker/operate/#a-broker-that-signs-nobody-in)), nobody could approve such a
  name, so it is denied at once, with a reason or without one, and one such name denies the whole
  request: the session reads `<NAME>: a person must approve it, and this broker signs nobody in
  (BROKER_SIGNIN=off)` after `was denied: `, and the command exits 77. What the policy grants at
  once is granted there as everywhere.
- Names that need approval must all need the same approver, else the request is refused
  `MIXED_APPROVERS`; request them separately.
- A **type secret** has `type=github-app`, `type=github-installations`, `type=google-login`,
  `type=azure-login`, `type=google-service-account` or `type=anthropic-federation`, and a current
  stored value. A GitHub App value holds an App identifier and private key.
  Every GitHub App token the broker releases is a profile's (the profiles file, above), and a
  `github-app` secret no profile names is refused by its name (`403 PROFILE_REFUSED`,
  `github-app-no-profile`). A release holds one installation token for each account in effect,
  the profile's `github.accounts` or the request's target, cut to the profile's repositories or
  the target's: a JSON object of installations, or that one account's bare token for a profile
  whose `github.value` is `token`. A profile that lists `github.repositories` names, in them, a
  repository of every account it lists: the profiles file, at the boot and in `profiles check`,
  refuses an account that no repository is of, since the broker would never mint for it.
  The broker asks GitHub for the profile's permissions cut to what
  each installation holds, as its list of installations gives them: a permission the installation
  holds none of is left out, and one it holds at a lower level is asked for at that level. A GitHub
  profile names `metadata: read`, which GitHub grants with every installation token: the broker
  refuses a token holding a permission its request did not name, so the profiles file, at the boot
  and in `profiles check`, refuses a `github` block whose `permissions` leave `metadata` out or name
  it at any level but `read`. The values answer names each entry the cut left out
  (`github_left_out`), the broker logs one line for each, and `agent-secrets` writes one line on
  stderr for each account that names them. A cut that leaves no permission but
  `metadata`, of a profile that names more, refuses the release with no request to GitHub
  (`422 PROFILE_MINT_REFUSED`): every installation GitHub lists holds `metadata`, so that token
  would do nothing the profile is for, and GitHub gives every permission the installation holds
  to a request that names none. So does a `value: token` profile whose account the App has more
  than one installation on (logins that differ only in case), since a token minted for each would
  leave all but one live with nobody holding it. GitHub's own `422`, such as for a repository the
  installation does not reach, refuses it the same way. The broker releases a token only once
  GitHub's answer shows it holds no more than the request asked, for every profile, and gives back
  one that holds more (`502 MINT_FAILED`). A target is an entry of the signed request's
  `locations`, so the record a
  person approves holds it and the approval page prints it beside the profile's source,
  permissions, accounts, repositories and proxy hosts, and a Google profile's scopes. The values
  answer carries the grant's target back in `locations`, as the request signed it and sorted, and
  leaves the key out for a grant with no target, so a client that signed a target can see that the
  broker applied it. A target
  outside the profile, a target on a name with no `github` block, and no target on a
  `value: token` profile of more than one account are refused `400 TARGET_REFUSED` before anybody
  is asked, naming the profile's accounts and repositories. An approval given after a profiles
  roll took the target out of its profile is refused the same way and grants nothing, and a
  release of a grant whose target its live profile does not hold answers `400 TARGET_REFUSED`
  too (`403 TARGET_REFUSED` at the airlock's proxy) and mints nothing. A body that carries
  `github_app`, the narrowing an `agent-secrets` of a release before `--target` sends for a stored
  `github-app` secret, is answered until every client has moved, for the services and people
  `BROKER_GITHUB_APP_FIELD_REQUESTERS` names and for a person asking for their own App: the broker
  serves the request as the profile of that secret the profiles file grants the caller at once
  whose scope holds the narrowing (the one with the most `write` permissions, the stored name and
  then the file's order breaking a tie), with the narrowing's account and repositories as its
  target, and releases it under the stored secret's name in the `installations` form, outside the
  airlock, echoing `github_app`; a read-only body gets each of that broker's read set — `actions`,
  `checks`, `contents`, `metadata`, `pull_requests` and `statuses` — at read where the profile
  holds it, and none it does not. `BROKER_GITHUB_APP_FIELD_SECRETS` names the stored secrets this
  may hand over at all, and a body for any other is refused.
  Such a grant releases only through that field, with the same `read_only`, and no `--target`
  grant is released through it. With `BROKER_GITHUB_APP_FIELD=off`, for a requester the list does
  not name, and for a body it cannot serve, the field is refused `400 GITHUB_APP_FIELD_REMOVED` on
  `POST /v1/requests` and
  `POST /v1/grants/{id}/values`, never `INVALID_REQUEST`, so a client of an earlier release does
  not send its request again without it. Where a person must approve a request that names a GitHub
  profile, the broker first mints the token the release would mint, for the target's account or
  the profile's accounts, and revokes it at once, so a profile it cannot mint costs nobody an
  approval: an App with no installation on an account in effect, a cut that leaves no permission
  but `metadata` and a repository the installation does not reach are each refused
  `422 PROFILE_MINT_REFUSED` then, and nobody is asked (the release of a grant answers the first
  `502 MINT_FAILED`).
  Where it reaches no verdict, the request waits for its approver as before and the release makes
  the refusal. GitHub gives no verdict when it is out of reach, when it answers a 5xx or a rate
  limit, and when it refuses the App itself with a `401`, `403` or `404`. The broker reaches no
  verdict when it cannot read the stored value or mint from it, when the check passes its own
  bounds (ten seconds for the whole check, five for one mint) or its caller goes away, when the
  enrollment has used its checks (five at once, one more every twelve seconds), and when it
  cannot read the requester or has no minter for the profile's type.
  A profile whose source's `proxy` tag names a kind is behind the airlock
  ([The airlock](#the-airlock)): its release hands the command a placeholder, and the proxy mints
  the profile's token itself, for the grant's target and with the same cut, keeps it until five
  minutes before it expires, and puts it in place of the placeholder only in the requests its kind
  and its own `allow` allow.
  A `github-installations` value holds installation tokens that a minter outside the broker
  minted before any request and stores again every few minutes, so the broker holds no App key
  for them: `{"shapeVersion": 1, "installations": {"<account login>": {"installation_id": 42,
  "token": "…", "expires_at": "<RFC 3339 time>", "permissions": {"contents": "read",
  "metadata": "read"}, "repository_selection": "all"}}}`. For an installation on selected
  repositories, `"repository_selection": "selected"` comes with one of three: the token's own list
  `"repositories": ["<account>/<name>", …]` with the installation's own selection
  `"installation_repositories"` beside it, for a token the minter asked for with no repositories
  and listed with that token; the token's own list alone, for a token the minter cut to a
  profile's repositories; or the seed minter's marker, below. The broker reads `shapeVersion` 1
  alone: a value of another version, or of none, refuses the whole value and serves nothing of the
  secret, and the refusal names the secret, the version read and the versions the broker knows.
  It reads the rest strictly too: a field it does not know or one in another case, a key twice in
  any object, a key that is no GitHub login, one account twice, or a token, expiry, permission,
  level or repository list it cannot read refuses the whole value (`value-malformed`). It is
  served as a `github-app` secret is, only through profiles on the
  accounts its `github-owners` tag lists, and its release has the same form: the installations
  object, or the one account's bare token for `value: token`. The broker mints nothing for it, and
  a token cannot be cut once it is minted, so the broker holds each stored token to the profile
  before it hands it over. What it holds the token to is what the minter declared beside it: the
  permissions, the repository lists and the marker. The broker cannot tell that the token under an
  account is that account's, or that it holds what its entry says; the minter and the deployment's
  appliers, which alone may write the secret, and the deployment's checks of its output stand
  behind that. A token that
  holds a permission the profile does not name, or a level
  above the profile's, is refused `422 PROFILE_MINT_REFUSED`. A token that holds less than the
  profile is served, and the values answer names each permission it lacks (`github_left_out`), as
  the cut of a `github-app` profile does; a token that holds none of the profile's permissions but
  `metadata` is refused. A profile that names `github.repositories` of an account is served only a
  token whose own list is the same, compared without regard to case. A profile that names none
  claims every repository the installation reaches, so it is served a token of every repository,
  the seed minter's marked token, or a token whose own list equals the installation's own
  selection, the order ignored; it is refused a token with no `installation_repositories` and one
  whose two lists differ by a repository, and a profile that names repositories of a token of
  every repository is refused too. The deployment's
  seed minter cuts one token to every repository of the installation but the protected ones and
  marks it `"repository_scope": "all-except-protected"`, beside `"repository_selection":
  "selected"` and no list. Only that minter writes the marker, into a secret its deployment lets
  only its minter and its appliers write, so the broker trusts it for the list the token was cut to
  and serves the token to a profile that names no repositories; the proof of the cut is the
  deployment's own check. Any other
  value of `repository_scope` refuses the value, and a `selected` token with no list and no marker
  is refused. A target of such a
  profile names its account alone: a target that names a repository is refused
  `400 TARGET_REPOSITORIES_REFUSED` before anybody is asked. The broker hands a stored token over
  only while more than 20 minutes of its own life are left, so a caller never gets a token about
  to expire: the Legion daemon serves a cached installation token until 20 minutes before its
  expiry, and a minter that stores new tokens every ten minutes, of the hour GitHub gives a token,
  leaves each at least 50, so three of its runs in a row may fail before a release is refused. The
  margin does not bound the grant: GitHub fixes a token's life at one hour, so a token can outlive
  its grant by up to an hour, and the broker adds no revocation. A stored token with 20 minutes or
  less left is refused `502 MINT_FAILED` (`class=upstream`), and the airlock's proxy, which reads
  the grant again five minutes before a token comes within the margin, answers a call it cannot
  carry `503` until the minter stores a newer token. The broker gives no such token back to
  GitHub: it is the stored one, which every other release of the secret hands over too. Where a
  person must approve a request for such a profile, the broker makes the same checks on the
  stored token before it asks anybody.
  The exec form puts a profile's release in the command's environment under the profile's name
  as the broker hands it over: the JSON object of installations, or that one account's bare token
  for a profile whose `github.value` is `token`, which is what a command sends to GitHub.
  `agent-secrets NAME --target ACCOUNT -- command`, or one `--target ACCOUNT/REPOSITORY` for each
  repository of one account, signs the target into the request and narrows a run of that one
  name; the client refuses a target of two accounts, or a run of two names with a target, with
  exit 2 before it sends anything. The release answers the grant's target as its `locations`, and
  the client runs nothing, exiting 2, when a release does not answer the target its request
  signed, as a broker from before `--target` releases the whole profile. A Google login
  value holds an `authorized_user` refresh-token credential. Its release returns a bare access
  token. A Google service-account value names one account,
  `{"service_account": "<id>@<project>.iam.gserviceaccount.com"}`, and its release returns a bare
  access token for that account: the broker impersonates it with its own Google credential
  through the IAM Service Account Credentials API. The account is the deployment's to choose,
  not the value's: `BROKER_SERVICE_ACCOUNT_SECRETS` binds each such secret to the one account the
  broker may mint for, and a secret it binds to no account, or whose stored value names another
  account than the bound one, releases nothing: the session is answered `MINT_FAILED` and the
  broker logs the failure as `class=binding`, before any call reaches
  Google. The token carries the scopes the secret's `scopes` tag names, or
  `https://www.googleapis.com/auth/cloud-platform` alone where it names none, and lasts an hour
  whatever lifetime the grant that released it has, since Google bounds the token itself: a
  shorter grant stops the next release rather than cutting the token short. That token acts as
  the account itself, which is why the secret is served to the one person who owns it, or to the
  members of the one group that does, whom its `requesters` tag names, beside the registered
  services a human-tier one may list, and for grants of an hour at most. An Anthropic federation
  value names an Anthropic organization's ids and one Google service account,
  `{"organization_id": "<uuid>", "federation_rule_id": "fdrl_…", "service_account_id": "svac_…", "service_account": "<id>@<project>.iam.gserviceaccount.com"}`,
  with `"workspace_id": "wrkspc_…"` where the federation rule is enabled for more than one
  workspace, and its release returns a bare Anthropic access token (`sk-ant-oat01-…`): the broker
  mints a new Google identity token of the bound account for the audience
  `https://api.anthropic.com`, with the account's email in it, through the same API, and trades it
  once at Anthropic's `POST /v1/oauth/token`
  ([Anthropic's reference](https://platform.claude.com/docs/en/manage-claude/wif-reference)). The
  same setting binds the secret to its account, with the same refusals, and the same `requesters`
  rule holds, but the type is human tier alone (`type-human-tier`), so no session gets a grant of
  it without asking, and a person approves every release of it: the model judge decides none,
  whatever seconds its `judge` tag gives the judge ([The model judge](#the-model-judge)).
  The federation rule's `token_lifetime_seconds` must be 3600 or less, since a grant of the secret
  lasts an hour at most; the broker refuses a longer token, naming that setting. The minted value
  expires at the token's own end, which the airlock's proxy reads to mint again five minutes before
  it, as it does for every minted value; the release's `expires_at` is the grant's end. The broker
  reads the stored value for each release, as it reads any secret, and mints from
  it. It never returns the stored App key or refresh token. It also reads each type secret's value
  once for each new version or type, and checks it as the minter reads it, so a value the
  minter could not mint from leaves the secret out (`value-malformed`) before anyone asks for it.
  `agent-secrets secret` checks a value the same way before it writes one. A token lasts about an
  hour, so a client
  keeps the release until its `expires_at` rather than asking again for each use. A deleted type
  secret stops releasing as any deleted secret does. A call to GitHub or Google that answers a
  5xx or a rate limit, or gets no answer, is attempted up to three times within the release
  ([the mint retry](/legion/broker/operate/#the-mint-retry)). A release GitHub or Google refuses,
  such as a Google login that needs a new sign-in, is refused `MINT_FAILED` with the upstream's
  HTTP status code, Google's `invalid_rapt` or `rapt_required` subtype (any other is named
  `unknown`), or the class of a failed call, never the upstream's own text, and the broker logs it
  for its operator.
  A grant ends for good when a release finds its secret's type changed
  (removed, or changed to another type) or its lifetime over the current `max-grant`: that
  release answers `GRANT_NOT_LIVE` and writes a `grant.revoked` audit row by
  `broker:release-recheck`, and the session asks again. A release of a stored secret checks the
  policy again once it has read the store, so a retag to a type reread during that read releases
  nothing and ends the grant the same way: retag a stored secret to a type, and wait for the broker
  to read the new tags, before storing its App key or login. That retag checks the value the
  secret holds, so a sign-in that may read it is refused while it is no value of that type
  ([manage a secret](/legion/broker/guides/manage-a-secret/#change-its-owner-tier-type-requesters-or-max-grant)).
  A change reverted before any release finds
  it leaves the grant as it was: it was approved for that secret, type and source, and its next
  release mints a token of that same type from that same frozen source, never the stored value. To
  end a grant, revoke it. A type secret the broker stops serving, such as one whose
  `github-owners` is removed or malformed, stops releasing until the secret is served again.
- A secret with `max-grant` needs a lifetime no longer than that duration. A request whose `--ttl`
  (or, with none, the default of 15 minutes for a human-tier secret and 12 hours otherwise, or session length under `default-grant=session`) is longer is refused `GRANT_REQUIRES_LIFETIME` before the broker writes a request. A
  `max-grant` set or lowered later holds what came before it to the new bound: a release that finds
  a live grant lasting longer ends it for good (`GRANT_NOT_LIVE`), and a pending request that asked
  for longer is no one's to approve (`NOT_APPROVER`), so its session cancels it and asks again
  within the bound.
- When every name is automatic, the request is granted at once.
- Otherwise it is **pending**: the request carries a reason, and the broker writes a
  credential-request record for the approver to
  decide. A session has at most one pending request for each secret: an identical request from the
  same session while one is pending (the same names and the same lifetime, or none) joins it
  rather than asking twice, and any other request naming one of its secrets, with or without
  `--ttl`, is refused `REQUEST_PENDING`, naming the pending request. The session waits for that
  request to be decided, or cancels it (`agent-secrets cancel`) and asks again. The one exception
  is a pending request whose record is signed for another broker address, another
  `BROKER_PUBLIC_URL`, such as a broker the session's box asked before it moved to this one: no
  person and no judge here can decide it, so when its session asks this broker again for one of
  its names, the broker ends it, cancelled by the broker with a detail that says the broker moved,
  whatever its age. That end holds for an ask that waits for a decision, the judge's or a
  person's, for an ask the broker decides at once and for one a live grant answers, and for an ask
  that waits for a decision the rule then applies to what is left. A request whose deadline has
  passed is expired rather than pending, whichever broker address it is signed for: the ask
  neither joins it nor is refused for it, and the broker's sweep ends it. An ask refused before
  the broker writes a request (an unknown name, a lifetime over the secret's maximum, two
  approvers) ends nothing, and a request signed for another broker address that no such ask
  reaches waits for its expiry.
  The broker's answer names the approver the record names and the request's page on the broker
  (`approve_url`), and `agent-secrets` prints both, so the agent can tell that person where to
  approve it; while the [model judge](#the-model-judge) decides a request, its answer carries
  `judge_deciding: true` in place of a page. The record
  fixes its approver when the request is made, and the request stays in that
  person's Inbox after the secret's tags change, though they may then be unable to approve it
  ([what an owner change does](#approvals)).

A granted request yields a **grant**: the session's right to read those values until the grant
ends. A grant lasts the lifetime its request asks for
(`agent-secrets NAME --ttl 2m -- command`, or `agent-secrets request NAME --ttl 2m`), counted from
the moment it is granted. A request that names no lifetime gets 15 minutes when any secret it names is human tier and 12
hours otherwise, or, when every secret it names carries the tag `default-grant=session`, a grant
that lasts until its session ends. The short human-tier default is the window in which the broker
releases and reuses that credential, so a session that needs it for longer names a `--ttl`, which
the approver reads on the card before deciding. A grant
also always ends with its session, and its session is a session name under one principal,
not the box the request came from.
Any other value of `default-grant` is refused
(`default-grant-malformed`). A secret with `max-grant` requires that shorter
lifetime and bounds it. The approver sees the lifetime the request asked for before deciding it.
While a grant lives, a new request from the same session for exactly the same names and the same
lifetime gets the same grant back, without asking anyone again, as long as the current policy still
allows it.

A grant binds to a session name under one principal.
The box is still there: the grant hangs off one enrollment at a time, and a session reads it only by
proving that enrollment's key.
The session name, with the principal beside it, is what decides which enrollment that is.
The principal is what the enrolling box proved, never anything its launcher states: the person, for
a box of theirs and for their sign-in on a machine; the fleet's Google account, for a machine's own
session; for a pod, the service account its projected token proved, paired with the account the
token login that enrolled it proved
(`packages/envoy/internal/cipher/enroll/enroll.go`, `carryPrincipal`).
A pod's grants follow that pair because the pods of several daemons can run as one account, and each
daemon logs in as an account of its own: a pod one daemon enrolled under another daemon's session
name takes none of that session's grants. A pod enrolled under a login that proved no account
carries nothing.
The broker records which principal holds a name, on the row the token exchange binds every token's
name in, so the name is one principal's whichever of the two claimed it first; a pod's name is held
under its service account alone, the principal its own token exchange claims it under.
An enrollment that names no session belongs to no principal at all.

Enrolling the same session name again under the same principal takes what every earlier enrollment
of that pair still holds.
Its grants move to the new enrollment, with the requests it is still waiting on a person for and the
names its operator withheld from it, and the move revokes and cancels none of them
(`enroll.go`, `adopt`).
An approval given after the move lands on the session as it stands then, because a decision grants to
the enrollment the request row names
(`packages/envoy/internal/cipher/requests/machine_state.go`, `ApplyDecision`).
That is how a grant follows a session to the next box it comes up on: a helper restarts, a machine is
rebuilt, a person signs in on a second machine, a Legion pod is replaced.
Each move is in the audit record, one `enrollment.adopted` row naming both enrollments and how much
moved, and one `grant.moved` row per grant, naming the grant, the enrollment holding it now and the
one it came from.

Three things take nothing.
A session name another principal holds is refused, unless that principal is a service account
paired with the enrolling pod's (below): the enrollment is refused `SESSION_HELD`, naming the
session and no holder, and writes nothing (`enroll.go`, `bindSessionName`).
A token exchange under that name is refused the same way, on the same row, so the name stays its
principal's for 90 days past the last lease or token under it
(`packages/envoy/internal/cipher/oauthstore/exchange.go`, `ErrSessionHeld` and `SessionRetention`).
A pod's name is held under its service account alone, though, so a pod another daemon enrolled
under the name, running as the same account, is not refused it: its grants follow another pair,
so it is admitted under the name and takes none of what the session holds (`enroll.go`, `adopt`).
A box whose key a person revoked trades nothing, so it enrols nothing: a launcher proof signed with a
revoked credential does not authenticate, and an enrollment already under way finds the credential
revoked and is refused `LAUNCHER_INVALID`.
A session enrolling under a name of its own has no earlier enrollment of that pair, so it starts with
nothing but the grants it asks for.

Two Kubernetes service accounts can be paired so that either takes a session name the other holds:
`BROKER_SESSION_ACCOUNT_PAIRS`, which an operator sets before moving a deployment's pods from one
account to the other ([Operate](/legion/broker/operate/#what-to-run)).
A pod's principal is the account its projected token proves, so without the pair a role's next
pod, on the new account, is refused its own session name, and what the name carries stays behind.
With the pair, the claim takes the name, whether an enrollment or a token exchange makes it
(`oauthstore/exchange.go`, `BindSession`).
An enrollment under the name then takes what the other account's enrollments of it still hold, by
the rule above for a successor under the same principal: the grants, the pending requests and the
withheld names (`enroll.go`, `adopt`).
A pod's grants follow its account and the account of the login that enrolled it, so the take
reaches only the other account's pods that a login of the same account enrolled: a pair moves a
session between the two accounts one daemon runs its pods as, and never from a pod another daemon
enrolled.
A pair works both ways, so pods moved back take the names back the same way.
A pair does not chain: a claim reads the claimant's own pairs, and every pair works both ways, so
with A paired with B and B with C, C takes a name B holds and never one A holds.
No pair takes a name an entry of the token exchange fixes.
A take holds the name for the taker's own claim, 90 days past its own lease or token, and not for
what was left of the holder's.
Two claims racing for one name end with one holder.
The broker logs each take at WARN once it has committed:
`broker: an enrollment took a session name from the account paired with its own`, naming the
session, the principal it took the name from (`from`), the one that took it (`to`) and the
enrollment, or `broker: the token exchange took a session name from the account paired with its
own`, naming the session, `from` and `to`.

A grant that moves still ends, three ways.
It ends when its own lifetime runs out, whichever box holds it by then.
It ends when the carry window closes: an enrollment the box's end left dormant holds what it holds
for `BROKER_SESSION_CARRY_SECONDS`, 12 hours by default, and the sweep then revokes those grants and
cancels those pending requests for good, with an `enrollment.carry_expired` audit row
(`enroll.go`, `EndCarried`).
Those 12 hours are the longest lifetime a request naming none gets (one naming a human-tier secret
gets 15 minutes), so the default carry is never longer than the longest grant given by default; 0
turns the carry off, and the broker refuses a window longer than a day.
And it ends at once when a person revokes the grant itself, the machine login behind it, or their
sign-in on a box.
The two login revokes reach the dormant enrollments that login made as well as its live ones, so a
grant a person has just taken back never waits on the session's next box
(`enroll.go`, `endCredential`).

A request is wider than its session: it belongs to the principal that made it, which the broker
records on every enrollment when it is made, from what the enrolling box proved and, for a pod, the
claim its launcher states (`enroll.go`, `ownerKey`).
That owner is the person, for a box of theirs and for their sign-in on a machine; the Legion claim
under the account the launcher's own login proved, for a pod the Legion daemon enrols, which states
the claim token of one agent of an issue (`legion-<project>-<issue>-<agent>`) for every generation
and every replacement pod of that agent, whatever role its step is on; and the machine itself, by
the subject its credential proved, for a machine's own session.
Every project's daemon logs in as the same service under an account of its own, so a daemon whose
pod states another project's claim token is another owner and takes nothing of that claim's.
A pod that states no claim is its own pod's, and each slot of it, one process of an issue pod, is
its own slot's, so the processes of one pod read none of each other's requests.
Any enrollment of the same owner reads the request (`agent-secrets status`) and cancels it
(`agent-secrets cancel`), and every other one is refused `NOT_YOURS`
(`packages/envoy/internal/cipher/requests/machine_read.go`, `OwnerAndState` and `Machine.owner`):
a person reads their request from another box, the next pod of a claim cancels what the pod before
it asked for, and a second machine of the same fleet, or another agent of the same issue, reads
neither.
A request of a private Dispatch project, by a secret's project tag, a project a credential it has
had names, or an issue key its reason names, is read and cancelled, apart from the enrollment it
hangs off, only by an enrollment whose own credential names that project for a person still on the
project's list. Every other enrollment, of the same owner or not, is answered `404 NOT_FOUND`, as
for an id no request has (`hide.go`, `HideRequestFromSession`), so `NOT_YOURS` never says
that a private project's request exists. A person reads such a request from another box only when
that box's session named the project, and the next pod of a claim cancels it only when its account
maps to the project.
An enrollment that records no owner, every one made before the broker recorded owners, reads and
cancels its own requests alone.
A pod that `BROKER_POD_OPERATORS` maps to a person records that person as its owner, as the
person's box does ([a person's pod](#a-persons-pod)).
The broker reads the setting each time it compares owners, and the pod keeps that owner only while
the setting maps its account to that person.
After the deployment takes the account out of the setting, or maps it to another person, the pod
reads and cancels its own requests alone, and the person's sessions read and cancel none of them.

Reading and cancelling is all the owner adds for a person and for a machine: their grants still
follow the session that asked, so a person's other session sees the request but never takes its
grant, and a session that resumes on another box gets its own grant back even after another
session of that person enrolled first.
A claim is the one owner a grant follows.
A role's next pod that resumes the claim's session takes what that session left by the carry, as
any session does, but one that comes up under a new session, given the claim afresh, is one the
carry cannot follow, so the next enrollment of a claim also takes the grants and the pending
requests a dormant enrollment of that claim still holds, after the carry has taken what its own
session left; a pod of the claim that is still running keeps what it holds (`enroll.go`, `adopt`).
The next pod can also come up before the pod before it is ended, when the daemon's revoke lands late
or never and the old enrollment lives until its lease lapses.
Then the end of the old enrollment, when it comes, hands what it holds to the running pod of the
claim instead of leaving it dormant (`enroll.go`, `handOver`).
So a person who approves a role's request after its pod is gone has the grant land on the role's
next pod, which reads the value, and that pod asking for the same names again joins the request
rather than putting a second card in the approver's Inbox.
A pending request that moves to the next pod names that pod's session for its wake, so the
request's expiry wakes the pod waiting on it, not the session of the pod that is gone.
Each move is in the audit record as the carry's is, its `enrollment.adopted` row naming the rule
`claim`, and an end that handed over names the pod it handed to (`handed_to`).

Within one principal the session name is asserted, not proved.
The name rides in the enrollment request's body, outside the launcher proof, which signs the call's
method and URL and nothing else
(`packages/envoy/internal/cipher/proof/proof.go`, `Verify`).
So any holder of a launcher credential of the same person, or of the same Google account, can enrol
under that session's name and take what the session holds.
The session it takes from need not have ended: a carry takes from every enrollment of the pair that
still holds something and has not been ended for good, a live one included
(`enroll.go`, `adopt`).
The box it took from keeps its enrollment and loses the grant: the grant hangs off the new
enrollment now, and the old enrollment's read of it is refused `NOT_YOURS`, or, for a grant of a
private project the old enrollment's credential does not name, `GRANT_NOT_LIVE`, as for an id no
grant has (`packages/envoy/internal/cipher/requests/machine_read.go`, `Values`).
Taking a grant releases its values at once and asks nobody, where a request for the same human-tier
secret under a name of its own would wait for the person who owns the secret
(`packages/envoy/internal/cipher/policy/policy.go`, `Evaluate`).
So a holder of that person's or that account's credential who knows a session's name gets what that
session was approved for, with no approval of its own.
Within one principal what stands between a caller and a secret is the sign-in on a box, or the
account, and never the session name.
Three things bound it.
Another principal is refused the name, but for a service account paired with the holder's (below),
and a pod of another daemon, which shares the account the name is held under, is admitted under it
and takes none of what the session holds.
Every adoption is in the audit record, naming both enrollments.
And a person's revoke ends the grant at once.

The broker cannot tell an operator moving pods from one account to another from an attack, so a
pair of service accounts is the operator's word that the move is intended.
While it stands, a pod of either account can claim any session name the other holds, not only its
own claim's, so a pod of one project's account can claim a name a shared account holds for another
project's claim.
It takes none of what that session holds, because a pair moves what a session holds only between
the two accounts' pods one daemon launched (`enroll.go`, `adopt`).
Two things bound this for Legion's pods: the daemon gives each claim only its own session's name,
the name of the session its agent keeps whatever role its step is on, and a name leaves the shared
account once that claim's pod has moved.
Remove a pair once no live pod runs as the old account.

A claim is asserted the same way: it rides in the enrollment's body, and it is the launcher's word,
so a holder of the Legion daemon's login can state any claim of the daemon's service and take what a
pod of that claim leaves, whether it enrols after that pod went dormant or runs when that pod ends.
A pod enrolling itself states none, `400 INVALID_CLAIM`, because a claim it picked for itself
would make it a sibling pod's owner, and neither does a pod under a service login a person
approved, which no token proved to be the launcher's
(`enroll.go`, `podUnderTokenLogin`).

The broker never stores a value. Each time a session reads a grant, the broker checks that the
session is still enrolled, the grant is live, its approval still verifies, and the current policy
still allows every name; then it reads the value from the secret store (AWS Secrets Manager in
production) and returns it to that session alone. `agent-secrets NAME -- command` puts each value
in the command's environment under its name and replaces itself with the command. `agent-secrets`
itself never prints a value, but the command is the agent's choice, and a command can print or send
what its environment holds (`printenv NAME` prints it). A session holding a grant can therefore read
the value: approve a secret only for a session you would trust with the value itself. A secret
behind the airlock is the exception: the broker releases a placeholder for it and never the value
([The airlock](#the-airlock)).

A grant ends when its lifetime runs out, when its session revokes it (`agent-secrets revoke`, or
`agent-secrets grant revoke` run inside the session), when
its approver or the person its session acts for revokes it in Dispatch or from one of their machines
(`agent-secrets grant revoke`), and when the enrollment holding it ends for good: a person's revoke
of the machine login or the sign-in behind it, or the carry window closing on an enrollment the
box's end left dormant.
A grant the session got without asking
is revoked like any other, and when its
operator revokes it the session asks for approval before it gets the same secrets again (the operator
[withholds](#owner-and-tier-who-may-have-which-secret) them, even when the grant had already
ended); a session revoking its own grant, and anyone but the operator revoking one, withholds
nothing. To end every session's automatic access to a secret, change its tags
([revoke a session or a grant](/legion/broker/guides/revoke-a-session/#end-every-sessions-access-to-a-secret)).

## The airlock

A deployment can keep a secret's value from its sessions altogether. The broker reads a profiles
file when it starts (`BROKER_PROFILES_FILE`, a `profiles.json`) whose `kinds` list declares the
airlock's kinds (`packages/envoy/internal/cipher/proxyrules`, `Kind`). A kind says where a
credential of it may go and how it goes in, for every secret of the kind:

```json
{"kinds": [
  {"name": "anthropic", "hosts": ["api.anthropic.com"], "headers": ["Authorization"],
   "variables": ["ANTHROPIC_ADMIN_TOKEN"], "ways": ["mint"], "max_grant": "1h",
   "allow": [{"method": "GET", "pathPrefix": "/v1/organizations/"},
             {"method": "POST", "pathPrefix": "/v1/organizations/"},
             {"method": "DELETE", "pathPrefix": "/v1/organizations/"}]}
],
 "profiles": [ ... ]}
```

- `name`: one lowercase word, `a` to `z`, the service's word where it has one.
- `hosts` (at least one): the hosts a credential of the kind may reach, each on port 443.
- `headers`: the request headers that may carry it, `authorization` when the kind names none.
- `allow` (optional): rows of a method and a `pathPrefix`. A prefix holds a path that equals it or
  continues it at a `/`, so `/storage-limit` holds `/storage-limit/7` and not
  `/storage-limitX`. With no `allow`, any method and path on the kind's hosts; an empty list
  allows nothing. A kind's row names no host.
- `ways` (at least one): how a credential of the kind goes into a call: `swap` (a stored value
  swapped in), `mint` (a token the broker mints and swaps in), `sign` (an AWS key pair, signed
  with again), `exchange` (an Entra identity, exchanged at Microsoft's token endpoint) or `dpop`.
- `variables`: the client variables each placeholder of the kind is to go into. The broker reads
  and checks the list (each a shell name) and `envoy-broker profiles check` prints it, but no
  release applies it yet: a command still reads a placeholder in the variable named as its secret,
  for example `ANTHROPIC_ADMIN_HDO`, and a later change puts it in the kind's variables too.
- `max_grant`: a Go duration that caps the `max-grant` tag of each secret of the kind.
- `placeholder_prefix`: the prefix each placeholder of the kind opens with, in place of
  `agent-secrets-placeholder-`, for a client that checks a key's shape before it sends it.

A stored secret is behind a kind when its `proxy` tag names that kind. Its `type` tag picks the
way it takes, and the kind's `ways` must hold it: no type takes `swap`, or `sign` for an AWS key
pair under a kind that lists `sign` and not `swap`; `github-app`, `github-installations`,
`google-login`, `google-service-account` and `anthropic-federation` take `mint`; `azure-login`
takes `exchange`. A profile is behind its source's kind. Each policy load refuses the secret, by
its name, when its `proxy` tag names no kind of the file (`proxy-kind-unknown`), when its way is
not one of the kind's (`proxy-way-not-in-kind`), and, for a kind with a `max_grant`, when it sets
no `max-grant` or one longer than that (`proxy-max-grant-above-kind`). Tags change at run time,
so the rules the proxy decides from are rebuilt at each policy load.

A profile may narrow its one source further with an `allow` of its own: the proxy forwards a
request only when the kind's `allow` and the profile's `allow` both allow it, so a profile narrows
its kind and never widens it. A profile's row may name a `host`, which must be one of its kind's
hosts, and then allows the request on that host alone. Each policy load refuses a profile row that
no row of its kind covers (same method, no host or the same host, and a prefix that the kind's
prefix holds at a `/`: `profile-allow-outside-kind`) and a row whose host the kind does not bind
(`profile-allow-host`). The narrowing must also be in the store: a profile with an `allow` whose
source does not carry the tag `proxy-narrowed=true` is refused (`profile-allow-not-narrowed`), and
a secret tagged `proxy-narrowed=true` is served through exactly one profile, which has an `allow`.
With no profile, with a profile that has no `allow`, or with more than one profile that names it,
the secret and each of those profiles are refused (`proxy-narrowed-profiles`), and a
`proxy-narrowed` tag of any value but `true` refuses the secret (`proxy-narrowed-malformed`). So
a reviewed change of the file alone cannot take a narrowing away. A profile whose source is a
stored token, a `google-login`, an `azure-login` or an `anthropic-federation` secret with no `proxy`
tag is refused with its source (`profile-source-not-proxied`): without the refusal the source
would be released to an approved command as its value, and the profile's `allow` would never be
read.

Two forms of a profile's `proxy` key survive while secrets move to kinds, and each goes when what
it stands in for is in place. A profile may name a kind as the string value of its `proxy` key,
`"proxy": "github"`, and is then served behind that kind as if its source carried the tag; a
string that names another kind than the source's tag is refused (`proxy-kind-conflict`), and one
that names the same kind is served with one warning line at each full load. The string goes once
every source it is written for carries its `proxy` tag. And a `proxy` object of kind `dpop` alone
(below) is kept until a `dispatch` kind with the `dpop` way serves the broker's minted Dispatch
tokens. In a file that declares a kind, every other `proxy` object is refused, naming `kinds`.

A profiles file that declares no kind, as every deployment's file did before the kinds, is read as
the broker before the kinds read it, so a broker of this release starts against it. Each `proxy`
object of no kind, of kind `aws` or of kind `azure` serves its profile's name behind the object's
`hosts`, `headers` and `allow`, in the way its kind took: no kind swaps a stored value or a minted
token in, `aws` signs with a stored key pair, and `azure` exchanges an `azure-login` token. The
name is behind a kind of its own, named for the profile (`DEEL_API_KEY`'s is `deelxapixkey`). No
`proxy` tag can name that kind, so a secret tagged `proxy` under such a file is refused as naming no
kind (`proxy-kind-unknown`). The name's fingerprint is the one the broker before the kinds gave it,
the whole object with its source's `judge` seconds, so it does not move at the upgrade, and a change
of the object's `kind`, `hosts`, `headers` or `allow` ends the name's live grants. A `judge` or
`guidance` in such an object is refused, naming the source's tag that holds it now. This reading is
transitional: it goes once every deployment's file declares kinds.

Who decides a request for a value is no part of a kind: that is the secret's own `judge` and
`guidance` tags ([The model judge](#the-model-judge)), and a profiles file that still sets `judge`
or `guidance` is refused, naming the tag that took its place. Every kind names at least one host:
a kind with no `hosts` would hold every value of it back and carry it nowhere, so the broker
refuses to start with a file that holds one, and `envoy-broker profiles check` refuses that file
too. Those hosts carry no port, so a kind binds a name on port 443 alone, and another port of that
name is another service the proxy tunnels unread.

The kinds a deployment declares are its own. These are the kinds the airlock's design names, each
with what narrows it and how its credential goes in:

| Kind | Its credential | How it goes in | Hosts | Headers | Longest grant |
| --- | --- | --- | --- | --- | --- |
| `github` | a GitHub App, or a stored token | mints an installation token; a stored token is swapped as it is | `api.github.com`, `github.com`, `uploads.github.com` | `Authorization`, also inside `Basic` | 12 hours |
| `slack` | a bot token | swaps it in | `slack.com` | `Authorization` | 12 hours |
| `aws` | a stored key pair | signs again (Signature Version 4) | `sts.amazonaws.com`, STS's regional hosts, and each other AWS host a consumer calls | the signature | 12 hours |
| `gcp` | a person's login or a service account | mints an access token | `*.googleapis.com`, `*.pkg.dev` | `Authorization` | 24 hours for a login, 1 hour for a service account |
| `gws` | a person's Workspace login | mints an access token | `*.googleapis.com` | `Authorization` | 24 hours |
| `azure` | an Entra identity | exchanges at Microsoft's token endpoint, then swaps the access token in | `login.microsoftonline.com`, `management.azure.com` | `Authorization` | 1 hour |
| `tailscale` | an API key, or an OAuth client | swaps an API key in | `api.tailscale.com` | `Authorization` | 12 hours |
| `datadog` | an API key, an application key or an access token | swaps it in | `api.us3.datadoghq.com` | `DD-API-KEY`, `DD-APPLICATION-KEY`, `Authorization` | 12 hours |
| `openrouter`, `typesafe`, `deepgram` | an API key | swaps it in | `openrouter.ai`, `api.typesafe.ai`, `api.deepgram.com` | `Authorization` | 12 hours |
| `tinker` | an API key | swaps it in; its placeholder opens with `tml-` | `tinker.thinkingmachines.dev`, `inference.thinkingmachines.dev` | `X-API-Key`, `Authorization` | 12 hours |
| `elevenlabs` | an API key | swaps it in | `api.elevenlabs.io` | `xi-api-key` | 12 hours |
| `anthropic` | an Anthropic organization's token by workload identity federation (`anthropic-federation`) | mints the token at Anthropic's token exchange and swaps it in; the kind's `allow` is `GET`, `POST` and `DELETE` under `/v1/organizations/` | `api.anthropic.com` | `Authorization` | 1 hour |
| `evals` | a ClickHouse login's password | swaps it in, at a private destination | `transcript-store.<tailnet>.ts.net` | `X-ClickHouse-Key` | 12 hours |
| `dispatch` | a CI run's results token, or a person's approve token, which the broker mints and holds | swaps it in with a DPoP proof, at a private destination | `dispatch.<tailnet>.ts.net` | `Authorization` | 15 minutes |

An `anthropic-federation` secret is minted as its type says ([Requests and grants](#requests-and-grants)):
the broker reads the Google identity token of the account `BROKER_SERVICE_ACCOUNT_SECRETS` binds to
the secret, for the audience `https://api.anthropic.com`, trades it at
`POST https://api.anthropic.com/v1/oauth/token` for a token that opens with `sk-ant-oat01-`, and
refuses a token whose `expires_in` is more than an hour. The kind's `allow` bounds every secret of
it, one with no profile included, so a token of the kind reaches Anthropic's Admin API alone and
never its token exchange or its Messages API, on the same host. A client that names
`api.anthropic.com` in its `NO_PROXY` reaches the proxy all the same under a grant of an
`anthropic` secret: `agent-secrets` drops that entry from the child's `NO_PROXY` and changes no
variable outside the child.

A host is written in one form, the one every request for it is compared in: a DNS host name of
letters, digits and hyphens between single dots, with no label longer than 63 characters,
starting or ending with a hyphen, or holding `--` as its third and fourth characters other than
the `xn--` of an internationalized name, no name longer than 253 characters, no leading,
trailing or doubled dot, at most one leading `*.` as the wildcard, an internationalized name as
its punycode (`xn--bcher-kva.example`, not `bücher.example`), and a last label that is not a
number (`foo.123` and `foo.0x` are refused, `123.foo` is read). Case does not matter: the proxy
compares hosts without regard to it, so `API.GitHub.com` is the same host as `api.github.com`
and is served. The broker refuses to start with a file that writes a host any other way, and
`envoy-broker profiles check` refuses that file too, naming the form to write where there is one:
`api.github.com` for `api.github.com.`, and the punycode for a Unicode name. Every host the
airlock's own validation refuses is among these, so that validation refuses no host after them.
What a host in another spelling would do if it were served depends on the spelling:

- An IP address in its canonical form, or an IPv6 address in upper case, and a name with a
  trailing dot are decrypted, and every request on them is refused `sni mismatch`, because a
  client sends no TLS server name (SNI) for an address and drops the dot from the one it sends
  for a name: the entry would carry nothing. A name the grammar refuses for another reason, such
  as `a_b.example.com.` or `foo.123.`, is decrypted the same way with its dot, and the refusal
  says both. An address in any other spelling — with a port, in brackets, with a zone,
  IPv4-mapped or not canonical — binds nothing, because the proxy reads the address a `CONNECT`
  names in its canonical form, so a `CONNECT` to it is tunnelled unread; and an address with a
  trailing dot is a name of numbers alone, which the resolver refuses, so a `CONNECT` to it is
  refused before anything is bound.
- A name with an underscore, a `--` label other than an internationalized one, or an `xn--` label
  the IDNA lookup profile refuses binds when DNS answers for it, but it is outside the LDH grammar
  public host names are written in.
- The rest — white space, a scheme, a port, brackets, an empty or overlong label, a number for a
  last label — is no name a client sends, or one clients read differently.

A secret behind a kind is **behind the airlock**, and the broker never releases its value to
anyone: a read of a grant of it leaves the name out of `values` and answers an `airlock` object
instead, which carries the proxy's URL, the roots to trust that proxy by, a proxy password, and,
for each name, a placeholder, the placeholder of each slot of the value, and the hosts its kind
binds. A placeholder is the kind's `placeholder_prefix`, else `agent-secrets-placeholder-`, and
32 letters and digits, minted for that one grant and name. The
file is the deployment's own, and the broker reads it once at start, so a change to it rolls the
brokers; `envoy-broker profiles check <file> [--against <old file>] [--secrets-json <file>]
[--private-upstream '<host>=<url> ...']` says whether a broker would serve a file, and what that
file adds and removes against the one in force: the kinds, hosts, headers, variables, ways,
`max_grant` and `allow` rows of the kinds and of the profiles. With `--against` it also lists,
under `widens`, each served name whose `allow` the change widens, and each host it adds to a name.
With `--secrets-json` (the list `gcloud secrets list --format='json(name,annotations)'` prints for
the agent secrets) it reads each secret's `proxy` and `proxy-narrowed` tags and refuses what the
policy load would refuse, in the load's words (`profile "<NAME>": <reason>: <detail>`): a secret
whose `proxy` tag names no kind of the file, a way or `max-grant` its kind does not take, a
profile's row outside its kind; and it exits 1 when a name behind the proxy under the old file is
behind none under the new one (`<n> name(s) leaving the proxy: ...`). Without it, a profile with
no `proxy` string in a file that declares a kind takes its kind from its source's tag, which the
file does not hold, so the check writes an `UNKNOWN:` line for it; and with `--against`, a secret
behind a kind by its own tag is no name the check can see, so it compares each kind both files
declare instead (`widens kind "<kind>": <row>`, `hosts kind "<kind>": <host> added`) and counts
each such kind as not read. A run that refuses nothing it read but left something unread exits 3,
never 0. Exit 1 says the broker would refuse the file; 3 says the check could not read all it
needed, so a job with no access to the store can tell the two apart by the number.

A command that works as `agent-secrets NAME -- command` works unchanged while its grant lives when
`NAME` is behind the airlock, except in these named cases: Bun's `node:https`; the AWS uploads and
URLs the `aws` kind refuses (a chunk-signed upload and a presigned URL); a private address literal
from Node, wget or Python's urllib; other names under a suffix a bound host is under; the App token
request `POST /app/installations/{id}/access_tokens` and every other call to `/app` or under `/app/`
that is not a `GET` or `HEAD`; a call to `/repos/{owner}/{repo}/installation`,
`/orgs/{org}/installation` or `/users/{username}/installation`, which GitHub answers only for the
App's JSON Web Token and which the proxy sends with an installation token; a client that signs an
App's JSON Web Token itself; a request that needs the values of two secrets at once; and a GraphQL
write to another account's repository.

The one `proxy` object a profile may still hold is of kind `dpop`: a capability the broker mints
for a grant of its own and holds, from no stored secret. Such a profile is a name and its proxy
block alone. Its grant is
answered a placeholder that opens with `agent-secrets-placeholder-`, the kind's word and a hyphen;
on the block's host the proxy takes a request only with that placeholder as its one
`Authorization` header, `DPoP <placeholder>`, swaps in a token it mints for the grant bound to its
pod's own key, adds a DPoP proof of that key, and sends the request without the command's
forwarding headers. The placeholder opens no tunnel anywhere else. Every dpop block binds one exact
host with an `allow` list none of whose entries is every path (`""` or `/`), in `Authorization` alone, and that host is a private destination's:
`BROKER_PROXY_PRIVATE_UPSTREAM`, `<host>=<url>` entries, one per bound host, sends each host's allowed
requests to its own address inside the broker's own cluster rather than wherever its name resolves. No block of another
kind may bind it. A stored secret under a dpop block is never released (`403 MINTED_ONLY`).
Each grant records a digest of its block and the destination, and a broker whose own digest differs
serves none of it. The policy never serves a dpop profile as a secret, whatever the store holds,
and a stored secret of its name is not served through it, so nothing of it can be requested or
approved; it is no refusal either, so it writes no refused line and counts in no unservable series. A grant of the capability is made by the broker
itself, for a kind it names in code. This release names one, the approve capability, below.

The approve capability is a person's approvals in Dispatch from a box they signed in on. A dpop
block named `DISPATCH_APPROVE_TOKEN`, binding Dispatch's host, turns it on, and the broker logs it
at start (`broker: the approve capability`, with the host and the grant digest, or `state=off`).
A session on such a box trades the person's sign-in, asking for
`requested_token_type=urn:envoy:params:oauth:token-type:proxy-placeholder`, and is answered a
placeholder (`agent-secrets-placeholder-approve-…`) of a grant of that person and session that
lasts 15 minutes, with the proxy's URL, its authority and the one host under `proxy`. Nobody
approves the grant: the sign-in is its authority, and revoking the sign-in ends it. Only a person's
sign-in is granted the placeholder. The proxy mints a token for the grant on each broker pod, of
the scope `approve` and the project scope person and bound to that pod's key, at the grant's first
allowed request on the pod, and again on that pod after the broker's policy version moves, and puts
the token it holds into every later allowed request while the grant lives; it never releases the
token. The broker logs each such token's SHA-256 and the thumbprint of the key it is bound to
(`broker: minted a grant's token`), one line per token it mints, never the token.
Dispatch holds that token to its approve
routes, and to the answer of an approval request among the asks, and answers every other route
`403 APPROVE_ONLY_TOKEN` before it runs, the mint and the change of a personal token among them.
The block's `allow` list narrows what reaches Dispatch by method and path prefix alone (`POST
/api/v1/artifacts/`, `POST /api/v1/asks/` and the like), so a request under one of those prefixes
that is no approve route, a document's comment or an ask's decline among them, reaches Dispatch,
and Dispatch refuses it; a request under no prefix, an issue's comment or any `GET`, is refused at
the proxy before any token is minted for it. `dispatch review` sends a person's review this way.

What the command gets is the placeholder and the way to spend it. `agent-secrets NAME -- command`
puts the placeholder in `NAME`, points `HTTPS_PROXY` at the broker's proxy port with that grant's
proxy password, writes a trust bundle (every trust file the command's own environment named, and
the system roots where it named none, plus every root the release carries) and a wgetrc naming it,
and sets `NO_PROXY` so the grant's bound hosts reach the proxy while loopback and the private
ranges stay direct. The environment it reads is the one the command will have, released values
included. A trust file or `WGETRC` named by a relative path is resolved from the directory the
command starts in, and a line on stderr names that variable. Each trust a grant's commands name
gets a directory of its own, named for the grant and a keyed hash of the bundle and wgetrc it
holds, so a later command of the same grant never gets an earlier command's bundle; the key is a
random file in the same private directory, so the name, which the command's variables carry,
lets nobody test a guess of a password the wgetrc holds. A grant keeps one such directory per
trust its commands named until it ends. It writes neither file and sets no trust variable, so
the command keeps the trust it has, only where all of these hold: the machine's own system trust
bundle, read with no variable (`/etc/ssl/certs/ca-certificates.crt` on Debian), trusts every
root the release carries; the command's environment carries the nine settings that make each
client family read that store, as a pod image and a launcher session set them (`SSL_CERT_FILE`,
`CURL_CA_BUNDLE`, `REQUESTS_CA_BUNDLE`, `NODE_EXTRA_CA_CERTS`, `GIT_SSL_CAINFO`,
`CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE`, `AWS_CA_BUNDLE` and `GRPC_DEFAULT_SSL_ROOTS_FILE_PATH`, each
an absolute path to a file that holds every root, and `UV_SYSTEM_CERTS` set to a value uv reads as
true, such as `1`); the last `ca_directory` of wget's startup files, when there is one, holds every
root; the wget startup files read are named by absolute paths (`SYSTEM_WGETRC` when set, `WGETRC`
when set, and otherwise the `HOME` wget reads `.wgetrc` from), and so is that last `ca_directory`,
since the command may resolve a relative one from another directory; and the command has no
`SSL_CERT_DIR` at all. `NODE_USE_SYSTEM_CA` does not count, since a Node before v24.6.0 or
v22.19.0 does not read it. Any other command, such as one from a shell started before those
settings were set, gets the bundle. The command calls the service as it did before. The value
reaches no answer
of the broker's, no file and no variable: the proxy swaps the placeholder for it in the headers
its kind names, inside the connection it carries to the bound host, and an answer of the proxy's
own that quotes what a service sent — the `502` for a status line it could not read, a failed
Azure exchange's refusal — writes each run the transport quotes as
`"[service bytes not shown]"`, and the same answers for a certificate that names other hosts write
a fixed phrase in place of the names, so a service that sent the value back there does not hand it
on.

The proxy reads the grant behind the password on every connection, and the name's kind and
profile decide each request (`packages/envoy/internal/cipher/proxy`). A host that no kind of the
grant's names binds is tunnelled unread, to a public address alone. A bound host is read with a
leaf the broker pod signs for it ([the proxy's chain](#the-proxys-chain)), and the request must
name that same host in its TLS name and in its `Host` header; then the kind's hosts and headers,
and the kind's and the profile's methods and paths, decide whether the swap happens. That
connection speaks HTTP/1.1 to a client that offers it, and HTTP/2 to a client that offers nothing
else, which is how every gRPC client connects. Each HTTP/2 stream is decided on its own, as one
request, with its `:authority` as its `Host`, so one connection carries no request for another
host. A `CONNECT` or an upgrade to `h2c` inside that connection is refused
(`PROTOCOL_SWITCH_REFUSED`), because what crosses after its answer is no request the proxy reads.
How a stream becomes the request the kind decides is tested, not proved: the Lean model of the
decision has no wire format. A refusal is an HTTP error from the proxy naming the rule that
refused it, such as `host not bound` or `not allowed by rule`, and ending with its answer id. The
broker logs one line for each call to a bound host, never with a value. A call it carried, or
could not carry, writes
`airlock proxy: a call to a bound host` with the enrollment, the grant, the secret, the host, the
method, the path and the answer's status. A call it refused writes its refusal record,
`broker: answer refused`, with the grant and enrollment as its caller, the secret, the host, the
method, the path, the reason and the answer id
([Refusals and the answer id](/legion/broker/operate/#refusals-and-the-answer-id)). A tunnel is
read by nobody, and a plain-HTTP request is forwarded with nothing decided, so each socket the
proxy opens for one writes a line of its own instead: `airlock proxy: a tunnel`, with the
grant, its enrollment, the session and the runtime behind it, the host and port asked for and
the address the broker dialled. A dial the proxy refused or could not make writes
`airlock proxy: a dial that did not happen`, with those fields, how many addresses the host
resolved to, the class of the failure and the reason. The host and the port are the command's
own text, cut to 256 bytes, the host lowercased and the port read as its number, and a refusal's
path and correlation id are too; none of that is redaction, so every line the proxy writes has
each placeholder — `agent-secrets-placeholder-`, in any case, and at most the 32 letters and
digits after it, up to where another placeholder starts, or a kind's own `placeholder_prefix` and
the 32 letters and digits after it, where the character before it is no ASCII letter or digit —
replaced by `[placeholder]`.

A path that holds a `.` or `..` segment is refused on every bound host (`path has a dot
segment`), whatever the kind allows, because a reader that resolves such a path first — which is
what RFC 3986 says it means — reads it as a path the prefix does not bound. Where the kind or the
profile has an `allow`, the proxy refuses more before it reads the allow rows, each a path a
service may read otherwise than the prefix test does: an empty segment, `//` (`path has an empty
segment`); `%2F` in any case in the path as sent (`path has an encoded slash`), which a service
may decode into a `/` the prefix never saw; `\`, or `%5C` in any case (`path has a backslash`),
which some servers read as `/`; a segment whose text before its first `;` is `.` or `..`, with
the `;` sent as it is or as `%3B` and the dots as they are or as `%2E` (`path has a parameter on
a dot segment`), since Tomcat, Jetty and Spring take a `;` parameter off a segment before they
resolve it, so `/v1/..;/admin` reads to them as `/admin`; and a path that, decoded once, still
holds `%2F`, `%2E`, `%5C` or `%25` in any case (`path has a double-encoded character`). A `%` can
reach the once-decoded path only from a `%25` in the path as sent, so this reads every spelling a
server that decodes twice would turn into `/`, `.` or `\` (`%252F`, `%25%32%46` and
`%25%32%45%25%32%45` among them), and the `%25` catches a server that decodes three times or
more. Then come the kind's rows (`not allowed by rule`) and the profile's (`not allowed by the
profile's rule`). After them, still only where one of the two has an `allow`, a request that
carries `X-HTTP-Method-Override`, `X-HTTP-Method` or `X-Method-Override` is refused (`request
carries a method override header`), and so is one whose query holds a parameter named `_method`,
with any value or none (`request carries a method override parameter`), because a service that
honours one acts on the method it names and not on the one the allow read. The proxy reads the
query as the frameworks that honour that parameter do (Express, Symfony and Laravel among them),
and refuses wherever one of them would read `_method`, or more. It splits the query into
parameters on `&` and on `;`, since a PHP service may be set to split on `;` too. It decodes each
name's percent-escapes, with `+` as a space, and cuts the name at its first NUL byte, as PHP and
Symfony do, so `_method%00x` is `_method`. It takes off leading spaces, reads `.`, a space and `[`
as `_`, and folds ASCII case. PHP reads `.` and a space as `_` too, but drops a name that starts
with `[`, so reading `[` as `_` refuses a name such as `[method` that PHP would not read at all:
it refuses more, never less. So `_meth%6Fd`, `%5Fmethod`, `.method`, `+_method` and `_method%00x`
each count as `_method`, and a name with a `%` that starts no valid escape is refused for the
same reason, since what a service makes of it is not known. Two forms of `_method` are not
refused. A `_method` field of a form body: the decision never reads the body, which the proxy
streams to the service unread, and Rails, Laravel and Symfony honour that field too, so a kind
whose service does must not count on its `allow` to bound the method of a form post. And
`_method[]`: PHP makes `$_GET['_method']` an array of it, which Symfony's method override
ignores, and Express's `methodOverride('_method')` reads no key `_method[]`. A kind with no
`allow` bounds no path or method, so none of these refusals applies to it, and its clients' paths
pass as they are: `gcloud storage` sends the `/` of an object's name as `%2F`, and an S3 key may
hold `//`.

A name's fingerprint is part of what a grant of it was decided under. For a name behind a kind it
holds the kind and that kind's `allow`, and the secret's `proxy-narrowed` tag; for a profile it
also holds the profile's whole entry, its own `allow` included, and its source's `judge` seconds.
So an edit of one profile's entry ends that profile's grants and no others, a retag of a secret's
`proxy` or `proxy-narrowed` tag ends the grants of its names, and an edit of a kind's `allow`
ends the grants of every name behind that kind. A kind that loses a host or a header ends those
grants too; a kind that only gains a host or a header ends none. The judge seconds sit where a
profile's `proxy` block wrote a `judge` before it became the source's tag, so moving the judge
from the file to the tag, with the same seconds, ends none. The fingerprint a grant is held to is
its approval's, the entry and the kind the approval was given under, and every release compares
it: a grant approved after an edit ends when the file rolls back to what the request was made
under. The proxy keeps the values it read for a grant until
the policy moves, a minted value nears its expiry, or a reread finds a new value of the stored
secret it is served from: a reread of a source, or of any profile of it, rereads the one stored
value they are all served from, so the proxy reads each of their grants' values again and a
source's new value reaches every profile's requests. A stored value has no version the broker
tracks, so each reread of it counts as a new value; a type secret's reread does only when it
finds a new version of the value, so a reread that finds the same one leaves the proxy spending
the tokens it minted from it.

What the airlock does not stop is as much of the design as what it does:

- A service that sends the credential back in its answer, or that makes a new credential and
  returns it (a deploy key made with an administrator's token), gives that to the caller. The
  proxy does not change what a service answers. A rule's methods and paths are how its owner
  closes such a path.

- The placeholder works as the proxy password for its grant's bound hosts until the grant ends,
  from wherever the proxy port is reachable.

- Once the grant ends, every call through the proxy gets `407`, to any host. A call still running
  at that moment, such as a WebSocket the proxy joined to a bound host, a gRPC call whose answers
  stream or a long poll, is ended with the grant within 15 seconds rather than running to the
  idle limit. A call whose service has not answered yet gets the `407` itself. A tunnel to a host
  no kind of the grant binds carries no value, so the idle limit is the whole of what bounds one.

- A `407` is the grant's own answer. A read of the grant that failed — the broker's store did
  not answer — is `503` with a fixed sentence that names nothing of that failure, and the call
  can be made again: a store down for a minute ends no grant, and would otherwise send every
  agent in the middle of a call to ask for a new one. A refusal of the grant's release that no
  retry clears is `403` in the broker's words, under the code the release itself answers: a
  profile's scope its minter refuses is `PROFILE_MINT_REFUSED`, and a target its live profile
  does not hold is `TARGET_REFUSED`.

- A private address that reaches the proxy is refused, because the proxy runs beside the broker,
  whose own identity reads every secret. So is an address inside the broker cluster's own
  Service, pod and node ranges, which the deployment names in `BROKER_AIRLOCK_CLUSTER_RANGES`
  beside `BROKER_PROFILES_FILE`: a Service range can be public address space, as GKE's
  default `34.118.224.0/20` is, so no private-address rule covers its ClusterIPs. In a cluster
  (`KUBERNETES_SERVICE_HOST` set to a non-empty value; empty or unset is outside a cluster), a
  broker whose profiles file declares a kind or a `dpop` block refuses to start without that
  setting, or when no range in it holds that address. A command that needs such a call sets its
  own `NO_PROXY` entry for that name.

- A value an agent received before its secret went behind the airlock keeps working until its
  owner rotates it.

### The proxy's chain

A broker pod signs each bound host's leaf with a certificate authority of its own, which the
deployment's settings name one of two ways (`packages/envoy/internal/cipher/proxy`, `ca.go` and
`intermediate.go`):

- **A pool of Google Certificate Authority Service** (`BROKER_PROXY_CA_POOL`). For each certificate
  request, at its start, at each renewal and at each retry, each pod makes a new ECDSA P-256 key in
  its own memory, which is never written anywhere, and sends the pool a request for it
  (`projects.locations.caPools.certificates.create`) with its own Google credential: an
  authority of path length 0 for servers alone (`serverAuth` its one extended key usage), for 7
  days, whose name constraints, marked critical, permit the names of the profiles file's hosts, a
  `*.` pattern as its domain, and exclude every IPv4 and IPv6 address (`0.0.0.0/0` and `::/0`).
  The pool's issuance policy fixes what the answer may be, and the pod holds the answer to it too:
  the answer must certify the pod's key, chain to a root, be an authority of path length 0 whose
  key usage signs certificates and whose one extended key usage is `serverAuth`, last 7 days and 5
  minutes at most, have critical name constraints that permit exactly the DNS names of the bound
  hosts (no other name, and not no constraint at all), permit no IP range and exclude all of
  `0.0.0.0/0` and `::/0`, sign a leaf a client trusts for every bound host, and chain to the same
  root as the pod's first answer, since every release the pod has made names that root. A name
  constraint applies only to names of its own kind (RFC 5280, section 4.2.1.10), so permitted DNS
  names alone would leave the key free to sign a trusted leaf for any IP address; the exclusion
  closes that. URI and email names are left unconstrained, since no TLS client takes either as a
  server's identity. What the constraints bound depends on the client. Go's `crypto/tls` never
  takes a subject's common name as the server's name, so it refuses every leaf that names a host
  only there, and GnuTLS refused every such leaf too. Clients built on OpenSSL (`openssl verify`,
  curl, Python's `ssl`, Node) read the common name of a leaf with no DNS alternative name and
  accept a name outside the allowed ones when that common name is one label, such as `localhost`,
  or a wildcard over two or more labels, such as `*.evil.example` for `a.evil.example`; they refuse
  a plain name, such as `evil.example`, and a wildcard over one label, such as `*.example`. Bun
  accepts any name through the common name once the leaf also carries a URI, email or
  registered-ID alternative name. Against those clients a leaked pod key is bounded by the
  intermediate's life, at most 7 days and 5 minutes, and not by its names. The `serverAuth` usage
  keeps the key from signing a certificate a verifier takes for e-mail or for a client. The pod
  asks for the next intermediate when the one it holds has less than 2 days left, whatever its
  life, but no sooner than 5 minutes after it took that one, so a pool that answers one of 2 days
  or less, which has less than 2 days left the moment it arrives, is asked at most twelve times an
  hour and never in a loop. It goes on asking with a backoff of 1 second doubling to 5 minutes
  until it gets one; an answer it refuses counts as a failure. It goes on signing with the
  intermediate it held until a minute after the new one's start, so after a renewal a command
  whose clock runs up to a minute behind the pool's never meets an intermediate that is not yet
  valid. A pod's first intermediate, and the one after an intermediate ended, have no earlier one
  to sign with, so they rely on the pool's `backdateDuration: 300s`, which covers five minutes in
  every case. It logs one line for each intermediate it takes, and one for each request that
  failed.
- **A mounted pair** (`BROKER_AIRLOCK_CA_CERT_FILE`, `BROKER_AIRLOCK_CA_KEY_FILE`), for a deployment
  without that service: an intermediate and then each authority up to and including its root, or a
  self-signed root alone. The broker refuses at start a chain that reaches no root, one whose life
  is over, one that would sign no trusted leaf for a bound host, and an intermediate past any bound
  a pool's answer is held to.

With both set, the pool is the one used. A leaf lasts 25 hours from its start to its end, and
never past the end of the certificate that signed it, so a leaf signed while the pool fails ends
with the intermediate the pod holds. The TLS answer carries the leaf and every authority between
it and the root, so a client that trusts the root alone completes the chain. A pod that holds no
certificate it may still sign with — before the pool's first answer, or once its intermediate
ended with no new one — answers `GET /healthz` with `503 NO_INTERMEDIATE`, so its readiness keeps
traffic away and a rolling update keeps the old pods until a new one has its intermediate. That
takes the pod out of every Service, the edge's included, so once every pod's intermediate has
ended with none in its place, the whole broker is down
([Health and logs](/legion/broker/operate/#health-and-logs)).

Every release carries the root of the pod's own chain and each root of
`BROKER_PROXY_RELEASE_ROOTS_FILE`, one PEM block after another in its root field. A rollout from
one authority to another hands out both at once: a client of any release writes the whole field
into its trust bundle, so it trusts a pod that signs under either root.

## Approvals

A person approves a secret request on the broker's own approval page, never in Dispatch. The
broker writes nothing in Dispatch: Dispatch's server reads the broker's pending list
for the person signed in, and its Inbox lists each pending secret request as a link to that
request's page on the broker, `<broker>/approve/<record>`, which the broker's answers name as
`approve_url` (an expired one is listed with no link, below). The
page signs the person in to the broker itself, with OpenID Connect and the broker's own client (the
deployment's Google Workspace, `BROKER_SIGNIN_*` in
[Operate](/legion/broker/operate/#approval-sign-in)), only when they hold no live sign-in there,
and brings them back to the same request. It prints the request from the broker's own record: the
secret names, the enrollment the broker verified asked (its type, operator, runtime and worker
slot), the session the request names when it differs, marked as the request's own claim, the
reason, the lifetime, the times, the approver and the state; then every other request waiting on
that person. Each request the person may decide has **Approve** and **Deny**, and one **Approve
all N shown** decides every request printed. A decision is posted to the broker's own origin and
is bound to that sign-in and to the requests the page printed, so a page on another origin,
Dispatch's among them, cannot make one, and no route a service calls decides a secret request: the
broker refuses it to the listed service `DECIDE_ON_BROKER_PAGE`, naming the page — or, for a
request the model judge holds, `JUDGE_DECIDES`, naming no page — or, for a request a private
project hides from the person the call names, `404 NOT_FOUND`, as for an id no record has, and
`503 PROJECT_ACCESS_UNAVAILABLE` where Dispatch does not answer. Those routes read no record for a
caller the broker has not authenticated: one with no ServiceAccount token, or one it does not
accept, is refused alike for every record id, so it learns nothing of which records exist.
The page prints, for each profile a request asks for, its source and the hosts its kind lets its
credential reach, and for each stored secret it asks for by its own name behind a kind, that
name, its kind, the kind's hosts and the requests the kind allows. An approval is bound to what
its page printed too: the page's forms carry the fingerprint of each profile, and of each such
secret, it printed, and an approval is refused `403 PROFILE_CHANGED`, deciding nothing, when one
it names has another fingerprint by then, as a profiles roll, a retag or an edit of the kind's
`allow` between the page and the click leaves it. Opened again, the page prints the name as the
broker now serves it, and its approval grants that. A person Dispatch calls a restricted member,
who decides no secret request, is refused as one before that check, so their approval says nothing
of a profile.

A broker that signs nobody in, `BROKER_SIGNIN=off`
([Operate](/legion/broker/operate/#a-broker-that-signs-nobody-in)), serves no such page, so no
request waits on it: every request that would reach a person is denied at once
([Requests and grants](#requests-and-grants)), and one a broker with sign-in on left pending is
ended by the broker's next sweep: expired, as a rule, if its deadline has passed, and otherwise
denied.

The broker checks the signed-in person against the record's approver: anyone else gets a page
saying who may decide it, with a link to sign in as someone else, and no buttons. A request for a
group's secret names the group as its approver, `group:<address>`, and the page names that
approver `a member of the group <address>`: each member the broker's last read of the group lists
may decide it, and it waits in each such member's Inbox, so a group not read for 2 hours lists it
to nobody and lets nobody decide it ([A group's secret](#a-groups-secret)). The record names the
member who decided it, and the grant names the group, so a member the group's last good read
listed lists and revokes the grant. A request for a shared
human-tier secret, or for a shared agent-tier secret its session's operator withheld from it,
names the approver `anyone`: anyone who signs in to the broker may decide it, and the
broker records the email of whoever did. It waits in the Inbox of the person responsible for the
agent that asked, which the broker records when the request is made, in this order: the session's
operator (the person of an `agent-secrets login` sign-in, the person who approved its machine
login, or, for [a person's pod](#a-persons-pod), its person while `BROKER_POD_OPERATORS` still maps
the pod's account to that person); for a fleet machine's own session, the person its
`BROKER_VM_EXCHANGE_ACCOUNTS` entry names; for a Legion worker pod, the assignee of the Dispatch
issue its claim names, which the broker reads at `BROKER_JUDGE_DISPATCH_URL` and stops waiting for
after three seconds. Where none of these names a person — a pod with no claim that acts for no
person (a person's pod too, once the setting drops its account or maps it to another person), a
broker with no judge settings, a Dispatch that did not answer in time, an issue nobody is assigned
to, a machine whose fleet names nobody — it waits in every person's Inbox, so no request waits
where nobody sees it. Where that person may not see the request, because it belongs to a private
project they are not on, from when it was made or since, it waits in the Inbox of everyone who may
see it, for as long as Dispatch answers within the list's two seconds for such requests
([A private project's secrets](#a-private-projects-secrets)). A Dispatch read that failed or ran
out of its three seconds is logged at WARN, naming the issue
([Health and logs](/legion/broker/operate/#health-and-logs)), so a broker that cannot read Dispatch
is seen to list those requests for everyone. Who is asked changes nobody's right
to decide: anyone signed in who may see it still decides it from its page. That is also why the broker takes the
claim and the assignee as they come: a claim is the launcher's word, so a launcher that states a
Legion claim naming some issue, and anyone who reassigns that issue in Dispatch, chooses whose
Inbox lists the request, and neither gains a right to decide it. A withheld secret is its owner's to approve for that
session even in a request that was already waiting when the operator withheld it: a request that
waited on anyone for a shared human-tier secret and the operator's own withheld secret is then the
operator's alone to approve, and anyone else's approval is refused `NOT_APPROVER`. While the broker
does not serve the withheld secret to that session, no one may approve such a request: not while
the secret is out of the broker's policy (deleted, or its tags refused), since its owner cannot be
read, nor while it is a service's, since no person approves a service's secret. It waits until the
secret is a person's or shared again, or expires. The approval it gets lasts as any approval does
(below): a change to another secret's tags does not end it, nor does the operator's later revoke of
another grant, unless that revoke withholds another secret the request got without asking.

An approval belongs to the person who gave it, so a change to a secret's tags reaches what was
approved before it wherever the new tags want the secret approved for that session:

- An approved grant keeps releasing its value only while the person who approved it may still
  approve the secret for its session. Once a shared human-tier secret becomes a person's, or a
  person's secret becomes another's, every such grant the new owner must approve and did not stops
  with `GRANT_NOT_LIVE`, whatever value the secret holds by then, and the session's next request is
  decided under the new tags. A grant keeps working when the secret becomes shared (anyone may
  approve a shared human-tier secret, and a shared agent-tier one needs no approval), and when the
  new tags give its session the secret without asking, as they do once an agent-tier secret is the
  session's operator's. A secret the operator withheld from the session stays human tier there
  under any tags, so a grant of it someone else approved stops once the secret is the operator's.
- A pending request is approved by whomever the new tags name to approve it for its session. One
  waiting on anyone for a secret that has become a person's is that person's alone to approve;
  anyone else's approval is refused `NOT_APPROVER`, the requester's own operator included. One
  waiting on a person for a secret another person must now approve can be approved by no one. One
  waiting on a person stays that person's when the secret becomes shared, or when the new tags give
  its session the secret without asking. A denial releases nothing, so the approver a request waits
  on can still deny it, which takes it off the pending list: for a request waiting on anyone, any
  signed-in person may deny it. Otherwise it expires, or its session cancels it
  (`agent-secrets cancel`) and asks again.

A record is decided once. A second click, a concurrent one, or one after the record expired changes
nothing (`RECORD_TERMINAL`), and the page says what became of the record. A pending request nobody
decides expires after 12 hours, a fixed time rather than
a setting (`agentSecretPendingTTL` in `packages/envoy/cmd/cipherd/main.go`).

A request or a machine login whose deadline passes before anybody decides it stays in its
approver's Dispatch Inbox, marked **Expired** with its deadline, with no link to a page that
decides it, until someone dismisses it with **Dismiss**. It is no longer counted as waiting on the
person. The broker reads it as expired from the moment its deadline passes, before the sweep ends
it and after, which is exactly what every decision of it refuses. A dismissal is the record's
once, for every Inbox: the first person whose Inbox holds a shared request and who dismisses it
takes it out of everyone's, and no route brings it back. The broker stores each dismissal
(`credential_request_dismissals`); a request the sweep had already ended before it kept them was
recorded as dismissed by `broker` and does not come back.

A machine login is still decided in Dispatch, with its typed code
([Machine login](#machine-login)), and a person's revoke of a grant or a machine login is still made
through Dispatch: Dispatch's server calls the broker as its own Kubernetes ServiceAccount, which the
broker lists as a caller (`BROKER_API_ACCOUNTS`), with that person's email from their Dispatch
session, and the broker takes that name on Dispatch's word. A listed account's token can therefore
approve a machine login and revoke in any person's name, though never approve a secret request:
list Dispatch's own account and nothing else.

### The model judge

A deployment may configure a **model judge** (`packages/envoy/internal/cipher/judge`). Who decides
a request is a setting of its keys, in each secret's own tags:

- **Nobody**: an agent-tier key is granted at once to the sessions its tags allow, as before.
- **The judge**: a key with a `judge` tag, the longest lifetime in seconds the judge may grant,
  no `admin` tag, and no `anthropic-federation` type. It may also carry a `guidance` tag: the key
  owner's instruction saying when the judge should grant it. A profile is decided by its source's
  tags. Set both with `agent-secrets secret retag <NAME> --judge 900 --guidance '<text>'`.
- **A person**: every other key, and every administrator's key and Anthropic federation key
  whatever its `judge` tag says, since either can release an organization administrator's
  credential. The person decides on the broker page, as before.

The judge decides a request when every key in it that needs a decision is a judge key. A request
with any key a person decides is that person's whole. The broker reads the answer when it writes
the request and keeps it with the request, so a restarted broker, or one with other tags, decides
it the same way. With the judge's settings unset there is no judge, and every request that needs a
decision waits for a person.

No request has to cite anything. The judge reads the Dispatch issue a reason names and the ask it
cites (`dispatch://KEY/ask/<id>`) as data for the model, and a key's guidance may ask for either:
for example, "grant only to post text that the key's owner approved in Dispatch; the reason must
cite that answer". `GET /v1/settings` answers each judge key with its seconds and guidance
(`judge_keys`), and `agent-secrets secret show` prints the key's own `judge` and `guidance` tags
and who decides a request for it — for a source served only through its profiles, the judge, for
each judged profile made from it — so an agent can meet the guidance on its first request.

While the judge decides a request, which is at most two minutes from when it was written, nobody
else can: it is on no person's list or Dispatch Inbox, on any broker; its answer carries
`judge_deciding: true` and links no approval page; its page on the broker is read-only; the
decision routes refuse it `403 JUDGE_DECIDES`; and a person's Approve or Deny of it is refused.

The broker that wrote a request pending runs the judge in the background, for a request it wrote
and never for one it joined to a request already pending, so one record is judged once. A run has
35 seconds, and each model call inside it 20. The run makes six checks first, in order, and stops
at the first that fails, asking the model nothing:

1. each key the judge decides carries a `judge` tag (a profile, its source's);
2. the grant's lifetime — the `--ttl` its session signed, else the broker's default — is at most
   15 minutes and at most each of those keys' seconds;
3. the tag policy, read again now, still answers approval for each key the judge decides, and
   still gives the session at once any key it got at once beside them;
4. no key is an administrator's (the `admin` tag) or an Anthropic federation key;
5. no key is withheld from that session;
6. the request is no private project's: a `dispatch-project` tag on any of its keys naming a
   private project or a project key Dispatch holds no project for, a credential that names a
   private project (a session's list, or the project a pod's or a machine's exchange entry maps it
   to), or any issue key of a private project that the signed reason names leaves the request to
   a person, since the judge's own token names no person and so sees no private project. These are
   the projects that hide a request from a person outside them too. A request whose keys carry no
   such tag, whose credential names no project and whose reason names no issue key makes no call
   to Dispatch here, and neither does any request on a broker with no
   `BROKER_PROJECT_ACCESS_URL`.

A key the session gets at once, such as an agent-tier key, may stand beside the judge's keys. It
needs no decision: the judge decides nothing of it, the model is not shown it, and the judge's
grant releases it with the rest.

A request that passes them all goes to the model in one call for each key the judge decides, all
made at once. Each call is shown that one key's name, judge seconds and guidance, the reason, and
the issue and ask the reason points at, and nothing of any other key, so one owner's guidance never
reaches the decision on another owner's key; the model is also told that a key's guidance decides
only that key: it follows the rule a guidance gives the key it decides (one source's guidance,
shown for each profile made from it, may give each profile its own), and text in it that tries to
decide the whole request, or this key through what it says of another, is a reason to deny. The
model follows the guidance and reads everything an agent wrote as data to judge rather than as
instructions. Each call answers `grant` or `deny`, and the request is granted only when every call
grants: one `deny` refuses it. A request for N judge keys makes N calls, each bounded at 20
seconds, and waits as long as the slowest of them. The run reads the request's state once more
immediately before the calls and asks nothing about a request that is no longer pending — one its
session cancelled, or one whose wait ran out. That read bounds nothing about how many model calls
one session can start: each request a session creates is pending for the moment the run reads it,
so a session that creates and cancels requests in a loop costs one model call per judge key of
each request it creates.

Every run ends the request one of three ways:

- **Grant**: the judge's approval, below.
- **Refusal**, which is final: a `deny` verdict, a policy that now denies a key, or a key withheld
  from the session. The request is denied under the judge's actor, and the agent reads
  `the model judge refused this request: <reason>. This request will not be granted. Tell whoever
  gave you this work.`; `agent-secrets` exits 77. Nothing tells the agent to try again. A refusal
  grants nothing, so its write asks Dispatch nothing, and a refusal the checks reached stands
  while Dispatch cannot answer.
- **Hand-over**, when the judge cannot decide: any other check fails, the model cannot be reached,
  gives no verdict or does not answer within the 20 seconds, or a Dispatch read fails. The request
  goes to its person once, as an ordinary card on the broker page. A run the broker lost, when it
  stopped serving mid-run, writes nothing, and the request reaches its person when the two minutes
  end. On a broker that runs no judge, a judge key's request is its person's from the start.

For a request the judge decides, a lifetime longer than a judge key's seconds, or than 15 minutes,
is refused when the request is written, naming the `--ttl` that would do, and the judge is never
asked; a request naming no lifetime gets the shortest of those seconds. A request a person decides
— a judge key beside a key with no `judge` tag, or any request on a broker that runs no judge —
keeps the ordinary lifetime rules.

An approval is the judge's own. The decision event's actor is `judge:<model id>` and its login is
null, so no owner tag makes a judge's approval read as a person's. The request's `decided_by`
holds that same string, which `agent-secrets status <request>` prints. The grant records the
record's own approver — the secret's owner, or `anyone` for a shared secret — so the person who
approves that secret lists the grant and can end it, and a judged grant of a shared secret is on
every signed-in person's page.

A judged grant is held to the keys' `judge` tags in force rather than to a person's standing, and
to the `admin` tag over both. Each release re-checks that the secret's `judge` tag still gives the
judge at least the grant's lifetime; a grant whose secret lost its `judge` tag, or whose seconds
fell below that lifetime, is refused and ended for good at that release, and putting the tag back
does not bring it back, as a lowered `max-grant` does not. A grant the new seconds still cover goes
on releasing, unless the secret is served as a profile behind a kind: that profile's
fingerprint holds the seconds, so any change of them ends its grants, refused as its profile
having changed, "its entry in the profiles file, or the judge tag of its source key". A secret its owner tags as an
administrator's after the judge decided it ends that grant for good, at the release and at the
next request that would reuse it: a person approves every release of one, so no judged grant
goes on releasing it, and taking the tag off again does not bring the grant back: the session
asks afresh, as a new request, which its person decides while the tag stands. The chain check
([The audit record](#the-audit-record)) admits a judge's approval only on a record that names
agent secrets alone, each one a key whose `judge` tag still gives the judge seconds of it, and a
lifetime within each one's seconds, so a forged record, and one a retag left behind, release
nothing either. What the reason says is the judge's to read against the guidance, and no part of
that check.

The judge reads Dispatch at three points before the model — the policy check, the project check
and the reads of what the reason points at. The first two and the issue read share one 5-second
bound, and the ask read has a 5-second bound of its own, both inside the run's 35 seconds, so a
slow Dispatch leaves the model call and the row's write their time. On a
broker with `BROKER_PROJECT_ACCESS_URL` set, the policy check asks what kind of member the
session's person is and reads the state of a tagged secret's
project, and the project check asks with the broker's account token whether a project the request
names is private: each key's `dispatch-project` tag, the projects of the credential that asked,
and the project of each issue key in the signed reason. The reads of what the reason points at are
the issue it names and the text of its spec document, and the ask it cites, with the broker's own
projected ServiceAccount token, read again from its file once the copy it holds is 30 seconds old.
A read its bound cuts short fails its check, and the request goes to its person. Another read,
with the same token, is one GET of the issue a Legion worker pod's claim names, for its assignee
alone ([Approvals](#approvals)); the reads of a person's project access and member kind at
`BROKER_PROJECT_ACCESS_URL`, by the token exchange, an enrollment, a secret request, an approval, a
release and every airlock connection, those for a private project's secrets
([A private project's secrets](#a-private-projects-secrets)), and each read of whether Dispatch
holds the person a credential names
([What it depends on](/legion/broker/operate/#what-it-depends-on)), are the last
([Operating the broker](/legion/broker/operate/)). An approval asks Dispatch again before it is
written, under a 5-second bound of its own that ends a second before the run does, so a Dispatch
slow to answer then fails the approval, and the request goes to its person. A run with a verdict
writes its `request.judged` row on a context of its own, which the run's deadline does not cancel,
so an approval that failed late still leaves its row; one whose answer was lost after it applied
leaves only the row it wrote itself, one still committing is waited for, for half the time the row
has, and where even that cannot tell, the row it writes says `applied` `unknown`. Such a row
ends no hold, since the approval may still be committing: the request reaches its person when the
two minutes end, as a lost run's does. The broker
writes nothing in Dispatch — no ask, no comment, no status — and reads nothing that belongs to a
person. It reaches its model at the endpoint its settings name, presenting the broker machine's
own Google identity token for the audience they name, or, where `BROKER_JUDGE_MODEL_ACCOUNT` names
a Google service account, that account's identity token for the same audience, which the broker
mints at IAM Service Account Credentials with the pod's own credential. A mint that fails hands
the request to its person, and the broker never presents its own token in that account's place.
The judge's other settings are all-or-none, and the
[configuration reference](/legion/broker/reference/config/) gives each one.

## The audit record

Everything a person decides rests on a **credential-request record**
(`packages/envoy/internal/cipher/record/record.go`): a fixed, line-by-line text holding the
session's signed request verbatim, the approver (a person's login, `anyone`, or `group:` and a
group's address), the enrollment's type, runtime id and operator, the lifetime (`-` for a grant
that lasts until its session ends), the policy version, the expiry, and, for a machine login, its
confirmation code. The
record's id is the SHA-256 of that text, so a record cannot change without changing its id.
Decisions are events on the record (`approved`, `denied`, `expired`, `cancelled`, `revoked`), each
naming who made it: a person by their login, and the model judge by its actor alone
([The model judge](#the-model-judge)).

Every time the broker releases a value or authenticates a launcher credential, it re-verifies the
whole chain: the record still reproduces its id, the session's signature inside it still verifies,
and exactly one approval stands behind it. That approval is the record's approver's, or, where
that approver is a group, the login of a member the group lists now, or the judge's on a record the
rules in force still let the judge decide. A grant or credential with no approved record behind it
releases nothing. A grant a member of a group approved releases nothing while the group has not
been read for 2 hours, as a lapsed grant rather than a broken chain ([A group's
secret](#a-groups-secret)). A grant the model judge approved for a group's secret rests on no
member, so it keeps releasing while the group is stale, and the judge keeps deciding new requests
for that secret, while the profiles in force give the judge that secret.

Beside the records, the broker keeps an append-only `audit` table with one row per event:
`enrollment.created`, `enrollment.revoked`, `enrollment.expired`, `request.created`,
`request.granted`, `request.denied`, `request.cancelled`, `request.expired`, `grant.used` (each time
a session or the airlock's proxy reads a grant, naming the secrets released and the reason the
grant's request gave, and, for a session's read, the reason its own call gave where that differs
and the command it said the release starts, with each secret the broker can name replaced by
`[redacted]`: the values of 8 bytes or more the release handed out, the strings of 8 bytes or more
of a stored JSON value, and every credential of a well-known shape; any other secret a session
typed into its command, a shorter value among them, is kept as typed, and `GET /v1/grant-log`
answers this row to the accounts that read the grant log),
`grant.revoked` (naming, under `withheld`,
the secrets an operator's revoke withheld from the session, under `withheld_on`, the session's
other enrollments it wrote them on besides the grant's own, and, on each other grant of the
session that revoke ended, the withheld secrets that grant held), `grant.withheld` (an operator's
revoke of a grant already revoked, naming under `withheld` the secrets it withheld from the
session, and under `withheld_on` the other enrollments it wrote them on),
`request.judged` (one row for every judge run that reached a verdict, its hand-overs to a person
and the runs a check stopped included: each check and its result, the model, whether the model was
asked, the `verdict` — what became of the request: `granted`, `refused`, `handed to the person`,
`not pending` for one already decided, cancelled or expired, or `unknown` where an approval's
answer was lost — the reason, the `calls` (one for each judge key the model was asked about, with
the key and what its call did: `granted`, `denied`, or `failed` with its error, a failure being no
deny, and the raw answer of a call whose answer did not parse), the token counts, that it priced
nothing, and, for a request handed to its person because a call failed, the raw answer of the
first call that failed, which is empty when that call sent no answer at all, a time-out among
them, even where a later call's answer did not parse; a refused request carries none there) and
`launcher_credential.revoked` (naming the machine login's `credential_id`, its `host`, its
`service` for a service's login, and under `enrollments` the sessions the revoke ended) and
`launcher_credential.token_login` (a service's token login: the `credential_id`, its `service`, the
token's `subject`, the `pod_uid` the token is bound to when it is, the login's `key_thumbprint`,
the credential's `expires_at` and the token's own `token_expires_at`). Each row names its actor:
`human:<email>` for a decision a person made on the broker's approval page or a change Dispatch
made for its signed-in person, `session:<enrollment id>` for
a session's own, `launcher:<credential id>` for one
a machine login made under its own credential, a person's revoke from their machine and a
service's token login included, `judge:<model id>` for one the model judge made, or `broker`. No
record, event or audit row ever holds a secret value.
[Operating the broker](/legion/broker/operate/#the-audit-record) shows how to read them.

A quarterly **access review** reads those rows over `GET /v1/grant-log`, the one route the caller
name `access-review` reaches (`BROKER_API_ACCOUNTS`; the names `dispatch` and `listener` do not
reach it, and the route changes nothing). It answers every `audit` row of a closed period, in the
order the rows were written, each event naming the session that asked, the person behind it, the
service the broker verified for that session and the one its credential claimed, the secrets and
how each was decided, the approver, who decided, the lifetime, the reason the session signed, and
the detail of a refusal. A period's end must be at least an hour ago, so that every row of it has
committed before the review reads it. Each decision and each grant end is also one INFO line in
the broker's log, `broker: grant log`, so a period can be read from the log pipeline without the
database.

Three things the log does not say, which a reviewer has to be told:

- A grant that ended because its enrollment ended writes no `grant.revoked` row. The end is the
  enrollment's own row, `enrollment.revoked` or `enrollment.expired`, which does not name the
  grants one by one.
- A grant that reaches its own `expires_at` writes no row at all. What says when it ended is its
  `request.granted` row and that row's `lifetime_seconds`.
- A minted token — `github-app`, `google-login` and `google-service-account` alike — lives its own
  hour from the moment it was minted, an `azure-login` token until the broker's projected token it
  carries ends, an `anthropic-federation` token as long as Anthropic's answer gave it, and a
  `github-installations` token until the `expires_at` its minter stored, the broker handing it over
  only while more than 20 minutes of that are left.
  Revoking the grant stops the next release; it does not revoke a token the broker
  has already released.

The review reads the `audit` table as it stands. It adds no audit type and writes no audit row of
its own: every row it reads is one the broker wrote when it decided.
