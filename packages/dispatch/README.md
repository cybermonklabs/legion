# Dispatch

Dispatch is the React single-page application for the native Dispatch workspace. A
workspace issue holds its status, primary specification, open asks, comments and
suggestions, artifacts, and durable event history. The Go server in
`packages/envoy/cmd/dispatch` serves the production build from `web/dist`.

## Application shape

`AuthGate` requests `GET /auth/whoami` and sends unauthenticated visitors to the
Google sign-in flow. All application requests are same-origin. React Router serves
the Inbox at `/`, an issue workspace at `/issues/:key/*`, and the
settings page at `/settings`; TanStack Query and the live socket's events
feed keep the issue, Inbox, documents, and sidebar current.

The desktop shell has a sidebar, issue content, and review margin. Issue content has Spec,
Conversation, Children, Artifacts, and Activity tabs; the Conversation lists one card per
conversation and the Activity tab holds the issue's whole event history. The Spec document
renders and edits through `@legion/proof-editor`, with Yjs presence and margin-linked marks. The
margin holds Comments and Pinned. Each issue-owned anchored comment thread is one card with flat
replies, inline reply and author-edit controls, mark-aligned cross-hover, and resolved history
behind `Resolved (N)`. Project-document thread replies are handled by the margin-by-owner
surface. Below the `xl` breakpoint, navigation is a drawer and the margin is a bottom sheet;
opening a thread uses a full-height view with a bottom-pinned composer. Controls use 44 px
minimum touch targets.

An upload replaces whatever document the owner already holds under the name being uploaded, so it names the token of the version it replaces.
It reads that document's token when the person chooses the file to upload (`GET /api/v1/issues/{key}/artifacts/{slug}/text`, which resolves a slug or a filename) and sends it as the upload's `precondition`, so a write landing between that choice and the upload is refused `409 PRECONDITION_FAILED` instead of writing over the text it left.
That is the whole window the dashboard guards: a change made before the file was chosen is replaced without a refusal, since the upload names the token the document held at the choice.
Every attempt of one press sends that one token, because the idempotency key covers the whole request body.
A first upload of a name replaces no version and names no token, and neither does a document whose tree is outside the Proof schema: it renders no markdown and so has no token, and an upload is how it is repaired.
Any other failure of that read ends the upload and says so, rather than uploading with no token: the page does not know what it would be replacing.

## Development

Run commands from this package:

```bash
bun run dev
bun run build:web
bun run typecheck
bun run lint
bun run test
bun run test:web web/src/path/to/file.test.tsx
```

`bun run build:web` produces `web/dist` for the Dispatch server. `bun run dev`
runs the Vite development server for interface work. `bun run test:web <file>` runs one unit
test file; a bare `bun test <file>` loads no DOM, since the DOM comes from the preloads
`test:web` names (`web/src/__tests__/register-dom.ts`).

## End-to-end tests

`bun run e2e` builds `@legion/contracts` and the SPA and drives Playwright
against the real Go Dispatch
server and Postgres. Playwright starts the harness itself: `e2e/global-setup.ts`,
its `globalSetup`, starts one stack per worker (`e2e/harness-pool.ts`:
the fakes, then `e2e/run-server.sh`, then the
plain-HTTP proxy) unless `PLAYWRIGHT_BASE_URL` selects a deployed server, and
`e2e/global-teardown.ts` stops them. It keeps the stacks' logs unless the run
showed that it passed. A run of one stack writes none, since that stack's
servers write to the run's own stderr.
Playwright runs under Node, which reads `@legion/contracts` from that package's
built `dist/`, and a fresh checkout has no `dist/`: `bun install` makes none.
The hook does not build it and cannot. Playwright loads
`e2e/playwright.config.ts` before it runs any hook, and that config imports the
package, so a run that has not built it is already dead when the hook is
reached. Whoever starts Playwright builds it instead: `bun run e2e` and the
package's other e2e scripts run `build:contracts` first, every CI job that runs
the suite carries a `Build contracts for dispatch e2e` step
(`.github/workflows/dispatch-browser.yaml`), and a Playwright command you type
yourself needs `bun run build:contracts` once.
The hook refuses the run before it starts a
harness or a Postgres when Playwright has not downloaded a browser the run
needs, naming `bun run e2e:install` and how large that download is.
`e2e/run-server.sh` builds the server
with the caller's `go` into `packages/envoy/dispatch-e2e` and execs it, so a
SIGTERM to the pid it hands its caller stops the server. It pins every server
setting: cookie identity, the production mode, with `alice` and `bob` signed in through the
server's dev sign-in route (`DISPATCH_DEV_SIGNIN=1`, fenced to a loopback
origin, listener and database, with a signing key the server generates for its
process, or one key per run in two-process mode, below),
`DISPATCH_NATS_DISABLED=1`, the fake GitHub origin, a throwaway App
key, a loopback listen host and the suite's dashboard origin. Address the
harness as `127.0.0.1:<port>`, never `localhost`: under that flag the router
refuses any other `Host`. The server process has no caller Home or XDG
directory and receives no inherited `DISPATCH_*`, `ENVOY_*` or `NATS_*`
variable, so neither a shell setting nor `~/.config/legion/envoy.json` /
`~/.local/share/dispatch` can redirect it. There are two ways in from outside.
One is the broker switch below: a URL in `DISPATCH_E2E_AGENT_SECRETS_URL`, with the token
of a ServiceAccount that broker lists in
`DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE`, points the server's credential
requests at a broker the caller runs, as the secrets broker's docs rig
(`docs/site/media/broker/rig.sh`) does with the token its local issuer
(`packages/envoy/cmd/dev-oidc-issuer`) keeps. The other is
`DISPATCH_E2E_NATS_URLS`: a NATS address there reaches the server as its
`NATS_URLS` in place of `DISPATCH_NATS_DISABLED=1`, which turns the events
publisher on for a driver that proves what a delivery writes. It is the rig's
own variable because a shell's `NATS_URLS` is the Envoy listener's, and a server
that took it would publish test notifications where real sessions receive them.

`DATABASE_URL` is a one-stack run's input unless it names a Postgres of its own
(stacks, below), and must name an isolated
loopback database: `e2e/seed.ts` empties it before every row and never selects a shared
default, and the dev sign-in flag refuses a non-loopback host. psql runs without
`PGHOSTADDR` (`e2e/psql.ts`), which would otherwise send it somewhere the server
never checked. The harness ports `DISPATCH_E2E_PORT`, `FAKE_ENVOY_PORT`,
`FAKE_GITHUB_PORT`, `PLAIN_HTTP_PORT` and `FAKE_BROKER_PORT` are its other
inputs, resolved for the whole suite by `e2e/harness-ports.ts`. Unset, each
server serves on a port the OS chooses and holds until it serves: the Dispatch
server on a socket `scripts/e2e/lib/held-port.py` hands it, each Bun listener by
binding port 0. Each reports its port on its listening line, and
`e2e/global-setup.ts` writes them into the `DISPATCH_E2E_STACKS` manifest every
worker inherits, which is the only variable that hook writes, so
no run shares a port with another and
none empties the database behind a server it did not start. Set, a port is
used as given, and an occupied one fails with `address already in use`.
`DISPATCH_E2E_REUSE_SERVERS=1` says the ports name a harness you started
yourself, and the hook then starts nothing. `e2e/harness-ports.ts` states that
rule in full — the accepted values and what a bad or duplicated port does. The
harness starts `e2e/fake-envoy.ts` and that listener is the only Envoy the
server ever talks to; tests seed its
live sessions with `setLiveSessions` from `e2e/agents.ts`, stop one of them with
`setSessionStopped` (the listener still knows it, so a send to it is stored and
Dispatch records the attempt `queued`), and say what a send answers it was with
`setSessionSendState`. It starts
`e2e/fake-broker.ts` as the secrets broker, so the
credential-request feature is on, and tests seed its pending requests with
`setPendingCredentialRequests` from `e2e/fake-broker-helpers.ts`; an empty
`DISPATCH_E2E_AGENT_SECRETS_URL` runs the suite with no broker instead, as a
deployment that configures none (`e2e/run-server.sh` and `e2e/harness-broker.ts` have the
switch). It also starts
`e2e/plain-http-proxy.ts` for the plain-HTTP project.

`DISPATCH_E2E_EMBEDDER=fake` starts one listener more, `e2e/fake-embedder.ts`, and points the
server's meaning search at it: the server calls it as it calls Bedrock, and it counts the calls
it was asked for, so a row reads back how many embeddings a stretch of typing cost. The rows of
`e2e/search-tiers.e2e.ts` need it and are skipped without it, since a suite whose assertions
count search hits counts the keyword answers Dispatch gives a deployment with no embedder:

```bash
cd packages/dispatch
DISPATCH_E2E_EMBEDDER=fake bun run e2e -- --project=chromium --project=iphone search-tiers
```

`DISPATCH_E2E_ACCESS_ISSUER=1` gives a run the secrets broker's service-account token, so a
driver can read the routes `DISPATCH_ACCESS_ACCOUNTS` gates (the membership answer and the
repository answer, listed in `packages/envoy/cmd/dispatch/README.md`), which a harness without it
refuses to every caller. The harness then starts one row more before the server,
`e2e/run-access-issuer.sh`: `packages/envoy/cmd/dev-oidc-issuer`, which keeps the broker
account's token in a file in the harness's own directory and reports its port as
`DISPATCH_E2E_ACCESS_ISSUER_PORT`. The server verifies that issuer's tokens and lists the account
in `DISPATCH_ACCESS_ACCOUNTS` (the account and the audience are in `e2e/access-account.sh`), and
the harness exports the file's path as `DISPATCH_E2E_ACCESS_TOKEN_FILE`, so the command
`e2e/acceptance/with-harness.sh` runs reads the token from it
(`e2e/acceptance/membership-answer.sh`, below). Unset, or any other value, starts no issuer and
sets nothing, which is how every spec runs (`e2e/harness-access.ts`).

Run the local harness with its isolated database available:

```bash
cd packages/dispatch
DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch_<issue>?sslmode=disable' \
  bun run e2e
```

One spec is `bun run e2e -- e2e/claim.e2e.ts --project=chromium`, which builds the contracts and
the SPA first, or the Playwright command itself, as in
`bunx playwright test --config e2e/playwright.config.ts --project=chromium e2e/broadcast.e2e.ts`,
which builds nothing. Playwright stops the harness
when the run ends, a Ctrl-C included, and exits with its own status.

A SIGTERM stops the stacks this suite starts and no more. A run that holds a stubbed row also
starts Playwright's own `webServer` (`e2e/stub/static-server.ts`), which Playwright stops on a
SIGINT and leaves running on a SIGTERM: measured on playwright 1.64.1, a signalled mixed run
exited 143 with every stack of this suite gone and that static server still serving its fixed
port, so the
next run holding a stubbed row was refused with
`http://127.0.0.1:4319/index.html is already used`. It is Playwright's own leak, not this
suite's - a run of the stubbed layer alone, which installs no signal handler here, leaks the
same server - so `kill` that process, or use a Ctrl-C, which stops everything.

A run killed outright - SIGKILL, an out-of-memory kill, a runner that crashes - runs no stop,
and what it started stops all the same: each harness, and each run's stack Postgres, hands
what it starts to a reaper (`Reaper`, `e2e/local-instance-support.ts`), which ends it once the
process that started it is gone: every process group of the harness, each stack's container
by the name the harness gives it, unique to the run, and each cluster of the binaries provider
by its data directory. A container and a cluster are handed over before `docker run` or
`pg_ctl start` begins, so a run killed while one starts leaves nothing the reaper cannot name,
and the reaper removes again while a start of that name or data directory still runs.
`e2e/__tests__/harness-orphans.test.ts` kills the parent of each with SIGKILL and finds
nothing left: the listeners and an interrupted start in every run, and a stack's real Postgres
where the run names a provider (`DISPATCH_E2E_PG_IMAGE` or `DISPATCH_E2E_PG_BIN`).

Playwright's UI mode starts the stacks itself. The UI server runs `globalSetup`
when a client connects rather than when it begins listening, so a session
opened on the listening URL starts its own stacks, runs rows against them, and
stops them when the session ends: measured on playwright 1.64.1,
a second Run all in
the one session left no second set of stacks, and closing it left nothing
behind. Every project is in play in such a session, whatever `--project` the
command line held, so it needs all three engines downloaded
(`bun run e2e:install`); with only Chromium, it stops at its `global-setup.ts`
node while the same command line without `--ui` runs green.

To point a session at a stack that is already up instead, start one, apply the
variables it prints, and start the UI in that environment. The file holds the
stack's `DATABASE_URL` with any password it carries, so it is made for you
alone: the subshell creates it under `umask 077` and then runs the harness in
its place, so `$!` is the harness, and a file an earlier run left is removed
first, since an existing file keeps its mode:

```bash
cd packages/dispatch
rm -f exports.sh
(umask 077 && exec bun e2e/harness-up.ts > exports.sh) &
# wait for its last line, `export DISPATCH_E2E_REUSE_SERVERS=1`, then
. ./exports.sh
bunx playwright test --config e2e/playwright.config.ts --ui
```

`kill` that harness when the session ends. `e2e/visual-run.sh` is the same
recipe written as a script, for the container it runs Playwright in. A session
of the stubbed layer alone needs no stack at all, since it addresses none:
`--project=stub --ui` opens as it is.

Playwright's own `--workers <n>` runs the suite in parallel, and the config's
default is 1. `e2e/global-setup.ts` starts one stack
per worker before the first row. A stack is a whole harness of its own — a
Dispatch server, the three fakes, the plain-HTTP proxy, a file-store bucket and
ports the OS chose — and a Postgres of its own, because `e2e/seed.ts` empties
its stack's database before every row. A worker finds its stack by
`TEST_PARALLEL_INDEX`, which Playwright gives it and a restarted worker keeps;
`e2e/harness-ports.ts` resolves it against the `DISPATCH_E2E_STACKS` manifest
the hook wrote, for the ports and for psql's database alike.

A stack's Postgres comes from one of two variables, and a run of more than one
stack that sets neither is refused. On a machine with Docker and no Postgres
binaries, `DISPATCH_E2E_PG_IMAGE` is a `docker run` of the image digest you
name; CI's visual, multiprocess and embedder jobs and the two-build job's
browser half use it too, since they run on the runner. On a machine with a
Postgres installation and no Docker, such as an issue pod or CI's shard and
alone jobs, which run in a container carrying PostgreSQL 18 with `vector`,
`DISPATCH_E2E_PG_BIN` is a directory holding `initdb` and `pg_ctl`.
Each stack's data lives in its own `worker-<i>` directory under the run's own
temporary directory and goes with the run. Either way the server runs with
crash safety off (`fsync`, `full_page_writes`, `synchronous_commit`), so no
commit waits for the disk: a database you name in `DATABASE_URL` keeps its
own settings, and where its commits wait for a disk that stalls, a write a
closed page left in flight can hold a table past the reset's 5 s.

```bash
cd packages/dispatch
DISPATCH_E2E_PG_BIN=/usr/lib/postgresql/18/bin \
  bun run e2e -- --workers 4
```

`DISPATCH_E2E_PG_STALL_DIR`, an absolute directory that is empty or does not
exist yet, makes either provider build `e2e/fsync-stall.c` into it with `cc`
and start the server with that library preloaded: while `stalled` exists in the
directory, every `fsync` and `fdatasync` of the server waits, as on a disk that
has stopped answering. The harness makes the directory readable by every user
and mounts all of it into a container, so it refuses one that holds anything
but an earlier build's files, and one whose path holds any whitespace or a
colon: the loader's `LD_PRELOAD` splits on a space and a colon.
`e2e/read-marker-disk-stall.multiprocess.e2e.ts` stalls it so. Without the
variable it is skipped, in an agent's shell too, which sets `CI`; on GitHub
Actions (`GITHUB_ACTIONS=true`) it fails naming the variable
(`e2e/disk-stall.ts`). A server you start yourself, one a run reaches
through `DATABASE_URL`, takes part once you build the library for it, since
the harness builds it only for a server it starts:

```bash
cd packages/dispatch
cc -shared -fPIC -O2 -o <dir>/fsync-stall.so e2e/fsync-stall.c
```

Start that server with `LD_PRELOAD=<dir>/fsync-stall.so` and
`PG_STALL_FILE=<dir>/stalled` in its environment, where it sees `<dir>` at the
same path, and make the directory and the library readable by the user the
server runs as (a container's `postgres`, for instance), or its loader skips
the library with a warning. The run then sets `DISPATCH_E2E_PG_STALL_DIR=<dir>`
beside `DATABASE_URL`, so the row stalls the disk through the same file. As it
stalls the disk, the row reads on the server, as the superuser the suite
connects as, that the stall holds it: a `wal_sync_method` of `fsync` or
`fdatasync`, that very call made by the server (`COPY ... TO PROGRAM` of
`timeout 1 sync --data /dev/null` for `fdatasync`, `sync /dev/null` for
`fsync`, so `timeout` and coreutils `sync` must be on the server's `PATH`)
going through before the stall and waiting during it, and `<dir>`'s library in
its memory map. A server short of any of them fails the row, naming what it
lacks.

A run of more than one worker is one stack per worker, each with a Postgres, a
file-store bucket and ports of its own, so five variables that name one of
those are refused before anything starts:

- `DATABASE_URL`, because each stack runs its own Postgres, whose URL the
  harness hands its servers and its psql,
- `PLAYWRIGHT_BASE_URL`, because a deployed run has one server every stack
  would share,
- `DISPATCH_E2E_REUSE_SERVERS`, because the servers a person started are one
  set,
- `DISPATCH_E2E_PROCESSES=2`, because two-process mode is two servers on one
  stack's database,
- any harness port you set yourself, because a port is one listener's.

Run instead a Postgres provider with those five unset: `DISPATCH_E2E_PG_BIN`, a
directory holding `initdb` and `pg_ctl`, or `DISPATCH_E2E_PG_IMAGE`, each of
which is a Postgres per stack. The other way is one worker, which the config
defaults to and the one case that takes a `DATABASE_URL`.
`e2e/global-setup.ts` refuses the run before it starts anything, naming the
variable and the reason, so you meet a sentence rather than an `EADDRINUSE`.
The refusal is the harness's own rule, not one caller's habit: a run of more
than one worker meets it however you started it, because one stack owns each of
the five. CI's shard leg sets `DISPATCH_E2E_PG_BIN` and none of the five, so
the rule reaches no job of this repository and only a run by hand.

Every other mode runs on the default one worker, and so one stack: the deployed
run, two-process mode,
a run against servers you started (`DISPATCH_E2E_REUSE_SERVERS=1`) and
`bun run dispatch:local`. `e2e/harness-pool.ts` and `e2e/harness-ports.ts` hold the whole model.

`e2e/run-server.sh` on its own is the server for a shell caller: with
`DATABASE_URL`, `FAKE_ENVOY_PORT`, `FAKE_GITHUB_PORT` and the broker switch's
input (`FAKE_BROKER_PORT`, or an empty `DISPATCH_E2E_AGENT_SECRETS_URL`) set and
`DISPATCH_E2E_PORT` unset, it writes
`msg="dispatch: listening" addr=127.0.0.1:<port>` to stderr once the server
serves; read the port from that line. `$!` of a backgrounded run is the server's
pid. A caller that needs the port before the server starts (the broker docs rig
names it in the broker's settings) starts `scripts/e2e/lib/held-port.py keep`
first and sets `DISPATCH_E2E_LISTEN_FROM` to the keeper's unix socket: the
server serves on the keeper's port.

Two-process mode runs two Dispatch servers against the one database, behind a round-robin proxy,
the way a rolling release serves a browser from two builds at once:

```bash
cd packages/dispatch
DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch_<issue>?sslmode=disable' \
  bun run e2e:multiprocess
```

CI's multiprocess job runs it on the harness's own Postgres instead, with the disk stall the
read-mark row needs, which a run by hand can do the same way:

```bash
cd packages/dispatch
DISPATCH_E2E_PG_IMAGE=ghcr.io/cybermonklabs/legion/ci@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11 \
  DISPATCH_E2E_PG_STALL_DIR="$(mktemp -d)" bun run e2e:multiprocess
```

It builds the dashboard twice (`web/dist`, and `web/dist-b` under another build id), then runs
Playwright with `DISPATCH_E2E_PROCESSES=2`: Dispatch A on
`DISPATCH_E2E_PORT_A` serving `web/dist`, Dispatch B on `DISPATCH_E2E_PORT_B` serving
`DISPATCH_E2E_WEB_DIST_B` (here `web/dist-b`; default A's), and `e2e/round-robin-proxy.ts` on
`DISPATCH_E2E_PORT`, which alternates requests and websockets between them. As every harness
port, each is used as given when set and chosen by the OS when unset; the proxy starts before
the servers, since both serve its origin, so A's and B's ports are held first (a
`held-port.py keep` each) and each server serves on its keeper's port. Both servers sign
cookies with one key the run generates, so a person stays signed in whichever answers.
`DISPATCH_E2E_SERVER_BIN` runs a prebuilt `envoy-dispatch` instead of the one `run-server.sh`
builds from this checkout, and `DISPATCH_E2E_BINARY_B` gives Dispatch B one of its own (the
harness hands it on as B's `DISPATCH_E2E_SERVER_BIN`), so a run can put two builds of the server
against the one database as `DISPATCH_E2E_WEB_DIST_B` puts two builds of the dashboard there;
`scripts/e2e/dispatch-two-builds.sh` and the cross-build spec below are the proofs that use
them. It runs
the `*.multiprocess.e2e.ts` specs but the sign-in spec below, in Chromium, which the default run
leaves out; arguments after `--` reach Playwright
(`bun run e2e:multiprocess -- <name>.multiprocess.e2e.ts`). A spec
pins a request to one process with the `X-Dispatch-E2E-Process: a|b` header or a whole browser
context with the `dispatch-e2e-process` cookie, or splits a context with the `dispatch-e2e-page`
and `dispatch-e2e-api` cookies (its page from one process, its API, sockets and `/auth` from the
other), and reads which process served what from the proxy's `/_proxy/requests` and
`/_proxy/sockets`; `e2e/harness-processes.ts` and `e2e/round-robin-proxy.ts` hold the whole
contract. Any
`DISPATCH_E2E_PROCESSES` other than `1`, `2` or unset is refused.

To run one build's page against another build's server, give B a previous release, as CI's
`dispatch-two-builds` job does with the directory `packages/envoy/scripts/build-dispatch-build.sh`
writes:

```bash
DISPATCH_E2E_BINARY_B=<dir>/envoy-dispatch DISPATCH_E2E_WEB_DIST_B=<dir>/web \
  bun run e2e:multiprocess -- cross-build.multiprocess
```

B then runs that envoy-dispatch and serves that web dist (both paths absolute or from
`packages/envoy`). That build must be `legion-envoy-v30.11.0` or later, which
`build-dispatch-build.sh` refuses to go below: it writes the default assignee every project needs
since migration `20261111210103`, it serves on the port the harness holds for it, as
A does, and signs cookies with the run's shared key, so no run names a port for B and the run's
global setup checks it like any other B. `e2e/cross-build.multiprocess.e2e.ts` splits each browser
context, its page from one build and its API, sockets and `/auth` from the other.

`DISPATCH_E2E_SIGNIN=oidc` signs people in as a deployment does instead of through dev sign-in:
the harness also starts `packages/envoy/cmd/dev-oidc-issuer` (`e2e/run-signin-issuer.sh`), and
both servers take it as their sign-in issuer, under one `DISPATCH_SIGNING_KEY` per run. Only
`e2e/signin.multiprocess.e2e.ts` runs then, which starts a sign-in at `/auth/start` on Dispatch A
and completes it at `/auth/callback` on B:

```bash
DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch_<issue>?sslmode=disable' \
  DISPATCH_E2E_SIGNIN=oidc bun run e2e:multiprocess
```

It needs `DISPATCH_E2E_PROCESSES=2`, which `e2e:multiprocess` sets.

`DISPATCH_E2E_SIGNIN=google` signs people in as a deployment that signs in with Google and takes
people from outside with a passkey does: the same issuer stands in for Google, and each server
admits the Workspace domain `example.com` (`DISPATCH_SIGNIN_DOMAIN`) and no Google account outside
it. Each server answers at a
second origin, `http://localhost:<port>` (`DISPATCH_OUTSIDE_ORIGIN`), beside its canonical
`http://127.0.0.1:<port>`, and passkey sign-in is on there (`DISPATCH_SIGNIN_PASSKEY=1`): a person
outside the domain whom a project's access list names enrolls a passkey through the link the
project's owner makes on its Access tab, and signs in with it as a restricted member. The issuer
signs in nobody of its own: a spec queues each person, with the claims Google gives that account,
at the issuer's `POST /_dev/sign-in-as`. The person's device is Chromium's virtual authenticator,
which the spec adds through the page's CDP session (`WebAuthn.addVirtualAuthenticator`); the mode's
one project is `chromium`. Only `e2e/outside-sign-in.e2e.ts`, which finds a Google account outside
the domain refused and no Google sign-in at the second origin, `e2e/outside-passkey.e2e.ts`, and
`e2e/leaver-hold.e2e.ts`, which holds a person's personal token once the issuer refuses their
refresh as Google refuses an account it no longer confirms, run then, with one server process or
two:

```bash
DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch_<issue>?sslmode=disable' \
  DISPATCH_E2E_SIGNIN=google bun run e2e -- --project=chromium outside-sign-in outside-passkey
```

Any `DISPATCH_E2E_SIGNIN` other than `oidc`, `google` or unset is refused, and `bun run
dispatch:local` refuses either of them.

The rules a row follows:

- A row binds `page`, `browser` or `context`, and a file it writes goes under
  `testInfo.outputPath()`. A claim that needs no browser goes in `e2e/__tests__/` or a Go test
  (`e2e/no-browser-rows.ts`).
- A row takes under 2 s, or carries `// SLOW(>=2s): <why>` directly above it;
  `bun e2e/tools/check-slow-rows.ts <report.json>` checks a run's report for both.
- A helper whose next step depends on what the page shows reads it with `settledState`
  (`e2e/settled.ts`), never with a one-instant read such as `isVisible()` or `count()`.
- An assertion fails with the change reverted, and what decides its scope, such as the seeding
  or a selector, is asserted too and seen failing once: a selector that cannot fail is no guard.
- A row of `e2e/__tests__/` starts a subprocess it waits for through `runBounded`, and takes its
  timeout from `rowLimit(...)` (`e2e/__tests__/bounded-subprocess.ts`).
- A markdown shape is a file in `packages/envoy/internal/dispatch/pmdoc/testdata/corpus/` and
  `bun run gen` in `pmdoc/gen` (`pmdoc/doc.go`), never a string in a spec.
- A real-stack row takes `@pr`, the pull-request lane's tag, only with a `// @pr: <why>` line
  beside it.
- In an Inbox test, move focus and the pointer off a row before you expect it to leave the list:
  the Inbox keeps the row under the reader's hand drawn (`ViewportAnchor`, `heldRow`), and
  `.check()` and `.click()` leave the pointer on it.
- In the windowed Agents list, role queries skip a hidden row where CSS locators, `getByText` and
  counts do not (`shownAgentRows` and `agentRow` in `e2e/agents.ts`), and `toBeHidden()` passes
  when nothing matches, so put `toHaveCount(1)` beside it where the row must be mounted.

## Local instance

`bun run dispatch:local`, from the repository root or from this package, brings up a seeded
Dispatch signed in as a person, to prove a change to a surface only a human acts on (the Inbox and
its snooze, the Agents page and its direct messages, a broadcast) with no production step. It is
the end-to-end harness, left running: it builds the SPA, starts the harness through
`e2e/harness-start.ts` as a Playwright run's stack pool does (the fake Envoy, the fake GitHub, the fake secrets
broker, `e2e/run-server.sh` and the plain-HTTP proxy) on ports the OS chooses, empties the
database, seeds `e2e/workspace.ts`, checks over
HTTP that a cookie from the dev sign-in route reads `/auth/whoami`, the Inbox, the broadcasts and
the agents, and prints:

- the URL that opens the dashboard signed in as `alice` (and one for `bob`),
- the seeded broadcast's page,
- the agent bearer (`e2e-token`) a local Oh My Pi session reaches this instance with,
- the database, the fake Envoy's session list and its reseed endpoint,
- the command that runs a spec against it, with the ports it serves on.

Open exactly the printed `127.0.0.1` origin: `localhost` is another `Host`, and the server refuses
it (`421 HOST_MISMATCH`). Ctrl-C stops every process it started, and so does a failure at any
step; the database's container stays.

Its inputs are the harness's own. `DATABASE_URL`, when set, is used as given: it must be a single
`postgres://` URL whose host (its `host` query parameter when it has one, as for libpq) is
`127.0.0.1`, `localhost`, `[::1]` or a Unix socket directory, and it is emptied and reseeded on
every start. Unset, the command runs `packages/envoy/scripts/dev-postgres.sh`, which creates,
starts or reuses the `dispatch-pg` container, and creates database `dispatch_local` in it once.
That script runs the container from its pinned image, pgvector on PostgreSQL 18, the version
Dispatch's tests and dev databases run; Dispatch's migrations need its `vector` extension. It
refuses a `dispatch-pg` on any other image or major and prints the commands that dump its
databases and restore them into a new container on the pinned image, quoted so they paste as they
read. The harness ports (`DISPATCH_E2E_PORT`, `FAKE_ENVOY_PORT`, `PLAIN_HTTP_PORT`,
`FAKE_GITHUB_PORT`, `FAKE_BROKER_PORT`) are unset unless you need a fixed one: unset, the OS
chooses each, so two instances never collide; set, the port is used as given, and an occupied one
fails with `address already in use`. psql runs without `PGHOSTADDR` (`e2e/psql.ts`), and a Ctrl-C
ends a psql in flight too.

A spec runs against the instance with the printed command, which sets
`DISPATCH_E2E_REUSE_SERVERS=1`, the five ports and the same `DATABASE_URL`; each spec empties the
database and the
fake Envoy, so restart the command to reseed it. `bun run build:web` while it runs refreshes the
SPA, which the server reads from `web/dist` on each request. `docker rm -f -v dispatch-pg` removes
the container, its anonymous data volume and every database in it.

## Acceptance drivers

`e2e/acceptance/block-graph.sh` drives the block graph end to end against a running server, as an
agent sees it, through the HTTP API and the real `dispatch` CLI
(`packages/envoy-client/bin/dispatch.ts`, run in an environment the script builds, so a calling
shell's Dispatch URL, token file and Envoy configuration never reach it). It covers a comment
citing a block and the block's backlinks, a deleted and restored block, a pinned block reference
that changes and is removed, `GET /api/v1/block-links` between the blocks of two documents, each
block's changed version, every block `part_of` its document, a link in a list item, the six facts
every node carries (`made_by`, `made_at`, `changed_by`, `changed_at`, `parent`, `parent_version`)
for an issue, a document, a file, a picture, a block, a removed pinned block, an ask, a comment, a
message, a session and a component, read by its own reference, as an edge's other end and on the
`Node:` line of `dispatch read`, and the older `GET /api/v1/artifacts/{id}/references` route. It
also covers the blocks of comments and messages: a mention in a comment starts at the comment's
block holding it, a node of kind `block`, id `comment/<uuid>#b<n>`, whose `parent` is the comment
(A10);
a comment or message reply that names a block (`--reply-to <comment>#b-b2`,
`--in-reply-to <message>#b-b2`) is a `replies_to` edge to that block, and `dispatch read` quotes it
(A7); the facts of a comment's and a message's block (A11); a block's id kept through a person's
move, reword, split, deletion and restore over `PATCH /api/v1/comments/{id}` with carried ids, and
a body holding block HTML read back byte for byte (A13); and a link pinned to a comment's block that
reads `changed`, then `removed` (A15). It also reads the bounds the server owns and no client
carries: a comment and a message of 48,000 characters taken and read back whole, a paragraph of
32,001 characters refused by name, and `dispatch limits` (A8); a passage only at the end of a block
longer than 32,000 characters found by meaning and a one-line ask option found by its text, each
hit naming the block (A9, skipped with one `SKIP:` line on a server with no embedder); and a
question of 2,001 characters taken by `dispatch ask` and by a document's decision block (A14).
Each passing check prints `PASS: <section> <what>`; the first
failure prints `FAIL: <section> <what>: <observed>` and exits 1. Its inputs:

- `DISPATCH_URL`, the server;
- `DISPATCH_TOKEN`, an agent bearer, or `DISPATCH_TOKEN_FILE`, a file holding one, which the script
  reads itself and never prints;
- `PROJECT`, an existing project key;
- `COMPONENT_SOURCE`, optional, where the component nodes come from (unset, the script reads no
  component node and prints one
  `SKIP: R12 component: COMPONENT_SOURCE is unset, so no component node is read; …` line):
  - `fixture` seeds the harness's fake GitHub on `FAKE_GITHUB_PORT` (9022 under
    `bun run dispatch:local`) with a two-component model in `acme/r12-arch` (`web`, part of
    `platform`), replacing every repository the fake served, adds that repository as an
    architecture source of `PROJECT`, syncs it, and removes the source on exit, also when a check
    fails;
  - `existing` reads the first project whose architecture holds a component with a parent, and
    writes nothing to it; a server with none is a `FAIL`;
  - `none` proves `GET /api/v1/settings/architecture-sources` is `[]` and prints
    `PASS: R12 component: this server has no architecture source, so it holds no component node`;
    a source present is a `FAIL` naming `COMPONENT_SOURCE=existing`.
- `DEV_SIGNIN_LOGIN`, optional, a login the server's dev sign-in route (`/auth/_dev/signin`) signs
  in: `bun run dispatch:local` has the route, a deployed server does not. Editing a comment needs
  the `person` scope, and is its author's alone, so the A13 and A15 comment sections sign that person in with a
  cookie jar, write the comments that person edits, and send each edit with the dashboard's origin.
  Unset, each of those two sections prints one
  `SKIP: … PATCH /api/v1/comments/{id} needs the person scope, which this bearer does not hold, and this server has no dev sign-in: DEV_SIGNIN_LOGIN is unset; …`
  line, and every other section runs.

It writes two issues in `PROJECT`: the first holds the spec, comments, messages and the documents
`cites.md`, `decisions.md`, `changes.md` and `list.md`; the second is its child, retitled, holding an
ask the script rewords, a comment on a quote of its spec and a reply, a message and a reply, and the
uploads `notes.txt` and `pixel.png`. It resolves the ask and closes both issues, also when a check
fails.

Its `A9` section needs a server with an embedder, so the harness is started with
`DISPATCH_E2E_EMBEDDER=1`, which gives it the development embedder beside its dev sign-in
(`e2e/run-server.sh`); without one that section prints its `SKIP:` line and every other section
runs. The harness leaves the embedder off by default, so that a suite whose assertions count
search hits counts the keyword answers Dispatch gives a deployment with no embedder.

Against `bun run dispatch:local`:

```bash
DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
  COMPONENT_SOURCE=fixture FAKE_GITHUB_PORT=9022 DEV_SIGNIN_LOGIN=alice \
  bash packages/dispatch/e2e/acceptance/block-graph.sh
```

`e2e/acceptance/verification.sh` drives the verification rules the same way, through the HTTP API
and the same real CLI. It covers a `plan_task` and a `check` that serve a spec's `requirement`, and
the `serves` and `needs` edges `GET /api/v1/references` lists for them; every attribute a `check`
carries, read back on the head of `dispatch read --ref`; the refusals a pointer earns — a block no
document holds, a block of an issue in another tree, a `needs` naming a block outside its own plan
— and a `when` the schema does not know, each `400 INVALID_MARKDOWN` naming the pointer or the
attribute; a requirement's id through a `move`; `dispatch result` and `dispatch results`, with a
check id no Verification document in reach holds refused `400 UNKNOWN_CHECK` and a pass counting a
skipped case refused `400 RESULT_CONTRADICTS`; a closed issue's result for an `after_merge` check,
which it takes beside a comment and a message, and the result for a `before_merge` check and the
document write it still refuses `409 ISSUE_CLOSED`; the coverage flags `dispatch read --issue` prints after `Coverage: `,
cleared by a check of the issue and by a check of a child, back when that check is deleted, with
the missing plan task named only once the issue has a plan document; the gap an approval request's
answer names and the covered spec that earns none; `dispatch plan-approve`, refused `409
NOT_PLAN_DOCUMENT` on a spec, and `dispatch request-approval`, refused `409
PLAN_APPROVED_BY_REVIEW` on a plan; the one request that names a spec and its Verification
document together; and, in the reading section, every result of a check printed with all forty
characters of its commit, beside a result at a commit that repository does not hold refused
`400 RESULT_COMMIT_UNKNOWN`. Its inputs are `DISPATCH_URL`, the bearer, `PROJECT`, and two
optional ones:

- `FAKE_GITHUB_PORT`, the harness's fake GitHub, which the server reads as
  `DISPATCH_GITHUB_API_BASE`. The run gives it one repository holding one pull request whose
  title names the issue and one further commit beside that pull request's head, replacing every
  repository the fake served. Where `PROJECT` has no default repository, the reading section
  seeds a repository of its own, sets it as `PROJECT`'s default and clears that default on exit.
  Where `PROJECT` has one, the setup seeds that repository and sets nothing. The results of the
  check are then compared with that pull request's head, a result at the further commit — whose
  first twelve characters are the head's — stands at none of them, and a result at a commit the
  repository holds neither of is refused. Unset, one `SKIP: reading …` line stands in for the
  section, which no other way of running has a pull request's head to read.
- `DEV_SIGNIN_LOGIN`, as `block-graph.sh` takes it. Recording a review is a person's write alone,
  so the two review sections sign that person in with a cookie jar and answer the approval requests
  the script opened; unset, each prints one
  `SKIP: review …: DEV_SIGNIN_LOGIN is unset; POST /api/v1/artifacts/{id}/reviews needs a person's …`
  line and every other section runs.

One rule governs every result the script records, beside the refusals above.
The record route takes a result at a 40-character commit the repository of the check's issue holds, or at one a repository the issue links an open or merged pull request in holds (`https://github.com/<owner>/<repo>/pull/<n>`), and refuses any other `400 RESULT_COMMIT_UNKNOWN`, naming the commit, each `owner/repo` it asked, at most 10 of the linked pull requests whose state it could not read and a count of the rest, and every linked pull request in whose repository it could not read the commit, with how to mend it.
It asks the issue's own repository first, and reads the links only where GitHub does not say that repository holds the commit: where GitHub proves it does not, and where that read does not answer, which stores the result under the issue's own repository unless a linked repository holds the commit.
Where a read of a linked pull request or of a linked repository does not answer and no repository is shown to hold the commit, it refuses the result `502 GITHUB_UNREADABLE`, and where its own read of the issue's links fails, it answers a server error, `500 INTERNAL`; either way it stores nothing, and the caller records the result again.
One record reads at most 10 linked pull requests, each distinct repository once, and a refusal says how many links the bound left unread.
The script links no pull request to any issue it writes, so no linked repository is asked about any result it records.
Only GitHub's `422 No commit found for SHA` refuses, and the script reads `PROJECT`'s default repository once, in its setup (`default_repo` on `GET /api/v1/projects`), to know which of two runs it is.
Where `PROJECT` has no default repository, every result recorded outside the reading section is stored because the check's issue resolves to no repository, which is never asked about; inside the section, each stored result names a commit it seeded its fake GitHub with, and the one commit it seeds nothing with is the refusal it drives.
Where `PROJECT` already has one, every issue the script writes resolves to that repository, so the setup reads that repository's default-branch head from GitHub with `gh` and every result is recorded at that head: no check stands down, and the run prints no `SKIP` for a recorded result.
A repository `gh` cannot read fails the run, naming it.
Where `FAKE_GITHUB_PORT` is set the setup also gives that repository, that head, the look-alike and the pull request to the fake, since a server reading the fake asks the fake and not GitHub; a server reading the real GitHub needs no such seeding, and a fake-backed run proves this script's fixture and its count rather than a server talking to GitHub.
Every other check runs either way, the `result` section's two refusals among them: a malformed field and an unknown check are refused before any commit is read.

It writes four issues in `PROJECT` — a root with a spec, a Verification document and a plan, a
child of it, a sibling in another tree and one more whose spec and Verification document are
approved together — and closes every one of them, also when a check fails, which withdraws any
approval request it left open. Nothing it writes is read by a second run. Against
`bun run dispatch:local`:

```bash
DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
  FAKE_GITHUB_PORT=9022 DEV_SIGNIN_LOGIN=alice \
  bash packages/dispatch/e2e/acceptance/verification.sh
```

`e2e/acceptance/write-guards.sh` drives the three guards on a write the same way, through the
real CLI and the HTTP API beside it. A person approves an issue's specification and the question
`dispatch ask` opens on that issue names the review: who gave it, when, that they approved, their
reason, and the line asking the sender to say what changed since it. An agent uploads a document
holding a question, the person answers it, and the agent uploads the body it composed again,
writing the same question with no `#id`: the uploaded block takes the live block's id, so the
issue still holds that one answered ask and the settlement writes the answer onto the block
again. An upload naming a token the live document has moved past is refused `409 PRECONDITION_FAILED`,
whose answer carries the live token, the live version and `dispatch doc-read`; the command exits
non-zero, nothing is written, and the same upload naming the token `dispatch doc-read` prints is
taken. A replacement that names no token at all is taken unchecked, as it was before #3000, since
no deployed `dispatch` command could name one (INFRA-4980), so that check runs against the one
server the rest of the script drives. Its inputs are `DISPATCH_URL`, the bearer, `PROJECT` and
`DEV_SIGNIN_LOGIN`, which is required here because the review and the answer are a person's
writes. It writes three issues in
`PROJECT` and closes all three, also when a check fails. Against `bun run dispatch:local`:

```bash
DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE DEV_SIGNIN_LOGIN=alice \
  bash packages/dispatch/e2e/acceptance/write-guards.sh
```

`e2e/acceptance/ci-check.sh` drives a check that CI proves the same way, through the HTTP API and
the same real CLI: a `check` block carrying `ci_check`, which names the GitHub check run that
proves it. It has one section for each of the checks `k1` to `k5` of the Verification document of
the work that added it, and each runs that check's `run` and asserts its `negative_control`:

- `k1`: a check with `ci_check` is stored and `dispatch doc-read` reads it back; the documents API
  refuses `400 INVALID_CI_CHECK` each of the four writes that break a rule, naming the block and
  the rule, and stores nothing of them: `ci_check` beside a `run`, `ci_check` with a `when` of
  `after_merge`, `ci_check` with no `negative_control`, and `ci_check` added to a check that
  holds a recorded result. A `ci_check` on a fresh block id is stored, and a new block under the
  old id is refused again. A check carrying neither `ci_check` nor `run` is stored as before. A
  document socket that declares schema version 3 is admitted with the scope `readonly`, and one
  that declares version 4 with `read-write` (`e2e/acceptance/socket-scope.ts`, the Hocuspocus
  frames the browser sends). The live editor's half of `k1` is the browser row of
  `e2e/verification.e2e.ts`.
- `k2`: `dispatch results` answers the check with the result of the named run at the issue's open
  pull request head: the run's name, conclusion, output and URL, and a pull-request body line that
  names the run and no recorder. A failed run prints `fail`, and `check_results` holds no row for
  either block. The head of the merged pull request still answers. Three such checks at one head
  cost one check-runs read (`GET /__fixture/requests`), an open head is read before an older
  merged one, an issue with no such check reads no check runs, and a summary of 2,500 runes is cut
  to 2,000.
- `k3`: the eleven outcomes of the run, among them the fake GitHub's `500` and a read the fake
  holds 12 s, past the server's own 10 s deadline, which therefore takes 10 s of the run. Two runs
  of one name are decided by the higher id, in both orders of their conclusions.
- `k4`: a tester's and an implementer's result for a check CI proves are refused `409
  CHECK_PROVED_BY_CI`, naming the block and the run; the same call against an ordinary check is
  recorded and `dispatch results` shows it.
- `k5`: the whole line the reviewer's gate reads (verdict, commit, role and where the commit
  stands), then a failed run, a head moved to a commit the run does not cover, and a head the fake
  GitHub serves that Dispatch's own head record does not hold yet.

Its inputs are all required: `DISPATCH_URL`, the bearer, `PROJECT`, `FAKE_GITHUB_PORT` (the fake
GitHub the server reads), `DATABASE_URL` (the server's own database) and `DEV_SIGNIN_LOGIN` (a
document socket admits a person alone, so the sockets of `k1` sign that person in). The harness
runs with NATS disabled, so no `pull_request` event stores a pull request head: the script stages
each head with `psql` as one `pull_request_heads` row attributed to its issue, and gives the fake
one repository holding the same pull requests and each head's check runs, replacing every
repository the fake served. That repository is `PROJECT`'s default where it has one, and
otherwise one of the run's own, which is no project's default. It writes nine issues in
`PROJECT` and closes every one of them, also when a check fails; on exit it deletes the rows it
staged and empties the fake. Against `bun run dispatch:local`, with the URL, the fake GitHub's
port and the database it prints:

```bash
DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
  FAKE_GITHUB_PORT=9022 DATABASE_URL=<the database it prints> DEV_SIGNIN_LOGIN=alice \
  bash packages/dispatch/e2e/acceptance/ci-check.sh
```

`e2e/acceptance/block-coverage.sh` takes the first three inputs, with `PROJECT` optional (unset,
every project), and only reads. For every document it can list (each project's documents and each
issue's documents), it counts the document as not indexed when `GET /api/v1/artifacts/{id}/blocks`
lists blocks while `GET /api/v1/references?to=<document>&kind=part_of` lists none, and prints
`documents=<n> indexed=<n> not_indexed=<n>`, then each not-indexed document's reference. A document
with no blocks counts as indexed, since it owes no nodes.

`e2e/acceptance/graph-store.sh` drives the people and the agents of the node and tuple store: what
each person authored, what reached each session, a comment that reached nobody, a failed attempt,
an attempt the listener queued for a session that is not running, and the role, the session and
the page a delivery writes. It takes the first three inputs,
`DATABASE_URL` (the database that server serves, on this machine, which it reads the tuples no
route answers through) and `FAKE_ENVOY_PORT` (the harness's fake Envoy, whose session fixture it
adds role-less sessions, one of them stopped, and a role's holder to and puts back on exit), with `DEV_SIGNIN_LOGIN`
optional. Its delivery checks need the server to have a bus: `bun run dispatch:local` leaves NATS
off, and without `DISPATCH_E2E_NATS_URLS` (which `e2e/run-server.sh` hands the server as its
`NATS_URLS`) the publisher writes no `delivered_to` tuple and those checks wait out their 30 s
poll. Start a JetStream NATS of your own (`nats-server -js`) and name it when you start the
instance, `DISPATCH_E2E_NATS_URLS=nats://127.0.0.1:<port> bun run dispatch:local`; then, against
it:

```bash
DATABASE_URL=postgres://dispatch@127.0.0.1:5433/dispatch_e2e FAKE_ENVOY_PORT=9023 \
  DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
  DEV_SIGNIN_LOGIN=alice bash packages/dispatch/e2e/acceptance/graph-store.sh
```

`e2e/acceptance/graph-sizing.sh` drives `envoy-dispatch census --store --sizing`: the write rate
by kind, every table this branch's migrations touch, where the two store tables reach in a year,
two runs printing one report, and the block-index backlog, with the reconciliation's own drain
timed beside a request that indexes the same document itself, a reading that gates nothing. It
takes the first three inputs, `DATABASE_URL`, the built `envoy-dispatch` on `PATH`, and two
required inputs of the production reading: `PRODUCTION_PROJECT`, the Google Cloud project whose
Cloud Monitoring holds production's disk use, and `PRODUCTION_DATABASE_ID`, the Cloud SQL instance
whose disk use it reads, through this pod's own Google identity. This repository is public and
names neither, so the driver refuses to start without them (exit 2). `PRODUCTION_WINDOW_DAYS` is
optional (21 unset, refused below 14). It writes a corpus under an issue of its own and removes
it on exit, since a corpus left behind is read by the next run as growth:

```bash
DATABASE_URL=postgres://dispatch@127.0.0.1:5433/dispatch_e2e DISPATCH_URL=http://127.0.0.1:8777 \
  DISPATCH_TOKEN=e2e-token PROJECT=CORE PRODUCTION_PROJECT=<gcp-project> \
  PRODUCTION_DATABASE_ID=<gcp-project>:<cloud-sql-instance> \
  bash packages/dispatch/e2e/acceptance/graph-sizing.sh
```

`e2e/acceptance/replace-blocks.sh` drives what `doc-edit`'s `replace` does with a `with` that
names blocks: a list written over a paragraph's whole text, a list written over the text of a
nested list item, the refusals in a heading, in a table cell and for a `with` opening with the
matched item's own marker, and the markdown each edit answers with. It takes the first three
inputs, with `PROJECT` optional (unset, it makes a project of its own), and writes one issue in
that project. The create of that project names `DISPATCH_DEFAULT_ASSIGNEE`, the person its
unowned work lands on: the bearer acts for no person, so the server refuses a project that names
none (`400 INVALID_PROJECT`). That input is required with `DISPATCH_URL` and no `PROJECT`; with
`DATABASE_URL` it defaults to `alice`, whom the instance this run starts seeds and signs in; and
with a `PROJECT` the server already holds, nothing is created and it is not read. It is the one
driver that can start its own server: given `DATABASE_URL`, a
loopback database of the run's own, it starts `bun run dispatch:local` and stops it again from
its own trap. That command truncates and reseeds the database it is given, so before anything
starts the run asks the server the connection string reaches, through `psql`, which database it
is (`current_database()`) and which port it answers on (`inet_server_port()`), and asks the same
through `LEGION_TEST_PG_DSN` where that is set. It refuses a connection string it gets no answer
through, and a database of the same name as the shared one on a server it cannot prove is
another: two names that differ pass, and two equal names pass only where both servers answered a
port and the ports differ. It parses neither string itself, so the database it judges is the one
libpq connects to, however the string spells it; it reads `DATABASE_URL` with no `PG*` variable
set, as the instance reads it.
The connection string the run is given must carry everything libpq needs to connect — host, port,
user, password and TLS settings among them — since the run unsets every `PG*` variable before it
starts the instance, so none survives into it.

```bash
createdb -h 127.0.0.1 -p 5433 -U legion_test dispatch_local_replace
DATABASE_URL=postgres://legion_test@127.0.0.1:5433/dispatch_local_replace?sslmode=disable \
  bash packages/dispatch/e2e/acceptance/replace-blocks.sh
```

`e2e/acceptance/create-document.sh` drives the command line's own create, the one that makes a
document from a title with no file and no text, through the same real CLI. It takes `DISPATCH_URL`,
the bearer and `DISPATCH_DEFAULT_ASSIGNEE`, a person who has signed in to that server: the bearer
acts for no person, so the project it creates must name the person its unowned work lands on
(`400 INVALID_PROJECT` otherwise). It makes the project and the issue it needs, and runs ten
assertions:
`dispatch artifact --issue <KEY> --name "Launch notes"`, with neither `--path` nor `--content`,
exits zero and says the document was made, naming its `dispatch://` reference; that reference
reads back one version whose markdown is empty; `dispatch read --issue <KEY>` lists the document
under `Documents:`; `dispatch doc-edit` writes a line into it and the line reads back; the same
name a second time with no content is refused, naming the document that holds the title; that
same create with `--force` is refused too, since a create with no content has no version
`--force` could ask for; `--name rows.jsonl` with no content is refused by the Markdown-name
rule, which sends nothing; and `--path` and `--content` each still make a document. Each passing
assertion prints `PASS: <n> <what>` on stderr, the first failure prints `FAIL:` and exits 1, and
a run that reaches the end writes `all <n> assertions passed` as the one line on stdout, so a run
cut short fails a reader that reads that line.

`e2e/acceptance/with-harness.sh` runs one command against a stack of its own, for a driver that
has no server to speak to: it starts `e2e/harness-up.ts`, waits for the stack's exports, applies
them, runs the command with `DISPATCH_E2E_PORT` and the rest in its environment, and stops the
stack when the command ends, exiting with the command's own status. From `packages/dispatch`:

```bash
DATABASE_URL=<a PostgreSQL this run may truncate> bash e2e/acceptance/with-harness.sh \
  bash -c 'curl -sS -o /dev/null "http://127.0.0.1:$DISPATCH_E2E_PORT/auth/_dev/signin?login=alice" &&
    DISPATCH_URL=http://127.0.0.1:$DISPATCH_E2E_PORT DISPATCH_TOKEN=e2e-token \
    DISPATCH_DEFAULT_ASSIGNEE=alice bash e2e/acceptance/create-document.sh'
```

The `curl` is the harness's dev sign-in (`/auth/_dev/signin`), which records `alice` as a person
who has signed in, so the project may name her.

`e2e-token` is the agent bearer the harness pins. Until cybermonklabs/legion#3435 the same thing
was written `bun e2e/run.ts -- <command>`; that program is gone, and the browser suite starts its
stacks through Playwright's own global setup now, which no shell driver can reach.

`e2e/acceptance/membership-answer.sh` drives the membership answer the secrets broker reads, its
three routes `GET /api/v1/access/projects/{key}`, `GET /api/v1/access/person` and
`GET /api/v1/access/private-projects`, on a stack of its own that holds the broker's token
(`DISPATCH_E2E_ACCESS_ISSUER=1`, above). With that token it reads alice's member kind and an
unknown login's, a project of the run's own as bob sees it, and the private-projects list as
exactly `{"projects":[]}`; it then makes its project private with SQL (nothing in this release
switches one) and reads it listed with its Slack channel and the time its list changed, hidden
from bob and seen by alice, its owner. Alice's own `dsp_` token and her cookie are each
`403 ACCESS_ACCOUNT_REQUIRED` on all three routes. On exit it makes the project public again, and a
run that cannot exits non-zero naming the project, since the next run's empty list would find it.
Its inputs are what `with-harness.sh` exports: without the switch there is no
`DISPATCH_E2E_ACCESS_TOKEN_FILE`, and the run stops with exit 2, naming the switch; a token file
it cannot read stops it the same way, naming the file.
It writes the database it is given, so it refuses a `DATABASE_URL` whose host is not loopback, or
that carries a connection override such as `?host=`, with exit 1, naming `DATABASE_URL` without
printing it.
From `packages/dispatch`:

```bash
DATABASE_URL=<a PostgreSQL this run may truncate> DISPATCH_E2E_ACCESS_ISSUER=1 \
  bash e2e/acceptance/with-harness.sh bash e2e/acceptance/membership-answer.sh
```

Every driver here but `document-roundtrip.sh`, `omp-roundtrip.sh`, `outside-path-reach.sh` and
`restricted-sweep.sh` sources `e2e/acceptance/lib.sh`, the preamble and the check vocabulary they
share: it refuses an unset input or a missing command with exit 2, reads the bearer from
`DISPATCH_TOKEN` or `DISPATCH_TOKEN_FILE` without printing it, unsetting both so no process the
script starts inherits it, and holds the `pass`, `fail`, `skip`, `want_status`, `expect` and
`query` every driver checks with. It also holds `start_horizon_watch` and `watch_horizon`, with
which `node-projects.sh` watches the event horizon from 5 s before it sends its event and names a
transaction of its own database that has held one of the watch's observations back for 5 s, and
the `observe_horizon` and `own_horizon_hold` they are built on
(`e2e/__tests__/node-projects-horizon-hold.test.ts` drives all four against a database).
`with-harness.sh` is no driver and sources none of it.

`e2e/acceptance/outside-path-reach.sh` is the driver of the check `c-outside-path-reach`: it
measures which of our listed addresses a machine reaches over the path given to a person from
outside. What it measures is TCP connects from the machine it runs on to the listed addresses and
ports, nothing more: it cannot see a service the origin itself forwards by path or by Host on its
own host and port. So it runs where the path starts. For a path that only that person's device
has, such as a tunnel, that is the device itself, not a VM, a container or a remote shell, whose
network is another one; for a public path, any host on the public internet will do, since a public
load balancer answers every address alike. It is handed to that person as the one file with its
target list, so it sources nothing and needs only bash (3.2, as macOS ships it, or later), curl
7.40 or later and sleep, all three on a stock macOS or Linux; curl's own `--connect-timeout` and
`--max-time` bound every
probe, so it needs no GNU `timeout`. A run takes about 29 seconds. Started by a shell that is not
bash, such as dash, busybox `sh` or zsh, it prints `UNKNOWN: run this with bash` and stops; the
`sh` of macOS is bash, and runs it. Its inputs:

- `OUTSIDE_ORIGIN`, the path's origin, `https://<host>[:<port>]`, an IPv6 host in brackets, its
  host on the rule a target's host follows;
- `OUTSIDE_PATH_TARGETS`, a file, or a pipe such as `/dev/stdin`, of `name host port` lines, one
  per internal target (the hub, the listeners, the secrets broker, the other Dispatch origin),
  ending in LF or CRLF. `#` starts a comment, and comments are ASCII like the rest of the list.
  `host` is a name or a literal address; a name has no empty label (`a..b`, `.example`); an IPv4
  address must be four numbers of 0 to 255 with no leading zero (curl reads `172.020.000.021` in
  octal, another host); an IPv6 address must be hexadecimal digits and colons alone, so curl reads
  none as a URL range (it reads `[1-1:1]` as one), and must not be IPv4-mapped however it is
  written: its text must not start with `ffff:` once its leading zeros and colons are dropped, as
  `::ffff:a00:1` and `0:0::ffff:a00:1` do (write `10.0.0.1`), since a v6-only device reaches no
  mapped address; and a `svc:` selector, which resolves nowhere, is refused. **The list must hold
  each of our services' addresses on the path as lines of their own, not names alone**: a name
  line proves only where that name resolves on the person's device, so a name that resolves
  elsewhere there says nothing of the address behind it. A list of names alone prints a warning.
  **The list is ASCII**: every byte must be printable ASCII, a tab or LF, but a CR that comes
  directly before an LF and a UTF-8 byte-order mark at offset 0, which the script drops before
  anything parses the list. Any other byte, a NUL, a control character or any byte above 0x7E (so
  any non-ASCII text, in a comment too), refuses the run, naming the byte and its offset counted
  from 0: `read` splits only on LF, and a reader that split on that byte would put the next target
  in the line's comment, where it would be lost. The script reads the list once, to its end, with
  the device's `TMOUT` unset, since bash's `read` takes it as a timeout; a read that stops before
  the end refuses the run.

Two settings bound the two reads of the origin, each as curl's `--connect-timeout` and
`--max-time`: `OUTSIDE_PATH_ORIGIN_CONNECT_SECONDS` (3 s when unset or empty) and
`OUTSIDE_PATH_ORIGIN_READ_SECONDS` (10 s). Each must be a whole number of seconds from 2 to
999999, in digits; any other value refuses the run, naming the setting. A curl built with the
synchronous resolver (neither threaded nor c-ares) gives up before it resolves a name, 127.0.0.1
included, with under 1000 ms of the bound left, which a 1 s bound reaches once 1 ms has passed;
curl reads 0 as no bound; and curl 7.55 and later refuse a bound past 2147483 s where a long has 32
bits. They do not bound the probes, whose 3 s to connect and 5 s in all the classifier's rules
count on. The script's tests set both to twice their longest row's timeout, so a fake Dispatch that
a loaded host keeps off the CPU makes a read late, never a verdict wrong.

```bash
OUTSIDE_ORIGIN=https://dispatch-outside.example OUTSIDE_PATH_TARGETS=targets.txt \
  bash outside-path-reach.sh
```

It asks the origin for `/healthz` (200) and for `/api/v1/projects` with no credential (401, with
Dispatch's own `NO_IDENTITY` refusal), then probes the origin's own host and port and every target
in five rounds started 6 s apart (0, 6, 12, 18 and 24 s), every host of a round at once. The
rounds are 6 s apart because a tunnel whose first handshake packet was lost stays down until
WireGuard sends it again, after its `RekeyTimeout` of 5 s plus up to 334 ms; probes started
together would all fall inside that.
Its curls ignore the device's `~/.curlrc` and any proxy, and read no URL as a pattern (`-g`), so
the run measures what that device itself reaches. No line of its output names the device's host
name, since that output can be posted where anyone reads it. Each probe is judged by whether TCP
connected, not by curl's exit code alone, since our services accept a connection and then wait for
the client:

| curl's answer | Class |
|---|---|
| a completed connect (`time_connect` above zero), whatever the exit | `REACHED` |
| no connect, in a round in which the origin did not connect either | `uncounted` |
| exit 7, refused | `unreachable` |
| exit 28, timed out, with no lookup completed | `unresolved (lookup timed out)` |
| exit 28 after a lookup of under a second | `unreachable` |
| exit 28 after a lookup of a second or more, too little of the 3 s left to connect | `UNKNOWN`, naming the lookup time |
| exit 6, the name does not resolve | `unresolved` |
| anything else | `UNKNOWN` |

A target's round counts only when the probe connected to the origin in that same round: a round
in which the device reached not even the origin proves nothing of a target, which reads
`uncounted` there unless it connected, so `PASS` needs the origin reached in every round. A
target is `REACHED` when any of its five rounds connected and `unreachable` only when all five
counted and were, so a network that loses packets does not hide one easily. Measured with a
reachable listener whose incoming packets were dropped at random, 120 runs per row: at 70% loss
no run printed `PASS` under bash 3.2 or 5.2 (194 and 201 of 600 rounds missed), where the same
rig with three rounds printed `PASS` in 8 of 240; at 50% loss three rounds already printed none
in 240. Heavier or longer loss can still miss all five. `PASS` (exit 0) needs Dispatch's
answers, a connect to the origin itself in every round and every target unreachable; `FAIL`
(exit 1) names each target reached, or an origin that served a read with no credential; every
run that proved nothing is `UNKNOWN` (exit 2), never a pass. Its classify mode,
`OUTSIDE_PATH_CLASSIFY="<exit> <namelookup> <connect>"`, classifies one recorded probe, and five
of them separated by `;` are judged as one target's rounds in order; that mode exits 4, never
`PASS`'s 0. The script's own header has every rule. A path that puts a sign-in of its own in front
of Dispatch, such as an identity-aware proxy, answers a request with no credential with that
sign-in, not Dispatch's, so the run reads `UNKNOWN` there.
`cd packages/dispatch && bun test e2e/__tests__/outside-path-reach` drives it against real
listeners, from four files: `outside-path-reach-input.test.ts` for what it decides before any
probe (the inputs it refuses, the origin's two answers and its classifier),
`outside-path-reach.test.ts` for the runs that probe, `outside-path-reach-blackhole.test.ts` for
the two runs that need a network dropping TEST-NET-1, which skip with the reason printed where the
network refuses that address at once, and `outside-path-reach-tmout.test.ts` for a list on a pipe,
a named pipe or a process substitution under an inherited `TMOUT`. Each row carries its own
timeout. The row of `outside-path-reach-input.test.ts` that runs the script under zsh skips where
zsh is missing, so a local run needs zsh for that row; CI's `dispatch-ui` job installs it.

`e2e/acceptance/restricted-sweep.sh` is the driver of the check `c-restricted-sweep`: a read sweep
of a deployment as a restricted member, for the objects of one project whose access list does not
name that member. Its parts are the files beside it: `restricted-sweep-lib.sh` (requests and
lines), `-fills.sh` (the fills file), `-writer.sh` (writer mode), `-lookup.sh` (the identities and
the full member's lookup), `-judge.sh` (the row rule and the rows) and `-sockets.sh` (the sockets).
A full member's token looks the project's objects up first: an issue whose spec has blocks, a
comment, an ask, a message, a handoff, a document of the project itself, a near-copy check, a
priority, a broadcast and a delivery run, with a person and a session that worked in it and a
repository only its issues resolve to. It also asks one question of the project as the full member
(`POST /api/v1/going/requests`), since no route lists a question's run, and reads the conversation
file `SWEEP_CONVERSATION_FILE` names, since no route lists those either. Then every GET row of the
route index (`packages/envoy/internal/dispatch/api/testdata/routes.txt`) is called as the member,
its wildcards filled with those objects, and each list once more for each of its query parameters
that names a project's object (read from the API document): a filter by a session
(`author_session`, `parent_agent`) with a session of the project alone, answered to the member as
one nobody holds, and a filter by a person (`assignee`, `author`, `approver`, `person`) with a
person of the project, whose answer the markers read; `session`, the session a caller acts as, is
no filter. A GET route the deployment's own index (`GET /api/v1`) lists and `routes.txt` lacks was
never called, and leaves the run `UNKNOWN`, naming it; a test hook (a `_test` path) is no
deployment's. A direct message to a session that
worked in the project alone is read by its own routes too, the live socket's agent view of that
session must answer the member as a session no one answers for, and every read the GitHub proxy
forwards (`/api/github/rest/`, `githubapi/proxy.go`'s `readable`) is called with the repository
only the project's issues resolve to. Each answer must be 404, 403, what a missing object is
answered, or one that holds no row of the project; any answer for the project's object (a 2xx, or
a refusal other than 403 and 404) where a missing one is answered otherwise is a row of it too,
since it says the object exists, and a proxy read must answer as a repository the App is not
installed on does, or refuse 403. `GET /api/v1/github/app-installation/{owner}/{repo}`, which the
server holds to the proxy's rule, must answer the member exactly what it answers them for a
missing repository, status and text, filled with the repository only the project's issues resolve
to (one the member's own project resolves to as well may answer them installed); it can fail only
where the App is installed on that repository and not on a missing one. A string an answer holds
is a row when it is, whole, the project's key, an issue key of it or an object the lookup found. A
string a person wrote (a `body`, `title`, `snippet` and the like) counts no project key and no
issue key, even whole, and a found object only whole, since a member's own comment may cite the
project's items, even as its whole body; any other string with no whitespace (a reference, a path,
a URL) is one too when such a key or object stands in it as a whole part between `/ : # ? & =`. A
session or a person the lookup read in the project that works in no project the member sees is a
row of it as well. It also holds both sockets: the member is not admitted to the room of the
project's document, while the same probe admits them to a document of their own, and the member's
event feed, open for the whole of `SWEEP_LIVE_SECONDS` (60 by default), is sent no event of the
project while the full member comments on `SWEEP_WRITE_ISSUE` and sends the project's session a
direct message that names no issue, both of which the full member's own feed must be sent; and
the member's feed must be sent the member's own writes of the window, the control of their feed,
which takes one of two forms, exactly one given: with `SWEEP_MEMBER_SESSION`, the direct message
the member sends their own session; with `SWEEP_MEMBER_ISSUE`, for a member who holds no session
(a browser's person alone), a comment and then a message, each a body alone, on that issue of a
project whose access list names the member, never of `SWEEP_PROJECT`. Their feed must be sent
each, and they read the message by `GET /api/v1/messages/{id}`. A subscription the server ends or
refuses on an open socket covers no window. The writes it makes are the full member's comment,
direct messages and question, and the member's own direct message, or comment and message. Its
inputs are `SWEEP_ORIGIN`
(`https://`, or `http://` to a loopback host), `SWEEP_PROJECT`, `SWEEP_FULL_TOKEN`,
`SWEEP_RESTRICTED_COOKIE` (the `Cookie` header's `<name>=<value>`), `SWEEP_MEMBER_SESSION` or
`SWEEP_MEMBER_ISSUE`, `SWEEP_WRITE_ISSUE` and `SWEEP_CONVERSATION_FILE` (`<session id>/<slug>`,
a file in the
conversation of a session that worked in the project alone), and `SWEEP_FULL_COOKIE`, the full
member's session cookie, which makes the GitHub proxy reads alone. Without it the proxy reads are
`SWEEP_FULL_TOKEN`'s, which a build from cybermonklabs/legion#5079 on reads; a build before it reads
the proxy as the browser's person alone and refuses the token (401, or 403 `OAUTH_SCOPE_REQUIRED`),
and each proxy read is then `UNKNOWN`, naming the build and `SWEEP_FULL_COOKIE`, never `PASS` or
`FAIL`, while every other row still runs. The direct messages and the agent view are made as
`SWEEP_FULL_TOKEN` too, which needs the `person` scope (a full member's personal token). It needs
bash 3.2 or later, curl
7.55 or later, jq 1.6 or later, and bun with this checkout's dependencies installed
(`bun install`), which runs the socket
probe `e2e/acceptance/socket-access.ts`. No token or cookie reaches a command line or the output.

```bash
SWEEP_ORIGIN=https://dispatch.example SWEEP_PROJECT=PRIV SWEEP_WRITE_ISSUE=PRIV-12 \
  SWEEP_CONVERSATION_FILE=<session id>/<slug> SWEEP_FULL_TOKEN=... SWEEP_FULL_COOKIE=... \
  SWEEP_RESTRICTED_COOKIE=... SWEEP_MEMBER_SESSION=<session id> bash e2e/acceptance/restricted-sweep.sh
```

Where no full member's credential may be, as on a public runner, the sweep splits in three. Fills
mode (`SWEEP_MODE=fills`), where the full member's token is, does the full member's half: the
lookup, the question and each row's can-fail call, and writes them to `SWEEP_FILLS_FILE` (ASCII
`name<TAB>value` lines, version 4, at most 40 KiB). The fills hold no credential. They hold the
target issue's title (the search rows' text), the member's login and the names of the project's
repository, its people and its conversation's file, and the origin they were made at by its
SHA-256 alone (`origin_sha256`). It names the route index it was
made from by its SHA-256 (`routes`), and the deployment by the SHA-256 of the key and creation
time of each of the member's own projects (`deployment`), which tell one deployment from another
and read the same at its private origin and its public one; fills mode and a runner each refuse a
list of projects that gives no creation time of each of them. A row that can fail is a
`can_fail<TAB><row>` line, each proxy read, the agent view and the app-installation row among
them, and every row it judged has a `full_status` line. A proxy read the full member's credential
made that cannot fail leaves no file; one the proxy refused the full member's token for is kept
with no `can_fail` line, which a runner reads `UNKNOWN`. Fills mode writes no file that lacks a
fill. It takes
the member's login in `SWEEP_MEMBER`, and reads the member's own projects as the ones whose access
lists name it. A runner, any run with `SWEEP_FILLS_FILE` and no `SWEEP_MODE`, does the member's
half with the member's cookie alone, refuses a full member's credential, and refuses fills of
another project, made from another route index than its own, older than 55 minutes, lacking a fill
whole fills hold, made at another deployment, or naming their origin by no SHA-256; it names the
origin they were made at, and the member's login, by the first 12 digits of their SHA-256 alone.
Those digits keep the value itself out of the log; they do not hide a login or a host name
someone can guess, whose SHA-256 anyone can take and compare. A row the fills hold no line of,
from fills made from another API document, was never asked of the full member: the member's
answer still FAILs it where it holds a row of the project, and otherwise the row is `UNKNOWN`,
never `blind`. The agent view's line says `(could fail when the fills were made)`: whether the
project's session answers is read then, and may have changed since. A runner reads the member's
live feed over a window it prints,
`live window <W0> <W1>` (UTC), whose end leaves the writer's 15 s delivery bound before the feed
closes, and which holds two of the writer's writes at 15 s, so `SWEEP_LIVE_SECONDS` is at least 45
on a runner; it makes the member's own writes alone, the direct message or the comment and the
message. Writer mode (`SWEEP_MODE=writer`) runs beside it with the full member's token and
cookie and the fills: every `SWEEP_WRITE_EVERY` seconds it comments on `SWEEP_WRITE_ISSUE` and sends
the fills' session of the project a direct message, and prints
`write <began> <ended> <comment id> <message id> delivered`, or `not delivered`, as both reach the
full member's own feed within `SWEEP_WRITE_EVERY` seconds (at least 5, at most that 15) or do not;
`<began>` is read before the comment is asked for and `<ended>` once the direct message is
answered (UTC). The runner's live check holds only once a delivered write falls strictly inside
its window, `<began>` after W0, `<ended>` before W1 and `<began>` no later than `<ended>`, each
`YYYY-MM-DDTHH:MM:SSZ`, which whoever ran both joins: a comment committed before the member's
feed subscribed counts for no window, and a line of another shape counts for none. The writer
sends the project's session `SWEEP_WRITE_SECONDS / SWEEP_WRITE_EVERY` direct messages (40 by
default), each a `btw` note that says it is the sweep's probe, which a live session takes at its
next turn; no route deletes a direct message, so a shorter `SWEEP_WRITE_SECONDS`, one that covers
the runner's window, sends fewer. The verdict is always a run's last line.

```bash
SWEEP_MODE=fills SWEEP_ORIGIN=... SWEEP_PROJECT=PRIV SWEEP_MEMBER=<login> SWEEP_FULL_TOKEN=... \
  SWEEP_FULL_COOKIE=... SWEEP_CONVERSATION_FILE=<session id>/<slug> SWEEP_FILLS_FILE=fills.tsv \
  bash e2e/acceptance/restricted-sweep.sh
SWEEP_MODE=writer SWEEP_ORIGIN=... SWEEP_PROJECT=PRIV SWEEP_WRITE_ISSUE=PRIV-12 SWEEP_FULL_TOKEN=... \
  SWEEP_FILLS_FILE=fills.tsv bash e2e/acceptance/restricted-sweep.sh &
SWEEP_ORIGIN=... SWEEP_PROJECT=PRIV SWEEP_FILLS_FILE=fills.tsv SWEEP_RESTRICTED_COOKIE=... \
  SWEEP_MEMBER_SESSION=<session id> bash e2e/acceptance/restricted-sweep.sh
# Or, for a member who holds no session, their own comment and message on an issue of theirs:
SWEEP_ORIGIN=... SWEEP_PROJECT=PRIV SWEEP_FILLS_FILE=fills.tsv SWEEP_RESTRICTED_COOKIE=... \
  SWEEP_MEMBER_ISSUE=OWN-3 bash e2e/acceptance/restricted-sweep.sh
```

Every row's line says whether it can fail (the full member is answered the project's object
otherwise than a missing one, or a row of it) or is `blind` (the full member is answered no row of
the project there either). A full member's read a row needs that is answered no rule reads (401,
429, a 5xx, no answer), or an answer the sweep could not mask, leaves the run `UNKNOWN`, naming it,
as does a GitHub proxy read or the app-installation row that cannot fail. A proxy read can fail
only where the full member's read of the project's repository is answered and a missing one's is
the proxy's own answer for a repository the App is not installed on (503
`GITHUB_TOKEN_UNAVAILABLE`); any other 5xx on either side, a rate limit or an outage, is no
answer, and a full member's 403, which GitHub answers an App that lacks a permission and the
member's half takes as a refusal, is no read that could fail. `PASS` (exit 0) needs every GET row
filled and called, every kind of object found, at
least one row that can fail and every socket control held; `FAIL` (exit 1) names each answer that
gave the member a row of the project and, in its verdict, the first; everything that proved
nothing, a project the full member finds nothing of among them, is `UNKNOWN` (exit 2). A `FAIL`
line names the route, the status and the row of the project the answer held (a key or an id; the
issue's title by its issue; the project's repository, a person of the project alone and a file of
its conversation by that kind), never the answer's text, and a line about either socket names the
member's answer by its status or frame types, never a body the server wrote, since a public
runner's log is public. `SWEEP_PACE_SECONDS` (0.1 by default) pauses before each request, which
keeps a run far under an edge's per-address request limit.
`e2e/__tests__/restricted-sweep-judging.test.ts`, `-sockets.test.ts` and `-split.test.ts` hold each
judging rule alone, against a fake Dispatch (`restricted-sweep-fake.ts`) that keeps every route
from the member but the one a row plants a leak in, and hold the sweep to calling every GET row,
read from what the fake was sent.
`bun run test:harness e2e/__tests__/restricted-sweep.test.ts` holds its inputs, the kinds it fills
wildcards with and the proxy reads it makes (against the server's route probes and `proxy.go`),
and, given `DISPATCH_E2E_PG_BIN` or `DISPATCH_E2E_PG_IMAGE`, runs it against stacks of the local
harness, in one run and split: this checkout's server, where it passes, every write the writer
makes is delivered and one falls inside the runner's window, and a build with no read check, where
it fails naming the route, a session's direct message, a GitHub proxy read, the agent view and both
sockets. The build with no read check is read from git: the commit `DISPATCH_E2E_SWEEP_BASE`
names, else the merge base of HEAD and `DISPATCH_E2E_SWEEP_BASE_BRANCH` (`origin/main` by
default), or, once a squash merge has put the read check on that branch, the commit just before
it. With neither variable set and a merge base that holds no restricted members, as every commit of
main's line does (the pull request that brought the read check to main brought restricted members
with it), it is b29b710bd5, kept by the tag `archive/INFRA-3862-slack-channels`, which the helper
fetches from origin where the clone lacks it (`pinnedControlBuild` in
`e2e/__tests__/restricted-sweep-stack.ts`). A commit that holds the read check, or no restricted
members, is refused by name.

```bash
DISPATCH_E2E_PG_BIN=<a directory holding initdb and pg_ctl, PostgreSQL with vector> \
  bun run test:harness e2e/__tests__/restricted-sweep.test.ts
```

`e2e/acceptance/outside-edge-refusals.sh` is the HTTPS refusal check of an outside edge: it asks the
edge's one IPv4 address, on 443, for each path and Host the edge must not forward, and reads
whether one of the edge's routes took the request, by the `X-Outside-Edge-Backend` marker every
route sets. `outside-path-reach.sh` cannot see this, since an address that answers on 443 forwards
by path and by Host there. It needs bash 3.2 or later, curl 8.4.0 or later, `mktemp`, and
`sha256sum` or `shasum`, and reads PASS (exit 0), FAIL (exit 1) or UNKNOWN (exit 2):

```bash
OUTSIDE_EDGE_ADDRESS=192.0.2.10 OUTSIDE_EDGE_ROUTED=routed.txt \
  OUTSIDE_EDGE_REFUSALS=refusals.txt bash outside-edge-refusals.sh
```

[`e2e/acceptance/outside-edge-refusals.md`](e2e/acceptance/outside-edge-refusals.md) says what its
inputs are, how it reads an answer, when a signal ends it, and what its three test files hold;
`cd packages/dispatch && bun test e2e/__tests__/outside-edge-refusals` runs them.

`e2e/acceptance/outside-agent-tailnet.sh` and `e2e/acceptance/outside-agent-public.sh` are the two
legs of the outside agent's acceptance: what a person from outside the sign-in domain gets when
their agents, on their own machine and on the public internet, work with ours through the edge
origins, and the gate both legs make before a real person from outside is named on an access
list. The tailnet leg is the one command and runs on a developer box; it starts the public leg
on a runner on the public internet. `e2e/acceptance/outside-agent-acceptance.md` says how to run
them, what each row reads, and the controls. `cd packages/dispatch && bun test
e2e/__tests__/outside-agent-acceptance.test.ts e2e/__tests__/outside-agent-public.test.ts` checks
that each leg prints `UNKNOWN`, never `PASS`, for a row whose target is absent, and that each leg
holds its rows to their controls (the public leg's P6, P7, P10 and T5 part; the tailnet leg's T2,
T3, T5, P7, the sweep, the workflow's inputs and the pins) against stand-ins of what it drives.

## Acceptance run against the deployed image

The `dispatch.acceptance.compose.yml` Compose file builds a Dispatch image from this tree and runs
it for Playwright with its own Postgres volume and database. It signs `alice` and `bob` in through
the dev sign-in route, as the local harness does. It also uses an acceptance-only agent token,
disabled NATS, and a private loopback fake Envoy with no token. `FAKE_ENVOY_PORT` and
`ENVOY_IMAGE_TAG` are its only required inputs: the acceptance service derives its `ENVOY_URL`
from `FAKE_ENVOY_PORT`, so it is the one harness port this recipe sets, and `e2e:deployed` starts
the fake listener on it. Before any row runs,
`e2e:deployed` puts a session in that fake and requires the server's `GET /api/v1/agents` to list
it, so a server reading another Envoy listener refuses the run instead of answering it from the
wrong sessions (`e2e/global-setup.ts`). With `PLAYWRIGHT_BASE_URL` set that hook starts only the
fake Envoy and the plain-HTTP proxy, the latter on a
port the OS chooses. The fake Envoy makes subscriber, Agents-page, and fixture-hook rows exercise
the deployed server instead of a real session. Fake GitHub rows still skip because the acceptance
service does not configure that listener. Its Postgres is PostgreSQL 18 on the volume
`dispatch-acceptance-pg18`; a volume `dispatch-acceptance-pg` left from before holds PostgreSQL 16
data no longer used, and `docker volume rm <project>_dispatch-acceptance-pg` removes it.

```bash
cd packages/envoy/deploy/compose
acceptance_envoy_port=19061
acceptance_dispatch_port=18767
acceptance_pg_port=55660
acceptance_image_tag=dispatch-acceptance-local
acceptance_compose_project=dispatch-acceptance
compose=(env FAKE_ENVOY_PORT="$acceptance_envoy_port" DISPATCH_ACCEPTANCE_PORT="$acceptance_dispatch_port" DISPATCH_ACCEPTANCE_PG_PORT="$acceptance_pg_port" ENVOY_IMAGE_TAG="$acceptance_image_tag" docker compose -p "$acceptance_compose_project" -f dispatch.acceptance.compose.yml)
"${compose[@]}" build dispatch-acceptance
"${compose[@]}" up -d dispatch-acceptance
cd ../../../dispatch
FAKE_ENVOY_PORT="$acceptance_envoy_port" \
PLAYWRIGHT_BASE_URL="http://127.0.0.1:${acceptance_dispatch_port}" \
PLAYWRIGHT_DATABASE_URL="postgres://postgres:dispatch@127.0.0.1:${acceptance_pg_port}/dispatch_acceptance?sslmode=disable" \
E2E_AGENT_TOKEN=acceptance-token \
bun run e2e:deployed
cd ../envoy/deploy/compose
"${compose[@]}" down -v
docker rmi "ghcr.io/cybermonklabs/legion/envoy:${acceptance_image_tag}"
```

`e2e/seed.ts` empties its database before each scenario. Always set
`PLAYWRIGHT_DATABASE_URL` to an isolated test database when using a deployed URL.

## The stubbed layer

`bun run e2e:stub` runs the stubbed layer: the rows of the `*.stub.e2e.ts` files, in a real
browser against the answers `bun run e2e:stub:record` recorded under `e2e/__recordings__/`, with
no Go server, Postgres or fake in the run. Each such file declares its `test` with
`stubbed(seed)` (`e2e/stub/stub-test.ts`).

A stubbed row asserts what the browser does with a fixed answer. Four kinds of claim stay on the
real stack: what the server decides, a read of the row's own write, a frame the server sends, and
a clock. A stubbed row skips only inside its own body: a `describe`-level skip whose condition
reads a worker fixture is seen by neither the row map nor the replay (`e2e/tools/row-map.ts`).

A new real-stack spec file needs a line in `e2e/stub/real-stack-reasons.ts` saying what the
server decides for its rows, and a new stub file a header comment saying why a recording can
answer its rows. After either, `bun e2e/tools/row-map.ts --write` regenerates `e2e/row-map.md`,
which `e2e/__tests__/row-map.test.ts` holds to a fresh generation.

The stubbed layer reads three variables of its own, and a run that is not recording needs none
of them. `DISPATCH_E2E_STUB_RECORD=1` makes the stubbed rows meet the real harness and write
what they fetched (`bun run e2e:stub:record`). `DISPATCH_E2E_STUB_PORT` moves the static file
server off 4319, which is what a second checkout on one machine needs, since the server refuses
to reuse one already listening rather than serve another checkout's build. And
`DISPATCH_E2E_STUB_RECORDINGS` points a run at a set of recordings other than the committed
one, which the nightly's re-record job uses so that it can see a committed recording no row
writes any more. A fourth, `DISPATCH_E2E_STUB_ONLY=1`, is how the config carries "this run is
the stubbed layer alone" to the rest of the run; set by hand it tells `globalSetup` to start no
stack at all, for the whole run and real-stack projects included, which then find no server to
address.

Apply the nightly job's re-recorded set by replacing `e2e/__recordings__` with its artifact
(`dispatch-browser-recordings-<attempt>`), never by copying it over the directory: a copy keeps
every recording no row writes any more.

## Projects

The suite has fifteen projects. `chromium` runs every spec except the two plain-HTTP specs, the
three the `-alone` projects below take (`aloneProjects` in `e2e/playwright.config.ts`) and the
stubbed layer's own ("The stubbed layer" above), which the last two projects of the list run.
`iphone` runs, from those same specs, only the tests tagged `@phone` (written at the end of the
test's or its `test.describe`'s title). A phone width is one under 768 px. A test takes the tag
only where the phone can change what it asserts: its layout, touch (`e2e/touch.ts`), a surface
only a phone has, a row only a phone runs, or a request count `iphone` got wrong; not where it
only reaches the same control by another route. `iphone` does not replace the check on a real
phone ("Phone check" below). `iphone`, `webkit` and `firefox` pick rows by title tag, so a test
that loses its tag drops out of them with no failure: read a project's rows with
`bunx playwright test --config e2e/playwright.config.ts --project=<name> --list`. And
`e2e/__tests__/row-configurations.test.ts` refuses a row that runs twice in one
engine, viewport, origin and server-process count. The iPhone project uses Chromium with iPhone
13 viewport, touch, and user-agent emulation. A deployed run (`bun run e2e:deployed`) uses the
same projects but the two stubbed ones and the three `-alone` ones, so its `iphone` project also
runs only the `@phone` tests, and it holds no wall-clock row: that command names no project, and
the three wall-clock projects are out of the default selection. Listed at this head, a deployed
run holds 1,302 rows in 169 files, and the other 51 are a wall-clock run of its own, under the
same base URL:
`bun run build:contracts && PLAYWRIGHT_BASE_URL=… bunx playwright test --config e2e/playwright.config.ts --project=chromium-alone --project=firefox-alone --project=webkit-alone --workers=1`,
which is `bun run e2e:clock` less the SPA build that script runs first and a deployed run has
no use for. `bunx` and not a bare `playwright`, which is on no PATH here (`command -v
playwright` in this package exits 1), and `build:contracts` because a fresh checkout has no
`@legion/contracts` build for the configuration to load - the same build `bun run e2e:deployed`
runs before its own Playwright command. The stubbed layer is left out of a deployed run because
it replays a recording
against a build this checkout made, which says nothing about a deployed image, and because that
command builds no `web/dist` for its server to serve. `webkit` and `firefox` select by tag as
well, `@webkit` and `@firefox`: each tagged row exercises one engine behaviour the comment above
the project in `e2e/playwright.config.ts` names, and a comment beside the tag names which.
`webkit` covers where a caret lands beside a collaborator's cursor, and `firefox` covers text
typed over what follows a block's last line break, which that engine's own editing mishandles.
Both run `e2e/download-names.e2e.ts`, which reads how the engine names a download from the
`filename*` and `filename` of its `Content-Disposition`, and whether it asks the server again
for a download address it holds. WebKit writes each space of the name it reads from `filename*`
as `_`, which that file's row of a name with spaces expects.
`firefox-download-navigation` runs the one row of
`e2e/shell.e2e.ts` that follows a Download link before a chunk fails. `webkit-iphone` runs the
phone-layout rows of `e2e/agent-view.e2e.ts`, `e2e/agent-view-settings.e2e.ts` and
`e2e/delivery-live.e2e.ts`, and the rows of `e2e/phone-conversation.e2e.ts` that check what the
Conversation's floating pills cover, selected by title, in WebKit with the iPhone 13 profile, since
iOS Safari is the engine those layouts exist for.
`chromium-plain-http` runs `e2e/plain-http-origin.e2e.ts` and `e2e/plain-http-proxy.e2e.ts`, with
the page opened at `http://dispatch-e2e.test:<PLAIN_HTTP_PORT>` (Chromium maps that name to the
local proxy), a plain-HTTP origin that is not loopback and so not a secure context. The proxy
forwards to the dev sign-in server at its loopback origin. The project proves a document takes a
new paragraph and a comment renders formatted there, where `crypto.randomUUID` does not exist, and
that the proxy refuses any other `Host` as the server's own fence does and keeps no request open
upstream once a page has closed. `chromium-plain-http-sign-out` runs the sign-out rows of
`e2e/keyed-writes.e2e.ts` on that same origin, where the browser has no `navigator.locks`, so a
tab holds the person's writes for another tab's sign-out on a renewed lease rather than on that
tab's lock.
The last three projects run the rows whose claim is a wall-clock time, with the machine to
themselves: `chromium-alone` runs `e2e/heavy-bodies.e2e.ts` and `e2e/nested-directive.e2e.ts`
(a card draws without holding the main thread for a second)
and `e2e/agent-workspace-timing.e2e.ts` (a sleeping live view of an ordinary conversation comes
back inside half a second on the fastest of up to five fresh wakes, a sleeping view at the
stream's worst case comes back inside 1.2 seconds on the fastest of up to eight fresh wakes, and
the Agents page paints six hundred sessions inside 100 milliseconds while holding the main
thread under 150 ms, the first load inside both of those bounds of up to twenty taken), and
`firefox-alone` and `webkit-alone` run `e2e/heavy-bodies.e2e.ts` in the other two engines.
Each sets `default: false`, so a run that names no project holds none of their 51 rows, and each
caps its own workers at one.
What holds a reading to a quiet machine is the run being one worker wide, and the three files
reach that in two ways: `e2e/main-thread-hold.ts` fails a measuring row of
`e2e/heavy-bodies.e2e.ts` or `e2e/nested-directive.e2e.ts` in a run of more than one worker,
since four workers on one machine make a second of wall clock unmeasurable, and the rows of
`e2e/agent-workspace-timing.e2e.ts`, which read a clock of their own, take that one worker from
the command that runs them.
`bun run e2e:clock` is the command that runs all three projects, and it names `--workers=1`
beside them; one file in one engine is
`bun run e2e -- --project=chromium-alone e2e/heavy-bodies.e2e.ts --workers=1`, which names that
worker count itself rather than taking the configuration's default, and which writes the project
as `--project=<name>`, since the option is variadic and would read the file after a space as a
second project name.
`bun run e2e:install` installs all three browsers.

## Accessibility and visual review

`e2e/review-routes.ts` lists every route of `web/src/app.tsx`, reviewed or excluded with a
reason, and two layers read it: the axe scan of `e2e/accessibility.e2e.ts` and the visual review
of `e2e/visual.e2e.ts`, whose baselines are committed as
`e2e/__screenshots__/<project>/<route>-<scheme>.png`. An axe finding is fixed in `web/src`;
`e2e/accessibility-exclusions.ts` takes an entry only where the fix would change what another row
claims. A baseline comes only from the pinned container, through
`e2e/visual-run.sh --update-snapshots`; the script's header says how to take one from CI on a
machine with no container.

## Phone check

Browser emulation covers responsive layout. A phone acceptance check runs against
a tailnet-reachable Dispatch server: configure `DISPATCH_LISTEN_HOST` with the
host's Tailscale IPv4 address (`tailscale ip -4`), use that tailnet address and
`DISPATCH_PORT`, open the Inbox from the phone, and answer an open ask. Set
`DISPATCH_INSECURE_COOKIE=1` only for HTTP; HTTPS keeps the normal secure cookie
setting.
