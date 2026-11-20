# agent-secrets

The secrets broker's client. An agent session runs it to use a secret:

```sh
agent-secrets DEMO_API_KEY --reason "Deploy the example service" -- ./deploy.sh
```

It looks the command up on `PATH` first, and refuses one it does not find (exit 1) before it asks
the broker anything, so no request waits on an approver for a command that could not run. It then
asks the broker for the named secrets, waits while a person approves them on the broker's own
approval page if the secret's owner and tier require it, and runs the command with each granted
value in its
environment, writing one line to standard error first: the names it injected, that the broker
`AGENT_SECRETS_URL` names gave them, and that store's kind, as `agent-secrets: NAME from the broker
AGENT_SECRETS_URL names (GCP Secret Manager)`, never a value. Every other line about a secret names
its source the same way - `request`, `status` and `self` as a `source:` line, a denial, a pending
request and every refusal as `; asked the broker AGENT_SECRETS_URL names (<store kind>)` - so an
answer of this CLI's is never taken for another secrets CLI's about the same name. An error the
broker answers also ends with its answer id, `(answer <id>)`, for whoever was refused to quote to
whoever runs the broker. For a refusal, from 400 to 499 but a `429`, the id names the broker's own
log line for it. A `429` and a failure of 500 and above name their id in no line: for
`MINT_FAILED`, the broker's `broker: mint failed` line names the secret and the time. This CLI's
lines are quoted wherever an agent's output is, so none names the store's account, region or
project, and
none names the broker's host but two: a broker that could not be reached, named by its URL so a
stale address reads as one, and a pending secret request, whose line names the page on the broker
where its approver decides it, the `approve_url` the broker's own answer carries. The secret
forms below, which call the store itself, name the account or project they read and write. No
printed form of the broker's URL, and no refusal or error that `agent-secrets`,
`agent-secrets-helper` or `agent-secrets-devrelay` prints or logs, holds a credential that URL
carries: the URL prints only when it is provably a plain http(s) URL (no user info, query, fragment,
escape or control byte), and as `(not printed: the value carries a credential or is not a plain
http(s) URL)` otherwise. Beside a URL that does not print, a refusal the broker answers prints its
status and `(not printed: the broker's address is not clean)`: a machine login's device
authorization prints `LOGIN_FAILED: broker <n>: (not printed: the broker's address is not clean)`,
and an OAuth refusal of a trade or of a machine login's refresh prints `the broker answered HTTP
<n>`, with no marker. None prints the refusal's words, its answer id or its
code, which whoever answers at that URL could fill with the credential it carries. A
code prints there only where it is one the program matched against its own list: `renew`'s exit
on a terminal refusal, and the helper's log of a refusal it acts on; the helper's log of a
sign-in the broker refused names its code as that marker. Every value of an answer `AGENT_SECRETS_URL`
answered that this CLI prints - the request, grant and enrollment ids, a request's state, who
decided it and the broker's sentence about it, whom and where a pending request waits on,
`self`'s lines, the cells of `machine list` and `grant list`, and the broker's settings - prints
as the broker's address lets it print: beside a URL that prints, on one line, as it came where it
holds no control character and Go-quoted where it holds one; beside one that does not, as
`(not printed: …)`, except, beside a URL with no user info, query, fragment or `%` (one refused
for its host alone, such as a host with an underscore or a trailing dot), a request, grant,
enrollment or machine login id that is a UUID, the shape of every id the broker makes, and that
the URL does not hold in any case or form: such an id is no credential and prints whole, in every
form and table alike, so `agent-secrets status <id>`
can follow a request. `--json` writes a 2xx body verbatim, every field, since it asks for the body
by name. Of a machine login the helper relays, the `user_code` prints on one line, and the
`verification_uri` as the URL above prints: whole where it is a plain http(s) URL, `(not printed:
the value carries a credential or is not a plain http(s) URL)` otherwise.
The helper's log lines name the `credential_id` of the machine login the broker issued, which the
log's own format escapes the same way.
People and launchers use its other forms to log a
machine in
(`agent-secrets machine
login`), register an agent session (`agent-secrets register --exec -- <agent>`), and inspect a
session's grants (`agent-secrets self`). People and agents manage the agent secrets themselves with
`agent-secrets secret list|show|create|set|retag|delete|restore`, which calls the broker's own
store (AWS Secrets Manager or GCP Secret Manager) under whatever cloud credential its environment
gives it - the store's own IAM decides what that credential may do - and then asks the broker to
reread each secret written, so the change
is served at once; those forms need no helper and no session. On GCP, `secret create` makes the
secret, empty and with its tags, before it asks for the value, so a store that refuses the create
asks for none; until the value is added, a failure or a hang-up, interrupt, quit or termination
deletes that empty secret again, and a secret that may hold the value, where the store's answer to
the add leaves that open, is kept. Each store call from the create until the value is stored or
the empty secret deleted has a deadline of 60 seconds, Secret Manager's Go client's own default
for those calls, which AWS's `CreateSecret` takes too, its SDK setting none. That bound is per
call, not per signal: a signal waits out the call in flight, and then its cleanup makes its own
calls, each with the same deadline, so a first signal against a store that answers nothing can
take a few minutes. A second signal ends the command at once, by that signal, naming what the
store may be left holding and the command that clears or checks it. From their own shell on a
machine
that runs the helper, people list and end their own machines' logins and their live grants with
`agent-secrets machine list|revoke` and `agent-secrets grant list|revoke`, under that machine's
login; a service's login is revoked on Dispatch's machine-login page. The helper refuses `machine
list|revoke` and `grant list` to a process inside a registered session's process tree
(`IN_SESSION`), so an agent's own commands cannot act as its operator. `grant revoke <grant_id>`
run inside a session is the session's own revoke instead, which `agent-secrets revoke` makes too:
it signs as the broker identity the session's use command takes, and the broker ends only a grant
of that identity's enrollment and refuses any other (`NOT_YOURS`), which stays its holder's, its
operator's or its approver's to end. Processes that sign as one enrollment share its grants: each
session of a person who ran `agent-secrets login` on the machine signs as that sign-in's one
enrollment, and processes that share one key dir sign as its enrollment, so each can end the
others' grants. The machine-command check covers that tree only: a process a
session sends out of it (`( cmd & )`, `setsid -f`, a tmux server it started) passes, and the same user can stop
the helper anyway. Such a process can list and revoke its operator's own machine logins, never a
service's machine login, and its operator's grants, which include grants the operator approved on
any session. `agent-secrets enroll --helper` is open to any process of the user, so such a process
can also enroll a box and read the operator's agent secrets. `agent-secrets --help` lists every
form, and each form answers `-h`.

Before the broker spends a stored value, it removes that value's trailing line endings under one
rule, which reads the value itself and no tag at all: the whole trailing run of carriage returns
and line feeds goes only where what is left is not empty and holds no carriage return and no line
feed, so a one-line API key or bearer token loses the ending an editor or a shell left behind,
a PEM document keeps the line feed that ends it, and a value that is nothing but line endings
stays as it is.

The rule reaches the stored value alone, in both of the broker's reads of the store: the value a
session is handed, and the input a minter reads. A token a minter makes is never changed. The
broker writes one line at INFO for each value it changed, `broker: a stored value's trailing line
ending was removed name=<NAME>`, which carries the secret's name and no part of any value.

`secret create` and `secret set` read the value they
store by this same rule, so a value stored through this client and a value stored by any other
writer are spent alike. Where the rule changes nothing, the client still removes one
final line feed, which it has always done, so `echo` and a file that ends in a newline store the
value without one.

A type secret's value is checked before it is written, by the decoder the broker's minter for that
type reads it with: `secret create --type` checks the value it stores, `secret set` checks it under
the type tag the secret holds, and `secret retag` checks the value the secret holds when it
changes the type. A value that minter could not mint
from is refused with the minter's reason, exit 2, and nothing is written. The retag reads the
value for that check and never prints it. A sign-in the store refuses that read still writes the
tags, and the retag says on standard error that the value was not checked and names the
permission the read needs. The broker reads every type secret's value with the same decoder and
refuses a secret whose value it refuses as `value-malformed`. A write after which the secret holds
a type needs a broker that reads the `type` tag, one whose `GET /v1/settings` answers
`secret_type_tag: true`: against an earlier broker, which reads no `type` tag and from
legion-envoy-v16.0.0 on would serve the secret's stored key as its value, it exits 1 before it
writes anything, naming the release to move the broker to. After the write, `create`, `set` and
`retag` each exit 1 when the reread names another type than the secret holds.

On a machine that runs agents directly, `agent-secrets-helper` (`../agent-secrets-helper`) holds
each session's key and signs for it; in a container or a Kubernetes pod, the session's key lives in
`AGENT_SECRETS_KEY_DIR`.

A use command (`agent-secrets NAME -- command`), and every other form that acts as a session,
takes its broker identity from the first of four sources that has one for the calling process, in
this order, and needs no registered agent session:

1. `key-dir`: the `key.pem` and `enrollment` a launcher put in `AGENT_SECRETS_KEY_DIR` (default
   `$XDG_RUNTIME_DIR/agent-secrets`), a box's or a pod's. Absent without `key.pem`; a `key.pem`
   that does not parse, one with no `enrollment`, or a fresh `enrollment.pending` still without
   `key.pem` after `AGENT_SECRETS_ENROLL_WAIT` (a launcher still enrolling), stops the command.
2. `sign-in`: a person's `agent-secrets login` on this machine. Absent when nobody signed in.
3. `helper`: `agent-secrets-helper`, asked first whether the caller is in a session it registered.
   Absent when there is no socket and no helper is expected (`AGENT_SECRETS_HELPER_SOCK` unset, no
   helper unit installed), or the helper answers `NOT_A_SESSION`, as it does for a process that
   only inherited `AGENT_SECRETS_HELPER_SOCK`. A stopped helper (no socket where that variable
   names one or the unit is installed), a socket that does not answer, `NO_CREDENTIAL` or any
   other refusal stops the command.
4. `machine`: the machine's own cloud identity, found with no setting from the Google exchange
   audience the broker answers in `GET /v1/settings`: an `external_account` file
   `GOOGLE_APPLICATION_CREDENTIALS` names, or the Compute Engine metadata server
   (`AGENT_SECRETS_MACHINE_IDENTITY` forces `metadata` or `pool` instead; `kubernetes` is refused,
   exit 2, since the broker mints no launcher credential for a Kubernetes identity, and a pod
   enrolls through its launcher or `enroll --pod`). The settings are read only from an `https`
   broker, or an `http` one on loopback. Once an identity is found, and before a token is minted
   for it, the audience is kept in the cache's `google-exchange-audience` (a machine with no
   identity keeps none): a broker that later names another is refused before any token is minted
   for it. The session it
   registers is kept in the machine cache, `$XDG_STATE_HOME/agent-secrets/machine` (a 0700
   directory, `key.pem` and `enrollment` each 0600), and signs every later command with no
   network call; each registration, the first and each one after the broker says the session is
   not live, makes a key of its own, and a lock in the cache makes commands that find it empty at
   once register one session between them. A broker that names no exchange audience is skipped
   with that reason; a broker that refuses the exchange stops the command.

A source that is present but broken, and any broker refusal, stop the command with that error
(exit 1); with no source, one refusal names each source and why it had nothing:
`no broker identity for this process: key-dir: …; sign-in: …; helper: …; machine: …`. Since the
cache is read below the sign-in, a person who runs `agent-secrets login` acts as themselves from
their next command on, and the cache stays for every process no person signed in for. A machine
session an older client kept in the key dir (beside a `machine-session` file) is moved into the
cache by the first command that finds it.

A process started with no environment of its own, such as a systemd unit or a cron job, reads its
settings from `~/.config/agent-secrets/config`: `KEY=value` lines, `#` comments and blank lines,
no quotes, no `export`, no expansion. It takes `AGENT_SECRETS_URL`, `AGENT_SECRETS_SIGNIN_ISSUER`,
`AGENT_SECRETS_SIGNIN_CLIENT_ID`, `AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE`,
`AGENT_SECRETS_MACHINE_IDENTITY`, `AGENT_SECRETS_MACHINE_TOKEN_FILE`,
`AGENT_SECRETS_MACHINE_AUDIENCE`, `AGENT_SECRETS_POOL_AUDIENCE` and `AGENT_SECRETS_POOL_ACCOUNT`,
and refuses any other key, a path that is not a regular file, a file another user than this one
or root owns, and a file its group or others can write, as a usage error (exit 2) naming the
file. A variable that is set and not empty wins over the file. With no `HOME` in the
environment (a system unit that names no `User=`), the file, the sign-ins and the machine cache
are under the home the password database gives the process's user.

`agent-secrets identity` answers by exit status alone (0 for a launcher's key dir or a helper
session, 1 otherwise) and asks nothing of the network. `agent-secrets identity --source` walks the
chain without registering anything and prints the source a command would use — `key-dir:
AGENT_SECRETS_KEY_DIR`, or where it is unset `key-dir: $XDG_RUNTIME_DIR/agent-secrets
(AGENT_SECRETS_KEY_DIR is unset)` or `key-dir: /run/user/$UID/agent-secrets (AGENT_SECRETS_KEY_DIR
and XDG_RUNTIME_DIR are unset)`, `sign-in: <person>`,
`helper: <socket>`, `machine: the cached enrollment in the machine cache (…)`, or `machine:` and
the identity it would trade — then `helper: <socket>` whenever the helper answered and is not
that source; with none, stdout is empty, the refusal above is on stderr, and it exits 1. The key
dir and the machine cache each hold a `key.pem`, so no line names either by its path. A
`helper: <socket>` line ends with the release the running helper reports for itself,
`(agent-secrets-helper <release>)`; a helper from before that report answers `unknown op version`,
and the line ends `(a helper from before this release; its binary is /proc/<pid>/exe)` instead.

A Kubernetes pod an operator launches by hand needs no credential a person installs and no machine
login a person approves: it logs in and enrols itself with the projected ServiceAccount token its
own kubelet mounts. Its ServiceAccount must be one the broker's service logins name, and the token
must be the pod's own projection, not one `kubectl create token` minted, since a login bound to no
pod enrols nothing. It then holds a session key and its enrollment id in its key dir, as a box
does, and a machine login, which Dispatch's machine-login page lists; each `enroll --pod` run logs
in again with a launcher key it keeps only in memory.

```sh
export AGENT_SECRETS_URL=https://broker.example AGENT_SECRETS_KEY_DIR=/run/agent-secrets
agent-secrets enroll --pod --service k8s-session
agent-secrets renew --pod-token-file /var/run/legion/agent-secrets-token/token &
agent-secrets DEMO_API_KEY --reason "Run the session" -- ./session.sh
```

`enroll --pod` reads `/var/run/legion/agent-secrets-token/token` unless `--pod-token-file` names
another file, takes the pod's UID from that token's own `kubernetes.io/pod/uid` claim as the
enrollment's runtime id, and writes `key.pem` and `enrollment` into `AGENT_SECRETS_KEY_DIR`
(`--out` names another directory), printing the enrollment id, alone, on stdout. Re-running it on
the same directory keeps that key, gets its own live enrollment back and renews that enrollment's
lease; concurrent runs on one directory take turns on `<out>/enroll.lock`. A pod holds its lease
with `renew --pod-token-file <file>`, which re-reads the file before every renewal because the
kubelet rotates the token in place. `renew` finds its identity through `AGENT_SECRETS_KEY_DIR`
and never through `--out`, so the command `enroll --pod` prints names that directory as well —
`AGENT_SECRETS_KEY_DIR=<dir> agent-secrets renew --pod-token-file <file>`, both shell-quoted —
and runs as it reads whichever of the two named the directory. Every other form then reads the
key dir as a box's.

An enrolment the broker refuses leaves the key — a new key while the old enrollment is live is
refused `ALREADY_ENROLLED` — and records what the broker said in `<out>/enrollment.error`, which
every later `agent-secrets` call in that directory reads out to the caller. A run that succeeds
removes it.

**Nothing in the pod ends its enrollment.** `unenroll` goes through the machine's
`agent-secrets-helper`, which a pod has not, so a pod cannot end its own; and the broker is never
told the pod is gone, so a pod that dies leaves its enrollment until its lease lapses —
`BROKER_LEASE_SECONDS` after the last renewal, since `renew` dies with the pod, while a broker
is serving. That bound holds only then: a broker coming back from an outage gives every
unrevoked enrollment the time no broker was serving (`enroll.ServingClock.Beat`), so a lease can
outlive the pod by that gap as well (the "When no broker is serving" section of the broker's
concepts page, `docs/site/src/content/docs/broker/concepts.md`). Someone who needs
it ended sooner revokes the machine login it was made under, on Dispatch's machine-login page,
which ends every enrollment under that login at once (the "Revoke a session" guide, built from
`docs/site/src/content/docs/broker/guides/revoke-a-session.md`).

The docs site's "Secrets Broker" section, built from `docs/site/src/content/docs/broker/`, has the
quickstart, the guides and the generated CLI reference.

## Install

Each `legion-envoy-v*` release of this repository ships the client. The release tag names the Envoy
package, not `agent-secrets`, and the repository's newest release can be another package's, so
look the tag up rather than reading `releases/latest`. These commands need `curl`, `tar`, `sort`
and `sha256sum` (on Ubuntu: `apt-get install -y curl ca-certificates`), and install into
`~/.local/bin`. On Linux arm64 run `asset=agent-secrets-arm64.tar.gz` first, and on macOS
`asset=agent-secrets-darwin-arm64.tar.gz` (Apple silicon) or `asset=agent-secrets-darwin-amd64.tar.gz`
(Intel); the block uses `agent-secrets-amd64.tar.gz` otherwise. The install steps run in a subshell
with `set -eu`, so a failed download or checksum stops them before anything is installed:

```sh
asset=${asset:-agent-secrets-amd64.tar.gz}
tag=legion-envoy-$(curl -fsSL 'https://github.com/cybermonklabs/legion.git/info/refs?service=git-upload-pack' \
  | grep -aoE 'refs/tags/legion-envoy-v[0-9]+\.[0-9]+\.[0-9]+([^-0-9]|$)' | grep -aoE 'v[0-9.]+[0-9]' \
  | sort -u -t . -k 1.2,1n -k 2,2n -k 3,3n | tail -n 1)
(
  set -eu
  test "$tag" != legion-envoy-
  echo "installing $tag"
  base=https://github.com/cybermonklabs/legion/releases/download/$tag
  curl -fsSLO "$base/$asset"
  curl -fsSLO "$base/SHA256SUMS"
  grep " $asset\$" SHA256SUMS > "$asset.sha256"
  if command -v sha256sum >/dev/null; then sha256sum -c "$asset.sha256"; else shasum -a 256 -c "$asset.sha256"; fi
  tar -xzf "$asset"
  mkdir -p ~/.local/bin
  install -m 0755 agent-secrets/bin/* ~/.local/bin/
)
export PATH="$HOME/.local/bin:$PATH"
agent-secrets --version
```

`sha256sum -c` (`shasum` on macOS) prints `<asset>: OK`; any other result stops the subshell.
If `~/.local/bin` is not already on your `PATH`, add the `export PATH=...` line to your shell
profile. The Linux archives hold `agent-secrets` and `agent-secrets-helper`; the macOS archives
hold `agent-secrets` alone. `agent-secrets --version` prints the release tag, such as
`agent-secrets legion-envoy-v28.1.1`. To also check that this repository's release
workflow on `main` built the archive from the commit the tag names, run, in the same shell and with
the GitHub CLI signed in:

```sh
commit=$(gh api "repos/cybermonklabs/legion/commits/$tag" --jq .sha) &&
  test -n "$commit" &&
  gh attestation verify "$asset" --repo cybermonklabs/legion \
    --cert-identity https://github.com/cybermonklabs/legion/.github/workflows/release-envoy-listener.yaml@refs/heads/main \
    --source-ref refs/heads/main --source-digest "$commit" --deny-self-hosted-runners
```

## Give a new machine access

A machine that runs agent sessions needs the helper and a machine login that its operator (the
person whose agents it runs) approves in Dispatch. The helper runs on Linux only, as a
`systemd --user` service. Replace `<broker URL>` with your deployment's broker address and
`<you@example.com>` with your Dispatch login:

```sh
export AGENT_SECRETS_URL=<broker URL>
mkdir -p ~/.config/agent-secrets
echo '<you@example.com>' > ~/.config/agent-secrets/operator
agent-secrets-helper install --url "$AGENT_SECRETS_URL"
loginctl enable-linger "$USER"
agent-secrets machine login
```

`loginctl enable-linger` keeps your `systemd --user` services, the helper among them, running
after you log out; without it, the helper stops when your last login session ends.

`agent-secrets machine login` prints a confirmation code and waits. Open Dispatch's machine-login
page (`/credentials/machine`; set `AGENT_SECRETS_APPROVE_URL` to Dispatch's address and the
command prints the full link), type the code, check that it matches your terminal, and approve. The
command then exits 0. Only the person named in the operator file can approve. The helper keeps the
login in `~/.config/agent-secrets/machine.json` and refreshes it itself, so you log a machine in
once; `agent-secrets machine login-status` prints `issued` while it holds the login.

Put `export AGENT_SECRETS_URL=<broker URL>` in the profile your shells and agents start from, and
start each agent as a machine session, so the helper signs for it:

```sh
agent-secrets register --wait 10 --exec -- <agent command>
```

A session whose launcher learns its omp session id only after the agent starts (omp opens its
transcript seconds in) registers unnamed with `--exec` and then runs `agent-secrets register
--session-id <id>` again from inside the session, so the session keeps one broker identity across a
restart of the helper or the machine. Run without `--exec`, `register` never makes its own process
the session's root, since that process ends with the command: inside a registered session (one
started with `--exec`, for `omp` or any other command) it names that session, and otherwise it
attaches the session to the launcher (`omp`) above it. It prints `enrolled` only once the helper
answers a session rooted above it. With neither a session nor an `omp` launcher above it, or
against a helper from before this (one that would pin the ended command), it refuses with exit 1
and says to relaunch the session under `register --exec -- omp …`, rather than print `enrolled`
for an identity nothing can use; upgrade the helper before the client.

A Kubernetes pod or a Google Cloud VM does not use the helper or a person's approval: the Legion
daemon enrolls its pods with their service-account tokens, and a VM enrolls itself (above). The
docs site's [Log a machine in](https://cybermonklabs.github.io/legion/broker/guides/log-a-machine-in/)
guide covers each step in full.

## Common commands

Each grant is for one session, and lasts until that session ends or for `--ttl`. What happens to a
request depends on the secret's `owner` and `tier` tags. Your own `tier=agent` secret is granted at
once to a session on a machine you logged in, and goes to you for approval from any other session.
Your own `tier=human` secret always goes to you for approval. A `shared` agent-tier secret is
granted at once; a `shared` human-tier one waits for anyone who signs in to the broker. A
service's secret goes only to that service's pods and VMs. Approvals happen on the broker's
approval page, which the approver's Dispatch Inbox links to. The docs site's
[Concepts](https://cybermonklabs.github.io/legion/broker/concepts/#owner-and-tier-who-may-have-which-secret) page has the full
table.

`--reason` is necessary wherever a person approves: a request naming a secret that goes to
someone for approval, and carrying no reason, is denied whole, and the session reads
`<NAME>: a reason is necessary; ask again with --reason` and exits 77. Write what the
credential is for, in a sentence the approver can decide on. A request every name of which is
granted at once reaches nobody and needs none.

```sh
agent-secrets DEMO_API_KEY --reason "Deploy the example service" -- ./deploy.sh
agent-secrets request DEMO_API_KEY --reason "Deploy" --ttl 1h
agent-secrets status <request_id>
agent-secrets self
agent-secrets revoke <grant_id>
```

In order: get a secret and run a command with it in its environment; ask without running
anything; show a request's state; show this session's enrollment and live grants; end one of this
session's grants.

From your own shell (outside every agent session), under this machine's login:

```sh
agent-secrets machine list
agent-secrets machine revoke <credential_id>
agent-secrets grant list
agent-secrets grant revoke <grant_id>
agent-secrets-helper sessions
```

`machine list` shows your machines' logins and your pods' logins, `grant list` the live grants of
your sessions and those you approved, and `agent-secrets-helper sessions` the sessions this
machine's helper holds.
`grant revoke` ends a grant: from your own shell it is the operator's revoke of any of these
grants, and from inside a session it ends a grant given to the broker identity (enrollment) the
session signs as; a grant of another enrollment is refused `was given to another broker identity
(enrollment)`. Every session of yours that signs as your `agent-secrets login` shares that one
enrollment, and so its grants.

Owners manage the secrets themselves, with no helper, under the credential of the first source
the command finds: on AWS, the AWS SDK's own order, in the broker's account (access keys in
`AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, then `AWS_WEB_IDENTITY_TOKEN_FILE`, then
`AWS_PROFILE`, then `AWS_DEFAULT_PROFILE`, then the rest of the SDK's default chain); on Google
Cloud, `CLOUDSDK_AUTH_ACCESS_TOKEN`, else the credentials file `GOOGLE_APPLICATION_CREDENTIALS`
names, else the gcloud configuration or login `CLOUDSDK_ACTIVE_CONFIG_NAME` or
`CLOUDSDK_CORE_ACCOUNT` names, else the well-known credentials file, else the active gcloud
account, else the metadata server. A source that is present but broken stops the command, and so
does a profile variable that names a profile the AWS config does not hold; every form prints the
source it used (`using …`) before its first store call, and a store refusal names it and how to
choose another from that source. The store's IAM decides what each may do. `--owner me` is the
email of the person that credential names, and is refused where it names none (a machine's role,
a service account, a token without an email scope); `--owner <email>` names the owning person
outright:

```sh
agent-secrets secret list
agent-secrets secret show NAME
agent-secrets secret create NAME --owner me --tier agent
agent-secrets secret set NAME
agent-secrets secret retag NAME --tier human
agent-secrets secret delete NAME
agent-secrets secret restore NAME
```

`--requesters` lists who may ask for a secret at all — people by their lowercase sign-in email,
services by their registered name, separated by spaces — and a secret carrying such a list bounds
its grants to an hour, so give it `--max-grant 1h` or less; `--requesters none` removes the list
on retag. `secret create` and `secret retag` check those rules before they write, but neither can
check whether an entry naming a service is one your deployment registers: this CLI holds no list
of registered services, so only the broker knows. The reread that follows the write is what
catches an unregistered name — the broker's refusal prints with its reason and the command exits
1, with the tag already written — so correct the name with another `secret retag`.

`agent-secrets --help` and `agent-secrets-helper --help` list every form and environment variable.

## Build

```sh
cd packages/envoy
go build -o bin/ ./cmd/agent-secrets ./cmd/agent-secrets-helper
```

On macOS, build `./cmd/agent-secrets` alone: the helper builds for Linux only. Each
`legion-envoy-v*` GitHub release ships both binaries for Linux, as `agent-secrets-amd64.tar.gz` and
`agent-secrets-arm64.tar.gz`, and `agent-secrets` alone for macOS, as
`agent-secrets-darwin-amd64.tar.gz` and `agent-secrets-darwin-arm64.tar.gz`.

The hidden value prompt is available on Linux and macOS for amd64 and arm64. Other targets,
including Windows, FreeBSD and other Linux architectures, build without a prompt and require
the value on stdin, for example `agent-secrets secret set DEMO_API_KEY < value.txt`.

## Test

```sh
cd packages/envoy
go test ./cmd/agent-secrets/... ./cmd/agent-secrets-helper/...
```
