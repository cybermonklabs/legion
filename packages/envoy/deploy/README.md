# Deploy

Compose definitions run the on-prem Envoy listener, the Dispatch server against an external
database, and a single-node Envoy with its own NATS. All services use host networking. The
listener serves `127.0.0.1:9020` for local session registration and webhook ingress; Dispatch
serves `127.0.0.1:8766` by default for the SPA, Google sign-in through a Cognito sign-in pool or
with Google directly, and the native Dispatch API.

## Host configuration lives in `compose/.env`

Docker Compose reads `compose/.env` next to the compose files on every
`docker compose` invocation, so the host's Dispatch settings belong there — not
in a shell session. A `docker compose up` run by anyone (or any agent) then
reproduces the same deployment; exported variables only override it.
`compose/.env` holds secrets and is git-ignored; `compose/dispatch.env.example`
is the template for Dispatch:

```bash
cp compose/dispatch.env.example compose/.env
$EDITOR compose/.env
```

`compose/single-node.env.example` is the template for `single-node.compose.yml`;
the site's Envoy page ("Running Envoy on one machine") uses it.

For browser access over the tailnet, bind Dispatch to the host's Tailscale
IPv4 address and allow the plain-HTTP session cookie. The tailnet link is
already encrypted; nothing else on the host can reach that address:

```
DISPATCH_LISTEN_HOST=100.x.y.z        # tailscale ip -4
DISPATCH_SERVER_URL=http://example-host-devbox:8766 # the exact browser URL and sign-in callback origin
DISPATCH_INSECURE_COOKIE=1            # cookies over the http:// tailnet URL
```

The origin humans open in the browser is the sign-in callback origin: the server builds
`<DISPATCH_SERVER_URL>/auth/callback` from `DISPATCH_SERVER_URL` in `compose/.env`, and
Dispatch's client at the sign-in issuer — the sign-in pool's app client, or the Google web
client — must list exactly that URL. On Sami's devbox that is `http://example-host-devbox:8766`
(recorded as `DISPATCH_PUBLIC_ORIGIN` in `compose/.env`); a deploy whose `/auth/start` redirect
stops matching it breaks sign-in for everyone, so the auto-deployer checks the redirect after
every deploy and rolls back on a mismatch.

Never bind `0.0.0.0`: that exposes the sign-in endpoints and session cookies on
every interface.

## Layout

- `compose/listener.compose.yml` — host-network listener container.
- `compose/single-node.compose.yml` — Envoy on one machine: a JetStream NATS of its own and the
  listener, both on loopback, for sessions that need to reach only each other. The site's Envoy
  page ("Running Envoy on one machine") walks through it.
- `compose/dispatch.compose.yml` — the production Dispatch server against an external database
  (`DATABASE_URL`).
- `compose/dispatch.acceptance.compose.yml` — isolated Postgres and Dispatch services for a
  deployed-image acceptance run, building the image itself; it is separate so production Compose
  commands need no acceptance-only harness variables, and the acceptance recipe none of
  production's.
- `scripts/up-listener.sh` — starts the listener with `docker compose`.
- `scripts/up-dispatch.sh` — starts Dispatch with `docker compose`.
- `scripts/sync-host.sh` — rsyncs `deploy/` to a remote host.
- `scripts/install-docker-debian.sh` — Docker install helper for fresh hosts.
- `scripts/read-secret.sh` — reads a secret from local SOPS-encrypted state.

## Image

The listener and Dispatch share the image built by `../docker/Dockerfile`. Its
build context is the repository root: a Bun stage builds the SPA and generates
the Go contracts, then a Go stage builds `envoy-listener` and `envoy-dispatch`.

`ENVOY_IMAGE_TAG` has no Compose default. `scripts/up-listener.sh` and
`scripts/up-dispatch.sh` set it to `local` for a checkout build. A host synced
without the Dockerfile must set a published image tag and pull it:

```bash
export ENVOY_IMAGE_TAG=<sha-or-release-tag>
docker compose -f compose/dispatch.compose.yml pull
docker compose -f compose/dispatch.compose.yml up -d
```

## Auto-deployer

`scripts/autodeploy.sh` polls `main`, selects the newest published Dispatch
image among its last 30 commits, creates a `pg_dump`, then pulls and starts
only the `dispatch` service. It retains the ten newest pre-deploy dumps, and
every dump taken before a deploy that failed (below). Its defaults target the
devbox; set `REPO`, `COMPOSE_PROJECT`, or `BACKUPS` to run against another
checkout, Compose project, or dump directory.

It asks the registry about each commit with `docker manifest inspect`, newest
first, and passes over a commit only when the registry answers that it holds no
such manifest. Silence and every other answer (a refused credential, a rate
limit, a server error) prove nothing about the image, and walking past such a
commit could deploy an image older than the one running, and older than its
database's schema. So the deployer logs that read, naming the image, and fails
the pass with nothing deployed.

`PASS_DEADLINE` (3600 seconds by default) is a pass's one deadline, counted from
its start. Every call of the pass that waits on the registry, Docker or the
database is given the time it leaves: the fetch of `main`, each manifest read
(the walk of the 30 commits has no bound of its own), the backup, each schema
read's container, the origin check, and every step of a rollback. A call that
answers slowly is waited for; one that has not answered when the pass's time is
spent is stopped and named as having run out the pass's deadline, and the pass
fails. Each step of a deploy keeps 180 seconds of the pass for its rollback (the
rollback's health check, its one step with a bound of its own), so a deploy whose
steps run until the pass is nearly spent still leaves its rollback that time, and
a deploy the pass has no more than 180 seconds left for does not start: the
previous image still serves, and the next pass starts with the whole deadline.
Three steps of a deploy keep a bound of their own, each cut to what the pass
leaves after those 180 seconds: the pull's 900 seconds (a pull that runs it out
changes nothing, and the next pass pulls again), the `up`'s 300 seconds (the
deploy's limit for the new image's start: a start that has not finished within it
is rolled back), and the health check's 180 at most (below), which stops at the
try the pass has no time for. Those three add up to 1380 seconds, and the least
`PASS_DEADLINE` the deployer takes is 1560 seconds, those and the rollback's 180:
a smaller value is refused at start, naming both numbers. The deadline is the
pass's, not the poll's: a rollback comes after the deploy's own steps, so a
deadline as short as `POLL` would leave a slow deploy's rollback no time. `POLL`,
`PASS_DEADLINE` and `SCHEMA_READ_DEADLINE` are whole numbers of seconds, and a
value with a unit, such as `300s`, is refused at start by name. A clock that
answers no time is named, with the step it stopped, and the pass fails.

It never deploys a commit older than the deployed one. The walk passes over a
commit the registry says it holds no image of, and where that commit is the
deployed one (its image was removed, or `IMAGE` names another repository), the
commit the walk reaches next is older. So the deployer deploys the newest
published commit only when the history of `REPO` says it is not an ancestor of
the deployed commit. An ancestor is logged on one line naming both commits, and
nothing is deployed; an order `jj` does not answer fails the pass. A deployed tag
that names no commit of the repository (`local`, a release tag) has no history
to be older than, and the newest published commit replaces it.

Two releases of Dispatch never serve the database together in this compose deployment:
`docker compose up -d --no-deps dispatch` stops and removes the old container before it starts the
new one, which boots and migrates. That order is this deployer's own, not a rule every deployment
keeps: a deployment that rolls Dispatch instead, running two processes against one database while
it rolls, is supported under the preconditions the server's own guidance names — the same keys
in every process (`DISPATCH_SIGNING_KEY`, or one shared data directory), which the server checks
itself (a process whose keys a live process does not share refuses to start), and every
migration written to leave the release before it able to read and write the schema
(`../cmd/dispatch/README.md`, "Running more than one process" and "Database migrations"). What
no deployment does is start an image older than its database's schema: migrations `0113` and
`0117` are each deployed with no earlier release running, and none is started over either again.

Run it continuously in its supervised host service, or use one pass for a
smoke check:

```bash
cd packages/envoy/deploy
scripts/autodeploy.sh --once
```
In the continuous loop each pass runs as a process of its own, a `--once` run.
A pass that fails logs its own line and ends, the loop logs that it failed, and
the next pass starts after `POLL`. One pass, `--once`, exits 1 when it fails.

A deploy that started its image and did not succeed is recorded in
`$BACKUPS/failed-deploy`, one line each: the commit, `rolled-back` or `stopped`,
and the backup taken before that deploy. The record is written as stopped before
the image starts and rewritten as rolled-back once the rollback restored the
previous image, so a pass that ends in between leaves the deployer stopped.
While the record names a commit, no pass deploys it or an ancestor of it again:
each pass names the commit and the file on one line, and only a newer published
commit deploys. A refused or failed rollback, or any stopped line, ends the
continuous loop with exit 3 and one line saying forward recovery is required,
and a deployer started again deploys nothing while that line is there. The
backups the record names are never pruned, so the dump taken before a failed
deploy is kept for the person recovering from it. A deploy that succeeds removes
the record; a person deletes it to deploy a recorded commit again, or to let the
deployer start after a stop.

The health check asks `/healthz` up to 30 times, each try a 3-second wait and a
request bounded at 3 seconds (`HEALTH_INTERVAL`, `HEALTH_REQUEST_BOUND`), 180
seconds at most, for `200` with
NATS connected: `"nats": false` fails it, and `"nats": null` (NATS turned off)
passes. Dispatch serves while NATS is down or refuses its credential, so a
`200` alone would keep an image whose NATS connection fails; such an image is
rolled back instead, as it was when Dispatch exited at boot without NATS. A
NATS outage fails the check for the previous image too, so its rollback then
reports that it did not become healthy. After health succeeds, the deployer
reads `DISPATCH_SERVER_URL` from `compose/.env` and checks that `/auth/start`
emits exactly `$DISPATCH_SERVER_URL/auth/callback`. It persists the new
`ENVOY_IMAGE_TAG` only after both checks. A pull that fails changes nothing; the previous image
keeps serving. A deploy that fails after the new image started restores the
previous tag and restarts only `dispatch`, unless the failed image changed the
set of migrations the database has applied. Before each deploy the deployer
reads every version in `schema_migrations` and records, as
`ENVOY_IMAGE_SCHEMA=<tag>:<max>-<count>-<digest>` in `compose/.env`, the set the
deployed tag runs against: the highest version applied, how many are applied,
and the SHA-256 of the versions, ascending, one per line. It is the set and not
`max(version)` alone because a migration whose version is below one already
applied, a numbered migration merged after a timestamped one deployed, leaves
the max where it was. The deployer keeps that record until the tag changes, so a
retry of a failed image compares against the set the previous tag runs against,
not the one the failed attempt left. When a deploy fails, it first stops the
failed image and confirms that no `dispatch` container still runs. Then it reads
the versions again, polling the advisory lock Dispatch's migration runner holds
for as long as the pass's deadline leaves, so a migration the failed image left in
flight, still working, ends before the read rather than refusing it. A key still
held when the pass's deadline passes refuses the rollback, naming the key and the
session the server reports as its holder. If the stop or the read fails, or the
value differs from the record, it starts nothing, logs `refusing to roll back to
<previous>` with what it knows (both values, each as `<max> (<count> applied)`,
or that whether the failed image migrated is not known, since the schema was not
read), and exits 3. Recovery is then forward: deploy an image that fixes the
failure, or start the failed one by hand once its failure is understood. Never
start the previous tag against the newer schema.

A record the deployer before this one wrote is `<tag>:<version>`, a bare
`max(version)`. While the tag that wrote it is still deployed it is compared the
old way, refusing the rollback once the max has moved past it, so a retry in
flight across the upgrade keeps the safety it started with; it sees an
out-of-order migration only through the max. The first deploy under a new tag
records the set.

The deployer's database clients (`pg_dump` for the backup, `psql` for the schema
version) run in the `pg_dump` image under `docker run --init`, so the `timeout`
the pass's deadline puts on a container also ends a hung connection. Each try of
a schema read bounds its `psql` inside the container, and its `lock_timeout`, by
what the read's deadline leaves: `SCHEMA_READ_DEADLINE` before a deploy, where a
refused read changes nothing, and the pass's deadline after a failed deploy's
stop, where it decides the rollback. Docker's start of the container is given
what the pass leaves. Each client gets
`DATABASE_URL` with its password removed as its argument, and the password as
`PGPASSWORD`; docker reads both from the deployer's environment by name. So no
process's arguments carry the password, the containers' included, which are host
processes too. `psql`'s stderr is dropped, since its diagnostics can quote the
connection, and a failed read is reported by its exit status alone.

The deployer accepts `DATABASE_URL` in exactly one form, and refuses every other
before anything runs, with a log line that points here and never prints the URL:

```text
postgres[ql]://<user>[:<password>]@<host>[:<port>]/<database>[?<key>=<value>[&<key>=<value>]…]
```

- `<user>`, `<database>` and each `<value>` are ASCII letters, digits, `.`, `_` and `-`.
- `<password>` is ASCII letters, digits, `.`, `_`, `~` and `-`, and percent-escapes.
  So a password's `@ : / ? # [ ]`, `%`, a space and any other character, non-ASCII
  included, must be percent-encoded (`%40 %3A %2F %3F %23 %5B %5D %25 %20`).
  `%00` is refused, and the decoded password must be valid UTF-8: Docker rewrites
  other bytes on their way to the clients.
- `<host>` is dot-separated labels of ASCII letters, digits and hyphens, none empty
  and none starting or ending with a hyphen: a host name or an IPv4 address. No
  brackets or IPv6 literal, no second host, no socket path.
- `<port>` is 1 to 65535.
- `<key>` is `sslmode`, `connect_timeout` or `application_name`. A password in the
  query is refused, under any spelling.

The deployer checks the form with the ASCII ranges whatever the machine's locale.
In that form the deployer, libpq and Dispatch's driver (pgx) all read the same
password from the same place, so the deployer can be sure it hands that password
to its clients as `PGPASSWORD` alone. `compose/dispatch.env.example`'s URL is in it.

## Listener configuration

| Var | Required | Notes |
| --- | --- | --- |
| `ENVOY_MACHINE_ID` | yes | Logical machine name used in published metadata. |
| `NATS_URLS` | yes | Comma-separated NATS URLs. |
| `ENVOY_ALLOW_REMOTE_NATS` | yes, for a shared NATS | Set to `1` to reach a NATS that is not this host's own. The listener owns the `ENVOY_NOTIFICATIONS` stream, so every start reconciles it; without this the start refuses a non-loopback `NATS_URLS`, naming the URL. `compose/listener.compose.yml` sets it. |
| `NATS_NKEY_SEED_FILE` | no | A file holding the NATS nkey user seed this process connects as (trimmed); wins over `NATS_NKEY_SEED`. A set but empty, missing, unreadable, blank or non-user-seed value refuses startup naming the variable and path. Neither set connects without a credential. |
| `NATS_NKEY_SEED` | no | The NATS nkey user seed itself, when `NATS_NKEY_SEED_FILE` is unset. |
| `ENVOY_NATS_SOCKS5_PROXY` | no | A SOCKS5 proxy, as `host:port`, every NATS connection goes through (reconnects included), each server's host name handed to it unresolved. A value that is not `host:port`, or that holds an nkey seed, refuses startup naming the variable, without printing the value; a set proxy also needs `ENVOY_ALLOW_REMOTE_NATS=1`. Unset dials NATS directly. |
| `ENVOY_NATS_REPLICAS` | no | The deployment's replica count, a whole number from 1 to 5; defaults to `1`. The listener creates the `ENVOY_NOTIFICATIONS` stream and its key-value buckets with this many copies and raises one it finds with fewer, at start and every 30 seconds after (WARN `envoy nats keeper restored the stream` or `envoy nats keeper raised a bucket`). More than 1 needs a clustered NATS: a single server refuses it, and the start exits naming `ENVOY_NATS_REPLICAS=<n>`; a refused bucket raise also names the bucket (`raise KV_envoy_interests: ENVOY_NATS_REPLICAS=3: context deadline exceeded`, after 10 s when the grant lacks `$JS.API.STREAM.UPDATE.KV_<bucket>`). Any other value refuses startup naming the variable. Set the same value for every Envoy program of the deployment, every listener and Dispatch included: JetStream's stream update has no compare-and-swap, so a start with a lower or unset value can put the old copies back until the next 30-second pass. Lowering is `nats stream edit <stream> --replicas=<n>` after every program's value is lowered, or the next pass raises it back. |
| `ENVOY_STREAM_MAX_BYTES` | no | The `ENVOY_NOTIFICATIONS` stream's byte limit, a positive whole number of bytes. A full stream refuses every new publish and keeps every message it holds, which leaves only by age, after 72 hours. The sender gets an error it can retry, `stream ENVOY_NOTIFICATIONS is full: nats: maximum bytes exceeded`: a GitHub webhook is answered 503 and logged `github publish failed`, and Dispatch's redelivery sweep has GitHub send it again (five times, over about half an hour); Dispatch's outbox retries its event; `/v1/messages/send` and `/v1/messages/publish` answer `503` with `Retry-After: 1` and the body `envoy: the message bus refused the message because its stream is full: stream ENVOY_NOTIFICATIONS is full: …`, recorded with the outcome `stream_full` (not `bus_unavailable`, since the bus answered). On a clustered NATS the server keeps the refused message's id on the stream's leader, a purge notwithstanding, and acknowledges a retry under it as a duplicate it does not hold; the listener and Dispatch then publish the message once more without its id, so the retry is stored as soon as the stream has room, with no operator step, and while the stream is still full it fails with `stream ENVOY_NOTIFICATIONS acknowledged a duplicate of a message it does not hold, and its publish without its id failed: stream ENVOY_NOTIFICATIONS is full: nats: maximum bytes exceeded`, retried the same way. Size the limit above what the stream takes in 72 hours at its busiest, with headroom, and leave the NATS server's `max_file_store` room for it. Unset creates the stream with no limit and keeps a deployed one; set, the listener raises a smaller limit, or none, to it at start and every 30 seconds and never lowers a deployed one, so a raise of a full stream (`nats stream edit ENVOY_NOTIFICATIONS --max-bytes=<n>`) stands. Any other value refuses startup naming the variable, and a limit the NATS server cannot reserve stops the start naming `ENVOY_STREAM_MAX_BYTES=<n>`. Set the same value for every Envoy program of the deployment, every listener and Dispatch included: programs with different limits do not fight, and the largest stands. Lowering or removing one is `nats stream edit ENVOY_NOTIFICATIONS --max-bytes=<n>` (`-1` for none) after every program's value is lowered or removed. |
| `ENVOY_SENDER_MESSAGES_PER_MINUTE` | no | How many messages one agent session may send through this listener in a minute, `/v1/messages/send` and `/v1/messages/publish` together: a whole number from 0 to 60000. Unset, blank or `0` sets no limit, and the routes answer as they do without the setting. The listener keeps one count for each sending session. The count starts at the burst and gains one message every 60 seconds divided by the rate, up to the burst. The counts are this process's own: each listener, and each replica of one, keeps its own, and a restart fills each count again. A message that finds its sender's count at 0 is answered `429` before the listener reads a session, finds a role's holder, reports a wake or stores it. `Retry-After` gives the whole seconds, rounded up and at least 1, until the count holds one message again. The JSON body holds `reason: "sender_rate_limited"`, `retry_after_seconds` (the same seconds), `messages_per_minute`, `burst` and an `error` that is this sentence with the values filled in: `Your sending is rate limited. This message was not sent. This listener lets one session send <rate> messages a minute and <burst> at once. You can send it again after <seconds> seconds.` Where a value in the sentence is 1, its noun is singular: `1 message a minute`, `after 1 second`. The request's log line has the outcome `sender_rate_limited` and the field `sender`, the session the limit counted. The sender is the session a secrets-broker token proves, else the request's `source_session`. A caller that presents a token and names another session in the body gets `403`. A caller that the listener admits on a bearer, or with no credential where `/v1` is open, names the sender itself in `source_session`. Such a caller can send under another session's id and spend that session's count, which then takes up to `burst × 60 ÷ rate` seconds to fill again. That caller can already send messages as that session. A message with another `source`, such as `dispatch` or `envoy`, is not counted, with a token or without one, and neither is a message with no sender, which is how the Legion daemon sends its notices. The agents' own Envoy tools send the source `agent` and give the agent no argument for it, so the limit counts each of their messages that has a sender. A caller that writes its own request can give another source, so the limit is not a security boundary. A counted message costs one when the listener stores it, may have stored it (any answer after an earlier forward of the same role send, or a `503` after the listener sent the message to the stream and the write's acknowledgement did not arrive in time) or reports a wake for it. A role send's `504` costs one whether or not a forward went out, also when the window ended before the first forward. The listener does not tell a `504` that forwarded nothing from one whose forward the stream may hold. A refusal before any store or wake report, such as a `400`, `403`, `413` or a `503` before the listener sent anything (one while it waits for NATS to reconnect), costs nothing. A repeat the listener recognises costs nothing too: the stream's `duplicate` answer, a repeat of a role message kept for the role's next holder, and a repeat of a role forward the route made in the last 10 minutes. So does an echo, a role message from the session that holds the role. A `duplicate` that answers the listener's own second send, made after its connection to NATS closed under the first, costs one, since the first may have stored the message. A role send's echo, repeat or `403` costs nothing only while the send has made no forward, since an earlier forward may have stored the message. A repeat that finds the count at 0 is refused as a new message is. The clients show the sentence and do not send the message again by themselves, except the Legion stall notice, which waits the seconds and sends it again at most 3 times. The start log has one line: `listener sender limit: <rate> messages a minute, <burst> at once, for each agent session in this process`, or `listener sender limit: none`. Where the rate is 1, the line reads `1 message a minute`. Any other value refuses startup naming the variable and the value. |
| `ENVOY_SENDER_MESSAGE_BURST` | no | How many of those messages one session may send at once: a whole number of 1 or more. Unset, it equals the rate. Set with no `ENVOY_SENDER_MESSAGES_PER_MINUTE`, or with a rate of `0`, it refuses startup naming the variable, and so does any other value. |
| `ENVOY_LISTENER_PORT` | no | Defaults to `9020`. |
| `ENVOY_LISTEN_HOST` | no | Defaults to `127.0.0.1`. |
| `ENVOY_LISTEN_FD` | no | An inherited listening TCP socket, by descriptor number, the listener serves on instead of binding, for a process a test harness starts: the harness opens the socket on port 0 and holds the port the kernel chose until the listener serves on it. Not for a container, since Docker cannot pass a host descriptor into one. `ENVOY_LISTEN_HOST` and the listener's `PORT` (which the compose files set from `ENVOY_LISTENER_PORT`), when set, must name the socket's address (`PORT=0` never does); a descriptor that is not a listening TCP socket, or a setting that names another address, refuses startup naming the variable, with nothing bound in its place. The API auth rule below judges the socket's own address. |
| `ENVOY_API_TOKEN` | conditional | Required when the listen address (`ENVOY_LISTEN_HOST`, or the `ENVOY_LISTEN_FD` socket's) is not loopback and neither the OIDC pair nor the Google pair is set; matching bearer token for Listener API requests. Empty no longer opens `/v1` while a verifier is configured. Each request it admits is counted as `credential="shared_token"` in `envoy_listener_requests_total` and logs `listener: request admitted on the shared token` at most once a minute. |
| `ENVOY_OIDC_ISSUER` | conditional | OIDC issuer whose projected service-account tokens authenticate to `/v1` beside `ENVOY_API_TOKEN`. Set with `ENVOY_OIDC_AUDIENCE` or not at all; half the pair refuses to boot naming the missing one, as does an issuer that does not answer discovery. |
| `ENVOY_OIDC_AUDIENCE` | conditional | Audience those tokens must carry (`envoy`). Set with `ENVOY_OIDC_ISSUER` or not at all. |
| `ENVOY_OIDC_ACCOUNTS` | no | `system:serviceaccount:<namespace>:<name>` subjects separated by commas or spaces: the only accounts whose projected tokens the OIDC pair admits. Unset or empty admits every account the issuer signs for, which is any pod of the cluster. A value that names no account (only blanks or commas) refuses startup naming the setting, and so does a comma that leaves an entry empty (a leading or trailing comma, or two in a row), naming which entry. Set without `ENVOY_OIDC_ISSUER` it refuses startup naming the pair it needs, and with an entry of another shape it refuses startup naming the entry; an unlisted account is answered 401 and logged `listener: service-account token rejected` with `reason` `not_listed` and the subject. |
| `ENVOY_OIDC_GOOGLE_AUDIENCE` | conditional | Audience a caller's Google identity token must carry, for a caller on Google Cloud that holds no Kubernetes token (a Cloud Run service). The issuer is Google's, read at boot; an issuer that does not answer discovery refuses to boot. Set with `ENVOY_OIDC_GOOGLE_ACCOUNTS` or not at all; half the pair refuses to boot naming the missing one. |
| `ENVOY_OIDC_GOOGLE_ACCOUNTS` | conditional | Service account addresses the Google pair admits, separated by commas or spaces and compared in lower case; the token must name one in its `email` claim with `email_verified` true, so the caller fetches it with `format=full`. An entry that is not an address refuses startup naming it, and so does a comma that leaves an entry empty. A refused token is logged `listener: Google identity token rejected` with its `reason` and, for a token that verifies, the account it names. |
| `ENVOY_ALLOWED_HOSTS` | conditional | Comma-separated host names or addresses (no port) an unauthenticated listener serves `/v1` to beside loopback; required for each name clients reach it by when neither `ENVOY_API_TOKEN` nor an OIDC or Google pair is set and they do not dial loopback. Any other `Host` gets a JSON 421. Unused when a bearer is required. Each request such a listener admits with no broker token is counted as `credential="none"` in `envoy_listener_requests_total` and logs `listener: request admitted with no credential` at most once a minute. |
| `ENVOY_API_ALLOW_UNAUTHENTICATED` | conditional | Set to `1` to start a listener whose address is not loopback with no `ENVOY_API_TOKEN`, OIDC pair or Google pair. Without it such a listener refuses to start. Set it only where something else fences the listener's port, because the flag itself admits every caller that reaches the port. The clustered hub's tailnet-only session listener (`envoy-infra:gcp:ClusteredNatsHub`'s `tailnet.listener` with no `clientNamespaces`) is the deployment that runs this way: it binds `0.0.0.0` in a pod of its own, and its NetworkPolicy admits TCP 9020 from the tailnet entry's pods alone. There that NetworkPolicy is what decides who may call `/v1`: the `Host` check admits a loopback name or address beside the names `ENVOY_ALLOWED_HOSTS` holds, so it stops a page that rebinds its own name and not a caller the policy already admits. This flag decides nothing. A hub listener that pods of the cluster reach sets the OIDC pair with `ENVOY_OIDC_ACCOUNTS` instead. |
| `ENVOY_WEBHOOKS` | no | Comma-separated enabled webhook providers. Without `github` the listener opens no CI store and publishes no CI settlements. |
| `ENVOY_GITHUB_WEBHOOK_SECRET` | conditional | Required when GitHub webhooks are enabled. |
| `ENVOY_GITHUB_MENTION_TRIGGER` | no | Defaults to `@legion`. |
| `ENVOY_REVIEWER_APP_ID` | conditional | Required when GitHub webhooks are enabled; the App id whose check verdicts the listener records. |
| `ENVOY_GITHUB_REPOSITORY_RENAMES` | no | Comma-separated `<former owner>/<name>=<current owner>/<name>` pairs, read when GitHub webhooks are enabled: each GitHub envelope of a pair's current repository is published a second time under its former name, the copy differing only in its topic, so a session that subscribed before the rename keeps hearing about the repository. One-way. Spell the former name as GitHub spelled it, capitals included: a NATS subject is compared with case, so another spelling addresses every copy to a subject nothing subscribes to, and the start-up line names each pair's copy subject prefix. A value it cannot read refuses startup naming the variable (`packages/envoy/AGENTS.md`). |
| `ENVOY_SLACK_SIGNING_SECRET` | conditional | Required when Slack webhooks are enabled. |
| `ENVOY_GHOSTWISPR_SIGNING_SECRET` | conditional | Required when Ghost Wispr webhooks are enabled. The listener answers 401 to an event whose signature does not check. |

Once it serves, the listener logs one JSON line `"msg":"envoy-listener listening"` whose `addr` is
the address it is bound to, in either mode: the port the kernel chose for `PORT=0` or for an
inherited socket, and `[::]:<port>` for a listener on every interface (`ENVOY_LISTEN_HOST=0.0.0.0`).
A harness reads its port there, with `jq -r 'select(.msg == "envoy-listener listening") | .addr'`.

A process is given a NATS nkey seed only after its NATS server has nkey users (the server's stage 1,
which keeps a `no_auth_user` fallback): a server with no users sends no nonce, and the Go client
refuses the nkey (`nats: nkeys not supported by the server`) instead of connecting without it.

## Dispatch configuration

Generate the agent token once and record it, with the external database URL, in
`compose/.env`:

```bash
cd packages/envoy/deploy
cp compose/dispatch.env.example compose/.env
sed -i "s|^DISPATCH_AGENT_TOKEN=.*|DISPATCH_AGENT_TOKEN=$(openssl rand -hex 32)|" compose/.env
$EDITOR compose/.env          # DATABASE_URL, logins, listen host
scripts/up-dispatch.sh
curl "http://$(tailscale ip -4):8766/healthz"
```

The Dispatch service reads its public browser origin and NATS URLs from
`compose/.env`; it does not mount an agent's `envoy.json`. Set
`DISPATCH_NATS_DISABLED=1` when no NATS connection is available.

| Var | Required | Notes |
| --- | --- | --- |
| `DATABASE_URL` | yes | The external Dispatch Postgres URL (production: the Aurora `dispatch` database), as `postgres://<user>:<password>@<host>:<port>/<database>?sslmode=require`, with the password percent-encoded. The auto-deployer accepts only that form, with `connect_timeout` and `application_name` the other query keys it allows, and refuses any other (Auto-deployer, above). The compose runs no Postgres of its own. |
| `DISPATCH_SERVER_URL` | yes | Public browser origin and sign-in callback origin. It must be the URL humans type into their browser; Dispatch's client at the sign-in issuer (the sign-in pool's app client, or the Google web client) must list `<DISPATCH_SERVER_URL>/auth/callback`. |
| `NATS_URLS` | yes | Comma-separated NATS URLs for Dispatch. |
| `ENVOY_ALLOW_REMOTE_NATS` | yes, for a shared NATS | Set to `1` to reach a NATS that is not this host's own. The server owns the `ENVOY_NOTIFICATIONS` stream, so it reconciles it on its first connection; without this the start refuses a non-loopback NATS, naming the URL. `compose/dispatch.compose.yml` sets it. |
| `NATS_NKEY_SEED_FILE` | no | A file holding the NATS nkey user seed this process connects as (trimmed); wins over `NATS_NKEY_SEED`. A set but empty, missing, unreadable, blank or non-user-seed value refuses startup naming the variable and path. Neither set connects without a credential. |
| `NATS_NKEY_SEED` | no | The NATS nkey user seed itself, when `NATS_NKEY_SEED_FILE` is unset. |
| `ENVOY_NATS_SOCKS5_PROXY` | no | A SOCKS5 proxy, as `host:port`, every NATS connection Dispatch makes goes through (reconnects included), each server's host name handed to it unresolved. A value that is not `host:port`, or that holds an nkey seed, refuses startup naming the variable, without printing the value; a set proxy also needs `ENVOY_ALLOW_REMOTE_NATS=1`. Unset dials NATS directly. |
| `ENVOY_NATS_REPLICAS` | no | The deployment's replica count, from 1 to 5; defaults to `1`. Dispatch creates the `ENVOY_NOTIFICATIONS` stream and its webhook redelivery bucket with this many copies and raises one it finds with fewer, at start and every 30 seconds after; `envoy-dispatch redeliver-webhooks` reads it too. More than 1 needs a clustered NATS. Set the listener's value: the copies stay up only when every program carries the same one. |
| `ENVOY_STREAM_MAX_BYTES` | no | The `ENVOY_NOTIFICATIONS` stream's byte limit in bytes: a full stream refuses new publishes, which the outbox retries, and keeps what it holds until it is 72 hours old (the listener's row says how to size it); unset creates the stream with none and keeps a deployed one, and set, Dispatch raises a smaller limit, or none, to it at start and every 30 seconds and never lowers one. Set the listener's value: the stream holds the largest limit any program carries. |
| `DISPATCH_AGENT_TOKEN` | yes | Shared devbox fallback bearer token; per-person tokens minted in Dispatch Settings are preferred for individual agents. To rotate it, set the new value followed by the old one, separated by a space or a line feed, move every consumer, then set the new value alone (`cmd/dispatch/README.md`). |
| `DISPATCH_SIGNIN_ISSUER` / `DISPATCH_SIGNIN_CLIENT_ID` / `DISPATCH_SIGNIN_CLIENT_SECRET`, with `DISPATCH_SIGNIN_GROUP` or `DISPATCH_SIGNIN_DOMAIN` | cookie identity, to sign anyone in | The issuer people sign in through, Dispatch's client there and its secret, and exactly one of: the group a person must be in, through a Cognito sign-in pool; or the Google Workspace domain whose people may sign in, with the issuer `https://accounts.google.com` and a Google web client. Cookie identity takes them together or not at all: with the issuer, client ID, secret and one of the two it signs people in, and with none of them it boots, serves its agent-token API and signs nobody in, answering every request that needs a person `401 NO_IDENTITY` — a request carrying a `dsession` cookie issued while sign-in was on included, since `DISPATCH_SIGNING_KEY` goes on verifying that cookie for its thirty days and no membership check is left. Some without the others, both the group and the domain, a domain under another issuer, or a group under Google's refuse to boot. Header identity is for tests and local harnesses only, and refuses every one of these settings. `DISPATCH_ALLOWED_LOGINS` is removed and refused at startup. |
| `DISPATCH_LISTEN_HOST` | no | Defaults to `127.0.0.1`; for direct tailnet access, set it to `$(tailscale ip -4)`, never `0.0.0.0`. |
| `DISPATCH_PORT` | no | Defaults to `8766`; the healthcheck follows it. |
| `DISPATCH_IDENTITY` | no | `cookie` (default) is Dispatch's production identity. `header:<name>` is only for tests and local harnesses. |
| `DISPATCH_IDENTITY_HEADER_TRUSTED` | test/local header identity | Set to `1` for every header-identity test or local harness. A missing flag refuses boot. |
| `DISPATCH_REPO_PROJECTS` | no | Optional boot seed for repository-to-project settings (`owner/repo=KEY,...`). Existing dashboard settings are not overwritten. |
| `DISPATCH_DEFAULT_PROJECT` | no | Native Dispatch project for external repositories without a stored mapping. |
| `DISPATCH_NATS_DISABLED` | no | Set to `1` to run database and live-socket paths without NATS. |
| `ENVOY_URL` | no | Envoy listener base URL for live agent titles; defaults to `http://127.0.0.1:9020` (set it when `ENVOY_LISTENER_PORT` changes). |
| `ENVOY_TOKEN` | conditional | Matching Listener API bearer token when `ENVOY_API_TOKEN` protects the listener. |
| `DISPATCH_OIDC_ISSUER` | conditional | OIDC issuer whose projected service-account tokens authenticate as agents beside `DISPATCH_AGENT_TOKEN`. Set with `DISPATCH_OIDC_AUDIENCE` or not at all; half the pair refuses to boot naming the missing one. An issuer that does not answer discovery refuses no boot: Dispatch starts and serves, discovers the issuer in the background, and until then answers a JWT-shaped bearer `503 OIDC_ISSUER_UNAVAILABLE` (`/healthz` reports `service_tokens` false) while every other credential is served. |
| `DISPATCH_OIDC_AUDIENCE` | conditional | Audience those tokens must carry (`dispatch`). Set with `DISPATCH_OIDC_ISSUER` or not at all. |
| `DISPATCH_URL` | no | Host-adapter override for the Dispatch base URL; use with `DISPATCH_TOKEN`. |
| `DISPATCH_TOKEN` | host adapters | Bearer token paired with `DISPATCH_URL`. |
| `DISPATCH_APP_CLIENT_ID` | GitHub App | The GitHub App's client ID; with its private key (the `pem` in the data volume's `app.json`) Dispatch handles webhooks and architecture sources and reads GitHub for the web app. `DISPATCH_APP_CLIENT_SECRET` is removed and refused at startup. |
| `DISPATCH_INSECURE_COOKIE` | HTTP | Set to `1` whenever browsers reach Dispatch over `http://` (the tailnet deployment); leave unset behind an HTTPS terminator. |

The Dispatch signing material is a named volume; the database is external.

## Backups and restore

The database's nightly logical backup is not this compose's job: the production
deployment's own scheduled backup task `pg_dump`s the `dispatch` database to object
storage and alerts when a day has no dump; restores go through that deployment's own
restore tooling. `scripts/autodeploy.sh` still takes its own pre-deploy dump of
`DATABASE_URL` before every image roll, kept locally; it never restores one itself.

## Sync to a remote host

```bash
./packages/envoy/deploy/scripts/sync-host.sh user@hostname
```

This ships only `deploy/`; the remote runs a pinned image and does not build
the listener or Dispatch image locally.
